---
name: regression-test
description: Write and run a regression test for a bug, red-to-green. Covers the mandatory order (failing test first, fix second), the Regression marker every such test carries, what makes a test behavioral rather than an assertion about the fix's shape, which level to test at (Go unit with a fake client and mock HTTP, envtest, CSI e2e, live cluster), how to run each suite, and what to do when a bug cannot be reproduced off a real cluster. Use whenever fixing a bug, before writing the fix.
---

# Regression tests

## Overview

A regression test is the test that would have caught a specific bug. It has to
exist, it has to be **red before the fix**, and it has to say which bug it
belongs to.

The order is not a style preference. A test written after the fix proves only
that the code passes its own author's expectations; it never demonstrated it can
fail, so it may assert nothing. A test that goes red first, for the reason the
bug describes, is the only kind that pins the behavior.

## The four rules

1. **Behavioral, never an assertion about the fix.** A test asserts observable
   behavior: a status field, a phase transition, an emitted event, which endpoint
   was called and how often, a returned selection, data that survived. See
   *What behavioral means* below.
2. **Carries a regression marker.** Every regression case names the bug in its
   doc comment, and gets a `Regression` row in the test plan. See *The marker*.
3. **Written before the fix.** The commit may carry both, but the test is
   authored and run first.
4. **Red for the right reason, then green.** Run it against the unfixed tree and
   confirm both that it fails and that the failure message describes the bug. A
   test that fails because it does not compile, because a fake is missing a
   route, or because the envtest assets are absent has demonstrated nothing.
   Only then write the fix.

Do not skip 4 by reasoning that the test obviously fails. A reconciler test
whose fake client lacks the object under test passes for the wrong reason
routinely.

## The marker

Put a `Regression:` line in the test's doc comment, with an identifier that
outlives the conversation:

```go
// Regression: #444 — a migration that never cut over left its NVMe target paths
// connected, so the next migration of the same volume found a poisoned data path
// and every later migration on that node queued behind it.
func TestReleasePathsWhenMigrationNeverCutOver(t *testing.T) {
```

Identifier, in order of preference:

- A GitHub issue number: `Regression: #444`.
- A date-based id when there is no issue:
  `Regression: 2026-08-17-vmig-validate-path-leak` — the date the bug was
  diagnosed plus a short slug. Unique, sortable, and it survives being copied
  into a commit message.
- Add the fixing commit once it exists, if it helps a reader:
  `Regression: 2026-08-17-vmig-validate-path-leak (fixed 5aa711cc)`.

State the **failure**, not the fix: what went wrong and what it cost. A reader
hitting this test in two years needs to know what breaks if they delete it.

Markers are greppable on purpose:

```bash
rg -n 'Regression:' --type go
```

**Also add the row to the test plan.** `operator/docs/tests/test-plan-<area>.md`
carries a numbered scenario matrix whose `Type` column has a `Regression` value
for exactly this — the scenario, the type, and the test function name, with the
issue cited in the scenario text. A regression test that is not in the matrix is
invisible to the next person auditing coverage. See the `design-doc` and
`test-scenarios` skills.

## What behavioral means

Valid — the reconciler runs and the test asserts what it did:

```go
res, err := r.Reconcile(ctx, req)
g.Expect(err).NotTo(HaveOccurred())
g.Expect(res.RequeueAfter).To(BeNumerically(">", 0))

updated := &v1alpha1.StorageNode{}
g.Expect(c.Get(ctx, key, updated)).To(Succeed())
g.Expect(updated.Status.ActionStatus.SubPhase).To(Equal("Suspending"))
```

Valid — an API contract, asserted by what the control plane saw. The call count
is the point: it is what distinguishes "never acted" from "acted twice":

```go
// A retried suspend must be idempotent: the operator may re-reconcile at any
// point, and a second POST restarts the backend's own state machine.
g.Expect(suspendCalls).To(Equal(1))
g.Expect(recorder.Events).To(ContainElement(ContainSubstring("NodeSuspended")))
```

Valid — a shared contract helper, when several reconcilers owe the same
behavior. `operator/internal/controller/reconcile_contract_test.go` is the
existing example (`expectIgnoreNotFoundNoRequeue`).

**Invalid** — asserting the shape of the fix rather than the absence of the bug:

```go
// NEVER. This passes if the helper is called and the paths still leak.
g.Expect(releasePathsCalled).To(BeTrue())
```

**Invalid** — asserting on source text, a log line's exact wording, or a
generated file's contents when the behavior is what broke. If the only test you
can think of is one of those, the behavior is not observable yet: make it
observable — a status field, a condition, an event, a counter on the fake — and
assert that.

## Choosing the level

| The bug lives in                                                                                    | Test it as                                                      | Where                                                                |
|-----------------------------------------------------------------------------------------------------|-----------------------------------------------------------------|----------------------------------------------------------------------|
| A pure helper, a selector, scoring, name generation, classification                                 | Go unit test                                                    | `<pkg>/<name>_test.go`, or `*_unit_test.go` in `internal/controller` |
| One reconciler's decision: sub-phase advance, requeue, status write, event, which endpoint it calls | Go unit test with a fake client and an `httptest` control plane | `internal/controller/*_unit_test.go`                                 |
| Interaction with a real API server: webhooks, finalizers, ownership, watches, RBAC                  | envtest suite                                                   | `internal/controller/*_controller_test.go` (see `suite_test.go`)     |
| CSI publish/stage/mount, multi-namespace or multi-cluster provisioning                              | CSI e2e (ginkgo)                                                | `csi-driver/e2e/`, labeled `SPDKCSI-*`                               |
| The data path: migration cutover, ANA state, SPDK behavior, real NVMe                               | live cluster                                                    | the sbtest framework under `test/`, or a documented manual recipe    |

Prefer the lowest level that can still fail for the bug's reason. A unit test
with a fake client and a mock control plane can drive states a live cluster
reaches only by luck — an operator restart mid-sub-phase, a 5xx on the third
call, a stale informer cache — and it runs in milliseconds.

`internal/controller/test_helpers_test.go` provides `newTestScheme`,
`newTestClient`, and `testCluster`; use them rather than rebuilding a scheme.
`sigs.k8s.io/controller-runtime/pkg/client/interceptor` is how an existing test
injects a client error, and `httptest.NewServer` with a route-counting handler is
how it fakes the control plane.

## Running the suites

```bash
# The operator's unit and envtest suites (regenerates manifests first).
make -C operator test

# One test, iterating. Envtest assets come from the repo-root .bin.
make -C operator setup-envtest
cd operator && KUBEBUILDER_ASSETS="$(../.bin/setup-envtest use 1.31 --bin-dir ../.bin -p path)" \
  go test ./internal/controller/ -run TestReleasePathsWhenMigrationNeverCutOver -v

# A pure-helper package, no assets needed.
cd operator && go test ./internal/volumemigration/ -run TestReleasePaths -v

# The other components.
make -C atlas-lib test          # go test -race, coverage
make -C csi-driver unit-test    # go test -race -cover over cmd and pkg
make -C csi-driver e2e-test E2E_TEST_ARGS='--focus=SPDKCSI-NVMEOF'
```

`make -C operator test` runs `manifests generate fmt vet` first, so it can change
tracked files — check `git status` after, not before.

### Race and order

- **`-race` is not on by default in the operator suite.** `atlas-lib` and
  `csi-driver` run with it; the operator does not. For a bug about concurrent
  reconciles, a shared map, or a cached client, run
  `go test -race ./internal/...` explicitly. A regression test for a data race
  that is not run under `-race` is not a regression test.
- **`go test -shuffle=on`** randomizes case order and prints the seed, which is
  how a case that leaks state into its neighbor gets caught. Replay with
  `-shuffle=<seed>`. Worth doing when a new case passes alone and fails in the
  suite, or the reverse.
- **`-count=1`** defeats the test cache when re-running a test that "already
  passed."

## The workflow

1. **Reproduce first.** Get the bug to fail in front of you: the failing CI job,
   the operator log, the control-plane log, the `kubectl describe` output, the
   artifact directory of the run that broke. Understand the mechanism before
   writing an assertion — a test written against a guess pins the guess.
2. **Write the failing test**, with its `Regression:` marker, at the lowest level
   that can fail for the bug's reason.
3. **Run it against the unfixed tree.** Confirm red, and read the message: it
   must describe the bug, not a missing symbol or an absent envtest binary.
4. **Write the fix.**
5. **Run it again.** Green.
6. **Run the whole package, then the component.** `go test ./internal/...`, then
   `make -C operator test`. A new case can expose an old leak; that is the
   feature working, and it is this change's problem.
7. **Add the matrix row** to the relevant test plan: ID, scenario, `Regression`,
   and the test function name.
8. **Gates before committing:** `make -C operator lint`, the house style gate
   (`.claude/skills/house-style/scripts/quality-gate.sh --changed`), and the
   drift gates if the fix touched markers or types (see `build-system`).

## When it cannot be reproduced below a live cluster

Some bugs are not reachable from a Go test: SPDK's internal state, real NVMe
multipath and ANA transitions, a control-plane RPC timing out at 5.01 seconds,
data corruption under concurrent I/O. Do not fake a test for those, and do not
weaken a real one until it passes.

Do this instead:

- **Test the nearest observable contract.** The mechanism may be out of reach
  while its precondition is not. If the bug was that more than one freeze window
  per migration silently loses writes, the operator-side contract is how many
  freeze-inducing calls the reconciler issues for one migration — countable
  against a mock control plane. If it was a validation Job leaving NVMe paths
  connected, the contract is that the reconciler issues the disconnect for every
  path it connected, on every terminal path including failure and cancellation.
  That is what keeps the fix from being undone.
- **Write the live-cluster scenario down** as an `E-nn` or `M-nn` row in the test
  plan, with the reproduction recipe as a numbered test concept — see
  `test-scenarios`. An honest "not automated, verified on a 3-node cluster with
  fio verify over 40 migrations" is worth more than a test that cannot fail, as
  long as it is written where the next person looks.
- **Say it plainly** in the commit message: what was verified, how, and at what
  sample size.

## Anti-patterns

- **Test after fix.** It never demonstrated it can fail.
- **Asserting the fix's shape instead of the bug's absence.** Assert the paths
  are released; do not assert that the release helper was called — unless the
  call itself is the contract, as with an idempotent POST's call count.
- **Loosening an assertion to get green.** If the test is wrong, fix the test and
  say why. If the fix is wrong, fix the fix.
- **A test that passes on the unfixed tree.** Delete it and start again; it is
  worse than nothing, because it looks like coverage.
- **A concurrency regression test that never runs under `-race`.**
- **A regression test with no marker and no matrix row.** In a year it is
  indistinguishable from an ordinary case, and the next person deleting a
  "redundant" test takes the coverage with it.
