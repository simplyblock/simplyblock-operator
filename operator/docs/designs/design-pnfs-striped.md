# Design Document: Striped pNFS (RWX) Volumes and Consistency-Group Snapshots

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-25 (last updated 2026-10-17)  
**Issue:** https://github.com/simplyblock/simplyblock-operator/issues/278  
**Target release:** after 26.4  
**Test Plan:** [`tests/test-plan-pnfs-striped.md`](../tests/test-plan-pnfs-striped.md)  
**Builds on:** [`design-pnfs-rwx.md`](design-pnfs-rwx.md), which delivers
single-volume pNFS exports. Everything there is assumed and not restated.

---

## Phasing Overview

| Phase | Delivers                                                       | Depends on                                     | Status                         |
|-------|----------------------------------------------------------------|------------------------------------------------|--------------------------------|
| 0     | External prerequisites (§Phase 0)                              | Control plane, storage plane, and guest kernel | Consistency groups in progress |
| 1     | Stripe assembly and topology, `stripe_count`, CR status change | The base design shipping, S0-6, and S0-7       | Planned                        |
| 2     | Group snapshot, clone, and restore                             | S0-1, S0-2, and S0-3 for clone                 | Planned                        |
| 3     | CSI GroupController and `VolumeGroupSnapshot`                  | Phase 2, S0-5                                  | Planned                        |
| 4     | Failover measured with a stripe assembled                      | Phase 1                                        | Planned                        |

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

**The two structural changes to the base design.** `CreateExport` stops formatting a
device and starts assembling one. That makes re-materializing an export on another
host a rebuild rather than a mount, which is the only place striping touches
failover. And the metadata server's nfsd describes the assembled device to clients
by its structure, one SCSI volume per member under a stripe, so each client reaches
every member directly and assembles nothing itself (§2.2).

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
| S0-6 | A guest nfsd patch (pnfs-os) that describes an exported device-mapper stripe as a pNFS volume topology (§2.2)    | Guest kernel            | Every striped export                              | Not started                               |
| S0-7 | `BLK_DEV_DM` and `DM_STRIPED` in the guest kernel and `lvm2` in its root filesystem (§2.1)                       | Guest kernel and image  | Assembly on the metadata server                   | Not started                               |

**Without S0-1 and S0-2** a snapshot of a striped RWX volume produces an
unmountable filesystem, so snapshotting has to be refused rather than served wrong.
That is the sharpest constraint in this document: striping without a consistency
group is a volume that cannot be backed up. Both are being built in the control
plane, so the constraint is a sequencing one rather than an open question. **Without S0-4** members may land on one
node, which does not break correctness but removes the reason to stripe at all.

**Without S0-6** a striped export is refused rather than served. An unpatched nfsd
describes the stripe as its first member, and a client given that layout writes
logical-volume offsets onto the first member's raw namespace (§2.2). S0-7 adds the
options and the package to the guest that `design-pnfs-mds-vm.md` §6.1 and §6.2
specify, which carry no device-mapper support today.

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
- Assemble those `n` namespaces into one LVM stripe in the metadata server's guest and make one XFS filesystem on it.
- Describe that stripe to clients as a pNFS SCSI volume topology, so every client reaches every member directly and runs no volume manager.
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

### 2.1 Assembly in the metadata server's guest

The export assembler runs in the metadata server's guest as `mds-agent`
([`design-pnfs-mds-vm.md`](design-pnfs-mds-vm.md)), and brings an export up as a
volume stack ([`design-node-volume-stack.md`](design-node-volume-stack.md)). A
single-volume export is the `Plain` plan, `fabric` → `filesystem`
(`csi-driver/internal/nfsexport/plan.go`). A striped export is the `Striped` plan,
`members(n)` → `lvmPhysicalVolume` → `lvmVolumeGroup` → `lvmLogicalVolume` →
`filesystem` (`atlas-lib/volstack/plans/plans.go`), so `CreateExport` gains a
volume-manager step between attaching the namespaces and making the filesystem. The steps stay idempotent and individually skippable, which
matters more here than in the base design because there are now more of them:

1. **Attach namespaces:** the `members` layer attaches all `n` member namespaces in the recorded order. The metadata server pod resolves one connection per member for the guest, which holds no control-plane credential, and the guest's path keeper reconnects every member as it does a single namespace (`design-pnfs-mds-vm.md` §6.4, §7.6). `CreateExport` waits for `n` devices before proceeding.
2. **Physical volumes:** `pvcreate` each member device.
3. **Volume group:** `vgcreate <vg> <member-1> … <member-n>`, with the members passed **in the recorded order** (§2.3). The group carries the ownership tag every volume-stack group carries (`design-node-volume-stack.md` §5.7).
4. **Striped logical volume:** `lvcreate --stripes <n> --stripesize <chunk> -l 100%FREE -n <lv> <vg>`.
5. **Filesystem:** `mkfs.xfs /dev/<vg>/<lv>`, only when `blkid` shows it is unformatted.
6. **Mount and export:** as the base design, against the striped logical volume rather than a raw namespace.

`--stripesize` takes the **per-stripe chunk size**, not the volume size. It is a
separate setting with its own default (§7, S2), and passing the requested capacity
there would produce a pathological layout. Earlier drafts of this document made
exactly that mistake.

LVM identifies its physical volumes by on-disk metadata, not by device path, so
`/dev/nvmeXnY` churn across reconnects is safe and the MDS needs no `eui64` symlink.
That is a client-side concern (§2.2). The metadata server is the only host that
activates the volume group (§2.4).

### 2.2 The layout the client receives

A pNFS SCSI layout names a device by a designator and addresses it in offsets of
the exported filesystem. When that filesystem sits on a striped logical volume, the
exported device is a device-mapper device that exists only on the metadata server.
The metadata server therefore describes it by its structure. The GETDEVICEINFO
reply carries the stripe as a volume topology, and each client composes the same
mapping over the raw members it attached:

```
STRIPE   stripe unit = the logical volume's chunk size, children in table order
 ├─ SLICE   start = member 1's offset in the table, length = per-member length
 │   └─ SCSI   designator = member 1's NGUID
 ├─ SLICE   start = member 2's offset in the table
 │   └─ SCSI   designator = member 2's NGUID
 └─ … one SLICE and one SCSI volume per member, n in all
```

nfsd builds the topology from the live device-mapper table of the exported device,
not from LVM metadata, so it describes what the metadata server's kernel actually
maps. The `striped` target becomes the `STRIPE`, with the target's chunk size as the
stripe unit and its devices in the target's order. Each device becomes a `SLICE` at
the offset the table gives it, which already includes LVM's `pe_start`, over a
`SCSI` volume carrying that member's own designator. The client maps an offset with
the arithmetic dm-stripe uses: the chunk number modulo `n` selects the member, and
the quotient selects the chunk on it (`bl_map_stripe` in
`fs/nfs/blocklayout/dev.c`, `stripe_map_sector` in `drivers/md/dm-stripe.c`). A
faithful topology is therefore a correct mapping.

Upstream nfsd lacks this, and S0-6 adds it. It describes every exported device as
one `SCSI` volume and takes the designator from the device's `get_unique_id`
operation. On a device-mapper device that operation answers with the first member
that has an identifier, because it treats every device under a table as a path to
one disk (`dm_blk_get_unique_id` in `drivers/md/dm.c`). The export assembler
therefore refuses to export a stripe with `n > 1` unless the guest kernel advertises
the topology support through the nfsd parameter `pnfs_dm_stripe` under
`/sys/module/nfsd/parameters/`. The refusal leaves the export `Degraded` with an
event naming the missing capability (SM-7).

The patch accepts exactly one table shape, a single `striped` target. A table with
several segments, or with a `linear` target, gets no layout, so clients fall back to
I/O through the metadata server rather than to a wrong mapping (SM-8). One target is
also what device-mapper's persistent-reservation operations require: register,
reserve, and preempt on the exported device reach every member only while the table
has a single target (`dm_call_pr`). Under that condition the metadata server's
reservation and the fencing of a client already cover all `n` members, and the
patch leaves the reservation path unchanged. Resize keeps the table at one target
(§3.3).

Staging on the client is the base design's single-namespace path, repeated per
member, on top of an exclusion from LVM:

1. **Exclude the members from LVM:** the `lvmFilter` layer (§2.4), before any member is attached.
2. **Attach namespaces:** the same `n` members the metadata server attached.
3. **Device identity:** the `nvme-eui.${NGUID}` alias under `/dev/disk/by-id/` for each member, as the base design creates for its single namespace. The client resolves each `SCSI` volume of the topology through these aliases, so one missing alias leaves the whole device unavailable and the client's I/O goes through the metadata server (SM-9).
4. **Mount the export** over NFS, as the base design describes.

The client's layout driver parses each `SCSI` volume of the topology separately and
registers the client's reservation key on each, so a client is registered on every
member it writes to. No LVM command runs on a client. Unstage is the base design's
unmount and release, applied to `n` namespaces, followed by the removal of the
exclusion.

### 2.3 What successive activations must agree on

Clients receive the stripe's geometry in every GETDEVICEINFO reply and keep no
configuration of their own. The contract that remains is between one activation of
the volume group and the next, on whichever metadata server failover selects:

- **Member order.** LVM records the order of the stripe on the members, and every
  activation reproduces it. The CR records the same order at creation. After an
  activation the assembler compares it with the device order of the table, so a
  volume group assembled from the wrong devices is caught before it is exported.
- **Stripe geometry.** nfsd reads `n` and the chunk size from the live table, so no
  second copy of either exists to drift.
- **Names.** The group is `vol-<uuid>` and the logical volume `lv-<uuid>`, the
  volume stack's naming rule (`plans.VolumeGroupName` and
  `plans.LogicalVolumeName`, `design-node-volume-stack.md` §5.5). The UUID is the
  first member's lvol UUID, which is also the volume ID in the striped volume's
  handle. Failover re-runs the assembly on another guest and arrives at the same
  names, because nothing in them is host-specific. An lvol UUID is unique across
  clusters and Kubernetes namespaces and fits LVM's name limits, so two
  same-named PVCs in different namespaces never select the same group.

### 2.4 One volume group, one host

Only the metadata server activates the volume group, writes LVM metadata, or mounts
the filesystem. Clients attach the same members, so the members' PV signatures are
visible on every client node, and LVM installed there must not act on them. Two
mechanisms keep it from doing so: an `lvmFilter` layer at the bottom of the
client's volume stack, and the volume group's system ID.

The client's plan, in the terms of
[`design-node-volume-stack.md`](design-node-volume-stack.md) §3, is:

```
lvmFilter → members(n) → alias → nfsMount
```

`lvmFilter` sits below `members` because the exclusion has to exist before the
first member appears. udev runs LVM's event activation while a namespace's device
is being added, which is before any layer above `members` runs. The layer exposes
nothing and hands the zero `Artifact` up, so `members` receives what it would as
the bottom of a stack. It is constructed with the members' NGUIDs and records them
(`Recorder`), so a teardown after a restart knows which entries are its own.

| Verb      | Behavior                                                                                                                                                                                                                                                                                                                                     |
|-----------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Observe` | `StateReady` when the host has no LVM, uses an LVM devices file that lists none of the members, or carries a reject entry for every member in `global_filter`. `StatePartial` when only some entries exist, `StateAbsent` when none do, and `StateForeign` when the devices file lists a member.                                             |
| `Ensure`  | Adds the missing reject entries to a block of `global_filter` marked as simplyblock's, one `r\|^/dev/disk/by-id/nvme-eui\.<NGUID>$\|` per member. On a devices-file host it writes nothing, because a devices file admits only the devices listed in it. On `StateForeign` it fails, and staging is refused with an event naming the member. |
| `Release` | Removes this volume's entries. `Down` runs top-down, so the release follows the detach of every member, and the exclusion lasts exactly as long as a member is attached.                                                                                                                                                                     |
| `Destroy` | Nothing. The entries are a hold on the host, not a durable object.                                                                                                                                                                                                                                                                           |

`StateForeign` is a refusal rather than a repair because a member listed in the
devices file was enrolled by the host's administrator, and the layer does not edit
the host's enrollment. Every striped volume on a node edits the same `lvm.conf`, so
the layer serializes its edits under a node-wide lock
([`design-node-volume-stack.md`](design-node-volume-stack.md) §12).

The layer also implements `Healer`. `Healthy` resolves each member by its NGUID and
reports false when a device-mapper device holds it (`/sys/block/<dev>/holders/`),
which means LVM activated the group regardless of the filter. `Heal` removes that
mapping with `dmsetup remove` and runs no LVM command.

LVM evaluates a filter against every name it knows for a device, and names that
match no pattern do not count. A device whose only matching name hits a reject
pattern is therefore rejected, whatever its kernel name. The entry names the
udev-created `nvme-eui` link because that name survives the kernel name changes a
reconnect causes. Whether every node OS in the support matrix gives LVM that name,
and which of them activate LVM outside `lvm.conf`, is S7.

The second mechanism does not depend on the host's configuration at all. The
volume group carries an LVM `system_id` that every metadata server guest is
configured with and no client node is. LVM does not activate a volume group whose
system ID differs from the host's own, and a host with no system ID uses only
volume groups that have none, so a client activates nothing even where the filter
is missing or wrong.

A client that activated the group anyway would hold a device-mapper device over the
members, which blocks their release on unstage, and could write LVM metadata the
metadata server owns (SM-10). Teardown involves no client-side state beyond the
base design's and the filter entries: `lvremove`, `vgremove`, and `pvremove` run on
the metadata server once the export is unexported, and the namespaces are released
after them.

### 2.5 Placement

The `n` members should land on `n` distinct storage nodes. That is the entire point:
a stripe whose members share a node has the throughput of one node and the failure
domain of one node, with extra moving parts. The control plane offers no placement
hint today (S0-4), so until it does, the driver creates members one at a time and
verifies afterward that they landed on distinct nodes, failing provisioning rather
than silently building a degenerate stripe. Refusing rather than warning is what the
test plan asserts (`SU-09`, `SE-09`), and S4 is the decision that could soften it to
a warning. Until S4 is answered, refusal is the contract.

The metadata server's placement is independent of where members live. One
metadata server pod serves every export of a storage cluster
(`design-pnfs-mds-vm.md`), and its guest reaches every member over NVMe-oF
wherever the member lives.

---

## 3. Consistency-Group Snapshot, Clone, and Restore

### 3.1 Why per-member snapshots are not enough

A striped XFS interleaves metadata and data across all members. Snapshotting members
independently yields images from different instants, so the filesystem's log refers
to blocks that another member's image does not have. The result does not mount, and
it does not fail loudly at snapshot time. It fails at restore, which is the worst
place to find out.

`xfs_freeze` in the metadata server's guest, which is how the base design quiesces a single-volume
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
- **Clone and restore:** clone the group, then run the full `CreateExport` assembly in the storage cluster's metadata server, as for any new export. The clone is an independent RWX volume with its own `NFSExport` CR, its own Service, and its own `fsid`.
- **Resize:** grow every member by the same amount, `pvresize` each, `lvextend`, then `xfs_growfs` on the MDS. Members must grow uniformly, because an LVM stripe cannot use unequal extents. Uniform growth also keeps the table at one target, the shape §2.2 requires: the new extents follow the old ones on every member, so the extension continues the existing striped segment (S6). The assembler checks that the table is still a single `striped` target before it grows the filesystem, and leaves the export `Degraded` otherwise (SM-8). A client keeps the slice lengths of its last GETDEVICEINFO, so grown space reaches it the way grown space on a single namespace does in the base design.

---

## 4. Changes to the Base Design

Everything else in [`design-pnfs-rwx.md`](design-pnfs-rwx.md) stands. These are the
edits striping forces:

| Base design section          | Change                                                                                                                                                                                                                                                                     |
|------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| §7.1 `NFSExport` CRD         | `status` gains `stripeCount` and `members[]` of `{lvolID, nguid, sizeBytes}`, replacing the single `lvolID` and `nguid`. `status` gains `vgName`, `lvName`, and the member **order**, all controller-derived, so failover reproduces the assembly byte-identically (§2.3). |
| §8.2 `CreateExport`          | The LVM steps in §2.1 sit between attach and `mkfs.xfs`.                                                                                                                                                                                                                   |
| §10.1 `NodeStageVolume`      | The `lvmFilter` layer comes first (§2.4), then attaching and aliasing repeat per member (§2.2). No LVM activation runs, and unstage releases `n` namespaces where the base design releases one.                                                                            |
| §8.3 `DeleteExport`          | `lvremove`, `vgremove`, and `pvremove` before the namespaces are released.                                                                                                                                                                                                 |
| §9.2 StorageClass            | A `stripe_count` parameter, defaulting to `1`, bounded by the eligible node count.                                                                                                                                                                                         |
| §9.3 `CreateVolume`          | Create `n` lvols of `ceil(S/n)`, verify distinct placement (§2.5), and record all members.                                                                                                                                                                                 |
| §12.2 Snapshot               | The group path in §3.3 replaces the per-lvol snapshot.                                                                                                                                                                                                                     |
| §13 Failover                 | Unchanged in shape, but step 4 rebuilds an LVM stripe rather than mounting a device, which lengthens the freeze. The `fsid` and the VG and LV names still have to match.                                                                                                   |
| §5.3 Compatibility           | The guest additionally needs S0-6 and S0-7. Client kernels need nothing beyond the base design, because the block layout client already decodes `STRIPE` and `SLICE` topologies.                                                                                           |
| `design-pnfs-mds-vm.md` §6.4 | The runner resolves one connection per member, and `ExportSpec` carries them as a repeated field in member order. The path keeper reconciles every member's paths.                                                                                                         |
| §16 Failure modes            | The lost member, the missing topology support, the reshaped table, the unresolved member, and the activating client (§6).                                                                                                                                                  |

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

The service is new (`csi-driver/internal/csi/controller/group.go`), registered
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

| #     | Scenario                                     | Expected behavior                                                                                                                                                                                                                                                                                     |
|-------|----------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| SM-1  | One member namespace is lost                 | The striped logical volume loses data and XFS errors. The export goes `Degraded`. **A stripe has no redundancy of its own**, and resiliency is per-lvol erasure coding or replication. This has to be said in user-facing docs, because "striped across four nodes" reads like redundancy and is not. |
| SM-2  | Members land on the same storage node        | Provisioning fails rather than building a degenerate stripe (§2.5).                                                                                                                                                                                                                                   |
| SM-3  | Group snapshot partially succeeds            | The backend must fail whole (S0-1). If it reports partial success, the driver deletes the surviving members and returns an error, because a partial group presented as a backup is the worst outcome available.                                                                                       |
| SM-4  | Snapshot requested with no consistency group | Refused at admission with `FailedPrecondition`, not attempted per member.                                                                                                                                                                                                                             |
| SM-5  | Failover cannot reassemble the stripe        | Missing member, or `vgchange` refusing the volume group. The export stays `Degraded` with an event naming the member, and no second host mounts it (the base design's invariant holds regardless).                                                                                                    |
| SM-6  | Uneven member sizes after a partial resize   | `lvextend` refuses. The reconciler retries the member resize before touching LVM again.                                                                                                                                                                                                               |
| SM-7  | The guest kernel lacks the topology support  | The assembler refuses to export `n > 1`, and the export stays `Degraded` with an event naming the missing capability. An unpatched nfsd would describe the first member as the whole stripe (§2.2).                                                                                                   |
| SM-8  | The table is no longer one `striped` target  | nfsd hands out no layout, and clients fall back to I/O through the metadata server. The assembler reports the export `Degraded` and does not grow the filesystem (§3.3).                                                                                                                              |
| SM-9  | A client cannot resolve one member's alias   | The client marks the whole device unavailable and sends its I/O through the metadata server, as the base design's FM-9 describes for a single namespace.                                                                                                                                              |
| SM-10 | A client node activates the volume group     | The filter and the system ID prevent it (§2.4). A group activated anyway is found by the `lvmFilter` layer's health check, which removes the mapping so unstage can release the members.                                                                                                              |

---

## 7. Open Questions

| #   | Question                                                                                                                                                   | Owner        |
|-----|------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------|
| S1  | Does the group-snapshot API allow members across pools, or is single-cluster and single-pool the permanent constraint (§3.2)?                              | Backend team |
| S2  | What stripe size does an NFS metadata and bulk-data mix actually want, and is one value right for every workload (§2.1)?                                   | Spike        |
| S3  | How long does reassembling a stripe add to the failover freeze, and does that keep NFR-2 in the base design achievable (§4)?                               | Spike        |
| S4  | Is refusing provisioning the right answer when members cannot be placed on distinct nodes, or should a degenerate stripe be allowed with a warning (§2.5)? | Product      |
| S5  | Can `n` be raised on an existing volume by adding members and reshaping, or is it permanently fixed (§1.2)?                                                | Product      |
| S6  | Does `lvextend` after a uniform `pvresize` continue the existing striped segment on the guest's LVM version, leaving one target (§3.3)?                    | Spike        |
| S7  | Does every node OS in the support matrix expose the udev `nvme-eui` link to LVM, and which of them activate LVM outside `lvm.conf` (§2.4)?                 | Spike        |

---

## 8. Phased Delivery Plan

- **Phase 0:** the prerequisites above. S0-6 and S0-7 are guest work in pnfs-os, and none of the others is implementable here.
- **Phase 1 (assembly):** `stripe_count`, multi-lvol create with placement verification, LVM assembly in `CreateExport`, the topology capability check, multi-namespace attach on client and MDS, and the `NFSExport` status change. Snapshots stay refused for `n > 1`.
- **Phase 2 (consistency group):** the group snapshot, clone, and restore path in §3.3, which lifts that refusal.
- **Phase 3 (group controller):** the CSI GroupController service and `VolumeGroupSnapshot` (§5).
- **Phase 4 (failover under stripe):** measure and tune reassembly inside the freeze bound (S3).

Phase 1 depends on the base design shipping and on S0-6 and S0-7. Phase 2 depends on S0-1 and S0-2
for the snapshot, and on S0-3 for the clone and restore it also carries, so an
unfinished S0-3 splits Phase 2 rather than delaying it. Phase 3 depends on Phase 2
and on S0-5. Phase 4 can run alongside Phase 3.

---

## 9. Test Plan

Scenarios live in [`tests/test-plan-pnfs-striped.md`](../tests/test-plan-pnfs-striped.md).
Risk concentrates in four places, and their scenarios must not be the ones cut: the
faithfulness of the topology nfsd describes, tested by writing through the direct
path and reading each member at the offsets the table computes; the determinism of
VG and LV naming across hosts, which is what failover depends on; the atomicity of
the group snapshot, tested by restoring and mounting rather than by inspecting ids;
and the degenerate-placement path, which is the one an under-provisioned cluster
hits first.
