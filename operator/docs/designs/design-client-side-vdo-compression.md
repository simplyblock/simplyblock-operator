# Design Document: Client-Side Compression and Deduplication via VDO

**Status:** Implemented (PR [#402](https://github.com/simplyblock/simplyblock-operator/pull/402), pending review)  
**Author:** Manohar Reddy  
**Date:** 2026-08-28  
**Issue:** [#277](https://github.com/simplyblock/simplyblock-operator/issues/277)  
**Depends on:** [`atlas-lib/lvm`](https://github.com/simplyblock/simplyblock-operator/pull/457) (merged)  
**Test Plan:** [`tests/test-plan-client-side-vdo-compression.md`](../tests/test-plan-client-side-vdo-compression.md)

---

## Overview

**What this is.** A `StoragePool` opt-in that gives every volume in it client-side
compression and deduplication, independently switchable. The mechanism is VDO
(`dm-vdo`): the CSI node plugin assembles an LVM stack on top of the volume's raw
NVMe-oF device, one physical volume, one volume group, one VDO pool, and one VDO
logical volume, and mounts the VDO logical volume in place of the raw device. VDO
does the actual compression and deduplication, in the kernel, on the client node,
before a write ever leaves the host.

**Why client-side.** The storage backend already offers server-side compression
per pool. Client-side compression trades that off differently: it spends CPU and
RAM on the node running the pod instead of on the storage node, and it can
deduplicate across writes from the same client in ways the backend cannot see.
Neither replaces the other. A pool picks one, the other, both, or neither.

**The two layers this is built on.** [`atlas-lib/lvm`](https://github.com/simplyblock/simplyblock-operator/pull/457)
is a general-purpose, typed wrapper around Linux LVM commands: `PhysicalVolume`,
`VolumeGroup`, and `LogicalVolume` as distinct value types, and a `Manager` whose
methods assemble, activate, deactivate, grow, and inspect an LVM stack, scoping
each command to a device only where LVM's own identity resolution needs it (§3).
[`atlas-lib/lvm/vdo`](#3-atlas-liblvmvdo-the-stack-lifecycle) is the VDO-specific
layer on top: it owns the one-volume-group-per-lvol naming convention, the
create-or-reactivate idempotence a CSI `NodeStageVolume` retry needs, and the
fallback path for a backing device that has gone unreachable without a clean
unstage. Neither layer knows anything about Kubernetes or CSI. `csi-driver/pkg/util/vdo.go`
(§5) is eighty-five lines of wiring between them and the node plugin's RPC
handlers.

**A node either has VDO or it does not, and that has to be known before a pod
lands there.** `kmod-kvdo` is not available for every kernel this product
supports, so a pool with either client-side flag set carries a topology
requirement, and a node advertises whether it actually has a working `kvdo`
module before the scheduler is allowed to place a pod that needs one (§4).

**What is out of scope.** Live-toggling compression or deduplication on a volume
that already exists (the mechanism is built, `SetFeatures`, but nothing calls it,
§8). Server-side and client-side compression composing or conflicting on the same
pool (§8). Non-x86_64 hosts (`vdo` has no `aarch64` build in the repositories
this product installs from, §6).

---

## Table of Contents

1. [Goals and Non-Goals](#1-goals-and-non-goals)
2. [API Design: StoragePool Parameters](#2-api-design-storagepool-parameters)
3. [`atlas-lib/lvm/vdo`: the Stack Lifecycle](#3-atlas-liblvmvdo-the-stack-lifecycle)
4. [Node Capability Gating](#4-node-capability-gating)
5. [CSI Driver Wiring](#5-csi-driver-wiring)
6. [Deployment](#6-deployment)
7. [Failure Modes Found and Fixed](#7-failure-modes-found-and-fixed)
8. [Open Questions](#8-open-questions)
9. [Testing Strategy](#9-testing-strategy)

---

## 1. Goals and Non-Goals

### Goals

- Compression and deduplication, each independently switchable per `StoragePool`,
  applied on the client before a write reaches the network.
- No change to a volume's identity: the CSI volume handle, the raw NVMe-oF
  connection, and everything the control plane knows about the lvol stay exactly
  as they are for a volume with neither flag set. VDO is a layer the node adds
  locally, not a different kind of volume.
- Idempotent, crash-safe node operations: a `NodeStageVolume` retried after a
  partial failure reactivates what already exists rather than recreating it, and
  a pod delete-and-recreate on the same node never destroys the volume's data.
- Correct behavior for the entire volume lifecycle a plain volume already
  supports: create, stage, expand, clone, snapshot restore, and reconnect after
  the storage side disconnects while the node stays up.

### Non-Goals

- **A generalized, pluggable node-side stack abstraction** (layers for the
  fabric, LVM, and filesystem stages, composed and persisted as a stack record)
  was proposed as a superset of this mechanism. It is not built. This document
  describes the concrete, VDO-specific path the CSI driver actually runs. A
  future feature that needs the same kind of node-side assembly (a striped
  volume group across several members, for instance) is free to generalize
  `atlas-lib/lvm` further, but nothing here commits to a particular shape for
  that ahead of a second real consumer.
- **Live toggling.** Changing `clientCompression`/`clientDeduplication` on a
  `StoragePool` whose `StorageClass` already exists has no effect (§2). Doing so
  for an individual already-provisioned volume is not wired into any code path
  (§8).
- **Server-side and client-side compression interacting.** Both parameters can
  be set on the same pool today. Whether that combination is meaningful, wasteful,
  or should be rejected is unresolved (§8).
- **Non-x86_64 nodes and non-RHEL-family distributions.** VDO's node-capability
  installer shells out to `dnf` (§4). A node whose package manager is not `dnf`,
  or whose architecture has no `vdo` build, never becomes VDO-capable (§6).

---

## 2. API Design: StoragePool Parameters

Two new fields on `StorageClassParameters` (`operator/api/v1alpha1/storagepool_types.go`),
next to the existing server-side `Compression`:

```go
// Compression enables compression for logical volumes.
// +kubebuilder:default="False"
Compression string `json:"compression,omitempty"`

// ClientCompression enables client-side (VDO) compression for logical volumes in this
// pool. Distinct from Compression (server-side). Independent of ClientDeduplication --
// either, both, or neither may be set. Changing this on a Pool whose StorageClass
// already exists has no effect (see issue #401) -- it only takes effect for pools
// whose StorageClass does not exist yet.
// +kubebuilder:default=false
ClientCompression *bool `json:"clientCompression,omitempty"`

// ClientDeduplication enables client-side (VDO) deduplication for logical volumes in
// this pool. Carries a significant, measured, fixed RAM cost per volume independent of
// ClientCompression -- intended to be opt-in on specific pools where duplicate data is
// actually expected, not enabled by default. Same StorageClass-immutability caveat as
// ClientCompression applies.
// +kubebuilder:default=false
ClientDeduplication *bool `json:"clientDeduplication,omitempty"`
```

`StorageClassParameters` as a whole is `+k8s:immutable`, because the Kubernetes
`StorageClass` fields it produces, `Parameters` and `AllowedTopologies`, are
themselves immutable once the object exists. `createStorageClassIfNotExists`
(`operator/internal/controller/simplyblockstoragepool_controller.go`) is create-only
for exactly this reason: there is no drift to reconcile, because the API server
would reject the update. A `StoragePool` whose flags change after its
`StorageClass` already exists keeps the old behavior until a new pool, and a new
`StorageClass`, is created.

`mergeStorageClassParameters` writes both fields into the `StorageClass`'s
`Parameters` map under the CSI driver's own key spelling:

```go
dst["client_compression"] = boolStr(p.ClientCompression)
dst["client_deduplication"] = boolStr(p.ClientDeduplication)
```

`boolStr` renders `"True"`/`"False"` (capitalized), matching every other boolean
`StorageClassParameters` field. The CSI driver reads these back with
`kube.BoolParam`, which accepts that capitalization alongside the lowercase form.

### Topology gating

`createStorageClassIfNotExists` adds a topology requirement whenever *either*
flag is true, not `client_compression` alone: a dedup-only volume still needs a
working `kvdo` module on the node just as much as a compression-only one does.

```go
if params["client_compression"] == scParamTrue || params["client_deduplication"] == scParamTrue {
    topologyExprs = append(topologyExprs, corev1.TopologySelectorLabelRequirement{
        Key:    "simplyblock.io/vdo-capable",
        Values: []string{"true"},
    })
}
```

This requirement is merged into the *same* `TopologySelectorTerm` as the existing
DHCHAP node-allow-list requirement, when both apply, rather than becoming a
second term. Kubernetes ANDs the expressions within one term and ORs separate
terms, and a pool that is both DHCHAP-restricted and VDO-only needs both
conditions to hold at once, not either one.

---

## 3. `atlas-lib/lvm/vdo`: the Stack Lifecycle

Every operation below is a function in `atlas-lib/lvm/vdo`, taking a `*lvm.Manager`
and an `lvolID` and returning a device path (create/grow) or an error. None of
them reference a Kubernetes or CSI type: they are node-level LVM/VDO
orchestration, moved out of the CSI driver into `atlas-lib` because nothing about
them is CSI-shaped (§5 explains what the CSI driver keeps instead). Each one is
built from the typed primitives `atlas-lib/lvm` provides:

```go
type PhysicalVolume struct{ DevicePath string }
type VolumeGroup struct{ Name string }
type LogicalVolume struct {
    VolumeGroup VolumeGroup
    Name        string
}
```

A device path, a volume group, and a logical volume are three distinct types
rather than three strings, so a volume group name accidentally passed where a
device path belongs is a compile error rather than an `lvcreate` failure
discovered against a real device. Every simplyblock volume's VDO stack lives in
its own volume group, named `vdo-<lvolID>`, containing exactly one VDO pool
(`vdopool`) and one VDO logical volume (`<lvolID>`).

### `CreateOrAttach`: idempotent create-or-reactivate

```go
func CreateOrAttach(
    ctx context.Context, manager *lvm.Manager, devicePath, lvolID string, compression, deduplication bool,
) (string, error)
```

Rescans the device, probes its on-disk volume-group identity, and branches three
ways:

1. **The volume group already exists and has the logical volume:** reactivate it
   (`vgchange -ay`) and return. Nothing is recreated. This is the path a retried
   `NodeStageVolume`, or a pod restaged after a routine reconnect, takes every
   time after the first.
2. **The volume group exists but the logical volume does not:** an earlier
   `pvcreate`/`vgcreate` completed and `lvcreate` did not, the signature of a
   create interrupted partway through. The empty volume group is removed and
   creation falls through to (3), because reactivating it forever would never
   produce a mountable device.
3. **Nothing exists on the device yet:** `pvcreate`, `vgcreate`, then
   `CreateLogicalVolume` with a `LogicalVolumeDefinition{Compression, Deduplication}`,
   which dispatches through `atlas-lib/lvm`'s `VolumeProvisioning` registry to
   contribute the `--type vdo --compression <y|n> --deduplication <y|n>` flags
   this package registers at `init()`.

### `ResolveClone`: a byte-level clone's foreign identity

```go
func ResolveClone(ctx context.Context, manager *lvm.Manager, devicePath, lvolID string) error
```

A CSI clone or snapshot restore is a byte-level copy at the storage layer, so the
new device's on-disk LVM signature is still the *source* volume's: the same
volume group name, the same PV and VG UUIDs. `CreateOrAttach`'s own "does
`vdo-<lvolID>` already exist" check cannot see this, because the volume group on
disk answers to the source's name, not this volume's. `ResolveClone` wraps
`atlas-lib/lvm`'s `ResolveClonedVolumeGroup` (rescan, probe, `vgimportclone` to
regenerate fresh UUIDs and rename the volume group, then rename the logical
volume inside), passing this stack's own `vdopool` name as the one logical volume
to leave alone. Driven from the device's actual on-disk identity rather than a
flag threaded from the CSI `VolumeContentSource`, so it is safe and cheap to call
on any freshly attached device, whether or not it turns out to be a clone. Must
run before `CreateOrAttach`.

### `Deactivate` and `Remove`: two different answers to the same failure

```go
func Deactivate(ctx context.Context, manager *lvm.Manager, lvolID string) error
func Remove(ctx context.Context, manager *lvm.Manager, lvolID string) error
```

`Deactivate` is the non-destructive counterpart to a plain NVMe-oF disconnect,
called from `NodeUnstageVolume` (§5): it deactivates the volume group
(`vgchange -an`) without destroying anything, because `NodeUnstageVolume` fires
whenever nothing on the node currently needs the volume mounted, including an
ordinary pod delete-and-recreate, not only when the volume is actually being
deleted. `Remove` is `Deactivate`'s destructive counterpart, called only when the
volume itself is being removed: it deactivates and then `vgremove -f`s the
volume group.

Both fall back to `RemoveOrphanedDMNodes` when the backing device has gone
unreachable without a clean unstage (crash, forced reschedule), because
`vgchange`/`vgremove` need to read and write volume-group metadata that lives on
the now-gone device and cannot do either. The two functions use **different
rules** for when that fallback fires, because they are trying to do opposite
things: `Deactivate` falls back only on the specific "volume group not found"
failure text, since anything else is a real problem worth surfacing rather than
papering over. `Remove` falls back on any failure at all, since the volume is
already being destroyed and there is no worse outcome to protect against.

### `Grow`

```go
func Grow(ctx context.Context, manager *lvm.Manager, devicePath, lvolID string) (string, error)
```

Grows the physical volume to the device's new full size, then the VDO pool to
consume the newly available space (`ExpandLogicalVolume`'s `-l+100%FREE`, which
is additive, not the absolute-size form `-l100%FREE` would be), then reads the
pool's new size back and grows the VDO logical volume to match it exactly
(`ExtendLogicalVolumeToSize`, an explicit byte count). The VDO logical volume
cannot be grown by the same free-space percentage the pool was, because only the
pool is a real, physical-extent-consuming volume group member. The VDO logical
volume on top is a virtual device sized in bytes, and after the pool's own
`100%FREE` grow there are no volume-group extents left for a second
percentage-based call to reference.

### `SetFeatures` and node-side logging

```go
func SetFeatures(ctx context.Context, manager *lvm.Manager, lvolID string, compression, deduplication bool) error
```

Toggles compression and deduplication on an already-active VDO volume without
recreating it (`lvchange`). Nothing calls it (§8).

This package has no Kubernetes dependency anywhere, so warnings (a best-effort
rescan that failed, an unreachable-device fallback firing) go through a
package-level `Logger *slog.Logger`, nil-safe and defaulting to `slog.Default()`,
rather than `klog`.

---

## 4. Node Capability Gating

`kmod-kvdo` is not available for every kernel this product runs on. A node
therefore has to prove it actually has a working `kvdo` module before the
scheduler is allowed to place a pod whose volume needs one, and that has to
happen without blocking every other node from becoming Ready while it does.

**Install, at DaemonSet start.** The CSI node DaemonSet runs with `hostPID: true`,
and its `csi-registrar` container's `postStart` hook `nsenter`s into the host PID
namespace to install `kmod-kvdo` and `vdo` via `dnf` if they are not already
present, then `modprobe kvdo`, and writes the result (`true`/`false`) to a marker
file on a host-path volume (`/var/lib/simplyblock/vdo-capable` on the host,
mounted at `/var/run/simplyblock/vdo-capable` in the container):

```sh
mkdir -p /var/run/simplyblock/vdo-capable;
if nsenter -t 1 -m -u -n -i -- sh -c 'rpm -q kmod-kvdo vdo >/dev/null 2>&1 || dnf install -y kmod-kvdo vdo' && modprobe kvdo; then
echo true > /var/run/simplyblock/vdo-capable/marker;
else
echo false > /var/run/simplyblock/vdo-capable/marker;
fi
```

**Advertise, from the running node plugin.** `newNodeServer` spawns
`advertiseVDOCapability` as a background goroutine, which polls the marker file
every five seconds for up to five minutes, then merge-patches the node's own
`simplyblock.io/vdo-capable` label to match. This runs independently of, and does
not block, `NodeStageVolume`/`NodeUnstageVolume`/CSI registration, which is what
lets the `dnf install` (seconds to minutes, and a hard failure on a kernel with no
`kvdo` build) happen without holding up every other volume operation on the node.

**An operator can override the label by hand.** Every automatic update writes a
`simplyblock.io/vdo-capable-managed-by: auto-detect` annotation alongside the
label. If the label is present without that annotation, `advertiseVDOCapability`
treats it as a deliberate human override and never touches it again.

**The topology key is always present, only its value changes.**
`buildAccessibleTopology` includes the `simplyblock.io/vdo-capable` key
unconditionally, never omitting it when the value is currently `false`. A CSI
node's topology key *set* is captured once, at plugin registration, moments
after the pod starts. The label itself is patched asynchronously afterward and
can take minutes. Omitting the key while the value is false would mean the key
is essentially never present at the moment registration actually happens, which
would defeat the topology gate entirely rather than merely delaying it.

**The scheduling pin, not only the storage class gate.** `AllowedTopologies` on
the `StorageClass` (§2) constrains where `WaitForFirstConsumer` binds a PVC, but
it says nothing about where a pod is rescheduled afterward, and a raw NVMe-oF
volume works identically from any node while VDO state does not. `vdoCapableSegment`
in `CreateVolume` merges a `simplyblock.io/vdo-capable=true` segment into the
provisioned `PersistentVolume`'s `AccessibleTopology` whenever either client-side
flag is set, alongside the existing DHCHAP segment, which is what pins
`PersistentVolume.spec.nodeAffinity` so a later pod reschedule cannot land the
volume on a non-VDO-capable node. This mirrors an existing fix for the same gap
in the DHCHAP case (issue #403).

---

## 5. CSI Driver Wiring

`csi-driver/pkg/util/vdo.go` is thin wiring: one shared `lvm.Manager`, and one
function per RPC concern, each delegating straight into `atlas-lib/lvm/vdo`.
Every call site shares one gate:

```go
func vdoParams(vc map[string]string) (compression, deduplication, wantsVDO bool) {
    compression, _ = kube.BoolParam(vc, paramClientCompression, false)
    deduplication, _ = kube.BoolParam(vc, paramClientDeduplication, false)
    return compression, deduplication, compression || deduplication
}
```

**`NodeStageVolume`** (`pkg/spdk/nodeserver.go`): after `initiator.Connect`, if
`wantsVDO`, calls `ResolveClonedVDO` unconditionally, then `CreateOrAttachVDO`.
The *returned* device path, not the raw NVMe-oF path, is what gets formatted and
mounted.

**`NodeUnstageVolume`**: calls `DeactivateVDO` before the raw device disconnects.
Order matters here for the same reason `Deactivate` exists at all (§3): this path
runs on every routine unstage, not only on deletion.

**`NodeExpandVolume`**: raw-block volumes skip filesystem resize entirely, since
the resize tools this driver uses cannot operate on an unmounted raw block
device. For filesystem-mode volumes with `wantsVDO`, `GrowVDO` runs first and the
resize targets the grown VDO device path it returns.

**`restageVolume`**: the same `CreateOrAttachVDO` call as `NodeStageVolume`,
explicitly idempotent, reactivating rather than reformatting.

**XFS stripe hints are skipped for VDO.** `stageVolume` only appends
`xfsStripeOptions` to `mkfs.xfs` when `wantsVDO` is false. Those hints are
computed for the raw, erasure-coded backend device. Once VDO is in the stack, the
filesystem sits on a device VDO virtualizes and relocates blocks on, so the
hints no longer describe anything real.

**`SetVDOFeatures` has no caller.** The mechanism exists (§3, §8) but nothing in
the CSI driver invokes it.

---

## 6. Deployment

**Image.** `csi-driver/deploy/image/Dockerfile` installs `lvm2` unconditionally
(`pvcreate`/`vgcreate`/`lvcreate`/`vgchange`/`lvextend`/`dmsetup`, all of which
`atlas-lib/lvm` shells out to) and `vdo` only on `amd64`, since `vdo` has no
`aarch64` build in the repositories this image installs from. Client-side VDO is
therefore x86_64-only. The `arm64` leg of the image build still succeeds, with
`lvm2` present but `vdo` and `vdoformat` absent, so `lvcreate --type vdo` would
fail on an `arm64` node regardless of what the topology gate says. `vdo`'s
absence there is a hard architecture limit, not a gap the capability gate closes.

**Chart.** `helm-charts/charts/simplyblock-operator/templates/node.yaml` adds
`hostPID: true` and the marker-file host-path volume (§4) to the CSI node
DaemonSet. `templates/node-rbac.yaml` grants the node ServiceAccount `patch` and
`update` on `nodes`, on top of the pre-existing `get`/`list`/`watch`, so
`advertiseVDOCapability` can write the label.

---

## 7. Failure Modes Found and Fixed

Every one of these was found live, against a real cluster, and each is fixed in
`csi-driver/pkg/util/vdo.go`'s history (now folded into `atlas-lib/lvm`) before
the code that carried it shipped:

- **Duplicate-PV ambiguity.** A simplyblock NVMe-oF HA volume presents two
  redundant local device nodes with byte-identical content. An LVM command run
  without `--devices` scans every visible device and cannot tell the two apart,
  reporting a "duplicate PV" error. Every `atlas-lib/lvm` command that names a
  device scopes itself to exactly that device (`atlas-lib/lvm`'s own package doc
  comment covers this in full).
- **Name-based existence checks lie.** `vgs <name>` answers "does a volume group
  by this name exist anywhere LVM can see," not "does it exist on this specific
  device." On a host whose LVM devices file restricts default visibility, this
  reported a volume group as present when it had never been created on the
  device actually being asked about, leaving no logical volume behind it.
  Replaced with a content-based probe (`pvs` on the specific device).
- **An interrupted create leaves an orphaned volume group.** `pvcreate` and
  `vgcreate` completing while `lvcreate` does not leaves a volume group with zero
  logical volumes. `vgchange -ay` against it "succeeds" while producing nothing
  mountable. `CreateOrAttach` detects the zero-LV case and removes the orphan
  before falling through to a fresh create, rather than reactivating it forever.
- **The destructive removal ran on a routine unstage.** Calling the equivalent
  of `Remove` from `NodeUnstageVolume` destroyed a volume's VDO metadata on an
  ordinary pod delete-and-recreate, well before the volume was ever meant to be
  removed. `NodeUnstageVolume` calls the non-destructive `Deactivate` instead.
- **The unreachable-device fallback had no escaping match for device-mapper's own
  naming.** `dmsetup` doubles every literal `-` in a compound name. Matching
  against the unescaped volume-group name found nothing in `dmsetup ls` output,
  leaving an orphaned stack that the fallback could not actually clean up.
- **`pvs`'s combined output pollutes an identity comparison.** A `WARNING:`
  line ahead of the actual field value (duplicate-PV warnings on a byte-level
  clone, in particular) corrupted both the identity check and any log message
  built from the raw output. Both the identity probe and the size probe now read
  only the first non-`WARNING:` line.
- **No udev daemon runs inside this container.** `lvcreate --type vdo` shelled
  out to `vdoformat`, which needs device-mapper's udev-sync handshake to
  complete. Without a udev daemon present it never does, failing with "device
  not cleared." `DM_DISABLE_UDEV=1` on every command's environment fixes it.
- **`lvextend -l100%FREE` is absolute, not additive.** Growing the VDO pool with
  the unprefixed form computed a target smaller than the pool's already-current
  size, since "100% of what is currently free" is smaller than the pool's
  existing size. The additive form, `-l+100%FREE`, is what "grow to consume all
  newly available space" actually means.

---

## 8. Open Questions

| #   | Question                                                                                                                                                                                                                                         | Owner       |
|-----|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------|
| Q1  | Should setting both `clientCompression`/`clientDeduplication` and server-side `compression` on the same `StoragePool` be rejected at admission, or left as a valid, if likely wasteful, combination? Nothing today prevents both from being set. | Product     |
| Q2  | `SetFeatures` (§3) exists and is tested but has no caller. Toggling compression/deduplication on an already-provisioned volume needs a `VolumeAttributesClass`-driven update path, or an explicit decision that this stays create-time-only.     | Product     |
| Q3  | The node-capability installer (§4) assumes `dnf` and RHEL-family package naming (`kmod-kvdo`, `vdo`). A Debian/Ubuntu host never becomes VDO-capable today. Whether that is an acceptable permanent restriction or a gap to close is undecided.  | Product     |
| Q4  | No minimum VDO pool size is enforced in code. A VDO pool below dm-vdo's own practical minimum will fail at `lvcreate` time with whatever error the tool itself produces, not a validated, CSI-level error earlier in provisioning.               | Engineering |

---

## 9. Testing Strategy

`atlas-lib/lvm` and `atlas-lib/lvm/vdo` carry unit coverage for every named
operation in §3, against a fake command runner, with no `lvm2` binary or kernel
module required (`atlas-lib/lvm/vdo/stack_test.go`, `volume_test.go`, and the
general-purpose primitive tests under `atlas-lib/lvm/*_test.go`). `vdoCapableSegment`
(§4) has direct coverage in `csi-driver/pkg/spdk/controllerserver_test.go`.
`csi-driver/pkg/util/vdo.go` itself, the thin wiring in §5, has no direct unit
tests of its own: every branch it adds over the functions it calls is one `if`
around a delegation, and the fake-runner tests already cover the delegated
behavior.

What unit tests cannot cover, and what §7's findings were all found by instead:
whether `dm-vdo` is actually present and behaves as `atlas-lib/lvm/vdo` assumes,
whether a real duplicate-PV HA volume actually produces the ambiguity §7
describes, and whether the whole stack survives a genuine node crash or storage-side
disconnect. Live-cluster verification against a real `StoragePool` with both
flags enabled, covering create, reattach-after-recreate, expand, clone/snapshot
resolution, and reconnect after an unclean disconnect, is the test plan's `M-`
series and is the harness every finding in §7 came from. See
[`tests/test-plan-client-side-vdo-compression.md`](../tests/test-plan-client-side-vdo-compression.md).
