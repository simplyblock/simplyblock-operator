# The cleanup passes

Nine passes, in the order `SKILL.md` runs them. Each one states the
repository's rule, what it must not remove, how to find candidates, and the
mistake it makes when it is applied mechanically.

Counts below were measured over `operator/internal/controller` (30 files, 18,041
lines, 424 functions) unless another scope is named. Re-measure rather than
trusting them. That is what `scripts/measure.sh` is for.

Each pass below names a goal. `catalog.md` names the moves: which refactoring
technique resolves which smell, in its Go form, and which of the classical
techniques do not apply here at all.

## 1. Dead code

**Rule.** Code that nothing reaches is deleted, not commented out, not moved to
a `legacy` file, and not kept "in case." Git holds the history, and a file does not
have to.

**What is dead, in the order it is cheap to prove:**

| Kind                              | How to prove it                                                     |
|-----------------------------------|---------------------------------------------------------------------|
| Unused unexported symbol          | `make lint` — the `unused` linter already reports it                |
| Unused exported symbol            | `go run golang.org/x/tools/cmd/deadcode@latest ./...` per module    |
| Unreachable branch                | the condition is a constant, or the guard above it already returned |
| Orphaned constant, error, or type | `grep -rn '\bName\b' --include='*.go'` across all three modules     |
| Commented-out code                | `measure.sh` reports it as `commented_code` (18 in the controllers) |
| A field nothing reads             | for a CRD field, this is `api-design`'s call, never a cleanup's     |

**Preserve.** An exported symbol in `atlas-lib` with no consumer today is not
dead: the library is deliberately ahead of its consumers, and `README.md` marks
which patterns are "available but not yet adopted." `statemachine` has zero
importers and is the shape new controllers are supposed to take. Check the
README before deleting anything public from `atlas-lib`.

**The usual mistake.** Deleting a symbol that only a build tag, a `//go:generate`
line, a Helm template, or a reflection-driven path references. Search the YAML
and the templates too, not only the Go.

## 2. Comments

**Rule.** A comment says what the code cannot: why the seam is here, why the
ordering matters, what breaks if it changes, which incident produced the guard.
A comment that restates the statement below it is deleted rather than reworded.

Every file also opens with a comment stating what is in it and why it lives
there. That is `new-files`, and a cleanup that splits a file owes the new file
its opening comment.

**Delete outright:**

- Restatement: `// increment the counter` above `counter++`.
- A doc comment that only repeats the signature: `// GetVolume gets a volume.`
- Section dividers made of punctuation.
- Commented-out code, always. It is dead code wearing a comment.
- `// TODO` with no owner, no issue, and no condition. Either it names what
  would resolve it or it is noise. Six of these sit in the controllers.

**Preserve, and never trade for brevity:**

- Why a guard exists, especially one traceable to an incident.
- Ordering constraints: write-ahead-of-side-effect reasoning, freeze counts,
  drain-before-transfer sequencing. This repository's worst outages came from
  reordering, and the comment is the only warning the next reader gets.
- An issue or ticket reference.
- A `Regression:` line. It ties the test to the bug and outlives the conversation.
- `+kubebuilder:` and other generator markers. They are not comments. They are
  input to `controller-gen`, and deleting one silently changes the CRD.

**Rewriting.** Prose goes through `house-style`: American English, the Oxford
comma, lowercase `simplyblock`, impersonal third person. Run
`.claude/skills/house-style/scripts/quality-gate.sh --changed` afterward.

**The usual mistake.** Deleting a comment that looks redundant because the code
was misread, because the comment was documenting the case the code does *not* handle.
When a comment and the code disagree, that is a finding to report, not a comment
to fix: one of the two is a bug, and this skill does not fix bugs.

## 3. Modernize onto atlas-lib

**Rule.** Before extracting, unifying, or simplifying a primitive, check whether
`atlas-lib` owns it. This is rung 2 of the ladder and the rung most often
skipped, and skipping it is how a repository ends up with two of everything.

The two authoritative lookups, neither of which is memory:

```bash
go doc github.com/simplyblock/atlas            # the overview and package list
(cd atlas-lib && go list ./...)                # every package, internal included
go doc github.com/simplyblock/atlas/nqn        # one package in detail
```

`atlas-lib/README.md` answers the other half: the flows the two consumers
actually perform, each with its idiomatic call sequence, and a _Today_ note
pointing at the live call site. A _Today_ note that says "still calls its own
client" is a modernization target the library has already documented.

**What to look for.** `scripts/find-twins.sh --handrolled` reports each of these
with its call sites:

| Written by hand                                 | Owned by       | Current count                                                                                        |
|-------------------------------------------------|----------------|------------------------------------------------------------------------------------------------------|
| `fmt.Sprintf("nqn.2014-08.io.simplyblock:...")` | `nqn`          | `nqn.Host(nodeUID)`; the package has 0 importers                                                     |
| `strings.Split(handle, ":")`                    | `lvol`         | `lvol.VolumeHandle.Split()`; 0 importers                                                             |
| `strings.Contains(err.Error(), ...)`, `== 503`  | `errs/class`   | one classifier, one retry policy; 0 importers                                                        |
| `switch ops.Status.SubPhase`                    | `statemachine` | 12 phase switches in 7 controller files                                                              |
| reading `/sys/class/nvme` directly              | `nvme`         | centralized in `atlas-lib/internal/sysfs`                                                            |
| `"nvme", "connect"` and friends                 | `nvmeof`       | nvme-cli shelled out to from the CSI initiator and the rebalancer; `nvmeof` uses `/dev/nvme-fabrics` |
| a second control-plane client                   | `controlplane` | `operator/internal/webapi` (2,065 lines, retiring)                                                   |

**Direction of travel for the control-plane client.** `operator/internal/webapi`
is being retired in favor of `atlas-lib/controlplane`, which already covers its
surface: `CreateVolume`, `CreatePool`, `GetStorageNodes`, `GetStorageNodeNICs`,
`GetStoragePools`, and the whole migration set. A cleanup does not tidy `webapi`
and does not extract from it. A call site being cleaned for another reason either
moves to `controlplane` or is left untouched. Picking that up as its own project
is a migration, not a cleanup pass, and it is a transition until the last call
site is gone.

**When the primitive is missing, or is there but unexported.** That is
`extract-to-atlas-lib`. Two live examples the twin finder reports:
`csi-driver/pkg/util/nvmerepair.go:356` is a copy of the unexported
`atlas-lib/nvmeof/inspect.go:498`, and `nvmerepair.go:373` is a copy of the
unexported `atlas-lib/nvmeof/repair.go:600`. The primitive exists. It just is not
reachable, and copying was the path of least resistance. Exporting it is the fix.

**The usual mistake.** Adopting an `atlas-lib` package without reading its
semantics. `errs/class` carries a specific retry policy, `DeleteVolume` is
idempotent by contract, and `ptr.ClampToInt` saturates rather than wrapping.
Swapping in a call whose edge behavior differs is a behavior change, and the
behavior gate is supposed to catch it, so read the package documentation first and
it never gets that far.

## 4. Deduplication

**Rule.** A second copy is worse than an awkward dependency on the first,
because the copies drift and a fix lands in one of them. That is
`CLAUDE.md`'s rule, and this pass is how it gets enforced on existing code.

`dupl` will not find these: `.golangci.yml` excludes `internal/*` from it, which
is 24,926 of the operator's 29,572 hand-written lines, the entire controller
tree. `scripts/find-twins.sh` covers the gap by comparing normalized function
bodies.

**Three cases, three homes:**

| Copies                             | Where the single version belongs                                         |
|------------------------------------|--------------------------------------------------------------------------|
| Inside one package                 | a helper in that package                                                 |
| Across packages in one module      | the package that owns the concern; `new-files` decides whether it is new |
| Across the operator and the driver | `atlas-lib` — hand off to `extract-to-atlas-lib`                         |

Live examples: `apiClient()` is copied verbatim in five controllers, and
`controlPlaneToStorageNodeSetRequests`, `tlsSecretToStorageNodeSetRequests`, and
`spdkProxyPodToStorageNodeSetRequests` are three names for one 16-line body in
`simplyblockstoragenodeset_controller.go`. The repeated controller helper set,
`handleDeletion` in five, `patchStatus` in four, `succeedOps` and `failOps` in
three, `ensureFinalizer`, `releaseLock`, `reconcileStart`, and `reconcileDelete`
in two each, is Kubernetes-shaped, so it belongs in a shared base in the
operator and **not** in `atlas-lib`. `reconciler-patterns` owns the shape it
should take.

**Preserve.** Two bodies that are identical today but answer to different
requirements are not duplication. They are a coincidence, and merging them
couples two things that are free to diverge. Before merging, ask what would make
the two copies differ. If a plausible answer exists, leave them and say why.

**The usual mistake.** Merging copies that have already drifted, by picking one
arbitrarily. When the twin finder pairs two bodies as *near* rather than exact,
one of them usually carries a fix the other does not. Diff them and take the
union deliberately. If the difference turns out to be a bug in one copy,
that is a finding for `regression-test`, not something to smooth over here.

## 5. Complexity and nesting

**Rule.** Nesting is inverted, not indented. A guard clause that returns early
is strictly easier to read than the block it replaces, and the repository has 32
functions in the controllers indenting five tabs or deeper.

**The order that works:**

1. **Return early.** Error first, precondition second, terminal case third.
   In a reconciler this is already the house pattern: a terminal object is a
   no-op return, not a nested body. See `reconciler-patterns`.
2. **Name the condition.** A four-predicate `if` becomes a named boolean or a
   small predicate function. The name is the documentation the compound
   expression was never going to be.
3. **Lift the invariant.** Anything computed inside a loop that does not depend
   on the loop leaves it.
4. **Split at the branch.** A function whose body is one big `switch` over a
   phase has one handler per phase. This is exactly the `statemachine` shape,
   and pass 3 may have already replaced the switch entirely.
5. **Then, and only then, extract.** After 1–4 there is usually much less left
   to extract than there looked to be at the start.

**Length is a symptom. Branching and nesting are the disease.** A 300-line flat
composite literal holds one idea and reads top to bottom, which is the case for
`BuildStorageNodeSetDaemonSet` at
`operator/internal/utils/storage_nodeset_ds.go:48`, 311 lines of it. A 90-line function at five levels of nesting with
twelve branches does not. Cut the second before the first, and never split a
flat literal just to move a number.

**Preserve.** `gocyclo` is enabled and is the authority on complexity, and
`measure.sh` reports `branches` as a cheap proxy, not a verdict. Do not add a
`nolint` to silence either. If the honest state is that a function is complex
because the problem is, the comment says which part is irreducible.

**The usual mistake.** Extracting a helper that takes eight parameters, three of
them booleans. That did not reduce complexity, it renamed it and added an
argument list, and the mechanism gate's "not exported to callers" rule fails it.

## 6. Function sizing

**Rule.** A function does one thing that its name describes completely. When the
honest name needs an `and`, it is two functions, which is the same test
`new-files` applies to a file.

Thresholds are prompts to look, not laws: 60 lines is worth a look, 100 needs a
reason in a comment. In the controllers, 75 functions are over 60 and 22 are over
100, the longest at 183. `atlas-lib`'s longest is 65, which is the standard this
repository already meets somewhere.

**Two shapes that are legitimately long,** and are left alone unless something
else is wrong with them:

- **A flat builder.** One composite literal, no branching. Split it only when a
  part of it is reused or independently testable.
- **A wiring sequence.** `operator/cmd/main.go:101` is 413 lines of setup calls
  in a row. Splitting it into `setupWebhooks`, `setupControllers`, and
  `setupHealthChecks` is right because the groups are real, not because 413 is a
  large number.

**Neither excuse applies to a `Reconcile` body.** A 181-line `Reconcile` is a
phase machine that has not been written down yet. `reconciler-patterns` has the
decomposition: `Reconcile` dispatches, one function per phase, one step per
function inside it.

**The usual mistake.** Slicing a long function at line boundaries into
`fooPart1` and `fooPart2`. If the extracted pieces cannot be named for what they
do, the split is arbitrary and the reader now has to hold three things instead of
one.

## 7. Restructure

**Rule.** A file is a unit of separation of concerns, and its opening comment is
the test. `new-files` section 0 states it, and this pass is the same rule
applied to files that already exist. If the honest comment needs an "and also,"
the file is two files.

**What justifies a move:**

- A file whose subject changed as it grew. 14 of the 30 controller files are
  over 600 lines, and the question for each is whether the extra length is one
  subject or several. `storagenodeops_controller.go` at 1,696 lines is a
  reconciler plus a hand-rolled state machine plus topology handling.
- A helper used by three siblings and living in one of them.
- A package that has become a bag: `operator/internal/utils/objects.go` at 750
  lines names nothing in particular.

**Mechanics.** `git mv` first, in its own commit, so the move is reviewable as a
move. Then fix imports and run `make lint`. Splitting a file inside a package
needs no import changes at all, which makes it the cheapest restructuring there
is, and every new file gets its opening comment.

**Preserve.** Package boundaries in `atlas-lib` are deliberate and documented in
its README `Layout` block: one cohesive concern per package, no `pkg/` prefix,
public when a consumer imports it and `internal/` when it does not. A move
inside `atlas-lib` updates that block.

**The usual mistake.** Splitting one concern across files that each need the same
context, so the reader has to reassemble it. A 900-line file with one subject is
easier to work in than three files with none.

## 8. Pattern unification

**Rule.** Siblings that solve one problem solve it one way. Where they differ,
the canonical version is the one that is already documented: `reconciler-patterns`
for controllers, `atlas-lib/README.md` for library flows, `api-design` for CRDs.

**What to unify:**

| Divergence                              | The canonical form                                                     |
|-----------------------------------------|------------------------------------------------------------------------|
| Phase advanced by a hand-rolled switch  | `atlas-lib/statemachine` with a declared graph                         |
| An untyped `phase` or `subPhase` string | a typed `FooPhase string` with its constants beside it                 |
| Error wrapping style, sentinel choice   | `errs` sentinels, `errors.Is` across package boundaries                |
| Status write style                      | one `patchStatus` shape, not four                                      |
| Requeue and backoff choices             | requeue against error, `RequeueAfter` for waiting; never a sleep       |
| Logging keys and levels                 | whatever the majority of controllers already do; pick one and state it |

**Preserve.** A deliberate difference with a reason is not divergence. When a
controller does something unlike its siblings and a comment says why, unifying it
deletes the reason along with the difference. If no comment says why, the finding
is that the comment is missing.

**The usual mistake.** Unifying toward the most common form when the most common
form is the old one. Count, then read `reconciler-patterns`: `statemachine` has
zero adoptions and twelve hand-rolled switches, and the majority is not the
target here.

## 9. Constants

**Rule.** A literal that appears twice, or once with meaning that is not obvious
from context, becomes a named constant next to the thing it configures.

**In scope:** timeouts, intervals, retry and backoff counts, size and capacity
thresholds, repeated label, annotation, and finalizer keys, phase and condition
strings, and container, volume, and port names in a builder.

**Preserve.** `0`, `1`, and `-1` where they mean nothing but themselves. A
constant named `one` is worse than the digit.

Keys that both consumers use are already named once, in `atlas-lib/kube/names.go`
(driver name, parameter, context, label, annotation, and finalizer keys). A new
constant for one of those is a fourth copy of a name that has to match character
for character across two components, so this pass looks there first.

**The usual mistake.** Hoisting a literal into a package-level constant far from
its use, where the next reader has to jump to find out that `defaultTimeout` is
30 seconds. Keep it adjacent, and let the name carry the unit.
