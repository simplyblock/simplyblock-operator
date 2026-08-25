# Design Document: Client-Side Compression (Issue #277)

**Status:** Partially Implemented  
**Author:** Manohar Reddy  
**Date:** 2026-08-06 (last updated 2026-08-25)  
**Issue:** https://github.com/simplyblock/simplyblock-operator/issues/277
**Test Plan:** [`tests/test-plan-issue-277-client-side-compression.md`](../tests/test-plan-issue-277-client-side-compression.md)

---

**Reader expectations**: this document is weighted toward hands-on validation
over top-down design. It grew out of testing whether specific mechanisms work
(VDO creation and reattach, clone/snapshot handling, migration, kernel/OS
support, compression vs. deduplication cost) and threading design decisions
through as they emerged from that testing — not from writing an architecture
first and citing evidence afterward. Design decisions and their rationale are
present throughout, but they sit alongside a lot of verification detail
rather than being the primary structure with evidence cited in support.
Readers expecting a conventional design doc (problem statement, chosen
approach, alternatives considered, interface surface, rollout plan, each
backed by evidence) should calibrate for that difference going in.

---

## Table of Contents

- [Design Document: Client-Side Compression (Issue #277)](#design-document-client-side-compression-issue-277)
  - [Table of Contents](#table-of-contents)
  - [1. Background](#1-background)
    - [VDO availability](#vdo-availability)
  - [2. Goals and Non-Goals](#2-goals-and-non-goals)
    - [Goals](#goals)
    - [Non-Goals](#non-goals)
  - [3. Architecture Overview](#3-architecture-overview)
  - [4. Node Capability: Auto-Install and Advertisement](#4-node-capability-auto-install-and-advertisement)
    - [Install + modprobe](#install--modprobe)
    - [Advertise via node label](#advertise-via-node-label)
  - [5. Scheduling Gate: Topology](#5-scheduling-gate-topology)
  - [6. StorageClass Parameter and CRD Changes](#6-storageclass-parameter-and-crd-changes)
  - [7. VDO Device Management (CSI Node Plugin)](#7-vdo-device-management-csi-node-plugin)
    - [Device creation](#device-creation)
    - [Notes](#notes)
    - [Wiring into `nodeserver.go`](#wiring-into-nodeservergo)
  - [8. Re-Provisioning and Failure Handling](#8-re-provisioning-and-failure-handling)
  - [9. Volume Expansion](#9-volume-expansion)
  - [10. RBAC Changes](#10-rbac-changes)
  - [11. Testing Strategy](#11-testing-strategy)
  - [12. Compatibility with Existing CSI Driver / Operator Features](#12-compatibility-with-existing-csi-driver--operator-features)
    - [Confirmed compatible, no changes needed](#confirmed-compatible-no-changes-needed)
    - [Real gaps found — need design/code changes](#real-gaps-found--need-designcode-changes)
    - [Needs a one-line confirmation, not a design change](#needs-a-one-line-confirmation-not-a-design-change)
  - [13. Open Questions and Discussion](#13-open-questions-and-discussion)

---

## 1. Background

[Issue #277](https://github.com/simplyblock/simplyblock-operator/issues/277) asks
for a client-side compression/dedup layer: when requested via a StorageClass, a
compression block device is auto-created between the CSI mount and the NVMe-oF
multipath device used for the volume, and this device must be re-included every
time the node-side CSI driver re-provisions the volume. To achieve this we use 
VDO (Virtual Data Optimizer, the `dm-vdo` device-mapper target) as the intended mechanism.

### VDO availability

VDO availability cannot be assumed present on any node. 

- `kmod-kvdo` and `vdo` packages are available via the distro's BaseOS repo, but
  are **not installed by default** on any node.
- `kmod-kvdo` ships prebuilt `kvdo.ko` binaries pinned to specific kernel-core NVRs 
  (RHEL's weak-modules mechanism)
- VDO availability It must be treated as an opt-in, per-node capability, checked 
  and advertised explicitly, with scheduling gated on it.


## 2. Goals and Non-Goals

### Goals

- Let a StorageClass opt a pool into client-side compression **and/or
  deduplication** via two new, independent, clearly named parameters
  (`clientCompression`, `clientDeduplication`), separate from each other —
  verified hands-on (Section 7) that the two carry genuinely different cost
  profiles: dedup's RAM cost dominates, whereas compression's is negligible.
- Automatically install and load the VDO kernel module/tooling on a node when
  the CSI node plugin starts there, without requiring a pre-baked golden image.
- Advertise per-node VDO capability so Kubernetes only schedules
  compression-requesting PVCs onto capable nodes, reusing this repo's existing
  CSI topology infrastructure.
- Insert a VDO device between the raw NVMe-oF multipath device and the
  filesystem mount in `NodeStageVolume`, and correctly re-attach (never
  re-create/reformat) that device on every reconnect/restage path, satisfying
  the issue's re-provisioning requirement.
- Fail loudly and visibly if a compression/deduplication-requesting volume ever lands on a
  node without working VDO support, rather than silently degrading.

### Non-Goals

- Changing or touching the existing server-side `compression` feature.
- Supporting a shared/pooled VDO instance across multiple volumes — this design
  is one VDO instance per volume, directly on its raw device.
- Solving airgapped/no-repo-access installation in this iteration — documented
  as a known prerequisite gap (see [Section 4](#4-node-capability-auto-install-and-advertisement)).
- Guaranteeing standalone `vdo` CLI longevity — Red Hat has been nudging toward
  LVM-integrated VDO; noted as a re-evaluate-later risk, not solved here.

---

## 3. Architecture Overview

```
     ┌──────────────────────────────────┐
     │ Pool CR (StorageClassParameters) │
     │ clientCompression: true          │
     │ clientDeduplication: false       │
     └──────────────────────────────────┘
       │ mergeStorageClassParameters()
       ▼
     ┌───────────────────────────────────────────┐
     │ Generated StorageClass                    │
     │ parameters.client_compression = "True"    │
     │ parameters.client_deduplication = "False" │
     │ allowedTopologies: vdo-capable=true       │
     └───────────────────────────────────────────┘
       │ WaitForFirstConsumer (existing)
       ▼
     ┌─────────────────────────────────┐
     │ Kubernetes scheduler            │
     │ only picks nodes advertising    │
     │ simplyblock.io/vdo-capable=true │
     └─────────────────────────────────┘
       ▼
┌────────────────────────────────────────────────────────────────────┐
│ simplyblock-csi-node (per node)                                    │
│                                                                    │
│ postStart hook: dnf install kmod-kvdo vdo (idempotent) + modprobe  │
│   |                                                                │
│   v  marker file                                                   │
│ nodeServer startup: read marker, patch Node label vdo-capable      │
│   |                                                                │
│   v  reused by NodeGetInfo -> buildAccessibleTopology              │
│                                                                    │
│ NodeStageVolume: initiator.Connect() -> devicePath                 │
│   |                                                                │
│   v  if client_compression=="true" OR client_deduplication=="true" │
│ CreateOrAttachVDO(devicePath, compression, dedup) -> vdoDevicePath │
│   |                                                                │
│   v                                                                │
│ stageVolume(vdoDevicePath, ...) -> format/mount                    │
└────────────────────────────────────────────────────────────────────┘
```

New in this design: `clientDeduplication`, the `allowedTopologies`/
`vdo-capable` label gate, the `postStart` install step, the `nodeServer`
marker-read/label-patch step, and `CreateOrAttachVDO`. Everything else
(`mergeStorageClassParameters`, `WaitForFirstConsumer`, the scheduler,
`initiator.Connect`, `NodeStageVolume`, `stageVolume`) already exists today.

---

## 4. Node Capability: Auto-Install and Advertisement

### Install + modprobe

Extend the `csi-node` container's `postStart` lifecycle hook with an
additional, independent step:

```
rpm -q kmod-kvdo vdo || dnf install -y kmod-kvdo vdo
modprobe kvdo
```

The container is already `privileged: true` with `SYS_ADMIN`/`SYS_MODULE`
capabilities, and already **read-only** mounts host `/lib/modules`. 
That is sufficient to `modprobe` a module that already exists
on the host (similarly to how `nvme-tcp` works today) 
BUT not to install new RPM packages, which need to write into the host's real 
`/lib/modules`, update `/var/lib/rpm`, and run `depmod`.

To reach real host root without adding a new writable hostPath mount of `/` to
this long-running DaemonSet, use `hostPID: true` plus
`nsenter -t 1 -m -u -n -i -- dnf install -y kmod-kvdo vdo`. This is the standard,
lower-blast-radius idiom used by driver-installer DaemonSets, versus mounting
the entire host filesystem read-write into a persistent pod.

**Airgapped clusters**: this install step requires BaseOS repo access from every
node. Clusters without that access need the module pre-baked into their node
image; the capability-detection step below still works correctly in that case
(it only checks `modprobe kvdo`, it doesn't require having performed the install
itself) — it just means the earlier install step is a no-op/failure that's
already handled by the `rpm -q` check, or informational failure if truly absent.

### Advertise via node label

Record success/failure of `modprobe kvdo` and add it to the node label

- `simplyblock.io/vdo-capable=true` on success
- `simplyblock.io/vdo-capable=false` (or the label removed) on failure

**Operator override, implemented and live-verified.** The auto-detect probe above
runs on every `csi-node` pod restart, which would otherwise clobber a label an
operator set by hand (the escape hatch airgapped clusters and non-RHEL golden
images need, see [§13](#13-open-questions-and-discussion)). `advertiseVDOCapability`
stamps a second annotation, `simplyblock.io/vdo-capable-managed-by:
auto-detect`, alongside every label value it writes itself. On startup it first
checks whether `simplyblock.io/vdo-capable` is already present without that
annotation and, if so, leaves it untouched rather than overwriting it with a
fresh probe result.

---

## 5. Scheduling Gate: Topology

This repo already has working CSI topology infrastructure; this design extends
it:

- `nodeserver.go`:`buildAccessibleTopology` already turns node labels
  into CSI topology segments reported via `NodeGetInfo`. Add the same surfacing for
  `simplyblock.io/vdo-capable=true`.
- `VolumeBindingMode` is already `WaitForFirstConsumer`, so Kubernetes already defers
  binding until topology can be evaluated — this gate works with no scheduler changes.

With this in place, a PVC requesting client compression and/or deduplication is
simply never bound to a node lacking VDO support in the first place.

---

## 6. StorageClass Parameter and CRD Changes

Compression and deduplication are two independent parameters.
VDO natively supports enabling/disabling each separately
(`lvcreate --compression y|n --deduplication y|n`, and `lvchange` for live
toggling after creation — both verified hands-on, see Section 7), and they
have genuinely different cost profiles: compression is CPU-only and cheap;
deduplication carries a large, measured, fixed RAM cost per volume (its UDS
index) regardless of whether compression is also on. Collapsing them into one
switch would force every volume to pay dedup's cost just to get compression's
much cheaper benefit.

- `operator/api/v1alpha1/storagepool_types.go`, `StorageClassParameters`: add

  ```go
  // ClientCompression enables client-side (VDO) compression for logical
  // volumes in this pool. Distinct from Compression (server-side).
  // Independent of ClientDeduplication -- either, both, or neither may be set.
  // +kubebuilder:default=false
  ClientCompression *bool `json:"clientCompression,omitempty"`

  // ClientDeduplication enables client-side (VDO) deduplication for logical
  // volumes in this pool. Carries a significant, measured, fixed RAM cost per
  // volume (Section 7) independent of ClientCompression -- intended to be
  // opt-in on specific volumes/pools where duplicate data is actually expected
  // (VM images, container layers, backup targets), not enabled by default.
  // +kubebuilder:default=false
  ClientDeduplication *bool `json:"clientDeduplication,omitempty"`
  ```

- Regenerate via `make generate && make manifests` (controller-gen updates
  `zz_generated.deepcopy.go` and CRD YAML).
- `simplyblockstoragepool_controller.go` `mergeStorageClassParameters`: add

  ```go
  dst["client_compression"] = boolStr(p.ClientCompression)
  dst["client_deduplication"] = boolStr(p.ClientDeduplication)
  ```

- **VDO is needed whenever *either* parameter is true** — Section 4/5's node
  capability check and topology gate must trigger on
  `client_compression == "true" || client_deduplication == "true"`, not on
  `client_compression` alone (a dedup-only volume still needs a working `kvdo`
  module just as much as a compression-only one).

---

## 7. VDO Device Management (CSI Node Plugin)

New file: `csi-driver/pkg/util/vdo.go`, parallel in structure to `initiator.go`.

VDO is managed through LVM (`lvcreate --type vdo`), the only VDO management
interface available on this platform's target OS — the standalone `vdo` CLI
is not part of the shipped `vdo` package on RHEL-family systems. `lvm2`
provides native VDO support (`lvm segtypes` lists `vdo`/`vdo-pool`). Each CSI
volume gets its own PV/VG/vdo-pool/LV stack — one VDO instance per volume,
per the intent in Section 2.

### Device creation

```bash
DEV=/dev/disk/by-id/nvme-uuid.<lvol-uuid>
VOLID=<lvol-uuid>
pvcreate --devices $DEV $DEV
vgcreate --devices $DEV vdo-$VOLID $DEV
lvcreate --devices $DEV --type vdo --config "activation{checks=0}" -n $VOLID -l 100%FREE \
  --compression y --deduplication y vdo-$VOLID/vdopool --yes
mkfs.xfs -f /dev/vdo-$VOLID/$VOLID && mount /dev/vdo-$VOLID/$VOLID <stagingPath>
```

`$DEV` is the same stable `/dev/disk/by-id/nvme-uuid.<lvol-uuid>` path
`initiator.go`'s `waitForDeviceReady` already resolves — no new
device-resolution logic is required. The resulting dm device is named
`vdo-<VOLID>-vdopool-vpool` (not the LV name), which is what `vdostats`/
`dmsetup` need to reference it. `-l 100%FREE` sizes the pool to the device's
actual available capacity rather than a hardcoded value; omitting `-V` sizes
the logical volume to the largest size that remains safe within the pool even
under zero dedup/compression savings (per `man lvmvdo`). Every LVM command is
scoped to `$DEV` via `--devices` — see "HA-duplicate-PV fix" below for why
this is required, not just tidy.

*This sequence, run against an NVMe-oF-connected simplyblock lvol, produces a
mountable filesystem; a file written after mount and read back matched its
original checksum, and `vdostats` reported the expected pool/data-block
accounting.*

```go
// CreateOrAttachVDO idempotently ensures a VDO-backed logical volume exists on top of
// devicePath, named after lvolID, and returns the resulting device path to format/mount.
// If the VG already exists it is reactivated (never recreated); only a genuinely absent VG
// is created fresh. compression and deduplication are set independently at creation time --
// changing them on an already-existing volume is SetVDOFeatures' job, not this function's.
func CreateOrAttachVDO(
	ctx context.Context, devicePath, lvolID string, compression, deduplication bool,
) (string, error)

// SetVDOFeatures toggles compression/deduplication on an existing, active VDO volume
// without recreating it. Not wired into any live update path in v1 -- included because the
// underlying mechanism exists and is needed if a future StorageClass/VolumeAttributesClass
// update path is added.
func SetVDOFeatures(ctx context.Context, lvolID string, compression, deduplication bool) error

// RemoveVDO deactivates and removes the VG/LV stack for lvolID, destroying its data. Only
// appropriate when the underlying volume itself is actually being removed, or to clean up
// a stale/orphaned stack whose backing device is already gone -- never call this from a
// routine NodeUnstageVolume (see DeactivateVDO below).
func RemoveVDO(ctx context.Context, lvolID string) error

// DeactivateVDO deactivates (but does not destroy) the VG/LV stack for lvolID -- the correct,
// non-destructive counterpart to a plain NVMe-oF disconnect. NodeUnstageVolume fires any time
// no pod on this node currently needs the volume mounted, including a routine pod
// delete+recreate on the same node, not only when the volume is actually being deleted.
func DeactivateVDO(ctx context.Context, lvolID string) error

// ResolveClonedVDO handles the case where devicePath is a block-level CoW
// clone/snapshot-restore of another VDO-formatted volume: detects a
// byte-duplicate PV/VG UUID against an already-known volume (not simply "a
// VG exists"), regenerates fresh PV/VG UUIDs and renames the VG to lvolID
// (the vgimportclone equivalent), before any activation is attempted. Must
// run BEFORE CreateOrAttachVDO's own vgs/lvs check, since a byte-identical
// clone will otherwise look like "this volume's VG already exists" under the
// wrong name/UUID. See Section 12. Unlike XFS's `nouuid` mount flag for FS
// UUID collisions, there is no equivalent filesystem-level workaround for the
// LVM/VDO layer.
func ResolveClonedVDO(ctx context.Context, devicePath, lvolID string) error

// GrowVDO extends the VG's pool LV to consume all newly available physical space on
// devicePath (matching the 100%FREE convention used at creation time), then grows the VDO
// logical volume's own size to match, and returns its device path. Takes devicePath rather
// than a target size -- see Section 9 for why this differs from an earlier draft's signature.
func GrowVDO(ctx context.Context, devicePath, lvolID string) (string, error)
```

**HA-duplicate-PV fix (a real bug found and fixed live, not anticipated by
this design's original sequence above).** A volume's two redundant NVMe-oF HA
paths each surface as their own local `/dev/nvmeXnY` device node while
presenting byte-identical backend content. LVM's default, system-wide device
scan cannot tell these apart: `pvscan --cache <path>` hit a genuine "duplicate
PV" ambiguity between a volume's two HA device nodes, and even once commands
were pointed at one specific path, a name-based `vgs <name>` existence check
still reported a VG as present when it had never actually been created on
that device (this host restricts default LVM visibility via an
`/etc/lvm/devices/system.devices` devices file, so a name-only lookup does not
reliably tie its answer back to the intended device). Fixed in three parts,
found and verified across three rounds of live debugging:

1. Every LVM command in `CreateOrAttachVDO`/`ResolveClonedVDO`/`GrowVDO` is
   scoped to exactly one device via LVM's `--devices` flag, bypassing the
   system-wide scan entirely.
2. `vgExists` is content-based (`pvVGName(devicePath) == vg`), not the
   original name-based `vgs --devices <path> <name>` lookup, using the same
   content-scanning identity check `ResolveClonedVDO` already used for clone
   detection.
3. A new `vgHasLV` check distinguishes a fully created VDO stack from an
   **orphaned VG** left behind by an interrupted create (`pvcreate`/
   `vgcreate` completed, `lvcreate` did not): such a VG reports zero LVs, and
   `vgchange -ay` against it "succeeds" while producing no mountable device
   at all. `CreateOrAttachVDO` now removes an orphaned VG and falls through to
   a fresh create rather than reactivating it forever.

With all three in place, two independently toggled VDO pools (compression-only,
dedup-only) mounted successfully end-to-end on the same cluster where this was
first found, confirming the fix chain works together. See the test plan's
Error-case coverage for the live evidence.

Compression and deduplication have different cost profiles: deduplication
carries a measurably larger, fixed per-volume memory cost than compression
does, independent of whichever combination of the two is enabled. See
[§13](#13-open-questions-and-discussion) for a real-hardware measurement of
that cost.

### Notes

- **Stable device identifier — two independent layers of resilience to
  `/dev/nvmeXnY` renumbering, both UUID-based, neither using Serial**: use
  `/dev/disk/by-id/nvme-uuid.<lvol-uuid>`, not the raw enumerated
  `/dev/nvmeXnY` node, as the PV's backing device.

  **Why Serial doesn't work here**: real `nvme list` output for a simplyblock
  ha-mode volume —

  ```
  Node          Generic     SN    Model                                  Namespace  Usage
  /dev/nvme0n1  /dev/ng0n1  ha    b7d433b7-fd53-4f73-b3d6-726c224b30e5    0x1        10.00 GB / 10.00 GB
  ```

  — shows `SN` (Serial Number) as the literal string `"ha"`, identical for
  every ha-mode volume on the cluster, not unique at all. `Model`, by
  contrast, is the lvol's own UUID. This isn't a hypothetical concern about
  Serial; it's what real output actually shows.

  **NVMe-oF/udev layer**: SPDK deliberately sets the NVMe namespace UUID field
  equal to the lvol's own UUID at creation time — confirmed directly from real
  `nvmf_subsystem_add_ns` debug output during the clone spike:
  `'namespace': {'bdev_name': 'LVS_4/CLN_12', 'uuid':
  '63a83b95-87e5-4fb0-9bcf-75a6b7e0ad49', ...}`. `udev` derives its
  `/dev/disk/by-id/nvme-uuid.<uuid>` symlink from that same namespace UUID
  field, which is a property of the namespace, not of whichever
  `/dev/nvmeXnY` enumeration the kernel assigns this time — so the symlink is
  recreated pointing at the correct device regardless of which node it lands
  on after a reconnect. `initiator.go`'s own glob resolution (`DevDiskByID`
  combined with `nvmf.model` + `nsId`) keys off that same SPDK-controlled
  `Model` field for the same reason.

  **LVM layer, independently**: once `pvcreate` runs on top of that device,
  LVM writes its own separate UUID into the on-disk PV header, generated by
  `pvcreate` itself and unrelated to the NVMe namespace UUID. `pvscan --cache`
  finds a PV by scanning the *content* of every visible block device for that
  UUID, not by remembering a specific device path — confirmed directly by the
  reconnect/reactivate test above, which removed the backing device entirely
  and reattached it under a different device node with `pvscan --cache`
  finding it regardless.

  Both layers are UUID-based and mutually independent — the NVMe/udev layer
  gets a stable path via the namespace UUID (tied to the lvol's own identity
  by SPDK), and the LVM layer is separately resilient to path changes by
  design, since it identifies its PV by content-scanning rather than trusting
  a remembered path at all.

- **Reattaching after a lost connection:** with no standalone `vdo` CLI,
  reattaching an existing VDO volume after an NVMe-oF reconnect (Section 8) is
  an LVM reactivation, not a VDO-specific operation — `pvscan --cache`
  rediscovers the PV regardless of which device node it lands on, and
  `vgchange -ay` reactivates the VG/LV without recreating or reformatting
  anything.

  *Reproduced by removing the backing device entirely (confirmed via
  `losetup -a` returning empty) and reattaching via a new device node backed
  by the same data: `pvscan --cache` found the PV under its new path,
  `vgchange -ay` reactivated it, and a canary file's SHA-256 matched exactly
  before and after. The same sequence reattaches cleanly across a full node
  reboot as well — the VG is invisible immediately after boot, since its PV
  isn't present until NVMe-oF reconnects, with no ghost state in between.*

- **Clone/snapshot identity collision — why `ResolveClonedVDO` must run
  before `CreateOrAttachVDO`'s own check**: a cloned or snapshot-restored
  volume needs VDO applied just as much as its source — the VDO container is
  physically part of the copied bytes, not something the driver chooses. The
  problem is identity, not necessity: LVM identifies a PV/VG by a UUID stored
  in its on-disk metadata, not by which CSI volume it logically belongs to. A
  byte-level clone copies that metadata verbatim, so the clone's device
  reports the same PV UUID, VG UUID, and VG name (the source volume's ID) as
  the original.

  `CreateOrAttachVDO`'s normal check ("does a VG named `<this-volumeID>`
  exist?") answers **no** for a fresh clone, because the VG on disk is still
  named after the source — indistinguishable from a genuinely blank device.
  Proceeding on that answer is unsafe in both directions: `lvcreate` on a
  device that already holds a VDO container destroys the cloned data;
  activating the device under its inherited identity risks an LVM collision
  the moment the source volume's own device is also visible on the same node.

  **Detection — implemented more simply than originally planned**: rather
  than threading `VolumeContentSource` through the volume context, the
  implementation detects a clone unconditionally, on every stage, purely from
  the device's own on-disk identity (`pvs` on `devicePath` reports a VG name
  that isn't this volume's own ID). This is correct regardless of whether the
  content-source fact survived into the volume context, and is a no-op
  (returns immediately) for a genuinely fresh device or one already resolved
  to its own identity — so no separate "is this a clone" plumbing was needed
  at all.

  **Ordering**: `ResolveClonedVDO` must complete — regenerating fresh PV/VG
  UUIDs and renaming the VG to `volumeID` — before `CreateOrAttachVDO`'s
  `vgs`/`lvs` check ever runs. Once renamed, the device is indistinguishable
  from a normal, uniquely identified VDO volume, and every subsequent
  reattach, reconnect, or grow for it uses the same logic as any other volume
  (Sections 9-10); this is a one-time disambiguation at first stage, not an
  ongoing behavioral difference.

  *Reproduced against a real cluster: a clone's raw device reported the
  identical PV UUID as its source; LVM's own device scanner silently skipped
  the duplicate rather than surfacing an error, and mounting the volume by
  its own path returned the source's data instead of the clone's.
  `vgimportclone`, plus a forced `lvmdevices --adddev` to register the
  previously skipped device, resolved the collision by giving the clone its
  own PV/VG UUIDs — but left the clone's LV still named after the source, so
  a complete implementation also needs an explicit LV rename.*

- **Multi-instance memory scaling:** `KVDO module bytes used` is a
  module-wide total, not per-instance. Two simultaneous VDO-backed volumes
  (both with compression and deduplication enabled) reported combined memory
  within 0.0003% of exactly double the single-instance figure — there is no
  shared-memory benefit across instances; each additional
  deduplication-enabled volume adds the full ~390MB (see
  [§13](#13-open-questions-and-discussion) for that measurement). This is a
  deduplication-scaling risk specifically — a fleet of compression-only
  volumes, which do not carry deduplication's index cost, would scale at a
  much smaller figure instead.

- **Multiple instances across a restart:** now verified in combination, via
  the real implementation (`CreateOrAttachVDO`/`NodeStageVolume`), not just
  raw LVM commands.

  **Tested:**
  - Multiple VDO instances in parallel — two simultaneous VDO-backed volumes
    on the same node, no naming/dm collisions, linear memory scaling (see
    "Multi-instance memory scaling" above).
  - A single VDO instance surviving a node restart — `pvscan --cache` +
    `vgchange -ay` reattach cleanly across a full reboot, with no ghost state
    in between (see "Reattaching after a lost connection" above).
  - **The combination:** two real PVCs (`clientCompression`/
    `clientDeduplication` both true) provisioned against real NVMe-oF-backed
    lvols on the same node, each written with distinct, checksummed data,
    then the node rebooted. Both VDO instances reattached cleanly: `vgs`
    showed both VGs intact post-reboot, `kvdo`'s module-wide usage count
    matched exactly 2 (no leak, no orphan, no duplicate), both LVs reported
    `VDOOperatingMode: normal` with compression and deduplication still
    enabled, and both files' SHA-256 checksums matched exactly before and
    after. Driver logs confirm the actual sequence: post-reboot, kubelet
    calls `NodeStageVolume` fresh for both volumes (not `restageVolume` —
    kubelet's own bookkeeping resets on reboot too), each independently
    running `pvscan --cache` → `vgs` → `vgchange -ay` → mount, with no errors
    and no lock-contention symptoms.
  - **Caveat:** in this run, the two `NodeStageVolume` calls' underlying LVM
    command sequences happened to complete one after the other rather than
    genuinely overlapping (confirmed from log timestamps — each device's full
    `pvscan`/`vgs`/`vgchange` sequence finished before the next one's
    started). This confirms clean *sequential* reactivation of multiple
    instances after a reboot, but does not by itself prove safety under
    LVM commands that are genuinely racing at the same instant — that
    narrower race window remains unexercised.

  **Scenarios to be tested:**
  - Genuinely concurrent (not just closely timed) `vgchange -ay`/
    `pvscan --cache` calls actually overlapping in execution, to exercise
    LVM's internal command locking (`/run/lock/lvm`) directly — not achieved
    by the test above, where the sequences happened not to overlap.
  - A boot-time activation race across several VGs becoming visible in close
    succession, ahead of or interleaved with the CSI driver's own controlled
    reconnect logic (kubelet's own reconnect timing meant this didn't
    manifest in the run above, but wasn't deliberately forced either).

- **Stale VDO/LVM state after a node loses a volume without a clean
  unstage** (e.g., a pod force-rescheduled off a node that goes NotReady, or
  the storage side disconnecting the initiator while the node itself stays
  up): the correct teardown sequence depends on whether the node rebooted.

  **Tested:**
  - The dangerous variant — node stays up, backing device disappears without
    a clean disconnect — originally reproduced by accident during the
    memory-scaling measurement above (an orphaned VDO stack from an earlier
    real-cluster migration spike had its backing lvol deleted server-side
    while its dm-mapper entries were still live in the kernel; `vgremove`
    failed outright, "VG not found").
  - **Now reproduced deliberately, against the real implementation (PR
    #402), and the full self-healing path confirmed end-to-end**: created a
    real VDO-backed PVC/pod, wrote data, then forcibly disconnected the
    backing NVMe-oF subsystem at the host level (`nvme disconnect`) while the
    node and pod both stayed up — simulating "the storage side disconnecting
    the initiator while the node itself stays up" directly. Kernel logs
    confirmed the actual failure sequence: for roughly 19 seconds, reads and
    even a new write both *appeared* to succeed (silently absorbed by
    caching), before the real I/O failure surfaced ("attempt to access
    beyond end of device") once something actually needed to reach the
    vanished device. At that point VDO correctly fenced itself into
    **read-only mode** rather than corrupting anything, and ext4
    independently aborted its own journal for the same reason — both layers
    protected data correctly on their own, with no wiring from this design
    needed for that part. The pod stayed reported as `1/1 Running` by
    Kubernetes throughout, with no visible signal anything was wrong.
  - Deleting the pod after this point **found two real bugs, both now
    fixed**: (1) `NodeUnstageVolume`'s `DeactivateVDO` call had no fallback
    for an unreachable device — `vgchange -an` failed with the identical
    "Volume group ... not found" `vgremove` hits, on every one of 18 retries,
    until kubelet gave up and force-removed the pod anyway, leaving the
    orphaned stack permanently stuck with nothing left to clean it up. (2)
    once a `dmsetup remove` fallback was added to `DeactivateVDO` (mirroring
    `RemoveVDO`'s existing one), it *still* didn't work at first: the
    orphaned-device name matching compared against the plain VG name, but
    device-mapper flattens `<vg>-<lv>` into a dm device name by doubling
    every literal `-` in the VG/LV name components, so the match silently
    found nothing. With both fixed, the full sequence — disconnect, pod
    delete, `vgchange -an` failing correctly, falling back to `dmsetup
    remove`, clean final state — was reproduced a second time and confirmed
    fully automatic, no manual intervention needed.

  **Scenarios to be tested:**
  - The clean-reboot variant end-to-end: in-kernel dm tables are RAM-only and
    a reboot clears them, so this should reduce to stale
    `/etc/lvm/devices/system.devices` bookkeeping only (harmless per the
    skip-on-missing behavior above) — not yet deliberately reproduced.
  - Whether kubelet's own volume reconciler reliably re-invokes
    `NodeUnstageVolume` on the *original* node specifically after that node
    itself goes NotReady and later rejoins — the test above forced the
    device to disappear while the node and kubelet stayed healthy the whole
    time, then deleted the pod normally; kubelet's behavior when the node
    itself was unreachable for a period and the pod was rescheduled
    elsewhere in the meantime is a different, still-unverified code path.
  - Whether cleanup should also prune the corresponding `system.devices`
    entry (`lvmdevices --deldev`/`--delpvid`) to stop it from accumulating
    dead entries across repeated node-failure cycles over a node's lifetime
    — not exercised by the fix above, which only addressed the live
    dm-mapper state, not any host-level LVM devices-file bookkeeping.

- **The ~390MB deduplication cost is constant with respect to volume size, by
  construction, not by coincidence of the sizes tested.** The measurements
  above span only a narrow range (5.1-9.2GiB), so constant-across-that-range
  alone wouldn't rule out cost scaling with size at a wider range. Checked
  directly against `lvm2`'s own compiled-in defaults instead:

  ```
  lvm dumpconfig --type default allocation/vdo_index_memory_size_mb
  # vdo_index_memory_size_mb=256
  ```

  `256` is a fixed default in `lvm2` itself, not a value `lvcreate` derives
  from the pool or device size — every instance measured in this section used
  this same default, since none of the test commands passed an explicit
  override (`--config allocation/vdo_index_memory_size_mb=...` or a custom
  metadata profile). The ~390MB figure is this 256MB UDS index plus roughly
  130-140MB of other fixed `kvdo` module structures, entirely decoupled from
  `-l`/`-L`. Barring an explicit override, this cost should hold at any
  volume size — 10GiB or 10TiB — not just the range actually measured.

  This does not mean deduplication *effectiveness* is size-independent,
  though, and the two shouldn't be conflated: a dense 256MB index covers
  roughly a 1TiB deduplication window (per VDO's own sizing guidance). A
  volume much larger than that, still using the same default index, would
  keep costing the same ~390MB but see deduplication effectiveness degrade
  for data beyond that window — a capacity ceiling on the *benefit*, not the
  *cost*. Whether the platform ever needs to raise `vdo_index_memory_size_mb`
  for specific large volumes is a separate tuning question from the fixed
  per-instance RAM tax documented above.

- **Minimum volume size:** `lvcreate --type vdo` enforces a hard floor around
  4.72GiB (`Minimum required size for VDO volume: 5063921664 bytes`) — not a
  tunable slab-size parameter. Any PVC smaller than this cannot use VDO at
  all, and the platform's minimum supported PVC size needs to be checked
  against this constraint before assuming VDO is always available on request.

- **Idempotency:** `lvcreate --type vdo -n X ...` fails if `X` already
  exists — the same check-then-act principle applies as for any other LVM
  object (`vgs`/`lvs`); there is no VDO-specific idempotency mechanism to rely
  on instead.

- **Write policy (`sync`/`async`/`auto`):** `auto` (LVM's default, and what
  every example command in this doc uses — none override it) picks `sync` or
  `async` based on whether the backing device reports a volatile write cache.
  A loop device typically doesn't report one, so any measurement taken
  against a loop device (as in earlier drafts of this doc, since removed) was
  most likely exercising `sync`, not what real deployments actually get.

  *Checked against a real NVMe-oF-backed lvol on `vm04`*: `nvme id-ctrl`
  reports `vwc: 0x5` (volatile-write-cache-present bit set), and the kernel
  agrees (`/sys/block/nvme0n1/queue/write_cache` = `write back`) — real
  simplyblock-backed storage does advertise a write cache to the client,
  unlike a loop device. The same VDO volume was then created three times on
  that real device, forcing `sync`, `async`, and `auto` in turn, each put
  through an identical `fio` sequential-write test (1MB blocks, `iodepth=16`,
  direct I/O, 2000MB): `sync` 37.3 MiB/s, `async` 35.7 MiB/s, `auto`
  34.5 MiB/s — within one run-to-run standard deviation of each other, i.e.,
  **no measurable throughput difference** between policies on real hardware.
  Something other than the local sync-vs-cache-ack decision dominates cost
  here; the large gap seen in earlier loop-device numbers doesn't generalize.

  **Recommendation**: don't force a non-default write policy for tests or for
  this feature. There's no throughput upside to justify it — the measurement
  above removes the "async is much faster" argument. Leave `vdo_write_policy`
  at its `auto` default — it's also the most representative choice for
  tests, since it's exactly what production gets with zero deliberate
  configuration.

  **Crash-consistency, now tested for real**: forced `vdo_write_policy=async`
  explicitly on a real NVMe-oF-backed lvol on `vm04`, wrote one file with an
  explicit `fsync()` and a second file without one, then genuinely crashed
  the node (`echo 1 > /proc/sys/kernel/sysrq && echo b > /proc/sysrq-trigger`
  — an immediate hard reboot with zero filesystem sync, run with direct VM
  access rather than through a container, since triggering a real kernel
  reboot needs `CAP_SYS_BOOT`, which neither `kubectl debug node`'s
  `sysadmin` profile nor an ordinary `privileged: true` pod's default
  capability set reliably includes). Confirmed via a genuinely new
  `who -b` boot timestamp that this was a real crash, not a graceful reboot
  that would have flushed everything anyway and told us nothing. Result: the
  `fsync()`'d file survived with an exact checksum match; the file written
  without `fsync()` was lost entirely (didn't exist after remount) — exactly
  the POSIX-correct outcome, and useful confirmation the test genuinely
  distinguished durable from non-durable writes rather than everything
  simply surviving by luck. **`async` correctly honors flush/FUA durability
  end-to-end through NVMe-oF to the simplyblock backend — an acknowledged
  (`fsync()`'d) write is safe under `async`, not just under `sync`.**

### Wiring into `nodeserver.go`

- `NodeStageVolume`: when **either** `vc["client_compression"] == "true"`
  **or** `vc["client_deduplication"] == "true"`, call `CreateOrAttachVDO`
  (passing both booleans through independently — see Section 6) between
  `initiator.Connect` and `ns.stageVolume`; pass the returned VDO device path
  into `stageVolume` instead of the raw NVMe-oF path. Stash this in the
  volume context (already persisted via `util.StashVolumeContext`) so
  Unstage/restage paths know VDO is in play. If the volume was created from a
  `VolumeContentSource` (clone/snapshot restore), call `ResolveClonedVDO`
  first (see above) — this is a required step, not optional hardening.
- `NodeUnstageVolume`: if the volume context indicates VDO was used, call
  `DeactivateVDO` before `initiator.Disconnect` — VDO must come down before
  the device underneath it is disconnected, and this is `DeactivateVDO`
  (deactivate, not destroy) specifically, since this path fires on every
  routine pod delete/recreate, not only when the volume is actually being
  removed (see Section 7's `DeactivateVDO` note).
- `restageVolume` and `ensureDeviceConnected`: after `initiator.Connect`
  re-establishes the raw device, reattach (LVM reactivate: `pvscan --cache` /
  `vgchange -ay`, never `lvcreate`) the existing VDO device before
  remounting, mirroring `restageVolume`'s existing "never reformat, data
  already exists" invariant. This is the mechanism satisfying the issue's
  requirement that the compression device be re-included on every
  re-provision.
- `stageVolume` already calls `xfsStripeOptions` to align `mkfs.xfs` to the
  backend's erasure-coding stripe geometry (`xfs_su`/`xfs_sw`). This must be
  skipped whenever a VDO layer is in play (`client_compression` or
  `client_deduplication`) — VDO virtualizes and relocates blocks, so the
  filesystem is no longer directly on the erasure-coded device; applying
  stripe alignment hints computed for the raw device to a VDO virtual device
  is not just useless, it's actively misleading. See Section 12.

---

## 8. Re-Provisioning and Failure Handling

Topology gating (Section 5) only controls *initial* PV scheduling. It does not
protect against a node's capability regressing after a volume is already bound
there (e.g., an OS update removes kvdo compatibility) — Kubernetes will not evict
an already-running pod just because its node's label flipped.

Because whether a volume uses VDO is baked into its on-disk format at first
creation (the raw device holds a VDO container, not a bare filesystem), a later
`NodeStageVolume`/`restageVolume` on a node without working VDO **cannot safely
fall back to a raw mount** — the bytes on disk are VDO-formatted. Silently
skipping the compression layer at that point would either fail to mount at all
(if VDO was already in use) or hide a real problem with zero signal (if the
volume is new and VDO silently isn't applied).

Therefore: if `client_compression == "true"` **or** `client_deduplication ==
"true"` is present in the volume context but `CreateOrAttachVDO` fails locally, `NodeStageVolume` and
`restageVolume` must return a hard, visible error — never silently proceed with
a raw mount. This should be alertable (surfaced via existing klog error paths,
consistent with how other hard failures in this file are already reported).

---

## 9. Volume Expansion

`NodeExpandVolume` (`nodeserver.go`) currently resizes the filesystem directly
against the device path from the volume context. **Corrected from an earlier
draft**: there is no standalone `vdo growPhysical`/`growLogical` command on
this system (see Section 7) — growing an LVM-managed VDO volume is an
`lvextend` operation against the pool LV (physical) and/or the VDO LV
(logical), followed by the existing filesystem resize logic
(`mount.NewResizeFs`) against the now-larger VDO logical device.

**Now verified hands-on, fully online, on `vm04`**: grew the backing loop
device (`truncate` + `losetup -c`), `pvresize`'d the PV, `lvextend`'d the
vdo-pool LV (physical, 5.5G→7.5G — succeeded while the filesystem stayed
mounted), `lvextend`'d the VDO LV (logical, 4G→6G), then `resize2fs` online.
Filesystem grew from 3.9G to 5.9G usable, a canary file's checksum survived
unchanged throughout, and 500MB of new (incompressible) data written into the
newly available space confirmed it's genuinely usable, not just reported. As
a side effect, `Data%` on the pool dropped from 63.68% to 46.71% after growth —
consistent with Section 7's finding that VDO's overhead is roughly fixed in
absolute terms, so it becomes a smaller fraction of larger pools. Not yet
tested: growth behavior specifically *at* the ~4.72GiB minimum-size floor
(this spike started from an already-above-floor 5.5G pool).

---

## 10. RBAC Changes

**Implemented.** `helm-charts/charts/simplyblock-operator/templates/node-rbac.yaml`
grants the CSI node's ClusterRole `patch`/`update` on `nodes` (alongside the
pre-existing `get`/`list`/`watch`), which the capability-labeling step in
Section 4 needs to write the `simplyblock.io/vdo-capable` label and its
`vdo-capable-managed-by` annotation.

---

## 11. Testing Strategy

The mechanism is implemented and live-verified end-to-end on a real cluster
(`config-israel`): CSI-integrated Stage/Unstage/expand/reconnect paths, node
capability auto-detection and operator override, the topology gate including
a genuine cross-node reschedule, and `VolumeMigration` compatibility all have
real evidence, not just unit coverage. What remains open is concentrated in a
few specific gaps rather than spread across the whole feature: interrupt
recovery mid-`lvcreate`, a live `kvdo`-unloadable regression, and load/scale
behavior at realistic fleet density. See [§13](#13-open-questions-and-discussion)
for the design-level open questions this doesn't resolve.

Full scenario matrix, coverage status, and hand-off test concepts:
[`tests/test-plan-issue-277-client-side-compression.md`](../tests/test-plan-issue-277-client-side-compression.md).

- **Unit:** `CreateOrAttachVDO`/`ResolveClonedVDO`/`RemoveVDO`/`GrowVDO`'s
  check-then-act branches (mocked `exec`), the `vdo-capable` label/annotation
  override logic, and the StorageClass parameter/topology generation, all
  without a cluster.
- **Integration:** real LVM/VDO commands against a real NVMe-oF-backed lvol,
  non-destructive: creation, reactivation, growth, clone resolution.
- **E2E:** the full StorageClass → scheduler → `NodeStageVolume` path on a
  real multi-node cluster, including reconnect, reboot, migration, and
  cross-node reschedule.
- **Load / long-running:** many VDO-backed volumes on one node and sustained
  write throughput. Only a single-node memory ceiling has been measured so
  far (see the test plan's Load Cases).

---

## 12. Compatibility with Existing CSI Driver / Operator Features

A full inventory of this repo's CSI RPCs, StorageClass parameters, CRDs, and
named subsystems was cross-checked against this design. Verdicts:

### Confirmed compatible, no changes needed
- **`CreateVolume`/`DeleteVolume`/`ControllerGetVolume`/`ValidateVolumeCapabilities`**
  — server-side lvol lifecycle, unaffected; VDO is purely a node-side addition.
- **`ControllerExpandVolume`:** feeds `NodeExpandVolume`'s existing size info;
  Section 9's `lvextend` chain consumes it the same way the existing resize
  logic does.
- **Guardian** (`csi-driver/pkg/util/guardian.go`) — **verified hands-on via
  research, not assumed**: `MarkBrokenLvol` is pure Kubernetes-level
  bookkeeping (marks state, later deletes the pod to force a fresh
  Stage/Publish cycle) and never touches the device/mount/dm layer itself.
  The actual device-level repair happens in `restageVolume`/
  `ensureDeviceConnected`, which this design already updates (Section 8) to
  reactivate the VDO/LVM stack. Guardian needs zero VDO-awareness.
- **VolumeMigration:** **verified end-to-end against the real `VolumeMigration`
  CRD and controller** (not the raw `sbctl lvol migrate` CLI this section
  originally spiked against), including the actual cutover this section had
  previously left unobserved. Two full runs on `config-israel`: a plain
  volume and a VDO-backed volume, each written with checksummed data, each
  migrated to a different storage node via a `VolumeMigration` CR. Both
  completed (`Phase: Completed`) in under 30 seconds, with the consuming
  pod's restart count staying `0` and the data checksum matching afterward.
  The cutover this section previously couldn't observe completing is now
  confirmed clean end-to-end.
  **New finding**: client-side VDO turns out to be completely unaffected by a
  migration, and not merely compatible with it as this section originally
  reasoned. `dm-vdo` lives on the **consumer** node, on top of the NVMe-oF
  client connection, never on the storage backend node a migration moves.
  Confirmed via `nvme list-subsys` on the consumer node: migration only
  re-points the underlying NVMe-oF path (the primary path's `traddr` switched
  from the source node's IP to the target's, and the HA sibling path was
  untouched), while the `dm-vdo` device and its filesystem mount on top never
  noticed. `CreateOrAttachVDO` is never re-invoked by a migration at all. One
  direct consequence: **a migration target node's `vdo-capable` status is
  irrelevant and requires no check.** Confirmed in code: the
  `VolumeMigration` controller has no `vdo-capable` reference of any kind.
  A volume that is part of a shared-namespace subsystem (i.e., has clones,
  see the CoW clone finding above) **cannot be migrated individually.** The
  backend enforces atomic group migration of the whole subsystem itself, and
  this doesn't require new client-side design work: after `ResolveClonedVDO`
  gives each clone its own independent PV/VG/LV identity (Section 7), each
  volume's reconnect logic already operates independently of any others
  sharing its original subsystem.
- **Encryption — verified compatible, not just assumed orthogonal** (see the
  detailed spike writeup below this list): the NVMe-oF client always receives
  plaintext for an "encrypted" volume, since the crypto bdev sits below the
  NVMf attach point server-side. `encryption=true` + `client_compression=true`
  is a fully compatible combination.
- **`replicate`, `distr_ndcs`/`distr_npcs`, `tune2fsReservedBlocks`,
  multi-cluster (zone/region routing), DHCHAP, node drain/recycle,
  rebalancer, failure domains, static PVC support, RBAC tenancy** — all
  server-side or connection-layer concerns, orthogonal to a node-local
  client-side block device stack. Static PVC users simply need to set
  `client_compression` in the static PV's `volumeAttributes` by hand.

**Encryption + client-side compression — spike detail**: an earlier draft of
this review assumed the client receives ciphertext for an encrypted volume,
which would defeat VDO's compression/dedup almost entirely. This was
retracted after verifying the actual bdev construction in `sbcli`
(`simplyblock_core/controllers/lvol_controller.py`): when encryption is
enabled, a crypto vbdev is layered on top of the base lvol and
`lvol.top_bdev` is reassigned to point at it (line 671); the NVMf
namespace-add RPC always exposes `lvol.top_bdev` (lines 1180-1181,
1299-1301) — the crypto bdev sits **below** the NVMf attach point, so every
NVMe-oF read is already decrypted server-side before bytes reach the wire.
The DEK is fetched from a server-side KMS and installed entirely on the
storage node (`_create_crypto_lvol`, lines 31-73); no key material ever
transits the CSI/client path (confirmed: `csi-driver/pkg/util/nvmf.go:206`,
`controllerserver.go:767,833` only pass a boolean flag). **The client always
receives plaintext — encryption here is at-rest only.**

The mechanical half of the original concern was empirically confirmed
correct in isolation, for completeness: writing the same ~536MB realistic
text dataset (repeated real journal logs) to a fresh VDO volume both as
plaintext and as AES-256-CTR ciphertext showed plaintext achieving a **~12.4x
reduction** (11,072 data blocks used for the full dataset) versus ciphertext
landing at **~1:1, effectively zero savings** (137,186 new data blocks for
137,173 new logical blocks written) — ciphertext genuinely doesn't compress
or dedup. But since the architecture never exposes ciphertext to the client,
this failure mode does not occur here. **No design change needed.**

### Real gaps found — need design/code changes

- **🔴 Clone/snapshot-restore volumes will carry byte-duplicate LVM/VDO
  metadata — confirmed, not speculative.** `CreateSnapshot`/clone provisioning
  in this driver is a true block-level CoW clone (SPDK-backed), not a
  copy-and-reformat. `stageVolume`'s `FormatAndMountSensitiveWithFormatOptions`
  already `blkid`-probes and skips `mkfs` if a filesystem is detected — proven
  by an **existing workaround already in the codebase**:
  `stagingMountFlags` adds `nouuid` for XFS specifically because "XFS refuses
  to mount two filesystems with the same UUID" on a volume and its
  clone/restored snapshot. A VDO/LVM stack has **no equivalent per-mount
  escape hatch** — `pvscan`/`vgscan`/udev auto-activation operate on the
  whole node's device namespace, not per-mount, so a byte-duplicate PV/VG
  UUID could cause `vgchange`/`lvm2-activation` failures or silent
  misdetection node-wide, merely from both devices being *visible* to the
  same node — not even requiring both to be mounted simultaneously (common in
  Kubernetes: clones are frequently scheduled onto the same node as their
  source). **Fix**: a `vgimportclone`-equivalent UUID-regeneration step
  (`ResolveClonedVDO`, added to Section 7) must run before any LVM activation
  whenever `VolumeContentSource` is set. This is the single most important
  finding from this review — it's a correctness gap, not a tuning question.
- **🟡 Existing XFS stripe-alignment tuning (`xfs_su`/`xfs_sw`,
  `xfsStripeOptions`) becomes meaningless — and actively misleading — under
  VDO.** VDO virtualizes and relocates blocks for deduplication, so a
  filesystem on a VDO device is no longer directly on the erasure-coded
  backend device the stripe hints were computed for. **Fix**: skip
  `xfsStripeOptions` entirely when `client_compression` is set (added to
  Section 7's wiring notes).
- ~~Server-side `encryption=true` + `client_compression=true` likely defeats
  compression~~ **Retracted — spiked and confirmed wrong; moved to "Confirmed
  compatible" above.** The client always receives plaintext for an encrypted
  volume (crypto bdev sits below the NVMf attach point, server-side-only key),
  so this combination is fine. See the detailed writeup above.
- **🟡 Server-side `compression=true` + `client_compression=true` is
  redundant, not harmful.** Compressing already-compressed data wastes CPU on
  both ends for negligible additional savings — a much milder version of the
  encryption issue. Same recommendation: worth a warning, lower priority than
  the encryption case. `client_deduplication` has no server-side counterpart
  to worry about here — the existing server-side feature is compression-only.

### Needs a one-line confirmation, not a design change
- ~~Placement webhook / volume placement injector~~ **Resolved (read the
  code): no conflict exists.** `SimplyblockVolumePlacementInjector` only picks
  which storage backend node hosts the lvol (a `host_id` hint annotation
  `CreateVolume` consumes); it never touches pod scheduling or injects pod
  affinity. It cannot conflict with Section 5's `vdo-capable` topology gate
  because the two apply to different things entirely: this webhook selects a
  storage node, the topology gate constrains the consuming pod's node. Also
  confirms client-side VDO's backend-node-agnosticism directly: `VolumeMigration`
  moving a VDO-backed volume's storage node has no `vdo-capable` check at all
  (verified live and in code, this session), correct since `dm-vdo` runs on
  the consumer node on top of the NVMe-oF client connection, never on the
  backend.
- ~~`AllowedTopologies` composition with DHCHAP's existing `allowedNodes`
  gate~~ **Resolved (read the code):** `createStorageClassIfNotExists`
  (`simplyblockstoragepool_controller.go`) builds one `TopologySelectorTerm`
  and appends both the DHCHAP and the `vdo-capable` expressions into its
  single `MatchLabelExpressions` list when both apply to the same pool. Its
  own doc comment states the reason explicitly: a list within one term
  is ANDed by Kubernetes, whereas separate terms would be ORed and
  incorrectly let a volume needing both land on a node satisfying only one.
  Implemented correctly from the start, not something this review needed to
  fix.

---

## 13. Open Questions and Discussion

- **Unresolved policy question: what exactly qualifies a volume for
  deduplication?** Per team discussion, deduplication is meant to be
  restricted to "specific volumes" rather than freely available anywhere
  compression is — a deliberate choice given the measured, fixed ~390MB/volume
  RAM cost (below) that compression alone doesn't carry. But the concrete
  rule hasn't been decided. Candidates, each implying different plumbing:
  - **Pool/StorageClass-level allowlist:** an admin explicitly enables
    `clientDeduplication` on specific pools (mirrors the existing
    `DHCHAP`/`AllowedNodes` pattern already on `Pool`).
  - **Workload-type label/annotation:** e.g., only PVCs labeled as VM-image or
    backup-target storage get dedup, auto-applied rather than admin-toggled
    per pool.
  - **Size threshold:** dedup only below/above some volume size, since the
    fixed per-instance index cost matters proportionally more for small
    volumes.
  This needs to be settled with the team before `ClientDeduplication`'s exact
  validation/gating behavior (Section 6) can be finalized — right now the
  field is a plain independent boolean with no restriction beyond the
  topology capability gate, which may not be what's wanted.
- ~~🔴 `upsertStorageClass` is create-only, not create-or-update — enabling
  compression/dedup on an existing Pool silently does nothing~~ **Resolved,
  by a stronger guarantee than either option this originally proposed.**
  `createStorageClassIfNotExists`
  (`operator/internal/controller/simplyblockstoragepool_controller.go`, its
  current name; this section originally cited it as `upsertStorageClass`)
  is still intentionally create-only, and a `Create` against an
  already-existing name still hits `AlreadyExists` and no-ops. But
  `StoragePoolSpec.StorageClassParameters` now carries `+k8s:immutable`,
  which `controller-gen` turns into an `x-kubernetes-validations: self ==
  oldSelf` rule the API server itself enforces. An attempt to edit
  `storageClassParameters` on an already-created Pool is **rejected outright
  by Kubernetes**, with an explicit validation error at the moment of the
  edit, not silently swallowed later inside the reconciler. This is neither
  of the two fixes originally proposed (a status condition, or documenting
  the limitation): it prevents the invalid edit from ever being persisted at
  all, so there is no silent-no-op window to detect or document. One loose
  end: the `ClientCompression`/`ClientDeduplication` field-level doc comments
  in `storagepool_types.go` still describe the old "changing this has no
  effect" behavior and cite issue #401 — worth updating to describe the
  actual, stronger guarantee (an explicit rejection, not a silent no-op) now
  that the type-level comment on `StorageClassParameters` already does.
- ~~Standalone `vdo` CLI longevity~~ **Resolved (verified hands-on)**: it's
  already gone, not just uncertain — the installed `vdo` 8.2.2.2 package on
  this cluster ships no `vdo` binary at all, only `vdoformat`/`vdostats`/etc.
  LVM-integrated VDO (`lvcreate --type vdo`) is the only management path that
  exists; Section 7 is now written against that reality.
- ~~Airgapped install~~ **Partially resolved**: `advertiseVDOCapability` now
  leaves an operator-set `simplyblock.io/vdo-capable` label alone rather than
  overwriting it with its own probe result, keyed off a
  `simplyblock.io/vdo-capable-managed-by` annotation the auto-detect path
  stamps on every label it writes itself (absent, or any other value, means a
  human set it). A cluster admin can label an airgapped or golden-image node
  by hand and it sticks. What's still open: no *tooling* to help an admin
  decide which nodes qualify (e.g., a kernel-version/package-availability
  check they could run before labeling) — this only solves "can override,"
  not "how do I know what to set it to."
- **Kernel/distro coverage beyond RHEL-family:** this design assumes a
  RHEL-compatible distro with `kmod-kvdo`/`vdo` packages available. Non-RHEL
  K8s node OSes (e.g., Ubuntu-based managed node images) were not evaluated and
  may not have an equivalent path at all — worth scoping which distros this
  feature needs to support before wide rollout.
- **Memory/CPU cost per VDO instance — now measured, not estimated, and worse
  than the earlier documentation-sourced guess**: a real VDO-backed LV at the
  minimum viable size (5.5GiB physical pool, `vm04`) reported, via
  `vdostats --verbose`, **`KVDO module bytes used: 408960848`** (~390MB of
  kernel RAM) for that single instance, and **918272 overhead blocks
  (~3.5GiB, 64% of the pool) consumed by VDO's own bookkeeping before a single
  byte of user data was written** — only 642 data blocks (~2.6MB) held actual
  post-dedup/compression data from 600MB of highly duplicate/compressible test
  input. With many small-to-medium PVCs per node, this is a real, significant
  per-node RAM and usable-capacity tax (~390MB RAM and ~3.5GiB of "wasted"
  physical space *per volume*, at minimum size) — not a rough theoretical
  concern. Whether the overhead ratio improves meaningfully for larger pools
  (bigger slab configuration) is unverified and should be tested before
  committing to "one VDO instance per PVC" for the platform's typical PVC size
  range.
- **DKMS/source-build does not escape the kernel-currency requirement**
  (checked and ruled out on `vm02` — a **prerequisite check only**: no source
  was cloned, no build was attempted, no module was loaded; this was closed
  off before reaching an actual build step). The hope was that building `kvdo`
  from
  upstream source ([github.com/dm-vdo/kvdo](https://github.com/dm-vdo/kvdo) —
  confirmed to be exactly the source Red Hat builds `kmod-kvdo` from) against
  the exact running kernel's headers might avoid the prebuilt-`kmod-kvdo`
  NVR-pinning fragility entirely. Note the source itself is *not* pinned to
  exact kernel NVRs the way the prebuilt RPM is — its `8.2.x.x` branch targets
  the whole `5.14.0-*.el9` family generically via
  `make -C /usr/src/kernels/$(uname -r) M=$(pwd)`. The blocker in practice was
  narrower: the repo providing `kernel-devel` only retains headers for the
  same small set of "current" kernel builds that `kmod-kvdo` itself targets —
  the running kernel's own `kernel-devel` package (`503.14.1.el9_5`) was **not
  available** (general internet/GitHub reachability was confirmed fine; the
  missing piece is vendor-side header retention, not network access, build
  tooling, or source versioning). Net effect is the same either way: the only
  real fix is running a kernel build the vendor is still actively shipping
  matching artifacts for — **node kernel currency is the actual prerequisite**,
  not a packaging inconvenience to engineer around.
- **VDO is now upstream/in-tree as of kernel 6.9:** per the `dm-vdo/kvdo`
  README, *"This repository is no longer being updated for newer kernels...
  The latest version of this project is available in the Linux kernel as the
  dm-vdo module starting in version 6.9."* On any node running a kernel ≥6.9,
  none of Section 4's install/modprobe/grub-pinning machinery is needed at
  all — `dm-vdo` is already part of the kernel, so node-capability detection
  degrades to a plain `modprobe dm-vdo` (or config) check with no package
  install and no boot-default side effect. Worth checking whether any of
  simplyblock's target node OSes (newer Ubuntu, a future RHEL/Rocky rebase)
  already clear that bar — Section 4 should detect and prefer the in-tree
  `dm-vdo` path over the legacy `kvdo`/`kmod-kvdo` install path when present,
  rather than assuming the RHEL9-era out-of-tree path forever.
- **Crash/interrupt recovery during `lvcreate --type vdo` — inconclusive, still
  open**: three separate attempts to interrupt `lvcreate --type vdo` mid-
  transaction (fixed-delay kill, immediate-detection kill, `SIGSTOP`) all
  failed to catch a genuinely partial state — the operation completes in
  well under a second even at 8-9GB. This bounds the practical risk (a
  `NodeStageVolume` interrupted by, say, a killed CSI pod has a narrow window
  to land mid-`lvcreate`) but doesn't rule it out — real production volumes,
  slower network-attached NVMe-oF storage, or a loaded node could widen the
  window. `CreateOrAttachVDO`'s check-then-act idempotency (Section 7) should
  still handle a genuinely half-created VG defensively (detect and clean up
  rather than error unrecoverably), even though this session couldn't
  reproduce that state to verify it end-to-end.
