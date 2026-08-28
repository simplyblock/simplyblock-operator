# Design Document: Striped pNFS (RWX) Volumes and Consistency-Group Snapshots

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-25  
**Issue:** https://github.com/simplyblock/simplyblock-operator/issues/278  
**Target release:** after 26.4  
**Test Plan:** [`tests/test-plan-pnfs-striped.md`](../tests/test-plan-pnfs-striped.md)  
**Builds on:** [`design-pnfs-rwx.md`](design-pnfs-rwx.md), which delivers
single-volume pNFS exports. Everything there is assumed and not restated.

---

## Phasing Overview

| Phase | Delivers                                                        | Depends on                      | Status                         |
|-------|-----------------------------------------------------------------|---------------------------------|--------------------------------|
| 0     | External prerequisites (§Phase 0)                               | Control plane and storage plane | Consistency groups in progress |
| 1     | Stripe assembly on both sides, `stripe_count`, CR status change | The base design shipping        | Planned                        |
| 2     | Group snapshot, clone, and restore                              | S0-1, S0-2, and S0-3 for clone  | Planned                        |
| 3     | CSI GroupController and `VolumeGroupSnapshot`                   | Phase 2, S0-5                   | Planned                        |
| 4     | Failover measured with a stripe assembled                       | Phase 1                         | Planned                        |

Snapshots of a striped volume are refused until Phase 2, which is a deliberate
sequencing choice rather than a gap: a per-member snapshot of a stripe is an
unmountable filesystem, so refusing is the only correct behavior before the
consistency group exists.

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

| #    | Prerequisite                                                                                                     | Kind                    | Blocks                                            | Status                                    |
|------|------------------------------------------------------------------------------------------------------------------|-------------------------|---------------------------------------------------|-------------------------------------------|
| S0-1 | Consistency-group snapshot API: atomically snapshot a set of lvols, returning a group id and per-member ids (§3) | Control plane (`sbcli`) | Every snapshot of a striped volume, and all of §5 | In progress                               |
| S0-2 | Freeze and unfreeze of all members for the duration of that group snapshot (§3)                                  | Storage plane (SPDK)    | Crash-consistency of the snapshot                 | In progress, with S0-1                    |
| S0-3 | Consistency-group clone and restore from a group snapshot (§3)                                                   | Control plane (`sbcli`) | Clone and restore of a striped volume             | Not shipped, recommended                  |
| S0-4 | A placement hint on lvol create, so members land on distinct storage nodes rather than by luck (§2.2)            | Control plane (`sbcli`) | The parallelism striping exists for               | Not shipped, recommended                  |
| S0-5 | Kubernetes ≥ 1.32 and the sidecar version floors for the group feature (§5)                                      | Ecosystem               | The user-facing `VolumeGroupSnapshot` only        | Sidecars met, CRDs are Phase 3 chart work |

**Without S0-1 and S0-2** a snapshot of a striped RWX volume produces an
unmountable filesystem, so snapshotting has to be refused rather than served wrong.
That is the sharpest constraint in this document: striping without a consistency
group is a volume that cannot be backed up. Both are being built in the control
plane, so the constraint is a sequencing one rather than an open question. **Without S0-4** members may land on one
node, which does not break correctness but removes the reason to stripe at all.

The operator chart already ships `csi-snapshotter` and `snapshot-controller` at
v8.2.0 and `csi-provisioner` at v5.1.0, so the version floor in S0-5 is met. The
`VolumeGroupSnapshot`, `VolumeGroupSnapshotClass`, and `VolumeGroupSnapshotContent`
CRDs are **not** in the chart, and shipping them is Phase 3 work here (§5) rather
than an existing capability.

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
2. **Physical volumes:** `pvcreate` each member device.
3. **Volume group:** `vgcreate <vg> <member-1> … <member-n>`, with the members passed **in the recorded order** (§2.3).
4. **Striped logical volume:** `lvcreate --stripes <n> --stripesize <chunk> -l 100%FREE -n <lv> <vg>`.
5. **Filesystem:** `mkfs.xfs /dev/<vg>/<lv>`, only when `blkid` shows it is unformatted.
6. **Mount and export:** as the base design, against the striped logical volume rather than a raw namespace.

`--stripesize` takes the **per-stripe chunk size**, not the volume size. It is a
separate setting with its own default (§7, S2), and passing the requested capacity
there would produce a pathological layout. Earlier drafts of this document made
exactly that mistake.

LVM identifies its physical volumes by on-disk metadata, not by device path, so
`/dev/nvmeXnY` churn across reconnects is safe and the MDS needs no `eui64` symlink.
That is a client-side concern (§2.2).

### 2.2 Assembly on the client

**This is the step the proof of concept did implicitly and a design has to state.**
A pNFS block layout points the client at a device and an offset. When the exported
XFS sits on a striped logical volume, the device the server describes *is* that
logical volume, so the client cannot satisfy the layout by holding the raw members.
It needs the identical composite device.

It can build one without being told how, because **LVM metadata lives on the
members, and the client attaches the same members.** The volume group is therefore
already described on disk from the client's point of view, and activating it yields
a device-mapper device with the same geometry:

1. **Attach namespaces:** the same `n` members the MDS attached.
2. **Device identity:** the `eui64` symlink per member, as the base design describes for its single namespace.
3. **Activate, read-only:** `vgchange -ay --readonly <vg>`, producing `/dev/mapper/<vg>-<lv>`. The client creates nothing. It discovers what the MDS already wrote.
4. **Ensure `blkmapd` is running**, so the kernel can match the signature the MDS advertises against that composite device.
5. **Mount the export** over NFS, as the base design describes.

On unstage the order reverses: unmount the NFS export, `vgchange -an <vg>`, then
release the namespaces. Skipping the deactivation leaves a device-mapper device
holding the namespaces open, so the release fails and the node accumulates stale
mappings.

**Relying on udev auto-activation is not good enough.** `lvm2-pvscan` will activate
the group on its own when the members appear, which is presumably why the proof of
concept worked without anyone writing this step down. Depending on it makes the data
path a side effect of distribution defaults, and it activates read-write. The step
is explicit and read-only here.

### 2.3 What the client and the server must agree on, exactly

The block layout identifies the device by a signature the server reads from it, so
the client's composite device has to be byte-identical where that signature lives.
That turns several things that look like implementation detail into contract:

- **Member order.** A stripe over the same members in a different order is a
  different device. The order is recorded on the CR and replayed, not re-derived
  from a set.
- **Stripe geometry.** `n` and the chunk size have to match, which they do only
  because both sides read the same on-disk metadata rather than being configured
  twice.
- **Names.** `<vg>` and `<lv>` derive from a stable identifier and nothing
  host-specific, because failover re-runs the assembly elsewhere and has to arrive
  at the same names. A host-derived or random suffix would make an export
  unrecoverable.
- **Namespace scope.** The identifier cannot be the PVC name alone: PVC names are
  unique only within a namespace, so two same-named PVCs in different namespaces
  would select the same volume group on one host. It carries namespace and UID
  information, and it has to stay inside LVM's name length and character limits,
  which means a deterministic hash rather than a concatenation.

### 2.4 One volume group, many hosts

The MDS and every client have the same volume group active at once. That is the
shared-storage configuration LVM warns about, and it is safe here only because of an
asymmetry the design has to keep true: **exactly one host ever writes LVM metadata or
mounts the filesystem.** Clients read.

What keeps it that way:

- Client activation is `--readonly`, so the client cannot write metadata even by accident.
- Clients never `mkfs`, never mount the logical volume, and never run a metadata-writing `vgchange`, `vgck --updatemetadata`, or `lvextend`. A resize is an MDS operation (§3.3), after which clients pick up the new size on reactivation.
- An `lvm.conf` filter on client nodes keeps auto-activation away from these groups, so the explicit read-only activation in §2.2 is the only path that brings them up.
- LVM `system_id` is worth setting so a client refuses ownership outright rather than relying on the caller passing `--readonly` every time.

Teardown is ordered by this same asymmetry. `lvremove` and `vgremove` on the MDS
fail while any client still has the group active, so `DeleteExport` cannot complete
until every client has unstaged. That makes client deactivation part of the export's
teardown path rather than a local cleanup detail, and a client that disappears
without deactivating leaves a group the MDS cannot remove until the members are
force-released.

### 2.5 Placement

The `n` members should land on `n` distinct storage nodes. That is the entire point:
a stripe whose members share a node has the throughput of one node and the failure
domain of one node, with extra moving parts. The control plane offers no placement
hint today (S0-4), so until it does, the driver creates members one at a time and
verifies afterward that they landed on distinct nodes, failing provisioning rather
than silently building a degenerate stripe. Refusing rather than warning is what the
test plan asserts (`SU-09`, `SE-09`), and S4 is the decision that could soften it to
a warning. Until S4 is answered, refusal is the contract.

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
freeze them together, and the driver enforces that before calling rather than
letting the backend refuse. This is stated as a firm rule because the test plan
asserts it (`VGS-04`). If S1 resolves the other way and cross-pool groups become
possible, the rule and that scenario relax together.

### 3.3 What the driver does with it

- **Snapshot:** `xfs_freeze -f` on the MDS, one group-snapshot call, `xfs_freeze -u`. The CSI snapshot id encodes the group as `{clusterID}:{poolID}:{groupSnapUUID}`, and list and delete operate on the group.
- **Clone and restore:** clone the group, then run the full `CreateExport` assembly on a newly selected MDS host. The clone is an independent RWX volume with its own `NFSExport` CR, its own Service, and its own `fsid`.
- **Resize:** grow every member, then `lvextend`, then `xfs_growfs` on the MDS. Members must grow uniformly, because an LVM stripe cannot use unequal extents.

---

## 4. Changes to the Base Design

Everything else in [`design-pnfs-rwx.md`](design-pnfs-rwx.md) stands. These are the
edits striping forces:

| Base design section     | Change                                                                                                                                                                                                                                                                     |
|-------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| §7.1 `NFSExport` CRD    | `status` gains `stripeCount` and `members[]` of `{lvolID, nguid, sizeBytes}`, replacing the single `lvolID` and `nguid`. `status` gains `vgName`, `lvName`, and the member **order**, all controller-derived, so failover reproduces the assembly byte-identically (§2.3). |
| §8.2 `CreateExport`     | The LVM steps in §2.1 sit between attach and `mkfs.xfs`.                                                                                                                                                                                                                   |
| §10.1 `NodeStageVolume` | The client-side activation in §2.2 sits between attaching the members and mounting the export, and its reverse runs on unstage.                                                                                                                                            |
| §8.3 `DeleteExport`     | `lvremove`, `vgremove`, and `pvremove` before the namespaces are released.                                                                                                                                                                                                 |
| §9.2 StorageClass       | A `stripe_count` parameter, defaulting to `1`, bounded by the eligible node count.                                                                                                                                                                                         |
| §9.3 `CreateVolume`     | Create `n` lvols of `ceil(S/n)`, verify distinct placement (§2.2), and record all members.                                                                                                                                                                                 |
| §12.2 Snapshot          | The group path in §3.3 replaces the per-lvol snapshot.                                                                                                                                                                                                                     |
| §13 Failover            | Unchanged in shape, but step 4 rebuilds an LVM stripe rather than mounting a device, which lengthens the freeze. The `fsid` and the VG and LV names still have to match.                                                                                                   |
| §16 Failure modes       | One new mode, the lost member (§6).                                                                                                                                                                                                                                        |

The `NFSExport` status change is the only one that touches a shipped API, and it
cannot be done by rewriting stored objects in place: an object already persisted
against the old schema does not become conformant because a controller wants it to.
So `lvolID` and `nguid` are **retained as deprecated** alongside `members[]` for one
release, the reconciler populates both, and the singular fields are removed only
once no stored object still relies on them. `stripeCount` defaults to `1`, so a
base-design export reads correctly through the new fields from the start.

---

## 5. Group Controller Service and `VolumeGroupSnapshot`

The same backend primitive serves a second, user-facing consumer. A Kubernetes
`VolumeGroupSnapshot` selects several PVCs by label and snapshots them atomically,
which is what a database needs when its data and its write-ahead log are separate
volumes. Implementing the upstream feature rather than a private mechanism means the
group is visible to any backup tool that understands CSI.

- **`CreateVolumeGroupSnapshot`:** resolve every source volume handle to its lvols, flattening each striped RWX volume into its `n` members, then call the group-snapshot API once for the whole flattened set. The CSI response carries one group identity, not a list of per-source ids, so the mapping from backend member snapshots to the individual `VolumeSnapshot` objects the snapshot controller creates has to be derivable from the group id and the source handle. Idempotent by group name, because the sidecar retries.
- **`DeleteVolumeGroupSnapshot`:** delete the backend group and its members.
- **`GetVolumeGroupSnapshot`:** report group and member status.

The service is new (`csi-driver/pkg/spdk/groupcontrollerserver.go`), registered
alongside Identity, Controller, and Node, advertising
`CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT` and the identity capability
`GROUP_CONTROLLER_SERVICE`.

Chart work: enable the group-snapshot feature gate on **both** the `csi-snapshotter`
sidecar and the `snapshot-controller` (`--feature-gates=CSIVolumeGroupSnapshot=true`
for the 8.2 line, which is not the same as the `--enable-volume-group-snapshots`
flag earlier drafts named), ship the `VolumeGroupSnapshot`,
`VolumeGroupSnapshotClass`, and `VolumeGroupSnapshotContent` CRDs with their RBAC,
and add a `VolumeGroupSnapshotClass` template. The exact flag spelling per sidecar
version is worth confirming against the shipped image rather than the release notes. All of it gated behind a value, because the
feature needs Kubernetes 1.32.

**Restore stays per-member.** Each auto-created member `VolumeSnapshot` is used as
the `dataSource` of a new PVC, which for an RWX member runs the whole assembly in
§2.1. There is no group restore in CSI, and this design does not invent one.

---

## 6. Failure Modes

Beyond the base design's table:

| #    | Scenario                                     | Expected behavior                                                                                                                                                                                                                                                                                     |
|------|----------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| SM-1 | One member namespace is lost                 | The striped logical volume loses data and XFS errors. The export goes `Degraded`. **A stripe has no redundancy of its own**, and resiliency is per-lvol erasure coding or replication. This has to be said in user-facing docs, because "striped across four nodes" reads like redundancy and is not. |
| SM-2 | Members land on the same storage node        | Provisioning fails rather than building a degenerate stripe (§2.2).                                                                                                                                                                                                                                   |
| SM-3 | Group snapshot partially succeeds            | The backend must fail whole (S0-1). If it reports partial success, the driver deletes the surviving members and returns an error, because a partial group presented as a backup is the worst outcome available.                                                                                       |
| SM-4 | Snapshot requested with no consistency group | Refused at admission with `FailedPrecondition`, not attempted per member.                                                                                                                                                                                                                             |
| SM-5 | Failover cannot reassemble the stripe        | Missing member, or `vgchange` refusing the volume group. The export stays `Degraded` with an event naming the member, and no second host mounts it (the base design's invariant holds regardless).                                                                                                    |
| SM-6 | Uneven member sizes after a partial resize   | `lvextend` refuses. The reconciler retries the member resize before touching LVM again.                                                                                                                                                                                                               |

---

## 7. Open Questions

| #  | Question                                                                                                                                                   | Owner        |
|----|------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------|
| S1 | Does the group-snapshot API allow members across pools, or is single-cluster and single-pool the permanent constraint (§3.2)?                              | Backend team |
| S2 | What stripe size does an NFS metadata and bulk-data mix actually want, and is one value right for every workload (§2.1)?                                   | Spike        |
| S3 | How long does reassembling a stripe add to the failover freeze, and does that keep NFR-2 in the base design achievable (§4)?                               | Spike        |
| S4 | Is refusing provisioning the right answer when members cannot be placed on distinct nodes, or should a degenerate stripe be allowed with a warning (§2.2)? | Product      |
| S5 | Can `n` be raised on an existing volume by adding members and reshaping, or is it permanently fixed (§1.2)?                                                | Product      |

---

## 8. Phased Delivery Plan

- **Phase 0:** the prerequisites above, none of them implementable here.
- **Phase 1 (assembly):** `stripe_count`, multi-lvol create with placement verification, LVM assembly in `CreateExport`, multi-namespace attach on client and MDS, and the `NFSExport` status change. Snapshots stay refused for `n > 1`.
- **Phase 2 (consistency group):** the group snapshot, clone, and restore path in §3.3, which lifts that refusal.
- **Phase 3 (group controller):** the CSI GroupController service and `VolumeGroupSnapshot` (§5).
- **Phase 4 (failover under stripe):** measure and tune reassembly inside the freeze bound (S3).

Phase 1 depends only on the base design shipping. Phase 2 depends on S0-1 and S0-2
for the snapshot, and on S0-3 for the clone and restore it also carries, so an
unfinished S0-3 splits Phase 2 rather than delaying it. Phase 3 depends on Phase 2
and on S0-5. Phase 4 can run alongside Phase 3.

---

## 9. Test Plan

Scenarios live in [`tests/test-plan-pnfs-striped.md`](../tests/test-plan-pnfs-striped.md).
Risk concentrates in three places, and their scenarios must not be the ones cut: the
determinism of VG and LV naming across hosts, which is what failover depends on; the
atomicity of the group snapshot, tested by restoring and mounting rather than by
inspecting ids; and the degenerate-placement path, which is the one an under-provisioned
cluster hits first.
