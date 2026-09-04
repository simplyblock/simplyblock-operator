# Design Document: Client-Side Compression and Deduplication

**Status:** Partially Implemented  
**Author:** Manohar Reddy  
**Date:** 2026-08-06 (last updated 2026-09-01)  
**Issue:** https://github.com/simplyblock/simplyblock-operator/issues/277  
**Test Plan:** [`tests/test-plan-issue-277-client-side-compression.md`](../tests/test-plan-issue-277-client-side-compression.md)

---

## Phase 0 — External Prerequisites

Client-side compression depends on a kernel module, a packaging path, and a
capacity floor that this repository does not build. All of them are collected
here, and each row names the section that specifies it.

| #    | Prerequisite                                                                              | Kind                | Blocks                                                      | Status                                                          |
|------|-------------------------------------------------------------------------------------------|---------------------|-------------------------------------------------------------|-----------------------------------------------------------------|
| P0-1 | `kmod-kvdo` and `vdo` reachable from every node's BaseOS repository                       | Node OS             | Auto-install on kernels below 6.9 (§4.1)                    | Available on RHEL-family nodes with repository access           |
| P0-2 | In-tree `dm-vdo`, kernel 6.9 or newer                                                     | Node OS             | The install-free capability path (§4.1)                     | Available on a current-enough kernel, detection not implemented |
| P0-3 | `lvm2` with the `vdo` and `vdo-pool` segment types, on the node and in the CSI node image | Node OS             | Every VDO stack operation (§7)                              | Available, installed into the CSI node image (§4.2)             |
| P0-4 | `vdoformat`, which `lvcreate --type vdo` invokes internally, in the CSI node image        | Node OS             | Volume creation (§7.2)                                      | Available on `x86_64` only, no `aarch64` build exists           |
| P0-5 | RHEL-family packaging: `dnf`, `rpm`, and the weak-modules mechanism                       | Node OS             | Auto-install (§4.1)                                         | Available, other distributions are a non-goal (§2)              |
| P0-6 | `hostPID` on the `csi-node` DaemonSet, so `nsenter` reaches the host mount namespace      | Kubernetes          | Auto-install (§4.1)                                         | Available, set by the development chart                         |
| P0-7 | A volume of at least 5GiB, above VDO's own floor of roughly 4.72GiB                       | Storage plane (VDO) | Any compressed or deduplicated volume (§6)                  | Enforced by VDO itself, with no operator-side pre-check         |
| P0-8 | The `node.kubernetes.io/out-of-service` taint, Kubernetes 1.24 or newer                   | Kubernetes          | Recovery of an RWO volume from a permanently dead node (§8) | Available in-cluster, not invoked by this design                |

Each missing item has a different consequence. Without P0-1 and P0-2 a node
never becomes VDO-capable, so the topology gate (§5) keeps compressed volumes
off it and the rest of the cluster is unaffected. Without P0-3 or P0-4 the node
plugin fails the stage rather than mounting the raw device, which is the
deliberate hard failure of §8. P0-4 confines the feature to `x86_64` nodes for
now. P0-7 is a user-visible provisioning constraint rather than a cluster
prerequisite: a smaller volume cannot hold a VDO container at all. P0-8 bounds
only the recovery path after a node dies permanently, described in §8.

---

## Table of Contents

- [Design Document: Client-Side Compression and Deduplication](#design-document-client-side-compression-and-deduplication)
  - [Phase 0 — External Prerequisites](#phase-0--external-prerequisites)
  - [Table of Contents](#table-of-contents)
  - [Overview](#overview)
  - [1. Background](#1-background)
    - [1.1 VDO Availability](#11-vdo-availability)
  - [2. Goals and Non-Goals](#2-goals-and-non-goals)
    - [Goals](#goals)
    - [Non-Goals](#non-goals)
  - [3. Architecture Overview](#3-architecture-overview)
  - [4. Node Capability: Auto-Install and Advertisement](#4-node-capability-auto-install-and-advertisement)
    - [4.1 Install and Module Load](#41-install-and-module-load)
    - [4.2 Container Image Dependencies](#42-container-image-dependencies)
    - [4.3 Capability Advertisement](#43-capability-advertisement)
  - [5. Scheduling Gate: Topology](#5-scheduling-gate-topology)
  - [6. StorageClass Parameter and CRD Changes](#6-storageclass-parameter-and-crd-changes)
  - [7. VDO Device Management](#7-vdo-device-management)
    - [7.1 Package Placement](#71-package-placement)
    - [7.2 Device Creation](#72-device-creation)
    - [7.3 Device Identity in HA Mode](#73-device-identity-in-ha-mode)
    - [7.4 Clone and Snapshot Restore](#74-clone-and-snapshot-restore)
    - [7.5 Write Policy](#75-write-policy)
    - [7.6 Wiring into `nodeserver.go`](#76-wiring-into-nodeservergo)
  - [8. Re-Provisioning and Failure Handling](#8-re-provisioning-and-failure-handling)
  - [9. Volume Expansion](#9-volume-expansion)
  - [10. RBAC Changes](#10-rbac-changes)
  - [11. Testing Strategy](#11-testing-strategy)
  - [12. Compatibility with Existing CSI Driver and Operator Features](#12-compatibility-with-existing-csi-driver-and-operator-features)
  - [13. Observability](#13-observability)
    - [Kubernetes Events](#kubernetes-events)
    - [Prometheus Metrics](#prometheus-metrics)
  - [14. Open Questions](#14-open-questions)

---

## Overview

Client-side compression and deduplication put a VDO device between the NVMe-oF
multipath device and the filesystem mount, on the node that consumes the volume
rather than on the storage node that hosts it. A StorageClass parameter selects
it, the CSI node plugin builds it, and the data reduction happens before a byte
reaches the wire.

The feature has three parts, and each answers a different question. Node
capability (§4) answers whether a node can run `dm-vdo` at all, by installing
and loading the module and recording the answer in a node label. The topology
gate (§5) answers where a compressed volume is allowed to land, by turning that
label into a CSI topology segment the scheduler honors. Device management (§7)
answers what happens at stage time, by assembling a per-volume LVM stack on the
raw device and handing the resulting device to the existing format-and-mount
path.

Compression and deduplication are two independent parameters, not one switch.
VDO enables each separately, and their costs differ by orders of magnitude:
compression is CPU-only and cheap, while deduplication carries a large fixed
RAM cost per volume for its UDS index. Keeping them separate is what lets a
pool buy compression without buying that index.

The code splits along the same line the repository splits on. The node-level
primitives, device-scoped LVM commands and the VDO stack lifecycle, live in
`atlas-lib` and know nothing about Kubernetes. The CSI driver holds what is
Kubernetes-shaped: the volume-context parameters, the node label, the topology
segment, and the stage and unstage sequencing.

---

## 1. Background

[Issue #277](https://github.com/simplyblock/simplyblock-operator/issues/277)
asks for a client-side compression and deduplication layer. When a StorageClass
requests it, a compression block device is created between the CSI mount and the
NVMe-oF multipath device that backs the volume, and that device has to be
re-included every time the node-side CSI driver re-provisions the volume. VDO,
the Virtual Data Optimizer exposed as the `dm-vdo` device-mapper target, is the
mechanism.

Nothing in the existing server-side `compression` parameter covers this. That
one compresses on the storage node, after the data has crossed the network.
Compressing on the consumer node reduces what crosses the network in the first
place, which is the point of the request.

### 1.1 VDO Availability

VDO cannot be assumed present on a node. The `kmod-kvdo` and `vdo` packages are
available through the distribution's BaseOS repository, but neither is installed
by default, and `kmod-kvdo` ships prebuilt `kvdo.ko` binaries pinned to specific
`kernel-core` NVRs through RHEL's weak-modules mechanism. A node that was
capable before a kernel update is not necessarily capable after one.

VDO is therefore treated as an opt-in, per-node capability: checked explicitly,
advertised explicitly, and gated on for scheduling.

---

## 2. Goals and Non-Goals

### Goals

- Let a StorageClass opt a pool into client-side compression, deduplication, or
  both, through two independent parameters, `clientCompression` and
  `clientDeduplication`, that are separate from the server-side `compression`
  parameter and from each other.
- Install and load the VDO kernel module and tooling on a node when the CSI node
  plugin starts there, without requiring a pre-baked golden image.
- Advertise per-node VDO capability so that Kubernetes schedules a
  compression-requesting PVC only onto a capable node, reusing the CSI topology
  infrastructure this repository already has.
- Insert a VDO device between the raw NVMe-oF multipath device and the
  filesystem mount in `NodeStageVolume`, and re-attach that device, never
  recreate or reformat it, on every reconnect and restage path. This is the
  issue's re-provisioning requirement.
- Fail loudly when a volume that requests compression or deduplication lands on
  a node without working VDO support, rather than degrading silently to a raw
  mount.

### Non-Goals

- Changing the existing server-side `compression` feature.
- Sharing one VDO instance across several volumes. This design creates one VDO
  instance per volume, directly on that volume's raw device.
- Airgapped installation. A cluster whose nodes cannot reach a BaseOS
  repository needs the module pre-baked into the node image, and §4.1 describes
  how capability detection still works in that case.
- Support for `aarch64` nodes. The `vdo` package that provides `vdoformat` has
  no `aarch64` build (P0-4), so the feature is `x86_64`-only.
- Support for node operating systems outside the RHEL family. The
  `kmod-kvdo` and `vdo` packages are a RHEL-family concept, and no equivalent
  path on a distribution such as Ubuntu has been evaluated.
- Guaranteeing the longevity of the standalone `vdo` CLI. Red Hat has moved
  toward LVM-integrated VDO, which is the interface this design uses (§7.1).

---

## 3. Architecture Overview

```
     ┌───────────────────────────────────────────────────┐
     │ StoragePool CR (StorageClassParameters)           │
     │   clientCompression: true                         │
     │   clientDeduplication: false                      │
     └───────────────────────────────────────────────────┘
       │ mergeStorageClassParameters()
       ▼
     ┌───────────────────────────────────────────────────┐
     │ Generated StorageClass                            │
     │   parameters.client_compression   = "True"        │
     │   parameters.client_deduplication = "False"       │
     │   volumeBindingMode: WaitForFirstConsumer         │
     └───────────────────────────────────────────────────┘
       │
       ▼
     ┌───────────────────────────────────────────────────┐
     │ Kubernetes scheduler                              │
     │   binds only to a node advertising                │
     │   simplyblock.io/vdo-capable=true                 │
     └───────────────────────────────────────────────────┘
       │
       ▼
┌──────────────────────────────────────────────────────────────────────┐
│ simplyblock-csi-node DaemonSet, one pod per node                     │
│                                                                      │
│  1. postStart hook: nsenter into PID 1, install kmod-kvdo and vdo,   │
│     modprobe kvdo, write the capability marker file                  │
│  2. advertiseVDOCapability: read the marker, patch the node label    │
│  3. NodeGetInfo -> buildAccessibleTopology: label to CSI segment     │
│  4. CreateVolume -> vdoCapableSegment: PV nodeAffinity               │
│  5. NodeStageVolume: initiator.Connect, then ResolveClonedVDO and    │
│     CreateOrAttachVDO, then stageVolume against the VDO device       │
│                                                                      │
│  Node label simplyblock.io/vdo-capable and its managed-by annotation │
└──────────────────────────────────────────────────────────────────────┘
              │ imports (node-level primitives, no Kubernetes awareness)
┌─────────────▼────────────────────────────────────────────────────────┐
│ atlas-lib: github.com/simplyblock/atlas/lvm and .../atlas/lvm/vdo    │
│   lvm.Manager: device-scoped LVM commands, content-based identity    │
│   lvm/vdo:     CreateOrAttach, ResolveClone, Deactivate, Remove,     │
│                Grow, SetFeatures                                     │
└──────────────────────────────────────────────────────────────────────┘
```

Everything below the scheduler runs on the consumer node, not on a storage
node. That single fact settles most of §12: a feature that operates on the
storage side, including volume migration, cannot disturb a device that lives on
the client side of the NVMe-oF connection.

New in this design are the `clientDeduplication` parameter, the
`vdo-capable` label and the topology gate built on it, the `postStart` install
step, the marker-read and label-patch step, and the VDO stack lifecycle in
`atlas-lib`. The pieces it reuses unchanged are
`mergeStorageClassParameters`, `WaitForFirstConsumer`, the scheduler,
`initiator.Connect`, `NodeStageVolume`, and `stageVolume`.

---

## 4. Node Capability: Auto-Install and Advertisement

### 4.1 Install and Module Load

The `csi-node` container's `postStart` lifecycle hook carries an additional,
independent step:

```bash
nsenter -t 1 -m -u -n -i -- sh -c 'rpm -q kmod-kvdo vdo >/dev/null 2>&1 || dnf install -y kmod-kvdo vdo'
modprobe kvdo
```

The result is written to `/var/run/simplyblock/vdo-capable/marker`, backed by
the host path `/var/lib/simplyblock/vdo-capable`, as the literal string `true`
or `false`. §4.3 turns that marker into a node label.

The container is already privileged, already holds `SYS_ADMIN` and
`SYS_MODULE`, and already mounts the host's `/lib/modules` read-only. That is
enough to `modprobe` a module the host already has, which is how `nvme-tcp` is
loaded today. It is not enough to install a package, because installation writes
into the host's real `/lib/modules`, updates `/var/lib/rpm`, and runs `depmod`.

Reaching real host root through `hostPID: true` and `nsenter -t 1` avoids
adding a writable hostPath mount of `/` to a long-running DaemonSet. This is the
idiom driver-installer DaemonSets use, and it keeps the blast radius smaller than
mounting the entire host filesystem read-write into a persistent pod.

**Airgapped clusters.** The install step needs BaseOS repository access from
every node. A cluster without it needs the module in the node image instead, and
capability detection still reports correctly there, because it checks
`modprobe` rather than whether the install ran. The `rpm -q` guard makes a
repeat install a no-op, and a genuinely unreachable repository leaves the marker
at `false`, which the topology gate then honors.

**In-tree `dm-vdo` on kernel 6.9 and newer.** Per the `dm-vdo/kvdo` project's
README, the out-of-tree `kvdo` module is no longer updated for newer kernels,
because its functionality merged into the mainline kernel as `dm-vdo` at 6.9. On
a node running such a kernel, none of the `dnf install` machinery is needed. The
capability check is specified to try `modprobe dm-vdo` first and fall through to
the `kmod-kvdo` install path only when that fails, so that a current-enough node
becomes capable with no install step and no BaseOS dependency at all. The
implemented probe still tries only the legacy path, which Q3 in §14 tracks.

### 4.2 Container Image Dependencies

The LVM commands in §7 run inside the `csi-node` container, not on the host, so
the image carries `lvm2` and `vdo` itself. `lvm2` provides `pvcreate`,
`vgcreate`, `lvcreate`, `vgchange`, `lvextend`, and `dmsetup`. The `vdo` package
provides `vdoformat`, which `lvcreate --type vdo` invokes internally to format a
new pool, so `lvm2` alone leaves volume creation failing on a missing
`/usr/bin/vdoformat`.

The `vdo` package has no `aarch64` build in this repository's package set, so
the image installs it on `amd64` only and the `arm64` leg of the multi-arch
build stays green. That confines the feature to `x86_64` nodes (P0-4).

### 4.3 Capability Advertisement

The outcome of the module load reaches the Kubernetes API as a node label:
`simplyblock.io/vdo-capable=true` on success, and `false`, or the label absent,
on failure.

`advertiseVDOCapability` runs on every `csi-node` pod start, and a hand-set
label is the escape hatch an airgapped cluster or a golden-image node depends
on, so the probe has to tell its own labels apart from an operator's. Every
label value the probe writes itself is stamped with a second annotation,
`simplyblock.io/vdo-capable-managed-by: auto-detect`. On startup the probe first
checks whether the label is already present without that annotation, and leaves
such a label untouched. A label carrying the annotation, or no label at all, is
the probe's to manage.

---

## 5. Scheduling Gate: Topology

This repository already has working CSI topology infrastructure, and the gate
extends it in two places.

On the node side, `buildAccessibleTopology` in `nodeserver.go` already turns
node labels into CSI topology segments reported through `NodeGetInfo`, and it
surfaces `simplyblock.io/vdo-capable=true` the same way. On the controller side,
`vdoCapableSegment` adds the same segment to the `CreateVolume` response
whenever either client parameter is true, which is what puts a matching
`nodeAffinity` on the resulting PersistentVolume and keeps the volume on capable
nodes for its whole life rather than only at first binding.

`VolumeBindingMode` is already `WaitForFirstConsumer`, so binding is deferred
until topology can be evaluated, and the gate needs no scheduler changes. A PVC
that requests client compression or deduplication is never bound to a node
lacking VDO support in the first place.

The gate covers initial placement only. A node whose capability regresses while
a volume is already bound there is §8's subject.

---

## 6. StorageClass Parameter and CRD Changes

Compression and deduplication are separate parameters because VDO supports
enabling each independently (`lvcreate --compression y|n --deduplication y|n`,
and `lvchange` for a live toggle after creation), and because their costs are
not comparable. Compression is CPU-only and cheap. Deduplication carries a
large fixed RAM cost per volume for its UDS index, whether or not compression is
also on. Separate parameters are what keep that cost on the pools that asked for
deduplication.

`StorageClassParameters` in `operator/api/v1alpha1/storagepool_types.go` gains
two fields:

```go
// ClientCompression enables client-side (VDO) compression for logical
// volumes in this pool. Distinct from Compression, which is server-side.
// Independent of ClientDeduplication: either, both, or neither may be set.
// +kubebuilder:default=false
ClientCompression *bool `json:"clientCompression,omitempty"`

// ClientDeduplication enables client-side (VDO) deduplication for logical
// volumes in this pool. Carries a significant fixed RAM cost per volume,
// independent of ClientCompression, so it is meant to be enabled on the
// pools where duplicate data is actually expected (VM images, container
// layers, backup targets) rather than by default.
// +kubebuilder:default=false
ClientDeduplication *bool `json:"clientDeduplication,omitempty"`
```

Both fields are optional, default to `false`, and are mutable: a change reaches
volumes staged after it, and §14's Q1 covers what an in-place toggle would
require. Adding them means `make -C operator manifests generate` for
`zz_generated.deepcopy.go` and the CRD YAML, and `make helm-sync` for the
chart's copy of the CRD.

`mergeStorageClassParameters` in `simplyblockstoragepool_controller.go` passes
both through to the generated StorageClass:

```go
dst["client_compression"] = boolStr(p.ClientCompression)
dst["client_deduplication"] = boolStr(p.ClientDeduplication)
```

Two rules follow from the two parameters being independent:

- **VDO is required whenever either parameter is true.** The capability check in
  §4 and the topology gate in §5 trigger on
  `client_compression == "true" || client_deduplication == "true"`, because a
  deduplication-only volume needs a working module exactly as much as a
  compression-only one does. `vdoParams` in `nodeserver.go` and
  `vdoCapableSegment` in `controllerserver.go` both apply that rule, and both
  tolerate the `"True"` spelling that `boolStr` emits.
- **A volume that requests either parameter is at least 5GiB.** VDO enforces a
  hard floor of roughly 4.72GiB, and a smaller device cannot hold a VDO
  container at all. Nothing pre-checks the size today, so the failure surfaces
  from `lvcreate` (§14, Q2).

---

## 7. VDO Device Management

### 7.1 Package Placement

VDO is managed through LVM (`lvcreate --type vdo`), which is the only VDO
management interface available on the target operating system: the standalone
`vdo` CLI is not part of the shipped `vdo` package on RHEL-family systems.
`lvm2` provides native VDO support, and `lvm segtypes` lists `vdo` and
`vdo-pool`. Each CSI volume gets its own PV, VG, VDO pool, and LV stack, which
is one VDO instance per volume as §2 requires.

Nothing in that stack is Kubernetes-shaped, so it lives in `atlas-lib` and the
CSI driver imports it:

| Package                                | Holds                                                                                               |
|----------------------------------------|-----------------------------------------------------------------------------------------------------|
| `github.com/simplyblock/atlas/lvm`     | `lvm.Manager`: device-scoped LVM commands, content-based volume-group identity, and dm node cleanup |
| `github.com/simplyblock/atlas/lvm/vdo` | The VDO stack lifecycle, and the `lvcreate` argument handler registered for the `vdo` segtype       |
| `csi-driver/pkg/util/vdo.go`           | One shared `lvm.Manager` and one wrapper per CSI concern, which is the whole CSI-side surface       |

The `lvm/vdo` package's public surface is the lifecycle of one volume's stack:

```go
// CreateOrAttach idempotently ensures a VDO-backed logical volume exists on top of
// devicePath, named after lvolID, and returns the device path to format and mount.
// An existing volume group is reactivated and never recreated. Only a genuinely
// absent one is created fresh. compression and deduplication are set independently
// at creation time, and changing them later is SetFeatures' job.
func CreateOrAttach(ctx context.Context, manager *lvm.Manager, devicePath, lvolID string, compression, deduplication bool) (string, error)

// ResolveClone re-stamps the identity of a device that is a block-level copy of
// another VDO volume, before any activation is attempted. See §7.4.
func ResolveClone(ctx context.Context, manager *lvm.Manager, devicePath, lvolID string) error

// Deactivate deactivates, and does not destroy, lvolID's stack. This is the
// counterpart to a plain NVMe-oF disconnect, and the correct call from a routine
// unstage.
func Deactivate(ctx context.Context, manager *lvm.Manager, lvolID string) error

// Remove deactivates and removes lvolID's stack, destroying its data. Appropriate
// only when the volume itself is being removed, or to clean up an orphaned stack
// whose backing device is already gone.
func Remove(ctx context.Context, manager *lvm.Manager, lvolID string) error

// Grow extends the pool LV to consume the newly available physical space on
// devicePath, grows the VDO logical volume to match, and returns its device path.
func Grow(ctx context.Context, manager *lvm.Manager, devicePath, lvolID string) (string, error)

// SetFeatures toggles compression and deduplication on an existing, active volume
// without recreating it. No live update path calls it yet (§14, Q1).
func SetFeatures(ctx context.Context, manager *lvm.Manager, lvolID string, compression, deduplication bool) error
```

`csi-driver/pkg/util/vdo.go` binds one process-wide `lvm.Manager` and exposes
`CreateOrAttachVDO`, `ResolveClonedVDO`, `DeactivateVDO`, `RemoveVDO`,
`GrowVDO`, and `SetVDOFeatures`, each a single call into the package above.
Keeping the wrappers means `nodeserver.go` never assembles an `lvm.Manager` of
its own, and the primitive stays usable outside a Kubernetes context.

### 7.2 Device Creation

```bash
DEV=/dev/disk/by-id/nvme-uuid.<lvol-uuid>
VOLID=<lvol-uuid>
pvcreate --devices $DEV $DEV
vgcreate --devices $DEV vdo-$VOLID $DEV
lvcreate --devices $DEV --type vdo --config "activation{checks=0}" -n $VOLID -l 100%FREE \
  --compression y --deduplication y vdo-$VOLID/vdopool --yes
mkfs.xfs -f /dev/vdo-$VOLID/$VOLID && mount /dev/vdo-$VOLID/$VOLID <stagingPath>
```

`$DEV` is the stable `/dev/disk/by-id/nvme-uuid.<lvol-uuid>` path that
`initiator.go`'s `waitForDeviceReady` already resolves, so no new
device-resolution logic is needed. The resulting dm device is named
`vdo-<VOLID>-vdopool-vpool` rather than after the LV, which is the name
`vdostats` and `dmsetup` need.

`-l 100%FREE` sizes the pool to the device's actual available capacity instead
of a hardcoded value. Omitting `-V` sizes the logical volume to the largest size
that stays safe within the pool even at zero savings, per `man lvmvdo`. Every
LVM command is scoped to `$DEV` through `--devices`, which §7.3 explains is a
correctness requirement rather than tidiness.

Idempotency is a check-then-act guard rather than a property of the tooling.
`lvcreate --type vdo -n X` fails when `X` already exists, so `CreateOrAttach`
probes with `pvs` and `lvs` before it calls anything, because VDO offers no
idempotency mechanism of its own. A stack that is present is reactivated with
`vgchange -ay`, and a stack that is present but incomplete is handled as §7.3
describes.

### 7.3 Device Identity in HA Mode

A volume's two redundant NVMe-oF HA paths each surface as their own local
`/dev/nvmeXnY` device node while presenting byte-identical content. Neither
identity layer this design relies on uses the NVMe serial number, because on a
real ha-mode volume `nvme list` reports `SN` as the literal string `ha`,
identical across every ha-mode volume in the cluster. The model field carries
the lvol's UUID instead.

- **The NVMe-oF and udev layer.** SPDK sets the NVMe namespace UUID equal to the
  lvol's own UUID at creation time, and `udev` derives the
  `/dev/disk/by-id/nvme-uuid.<uuid>` symlink from that field. The symlink is a
  property of the namespace rather than of whichever `/dev/nvmeXnY` enumeration
  the kernel assigns this time, so it is recreated pointing at the correct
  device after a reconnect, on whichever node the volume lands. The glob
  resolution in `initiator.go` keys off the same SPDK-controlled model field.
- **The LVM layer, independently.** Once `pvcreate` has run, LVM writes its own
  UUID into the on-disk PV header, unrelated to the NVMe namespace UUID.
  `pvscan --cache` finds a PV by scanning the content of every visible block
  device for that UUID rather than by remembering a path, so it locates the same
  PV under a new device node after a reconnect with no resolution logic of this
  design's own.

Byte-identical HA paths defeat LVM's default, system-wide device scan.
`pvscan --cache <path>` hits a duplicate-PV ambiguity between a volume's two
device nodes, and a name-based `vgs <name>` check reports a VG as present when
it was never created on the intended device, because this host restricts default
LVM visibility through `/etc/lvm/devices/system.devices` and a name-only lookup
does not tie its answer back to a device. Three properties of `lvm.Manager`
handle it:

1. Every command is scoped to exactly one device through LVM's `--devices`
   flag, which bypasses the system-wide scan. `lvm.Manager.exec` inserts the
   scope directly after the binary name, and `Run` is the deliberate unscoped
   escape hatch for a command that has no device.
2. Existence is content-based. `Manager.VolumeGroup` reads the VG name from the
   device itself, tolerating the `WARNING: … is duplicate for PVID …` lines a
   duplicated PV puts ahead of the real field, and treats a device with no PV
   signature as belonging to no VG rather than as an error.
3. `Manager.HasLogicalVolume` distinguishes a complete stack from an **orphaned
   VG** left by an interrupted create that reached `vgcreate` but never
   `lvcreate`. Such a VG reports zero LVs, and `vgchange -ay`
   against it succeeds while producing no mountable device, so `CreateOrAttach`
   removes it and falls through to a fresh create.

The same content-based identity is what makes clone detection possible in §7.4,
and `Manager.RemoveOrphanedDMNodes` is what cleans up after a device disappears
without a clean unstage in §8.

### 7.4 Clone and Snapshot Restore

A PVC clone and a VolumeSnapshot restore are both block-level copy-on-write
copies at the storage layer rather than a reformat, so a clone of a VDO-backed
volume carries its source's on-disk LVM metadata verbatim: the same PV UUID, the
same VG UUID, and the same VG name. LVM identifies a PV and a VG by that on-disk
UUID, not by the CSI volume it logically belongs to. A clone is therefore
indistinguishable from its source, and `CreateOrAttach`'s own question, whether
a VG named after this volume exists, is answered no while the VG on disk is
named after the source. Two failures follow from that answer: a fresh
`lvcreate` over the cloned data, and a name collision with the source once both
devices are visible on one node. `ResolveClone` is what prevents both.

`ResolveClone` runs before `CreateOrAttach` on every stage and settles the
question from the device's own on-disk identity, which is why it does not depend
on whether the content-source fact survived into the volume context. When the VG
it finds belongs to a different volume, it regenerates the PV and VG UUIDs and
renames the VG and its LV to this volume's identity, the `vgimportclone` and
`lvrename` equivalent, before any activation happens. VDO's own pool LV is
structural and named identically in every stack, so it is preserved rather than
renamed. A genuinely fresh device, and one already resolved, are both left
alone.

Resolution happens once, at first stage. Afterward, the device is
indistinguishable from any other VDO volume, and every later reattach,
reconnect, and grow takes the same path as any other volume. Unlike the `nouuid`
mount flag that answers an XFS UUID collision, the LVM and VDO layer has no
filesystem-level workaround, which is why the rename is required rather than
optional.

### 7.5 Write Policy

LVM's default write policy is `auto`, which picks `sync` or `async` according to
whether the backing device reports a volatile write cache, and simplyblock-backed
storage does. No command in this design overrides it. Throughput measured against
a real NVMe-oF-backed lvol was statistically indistinguishable across `sync`,
`async`, and `auto`, so there is no performance case for forcing a policy, and
`vdo_write_policy` stays at `auto`. Durability under `async` is not weaker for
an acknowledged write: flush and FUA semantics carry end-to-end through NVMe-oF
to the backend, which §8 describes.

### 7.6 Wiring into `nodeserver.go`

`vdoParams` parses the two parameters out of a volume context and reports
whether either is set, and every path below keys off that one answer.

- **`NodeStageVolume`** calls `ResolveClonedVDO` and then `CreateOrAttachVDO`
  between `initiator.Connect` and `ns.stageVolume`, and passes the returned VDO
  device path into `stageVolume` in place of the raw NVMe-oF path. Both
  parameters pass through independently, per §6. `ResolveClonedVDO` runs on
  every stage rather than only when a `VolumeContentSource` is present, for the
  reason §7.4 gives. The volume context, already persisted through
  `util.StashVolumeContext`, is what later unstage and restage paths read to
  learn that VDO is in play.
- **`NodeUnstageVolume`** calls `DeactivateVDO` before `initiator.Disconnect`,
  because VDO has to come down before the device underneath it goes away. The
  call is the non-destructive `DeactivateVDO` rather than `RemoveVDO`, because
  this path fires whenever no pod on the node currently needs the volume
  mounted, a routine pod delete and recreate included, and not only when the
  volume is being deleted.
- **`restageVolume` and `ensureDeviceConnected`** reattach the existing VDO
  device after `initiator.Connect` re-establishes the raw device, and before
  remounting. Reattachment is `pvscan --cache` and `vgchange -ay`, never
  `lvcreate`, which mirrors the "never reformat, the data already exists"
  invariant `restageVolume` holds today. This is the mechanism that satisfies
  the issue's re-provisioning requirement. It holds across a node reboot with no
  ghost state in between, because the VG is invisible until NVMe-oF reconnects
  and `pvscan --cache` rediscovers its PV.
- **`NodeExpandVolume`** calls `GrowVDO` before the existing filesystem resize,
  per §9.
- **`stageVolume`** skips `xfsStripeOptions` whenever VDO is in play. Those
  options align `mkfs.xfs` to the backend's erasure-coding stripe geometry
  through `xfs_su` and `xfs_sw`, and VDO virtualizes and relocates blocks, so
  the filesystem no longer sits directly on the erasure-coded device. Applying
  hints computed for the raw device to a VDO virtual device is misleading rather
  than merely useless.

---

## 8. Re-Provisioning and Failure Handling

Topology gating (§5) controls initial PV scheduling only. It does not protect
against a node's capability regressing after a volume is bound there, for
instance when an OS update leaves `kvdo` incompatible with the running kernel,
and Kubernetes does not evict a running pod because its node's label changed.

Whether a volume uses VDO is baked into its on-disk format at creation, because
the raw device holds a VDO container rather than a bare filesystem. A later
`NodeStageVolume` or `restageVolume` on a node without working VDO therefore
cannot fall back to a raw mount: the bytes on disk are VDO-formatted. When
either client parameter is present in the volume context and
`CreateOrAttachVDO` fails locally, both paths return a hard error, reported
through the same klog error paths as every other hard failure in that file.

| Failure                                             | Detection                                                       | Behavior                                                                                         |
|-----------------------------------------------------|-----------------------------------------------------------------|--------------------------------------------------------------------------------------------------|
| Node not VDO-capable at first binding               | Missing `vdo-capable` topology segment                          | The PVC stays `Pending`, and the volume is never bound to that node (§5)                         |
| Node capability regresses after binding             | `CreateOrAttachVDO` fails at stage or restage                   | Hard error from `NodeStageVolume` or `restageVolume`, never a raw mount                          |
| Volume smaller than VDO's floor                     | `lvcreate` exits 5, reporting the minimum size                  | Stage fails with that error surfaced, and no partial stack is left behind (§14, Q2)              |
| Interrupted create, VG present with no LV           | `HasLogicalVolume` reports zero LVs                             | The orphaned VG is removed, and a fresh stack is created (§7.3)                                  |
| Clone or restore carrying its source's LVM identity | Content probe finds a foreign VG on the device                  | `ResolveClone` re-stamps PV and VG UUIDs before activation (§7.4)                                |
| Backing device gone without a clean unstage         | `vgchange -an` fails with `Volume group … not found`            | Fallback to `dmsetup remove` of the live dm nodes, retried across passes                         |
| Node reboot with several VDO volumes                | Kubelet stages every volume afresh after a restart              | Each stack reattaches independently and idempotently                                             |
| Node permanently dead, RWO volume attached          | `FailedAttachVolume: Multi-Attach error` on the replacement pod | The replacement pod stays blocked until an administrator applies the out-of-service taint (P0-8) |
| Unclean node crash mid-write                        | VDO's journal thread hits a fatal I/O error                     | VDO fences itself read-only and the filesystem aborts its journal, without corrupting data       |

**Restart is a re-provision like any other.** Kubelet's own bookkeeping resets
on a reboot, so it calls `NodeStageVolume` afresh for every volume rather than
`restageVolume`, and each VDO instance reattaches independently.

**Stale state after a node loses a volume without a clean unstage.** This covers
a pod force-rescheduled off a node that went `NotReady`, and the storage side
disconnecting the initiator while the node stays up. `DeactivateVDO`'s
`vgchange -an` fails with `Volume group … not found`, because no device remains
to read the VG's metadata from, and the fallback is a direct `dmsetup remove` of
the live dm nodes rather than the normal LVM teardown path.
`Manager.RemoveOrphanedDMNodes` matches every dm node whose name starts with the
volume group's dash-escaped name, `vdo-<uuid>` appearing as `vdo--<uuid>` in
`dmsetup ls` output, and removes each with a plain `dmsetup remove`. It retries
across up to three passes, so a node still blocked by a live dependency clears
once that dependency is gone. Kubelet's volume reconciler re-invokes
`NodeUnstageVolume` on the original node once it rejoins, which is what triggers
the cleanup.

**A permanently failed node blocks the replacement pod indefinitely.**
Kubernetes' attachdetach-controller refuses to attach an RWO volume to a
replacement pod on another node while the original node's attachment cannot be
confirmed released, and this design does nothing to shorten that wait. The block
clears when the original node rejoins and releases the attachment. When the node
never rejoins, nothing here invokes the `node.kubernetes.io/out-of-service`
taint (P0-8), the mechanism by which an administrator or an automation marks a
confirmed-dead node so that the attachdetach-controller force-detaches without
waiting for a graceful release. A workload on a permanently dead node stays
stuck until someone intervenes. §14's Q4 owns whether this design should invoke
it.

**The node's `/etc/lvm/devices/system.devices` file** restricts LVM's default
visibility to specific devices, and no code path prunes a stale entry after its
device disappears. The file gates which devices LVM considers rather than
causing a failure of its own, so this is hygiene rather than safety, but it is
unbounded: a node accumulates one stale entry per failure cycle over its
lifetime with no automatic pruning. §14's Q5 owns it.

**Crash consistency under `async`.** An acknowledged write survives an unclean
node crash, because flush and FUA durability carries end-to-end through NVMe-oF
to the simplyblock backend. A write that was never `fsync()`'d is lost, which is
the correct POSIX outcome. Nothing here depends on forcing the `sync` policy
(§7.5).

---

## 9. Volume Expansion

`NodeExpandVolume` resizes the filesystem against the device path from the
volume context. There is no standalone `vdo growPhysical` or `growLogical`
command on this system, because VDO is managed through LVM (§7.1), so growing a
VDO volume is an `lvextend` against the pool LV for physical space and against
the VDO LV for logical size, followed by the existing filesystem resize through
`mount.NewResizeFs` against the now larger VDO logical device. `GrowVDO` takes
the device path rather than a target size, because the physical step consumes
whatever new space the device reports, matching the `100%FREE` convention
creation uses.

Growth is online: the physical extend, the logical extend, and the filesystem
resize all run while the filesystem stays mounted. Overhead is roughly fixed in
absolute terms rather than proportional, so a pool's `Data%` falls as it grows.
Growth from exactly at VDO's minimum-size floor is not covered (§14, Q2).

---

## 10. RBAC Changes

`helm-charts/charts/simplyblock-operator/templates/node-rbac.yaml` grants the
CSI node's ClusterRole `patch` and `update` on `nodes`, alongside the
pre-existing `get`, `list`, and `watch`. The capability-labeling step in §4.3
needs them to write the `simplyblock.io/vdo-capable` label and its
`vdo-capable-managed-by` annotation.

Node objects are cluster-scoped, and `patch` on them is a wide grant: the CSI
node plugin can label and taint any node in the cluster. It is the narrowest
verb set that self-labeling allows, since a node plugin cannot patch only its
own Node object through RBAC alone, and the plugin already runs privileged on
every node.

---

## 11. Testing Strategy

The mechanism is verified end-to-end on a live cluster, and what remains open is
concentrated rather than spread across the feature. The CSI-integrated stage,
unstage, expand, and reconnect paths, node capability auto-detection and its
operator override, the topology gate including a genuine cross-node reschedule,
and `VolumeMigration` compatibility all carry real evidence. The gaps are
interrupt recovery mid-`lvcreate`, a live regression in which `kvdo` becomes
unloadable, and behavior at realistic fleet density.

Full scenario matrix, coverage status, and hand-off test concepts:
[`tests/test-plan-issue-277-client-side-compression.md`](../tests/test-plan-issue-277-client-side-compression.md).

- **Unit:** the check-then-act branches of the VDO stack lifecycle and the
  device-scoping and content-identity behavior underneath it, with every LVM
  command answered by a fake runner. On the CSI side, the override logic for the
  capability label, the topology segment, and the StorageClass parameter
  generation, all without a cluster.
- **Integration:** the operator's reconcile loop against `envtest`, proving that
  a pool's two parameters reach the generated StorageClass and its
  `allowedTopologies` and compose with the constraints DHCHAP already adds.
- **E2E:** the full path from StorageClass through the scheduler to
  `NodeStageVolume` on a live multi-node cluster, with real LVM against a real
  NVMe-oF lvol: creation, reattachment, growth, clone resolution, reconnect,
  reboot, migration, and cross-node reschedule.
- **Load and long-running:** many VDO-backed volumes on one node, and sustained
  write throughput. Only a single-node memory ceiling is measured so far.

The risk concentrates in §7.3 and §7.4, where a wrong device scope or an
unresolved clone identity destroys data rather than failing a mount, and in §8,
where the failure paths are the product.

---

## 12. Compatibility with Existing CSI Driver and Operator Features

This repository's CSI RPCs, StorageClass parameters, CRDs, and named subsystems
were cross-checked against this design. Most are unaffected for one structural
reason: VDO is a node-local block device on the client side of the NVMe-oF
connection, and a server-side or connection-layer feature never sees it.

| Feature                                                                             | Interaction                                                                                                                                                                            |
|-------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `CreateVolume`, `DeleteVolume`, `ControllerGetVolume`, `ValidateVolumeCapabilities` | Server-side lvol lifecycle, unaffected. VDO is purely a node-side addition                                                                                                             |
| `ControllerExpandVolume`                                                            | Feeds `NodeExpandVolume` the size it already feeds it, and §9's `lvextend` chain consumes it                                                                                           |
| `replicate`, `distr_ndcs` and `distr_npcs`, `tune2fsReservedBlocks`                 | Server-side or filesystem-level concerns, orthogonal to a client-side device stack                                                                                                     |
| Multi-cluster zone and region routing, DHCHAP, failure domains                      | Connection-layer concerns. DHCHAP's own `allowedTopologies` gate composes with §5's                                                                                                    |
| Node drain and recycle, the rebalancer                                              | Operate on storage nodes, never on the consumer node where VDO lives                                                                                                                   |
| RBAC tenancy                                                                        | Unchanged. §10 covers the one new grant                                                                                                                                                |
| Static PVC support                                                                  | Works, and a static PV sets `client_compression` in its `volumeAttributes` by hand                                                                                                     |
| The placement webhook and volume placement injector                                 | `SimplyblockVolumePlacementInjector` selects which storage node hosts the lvol through a `host_id` hint. It never touches pod scheduling, so it cannot conflict with the topology gate |

Three features needed more than a structural argument.

**Guardian.** `MarkBrokenLvol` in `csi-driver/pkg/util/guardian.go` is
Kubernetes-level bookkeeping: it marks state and later deletes the pod to force
a fresh stage and publish cycle, and it never touches the device, mount, or dm
layer. The device-level repair happens in `restageVolume` and
`ensureDeviceConnected`, which §7.6 already extends to reactivate the stack, so
Guardian needs no VDO awareness.

**VolumeMigration.** A migration moves an lvol between storage nodes, and
`dm-vdo` lives on the consumer node on top of the NVMe-oF client connection, so
a migration re-points the underlying path and leaves the VDO device and its
mount untouched. `CreateOrAttachVDO` is never re-invoked by a migration, and the
target node's `vdo-capable` status is therefore irrelevant: the
`VolumeMigration` controller carries no `vdo-capable` reference and needs none.
A volume sharing a subsystem namespace with its clones cannot be migrated
individually, because the backend enforces atomic group migration of the whole
subsystem. That needs no client-side design work: once `ResolveClone` has given
each clone its own PV, VG, and LV identity (§7.4), each volume's reconnect logic
operates independently of the others.

**Encryption.** An encrypted volume reaches the NVMe-oF client as plaintext, so
`encryption=true` composes with `client_compression=true` without defeating it.
In `sbcli`'s `simplyblock_core/controllers/lvol_controller.py`, enabling
encryption layers a crypto vbdev on top of the base lvol and reassigns
`lvol.top_bdev` to it, and the NVMf namespace-add RPC always exposes
`lvol.top_bdev`. The crypto bdev therefore sits below the NVMf attach point, and
every read is decrypted server-side before the bytes reach the wire. The DEK is
fetched from a server-side KMS and installed on the storage node, and no key
material transits the CSI path: `csi-driver/pkg/util/nvmf.go` and
`controllerserver.go` pass a boolean flag. Encryption here is at-rest only,
which matters because ciphertext does not compress or deduplicate at all, as the
test plan's measurement records.

---

## 13. Observability

The CSI node plugin reports through klog today and has no Prometheus registry of
its own, so the events below are what this design owes a field engineer. Each
one marks a decision that is otherwise invisible: a volume that will not stage,
or a node that quietly stopped being eligible. Guardian's
`emitSharedSubsystemEvent` is the pattern, writing an event through the
clientset rather than a controller-runtime recorder.

### Kubernetes Events

| Event                                                              | Type    | Reason              |
|--------------------------------------------------------------------|---------|---------------------|
| A stage failed because the node cannot build a VDO stack           | Warning | `VDOStageFailed`    |
| A node's capability probe flipped the label to `false`             | Warning | `VDONotCapable`     |
| An operator-set `vdo-capable` label was left in place by the probe | Normal  | `VDOLabelRespected` |
| A clone or restore had its LVM identity re-stamped at first stage  | Normal  | `VDOCloneResolved`  |
| An orphaned volume group was removed and the stack recreated       | Warning | `VDOStackRepaired`  |

### Prometheus Metrics

| Metric                                        | Labels           | Description                                               |
|-----------------------------------------------|------------------|-----------------------------------------------------------|
| `simplyblock_csi_vdo_volumes`                 | `node`           | VDO stacks currently active on the node                   |
| `simplyblock_csi_vdo_stage_failures_total`    | `node`, `reason` | Stages that failed with VDO in play, by classified reason |
| `simplyblock_csi_vdo_clone_resolutions_total` | `node`           | Clone identities re-stamped at first stage                |
| `simplyblock_csi_vdo_orphan_repairs_total`    | `node`, `kind`   | Orphaned volume groups and dm nodes cleaned up, by kind   |

Neither table is implemented. The CSI driver has no metrics endpoint at all
today, and adding one is a larger change than this feature (§14, Q6). Until it
exists, every row above is a klog line, which is why §14 tracks it rather than
§11 asserting coverage for it.

---

## 14. Open Questions

| #   | Question                                                                                                                                                                                                                                                              | Owner         |
|-----|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------|
| Q1  | **Toggling features on a live volume:** `SetFeatures` exists and works, and nothing calls it. Whether a `clientCompression` change on an existing pool should reach staged volumes, through a VolumeAttributesClass or a restage, is undecided                        | Operator team |
| Q2  | **Enforcing VDO's size floor:** a PVC below roughly 4.72GiB fails inside `lvcreate`. Whether the webhook should reject it at admission, and where the floor is expressed so it survives a VDO change, is undecided. Growth from exactly at the floor is also untested | Operator team |
| Q3  | **In-tree `dm-vdo` detection:** §4.1 specifies `modprobe dm-vdo` before the install path, and the probe implements only the legacy path. Landing it removes the BaseOS dependency on kernels 6.9 and newer                                                            | Operator team |
| Q4  | **Permanently failed nodes:** an RWO volume stays attached to a dead node until an administrator applies `node.kubernetes.io/out-of-service` (P0-8). Whether this operator should apply that taint, and on what evidence a node is confirmed dead, is undecided       | Operator team |
| Q5  | **`system.devices` hygiene:** nothing prunes a stale entry after its device disappears, so a node accumulates one per failure cycle. Whether the node plugin should run `lvmdevices --deldev` at unstage is undecided (§8)                                            | Operator team |
| Q6  | **CSI-side metrics:** §13's metrics need a Prometheus endpoint the CSI driver does not have. Whether to add one for this feature or wait for a driver-wide decision is open                                                                                           | Operator team |
| Q7  | **`lvm2` VDO segtype detection:** the capability probe checks the kernel module and not whether `lvm segtypes` lists `vdo`. Whether the segtype check belongs alongside `modprobe` is undecided                                                                       | Operator team |
| Q8  | **Non-RHEL and `aarch64` nodes:** both are non-goals today (§2), and P0-4 makes `aarch64` a packaging problem rather than a design one. Whether either becomes supported depends on demand                                                                            | Product       |
| Q9  | ~~**Where the VDO and LVM primitives live**~~ **Resolved.** They are `atlas-lib` packages, `lvm` and `lvm/vdo`, and the CSI driver holds only the wrappers in `csi-driver/pkg/util/vdo.go` (§7.1)                                                                     | Resolved      |
