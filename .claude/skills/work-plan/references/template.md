# Work Plan Template

The work plan is the **only** home for work items. The design says what is being
built and the test plan says how it is proved. This document says what has to be
done, in what order, and what can run at the same time. Each item is written to
become one GitHub issue without editing.

Two rules make the document survive contact with the work:

- **Dependencies are declared per item. Waves are derived from them**, by
  `scripts/check-work-plan.py`, and are never maintained by hand. A hand-written
  wave number is wrong one commit after the first dependency changes.
- **The plan is not a status board.** It is written once, checked, and filed as
  issues. After that the issues carry the status and this document is history.
  Do not add progress columns or tick boxes off in place of closing an issue.

Copy the skeleton, drop what does not apply. Bracketed `<…>` is a placeholder.
The HTML comments say what belongs there and are not part of the output.

---

````markdown
# Work Plan: <Feature Name>

Related design: [`designs/design-<slug>.md`](../designs/design-<slug>.md)
Test plan: [`tests/test-plan-<slug>.md`](../tests/test-plan-<slug>.md)

**Author:** <Name (handle)>
**Date:** <YYYY-MM-DD>
**Design status at split:** <Accepted | Draft — the section citations may be stale>
**Issues:** <not yet filed | filed YYYY-MM-DD, #<first>–#<last>>

Work item IDs (`W-01`) are permanent and are cited from issue bodies and commit
messages, so they are never renumbered. Each item below is one GitHub issue: the
description and acceptance criteria are the issue body verbatim.

`Depends on` is ordering the work requires to be correct. `Conflicts with` is
ordering the mechanics require: two items that rewrite the same file, or that
both regenerate the same manifests, are serialized even when neither needs the
other's result. §1 is derived from both.

---

## 1. Execution Order

<!-- Generated. Run `.claude/skills/work-plan/scripts/check-work-plan.py
     <this file> --write` to fill or refresh this section, and never edit the
     three tables by hand. -->

### Waves

| Wave | Items | Runs after |
|---|---|---|
| 1 | W-01, W-02 | — |
| 2 | W-03 | wave 1 |

### Critical path

W-01 → W-03 → W-07 (3 items, weight 7)

### Serialization notes

| Items | Reason |
|---|---|
| W-04, W-05 | both add a field to `storagenodeops_types.go` and regenerate the CRDs |

---

## 2. External Prerequisites

<!-- Only when the design has a Phase 0 table. Do not restate it: name each
     blocker, link the design row, and list the items that cannot start. When
     the design has no Phase 0 table, delete this section. -->

| Prerequisite | Blocks | Design |
|---|---|---|
| `sbcli` endpoint `POST /volume/<id>/freeze` | W-06, W-08 | design § Phase 0 |

An item blocked only on an external prerequisite is still filed. It carries the
`blocked` label and names the prerequisite in its body, because an issue nobody
can start is still work somebody has to schedule.

---

## 3. Work Items

<!-- One `###` block per item, in ID order, which is not necessarily execution
     order. Every field below is required except `Conflicts with`. -->

### W-01 — <Imperative title, under 70 characters>

**Depends on:** —
**Conflicts with:** —
**Labels:** operator, enhancement, high
**Design:** §5.2, §6.1
**Scenarios:** U-14, U-15
**Size:** S

<!-- One or two paragraphs, imperative, self-contained. Someone who has not read
     the design has to be able to start from this text plus the linked sections:
     name the files, the types, the endpoints, and the decision already made.
     Do not write "as discussed" or "see design" in place of saying the thing. -->

<Add `subPhase` to `StorageNodeOpsStatus` in
`operator/api/v1alpha1/storagenodeops_types.go` as a typed
`StorageNodeOpsSubPhase string` with its constants beside it, then regenerate
the manifests. The untyped variant on `StorageNode.status.actionStatus` stays as
it is; this item does not touch it.>

**Acceptance criteria**

<!-- Checkable from the outside: a field exists, a test passes, a phase
     transition is rejected, a metric is exported. Never "code reviewed" or
     "works correctly". -->

- [ ] <`StorageNodeOpsStatus.SubPhase` exists, typed, with kubebuilder enum markers>
- [ ] <`make -C operator manifests generate` produces no further diff>
- [ ] <`U-14` and `U-15` pass>

### W-02 — <…>

**Depends on:** —
**Conflicts with:** W-01
**Labels:** atlas-lib, enhancement, medium
**Design:** §7
**Scenarios:** U-20
**Size:** M

<…>

**Acceptance criteria**

- [ ] <…>

---

## 4. Not in This Plan

<!-- What a reader will look for and not find, with the reason. This is the
     section that stops the plan being read as the whole scope of the design. -->

| Deferred | Why | Where it goes |
|---|---|---|
| <Multi-cluster fan-out> | <design §9 marks it Phase 3> | <a later work plan> |
````

---

## Field rules

| Field            | Rule                                                                                                                                                                                                                                   |
|------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| ID               | `W-nn`, two digits, permanent, assigned in the order written. Never renumbered, never reused                                                                                                                                           |
| Title            | Imperative and specific. Under 70 characters, because it is the issue title. `Add`, `Split`, `Replace`, `Delete`, not `Improve` or `Refactor`                                                                                          |
| `Depends on`     | Item IDs, or `—`. Only what the item genuinely cannot start without                                                                                                                                                                    |
| `Conflicts with` | Item IDs that must not be in flight at the same time, with the reason recorded in §1's serialization table. Omit the field when there are none                                                                                         |
| `Labels`         | Exactly one component (`operator`, `csi`, `atlas-lib`), one type (`enhancement`, `bug`, `documentation`), one priority (`critical`, `high`, `medium`, `low`), plus optional `blocked` and `security`. `gh label list` is the authority |
| `Design`         | The design sections that specify the item, as `§n.m`. An item with no design section is either missing from the design or not in scope                                                                                                 |
| `Scenarios`      | Test plan IDs the item makes pass, or `—` when it is not directly testable. The plan and this column are edited together                                                                                                               |
| `Size`           | `S` a session, `M` a day or two, `L` longer, which is a reason to split it before filing rather than a size to file                                                                                                                    |

## What makes an item one item

`SKILL.md` §2 carries this: one issue, one branch, one reviewable pull request,
tests inside the item, regeneration as an acceptance criterion, and where to
split and where to merge. It is not repeated here.

## Checking it

```bash
.claude/skills/work-plan/scripts/check-work-plan.py \
  operator/docs/tasks/work-plan-<slug>.md            # validate and report
.claude/skills/work-plan/scripts/check-work-plan.py <file> --write   # refresh §1
.claude/skills/work-plan/scripts/check-work-plan.py <file> --issues  # emit the gh script
```

The checker rejects a dependency on an unknown ID, a dependency cycle, a
duplicate ID, a label outside the repository's set, an item with no acceptance
criteria, and a §1 that disagrees with the declared dependencies. `--issues`
writes a `gh issue create` script to standard output in wave order, substituting
real issue numbers into each `Blocked by` line as it goes. It never runs itself.
