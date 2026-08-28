---
name: code-cleanup
description: Clean up, refactor, modernize, or restructure existing code in this repository without changing what it does: simplify, deduplicate, unify patterns, adopt an atlas-lib primitive in place of a hand-rolled one, extract functions and constants, split files and regroup packages, cut cyclomatic complexity, flatten nesting, separate concerns, rewrite comments to be explicit, and delete dead code. Use whenever asked to clean up, tidy, refactor, simplify, modernize, deduplicate, restructure, reorganize, split, flatten, or reduce the complexity of code that already works.
---

# Code cleanup

**Conserve behavior. Minimize mechanism.** Everything below serves those two
sentences in that order. Behavior is what callers, the control plane, the
cluster, and the tests can observe. Mechanism is the code, state, indirection,
configuration, and suppression it takes to produce it. A cleanup that leaves
behavior alone and takes mechanism away is the whole deliverable. Anything that
changes behavior is a different task with a different skill, however small it
looks.

Vocabulary and gate structure adapted from prior art, and the technique and
smell names from Fowler's refactoring catalog. See `references/source-notes.md`.

## Scope

| In scope                                                               | Not this skill                                                                               |
|------------------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| Simplification, deduplication, extraction of functions and constants   | A behavior change, however trivially small. A design doc or an issue first                   |
| Unifying patterns and concepts across siblings                         | A bug fix. See `regression-test`, which requires the failing test first                      |
| Modernizing onto `atlas-lib` primitives                                | Moving a shared primitive *into* `atlas-lib`. See `extract-to-atlas-lib`, from pass 3        |
| Complexity, nesting, function and file sizing                          | Renaming or retyping a field under `operator/api/**`. That is an API break. See `api-design` |
| Splitting files, regrouping packages, moving code between them         | A new feature, a new CRD, a new controller                                                   |
| Rewriting comments to be explicit and on point, and deleting dead code | Performance work that changes an algorithm's observable output or ordering                   |

## Never touch

A cleanup pass that edits one of these produces a diff that CI reverts or that
strips someone else's copyright.

| Path                                                                              | Why                                                                                     |
|-----------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------|
| `*.gen.go`, `atlas-lib/internal/cpapi/cpapi.gen.go` (19,268 lines)                | oapi-codegen output. Change the spec or the generator config                            |
| `**/zz_generated.deepcopy.go`, `operator/config/**`, `operator/dist/install.yaml` | controller-gen and installer output. See `build-system`                                 |
| `helm-charts/charts/**`, `csi-driver/charts/**`                                   | synced from the operator manifests by `make helm-sync`                                  |
| `csi-driver/pkg/**` and `csi-driver/e2e/**` files carrying an Apache header (23)  | inherited upstream SPDK-CSI code. Reshaping it discards the provenance the header names |
| Field names, types, and marker semantics under `operator/api/**`                  | a shipped field is a contract, so renaming it is a breaking change, not a rename        |

`operator/internal/webapi` is a special case: it is being retired in favor of
`atlas-lib/controlplane`, so it takes no investment. Do not tidy it, do not
extract helpers from it, and do not add to it. A call site that has to change
anyway moves to `controlplane` or is left exactly as it is.

## The phases

### 0. Scope and mode

Three things before an edit.

1. **Name the target in one sentence.** A file, a package, a controller, a
   duplicated family, or a measured worklist row. "The repository" is not a
   target. It is a backlog, and `references/worklist.md` already holds one.
2. **Start from a clean worktree.** `git status` shows nothing pending in the
   target. A cleanup diff has to be readable on its own, because reviewing it is
   how anyone confirms the behavior did not move.
3. **Classify every finding as behavior-preserving or not**, before touching
   any of it. The ones that are not preserving do not get quietly folded in "while
   we are here." They are reported, and they leave with the diff. A cleanup
   commit that also fixes a bug is a commit nobody can review and nobody can
   revert.

### 1. Baseline

```bash
.claude/skills/code-cleanup/scripts/measure.sh --paths <target> --detail \
  --baseline /tmp/cleanup-before.tsv
```

Record the numbers. Without a baseline there is no mechanism gate in phase 5,
and "it reads better now" is not a result. It is an impression, and it is the
one every author of every refactor has.

The metrics and their thresholds are documented in the script header. Two are
worth knowing before starting. `dup_bodies` counts functions whose normalized
bodies are byte-identical, which is the copy-paste detection `dupl` cannot do
here because `.golangci.yml` excludes `internal/*`. `handrolled_*` counts the
places where a primitive `atlas-lib` already owns was written out by hand.

For a sense of scale, the same script over `atlas-lib` reports a longest function
of 65 lines and none over 100, against 22 over 100 in
`operator/internal/controller`. `atlas-lib` is the shape this repository already
knows how to write.

### 2. Lock behavior

Run the suite for the target **before** editing, and confirm it is green:

```bash
make atlas-test   # or csi-test, operator-test; make test runs all three
```

Then ask what would fail if this cleanup broke the target. If the honest answer
is nothing, the tests come first, and `regression-test` has the mechanics: the
level to test at, the fake client and mock HTTP setup, and the marker. A
characterization test written here pins current behavior rather than a bug, so it
carries no `Regression:` line. It says which cleanup it was written for.

**A refactor of untested code is a rewrite with extra steps.** If the target
cannot be covered (it only fails on a real cluster, it needs hardware), say so
and stop at diagnosis. Report what would have been cleaned up and what evidence
is missing. Diagnosis is a legitimate outcome. An unverifiable claim of
equivalence is not.

### 3. The ladder

Work the rungs in order, top down. Each one makes the rungs below it smaller,
and skipping ahead produces work that has to be undone.

| Rung               | The question                                                             | Why it comes first                                                       |
|--------------------|--------------------------------------------------------------------------|--------------------------------------------------------------------------|
| 1. **Delete**      | Can this go entirely: is anything still reached, still called, still on? | The cheapest mechanism to review is the mechanism that is gone           |
| 2. **Reuse**       | Does `atlas-lib`, the standard library, or a sibling already do this?    | Extracting a helper that already exists elsewhere creates the third copy |
| 3. **Unify**       | Do these siblings do one thing several ways?                             | Unify before extracting, or the extraction has to be redone per variant  |
| 4. **Extract**     | What does this block do, and can it be named?                            | Only now, because rungs 1–3 changed what is left to name                 |
| 5. **Restructure** | Does the seam between files and packages match the concerns?             | Moving code is cheap once it is the right code                           |
| 6. **Flatten**     | Can the nesting invert into guard clauses and early returns?             | Last, because the earlier rungs delete most of the nesting for free      |

The rung most often skipped is 2. Read `references/passes.md` pass 3 before
extracting anything.

### 4. The passes

In this order, because each pass makes the next one's diff smaller and safer.
`references/passes.md` carries each pass in full: the repository's rule, what to
preserve, how to find candidates, and the mistake it usually makes.

The passes are entered from a measurement. **A cleanup is also entered from a
smell**, which is the other half and the one reading code produces:
`references/catalog.md` indexes the named code smells onto these passes and onto
the refactoring techniques that resolve each one, filtered to the subset that
applies to Go.

| #   | Pass                     | Removes                                                             | Delegates to           |
|-----|--------------------------|---------------------------------------------------------------------|------------------------|
| 1   | Dead code                | unreachable branches, unused helpers, orphaned constants, leftovers | —                      |
| 2   | Comments                 | restatement, stale prose, commented-out code, vague `TODO`s         | `house-style`          |
| 3   | Modernize onto atlas-lib | hand-rolled NQNs, handle parsing, error classification, sysfs reads | `extract-to-atlas-lib` |
| 4   | Deduplication            | copies inside a module, and copies across two of them               | `extract-to-atlas-lib` |
| 5   | Complexity and nesting   | nested blocks, compound conditions, flag arguments                  | `reconciler-patterns`  |
| 6   | Function sizing          | functions doing several things under one name                       | `reconciler-patterns`  |
| 7   | Restructure              | files and packages whose subject is no longer one subject           | `new-files`            |
| 8   | Pattern unification      | siblings that solve one problem several ways                        | `reconciler-patterns`  |
| 9   | Constants                | magic numbers and repeated string literals                          | —                      |

### 5. The gates

Two gates, and they answer different questions. Both run before the work is
handed back.

**The behavior gate: did anything observable change?**

- The suite from phase 2 is green, including the tests written there.
- `make -C operator manifests generate` and `make helm-sync` produce no diff,
  or produce exactly the diff the change implies. See `build-system`.
- `make lint` passes without a new `nolint` directive. A suppression added to
  make a cleanup pass is mechanism the cleanup was supposed to remove. The
  repository carries 52 of them already.
- `.claude/skills/house-style/scripts/quality-gate.sh --changed` passes over
  rewritten comments.
- Boundary cases the tests do not reach are named explicitly, not assumed.

**The mechanism gate: did the mechanism actually fall?**

```bash
.claude/skills/code-cleanup/scripts/measure.sh --paths <target> \
  --compare /tmp/cleanup-before.tsv
```

Same scope, same method, same thresholds. Then:

- Every metric that rose is paid for by a larger fall in another, and the trade
  is named in the report. Splitting one 180-line function into four raises
  `funcs` and lowers `max_func_len`. That is a trade, and it is stated as one.
- **Every symbol added maps to one removed.** A new helper beside the code it was
  meant to replace is not a reduction. It is a second way to do the thing.
- **Mechanism was not exported.** Moving a decision to the caller, to a new
  parameter, to a config field, or to a marker moves the cost out of the scope
  being measured and into someone else's. The gate fails on it.
- The superseded code is gone. `grep` proves it, and the proof goes in the report.

**Terminal versus transition.** A pass that leaves both the old path and the new
one standing (an adapter, a shim, a duplicated call site behind a flag) is a
*transition*, not a reduction. It may be the right move on a large migration, and
it is then reported as a transition with the owner and the condition under which
the old path goes away. A transition never claims the reduction. Only the commit
that deletes the old path does.

### 6. Commit shape and report

- **Pure moves in their own commit.** A commit that moves code and a commit that
  edits it are both reviewable. One commit that does both is not. `git mv`, then
  edit.
- **One pass per commit** where the passes are independent. A reviewer checking
  a deduplication does not want to read a comment rewrite.
- Report: what was measured before, what was measured after, which trades were
  made, what behavior evidence exists, which findings were classified as
  behavior-changing and left alone, and, if it applies, what is still a
  transition and who owns finishing it.

## When to stop

- The next rung requires a behavior change. Stop, report it, leave it.
- The mechanism gate comes back net-neutral twice in a row. The target is
  already near its minimum, or the wrong thing is being measured. Either way,
  more editing is churn.
- The diff passes roughly 400 lines. Split it. A cleanup nobody can review is
  a cleanup nobody can trust.
- A pass wants a path from *Never touch*. That is not a cleanup, and it needs
  the skill that owns the path.
