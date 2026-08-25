# Design Document: Striped pNFS (RWX) Volumes and Consistency-Group Snapshots

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-25  
**Issue:** https://github.com/simplyblock/simplyblock-operator/issues/278  
**Target release:** after 26.4  
**Builds on:** [`design-pnfs-rwx.md`](design-pnfs-rwx.md), which delivers
single-volume pNFS exports. Everything there is assumed and not restated.

---

## Overview

**What this adds.** A single-volume RWX export is capped by one lvol: its
throughput, its capacity, and its failure domain are one backing volume's. This
design spreads one RWX volume across `n` lvols with an LVM stripe underneath the
XFS filesystem, so a shared filesystem scales past what one lvol can serve.

**What that costs, and why it is a separate document.** Striping breaks the
assumption that makes single-volume snapshots simple. A snapshot of a striped XFS
is only mountable if every member is snapshotted at the same instant, which needs a
backend consistency group the control plane does not implement. Once that primitive
exists it also backs the user-facing Kubernetes `VolumeGroupSnapshot`, which brings
its own CSI service and version floors. None of it is needed to export a filesystem
over pNFS, so none of it belongs in the document that does.

**What is inherited unchanged.** The `NFSExport` CR and its reconciler, MDS
eligibility and selection, the csi-link control channel, the per-export Service and
its EndpointSlice, PR fencing, and the failover state machine all come from the base
design. This document changes what sits under the filesystem, not how the export is
placed, addressed, or recovered.

**The one structural change to the base design.** `CreateExport` stops formatting a
device and starts assembling one. That makes re-materializing an export on another
host a rebuild rather than a mount, which is the only place striping touches
failover.

---

## Phase 0 — External Prerequisites

In addition to every prerequisite in the base design, which still applies.

| #    | Prerequisite                                                                                                                                                    | Kind                    | Blocks                                            | Status                   |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------------------------------------------------|--------------------------|
| S0-1 | Consistency-group snapshot API: atomically snapshot a set of lvols, returning a group id and per-member ids (§3)                                                 | Control plane (`sbcli`) | Every snapshot of a striped volume, and all of §5 | Not shipped              |
| S0-2 | Freeze and unfreeze of all members for the duration of that group snapshot (§3)                                                                                  | Storage plane (SPDK)    | Crash-consistency of the snapshot                 | Unknown                  |
| S0-3 | Consistency-group clone and restore from a group snapshot (§3)                                                                                                    | Control plane (`sbcli`) | Clone and restore of a striped volume             | Not shipped, recommended |
| S0-4 | A placement hint on lvol create, so members land on distinct storage nodes rather than by luck (§2.2)                                                             | Control plane (`sbcli`) | The parallelism striping exists for               | Not shipped, recommended |
| S0-5 | Kubernetes ≥ 1.32 with the group-snapshot CRDs, `external-snapshotter` ≥ 8.2, and `external-provisioner` ≥ 5.1 (§5)                                              | Ecosystem               | The user-facing `VolumeGroupSnapshot` only        | Available, version-gated |

**Without S0-1 and S0-2** a snapshot of a striped RWX volume produces an
unmountable filesystem, so snapshotting has to be refused rather than served wrong.
That is the sharpest constraint in this document: striping without a consistency
group is a volume that cannot be backed up. **Without S0-4** members may land on one
node, which does not break correctness but removes the reason to stripe at all.

For the record, the operator chart already ships `csi-snapshotter` and
`snapshot-controller` at v8.2.0 and `csi-provisioner` at v5.1.0, so S0-5 is a
version floor that is already met rather than work to do.

---

## Table of Contents

- [Overview](#overview)
- [Phase 0 — External Prerequisites](#phase-0--external-prerequisites)

1. [Goals and Non-Goals](#1-goals-and-non-goals)
2. [Striping the Backing Volume](#2-striping-the-backing-volume)
3. [Consistency-Group Snapshot, Clone, and Restore](#3-consistency-group-snapshot-clone-and-restore)
4. [Changes to the Base Design](#4-changes-to-the-base-design)
5. [Group Controller Service and `VolumeGroupSnapshot`](#5-group-controller-service-and-volumegroupsnapshot)
6. [Failure Modes](#6-failure-modes)
7. [Open Questions](#7-open-questions)
8. [Phased Delivery Plan](#8-phased-delivery-plan)
9. [Test Plan](#9-test-plan)

---

## 1. Goals and Non-Goals

### 1.1 Goals

- Provision an RWX PVC of size `S` as `n` lvols of `ceil(S/n)` each, GiB-aligned per `util.AlignToGiBBytes`, where `1 ≤ n ≤` the number of eligible storage nodes.
- Assemble those `n` namespaces into one LVM stripe on the MDS host and make one XFS filesystem on it.
- Reproduce that assembly deterministically on another host, so failover keeps working.
- Snapshot, clone, and restore a striped volume through a backend consistency group, so the result is mountable.
- Expose the same primitive as the upstream `VolumeGroupSnapshot`, so a user can snapshot several PVCs atomically.

### 1.2 Non-Goals

- Re-striping an existing volume across a changed node set. A volume's `n` is fixed at creation.
- Redundancy across members. A stripe has none: losing one member loses the filesystem. Resiliency stays per-lvol, through erasure coding or replication (§6).
- Mixed `n` within one export, or changing `n` on resize.
- Striping for RWO volumes, which have no reason to want it.

---

## 2. Striping the Backing Volume

### 2.1 Assembly on the MDS host

`CreateExport` gains a volume-manager step between attaching the namespaces and
making the filesystem. The steps stay idempotent and individually skippable, which
matters more here than in the base design because there are now more of them:

1. **Attach namespaces:** csi-node connects all `n` member namespaces and owns their reconnect lifecycle. `CreateExport` waits for `n` devices before proceeding.
2. **LVM stripe:** `pvcreate` each device, `vgcreate vg_{pvc}`, then `lvcreate --stripes n --stripesize <S> -l 100%FREE -n lv_{pvc} vg_{pvc}`.
3. **Filesystem:** `mkfs.xfs /dev/vg_{pvc}/lv_{pvc}`, only when `blkid` shows it is unformatted.
4. **Mount and export:** as the base design, against the striped logical volume rather than a raw namespace.

LVM identifies its physical volumes by on-disk metadata, not by device path, so
`/dev/nvmeXnY` churn across reconnects is safe and the MDS needs no `eui64` symlink.
That is a client-side concern and stays one.

**Naming is part of the contract.** `vg_{pvc}` and `lv_{pvc}` derive from the volume
name and nothing else, because failover re-runs this assembly on a different host and
has to arrive at the same names. A random or host-derived suffix would make an export
unrecoverable, so the naming is recorded on the CR rather than recomputed.

### 2.2 Placement

The `n` members should land on `n` distinct storage nodes. That is the entire point:
a stripe whose members share a node has the throughput of one node and the failure
domain of one node, with extra moving parts. The control plane offers no placement
hint today (S0-4), so until it does, the driver creates members one at a time and
verifies afterward that they landed on distinct nodes, failing provisioning rather
than silently building a degenerate stripe.

The MDS host is chosen independently of where members live, as in the base design.
Co-locating the MDS with a member buys nothing, because the MDS reaches every member
over NVMe-oF regardless.

---

## 3. Consistency-Group Snapshot, Clone, and Restore

### 3.1 Why per-member snapshots are not enough

A striped XFS interleaves metadata and data across all members. Snapshotting members
independently yields images from different instants, so the filesystem's log refers
to blocks that another member's image does not have. The result does not mount, and
it does not fail loudly at snapshot time. It fails at restore, which is the worst
place to find out.

`xfs_freeze` on the MDS host, which is how the base design quiesces a single-volume
snapshot, is necessary here but no longer sufficient. Freezing stops new writes, but
the members are still snapshotted by `n` separate backend calls, and nothing makes
those calls simultaneous. The atomicity has to come from the backend.

### 3.2 What the backend has to offer

```
SnapshotGroup {
  group_uuid
  group_name
  lvol_ids[]        // every member of the stripe
  created_at
  status
}
```

- `POST /api/v2/clusters/{id}/storage-pools/{pool_id}/snapshot-groups` freezes every member, snapshots them, and unfreezes, returning a group id and per-member snapshot ids. It fails whole or not at all: a partial group is worse than no group, because it looks like a usable backup.
- `POST /snapshot-groups/{group_id}/clone` clones the whole group, yielding `n` new lvols that are mutually consistent.
- `GET` and `DELETE` on the group, operating on all members.

All members of a group must live in one cluster and one pool, so the backend can
freeze them together. The driver enforces that before calling rather than letting the
backend refuse.

### 3.3 What the driver does with it

- **Snapshot:** `xfs_freeze -f` on the MDS, one group-snapshot call, `xfs_freeze -u`. The CSI snapshot id encodes the group as `{clusterID}:{poolID}:{groupSnapUUID}`, and list and delete operate on the group.
- **Clone and restore:** clone the group, then run the full `CreateExport` assembly on a newly selected MDS host. The clone is an independent RWX volume with its own `NFSExport` CR, its own Service, and its own `fsid`.
- **Resize:** grow every member, then `lvextend`, then `xfs_growfs` on the MDS. Members must grow uniformly, because an LVM stripe cannot use unequal extents.

---

## 4. Changes to the Base Design

Everything else in [`design-pnfs-rwx.md`](design-pnfs-rwx.md) stands. These are the
edits striping forces:

| Base design section         | Change                                                                                                                                                                        |
|-----------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| §7.1 `NFSExport` CRD        | `status` gains `stripeCount` and `members[]` of `{lvolID, nguid, sizeBytes}`, replacing the single `lvolID` and `nguid`. `spec` gains `vgName` and `lvName` so failover reproduces the assembly (§2.1). |
| §8.2 `CreateExport`         | The LVM steps in §2.1 sit between attach and `mkfs.xfs`.                                                                                                                       |
| §8.3 `DeleteExport`         | `lvremove`, `vgremove`, and `pvremove` before the namespaces are released.                                                                                                     |
| §9.2 StorageClass           | A `stripe_count` parameter, defaulting to `1`, bounded by the eligible node count.                                                                                             |
| §9.3 `CreateVolume`         | Create `n` lvols of `ceil(S/n)`, verify distinct placement (§2.2), and record all members.                                                                                      |
| §10.1 `NodeStageVolume`     | Connect all `n` namespaces and create an `eui64` symlink per member, rather than one of each.                                                                                   |
| §12.2 Snapshot              | The group path in §3.3 replaces the per-lvol snapshot.                                                                                                                          |
| §13 Failover                | Unchanged in shape, but step 4 rebuilds an LVM stripe rather than mounting a device, which lengthens the freeze. The `fsid` and the VG and LV names still have to match.        |
| §16 Failure modes           | One new mode, the lost member (§6).                                                                                                                                             |

The `NFSExport` status change is the only one that touches a shipped API. Because
`stripeCount` defaults to `1` and a one-member `members[]` describes exactly what the
base design's single `lvolID` described, an export created before striping exists
stays valid, and the migration is a status rewrite the reconciler can do in place.

---

## 5. Group Controller Service and `VolumeGroupSnapshot`

The same backend primitive serves a second, user-facing consumer. A Kubernetes
`VolumeGroupSnapshot` selects several PVCs by label and snapshots them atomically,
which is what a database needs when its data and its write-ahead log are separate
volumes. Implementing the upstream feature rather than a private mechanism means the
group is visible to any backup tool that understands CSI.

- **`CreateVolumeGroupSnapshot`:** resolve every source volume handle to its lvols, flattening each striped RWX volume into its `n` members, call the group-snapshot API once for the whole flattened set, and return the group id with per-source snapshot ids. Idempotent by group name, because the sidecar retries.
- **`DeleteVolumeGroupSnapshot`:** delete the backend group and its members.
- **`GetVolumeGroupSnapshot`:** report group and member status.

The service is new (`csi-driver/pkg/spdk/groupcontrollerserver.go`), registered
alongside Identity, Controller, and Node, advertising
`CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT` and the identity capability
`GROUP_CONTROLLER_SERVICE`.

Chart work: enable `--enable-volume-group-snapshots=true` on the `csi-snapshotter`
sidecar, ship the `VolumeGroupSnapshot`, `VolumeGroupSnapshotClass`, and
`VolumeGroupSnapshotContent` CRDs with their RBAC, and add a
`VolumeGroupSnapshotClass` template. All of it gated behind a value, because the
feature needs Kubernetes 1.32.

**Restore stays per-member.** Each auto-created member `VolumeSnapshot` is used as
the `dataSource` of a new PVC, which for an RWX member runs the whole assembly in
§2.1. There is no group restore in CSI, and this design does not invent one.

---

## 6. Failure Modes

Beyond the base design's table:

| #     | Scenario                                    | Expected behavior                                                                                                                                                       |
|-------|---------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| SM-1  | One member namespace is lost                | The striped logical volume loses data and XFS errors. The export goes `Degraded`. **A stripe has no redundancy of its own**, and resiliency is per-lvol erasure coding or replication. This has to be said in user-facing docs, because "striped across four nodes" reads like redundancy and is not. |
| SM-2  | Members land on the same storage node       | Provisioning fails rather than building a degenerate stripe (§2.2).                                                                                                      |
| SM-3  | Group snapshot partially succeeds           | The backend must fail whole (S0-1). If it reports partial success, the driver deletes the surviving members and returns an error, because a partial group presented as a backup is the worst outcome available. |
| SM-4  | Snapshot requested with no consistency group | Refused at admission with `FailedPrecondition`, not attempted per member.                                                                                                |
| SM-5  | Failover cannot reassemble the stripe       | Missing member, or `vgchange` refusing the volume group. The export stays `Degraded` with an event naming the member, and no second host mounts it (the base design's invariant holds regardless). |
| SM-6  | Uneven member sizes after a partial resize  | `lvextend` refuses. The reconciler retries the member resize before touching LVM again.                                                                                   |

---

## 7. Open Questions

| #  | Question                                                                                                                                                     | Owner        |
|----|--------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------|
| S1 | Does the group-snapshot API allow members across pools, or is single-cluster and single-pool the permanent constraint (§3.2)?                                  | Backend team |
| S2 | What stripe size does an NFS metadata and bulk-data mix actually want, and is one value right for every workload (§2.1)?                                       | Spike        |
| S3 | How long does reassembling a stripe add to the failover freeze, and does that keep NFR-2 in the base design achievable (§4)?                                   | Spike        |
| S4 | Is refusing provisioning the right answer when members cannot be placed on distinct nodes, or should a degenerate stripe be allowed with a warning (§2.2)?     | Product      |
| S5 | Can `n` be raised on an existing volume by adding members and reshaping, or is it permanently fixed (§1.2)?                                                    | Product      |

---

## 8. Phased Delivery Plan

- **Phase 0:** the prerequisites above, none of them implementable here.
- **Phase 1 (assembly):** `stripe_count`, multi-lvol create with placement verification, LVM assembly in `CreateExport`, multi-namespace attach on client and MDS, and the `NFSExport` status change. Snapshots stay refused for `n > 1`.
- **Phase 2 (consistency group):** the group snapshot, clone, and restore path in §3.3, which lifts that refusal.
- **Phase 3 (group controller):** the CSI GroupController service and `VolumeGroupSnapshot` (§5).
- **Phase 4 (failover under stripe):** measure and tune reassembly inside the freeze bound (S3).

Phase 1 depends only on the base design shipping. Phase 2 depends on S0-1 and S0-2.
Phase 3 depends on Phase 2 and on S0-5. Phase 4 can run alongside Phase 3.

---

## 9. Test Plan

Scenarios live in [`tests/test-plan-pnfs-striped.md`](../tests/test-plan-pnfs-striped.md).
Risk concentrates in three places, and their scenarios must not be the ones cut: the
determinism of VG and LV naming across hosts, which is what failover depends on; the
atomicity of the group snapshot, tested by restoring and mounting rather than by
inspecting ids; and the degenerate-placement path, which is the one an under-provisioned
cluster hits first.
