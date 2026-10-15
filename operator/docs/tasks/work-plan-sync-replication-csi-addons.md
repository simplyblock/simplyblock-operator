# Work Plan: Synchronous Replication through csi-addons

Related design: [`designs/design-sync-replication-csi-addons.md`](../designs/design-sync-replication-csi-addons.md)
Test plan: [`tests/test-plan-sync-replication-csi-addons.md`](../tests/test-plan-sync-replication-csi-addons.md)

**Author:** Geoffrey (geoffrey1330)
**Date:** 2026-10-15
**Design status at split:** Draft (the section citations may be stale)
**Issues:** not yet filed

Work item IDs (`W-01`) are permanent and are cited from issue bodies and commit
messages, so they are never renumbered. Each item below is one GitHub issue: the
description and acceptance criteria are the issue body verbatim.

`Depends on` is ordering the work requires to be correct. `Conflicts with` is
ordering the mechanics require. §1 is derived from both.

This plan covers Phase 1 (the per-volume `VolumeReplication` path) and the one
csi-driver teardown fix a pod move depends on. Phase 2 (the consistency-group
variant, design §14) gets its own plan. The design's baseline node path is
already built and unit-tested (design Implementation Status). These items are the
switch from the superseded patch-the-class signal to the mutable site annotation
(design §6.2), and are the design's remaining Phase 1 work.

---

## 1. Execution Order

<!-- Generated. Run `.claude/skills/work-plan/scripts/check-work-plan.py
     <this file> --write` to refresh this section, and never edit the
     three tables by hand. -->

### Waves

| Wave | Items      | Runs after |
|------|------------|------------|
| 1    | W-01, W-04 | —          |
| 2    | W-02, W-03 | wave 1     |

### Critical path

W-01 → W-02 (2 items, weight 3)

### Serialization notes

No items conflict. Every wave above can run fully in parallel.

---

## 2. External Prerequisites

| Prerequisite                                                                                                                                            | Blocks     | Design                  |
|---------------------------------------------------------------------------------------------------------------------------------------------------------|------------|-------------------------|
| The v2 sync-replication REST routes (`failover`, `demote`, `replication/status`) and the site query parameter, on the `sbcli` `sync-replication` branch | W-02, W-03 | design § Phase 0 (P0-1) |

P0-1 is on the branch and not yet released, so it is available for development and
testing but not for a shipped build. The repository has no `blocked` label, so the
items that consume these routes carry the prerequisite in their body rather than a
label.

---

## 3. Work Items

### W-01 — Add the sync-site annotation and handle resolver to atlas-lib

**Depends on:** —
**Conflicts with:** —
**Labels:** atlas-lib, enhancement, high
**Design:** §5.2, §6.2
**Scenarios:** U-47
**Size:** S

Add to `atlas-lib/kube` the shared primitives both the node and the controller
need to carry the active site on the `VolumeReplication` object rather than in its
class. Declare the annotation key `simplyblock.io/sync-site` as a constant, extend
`VolumeReplicationRef` (in `volumereplication.go`) to carry the annotation's value,
and add a resolver `VolumeReplicationForVolumeHandle` that maps a backend volume
handle to its `VolumeReplication` by the existing chain (the `PersistentVolume`
whose `csi.volumeHandle` matches, its `claimRef` to the `PersistentVolumeClaim`,
then the `VolumeReplication` whose `dataSource` names that PVC). The node already
resolves a claim to its `VolumeReplication` through `SyncVolumeReplicationForClaim`;
this adds the handle-keyed path the controller's `PromoteVolume` and `DemoteVolume`
need (W-03), and the annotation field both read.

**Acceptance criteria**

- [ ] `kube.SyncSiteAnnotation` constant equals `simplyblock.io/sync-site`.
- [ ] `VolumeReplicationRef` carries the annotation's value, extracted from the object's metadata.
- [ ] `VolumeReplicationForVolumeHandle` returns the `VolumeReplication` for a volume handle, and `errs.ErrNotFound` when none protects it.
- [ ] `U-47` passes; `make -C atlas-lib test` is green.

### W-02 — Patch the VolumeReplication site annotation at NodeStageVolume

**Depends on:** W-01
**Conflicts with:** —
**Labels:** csi, enhancement, high
**Design:** §6.2, §6.7
**Scenarios:** U-42, U-43, U-44, U-45
**Size:** M

Replace the superseded class re-point in the csi-driver node path. In
`csi-driver/internal/csi/node` (`stage_sync.go`, `syncwiring.go`), delete
`patchVolumeReplicationToSite` and the class-by-site selection (the
`VolumeReplicationClassForSite` lookup), and instead have `NodeStageVolume` patch
the correlated `VolumeReplication`'s `simplyblock.io/sync-site` annotation to this
node's site and `spec.replicationState` to `primary`. The patch is idempotent: a
`VolumeReplication` already carrying this site and `primary` is left unchanged and
staging proceeds to the existing gate. The node RBAC is unchanged: it already holds
`patch` on `volumereplications` and needs no `create` or `delete`.

This item carries Open Question 3 (design §6.3, §16): on a move the node changes
only the annotation, since `replicationState` is already `primary`. Verify against
the vendored csi-addons that an annotation-only change re-invokes `PromoteVolume`;
if it does not, re-assert `replicationState: primary` in the same patch to force the
reconcile, which is a no-op for the administrator's ownership.

**Acceptance criteria**

- [ ] `NodeStageVolume` patches `simplyblock.io/sync-site` and `replicationState`, and never changes `spec.volumeReplicationClass`.
- [ ] `patchVolumeReplicationToSite` and the class-by-site selection are gone from the node package.
- [ ] A re-stage of a volume already carrying this site and `primary` issues no patch and proceeds to the gate.
- [ ] A move re-invokes `PromoteVolume` (annotation-only, or with `replicationState` re-asserted if OQ3's verification requires it).
- [ ] `U-42`, `U-43`, `U-44`, and `U-45` pass; `make -C csi-driver unit-test` is green.

### W-03 — Read the site annotation in PromoteVolume and DemoteVolume

**Depends on:** W-01
**Conflicts with:** —
**Labels:** csi, enhancement, high
**Design:** §5.2, §6.3
**Scenarios:** U-46
**Size:** M

Change the driver's controller to take the sync write site from the object instead
of the class. In `csi-driver/internal/csi/controller/replication_sync.go`, have
`PromoteVolume` and `DemoteVolume` resolve the volume to its `VolumeReplication`
(W-01) and read the `simplyblock.io/sync-site` annotation for the `?site=` of the
backend route, and drop the `site` class parameter and the `siteParam` constant
from `resolveMethod` and the dispatch. A sync cluster now has one site-agnostic
`VolumeReplicationClass` (design §9.1), so update the sample class and any shipped
example to the single class.

**Acceptance criteria**

- [ ] `PromoteVolume` and `DemoteVolume` address `?site=` from the `simplyblock.io/sync-site` annotation, not from a class parameter.
- [ ] `resolveMethod` no longer reads or requires a class `site` parameter, and the `siteParam` constant is gone.
- [ ] The shipped sample `VolumeReplicationClass` is a single `method: sync` class with no `site`.
- [ ] `U-46` passes; `make -C csi-driver unit-test` is green.

### W-04 — Make NodeUnstageVolume teardown bounded and context-independent

**Depends on:** —
**Conflicts with:** —
**Labels:** csi, bug, high
**Design:** §12
**Scenarios:** U-48, U-49
**Size:** M

Fix the two teardown faults a live pod move exposed, both of which wedge
`NodeUnstageVolume` on the old node and so block the volume from detaching and
re-attaching on the new one (design §12, Open Question 4). Bound and force the
NVMe-oF disconnect in `csi-driver/internal/csi/node` so a disconnect that cannot
complete is abandoned within a deadline rather than hanging the unstage, and make
`teardownPlan` identify the volume's namespace from the stack record when the
stashed volume context is missing, so the unstage completes instead of returning
`Internal`. This is independent of the site-annotation switch and can land in
parallel.

**Acceptance criteria**

- [ ] The NVMe-oF disconnect in the unstage path is bounded by a deadline and forced, and cannot block the RPC indefinitely.
- [ ] `NodeUnstageVolume` completes when the stashed volume context is absent, identifying the namespace from the stack record.
- [ ] `U-48` and `U-49` pass; `make -C csi-driver unit-test` is green.

---

## 4. Not in This Plan

| Deferred                                                                                              | Why                                                                                                                                 | Where it goes                                      |
|-------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------|
| The `sbcli` sync backend routes, the status-route site-optional fix, and the birth-state promote gate | A different repository (`sbcli`); the routes are the design's Phase 0 prerequisite (P0-1), and the two fixes are already made there | The `sbcli` repository                             |
| Lost-site fencing under the automatic force                                                           | Open Question 1 (design §16): a backend guarantee, not a csi-driver change                                                          | Backend team                                       |
| Stale VR cache at NodeStage                                                                           | Open Question 2 (design §16, §6.6): the node trusts the cache today, a bounded pre-existing choice                                  | A later refinement                                 |
| The follow-pod E2E harness (one class, the site annotation)                                           | The regression test lives in the `sbcli` repository, not here                                                                       | The `sbcli` repository                             |
| The consistency-group variant                                                                         | Phase 2 (design §14)                                                                                                                | `work-plan-sync-replication-csi-addons-phase-2.md` |
