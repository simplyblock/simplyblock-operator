# Design Document: Client-Side Compression (Issue #277)

**Status:** Partially Implemented  
**Author:** Manohar Reddy  
**Date:** 2026-08-06 (last updated 2026-08-31)  
**Issue:** https://github.com/simplyblock/simplyblock-operator/issues/277
**Test Plan:** [`tests/test-plan-issue-277-client-side-compression.md`](../tests/test-plan-issue-277-client-side-compression.md)

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
    - [Clone and snapshot restore handling](#clone-and-snapshot-restore-handling)
    - [Wiring into `nodeserver.go`](#wiring-into-nodeservergo)
  - [8. Re-Provisioning and Failure Handling](#8-re-provisioning-and-failure-handling)
  - [9. Volume Expansion](#9-volume-expansion)
  - [10. RBAC Changes](#10-rbac-changes)
  - [11. Testing Strategy](#11-testing-strategy)
  - [12. Compatibility with Existing CSI Driver / Operator Features](#12-compatibility-with-existing-csi-driver--operator-features)
    - [Confirmed compatible, no changes needed](#confirmed-compatible-no-changes-needed)

---

## 1. Background

[Issue #277](https://github.com/simplyblock/simplyblock-operator/issues/277) asks
for a client-side compression/dedup layer: when requested via a StorageClass, a
compression block device is auto-created between the CSI mount and the NVMe-oF
multipath device used for the volume, and this device must be re-included every
time the node-side CSI driver re-provisions the volume. VDO (Virtual Data
Optimizer, the `dm-vdo` device-mapper target) is the mechanism used to
achieve this.

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
- Supporting non-RHEL-family node OSes. `kmod-kvdo`/`vdo` packages are a
  RHEL-family concept; a distro like Ubuntu has no equivalent path evaluated
  or supported by this design.

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

**In-tree `dm-vdo` on kernel 6.9+.** Per the `dm-vdo/kvdo` project's own
README, the out-of-tree `kvdo` module is no longer updated for newer
kernels: its functionality merged into the mainline kernel as the `dm-vdo`
module starting at 6.9. On a node already running a kernel at or above that
line, none of the `dnf install`/`modprobe kvdo` machinery above is needed.
The capability check tries `modprobe dm-vdo` first, and only falls through
to the `kmod-kvdo`/`vdo` install path above when that fails, so a node on a
current-enough kernel gets VDO capability with no install step, and no
`dnf`/BaseOS dependency, at all.

### Advertise via node label

Record success/failure of `modprobe kvdo` and add it to the node label

- `simplyblock.io/vdo-capable=true` on success
- `simplyblock.io/vdo-capable=false` (or the label removed) on failure

**Operator override, implemented and live-verified.** The auto-detect probe above
runs on every `csi-node` pod restart, which would otherwise clobber a label an
operator set by hand (the escape hatch an airgapped cluster or a golden-image
node needs). `advertiseVDOCapability`
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
- **A PVC requesting `clientCompression` or `clientDeduplication` must be at
  least 5GiB.** VDO enforces its own hard minimum around 4.72GiB; a smaller
  device cannot hold a VDO container at all.

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
scoped to `$DEV` via `--devices` — see "HA-mode device identity" below for
why this is required, not just tidy.

*This sequence, run against an NVMe-oF-connected simplyblock lvol, produces a
mountable filesystem; a file written after mount and read back matched its
original checksum, and `vdostats` reported the expected pool/data-block
accounting.*

**Why `$DEV` is stable, at two independent layers.** Neither uses Serial: a
real ha-mode volume's `nvme list` output shows `SN` as the literal string
`"ha"`, identical across every ha-mode volume on the cluster, not unique at
all. `Model`, by contrast, is the lvol's own UUID.

- **NVMe-oF/udev layer:** SPDK sets the NVMe namespace UUID field equal to
  the lvol's own UUID at creation time, and `udev` derives the
  `/dev/disk/by-id/nvme-uuid.<uuid>` symlink from that same field, a property
  of the namespace rather than of whichever `/dev/nvmeXnY` enumeration the
  kernel assigns this time. The symlink is recreated pointing at the correct
  device regardless of which node it lands on after a reconnect.
  `initiator.go`'s own glob resolution (`DevDiskByID` combined with
  `nvmf.model` + `nsId`) keys off that same SPDK-controlled `Model` field.
- **LVM layer, independently:** once `pvcreate` runs on top of that device,
  LVM writes its own separate UUID into the on-disk PV header, unrelated to
  the NVMe namespace UUID. `pvscan --cache` finds a PV by scanning the
  *content* of every visible block device for that UUID, not by remembering
  a specific device path, so it locates the same PV under a new device node
  after a reconnect without any resolution logic of this design's own.

Idempotency follows the same principle as identity: `lvcreate --type vdo -n
X ...` fails if `X` already exists, the same check-then-act that
`CreateOrAttachVDO` applies via `vgs`/`lvs` before ever calling it, since
there is no VDO-specific idempotency mechanism to rely on instead. And VDO
itself enforces a hard floor around 4.72GiB (`Minimum required size for VDO
volume: 5063921664 bytes`), not a tunable slab-size parameter: any PVC
smaller than this cannot use VDO at all.

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
// wrong name/UUID. Unlike XFS's `nouuid` mount flag for FS UUID collisions,
// there is no equivalent filesystem-level workaround for the LVM/VDO layer.
func ResolveClonedVDO(ctx context.Context, devicePath, lvolID string) error

// GrowVDO extends the VG's pool LV to consume all newly available physical space on
// devicePath (matching the 100%FREE convention used at creation time), then grows the VDO
// logical volume's own size to match, and returns its device path. Takes devicePath rather
// than a target size -- see Section 9 for why this differs from an earlier draft's signature.
func GrowVDO(ctx context.Context, devicePath, lvolID string) (string, error)
```

**HA-mode device identity.** A volume's two redundant NVMe-oF HA paths each
surface as their own local `/dev/nvmeXnY` device node while presenting
byte-identical backend content. LVM's default, system-wide device scan
cannot tell these apart: `pvscan --cache <path>` hits a "duplicate PV"
ambiguity between a volume's two HA device nodes, and even once commands are
pointed at one specific path, a name-based `vgs <name>` existence check still
reports a VG as present when it was never actually created on that device
(this host restricts default LVM visibility via an
`/etc/lvm/devices/system.devices` devices file, so a name-only lookup does
not reliably tie its answer back to the intended device). Handled in three
parts:

1. Every LVM command in `CreateOrAttachVDO`/`ResolveClonedVDO`/`GrowVDO` is
   scoped to exactly one device via LVM's `--devices` flag, bypassing the
   system-wide scan entirely.
2. `vgExists` is content-based (`pvVGName(devicePath) == vg`), not a
   name-based `vgs --devices <path> <name>` lookup, using the same
   content-scanning identity check `ResolveClonedVDO` uses for clone
   detection.
3. A `vgHasLV` check distinguishes a fully created VDO stack from an
   **orphaned VG** left behind by an interrupted create (`pvcreate`/
   `vgcreate` completed, `lvcreate` did not): such a VG reports zero LVs, and
   `vgchange -ay` against it "succeeds" while producing no mountable device
   at all. `CreateOrAttachVDO` removes an orphaned VG and falls through to a
   fresh create rather than reactivating it forever.

See the test plan's Error-case coverage for verification.

Compression and deduplication have different cost profiles: deduplication
carries a measurably larger, fixed per-volume memory cost than compression
does, independent of whichever combination of the two is enabled — an
accepted cost of client-side deduplication, not a design constraint.

### Clone and snapshot restore handling

A PVC clone or a VolumeSnapshot restore is a byte-level copy-on-write copy at
the storage layer, not a reformat, so a clone of a VDO-backed volume carries
the exact same on-disk LVM metadata as its source: the same PV UUID, VG UUID,
and VG name. LVM identifies a PV/VG by that on-disk UUID, not by which CSI
volume it logically belongs to, so without correction the clone is
indistinguishable from its source to LVM, and `CreateOrAttachVDO`'s own "does
a VG named `<this-volumeID>` exist?" check answers no (the VG on disk is
still named after the source), which would either destroy the cloned data via
a fresh `lvcreate`, or collide with the source once both devices are visible
on the same node.

`ResolveClonedVDO` fixes this before `CreateOrAttachVDO` ever runs: on every
stage, it checks the device's own on-disk identity (not a volume-context
flag, so it works regardless of whether the content-source fact survived into
that context), and if the VG it finds belongs to a different volume, it
regenerates fresh PV/VG UUIDs and renames the VG (`vgimportclone` +
`lvrename`) to this volume's own identity before any activation happens. It's
a no-op for a genuinely fresh device or one already resolved. Once renamed,
the device is indistinguishable from any other VDO volume, so every later
reattach, reconnect, or grow uses the same logic as any other volume: this
is a one-time disambiguation at first stage, not an ongoing difference.
Verified live for both a direct PVC clone and a snapshot restore, each
scheduled onto the same node as its still-live source. See the test plan.

`auto` (LVM's default, and what every example command in this doc uses, none
override it) picks `sync` or `async` for the write policy based on whether
the backing device reports a volatile write cache, and real simplyblock-backed
storage does. Measured against a real NVMe-oF-backed lvol, throughput was
statistically indistinguishable across `sync`/`async`/`auto`, so there's no
performance case for forcing a non-default policy. **Recommendation: leave
`vdo_write_policy` at `auto`**, which is also what production gets with zero
deliberate configuration.

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
  re-provision. It holds across a full node reboot too, with no ghost state
  in between: the VG is invisible immediately after boot, since its PV isn't
  present until NVMe-oF reconnects and `pvscan --cache` rediscovers it.
- `stageVolume` already calls `xfsStripeOptions` to align `mkfs.xfs` to the
  backend's erasure-coding stripe geometry (`xfs_su`/`xfs_sw`). This must be
  skipped whenever a VDO layer is in play (`client_compression` or
  `client_deduplication`) — VDO virtualizes and relocates blocks, so the
  filesystem is no longer directly on the erasure-coded device; applying
  stripe alignment hints computed for the raw device to a VDO virtual device
  is not just useless, it's actively misleading.

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

**Restart is a re-provision like any other.** Each VDO instance reattaches
independently on reboot, since kubelet calls `NodeStageVolume` fresh for
every volume after a restart rather than `restageVolume` (kubelet's own
bookkeeping resets on reboot too), and reattachment is already idempotent per
instance.

**Stale VDO/LVM state after a node loses a volume without a clean unstage**
(e.g., a pod force-rescheduled off a node that goes NotReady, or the storage
side disconnecting the initiator while the node itself stays up): the
correct teardown sequence depends on whether the node rebooted. When the
device disappears without a clean disconnect, `DeactivateVDO`'s `vgchange
-an` fails ("Volume group ... not found") because there is no device left to
read the VG's metadata from, and the fallback is a direct `dmsetup remove` of
the live dm nodes (see `RemoveOrphanedDMNodes` in Section 7) rather than the
normal LVM teardown path: it matches every dm node whose name starts with
the volume group's dash-escaped name and removes each with a plain `dmsetup
remove`, retrying up to three passes so a node still blocked by another
live dependency clears once that dependency is gone. The removal mechanism
itself is proven correct; what's unverified is whether it always runs at
all, since it only fires when `NodeUnstageVolume` is actually invoked on the
affected node, and whether kubelet's volume reconciler reliably re-invokes
`NodeUnstageVolume` on the *original* node after a NotReady/rejoin cycle,
once the pod has already been rescheduled elsewhere, is a different,
unverified code path (see the test plan). Separately, VDO fences itself into
read-only mode on the underlying I/O failure rather than corrupting
anything, and ext4 independently aborts its own journal for the same
reason, neither needing wiring from this design.

The node's `/etc/lvm/devices/system.devices` file, which restricts LVM's
default visibility to specific devices, is a separate concern this design
does not address at all: no code path prunes a stale entry after its
device disappears, unlike the dm-node cleanup above, which exists and only
needs reliably triggering. It doesn't affect correctness, since the file
only gates which devices LVM considers rather than causing failures on its
own, so this is a hygiene concern rather than a safety one, but it is
unbounded: a node that accumulates enough failure cycles over its lifetime
grows a stale entry per cycle with no automatic pruning anywhere.

**Crash-consistency under `async`** (the default write policy, see Section
7): an `fsync()`'d write survives a genuine, unclean node crash with an exact
checksum match, and a write without `fsync()` is correctly lost. `async`
honors flush/FUA durability end-to-end through NVMe-oF to the simplyblock
backend, so an acknowledged write is safe under `async`, not just under
`sync`.

Coverage status and the remaining open questions for this section (a
genuinely overlapping, not just closely timed, `vgchange`/`pvscan` race, a
boot-time activation race across several VGs, the clean-reboot teardown
variant, whether kubelet reliably re-invokes `NodeUnstageVolume` on the
original node after a NotReady/rejoin cycle, and `system.devices` bookkeeping
hygiene) are tracked in the test plan, not restated here.

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
behavior at realistic fleet density.

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
