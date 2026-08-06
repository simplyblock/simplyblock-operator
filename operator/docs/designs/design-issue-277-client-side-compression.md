# Design Document: Client-Side Compression (Issue #277)

Hands-on experiment transcript (exact commands and output run against the live
test cluster): [`spike-log-issue-277-client-side-compression.md`](spike-log-issue-277-client-side-compression.md).

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
    - [Compression vs. deduplication — isolated cost, verified hands-on](#compression-vs-deduplication--isolated-cost-verified-hands-on)
    - [Wiring into `nodeserver.go`](#wiring-into-nodeservergo)
  - [8. Performance Characteristics](#8-performance-characteristics)
  - [9. Re-Provisioning and Failure Handling](#9-re-provisioning-and-failure-handling)
  - [10. Volume Expansion](#10-volume-expansion)
  - [11. RBAC Changes](#11-rbac-changes)
  - [12. Testing Strategy](#12-testing-strategy)
  - [13. Compatibility with Existing CSI Driver / Operator Features](#13-compatibility-with-existing-csi-driver--operator-features)
    - [Confirmed compatible, no changes needed](#confirmed-compatible-no-changes-needed)
    - [Real gaps found — need design/code changes](#real-gaps-found--need-designcode-changes)
    - [Needs a one-line confirmation, not a design change](#needs-a-one-line-confirmation-not-a-design-change)
  - [14. Open Questions and Discussion](#14-open-questions-and-discussion)

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
  deduplication** via two new, independent, clearly-named parameters
  (`clientCompression`, `clientDeduplication`), separate from each other. 
  have also Verified hands-on (Section 7) that the two carry genuinely different 
  cost profiles — dedup's RAM cost dominates. where as compression's RAM cost is negligible.
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
                     ┌─────────────────────────────────────────┐
                     │   Pool CR (StorageClassParameters)       │
                     │   clientCompression: true                │
                     │   clientDeduplication: false (independent)│
                     └───────────────┬───────────────────────────┘
                                     │ mergeStorageClassParameters()
                                     ▼
                     ┌─────────────────────────────────────────┐
                     │   Generated StorageClass                 │
                     │   parameters.client_compression = "True"  │
                     │   parameters.client_deduplication = "False"│
                     │   allowedTopologies: vdo-capable=true      │ (new; either param true triggers this)
                     └───────────────┬───────────────────────────┘
                                     │ WaitForFirstConsumer (existing)
                                     ▼
                     ┌─────────────────────────────────────────┐
                     │   Kubernetes scheduler                    │
                     │   only picks nodes advertising            │
                     │   simplyblock.io/vdo-capable=true          │ (new label)
                     └───────────────┬───────────────────────────┘
                                     ▼
   ┌─────────────────────────────────────────────────────────────────────┐
   │  simplyblock-csi-node (per node)                                     │
   │                                                                       │
   │  postStart hook: dnf install kmod-kvdo vdo (idempotent) + modprobe   │ (new)
   │       │                                                              │
   │       ▼ marker file                                                  │
   │  nodeServer startup: read marker → patch Node label vdo-capable      │ (new)
   │       │                                                              │
   │       ▼ (reused by NodeGetInfo → buildAccessibleTopology)            │
   │                                                                       │
   │  NodeStageVolume: initiator.Connect() → devicePath                   │ (existing)
   │       │                                                              │
   │       ▼ if client_compression=="true" OR client_deduplication=="true"│
   │  CreateOrAttachVDO(devicePath, compression, dedup) → vdoDevicePath   │ (new)
   │       │                                                              │
   │       ▼                                                              │
   │  stageVolume(vdoDevicePath, ...) → format/mount                      │ (existing, now VDO-backed)
   └─────────────────────────────────────────────────────────────────────┘
```

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

- `operator/api/v1alpha1/pool_types.go`, `StorageClassParameters` (line 67): add

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
- `simplyblockpool_controller.go` `mergeStorageClassParameters` (line 403): add

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

VDO management can also be done with `vdo` cli. But this was not available on RHEL machines. So 
This design uses the LVM-integrated path, not because it's preferred in the abstract, but because it's the only
management path that actually exists on this OS. One VG/pool/LV set per
volume keeps the "one VDO instance per volume" intent from Section 2.

**Verified end-to-end on `vm04`, against a real simplyblock lvol over real
NVMe-oF**  — connected via the same two-path multipath
`nvme connect` the CSI driver already issues, resolved the stable
`/dev/disk/by-id/nvme-uuid.<lvol-uuid>` path exactly as `initiator.go`'s
`waitForDeviceReady` would, then built VDO directly on top:

```bash
DEV=/dev/disk/by-id/nvme-uuid.b7d433b7-fd53-4f73-b3d6-726c224b30e5
VOLID=b7d433b7-fd53-4f73-b3d6-726c224b30e5
pvcreate $DEV
vgcreate vdo-$VOLID $DEV
lvcreate --type vdo --config "activation{checks=0}" -n $VOLID -l 100%FREE vdo-$VOLID/vdopool --yes
mkfs.ext4 -q /dev/vdo-$VOLID/$VOLID && mount /dev/vdo-$VOLID/$VOLID /mnt/real-vdo

### testing
echo real-simplyblock-cluster-canary > /mnt/real-vdo/canary.txt
sha256sum /mnt/real-vdo/canary.txt
vdostats --human-readable   # dm device name is vdo-<VOLID>-vdopool-vpool, not the LV name
lvremove -f vdo-$VOLID && vgremove -f vdo-$VOLID && pvremove $DEV
```

This produced a real, mountable, working deduped/compressed filesystem on an
actual connected NVMe-oF volume — the entire loop-device recipe carried over
with zero changes.

```go
// CreateOrAttachVDO idempotently ensures a VDO-backed LV exists on top of
// devicePath (its own PV/VG/vdo-pool/LV stack, named after volumeID) and
// returns the resulting /dev/mapper/... path. Checks whether the VG already
// exists first (`vgs <name>`); if so, reactivates it (`vgchange -ay`,
// `lvchange -ay`) rather than recreating; only runs pvcreate/vgcreate/lvcreate
// when the VG is genuinely absent. compression/deduplication are independent
// (verified: `lvcreate --type vdo --compression y|n --deduplication y|n`) --
// pass both through from the volume context's two separate parameters.
func CreateOrAttachVDO(devicePath, volumeID string, compression, deduplication bool) (vdoDevicePath string, err error)

// SetVDOFeatures live-toggles compression/deduplication on an existing VDO
// volume without recreating it (`lvchange --compression y|n --deduplication
// y|n VG/vdopool` -- verified hands-on, works on an active, mounted volume).
// This is what would back a future StorageClass/VolumeAttributesClass update
// to flip either feature on an already-provisioned volume; out of scope for
// v1 but the mechanism is confirmed to exist if wanted later.
func SetVDOFeatures(volumeID string, compression, deduplication bool) error

// RemoveVDO deactivates and removes the VG/LV stack for volumeID.
func RemoveVDO(volumeID string) error

// ResolveClonedVDO handles the case where devicePath is a block-level CoW
// clone/snapshot-restore of another VDO-formatted volume: detects a
// byte-duplicate PV/VG UUID against an already-known volume (not simply "a
// VG exists"), regenerates fresh PV/VG UUIDs and renames the VG to volumeID
// (the vgimportclone equivalent), before any activation is attempted. Must
// run BEFORE CreateOrAttachVDO's own vgs/lvs check, since a byte-identical
// clone will otherwise look like "this volume's VG already exists" under the
// wrong name/UUID. See Section 13 — this is a confirmed real gap, not
// speculative, and has no existing filesystem-level workaround the way XFS's
// `nouuid` mount flag does for FS UUID collisions.
func ResolveClonedVDO(devicePath, volumeID string) error

// GrowVDO extends the vdo-pool LV's physical size (lvextend) and the vdo LV's
// logical size (lvextend) to newSize. NOT YET VERIFIED HANDS-ON — see caveat
// below.
func GrowVDO(volumeID string, newSize int64) error
```

### Compression vs. deduplication — isolated cost, verified hands-on

Compression and deduplication were isolated on `vm04` by creating four VDO
instances, each with an explicit `--compression y|n --deduplication y|n`
combination, against the same ~1GB test dataset (500MB of real, repeated
journal-log text plus an exact duplicate copy of it, giving both internal
compressibility and cross-block duplication to exercise both mechanisms):

| Compression | Dedup | Data blocks used (physical) | `KVDO module bytes used` (RAM) |
|---|---|---|---|
| Y | N | 74,017 (~289MB) | **182MB** |
| N | Y | 116,733 (~456MB) | **390MB** |
| Y | Y | 34,014 (~133MB) | **390MB** |
| N | N | 254,299 (~993MB) | **182MB** |

**Conclusion**: RAM cost tracks **deduplication only** — 182MB whether
compression is on or off, jumping to 390MB whenever dedup is on, regardless
of compression state. Compression adds no measurable memory overhead in
either direction (it's a pure CPU cost, no persistent index structure,
consistent with LZ4 being a lightweight streaming codec). Best space savings
require both together (133MB vs. 289MB compression-only vs. 456MB dedup-only
for the same data) — compression and dedup are complementary, not redundant,
when both apply.

**Re-verified against the real simplyblock cluster, not just loop devices**:
created a real lvol, real NVMe-oF connect, `lvcreate --type vdo --compression
y --deduplication n` (measured 182MB), then `lvchange --deduplication y
vdo-.../vdopool` **live, without recreating the volume** (measured ~391MB
afterward, matching the loop-device figure almost exactly) — confirms both
the independent-flags mechanism and the live-toggle mechanism work
identically on real hardware.

Notes:

- **Stable device identifier**: same as before — use `/dev/disk/by-id/...`,
  not the raw enumerated `/dev/nvmeXnY` node, as the PV's backing device.
- **Reattach mechanism — now verified hands-on** (`vm04`, scratch loop device):
  since there's no standalone `vdo` CLI, reattaching an existing VDO volume
  after an NVMe-oF reconnect (Section 9) means re-activating the LVM stack.
  Simulated total device loss (unmount, `vgchange -an`, remove the loop device
  entirely — confirmed via `losetup -a` returning empty) then a "reconnect"
  (new loop device from the same backing file, deliberately exercising the
  case where the device node's identity changes, matching real NVMe-oF ANA
  reconnect behavior): `pvscan --cache` correctly rediscovered the PV
  regardless of device path, `vgchange -ay` reactivated the VG/LV with **zero
  recreate/reformat**, and a canary file's SHA-256 matched exactly before and
  after. This confirms the mechanism Section 9 relies on. **Also verified
  across a real full node reboot** (not just a simulated device
  disconnect/reconnect): the same VG was invisible immediately after boot (no
  ghost state, since its PV wasn't present at boot time), and the identical
  `pvscan --cache`/`vgchange -ay` sequence reattached it cleanly afterward
  with no data loss — see the spike log §12.
- **Why `ResolveClonedVDO` has to run *before* `CreateOrAttachVDO`'s own
  check, not alongside it**: the bug here isn't about *whether* a clone needs
  VDO applied — it obviously does, since the compression container is
  physically part of the bytes that got copied, not something the driver
  chooses. The bug is that **LVM identifies a PV/VG by a UUID stored in its
  on-disk metadata, not by which CSI volume it logically belongs to**. A
  byte-level clone copies that metadata verbatim, so the clone's device
  claims to be *the same PV UUID, VG UUID, and VG name (the source's
  volumeID)* as the original. `CreateOrAttachVDO`'s check ("does a VG named
  `<this-volumeID>` exist?") will answer **no** for a fresh clone — the VG on
  disk is still named after the *source's* volumeID — which looks exactly
  like "genuinely blank device, safe to `lvcreate`." Two ways that goes
  wrong: `lvcreate` on a device that already has a VDO container destroys the
  cloned data; alternatively, blindly activating the device under its
  inherited name/UUID risks a live LVM collision the moment the source
  volume's own device is also visible on the same node (both scheduled
  there, or a stale device-cache entry). Neither is safe, and neither looks
  like an error until it collides.

  **Detection, concretely**: the CSI controller already knows when a volume
  is a clone/snapshot-restore (`VolumeContentSource` on the `CreateVolume`
  request) — thread that fact into the volume context so `NodeStageVolume`
  has it explicitly, rather than trying to infer it purely from probing the
  device. Combine with a device-level check (`blkid`/`pvs` on `devicePath`
  shows a valid LVM/VDO signature, but the VG name it reports is *not*
  `volumeID`) as a defensive double-check for cases where content-source
  metadata didn't make it through. Either signal alone is enough to know
  "this device carries an inherited identity, not its own."

  **Ordering**: `ResolveClonedVDO` must run and complete — regenerate fresh
  PV/VG UUIDs, rename the VG to `volumeID` — *before* `CreateOrAttachVDO`'s
  vgs/lvs check ever runs. Once the rename/UUID-regen is done, the device is
  indistinguishable from a completely normal, uniquely-identified VDO volume
  belonging to this volumeID, and every subsequent reattach/reconnect/grow
  for it goes through the exact same already-verified logic as any other
  volume (Sections 9-10) with no further special-casing. This is a one-time
  disambiguation on first stage, not an ongoing behavioral difference.
- **Multiple concurrent instances — verified, memory scales exactly linearly**:
  2 simultaneous VDO-backed LVs on the same node showed no naming/dm
  collisions and stayed fully data-isolated. `KVDO module bytes used` is a
  module-wide total, not per-instance (both instances reported the identical
  figure) — with 1 instance it was `408960848` bytes; with 2 it was
  `817919568`, within 0.0003% of exactly double. **No shared-memory benefit
  across instances — the ~390MB/instance RAM cost multiplies directly by
  volume count.** This test used the default (both compression and dedup on)
  configuration — the isolated-cost table above confirms that ~390MB is
  attributable to deduplication specifically, so this linear-scaling risk is
  really a **deduplication-scaling** risk, not a compression one; a fleet of
  compression-only volumes would multiply the much smaller ~182MB figure
  instead. See spike log §9.
- **Sizing — hard minimum confirmed, much larger than earlier estimated**: a
  real `lvcreate --type vdo` attempt at 1.9GB failed outright:
  `Minimum required size for VDO volume: 5063921664 bytes` (~4.72GiB). This is
  not a tunable slab-size nuance as an earlier draft of this doc assumed — it
  is a hard floor. **Any PVC smaller than ~4.72GiB physical cannot use client-side
  compression via VDO at all**, and the platform's minimum supported PVC size
  needs to be checked against this before committing to "VDO always available
  when requested."
- **Idempotency semantics**: `lvcreate --type vdo -n X ...` fails loudly if `X`
  already exists — same check-then-act principle as before, just against LVM
  objects (`vgs`/`lvs`) instead of a nonexistent `vdo status`.

### Wiring into `nodeserver.go`

- `NodeStageVolume` (line 248-346): when **either**
  `vc["client_compression"] == "true"` **or**
  `vc["client_deduplication"] == "true"`, call `CreateOrAttachVDO` (passing
  both booleans through, independently — see Section 6/7) between
  `initiator.Connect` (line 322) and `ns.stageVolume` (line 332); pass the
  returned VDO device path into `stageVolume` instead of the raw NVMe-oF path.
  Stash this in the volume context (already persisted via
  `util.StashVolumeContext`) so Unstage/restage paths know VDO is in play.
  **If the volume was created from a `VolumeContentSource` (clone/snapshot
  restore)**, call `ResolveClonedVDO` first — see Section 13, this is a
  required step, not optional hardening.
- `NodeUnstageVolume` (line 348-387): if the volume context indicates VDO was
  used, call `RemoveVDO` **before** `initiator.Disconnect` (line 377) — VDO must
  come down before the device underneath it is disconnected.
- `restageVolume` (line 798) and `ensureDeviceConnected` (line 758): after
  `initiator.Connect` re-establishes the raw device, re-**attach** (LVM
  reactivate: `pvscan --cache` / `vgchange -ay`, never `lvcreate`) the existing
  VDO device before remounting — mirroring `restageVolume`'s existing "never
  reformat, data already exists" invariant. This is the concrete mechanism
  satisfying the issue's requirement that the compression device be
  re-included on every re-provision. **Now verified hands-on** — see Section 7.
- `stageVolume` (line 577) already calls `xfsStripeOptions` to align `mkfs.xfs`
  to the backend's erasure-coding stripe geometry (`xfs_su`/`xfs_sw`). **This
  must be skipped whenever a VDO layer is in play** (`client_compression` or
  `client_deduplication`) — VDO virtualizes and relocates blocks, so the filesystem is no longer directly on
  the erasure-coded device; applying stripe alignment hints computed for the
  raw device to a VDO virtual device is not just useless, it's actively
  misleading. See Section 13.

---

## 8. Performance Characteristics

**This is currently the single biggest open risk in this design and was
previously completely unmeasured.** Every prior spike checked correctness
(does it work, does data survive) — none checked cost. A `vm04` spike using
`fio` (direct I/O, `iodepth=16`, against the raw block device to isolate
VDO's own overhead from filesystem noise) found:

| Test | Raw device | VDO-backed | Ratio |
|---|---|---|---|
| Sequential write, incompressible data | 1076 MiB/s | 99.6 MiB/s | **~10.8x slower** |
| Sequential write, realistic ~50% compressible data | 1076 MiB/s | 135 MiB/s | **~8x slower** |
| Sequential read | 2538 MiB/s | 957 MiB/s | ~2.65x slower |

A naive first attempt at a "compressible data" test used all-zero buffers and
showed VDO matching raw performance (1045 MiB/s) — this was **misleading**:
all-zero blocks hit VDO's dedicated zero-block fast path (not even written to
disk, just mapped), which real compressible data (logs, text, JSON — compress
well via LZ4 but aren't literally all-zero) does not get. The
`buffer_compress_percentage`-based test above is the representative number.

**Caveats that keep this a bound, not a production verdict**:
- The test node had 12 vCPUs, so the write penalty isn't simple CPU
  starvation.
- `lvs -o+vdo_write_policy` showed `auto`, which likely resolves to `sync` for
  a loop device (no volatile write-cache reporting) — real NVMe-oF-backed
  storage may let VDO run `async` and close much of this gap.
- This was a loop-device-in-a-VM environment, not real NVMe hardware.

**This must be re-measured against real NVMe-oF-backed storage before this
feature ships or is enabled by default.** An 8-11x write throughput
regression, even if it turns out to be partly a test-environment artifact, is
the kind of number that should gate whether client-side compression is
opt-in-per-workload rather than something casually recommended — this is a
performance-oriented NVMe storage platform, and silently trading an order of
magnitude of write throughput for compression is a decision customers need to
make deliberately, not discover after the fact. See the spike log §10 for the
full command transcript.

---

## 9. Re-Provisioning and Failure Handling

Topology gating (Section 5) only controls *initial* PV scheduling. It does not
protect against a node's capability regressing after a volume is already bound
there (e.g. an OS update removes kvdo compatibility) — Kubernetes will not evict
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

## 10. Volume Expansion

`NodeExpandVolume` (nodeserver.go:479) currently resizes the filesystem directly
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
newly-available space confirmed it's genuinely usable, not just reported. As
a side effect, `Data%` on the pool dropped from 63.68% to 46.71% after growth —
consistent with Section 7's finding that VDO's overhead is roughly fixed in
absolute terms, so it becomes a smaller fraction of larger pools. Not yet
tested: growth behavior specifically *at* the ~4.72GiB minimum-size floor
(this spike started from an already-above-floor 5.5G pool).

---

## 11. RBAC Changes

`helm-charts/charts/simplyblock-operator/templates/node-rbac.yaml:19-21`
currently grants the CSI node's ClusterRole only `get`/`list`/`watch` on
`nodes`. Add `patch`/`update` so the capability-labeling step in Section 4 can
write the `simplyblock.io/vdo-capable` label.

---

## 12. Testing Strategy

- Unit tests for `vdo.go`: idempotent create/attach/remove/grow, small-device
  slab-size edge cases, stable-identifier resolution — mirroring existing
  `initiator_test.go` patterns.
- Unit tests for the `NodeStageVolume`/`NodeUnstageVolume`/`restageVolume`/
  `ensureDeviceConnected`/`NodeExpandVolume` wiring with a mocked VDO layer,
  matching existing `nodeserver_test.go` patterns.
- Unit tests for `buildAccessibleTopology`'s new label surfacing and
  `upsertStorageClass`'s new `AllowedTopologies` branch.
- **Update — no longer fully blocked**: `vm04` was upgraded (with explicit
  sign-off) to `5.14.0-687.33.1.el9_8` and rebooted; `kvdo` now loads there,
  and a real VDO-backed LV was created, formatted, mounted, written to, and
  measured (Section 7) — the mechanism itself is proven end-to-end on real
  hardware. What's still blocked is the *CSI-integrated* e2e (a real
  `client_compression=true` PVC scheduled through the actual StorageClass →
  topology gate → `NodeStageVolume` path), since that code doesn't exist yet —
  this is now a "write the code" blocker, not an "no compatible kernel exists"
  blocker. `vm04` remains on the new kernel with `kvdo` loaded (left
  intentionally, not reverted) for further hands-on testing before/during
  implementation.
- Until the CSI-integrated code exists, the auto-install/label/topology-gate
  path can still be validated in isolation (label correctly ends up
  `false`/absent on incapable nodes, PVC scheduling correctly avoids them)
  without exercising the actual VDO create/mount code path.

---

## 13. Compatibility with Existing CSI Driver / Operator Features

A full inventory of this repo's CSI RPCs, StorageClass parameters, CRDs, and
named subsystems was cross-checked against this design. Verdicts:

### Confirmed compatible, no changes needed
- **`CreateVolume`/`DeleteVolume`/`ControllerGetVolume`/`ValidateVolumeCapabilities`**
  — server-side lvol lifecycle, unaffected; VDO is purely a node-side addition.
- **`ControllerExpandVolume`** — feeds `NodeExpandVolume`'s existing size info;
  Section 10's `lvextend` chain consumes it the same way the existing resize
  logic does.
- **Guardian** (`csi-driver/pkg/util/guardian.go`) — **verified hands-on via
  research, not assumed**: `MarkBrokenLvol` is pure Kubernetes-level
  bookkeeping (marks state, later deletes the pod to force a fresh
  Stage/Publish cycle) and never touches the device/mount/dm layer itself.
  The actual device-level repair happens in `restageVolume`/
  `ensureDeviceConnected`, which this design already updates (Section 9) to
  reactivate the VDO/LVM stack. Guardian needs zero VDO-awareness.
- **VolumeMigration** — **verified hands-on against a real simplyblock cluster**
  (real lvol, real `sbctl lvol migrate --batch`, real `nvme connect`), not just
  research this time. Confirmed: the volume's identity never changes — the
  migration's new connect strings reuse the **exact same NQN**, and the new
  target paths simply show up as additional live legs on the same
  `nvme-subsys0` alongside the original paths (`nvme list-subsys` showed 4
  live paths — 2 original + 2 migration — under one unchanged NQN). No bytes
  visible to the client change identity; this is the identical mechanism
  already validated in the reconnect/reboot spikes (Sections 7/9), so no new
  VDO-side logic is needed beyond what's already designed.
  **New finding, not anticipated by the original research**: a volume that is
  part of a shared-namespace subsystem (i.e. has clones — see the CoW clone
  finding above) **cannot be migrated individually** — `sbctl lvol migrate`
  without `--batch` was hard-rejected: *"LVol ... belongs to a shared
  NVMe-oF subsystem with 2 member(s) ... Use --batch to migrate the whole
  subsystem together."* The backend enforces atomic group migration itself.
  This doesn't require new client-side design work — after `ResolveClonedVDO`
  gives each clone its own independent PV/VG/LV identity (Section 7), each
  volume's reconnect logic already operates independently of any others
  sharing its original subsystem — but it's worth knowing the backend
  guarantees this atomicity rather than the client needing to coordinate it.
  **Not fully verified**: the actual data-copy (`snap_copy`) phase ran far
  longer than expected on the test cluster (contending with a concurrent
  `REBALANCING` operation) and hadn't reached cutover after ~10 minutes of
  observation. Confirmed stable: device identity and data integrity through
  the "both old and new paths live simultaneously" pre-cutover window.
  **Not observed**: the actual final cutover moment (old paths going
  inaccessible, new paths becoming primary) — left running in the background,
  not confirmed to complete cleanly end-to-end.
- **Encryption — verified compatible, not just assumed orthogonal** (see the
  detailed spike writeup below this list): the NVMe-oF client always receives
  plaintext for an "encrypted" volume, since the crypto bdev sits below the
  nvmf attach point server-side. `encryption=true` + `client_compression=true`
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
`lvol.top_bdev` is reassigned to point at it (line 671); the nvmf
namespace-add RPC always exposes `lvol.top_bdev` (lines 1180-1181,
1299-1301) — the crypto bdev sits **below** the nvmf attach point, so every
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
  `stagingMountFlags` adds `nouuid` for XFS specifically because "xfs refuses
  to mount two filesystems with the same uuid" on a volume and its
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
  volume (crypto bdev sits below the nvmf attach point, server-side-only key),
  so this combination is fine. See the detailed writeup above.
- **🟡 Server-side `compression=true` + `client_compression=true` is
  redundant, not harmful.** Compressing already-compressed data wastes CPU on
  both ends for negligible additional savings — a much milder version of the
  encryption issue. Same recommendation: worth a warning, lower priority than
  the encryption case. `client_deduplication` has no server-side counterpart
  to worry about here — the existing server-side feature is compression-only.

### Needs a one-line confirmation, not a design change
- **Placement webhook / volume placement injector** — if this webhook
  co-locates a workload pod with its backing storage node for network
  locality, and the storage node isn't also `vdo-capable`, its placement hint
  could conflict with the topology gate from Section 5. Likely a narrow edge
  case; worth confirming the two constraints compose (AND) rather than
  conflict when both apply to the same StorageClass.
- **`AllowedTopologies` composition with DHCHAP's existing `allowedNodes`
  gate**: `upsertStorageClass` already sets `AllowedTopologies` conditionally
  for DHCHAP; when both DHCHAP-gating and `client_compression`-gating apply
  to the same pool, confirm the generated `TopologySelectorTerm` entries
  compose as AND (Kubernetes semantics say they should, since
  `matchLabelExpressions` is a list within one term) rather than one
  silently overwriting the other in `simplyblockpool_controller.go`.

---

## 14. Open Questions and Discussion

- **Unresolved policy question: what exactly qualifies a volume for
  deduplication?** Per team discussion, deduplication is meant to be
  restricted to "specific volumes" rather than freely available anywhere
  compression is — a deliberate choice given the measured, fixed ~390MB/volume
  RAM cost (Section 7) that compression alone doesn't carry. But the concrete
  rule hasn't been decided. Candidates, each implying different plumbing:
  - **Pool/StorageClass-level allowlist** — an admin explicitly enables
    `clientDeduplication` on specific pools (mirrors the existing
    `DHCHAP`/`AllowedNodes` pattern already on `Pool`).
  - **Workload-type label/annotation** — e.g. only PVCs labeled as VM-image or
    backup-target storage get dedup, auto-applied rather than admin-toggled
    per pool.
  - **Size threshold** — dedup only below/above some volume size, since the
    fixed per-instance index cost matters proportionally more for small
    volumes.
  This needs to be settled with the team before `ClientDeduplication`'s exact
  validation/gating behavior (Section 6) can be finalized — right now the
  field is a plain independent boolean with no restriction beyond the
  topology capability gate, which may not be what's wanted.
- **🔴 `upsertStorageClass` is create-only, not create-or-update — enabling
  compression/dedup on an existing Pool silently does nothing.** Found reading
  the actual code (`operator/internal/controller/simplyblockpool_controller.go:352-397`),
  not speculation:

  ```go
  if err := r.Create(ctx, sc); err != nil && !apierrors.IsAlreadyExists(err) {
      return err
  }
  return nil
  ```

  Despite the name, this never updates an already-existing StorageClass — a
  `Create` against a name that already exists returns `AlreadyExists`, which
  is caught and swallowed, and the function returns success having changed
  nothing. This is pre-existing behavior (likely because Kubernetes
  StorageClasses are largely immutable via the API — `parameters` and
  `allowedTopologies` can't be patched on an existing one), not something this
  design introduces — but it directly undermines the new parameters:

  1. An admin has an existing `Pool` with an already-created StorageClass
     (no `client_compression`/`client_deduplication`, no `vdo-capable`
     topology requirement).
  2. They edit the Pool CR, setting `storageClassParameters.clientDeduplication: true`.
  3. The reconciler builds the *correct* new `params`/`AllowedTopologies` and
     calls `upsertStorageClass` — which hits `AlreadyExists` and silently
     no-ops.
  4. **Nothing happens.** The existing StorageClass keeps serving plain,
     non-VDO volumes forever. No error, no Event, no status condition —
     zero signal that the change had no effect.

  This needs a real resolution before shipping, not just documentation of the
  limitation: either (a) the reconciler detects a mismatch between the Pool
  spec's intended parameters and the existing StorageClass's actual
  `Parameters`/`AllowedTopologies` and surfaces it loudly (a status condition
  or Kubernetes Event telling the admin their change was ignored and a new
  Pool/StorageClass is required), or (b) accept the limitation but document it
  prominently at the `ClientCompression`/`ClientDeduplication` field level
  (e.g. in the CRD's kubebuilder doc comment) so it's visible wherever someone
  reads the API, not buried in a design doc.
- ~~Standalone `vdo` CLI longevity~~ **Resolved (verified hands-on)**: it's
  already gone, not just uncertain — the installed `vdo` 8.2.2.2 package on
  this cluster ships no `vdo` binary at all, only `vdoformat`/`vdostats`/etc.
  LVM-integrated VDO (`lvcreate --type vdo`) is the only management path that
  exists; Section 7 is now written against that reality.
- **Airgapped install**: the auto-install approach (explicitly chosen for
  simplicity) has a hard dependency on BaseOS repo network access from every
  node. Customers without that access will need a documented golden-image
  prerequisite instead — not solved in this iteration.
- **Kernel/distro coverage beyond RHEL-family**: this design assumes a
  RHEL-compatible distro with `kmod-kvdo`/`vdo` packages available. Non-RHEL
  k8s node OSes (e.g. Ubuntu-based managed node images) were not evaluated and
  may not have an equivalent path at all — worth scoping which distros this
  feature needs to support before wide rollout.
- **Memory/CPU cost per VDO instance — now measured, not estimated, and worse
  than the earlier documentation-sourced guess**: a real VDO-backed LV at the
  minimum viable size (5.5GiB physical pool, `vm04`) reported, via
  `vdostats --verbose`, **`KVDO module bytes used: 408960848`** (~390MB of
  kernel RAM) for that single instance, and **918272 overhead blocks
  (~3.5GiB, 64% of the pool) consumed by VDO's own bookkeeping before a single
  byte of user data was written** — only 642 data blocks (~2.6MB) held actual
  post-dedup/compression data from 600MB of highly-duplicate/compressible test
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
- **VDO is now upstream/in-tree as of kernel 6.9**: per the `dm-vdo/kvdo`
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
  reproduce that state to verify it end-to-end. See spike log §11.
