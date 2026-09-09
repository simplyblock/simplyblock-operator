# Design Document: Consistency Groups

**Status:** Draft  
**Author:** Israel Geoffrey (geoffrey1330)  
**Date:** 2026-09-09  
**Test Plan:** [`tests/test-plan-consistency-groups.md`](../tests/test-plan-consistency-groups.md)

---

## Phasing Overview

| Phase       | Status  | Scope                                                                                                                                                                                            | Sections           |
|-------------|---------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------|
| **Phase 1** | Planned | A standalone consistency group in the control plane: membership at provisioning, a group snapshot as one crash-consistent generation, and a listing that shows which snapshots belong to a group | §4, §5, §6, §7, §8 |
| **Phase 2** | Planned | The Kubernetes-native surface: a `VolumeGroupSnapshot` snapshots the group through the CSI GroupController service                                                                               | §5.3, §9, §10      |

Phase 1 stands alone: it decouples the consistency group from the replication policy it is bolted onto today, and it makes a group snapshot and its member snapshots first-class and legible through `sbctl`. Phase 2 puts the Kubernetes `VolumeGroupSnapshot` surface on top of the same backend group. Replication and backup of a consistency group are out of scope for this design and are noted as future work in §2.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                                                                      | Kind                    | Blocks             | Status                                                                                                         |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|--------------------|----------------------------------------------------------------------------------------------------------------|
| P0-1 | `bdev_lvol_snapshot_group`: one frozen SPDK call that snapshots every member of a logical volume store at a single point in time                                                                                  | Storage plane (SPDK)    | Phase 1            | Shipped (verified 2026-09-07)                                                                                  |
| P0-2 | A standalone consistency-group backend: a group that exists without a replication policy, with create, member-remove, snapshot take, snapshot delete, and a group-aware snapshot listing, all scoped to a cluster | Control plane (`sbcli`) | Phase 1            | Partial: a policy-coupled group exists on the `replication-features` branch (§1); the standalone form does not |
| P0-3 | Volume-create accepts a `consistency_group` field and, inside one atomic create, ensures the group, joins the volume, and enforces placement                                                                      | Control plane (`sbcli`) | Phase 1 membership | Not shipped as a standalone path                                                                               |
| P0-4 | external-snapshotter `VolumeGroupSnapshot` CRDs (`v1beta1`) installed, and the `CSIVolumeGroupSnapshot` feature gate enabled on both the `snapshot-controller` and the `csi-snapshotter` sidecar                  | Ecosystem / Kubernetes  | Phase 2            | Images at `v8.2.0` support it, but the CRDs and the gate are not enabled today                                 |

P0-1 is the one primitive the whole design rests on, and it is live. P0-2 and P0-3 are the backend work that turns the existing policy-coupled group into a standalone object and lets a volume join a group at creation. Without P0-4 the Phase 2 CSI GroupController has no Kubernetes objects to reconcile, so Phase 1 (the backend group plus `sbctl`) is the whole feature until the gate is turned on.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [Group Lifecycle](#4-group-lifecycle)
5. [Snapshotting a Consistency Group](#5-snapshotting-a-consistency-group)
6. [Representing Group Snapshots in the Control Plane](#6-representing-group-snapshots-in-the-control-plane)
7. [Cloning a Consistency Group](#7-cloning-a-consistency-group)
8. [Membership Changes and Snapshot Validity](#8-membership-changes-and-snapshot-validity)
9. [CSI GroupController](#9-csi-groupcontroller)
10. [Backend API Requirements](#10-backend-api-requirements)
11. [Scenarios](#11-scenarios)
12. [Failure Modes and Fallback](#12-failure-modes-and-fallback)
13. [Observability](#13-observability)
14. [Testing Strategy](#14-testing-strategy)
15. [Migration Strategy](#15-migration-strategy)
16. [Open Questions](#16-open-questions)

---

## Overview

A consistency group is a set of volumes that snapshot as one crash-consistent unit. A single frozen backend call (`bdev_lvol_snapshot_group`) takes one snapshot of every member at the same point in time, so a database whose data and write-ahead log live on separate volumes can be restored to a state that actually existed rather than to two moments that do not agree.

This design makes the consistency group a first-class concept in its own right. A group is named by a **label on its member PVCs**, `storage.simplyblock.io/consistency-group`. The group is born from the first volume that carries the label, its members are pinned to one storage node and logical volume store so the frozen snapshot is possible, and it dies with its last member. A **`VolumeGroupSnapshot`** is the Kubernetes representation of one snapshot of the group, and each snapshot it produces is one **generation** the control plane can list, clone, and reason about by membership.

| Concern          | Mechanism                                                | Decided when                             |
|------------------|----------------------------------------------------------|------------------------------------------|
| Group membership | PVC label `storage.simplyblock.io/consistency-group`     | Volume creation, one-way                 |
| Group placement  | First member's node and logical volume store             | Volume creation, immutable for the group |
| A group snapshot | `VolumeGroupSnapshot` (Kubernetes) or `sbctl` (headless) | On demand                                |
| One generation   | `group_seq` on every member snapshot of that snapshot    | At snapshot time                         |

Replication and backup of a consistency group are deliberately not part of this design (§2). A reader who stops here has the model: a persistent group defined by a label, snapshotted as a `VolumeGroupSnapshot`, with each snapshot a generation the control plane represents by its membership.

---

## 1. Background

**The SPDK primitive is live.** `bdev_lvol_snapshot_group` freezes I/O across a set of volumes on one logical volume store, snapshots them all, and unfreezes, producing one crash-consistent set of snapshots (P0-1, verified on a live cluster 2026-09-07). Everything else in this design is about giving that primitive a first-class identity, a Kubernetes surface, and a legible representation.

**Consistency groups exist today only as a sub-feature of replication policies.** On the `replication-features` and `retain_source_lvolID` branches (deployed to the test cluster, not on the branch currently checked out), a `ConsistencyGroup` record is created *by* a replication policy (`create_group_for_policy`), and its snapshots are taken on the policy's replication cadence. The record already carries the shape this design needs:

- `members`, a map of lvol id to a membership epoch `{joined_seq, removed_seq}`.
- `last_group_seq`, a monotonic generation counter.
- `node_id` and `lvs_name`, the pinned placement every member shares.
- `included_in_seq(lvol_id, seq)`, which decides whether a member belongs to a generation.

But it also carries `policy_id`, and the group is created, snapshotted, and deleted through the policy. `create_group_snapshot(policy_id)` names each member snapshot `repl_cg_<group8>_<seq>_<lvol8>_<ts>` and stamps `group_id` and `group_seq` on the `SnapShot` record. This design keeps the membership and generation model and removes the policy from the middle of it.

**The representation gap.** The `SnapShot` model carries `group_id` and `group_seq`, but `list_snapshots` does not surface either. A group snapshot appears in `sbctl snapshot list` as an ordinary snapshot, and the only way to tell that a snapshot belongs to a group, or which generation it is, is to parse the `repl_cg_` name. The consistency-group regression test has to do exactly that. Making group membership legible in the listing is a first-class goal of this design (§6), because a snapshot a user cannot identify as part of a group is a snapshot they cannot safely restore as part of one.

---

## 2. Goals and Non-Goals

### Goals

- A set of volumes snapshots as one crash-consistent generation, verified by hashed cross-volume data rather than by timestamps.
- Group membership is declared by a PVC label and requires no new simplyblock CRD.
- A group is born from its first member volume and deleted with its last, with no separate create or delete step for the common Kubernetes path.
- Members are colocated on one storage node and logical volume store, which the frozen group snapshot requires, and a volume that cannot be colocated fails creation loudly.
- Membership is one-way: a volume's membership window is fixed at creation and closes permanently on removal, so generation math never reasons about gaps in one volume's history.
- The control plane represents a group snapshot as a listable generation, and every member snapshot names its group and generation, so a snapshot's group membership is answerable without parsing a name (§6).
- A group snapshot is restorable per member, and the restored set is crash-consistent because the source generation was frozen (§7).
- A member that leaves the group does not invalidate the generations that already contain it (§8).

### Non-Goals

- **Replication of a consistency group.** Cross-cluster replication and fail-over of a group are future work. This design leaves the `ConsistencyGroup` record independent of any replication policy so that work can attach later without re-shaping the group.
- **Backup of a consistency group.** S3 or object backup of a group's generations is future work, on the same independent record.
- **Per-member snapshot policies.** A generation is a property of the whole group, not of any single member.
- **Group-wide restore as a single Kubernetes operation.** The CSI specification has no group-restore verb, so restore is per member (§7). A single-call restore exists only on the headless `sbctl` path, and only as a convenience.
- **Ad-hoc groups over arbitrary volumes.** A group's members must share one logical volume store, so a label applied to volumes scattered across nodes cannot form a group. Placement is decided at provisioning (§4.2).
- **A simplyblock ConsistencyGroup CRD.** The group's Kubernetes identity is the label, and its lifecycle is driven by volume creation and deletion. A CRD would add a second source of truth for membership that the label already owns.

---

## 3. Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────────┐
│                        Kubernetes Control Plane                           │
│                                                                           │
│   ┌──────────────────────────────┐   ┌─────────────────────────────────┐ │
│   │   CSI provisioner (spdkcsi)  │   │  CSI GroupController (spdkcsi)   │ │
│   │  1. read PVC label           │   │  advertises                     │ │
│   │     storage.simplyblock.io/  │   │  GROUP_CONTROLLER_SERVICE        │ │
│   │     consistency-group        │   │  CreateVolumeGroupSnapshot       │ │
│   │  2. pass consistency_group   │   │  DeleteVolumeGroupSnapshot       │ │
│   │     to volume create         │   │  GetVolumeGroupSnapshot          │ │
│   └──────────────────────────────┘   └─────────────────────────────────┘ │
│                                          ▲ driven by the csi-snapshotter  │
│                                          │ sidecar + VolumeGroupSnapshot   │
│  PVC label  storage.simplyblock.io/consistency-group                      │
│  VolumeGroupSnapshot / VolumeGroupSnapshotContent   (Phase 2)             │
│  materialized VolumeSnapshot per member, backref to the VolumeGroupSnapshot│
└──────────────────────────────────────────────────────────────────────────┘
              │ HTTP (webapi client, service-account bearer token)
┌─────────────▼──────────────────────────────────────────────────────────┐
│                        simplyblock Backend API                          │
│  POST   /api/v2/clusters/{id}/storage-pools/{pid}/volumes                │
│           body carries consistency_group  (ensure group, join, place)   │
│  POST   /api/v2/clusters/{id}/consistency-groups/{gid}/snapshots         │
│  GET    /api/v2/clusters/{id}/consistency-groups/{gid}/snapshots         │
│  DELETE /api/v2/clusters/{id}/consistency-groups/{gid}/snapshots/{seq}   │
│  GET    /api/v2/clusters/{id}/snapshots?consistency_group={gid}          │
└──────────────────────────────────────────────────────────────────────────┘
              │ JSON-RPC
┌─────────────▼──────────────────────────────────────────────────────────┐
│                        Storage node (SPDK)                              │
│  bdev_lvol_snapshot_group   one frozen snapshot per member (P0-1)       │
└──────────────────────────────────────────────────────────────────────────┘
```

**The label is the only membership source of truth.** The CSI provisioner reads it and passes it to the backend, which decides the group. Nothing in Kubernetes stores group membership separately, so there is no second source of truth to disagree with the backend.

**There is no operator reconciler in this design.** Membership is set by the provisioner at volume creation, and snapshots are taken by the CSI GroupController driven by the csi-snapshotter sidecar. The operator is not in the group-snapshot path.

**The persistent group versus the ephemeral VolumeGroupSnapshot.** Upstream Kubernetes has no persistent group: a `VolumeGroupSnapshot` selects PVCs by label at snapshot time, and the "group" is whatever the selector matched. simplyblock needs a *persistent* backend group, because `bdev_lvol_snapshot_group` requires every member on one logical volume store, and that placement must be arranged at provisioning, not discovered at snapshot time. So a `VolumeGroupSnapshot` in this design snapshots an *existing* backend group, and its selector must resolve to exactly the group's current membership (§9). This is the one place the design departs from the upstream model, and §11 plays the consequence through.

---

## 4. Group Lifecycle

### 4.1 Birth and membership at provisioning

A group is born from the first volume that carries the `storage.simplyblock.io/consistency-group` label. The CSI provisioner reads the label with the volume context that `--extra-create-metadata` already supplies, and passes `consistency_group=<name>` to the backend volume-create call (P0-3). Inside one atomic create the backend ensures the group exists, joins the volume, and enforces placement:

```
PVC labeled storage.simplyblock.io/consistency-group: db-group
   │ CSI CreateVolume, provisioner reads the label
   ▼
volume-create REST call carries consistency_group=db-group
   │
   ▼
backend, atomically inside volume create:
   first labeled volume  ← create the group, pin it to this volume's node
                           and logical volume store, open the member epoch
   later labeled volume  ← place on the pinned node (mandatory), share the
                           NVMe subsystem when possible (best effort),
                           open the member epoch
```

**Membership is fixed at creation.** A label added to a PVC after its volume exists does not join the volume to the group, because the join happens only in the create path. This keeps membership decidable from one event rather than from the mutable state of a label over time.

**Concurrent first volumes converge on one group.** Two volumes created at the same time with the same label must not create two groups. Ensure-group is idempotent by cluster and name (§10), and the volume that loses the race joins the group the winner created, under the winner's placement pin.

### 4.2 Placement is two-tier

The frozen group snapshot operates on one logical volume store, so every member must live on one store on one node.

- **Node and logical volume store colocation is mandatory.** The first member pins the group. A later labeled volume is placed on the pinned node, and if it cannot be placed there the volume create fails. It does not join the group unpinned, and it does not land on another node, because the frozen group snapshot operates on one store and cannot reach a member placed off it.
- **NVMe subsystem colocation is best effort.** A member shares the group's subsystem when the StorageClass is namespaced (`max_namespace_per_subsys` greater than one) and the subsystem has a free namespace slot. When it does not, the member is placed on the pinned node in its own subsystem. Subsystem sharing is an efficiency, not a correctness requirement.

### 4.3 Membership is one-way

A member's epoch opens at creation and closes permanently when the volume leaves the group. The same volume never rejoins. The backend records membership as `{joined_seq, removed_seq}` per volume, and `included_in_seq(lvol_id, seq)` decides whether a member belongs to a generation: `joined_seq <= seq` and (`removed_seq == 0` or `seq <= removed_seq`). A one-way rule keeps those windows unambiguous, and no member has more than one window to reason about.

Re-establishing a member is done by creating a new volume that carries the label, for example, a clone of the removed one. It joins as a new member with a fresh epoch under its own logical volume identity, at `last_group_seq + 1`, so earlier generations do not contain it. The generation math never sees a volume leave and return.

### 4.4 Death with the last member

A group lives exactly as long as its members. Removing the last member deletes the group record. What happens to the group's generations when a member leaves is the subject of §8, and it is the one place where a volume operation can widen into snapshot deletion, so the backend logs and events it.

---

## 5. Snapshotting a Consistency Group

### 5.1 A snapshot is one generation

A group snapshot freezes I/O across every current member, snapshots them all with one `bdev_lvol_snapshot_group` call, and unfreezes. It produces one **generation**: a `group_seq` value stamped on every member snapshot taken, so the set is identifiable and mutually crash-consistent. The operation is all-or-nothing: a failure anywhere unfreezes and rolls back the partial snapshots, and the generation counter does not advance.

The generation is identified as `{group_uuid}:{group_seq}`. Each member snapshot carries `group_id` (the group) and `group_seq` (the generation), which is what §6 surfaces.

### 5.2 Membership at snapshot time

A snapshot includes exactly the members whose epoch is open at the current `group_seq`. A late joiner that joined at generation 5 is absent from generations 1 through 4, and a member removed at generation 7 is absent from generation 8 onward. The snapshot is a faithful cut of the membership as it stands, no more and no less.

### 5.3 The Kubernetes path (Phase 2)

A `VolumeGroupSnapshot` is the Kubernetes representation of one group snapshot:

1. The user creates a `VolumeGroupSnapshot` naming a class and a label selector on `storage.simplyblock.io/consistency-group`.
2. The snapshot-controller resolves the selector to bound PVCs, creates a `VolumeGroupSnapshotContent` with the resolved volume handles, and binds the two.
3. The csi-snapshotter sidecar calls the CSI `CreateVolumeGroupSnapshot` with the handles.
4. The CSI GroupController verifies the handle set equals the group's current membership, then takes one generation through the backend (§9, §10).
5. The snapshot-controller materializes one `VolumeSnapshot` and `VolumeSnapshotContent` per member, each carrying `status.volumeGroupSnapshotName` back to the group snapshot.

The result is one `VolumeGroupSnapshot`, one backend generation, and one `VolumeSnapshot` per member. The materialized per-member `VolumeSnapshot` objects are what a restore consumes (§7).

---

## 6. Representing Group Snapshots in the Control Plane

This is the section the current code does not address, and the one a user feels first.

### 6.1 The gap today

The `SnapShot` model carries `group_id` and `group_seq`, and `create_group_snapshot` stamps both. But `list_snapshots` builds a display dict with `UUID`, `Name`, `LVol ID`, `Base Snapshot`, `Clones`, `Status`, and a few more, and **neither `group_id` nor `group_seq` is among them**. In `sbctl snapshot list`, a group snapshot is indistinguishable from an ordinary one, and the only signal of membership is the `repl_cg_<group8>_<seq>_<lvol8>_<ts>` name. A representation that requires parsing a name is a representation that breaks the moment the name format changes, and it is invisible to `--json` consumers that read fields.

### 6.2 Surface the group on every snapshot

`list_snapshots` and `snapshot get` gain the group fields the model already holds:

- `group_id`: the consistency group the snapshot belongs to, empty for an ordinary snapshot.
- `group_seq`: the generation, zero for an ordinary snapshot.

In the human table these render as one `Group` column (the group's short id or name) and one `Gen` column, shown only when any snapshot in the listing carries a group. In `--json` they are always present, so a consumer reads a field rather than a name. `snapshot list` gains a `--consistency-group <id>` filter, so a caller can list exactly one group's snapshots without client-side name matching.

### 6.3 Represent the generation, not only the member snapshot

A per-snapshot `group_seq` answers "which generation is this member snapshot," but an operator also asks "what generations does this group have, and is each one complete." A group-scoped listing answers that:

`GET /api/v2/clusters/{id}/consistency-groups/{gid}/snapshots` returns one row per generation: the `group_seq`, the creation time, the expected member count (from `included_in_seq` over the membership at that `group_seq`), the present member count, and per-member `{lvol_id, snapshot_id, ready}`. A generation whose present count is below its expected count is incomplete (a member snapshot was pruned or its volume hard-deleted, §8), and the listing says so rather than leaving it to be discovered at restore time.

### 6.4 The Kubernetes representation

On the Kubernetes side, one generation is one `VolumeGroupSnapshot`, and `groupSnapshotHandle` on its `VolumeGroupSnapshotContent` is `{group_uuid}:{group_seq}`. The per-member `VolumeSnapshotContent` objects carry each member's snapshot handle, and the `volumeSnapshotHandlePairList` maps each source volume handle to its member snapshot. A user reading `kubectl get volumegroupsnapshot` sees the generation as one object, and `kubectl get volumesnapshot -l ...` sees its members, each backref'd by `status.volumeGroupSnapshotName`. The Kubernetes and control-plane representations agree by construction, because both are derived from the same `group_id` and `group_seq`.

---

## 7. Cloning a Consistency Group

### 7.1 Restore is per member

The CSI specification has no group-restore verb. A `VolumeGroupSnapshot` materializes one `VolumeSnapshot` per member (§5.3), and each is an ordinary `dataSource` for a new PVC. Cloning a group is therefore N per-member clones, one for each member snapshot of one generation. This matches the reference implementation (Ceph-CSI restores a group's members individually), and it is the only path the Kubernetes API offers.

The restored set is crash-consistent without any group machinery, because the source generation was one frozen cut. The clones do not need to be a group to be mutually consistent. They need to be a group only if the user intends to keep snapshotting them together going forward.

### 7.2 The clones form a new group only if labeled

If the restore PVCs carry `storage.simplyblock.io/consistency-group: <new-name>`, they birth a new group at provisioning (§4.1), pinned to wherever the first restored volume lands, with fresh epochs. If they are unlabeled, they are independent volumes that happen to be mutually consistent. The design does not couple the two: restoring the data and forming a new group are separate decisions.

A subtlety the user must know: a *new* group must satisfy the mandatory placement rule (§4.2), so a group-forming restore has to place all its clones on one node and store. A restore that only wants the consistent data, with no intent to re-snapshot as a group, should leave the PVCs unlabeled and avoid that constraint.

### 7.3 The headless convenience

Because the group is a first-class control-plane object, `sbctl` can offer a single-call group clone that Kubernetes cannot: `sbctl consistency-group clone <gid> <seq> --into <new-name>` loops over the generation's member snapshots, clones each into a new volume, and optionally forms a new group from the clones. Whether to build this convenience, versus leaving group clone to N per-member clones, is Open Question 3. It is a `sbctl`-only path either way, because the CSI API has no group-restore verb to expose it through.

---

## 8. Membership Changes and Snapshot Validity

This section answers the question directly: does a volume leaving the group invalidate the snapshots that already contain it? The answer is no, with one action that is the exception, and the section is precise about which.

### 8.1 Prior generations are immutable cuts

A generation is a snapshot of the membership as it stood at that `group_seq`. When member V leaves at generation k (`removed_seq = k`), `included_in_seq` still reports V as present in generations 1 through k and absent from k+1 onward. Generations 1 through k remain valid, immutable, crash-consistent cuts that include V. Restoring generation 3 recreates every member the group had at generation 3, V among them. Leaving does not reach back and alter history.

### 8.2 Detach preserves history; delete is the exception

The one action that can invalidate a prior generation is deleting the member's snapshot data, and that turns on how the volume leaves:

- **Detach (leave the group, keep the volume).** The member's epoch closes, and its snapshots in prior generations are untouched. Every generation that contained the member stays fully restorable. This is the history-preserving exit, and it is what "a volume leaves the group" should mean by default.
- **Delete the volume.** Deleting a member volume must not silently delete the group snapshots that prior generations depend on. In SPDK a snapshot is a read-only blob with its own identity, so the member's group snapshots can outlive the volume, and prior generations stay restorable. The backend must therefore preserve a member's group snapshots when the volume is deleted, and only a deliberate delete of the generation itself (§10) removes them. A volume delete that also destroyed its group snapshots would silently invalidate every prior generation the member belonged to, which is the one data-loss path this section exists to forbid.

### 8.3 Incomplete generations are reported, not hidden

If a member's snapshot in some generation is gone (pruned, or its volume hard-deleted under a policy that did not preserve it), that generation is incomplete: restoring it produces fewer volumes than the membership at that `group_seq` calls for. The group-scoped listing (§6.3) reports the present count against the expected count, and a restore of an incomplete generation warns rather than silently returning a partial set. The membership epochs are what make "expected" computable: the listing knows exactly which members a generation should have.

### 8.4 The migration hazard

A group's members are pinned to one logical volume store (§4.2). If a member is migrated to another node, the group can no longer be frozen on one store, and a later group snapshot fails. Migration of a group member is therefore either refused, or it moves the whole group's placement pin, and which of the two is Open Question 2. Until it is settled, a group snapshot that finds a member off the pinned store fails loudly (§12) rather than snapshotting an inconsistent subset.

### 8.5 Orphaned Kubernetes snapshots

When a member PVC is deleted, the materialized `VolumeSnapshot` objects that reference it in prior generations are orphaned from their source PVC but remain valid, pre-provisioned snapshot objects, and they stay restorable. This mirrors ordinary `VolumeSnapshot` behavior, where a snapshot outlives its source claim.

---

## 9. CSI GroupController

### 9.1 The service

`spdkcsi` gains the CSI GroupController service, advertising `GROUP_CONTROLLER_SERVICE` and the `CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT` capability, and implementing `CreateVolumeGroupSnapshot`, `DeleteVolumeGroupSnapshot`, and `GetVolumeGroupSnapshot`. The driver advertises no group capability today (`ControllerGetCapabilities` returns the per-volume set), so this is new.

### 9.2 Selector must equal membership

`CreateVolumeGroupSnapshot` receives the volume handles the snapshot-controller resolved from the label selector. Because the backend group is persistent and placement-pinned (§3), the driver does not snapshot whatever the selector matched. It resolves the group from the handles, and it verifies the handle set equals the group's current membership. A selector that resolves to a set differing from the membership (a PVC labeled after creation, a member not matched, a non-member matched) is refused with `FAILED_PRECONDITION`, because snapshotting a set the group does not represent would produce a generation that is not a faithful cut of the group. This is the mechanical consequence of §3, and Open Question 1 is whether `FAILED_PRECONDITION` is the desired handling.

### 9.3 The mapping

`CreateVolumeGroupSnapshot` maps to the backend take-generation call and returns `group_snapshot_id = {group_uuid}:{group_seq}` plus one `{snapshot_id, source_volume_id, ready, creation_time}` per member. `DeleteVolumeGroupSnapshot` maps to the delete-generation call. `GetVolumeGroupSnapshot` maps to the group-scoped generation read (§6.3). Delete must treat a missing group or generation as success, because a group deleted with its last member leaves `VolumeGroupSnapshot` objects that later delete against nothing.

---

## 10. Backend API Requirements

The driver and `sbctl` reach the backend over HTTP. Every endpoint is scoped to a cluster.

| Method   | Endpoint                                                         | Notes                                                                                                                                                                                 |
|----------|------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `POST`   | `/api/v2/clusters/{id}/storage-pools/{pid}/volumes`              | Existing create, extended with a `consistency_group` field: ensure the group, join the volume, enforce placement, all atomically. Ensure-group is idempotent by cluster and name.     |
| `GET`    | `/api/v2/clusters/{id}/consistency-groups?name={name}`           | Resolve a group by name. Returns the group, its id, its pinned placement, and its members with epochs, or empty when no such group exists.                                            |
| `POST`   | `/api/v2/clusters/{id}/consistency-groups/{gid}/snapshots`       | Take one generation. Returns `{group_seq, members: [{lvol_id, snapshot_id}]}`. Idempotent by an external snapshot name the sidecar supplies: a retry returns the existing generation. |
| `GET`    | `/api/v2/clusters/{id}/consistency-groups/{gid}/snapshots`       | List generations: per generation the `group_seq`, creation time, expected and present member counts, and per-member `{lvol_id, snapshot_id, ready}` (§6.3).                           |
| `GET`    | `/api/v2/clusters/{id}/consistency-groups/{gid}/snapshots/{seq}` | Read one generation's readiness and member snapshot handles.                                                                                                                          |
| `DELETE` | `/api/v2/clusters/{id}/consistency-groups/{gid}/snapshots/{seq}` | Delete one generation and all its member snapshots atomically. Never deletes the group.                                                                                               |
| `DELETE` | `/api/v2/clusters/{id}/consistency-groups/{gid}/members/{lvid}`  | Detach a member, closing its epoch one-way. Preserves the member's snapshots in prior generations (§8.2).                                                                             |
| `GET`    | `/api/v2/clusters/{id}/snapshots?consistency_group={gid}`        | The per-snapshot listing (§6.2), extended with `group_id` and `group_seq` on every row and a group filter.                                                                            |

The group-birth path is the volume-create field, not a separate call, so a volume joins its group in the same operation that creates it. Ensure-group idempotency by cluster and name is what lets concurrent first volumes converge (§4.1).

---

## 11. Scenarios

Each scenario is played through against the model above. Where another implementation informs the design, it is named.

### 11.1 Snapshot a healthy group

A `db-group` has three members on one store. A `VolumeGroupSnapshot` selects them. The GroupController verifies the three handles equal the membership, takes generation 4, and returns three member snapshots stamped `group_seq = 4`. The snapshot-controller materializes three `VolumeSnapshot` objects. `sbctl snapshot list --consistency-group db-group` shows three rows at `Gen 4`, and the group-scoped listing shows generation 4 with present three of expected three. This is the happy path, and it is what the regression test's hashed writer verifies for crash consistency.

### 11.2 Clone the group from generation 4

The user creates three PVCs, each `dataSource` a member `VolumeSnapshot` of generation 4. The three clones are mutually crash-consistent because generation 4 was one frozen cut. If the PVCs are labeled `db-group-restored`, they form a new group pinned wherever the first clone lands; if unlabeled, they are three consistent but independent volumes. There is no single group-restore call in Kubernetes, matching Ceph-CSI. The headless `sbctl consistency-group clone db-group 4 --into db-group-restored` is the one-call alternative (Open Question 3).

### 11.3 A member leaves after generation 4

Member V is detached at generation 5 (`removed_seq = 5`). Generation 4 still lists three members including V and stays fully restorable. Generation 6, taken after the detach, lists two members. Nothing about generation 4 changed. If instead V's volume is deleted, V's generation-4 snapshot is preserved (§8.2), so generation 4 remains complete and restorable. The group-scoped listing shows generation 6 with expected two, present two, and generation 4 with expected three, present three.

### 11.4 A member is hard-deleted without preserving its snapshots

This is the forbidden path (§8.2), included to show what the design prevents. If a volume delete also destroyed the member's group snapshots, generation 4 would drop to present two of expected three, and a restore of generation 4 would silently return two volumes for a three-volume application. The design forbids the volume delete from destroying group snapshots, and the group-scoped listing reports any incompleteness that arises another way rather than hiding it.

### 11.5 Partial failure during a group snapshot

`bdev_lvol_snapshot_group` fails on the third member. The backend unfreezes, rolls back the two snapshots already taken, and does not advance `group_seq`. No partial generation exists, and the `VolumeGroupSnapshot` reports not-ready with the error. This all-or-nothing contract matches the CSI group-snapshot requirement and Ceph-CSI's behavior, and the existing `create_group_snapshot` already implements the rollback.

### 11.6 Selector drift

A user labels a fourth PVC `db-group` after its volume was created. Because membership is fixed at creation (§4.1), the volume is not a group member, but the `VolumeGroupSnapshot` selector now matches four PVCs. The GroupController finds the handle set does not equal the three-member group and refuses with `FAILED_PRECONDITION` (§9.2) rather than snapshotting a four-way set the group does not represent. The fix is to remove the stray label, or to have created the fourth volume with the label so it is a real member.

### 11.7 A member migrates off the pinned store

Live migration moves member V to another node. The group can no longer be frozen on one store. The next group snapshot finds V off the pinned store and fails loudly (§8.4, §12), rather than snapshotting the two members still on the store and calling it a group generation. Whether migration of a group member is refused outright is Open Question 2.

---

## 12. Failure Modes and Fallback

| Failure                                                        | Detection                               | Behavior                                                                                                                                              |
|----------------------------------------------------------------|-----------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------|
| A labeled volume cannot be placed on the group's pinned node   | Backend rejects the volume create       | The `CreateVolume` fails, the PVC stays Pending with the backend error surfaced through the provisioner. The volume does not join the group unpinned. |
| A `VolumeGroupSnapshot` selector does not equal the membership | GroupController compares handle set     | `FAILED_PRECONDITION`, the snapshot is refused. No partial generation is taken (§9.2).                                                                |
| A member is off the pinned store at snapshot time              | Backend finds a member on another store | The group snapshot fails loudly. No generation is taken (§8.4).                                                                                       |
| `bdev_lvol_snapshot_group` fails mid-group                     | The RPC returns an error                | Unfreeze, roll back partial snapshots, do not advance `group_seq`. All-or-nothing (§11.5).                                                            |
| A generation is incomplete (a member snapshot is gone)         | Present count below expected count      | The group-scoped listing reports it, and a restore of that generation warns rather than returning a partial set (§8.3).                               |
| A `VolumeGroupSnapshot` is deleted after its group is gone     | GroupController resolves no group       | Delete returns success. A missing handle is not an error, matching CSI snapshot-delete semantics.                                                     |

Every path degrades to a defined state: a failed create leaves a Pending PVC, a refused snapshot leaves no generation, and an incomplete generation is reported rather than silently restored short.

---

## 13. Observability

**Baseline.** Group snapshots are a control-plane operation, and the observability is the backend's. There is no operator reconciler in this design, so there are no operator events or conditions to add. The metrics below are the backend's to export.

### Kubernetes Events

The Kubernetes-visible object is the `VolumeGroupSnapshot`, and the snapshot-controller and csi-snapshotter already emit the standard snapshot events on it (creating, ready, error). This design adds no operator events. The one event worth ensuring the driver surfaces is the selector-mismatch refusal, so a `FAILED_PRECONDITION` is legible on the `VolumeGroupSnapshot` rather than only in the sidecar log.

### Prometheus Metrics

| Metric                                                 | Labels              | Description                                                                                        |
|--------------------------------------------------------|---------------------|----------------------------------------------------------------------------------------------------|
| `simplyblock_consistency_group_members`                | `cluster`, `group`  | Gauge of current member count per group.                                                           |
| `simplyblock_consistency_group_generation`             | `cluster`, `group`  | Gauge of the latest `group_seq` per group.                                                         |
| `simplyblock_consistency_group_snapshot_total`         | `cluster`, `result` | Counter of group snapshot outcomes, `result` one of `taken`, `precondition_failed`, `rolled_back`. |
| `simplyblock_consistency_group_incomplete_generations` | `cluster`, `group`  | Gauge of generations whose present member count is below their expected count.                     |

`simplyblock_consistency_group_incomplete_generations` is the load-bearing one: a value above zero is a generation that will restore short, which is the silent failure §8 exists to prevent, so it is the alert. The `precondition_failed` result on the snapshot counter is the second signal, catching selector drift and migration hazards before a user notices a refused snapshot.

---

## 14. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts: [`tests/test-plan-consistency-groups.md`](../tests/test-plan-consistency-groups.md)

- **Unit:** the membership epoch math (`included_in_seq` over join and remove at various generations), the group-scoped listing's expected-versus-present computation, and the GroupController's selector-equals-membership check, all without a cluster.
- **Integration:** the CSI GroupController against a mock backend and the snapshot-controller under `envtest`, asserting a `VolumeGroupSnapshot` materializes one `VolumeSnapshot` per member and that a drifted selector is refused.
- **E2E:** the cross-volume consistency claim, which only a live cluster proves: provision labeled members, run a hashed round-robin writer across them, take generations, restore every member from one generation, and assert the group prefix property and a passing negative control against a mixed-generation restore. This mirrors the existing consistency-group regression script and is where the crash-consistency guarantee is actually verified.
- **Load / long-running:** group snapshot cadence under sustained write load, asserting the generation counter advances and no member's snapshot diverges by more than one write from the others.

The risk concentrates in the E2E cross-volume assertion, the placement failure path (a member that cannot colocate must fail creation), and the delete-preserves-snapshots rule (§8.2), which is the one data-loss path. Those must not be cut if the schedule slips. Phase 2 scenarios (`VolumeGroupSnapshot` create, clone, and delete-after-group-gone) become testable only once P0-4 enables the group feature gate.

---

## 15. Migration Strategy

The consistency group exists today only as a sub-feature of a replication policy (§1). This design makes it standalone.

- **Today (backend, `replication-features` branch):** a `ReplicationPolicy` with a consistency-group flag creates and owns a `ConsistencyGroup` (`policy_id` set), and snapshots are taken on the policy's replication cadence.
- **Target:** a `ConsistencyGroup` is a first-class record born from its first labeled volume, snapshotted on demand through a `VolumeGroupSnapshot` or `sbctl`, with `policy_id` unset. Replication, if it is added later, attaches to the standalone group rather than owning it.

The backend work (P0-2, P0-3) makes `policy_id` optional on the group record, adds the standalone create through the volume-create field, adds the group-scoped snapshot listing, and surfaces `group_id` and `group_seq` in the per-snapshot listing. The membership epoch model and `bdev_lvol_snapshot_group` are unchanged, so a group created either way snapshots identically. Existing policy-coupled groups keep working as the compatible special case until the policy coupling is removed in a later change.

---

## 16. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                               | Owner              |
|-----|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------|
| 1   | **Selector versus fixed membership.** A `VolumeGroupSnapshot` selector that resolves to a set differing from the group's membership is refused with `FAILED_PRECONDITION` (§9.2). Confirm this is the desired handling, rather than snapshotting the selector's set or the intersection.                                               | CSI / Backend team |
| 2   | **Migration of a group member.** A member migrated off the pinned store breaks future group snapshots (§8.4). Should migration of a group member be refused, or should it move the whole group's placement pin? Until settled, a snapshot with a member off the store fails loudly.                                                    | Backend team       |
| 3   | **Headless group clone.** Should `sbctl` offer a one-call `consistency-group clone <gid> <seq>` that clones every member of a generation (§7.3), or is group clone left to N per-member clones? The Kubernetes path is per-member either way.                                                                                          | Backend team       |
| 4   | **Label key prefix.** The design uses `storage.simplyblock.io/consistency-group` to sit in the shipped `storage.simplyblock.io/` annotation family, while the placement pins use the `simplyblock.io/` prefix. Confirm the choice before it ships, because a label key cannot be changed without breaking every manifest that sets it. | Operator team      |
| 5   | **Deleting a member volume with group snapshots.** §8.2 requires a volume delete to preserve the member's group snapshots. Confirm the backend delete path preserves them rather than cascading, since this is the one data-loss path in the design.                                                                                   | Backend team       |
