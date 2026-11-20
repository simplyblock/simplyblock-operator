# Design Document: Client-Side Compression and Deduplication

**Status:** Partially Implemented  
**Author:** Manohar Reddy  
**Date:** 2026-08-06 (last updated 2026-09-21)  
**Issue:** https://github.com/simplyblock/simplyblock-operator/issues/277  
**Test Plan:** [`tests/test-plan-issue-277-client-side-compression.md`](../tests/test-plan-issue-277-client-side-compression.md)

**2026-09-21 rewrite:** the mechanism below is a row of
[`atlas-lib/volstack`](design-node-volume-stack.md)'s plan catalog rather than a
stack of its own. Every node RPC on the data path is one runner call against a
plan, for every volume this driver stages, so a volume carrying either
client-side parameter differs from every other volume in which layers its plan
names and in nothing else (§7.1, §7.6). The earlier rewrite of 2026-09-16
described the three LVM layers driven through an adapter standing in for
`fabric`, because the fabric and filesystem layers were unwired at the time;
both are wired now, and the adapter is gone. `atlas-lib/lvm/vdo` retired with
neither (§7.1). The topology gate no longer
proposes `AllowedTopologies` on the generated StorageClass (§5): DHCHAP's own
copy of that mechanism was found broken and removed in PR #484, for a reason
that applies identically here, and `vdoCapableSegment` mirrors DHCHAP's
*current* fix instead. §4.1's `hostPID` prerequisite is dropped: the existing
`nvme-tcp`/`nvme-rdma` `modprobe` in the node plugin's `postStart` hook
already proves a plain `modprobe` from this privileged container reaches the
host kernel without it. File paths throughout reflect the post-#497
`csi-driver/internal/csi/{node,controller}` layout and the post-#513
`SimplyblockDriver`-managed DaemonSet (`operator/internal/controllers/driver`),
neither of which existed when this design was first written.

---

## Phase 0 — External Prerequisites

Client-side compression depends on a kernel module, a packaging path, and a
capacity floor that this repository does not build. All of them are collected
here, and each row names the section that specifies it.

| #        | Prerequisite                                                                                                                                                                                                                                                      | Kind                | Blocks                                                      | Status                                                       |
|----------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|-------------------------------------------------------------|--------------------------------------------------------------|
| ~~P0-1~~ | ~~`kmod-kvdo` and `vdo` reachable from every node's BaseOS repository~~ **No longer needed.** §4.1/Q11 dropped the install path. Capability is limited to kernels that already carry `dm-vdo` in-tree                                                             | —                   | —                                                           | —                                                            |
| P0-2     | In-tree `dm-vdo`, kernel 6.9 or newer                                                                                                                                                                                                                             | Node OS             | Every volume this feature serves (§4.1)                     | Available on a current-enough kernel, detected by `modprobe` |
| P0-3     | `lvm2` with the `vdo` and `vdo-pool` segment types, on the node and in the CSI node image                                                                                                                                                                         | Node OS             | Every VDO stack operation (§7)                              | Available, installed into the CSI node image (§4.2)          |
| P0-4     | `vdoformat`, which `lvcreate --type vdo` invokes internally, in the CSI node image                                                                                                                                                                                | Node OS             | Volume creation (§7.2)                                      | Available on `x86_64` only, no `aarch64` build exists        |
| ~~P0-5~~ | ~~RHEL-family packaging: `dnf`, `rpm`, and the weak-modules mechanism~~ **No longer needed.** No install step remains that needs it                                                                                                                               | —                   | —                                                           | —                                                            |
| ~~P0-6~~ | ~~`hostPID` on the `csi-node` DaemonSet, so `nsenter` reaches the host mount namespace~~ **No longer needed.** §4.1 dropped `nsenter`: the same `SYS_MODULE`/`/lib/modules` access that already loads `nvme-tcp`/`nvme-rdma` without `hostPID` loads `dm-vdo` too | —                   | —                                                           | —                                                            |
| P0-7     | A volume of at least 5GiB, above VDO's own floor of roughly 4.72GiB                                                                                                                                                                                               | Storage plane (VDO) | Any compressed or deduplicated volume (§6)                  | Enforced by VDO itself, with no operator-side pre-check      |
| P0-8     | The `node.kubernetes.io/out-of-service` taint, Kubernetes 1.24 or newer                                                                                                                                                                                           | Kubernetes          | Recovery of an RWO volume from a permanently dead node (§8) | Available in-cluster, not invoked by this design             |

Each missing item has a different consequence. Without P0-2 a node never
becomes VDO-capable, so the topology gate (§5) keeps compressed volumes off
it and the rest of the cluster is unaffected. Without P0-3 or P0-4 the node
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
    - [4.1 Module Load and Detection](#41-module-load-and-detection)
    - [4.2 Container Image Dependencies](#42-container-image-dependencies)
    - [4.3 Capability Advertisement](#43-capability-advertisement)
  - [5. Scheduling Gate: Topology](#5-scheduling-gate-topology)
    - [5.1 Reporting an Unsatisfiable Pool](#51-reporting-an-unsatisfiable-pool)
  - [6. StorageClass Parameter and CRD Changes](#6-storageclass-parameter-and-crd-changes)
  - [7. VDO Device Management](#7-vdo-device-management)
    - [7.1 Package Placement](#71-package-placement)
    - [7.2 Device Creation](#72-device-creation)
    - [7.3 Device Identity in HA Mode](#73-device-identity-in-ha-mode)
    - [7.4 Clone and Snapshot Restore](#74-clone-and-snapshot-restore)
    - [7.5 Write Policy](#75-write-policy)
    - [7.6 Wiring into the node service](#76-wiring-into-the-node-service)
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

VDO cannot be assumed present on a node. `dm-vdo`, the device-mapper target
VDO is exposed as, merged into the mainline kernel at 6.9. A node running an
older kernel has no path to it at all, and this design does not attempt one
(§4.1, Q11). A node that was capable before a kernel downgrade or a swap to
an older image is not necessarily capable after one, though a kernel only
moves backward by administrator action rather than by a routine update.

VDO is therefore treated as an opt-in, per-node capability: checked
explicitly, advertised explicitly, and gated on for scheduling.

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
- Name the cause when no node in the cluster can serve a pool at all, rather
  than leaving every PVC from that pool `Pending` behind a scheduler message
  that does not mention VDO.

### Non-Goals

- Changing the existing server-side `compression` feature.
- Sharing one VDO instance across several volumes. This design creates one VDO
  instance per volume, directly on that volume's raw device.
- Support for a node running a kernel older than 6.9. There is no install
  path to fall back to (§4.1, Q11), so such a node is gated out by §5 rather
  than failing a volume, and the hand-set label of §4.3 is its only route to
  capability. This bound applies identically regardless of node operating
  system, since the probe needs nothing distribution-specific.
- **`aarch64` nodes, for this design's experimental status only.** Not
  dropped: it is a goal for the feature's final release (§14, Q8), not a
  permanent exclusion. For now, the `vdo` package that provides `vdoformat`
  has no `aarch64` build (P0-4), so the feature is `x86_64`-only until that
  packaging gap closes. Non-RHEL node operating systems carry no separate
  gap of their own to close: dropping the install path (Q11) means nothing
  here depends on a RHEL-family concept any longer.

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
     │   storage.simplyblock.io/vdo-capable=true         │
     └───────────────────────────────────────────────────┘
       │
       ▼
┌──────────────────────────────────────────────────────────────────────┐
│ simplyblock-csi-node DaemonSet, one pod per node                     │
│                                                                      │
│  1. advertiseVDOCapability: modprobe dm-vdo, else kvdo, and log     │
│     the kernel, each module's answer, and lvm's segment types        │
│  2. the same call patches the node label with the verdict            │
│  3. NodeGetInfo -> buildAccessibleTopology: label to CSI segment     │
│  4. CreateVolume -> vdoCapableSegment: PV nodeAffinity               │
│  5. NodeStageVolume: plan.go selects the LVM row, and the runner     │
│     walks it: fabric, the three LVM layers, then the filesystem      │
│                                                                      │
│  Node label storage.simplyblock.io/vdo-capable and its annotation    │
└──────────────────────────────────────────────────────────────────────┘
              │ imports (node-level primitives, no Kubernetes awareness)
┌─────────────▼────────────────────────────────────────────────────────┐
│ atlas-lib: .../atlas/lvm, .../atlas/volstack/{layers,plans}          │
│   lvm.Manager:   device-scoped LVM commands, content-based identity  │
│   lvm's vdo:     the VolumeProvisioning handler lvcreate's VDO       │
│                  arguments come from                                 │
│   volstack:      the layers, the plan rows, and the runner that      │
│                  walks them                                          │
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

### 4.1 Module Load and Detection

**Revised 2026-09-22: the probe is the node plugin's, not a lifecycle hook's.**
It began in the `csi-node` container's `postStart` hook, beside the `nvme-tcp`
and `nvme-rdma` loads already there, exchanging its answer with the plugin
through a marker file on a host path. The hook was the wrong place: its output
reaches neither the container's log stream nor anywhere else a reader can get at
it, because Kubernetes surfaces a lifecycle hook's output only as an event when
the hook *fails*. A probe that ran and answered no therefore left no trace of
having run, which is precisely the case an operator needs to see — a node that
cannot run VDO and a node whose probe never ran leave the cluster looking
identical, with every volume needing the capability unschedulable and nothing
anywhere having failed.

The plugin runs in that same privileged container, holding `SYS_MODULE` and
mounting the host's `/lib/modules` read-only, which is what a module load
actually needs: loading affects the whole kernel regardless of which namespace
the loading process sits in, so no `nsenter` and no `hostPID` are involved. It
asks the kernel directly, in `csi-driver/internal/csi/node/capability.go`, and
the marker file and the host path that carried it are gone with the hook.

Two module names are tried, because "VDO in the kernel" means two different
things depending on the node's operating system: `dm-vdo` is upstream's in-tree
name (kernel 6.9 and newer), and `kvdo` is what RHEL-family systems still ship
it as through the separate `kmod-kvdo` package, verified live on a Rocky/RHEL 9
kernel carrying no `dm-vdo` at all. Either one loading means the node can run
VDO, so the second is tried only when the first does not load, and both answers
are reported either way.

There is no install step, per §14's Q11: the probe either finds a module or it
does not. This is a live question asked of the running kernel rather than a
version check, which is what lets the same probe work unchanged on any node
operating system, and nothing about the answer depends on how the node got its
kernel.

It runs once per plugin start, which is when the answer can have changed: the
DaemonSet is one pod per node, and a kernel change restarts that pod. A
capability gained without a restart is §14's Q12 and is not covered here.

**Measured on the e2e runners, 2026-09-22.** All seven nodes run RHEL 9.4 on
kernel `5.14.0-427.24.1.el9_4.x86_64`, which carries no in-tree `dm-vdo` — that
arrives in 6.9 — and has no `kmod-kvdo` installed, so both loads fail against
the host's own `/lib/modules`. `lvm segtypes` on the same nodes lists `vdo` and
`vdo-pool`: userspace LVM offers the segment types while the kernel cannot
provide them, which is the inverse of the case §14's Q7 anticipated and is why
the segment types are reported rather than trusted. Every node is therefore
labeled `false`, §5's gate refuses placement, and the feature's own E2E suite
skips rather than waiting out a pod that can never be scheduled. Exercising this
design on that pipeline needs those nodes to gain the module first.

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
`storage.simplyblock.io/vdo-capable=true` on success, and `false`, or the
label absent, on failure.

`advertiseVDOCapability` runs on every `csi-node` pod start, and a hand-set
label is the escape hatch a golden-image node depends on, so the probe has to
tell its own labels apart from an operator's.

The probe itself runs here too, rather than in the `postStart` hook §4.1
originally put it in, and the marker file the two exchanged it through is gone.
A hook's output reaches nothing a reader can get at: Kubernetes surfaces it only
as an event when the hook *fails*, so a probe that ran and answered no left no
trace of having run. That is the difference between a node that cannot run VDO
and a node whose probe never ran, and from outside the cluster the two are
identical: every volume needing the capability is unschedulable, and nothing
anywhere has failed. The plugin is in the same privileged container over the
same `/lib/modules`, so it asks the kernel directly and logs what it was told.

What it logs is the whole probe and not only its verdict: the kernel version,
each module it tried and what `modprobe` said about that module by name, and
whether `lvm segtypes` lists the vdo types. The segment types are recorded and
not acted on, which is what will settle §14's Q7: a node whose module loads
while LVM offers no vdo segtype is exactly the case that question is about, and
nothing before this would have shown it. Every label value the probe
writes itself is stamped with a second annotation,
`storage.simplyblock.io/vdo-capable-managed-by: auto-detect`. On startup the
probe first checks whether the label is already present without that
annotation, and leaves such a label untouched. A label carrying the annotation,
or no label at all, is the probe's to manage.

The probe is triggered by pod start and by nothing else, and the two directions
of change are not covered equally. Capability lost is covered, because the event
that takes it away is a kernel change, which reboots the node and restarts the
pod, so the probe re-runs and flips the label to `false`. Capability gained
outside a restart is not: `lvm2` gaining the `vdo`/`vdo-pool` segment types
after the fact (P0-3) leaves the node labeled `false` until its `csi-node`
pod restarts, and until then §5's gate keeps volumes off a node that would
now serve them. Deleting the pod is the operator's lever, and the hand-set
label of the paragraph above is the other, since the probe leaves an
unannotated label alone. §14's Q12 resolves this by re-checking on an
interval rather than at pod start alone.

---

## 5. Scheduling Gate: Topology

**Revised 2026-09-16.** This section originally proposed a second copy of the
constraint on the generated StorageClass's `AllowedTopologies`, mirroring
DHCHAP's original mechanism. That mechanism was found broken for DHCHAP itself
and removed in PR #484: `AllowedTopologies` becomes external-provisioner's
`requisite` list, which is checked against the *selected* node's `CSINode`
object, and a `CSINode`'s topology keys are fixed once, at CSI plugin
registration. A label a `postStart` hook or an operator applies after that —
which is the case here whenever the probe has not finished by the time the
plugin registers — can never retroactively
join that set. Requiring it in the StorageClass's `AllowedTopologies` makes
every PVC from a client-side pool fail provisioning permanently, the exact
failure PR #484's `[[project_dhchap_sc_allowedtopologies_gap]]` reproduced live.
`vdoCapableSegment` mirrors DHCHAP's *current*, fixed mechanism instead, and
`buildAccessibleTopology` needs no `vdo-capable` awareness at all: it is
unrelated to the gate below.

The gate is a single PV `nodeAffinity` pin, not two copies of the constraint.
`vdoCapableSegment` (`csi-driver/internal/csi/controller/params.go`, alongside
`dhchapAllowedNodeSegment`) adds `storage.simplyblock.io/vdo-capable=true` to
the `CreateVolume` response's `AccessibleTopology` whenever either client
parameter is true, built straight from the StorageClass parameters and never
from `req.GetAccessibilityRequirements()` — the same reasoning
`dhchapAllowedNodeSegment`'s own comment gives. External-provisioner turns that
into `PersistentVolume.spec.nodeAffinity`, which holds for the volume's whole
life, independent of `CSINode` registration timing.

`VolumeBindingMode` is already `WaitForFirstConsumer`, but unlike the original
proposal, initial placement is *not* filtered at the scheduler's Filter stage:
a pod is tentatively assigned to any node, the volume is created there, and
only then does the PreBind plugin reject a wrong node on the PV's
`nodeAffinity` and reschedule — the same behavior DHCHAP now has, verified live
in `[[project_dhchap_sc_allowedtopologies_gap]]` to self-heal without wedging
an unpinned pod. The consequence, also verified there: a volume is created
before the placement failure surfaces, rather than never created at all. Not a
leak (deleting the PVC reclaims it), but capacity is consumed for a workload
that briefly cannot run, and the scheduler's message is less obvious than a
Filter-stage rejection would have been. A pod hard-pinned (`nodeSelector`) to a
non-capable node still wedges, as it would under any topology mechanism.

The gate covers initial placement only. A node whose capability regresses while
a volume is already bound there is §8's subject.

### 5.1 Reporting an Unsatisfiable Pool

**Not implemented**, unchanged from this design's original state: with no
capable node anywhere in the cluster, every PVC from a client-side pool
provisions, binds tentatively, and is rejected and rescheduled indefinitely
rather than failing cleanly, and nothing surfaces the cause. A StoragePool
reconciler check — counting nodes carrying `vdo-capable=true` and emitting a
Warning event at zero, mirroring the pattern already in place for
`InvalidClusterReference` — remains the natural fix and is not blocked on
anything above; it is out of scope for this change and stays owned by §14's
Q13.

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
  compression-only one does. `wantsVDO` in `csi-driver/internal/csi/node/plan.go`
  and `vdoCapableSegment` in `csi-driver/internal/csi/controller/params.go` both apply that rule, and both
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

Nothing in that stack is Kubernetes-shaped, so it lives in `atlas-lib`:

| Package                                        | Holds                                                                                                                                                                                                                   |
|------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `github.com/simplyblock/atlas/lvm`             | `lvm.Manager` (device-scoped LVM commands, content-based volume-group identity, dm node cleanup), and the built-in `vdo` `VolumeProvisioning` handler, registered by the package's own `init`                           |
| `github.com/simplyblock/atlas/volstack/layers` | `lvmPhysicalVolume`, `lvmVolumeGroup`, `lvmLogicalVolume`: the `Layer` contract's `Ensure`/`Release`/`Destroy`/`Grow`, generalized over every LVM-backed shape, not only VDO                                            |
| `github.com/simplyblock/atlas/volstack/plans`  | the rows themselves: `LVM` is `fabric` → those three → `filesystem`, and `LVMRawBlock` is the same row with its top layer absent. `LogicalVolumeOptions` carries the pool name, the definition, and the node capability |
| `csi-driver/internal/csi/node/plan.go`         | the selection alone: which row a volume is, read off its volume context and its volume capability                                                                                                                       |

There is no VDO-specific type on either side of that split. What separates a
VDO volume from a linear one is the contents of `LogicalVolumeOptions`: a
definition asking for compression, deduplication, or both, the name of the pool
`lvcreate --type vdo` creates alongside the logical volume, and the label a node
must carry for the volume to be staged there at all. The handler in
`atlas-lib/lvm` turns that definition into `lvcreate`'s arguments, and it is
registered by its own package's `init` rather than by an import the caller has
to remember: a forgotten import would not fail the build, it would create a
plain linear volume and drop the feature silently.

The pool's name reaches two layers rather than one. The logical-volume layer
needs it as `lvcreate`'s `<vg>/<pool>` target, and the physical-volume layer
needs it as a name to leave alone when it resolves a clone: a byte-level clone
carries its source's LVM metadata, the resolution renames the volume carrying
the source's name, and the pool is named the same in every volume's group
(§7.4).

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
`lvcreate --type vdo -n X` fails when `X` already exists, and VDO offers no
idempotency mechanism of its own, which is what each layer's `Observe` answers:
it probes with `pvs` and `lvs` and reports a state, and `Ensure` acts on that
state rather than re-deriving it. A stack that is present but not mapped is
reactivated with `vgchange -ay`, and a stack that is present but incomplete is
handled as §7.3 describes. Creating is what absent alone permits, and absent
means the device carries no volume group at all.

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
   against it succeeds while producing no mountable device, which is the state
   the layer reports as partial: the orphaned VG is removed and a fresh create
   follows.

The same content-based identity is what makes clone detection possible in §7.4,
and `Manager.RemoveOrphanedDMNodes` is what cleans up after a device disappears
without a clean unstage in §8.

### 7.4 Clone and Snapshot Restore

A PVC clone and a VolumeSnapshot restore are both block-level copy-on-write
copies at the storage layer rather than a reformat, so a clone of a VDO-backed
volume carries its source's on-disk LVM metadata verbatim: the same PV UUID, the
same VG UUID, and the same VG name. LVM identifies a PV and a VG by that on-disk
UUID, not by the CSI volume it logically belongs to. A clone is therefore
indistinguishable from its source, and the question "does a VG named after this
volume exist" is answered no while the VG on disk is named after the source. Two
failures follow from that answer: a fresh `lvcreate` over the cloned data, and a
name collision with the source once both devices are visible on one node. The
physical-volume layer is what prevents both, which is why the LVM rows have a
layer for a label at all: a clone is spotted by whose name is on the device, one
layer below the group that would otherwise be created over it.

The layer's `Observe` reports the device foreign, and its `Ensure` answers that
state by re-stamping the identity rather than creating anything. It settles the
question from the device's own on-disk content, which is why it does not depend
on whether the content-source fact survived into the volume context. The PV and
VG UUIDs are regenerated and the VG and its LV renamed to this volume's
identity, `vgimportclone` and `lvrename`, before any activation happens. VDO's
own pool LV is structural and named identically in every stack, so the plan
passes its name down as one to preserve, and exactly the LV carrying the
source's name is renamed. A genuinely fresh device, and one already resolved,
are both left alone.

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

### 7.6 Wiring into the node service

Nothing in the node service branches on VDO. Every node RPC on the data path is
one runner call against a plan, for every volume this driver stages, so what
either client-side parameter changes is which row `plan.go` selects and nothing
else:

- **`planFor`** returns the `LVM` row when the volume context carries either
  parameter, and `LVMRawBlock` when such a volume is opened as a block device.
  A volume carrying neither gets `Plain` or `RawBlock`, exactly as before.
- **`NodeStageVolume`** is `runner.Up` against that plan. The three LVM layers
  create the stack on a blank device and reactivate it on one that already
  carries it, which is the distinction their `Observe` exists to make: absence
  means the device carries no volume group at all, established by reading the
  device, and never inferred from this volume's group not being found.
- **`NodeUnstageVolume`** is `runner.Down`, which releases and never destroys,
  because it fires whenever no pod on this node needs the volume mounted, a
  routine pod delete and recreate included. `runner.Destroy` follows only for a
  volume that is being deleted.
- **`restageVolume` and `NodePublishVolume`** are `runner.Heal`, which repairs
  the layers reporting themselves unhealthy and creates nothing. That is the
  same "never reformat, the data already exists" invariant a restage has always
  held, expressed once in the runner instead of once per call site.
- **`NodeExpandVolume`** is `runner.Grow` (§9).
- **The teardown follows the stack record**, not the volume's class: the record
  names the layers that were built and what the logical-volume layer was built
  with, so a class edited or deleted after the volume was provisioned cannot
  point a release at a pool by another name.

**Format options.** A format on top of a VDO device skips `mkfs`'s full-device
discard (`-K` for `mkfs.xfs`, `-E nodiscard` for `mke2fs`), which VDO's block
map processes in proportion to the volume's size: confirmed live on both,
formatting the same 20G VDO volume, 11.5s against 0.13s for XFS and 12.08s
against 0.09s for ext4. A freshly created VDO volume has nothing on it worth
discarding.

The stripe hints XFS is otherwise given are omitted for the same volume, and
that omission is a property of the layer rather than a conditional: the hints
describe the erasure-coded backend, VDO virtualizes and relocates blocks, and a
layer whose blocks are virtualized reports the zero `Geometry`, which is what
`filesystem_strategy.go` derives the hint from. What remains a conditional in
the CSI driver is only the hints the volume *context* carries, which predate
`Geometry` and are still read for every volume that is not behind a VDO device.

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
either client parameter is present in the volume context and the
bring-up fails locally, both paths return a hard error, reported through the
same klog error paths as every other hard failure in that file. `runner.Up`
releases what it already brought up when a layer fails, and never destroys, so
a format that failed does not trigger the removal of the volume underneath
it.

| Failure                                             | Detection                                                       | Behavior                                                                                               |
|-----------------------------------------------------|-----------------------------------------------------------------|--------------------------------------------------------------------------------------------------------|
| No VDO-capable node anywhere in the cluster         | Capable-node count is zero at pool reconcile                    | A Warning event on the StoragePool names the count, and every PVC from the pool stays `Pending` (§5.1) |
| Node not VDO-capable at first binding               | Missing `vdo-capable` topology segment                          | The PVC stays `Pending`, and the volume is never bound to that node (§5)                               |
| Node capability regresses after binding             | The logical-volume layer fails at stage or restage              | Hard error from `NodeStageVolume` or `restageVolume`, never a raw mount                                |
| Volume smaller than VDO's floor                     | `lvcreate` exits 5, reporting the minimum size                  | Stage fails with that error surfaced, and no partial stack is left behind (§14, Q2)                    |
| Interrupted create, VG present with no LV           | `HasLogicalVolume` reports zero LVs                             | The orphaned VG is removed, and a fresh stack is created (§7.3)                                        |
| Clone or restore carrying its source's LVM identity | The physical-volume layer reads a foreign VG off the device     | `vgimportclone` re-stamps the identity, and the pool keeps its name (§7.4)                             |
| Backing device gone without a clean unstage         | `vgchange -an` fails with `Volume group … not found`            | Fallback to `dmsetup remove` of the live dm nodes, retried across passes                               |
| Node reboot with several VDO volumes                | Kubelet stages every volume afresh after a restart              | Each stack reattaches independently and idempotently                                                   |
| Node permanently dead, RWO volume attached          | `FailedAttachVolume: Multi-Attach error` on the replacement pod | The replacement pod stays blocked until an administrator applies the out-of-service taint (P0-8)       |
| Unclean node crash mid-write                        | VDO's journal thread hits a fatal I/O error                     | VDO fences itself read-only and the filesystem aborts its journal, without corrupting data             |

**Restart is a re-provision like any other.** Kubelet's own bookkeeping resets
on a reboot, so it calls `NodeStageVolume` afresh for every volume rather than
`restageVolume`, and each VDO instance reattaches independently.

**Stale state after a node loses a volume without a clean unstage.** This covers
a pod force-rescheduled off a node that went `NotReady`, and the storage side
disconnecting the initiator while the node stays up. The volume-group layer's
`vgchange -an` fails with `Volume group … not found`, because no device remains
to read the VG's metadata from, and the fallback is a direct `dmsetup remove` of
the live dm nodes rather than the normal LVM teardown path. The layers below it
answer that same situation independently, so a `Down` walk is not aborted by the
one layer that has nothing left to read: with no device beneath them, the
physical-volume and logical-volume layers report absent, and the volume-group
layer asks `dmsetup` whether the group is still mapped rather than asking a
device that is gone.
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
visibility to specific devices, and nothing prunes a stale entry on its own. The
volume-group layer removes the entry with `lvmdevices --deldev` on exactly the
release that had to fall back to the device-mapper cleanup above, which is the
release whose device is already gone and whose entry nothing else will ever
clean up. It is hygiene rather than correctness — the file gates which devices
LVM considers rather than causing a failure of its own — so a failure there is
logged and never fails the teardown a volume depends on.

**Crash consistency under `async`.** An acknowledged write survives an unclean
node crash, because flush and FUA durability carries end-to-end through NVMe-oF
to the simplyblock backend. A write that was never `fsync()`'d is lost, which is
the correct POSIX outcome. Nothing here depends on forcing the `sync` policy
(§7.5).

---

## 9. Volume Expansion

`NodeExpandVolume` is one `runner.Grow` against the volume's plan, bottom to
top, skipping the layers that cannot grow. There is no standalone `vdo
growPhysical` or `growLogical` command on this system, because VDO is managed
through LVM (§7.1), so the logical-volume layer's `Grow` is an `lvextend`
against the pool LV for physical space and then against the VDO LV for logical
size, and the filesystem layer above it resizes onto what that produced. No step
takes a target size: each consumes whatever the layer below now reports, which
is the same `100%FREE` convention creation uses, and is what makes the walk
convergent when kubelet reissues an expansion that already succeeded.

Growth is online: the physical extend, the logical extend, and the filesystem
resize all run while the filesystem stays mounted. Overhead is roughly fixed in
absolute terms rather than proportional, so a pool's `Data%` falls as it grows.
Growth from exactly at VDO's minimum-size floor is not covered (§14, Q2).

---

## 10. RBAC Changes

`operator/internal/controllers/driver/rbac.go`, which builds the roles the
operator applies to the plugins it deploys, grants the CSI node's ClusterRole
`patch` on `nodes`, alongside the pre-existing `get`, `list`, and `watch`. The capability-labeling step in §4.3
needs them to write the `storage.simplyblock.io/vdo-capable` label and its
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

**Guardian.** `MarkBrokenLvol` in `csi-driver/internal/guardian/guardian.go` is
Kubernetes-level bookkeeping: it marks state and later deletes the pod to force
a fresh stage and publish cycle, and it never touches the device, mount, or dm
layer. The device-level repair happens in `restageVolume` and
`ensureDeviceConnected`, which §7.6 already extends to reactivate the stack, so
Guardian needs no VDO awareness.

**VolumeMigration.** A migration moves an lvol between storage nodes, and
`dm-vdo` lives on the consumer node on top of the NVMe-oF client connection, so
a migration re-points the underlying path and leaves the VDO device and its
mount untouched. Nothing re-runs the volume's bring-up on a migration, and the
target node's `vdo-capable` status is therefore irrelevant: the
`VolumeMigration` controller carries no `vdo-capable` reference and needs none.
A volume sharing a subsystem namespace with its clones cannot be migrated
individually, because the backend enforces atomic group migration of the whole
subsystem. That needs no client-side design work: once the clone resolution has given
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
material transits the CSI path: `csi-driver/internal/csi/controller/volume.go`
passes a boolean flag. Encryption here is at-rest only,
which matters because ciphertext does not compress or deduplicate at all, as the
test plan's measurement records.

---

## 13. Observability

The CSI node plugin reports through klog today and has no Prometheus registry of
its own, so the events below are what this design owes a field engineer. Each
one marks a decision that is otherwise invisible: a volume that will not stage,
a node that quietly stopped being eligible, or a pool nothing in the cluster can
serve.

Two emitters, on two objects. The node plugin's events land on the Pod whose
volume is affected, or on the Node whose capability changed, written through the
clientset the way Guardian's `emitSharedSubsystemEvent` writes them rather than
through a controller-runtime recorder. The pool event is the operator's and
lands on the StoragePool, through the `events.EventRecorder` the StoragePool
reconciler already carries (§5.1).

### Kubernetes Events

| Event                                                              | Object      | Type    | Reason                 |
|--------------------------------------------------------------------|-------------|---------|------------------------|
| A stage failed because the node cannot build a VDO stack           | Pod         | Warning | `VDOStageFailed`       |
| A node's capability probe flipped the label to `false`             | Node        | Warning | `VDONotCapable`        |
| An operator-set `vdo-capable` label was left in place by the probe | Node        | Normal  | `VDOLabelRespected`    |
| A clone or restore had its LVM identity re-stamped at first stage  | Pod         | Normal  | `VDOCloneResolved`     |
| An orphaned volume group was removed and the stack recreated       | Pod         | Warning | `VDOStackRepaired`     |
| No node in the cluster can serve a pool's client-side parameters   | StoragePool | Warning | `VDOPoolUnsatisfiable` |

### Prometheus Metrics

| Metric                                        | Labels           | Description                                               |
|-----------------------------------------------|------------------|-----------------------------------------------------------|
| `simplyblock_csi_vdo_volumes`                 | `node`           | VDO stacks currently active on the node                   |
| `simplyblock_csi_vdo_stage_failures_total`    | `node`, `reason` | Stages that failed with VDO in play, by classified reason |
| `simplyblock_csi_vdo_clone_resolutions_total` | `node`           | Clone identities re-stamped at first stage                |
| `simplyblock_csi_vdo_orphan_repairs_total`    | `node`, `kind`   | Orphaned volume groups and dm nodes cleaned up, by kind   |

Neither table is implemented, for two different reasons. The metrics need a
Prometheus endpoint the CSI driver does not have at all, and adding one is a
larger change than this feature (§14, Q6). The events need only their emitters:
the node-plugin rows are klog lines today, and the pool row waits on §5.1's
capable-node count (§14, Q13). That is why §14 tracks all of it rather than §11
asserting coverage for it.

---

## 14. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | Owner         |
|-----|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------|
| Q1  | **Toggling features on a live volume:** `lvchange --compression <y\|n> --deduplication <y\|n>` against the pool LV is a live attribute change LVM applies without recreating or reformatting anything, and nothing in the layers runs it: a volume's definition is read when it is created and never again. What is undecided is both whether a `clientCompression` change on an existing pool should reach staged volumes automatically, through a `VolumeAttributesClass` or a restage, and which layer verb would carry it                                                                                                                                                                                                                                                                                   | Operator team |
| Q2  | ~~**Enforcing VDO's size floor:** a PVC below roughly 4.72GiB fails inside `lvcreate`. Whether the webhook should reject it at admission, and where the floor is expressed so it survives a VDO change, is undecided. Growth from exactly at the floor is also untested~~ **Resolved.** A new admission webhook checks PVC size at creation time and rejects a request below the floor whenever `clientCompression` or `clientDeduplication` is on, so the error shows up immediately instead of later as an `lvcreate` failure. This is a new webhook on PVC admission, separate from the existing `StoragePoolValidator`, which only checks StoragePool objects. Growth from exactly at the floor is still untested                                                                                           | Operator team |
| Q3  | ~~**In-tree `dm-vdo` detection:** §4.1 specifies `modprobe dm-vdo` before the install path, and the probe implements only the legacy path. Landing it removes the BaseOS dependency on kernels 6.9 and newer~~ **Absorbed by Q11.** There is no install path or legacy path left to choose between: `modprobe dm-vdo` is the whole probe (§4.1)                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | Resolved      |
| Q4  | ~~**Permanently failed nodes:** an RWO volume stays attached to a dead node until an administrator applies `node.kubernetes.io/out-of-service` (P0-8). Whether this operator should apply that taint, and on what evidence a node is confirmed dead, is undecided~~ **Resolved.** The operator does not apply the taint. Confirming a node is permanently, rather than transiently, dead is a judgment this design has no evidence to make safely, so §8's behavior stands: the replacement pod stays blocked until an administrator applies it by hand                                                                                                                                                                                                                                                         | Resolved      |
| Q5  | ~~**`system.devices` hygiene:** nothing prunes a stale entry after its device disappears, so a node accumulates one per failure cycle~~ **Resolved.** `lvm.Manager.ForgetDevice` runs `lvmdevices --deldev`, and the volume-group layer calls it on the release that had to fall back to the device-mapper cleanup, which is the only release whose entry nothing else will ever clean up. Best-effort: a failure is logged and never fails the teardown (§8)                                                                                                                                                                                                                                                                                                                                                   | Resolved      |
| Q6  | **CSI-side metrics:** §13's metrics need a Prometheus endpoint the CSI driver does not have. `csilink` (`operator/internal/csilink`, `csi-driver/internal/csilink`), the operator↔CSI-driver reverse RPC channel, already exists on `main` and is a route to the operator for this data that needs no CSI-side Prometheus endpoint at all. Whether to build the metrics over it now, wait for a driver-wide Prometheus decision, or do both is still open                                                                                                                                                                                                                                                                                                                                                       | Operator team |
| Q7  | **`lvm2` VDO segtype detection:** the capability probe checks the kernel module and not whether `lvm segtypes` lists `vdo`. Whether the segtype check belongs alongside `modprobe` is undecided                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | Operator team |
| Q8  | ~~**Non-RHEL and `aarch64` nodes:** both are non-goals today (§2), and P0-4 makes `aarch64` a packaging problem rather than a design one. Whether either becomes supported depends on demand~~ **Resolved, and partly for free.** `aarch64` is a goal for the feature's final release, not an experimental-only exclusion, and still needs P0-4's packaging gap (no `aarch64` `vdo` build) closed, which stays separate follow-up work. Non-RHEL is no longer a separate gap at all: Q11 dropped the RHEL-only install path, so nothing left in this design depends on a RHEL-family concept, and a non-RHEL node with an in-tree-capable kernel already works                                                                                                                                                  | Resolved      |
| Q10 | ~~**Where the install runs:** the module install rides the `csi-node` `postStart` hook (§4.1), which puts `dnf` in the node plugin's readiness path. Whether a short-lived pod, started when a node needs it, should own the install instead is undecided~~ **Superseded by Q11.** A separate installer pod was the right answer to a slow `dnf install` blocking node readiness, but Q11 removed `dnf` from this design entirely: capability is now a single `modprobe dm-vdo` probe, which answers in milliseconds. Nothing about it is slow enough to justify moving it out of the `postStart` hook, so it stays there (§4.1)                                                                                                                                                                                | Resolved      |
| Q11 | ~~**Which kernel the target distributions ship:** whether the install path or the in-tree `dm-vdo` path is the common case depends on the default kernel of each supported node OS, and OpenShift, Rancher, and K3s have not been surveyed against the 6.9 line (§4.1)~~ **Resolved.** No survey needed: the design drops the `dnf install kmod-kvdo` path entirely and limits capability to kernels that already have in-tree `dm-vdo` (6.9+). The operator never has to know a node's kernel version ahead of time, since detection is a live probe rather than a version check: try `modprobe dm-vdo`, and label the node `vdo-capable=true` on success, `false` on failure. This absorbs Q3, which specified this same probe order as a fallback in front of the install path that no longer exists here    | Resolved      |
| Q12 | ~~**Re-checking capability:** the probe runs at `csi-node` pod start and never again, so a node that becomes capable without a restart stays labeled `false`, and an install slower than the probe's five-minute wait leaves that label behind durably (§4.3). Whether the probe should re-check on an interval, and at what cost in API writes, is undecided~~ **Resolved.** The probe must re-check: a pod-start-only probe leaves a node stuck at `false` forever whenever capability changes without a restart, such as `lvm2` gaining the `vdo`/`vdo-pool` segment types after the fact (§4.3). Dropping the install path (Q11) removes the slow-install variant of this failure, but not the underlying gap. What remains open is only the interval and its API-write cost, not whether to recheck at all | Operator team |
| Q13 | **Reporting an unsatisfiable pool:** §5.1's capable-node count and its `VDOPoolUnsatisfiable` event are not implemented. Whether the pool's view refreshes through a node watch rather than the reconciler's requeue, and whether the fact earns a durable status field of its own, is undecided                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                | Operator team |
| Q9  | ~~**Where the VDO and LVM primitives live**~~ **Resolved, and revised 2026-09-21.** `atlas-lib/lvm` (device-scoped commands and the built-in `vdo` provisioning handler), `atlas-lib/volstack/layers` (the three LVM layers), and `atlas-lib/volstack/plans` (the rows they compose into), not the flat `atlas-lib/lvm/vdo` package this row originally named, which was deleted with zero importers. The CSI driver holds the selection alone, in `csi-driver/internal/csi/node/plan.go` (§7.1)                                                                                                                                                                                                                                                                                                                | Resolved      |
