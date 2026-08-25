---
name: work-plan
description: Break a settled design into dependency-ordered work packages in operator/docs/tasks/work-plan-<slug>.md, each ready to become one GitHub issue, then derive which items can run in parallel and which must be serialized. Use when asked for a work plan, work breakdown, task list, implementation plan, work packages, or milestones for a design, when asked what can be worked on in parallel or what blocks what, and when asked to file the work as GitHub issues. Also use to amend or supersede an existing work plan after its design changed.
---

# Work plans

A work plan turns a design into the work: one item per unit of work, each ready
to be filed as a GitHub issue without editing, with the ordering constraints
declared so that the parallelism is derived rather than guessed.

| Document   | Path                                       | Owned by            |
|------------|--------------------------------------------|---------------------|
| Design doc | `operator/docs/designs/design-<slug>.md`   | `design-doc`        |
| Test plan  | `operator/docs/tests/test-plan-<slug>.md`  | `design-doc`        |
| Work plan  | `operator/docs/tasks/work-plan-<slug>.md`  | this skill          |

Reference material:

- `references/template.md` — the skeleton, the per-item field rules, and what
  makes one item one item.
- `scripts/check-work-plan.py` — validates the plan, derives the waves and the
  critical path, and emits the `gh issue create` script.
- `design-doc/references/conventions.md` — the shared house conventions these
  documents follow: file naming, the metadata block, section numbering.

## 0. Is the design ready to be split?

**This step is the reason the work plan is a separate skill.** A design is
reviewed and revised before it is sound, and every work item cites the design's
sections and depends on its decisions. Splitting a design that is still moving
produces items naming sections that renumber under them and packages a later
revision merges or deletes, and the cost is not the wasted draft. It is that the
stale plan gets filed as issues and then worked from.

Five checks. Each has an answer for what a failure means.

| Check                      | Ready when                                                         | A failure means                                                                   |
|----------------------------|--------------------------------------------------------------------|-----------------------------------------------------------------------------------|
| **Status**                 | `Accepted`, `Partially Implemented`, or a phase-implemented status | `Draft` and `Proposed` are still under review; the shape can still change         |
| **Open questions**         | none that would change the decomposition                           | see the distinction below — this is the check that takes judgment                 |
| **Section numbering**      | the numbered sections are settled                                  | items cite `§n.m`, and a renumber silently invalidates every citation in the plan |
| **Test plan**              | exists, with its scenario matrix                                   | the `Scenarios` column would be invented, and an item's proof is part of the item |
| **External prerequisites** | the design's `## Phase 0` table is complete, or there are none     | prerequisites surface as mid-plan surprises rather than as `blocked` items        |

**Which open questions matter.** Not all of them block a split, and treating them
as equivalent is how a plan never gets written:

- **Blocking**, because they change the item boundaries: whether this is one CRD
  or two, whether a phase exists at all, which component owns a behavior, whether
  a control-plane endpoint will exist.
- **Not blocking**, because they change an item's content and not its existence:
  a field's name, a default value, a threshold, an event reason, the wording of a
  condition. An item can be filed with the question named in its body.

**When the design is not ready**, say which check failed and what would clear it,
and stop. Do not write half a plan. If the user asks for the breakdown anyway,
which is legitimate for planning and estimation, write it and record the state it
was split from in the `**Design status at split:**` line, so a reader knows the
citations may be stale.

## 1. Derive the items from the design and the code

Never enumerate items from the design's prose alone, and never from the
conversation. An item to add a field that already exists is worse than no item.

- **The design's phasing table** bounds the plan. One work plan covers one phase
  unless the phases are small. A later phase gets its own plan.
- **The design's section list** is the item source. Every numbered section
  describing unimplemented behavior owes at least one item, and an item with no
  design section is either out of scope or a gap in the design, so report it
  rather than inventing the design decision in the plan.
- **The code** decides what is left. Grep `operator/api/v1alpha1/` for the
  fields, `operator/internal/controller/` for the reconcilers, and `atlas-lib`
  for the primitive before writing an item that adds any of them.
- **The test plan** supplies the `Scenarios` column. Use the IDs it already
  defines. When an item needs a scenario that does not exist, the test plan owes
  a row, which is a `design-doc` edit and not something to paper over here.
- **`## Phase 0 — External Prerequisites`** becomes §2 of the plan and the
  `blocked` label on the items that wait for it.

## 2. What makes one item one item

**One item is one issue, one branch, and one reviewable pull request.**

- **The tests that prove an item are part of that item**, never a follow-up item.
  A test item is the item that gets dropped when the schedule tightens.
- **Regeneration is never its own item.** `make -C operator manifests generate`
  and `make helm-sync` belong to the item that made them necessary, as an
  acceptance criterion. See `build-system`.
- **Split** when an item would touch two components or need two review
  audiences. The `atlas-lib` boundary is the one that most often means two items,
  and `extract-to-atlas-lib` describes the sequence such a split has to follow:
  the library lands first, then each consumer adopts, then the copies go.
- **Merge** when an item's only acceptance criterion is that another item
  compiles.
- **A CRD change is its own item**, because `api-design` governs what is
  breaking and the regeneration lands with it.

## 3. The two kinds of ordering

Declaring only the first kind is the most common defect in a work plan, and the
second kind is what turns a parallel wave into a merge conflict.

| Field            | Means                              | Example                                                                    |
|------------------|------------------------------------|----------------------------------------------------------------------------|
| `Depends on`     | ordering **correctness** requires  | the reconciler conversion cannot start before the state graph exists       |
| `Conflicts with` | ordering the **mechanics** require | two items that both add a field to one `_types.go` and regenerate the CRDs |

A conflict is not a dependency: either item may go first, but not both at once.
Name the reason in the plan's serialization table, because a reader who cannot
see why two items conflict will parallelize them anyway.

## 4. Write it

Follow `references/template.md`, which carries the field rules.

**Work item IDs** are `W-nn`, two digits, permanent, assigned in the order the
items are written and never renumbered or reused: they are cited from issue
bodies, branch names, and commit messages, and an ID outlives the plan that
introduced it. They live only in the work plan, the way `U-`, `I-`, `E-`, and
`M-` live only in the test plan.

Item fields are bold-key lines directly under the heading, one per line, read by
the checker:

```markdown
### W-03 — Add a typed subPhase to StorageNodeOpsStatus

**Depends on:** W-01, W-02
**Conflicts with:** —
**Labels:** operator, enhancement, high
**Design:** §5.2
**Scenarios:** U-42
**Size:** S
```

Two rules about content:

- **Labels come from the repository, not from imagination.** Exactly one
  component (`operator`, `csi`, `atlas-lib`), one type (`enhancement`, `bug`,
  `documentation`), and one priority (`critical`, `high`, `medium`, `low`), plus
  optional `blocked` and `security`. `gh label list` is the authority. The
  checker's copy of the set is a convenience that can go stale.
- **Acceptance criteria are checkable from outside the item.** A field exists, a
  test passes, a transition is rejected, a metric is exported, a regeneration
  produces no diff. Never "reviewed" and never "works correctly."

## 5. Derive the execution order

**Never hand-write the waves.** Declare the dependencies per item and generate
section 1:

```bash
.claude/skills/work-plan/scripts/check-work-plan.py \
  operator/docs/tasks/work-plan-<slug>.md --write
```

The checker rejects an unknown or circular dependency, a duplicate ID, a label
outside the repository's set, and an item with no acceptance criteria or no
description. **It has to pass with zero errors.**

The run also reports the **critical path**, the honest answer to "how long is
this." It is the longest chain of items that cannot be done in parallel, and the
wave count can exceed its length when a conflict serializes two
otherwise parallel items, and that difference is worth reporting, because it is
mechanical serialization that better item boundaries might remove.

## 6. Filing the issues

**Filing is a separate, explicit act.** `--issues` writes a `gh issue create`
script to standard output, in wave order, substituting each created issue number
into the `Blocked by` line of the items that depend on it:

```bash
.claude/skills/work-plan/scripts/check-work-plan.py \
  operator/docs/tasks/work-plan-<slug>.md --issues > /tmp/file-issues.sh
```

It creates nothing itself. Hand the script over, or run it only when the user
asks for the issues to be filed, and say how many issues it would create before
running anything. After they are filed, record the range in the plan's
`**Issues:**` line.

## Before handing the work back

1. `check-work-plan.py` reports zero errors, and every warning it raised is
   either resolved or reported with its reason.
2. Section 1 was written by `--write`, not by hand.
3. Every `Design` section and every `Scenarios` ID an item cites exists in the
   companion documents. A `Scenarios` value the test plan does not define means
   one of the two documents is missing a row.
4. No item adds something the code already has, checked by grep rather than
   recalled.
5. The design links to the plan through a `**Work Plan:**` line in its metadata
   block, and the plan links back to both companions.
6. `.claude/skills/house-style/scripts/quality-gate.sh --paths <the plan>` passes
   with zero errors, and every punctuation warning outside a code block or a
   table is fixed or justified in writing. The `design-doc` skill's step 6 states
   that rule in full, and it applies here unchanged.
7. The reply says how many items, how many waves, the critical path length, and
   which items are blocked on external prerequisites.

## After it is filed

**A work plan whose issues are filed is history.** The issues carry the status
and the document records what was planned. Do not add progress columns, do not
tick acceptance criteria in place of closing an issue, and do not delete an item
that turned out to be unnecessary. Say so in the item and close its issue.

**When the design changes under a filed plan**, the choice is between two moves,
and the deciding question is whether the original scope is still in flight:

- **Amend** while items remain open: append items with the next free IDs, mark a
  superseded item struck through with the ID that replaced it, and re-run
  `--write`, because appended items change the waves.
- **Supersede** once the plan's issues are closed: start
  `work-plan-<slug>-phase-2.md` and leave the first plan as the record of what
  was built.

Either way, an ID is never reused and never renumbered, and the design's
`**Work Plan:**` line points at the current plan.
