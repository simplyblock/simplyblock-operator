# Design Document: S3 Bucket Provisioning

**Status:** Draft  
**Author:** Max Schettler  
**Date:** 2026-10-12 (last updated 2026-10-14)  
**Issue:** none yet — no GitHub issue has been filed for this design  
**Test Plan:** [`tests/test-plan-bucket-provisioning.md`](../tests/test-plan-bucket-provisioning.md)  
**Work Plan:** [`tasks/work-plan-bucket-provisioning.md`](../tasks/work-plan-bucket-provisioning.md)

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                           | Kind                                               | Blocks                                                                              | Status                                                                                                                                                                                                                                                                                    |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------|-------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| P0-1 | The pNFS RWX path (`NFSExport` CRD, MDS pod, `fstype: pnfs` StorageClass support), per `design-pnfs-rwx.md` and `design-pnfs-mds-vm.md`                | Ecosystem (this repo)                              | Everything in this design, which provisions no storage of its own                   | Deployed on at least one development cluster; not present on every cluster (confirmed absent on one shared cluster checked during this design's research)                                                                                                                                 |
| P0-2 | The mount-propagation mechanism in §5 actually delivers a bucket's mount into an already-running gateway pod without a restart                         | Ecosystem (this repo)                              | The entire mount-delivery design in §5. Every other section depends on this holding | Verified on a live cluster ([`0004-direct-csi-mount-spike-result.md`](../../../../docs/adr/0004-direct-csi-mount-spike-result.md)): a `NodePublishVolume` bind mount under the node plugin's own `hostPath` propagates into an already-running pod via `HostToContainer`, with no restart |
| P0-3 | The `ObjectStore` CRD that provisions a gateway's Deployment, Service, and TLS, and whose fixed pod label this design relies on for node-set discovery | Ecosystem (this repo)                              | §5's node-set discovery                                                             | Designed (`design-objectstore.md`), not yet implemented. Referenced here, not specified here (§2)                                                                                                                                                                                         |
| P0-4 | The S3 gateway binary's `CreateBucket` handler, which creates the `S3Bucket` object this design reconciles                                             | Ecosystem (separate repository, out of scope here) | The only trigger for this design's reconciler                                       | Not built. This design's reconciler is reachable by any client of the Kubernetes API, not only that handler                                                                                                                                                                               |

Without P0-1, an `S3Bucket` waits in `Pending` indefinitely with an event naming the cause: there is no StorageClass to provision against. P0-2 is now resolved (`0004-direct-csi-mount-spike-result.md`): a `NodePublishVolume` bind mount, placed under the CSI node plugin's own `hostPath` subtree, does reach an already-running gateway pod through `HostToContainer` propagation, with no restart. P0-3's absence does not block this design's CRD or reconciler from existing. It blocks the node-set discovery in §5.3 from having a Deployment to list pods against, so that section is written against the label convention `design-objectstore.md` specifies rather than against code that exists yet.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API Design — New CRD](#4-api-design--new-crd)
5. [Mount Delivery to Gateway Pods](#5-mount-delivery-to-gateway-pods)
6. [State Machine](#6-state-machine)
7. [Controller Design](#7-controller-design)
8. [Backend API Requirements](#8-backend-api-requirements)
9. [Failure Modes and Fallback](#9-failure-modes-and-fallback)
10. [Observability](#10-observability)
11. [Testing Strategy](#11-testing-strategy)
12. [Open Questions](#12-open-questions)

Appendices:

- [Appendix A: `s3bucket_types.go`](#appendix-a-s3bucket_typesgo)

---

## Overview

An `S3Bucket` is the desired existence of one S3 bucket's backing storage: a `PersistentVolumeClaim` on the `pnfs` path (`design-pnfs-rwx.md`), delivered to every node currently running a gateway pod of the `S3Bucket`'s `ObjectStore` by the CSI node plugin itself. A new `DeliverBucket` RPC, registered on the `csi-link` session the plugin already holds open to the operator (`design-pnfs-rwx.md`'s NFSExport assembly uses the same channel), tells it to call its own `NodeStageVolume`/`NodePublishVolume` in-process, the same two RPCs kubelet would otherwise issue, because the gateway pods themselves never reference the PVC directly.

The gateway pods instead carry one static `hostPath` volume, set once at pod creation and never touched again, with `mountPropagation: HostToContainer`. The `NodePublishVolume` call `DeliverBucket` triggers places its `target_path` under that same host path, and propagation delivers it into every gateway pod on the node without restarting any of them. A bucket's lifecycle therefore never touches a gateway pod's spec: creating bucket eleven does not restart gateway pods one through ten.

A reader who stops here should understand the one load-bearing fact this design turns on: bucket creation and gateway pod lifecycle are independent because mount propagation behaves this way for a CSI-staged pNFS mount, confirmed on a live cluster (§5.4, Phase 0 P0-2, `0004-direct-csi-mount-spike-result.md`).

---

## 1. Background

The S3 gateway (a separate repository, importing `versitygw`'s `embedgw` package) runs as a stateless Kubernetes Deployment. Each bucket it serves is backed by its own simplyblock logical volume rather than a filesystem shared across all buckets, exposed to the gateway process through `versitygw`'s `--bucketlinks` option: a bucket's top-level directory entry is a symlink to that volume's mount point. This gives each bucket independent QoS, snapshots, capacity, and disaster-recovery grouping, which a shared filesystem cannot.

Provisioning the volume itself is not new work. `design-pnfs-rwx.md` §9.3 already specifies that a `PersistentVolumeClaim` against a StorageClass naming `csi.storage.k8s.io/fstype: pnfs` causes the CSI controller to create the backing lvol, create an `NFSExport` CR for it, and block until the operator's existing `NFSExportReconciler` drives that export to `Ready` — binding the storage cluster's single MDS pod (`design-pnfs-mds-vm.md`; one MDS pod serves every export of a storage cluster, created lazily on the first one, so only the first bucket a cluster ever provisions pays a guest-boot cost) and assembling the export (attach the namespace, `mkfs.xfs` if unformatted, mount, `exportfs`). None of that is this design's to build.

What does not exist is a way to get that volume's mount in front of a fleet of already-running, stateless gateway pods without editing their pod spec, and a Kubernetes-level trigger, independent of the gateway binary, that a bucket should exist at all. This design is those two things: the `S3Bucket` CRD and its reconciler, and the mount-delivery mechanism in §5.

---

## 2. Goals and Non-Goals

### Goals

- Represent one S3 bucket's backing volume as Kubernetes desired state (`S3Bucket`), reconciling it the same way every other volume-backed resource in this operator converges.
- Deliver that volume's mount into every gateway pod of the bucket's `ObjectStore`, on every node one runs on, without ever modifying a gateway pod's spec or restarting it for a bucket lifecycle event.
- Keep new node-side privilege at zero. No gateway pod gains `SYS_ADMIN`/`SYS_MODULE` or any mount capability, and no new component is added to the node at all: the only thing that mounts anything is the existing, unmodified CSI node plugin, reached over the `csi-link` session it already holds open.
- Make bucket creation synchronous from the gateway's point of view: a client can create an `S3Bucket`, watch its `status.phase`, and act on `Ready` or `Degraded` without polling heuristics.
- Survive an operator restart mid-provisioning and a gateway pod being rescheduled to a different node, converging the per-node delivery set to match either without manual intervention.

### Non-Goals

- **The `ObjectStore` CRD itself:** the gateway's Deployment, Service, TLS, and replica count. This design reads an `ObjectStore`'s gateway pod placement (§5.3) and otherwise treats it as given.
- **The S3 gateway binary**, its `CreateBucket` handler, and the Vault-backed IAM it uses. A separate repository, out of this operator's scope entirely.
- **Phase 2's native backend** (log-structured extents on FoundationDB). This design provisions a POSIX/XFS volume only.
- **Cross-bucket operations.** `CopyObject` between two buckets, each its own volume, is gateway-binary behavior with no Kubernetes-side component.
- **A new privilege-isolation boundary beyond what already exists.** The CSI node plugin's existing privilege (`SYS_ADMIN`, `SYS_MODULE`) is unchanged by this design; it is reused, not widened or duplicated.

---

## 3. Architecture Overview

```
┌───────────────────── kubernetes/operator (one manager process) ─────────────────┐
│                                                                                    │
│   ┌──────────────────────────── S3BucketReconciler ─────────────────────────┐    │
│   │ 1. Ensure a PersistentVolumeClaim exists for spec.bucketName            │    │
│   │ 2. Watch the PVC/PV binding; find the NFSExport bound to it             │    │
│   │ 3. Read the ObjectStore's gateway Deployment's pod→node placement       │    │
│   │ 4. Call DeliverBucket on each such node, over its csi-link session      │    │
│   │ 5. Phase = Ready once PVC Bound, NFSExport Ready, every node reports    │    │
│   │    the bucket delivered                                                 │    │
│   └───────────────────────────────────┬───────────────────────────────────┘    │
│    S3Bucket CR   spec.{bucketName,capacityBytes,storageClassName}   │             │
│                  status.{phase,persistentVolumeClaimName,nfsExportName,          │
│                          mountDeliveredNodes,conditions}            │             │
│                                                                      │ DeliverBucket/ │
│   watches: PersistentVolumeClaim, PersistentVolume, NFSExport       │ WithdrawBucket,│
│   (design-pnfs-rwx), the ObjectStore's gateway Deployment's Pods    │ an in-process  │
│                                                                      │ call           │
│                                                                      ▼                │
│                                                        ┌───────────────────────┐    │
│                                                        │  csi-link hub          │    │
│                                                        └───────────┬───────────┘    │
└──────────────┬──────────────────────────────────────────────────────┼───────────────┘
               │ ordinary PVC (fstype: pnfs)                          │ routed down the
               ▼                                                     │ session the node
┌──────────────────────────────┐                                     │ plugin dialed out
│  CSI driver + NFSExport       │                                     ▼
│  reconciler (unmodified,      │                      ┌───────────────────────────┐
│  design-pnfs-rwx.md)          │                      │  CSI node plugin           │
│  - creates lvol, NFSExport    │                      │  (existing, unmodified     │
│  - binds the cluster's MDS    │                      │  DaemonSet; the RPC is the │
│  - assembles the export       │                      │  only new surface)         │
│                               │                      │  - calls its own in-process│
└──────────────────────────────┘                      │    NodeStageVolume /       │
                                                        │    NodePublishVolume       │
                                                        └────────────┬─────────────┘
                                                                     │ mounts, via the
                                                                     │ same already-
                                                                     │ privileged plugin
                                                                     ▼
                                                        ┌───────────────────────────┐
                                                        │  gateway Pod (unmodified   │
                                                        │  pod spec, never restarted │
                                                        │  for bucket lifecycle)     │
                                                        └───────────────────────────┘
```

**What the diagram cannot show:** the set of nodes the reconciler calls tracks the gateway Deployment's *current* pod-to-node assignment, not a fixed set (§5.3 covers reconciling that set as gateway pods are rescheduled). No new Pod, Service, or DaemonSet exists anywhere in this flow. `DeliverBucket`/`WithdrawBucket` are new RPCs on the CSI node plugin's existing `csi-link` session, the same channel `design-pnfs-rwx.md`'s NFSExport assembly already uses to reach a specific node from the operator.

---

## 4. API Design — New CRD

### 4.1 `S3Bucket` Spec and Status (new)

`S3BucketSpec` carries the bucket's identity and sizing; `ObjectStoreRef`, `BucketName`, and `StorageClassName` are immutable, because none of them has a defined reconciliation if changed after the volume exists. `S3BucketStatus` is observed state only: `MountDeliveredNodes` in particular is read back from the gateway Deployment's own pods and each node's CSI node plugin rather than accepted from a user, since a user-supplied node list could name a node no gateway pod runs on, or miss one that does.

```go
// S3BucketPhase is the lifecycle phase of a bucket's provisioning.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded;Deleting
type S3BucketPhase string

const (
	S3BucketPhasePending      S3BucketPhase = "Pending"
	S3BucketPhaseProvisioning S3BucketPhase = "Provisioning"
	S3BucketPhaseReady        S3BucketPhase = "Ready"
	S3BucketPhaseDegraded     S3BucketPhase = "Degraded"
	S3BucketPhaseDeleting     S3BucketPhase = "Deleting"
)
```

`ObjectStoreRef`, `BucketName`, and `StorageClassName` are immutable (`+k8s:immutable`), because none has a defined reconciliation if changed after the volume exists:

```go
// StorageClassName names the StorageClass the backing PVC is created
// against. It must select the pnfs fstype (design-pnfs-rwx.md §9.2), or
// the bucket never leaves Pending: versitygw's --bucketlinks mapping
// depends on every gateway pod sharing this volume RWX. Immutable.
// +kubebuilder:validation:Required
// +k8s:immutable
StorageClassName string `json:"storageClassName"`
```

`BucketName` is validated against the S3 bucket-naming grammar rather than the Kubernetes object-name grammar, since the two disagree on underscores and case: `+kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$``.

`S3BucketStatus` is observed state only. `MountDeliveredNodes` in particular is read back from the gateway Deployment's own pods and confirmed by each node's CSI node plugin rather than accepted from a user, since a user-supplied node list could name a node no gateway pod runs on, or miss one that does:

```go
// MountDeliveredNodes lists the nodes whose CSI node plugin has
// confirmed this bucket staged and published, mirrored from the
// ObjectStore's gateway Deployment's pod placement. Observed, not
// desired.
// +optional
MountDeliveredNodes []string `json:"mountDeliveredNodes,omitempty"`
```

The whole type, including `Phase`, `NFSExportName`, `Conditions`, and `ObservedGeneration`, is in Appendix A. A finalizer (`objectstore.simplyblock.io/s3bucket`) is mandatory: the CR is the only record of a PVC and a set of per-node mount deliveries, and a direct delete without it orphans both.

A CR as it exists once `Ready`:

```yaml
apiVersion: objectstore.simplyblock.io/v1alpha1
kind: S3Bucket
metadata:
  name: team-a-reports
  namespace: s3-system
  finalizers:
    - objectstore.simplyblock.io/s3bucket
spec:
  objectStoreRef: team-a-store
  bucketName: team-a-reports
  capacityBytes: 107374182400
  storageClassName: simplyblock-pnfs
status:
  phase: Ready
  persistentVolumeClaimName: s3bucket-team-a-reports
  nfsExportName: nfsexp-8f1a2c0e3b
  mountDeliveredNodes:
    - talos-8ep-csp
    - talos-pub-abo
  conditions:
    - type: VolumeBound
      status: "True"
      reason: PersistentVolumeBound
    - type: ExportReady
      status: "True"
      reason: NFSExportReady
    - type: MountDelivered
      status: "True"
      reason: AllNodesPublished
  observedGeneration: 3
```

---

## 5. Mount Delivery to Gateway Pods

### 5.1 The mechanism

Every gateway pod of an `ObjectStore` mounts one `hostPath` volume at a fixed path, set in the pod template at Deployment creation and never modified afterward, with `mountPropagation: HostToContainer`. `DeliverBucket` (§5.2) makes the CSI node plugin call its own `NodeStageVolume` then `NodePublishVolume` in-process, with `target_path` set to a path under that same shared host directory. `HostToContainer` propagation delivers the resulting bind mount into every gateway pod on the same node without the gateway pod's spec changing.

The chosen `staging_target_path` and `target_path` must themselves resolve to a subdirectory of one of the three host paths the CSI node plugin's own `DaemonSet` already mounts `Bidirectional` (`/var/lib/kubelet/plugins/…`, `/var/lib/kubelet/pods/…`, or `/var/lib/nvme/…`): a mount the node plugin makes outside those three stays inside its own container's mount namespace and never reaches the host or any other pod (`0004-direct-csi-mount-spike-result.md`). The shared host directory the gateway pods mount is therefore a subdirectory of one of these three, not an arbitrary path of this design's choosing.

### 5.2 `DeliverBucket` and `WithdrawBucket`

No new component runs on the node. `DeliverBucket` and `WithdrawBucket` are two new RPCs the CSI node plugin itself answers, registered on the `csi-link` session it already dials out to the operator and holds open (`csi-driver/internal/csilink`), the same channel `design-pnfs-rwx.md`'s NFSExport assembly already uses to reach one node's plugin from the operator. The plugin needs no new mount capability: it already has every capability `NodeStageVolume`/`NodePublishVolume` require, and this adds a caller for them that is not kubelet.

`DeliverBucket(bucket, volumeContext)` calls `NodeStageVolume` then `NodePublishVolume` in-process, with `target_path` derived deterministically from the bucket's name, so every gateway pod on a node sees the same bucket appear at the same relative path regardless of when it was delivered. `WithdrawBucket(bucket)` calls `NodeUnpublishVolume` then `NodeUnstageVolume` the same way. Both are idempotent, matching the CSI calls they wrap.

The plugin already persists a durable stack record per staged volume (`ns.stack.store`, read by `attachShape` and `teardownPlan`) so it can heal a mount after its own restart or a fabric blip, independent of kubelet. `DeliverBucket`/`WithdrawBucket` read and write that same record rather than a new one: a bucket this design delivers is, from the plugin's point of view, a volume staged and published like any other, and the existing restart-recovery path covers it without new code.

### 5.3 Tracking gateway pod placement

The reconciler lists the `ObjectStore`'s gateway Deployment's pods (by the fixed label `objectstore.simplyblock.io/instance=<name>`, `design-objectstore.md`, no field read off the `ObjectStore` itself) and derives the current node set. For every node in that set the bucket is not yet delivered to, it resolves the node's `csi-link` session (`PeerRegistry.Conn(link.NodePeer(nodeName))`, the same lookup `nfsexport_assembler.go` uses) and calls `DeliverBucket`. For every node holding a delivery no longer in the set, it calls `WithdrawBucket` the same way. This reconciles on every `S3Bucket` pass and whenever the watched Deployment's pod set changes, so a gateway pod rescheduled to a new node gets the bucket delivered there within one reconcile, not only at bucket-creation time. No Kubernetes object is created or deleted for this: what changes is the CSI node plugin's own stack record on the affected nodes.

### 5.4 Verification

Two alternatives considered and rejected. A dedicated pod per `(bucket, node)` pair that references the bucket's `PersistentVolumeClaim` directly, relying on kubelet's own CSI flow and a `Bidirectional` `hostPath` mount on that pod to reach the shared host path, is not used here. `0004-direct-csi-mount-spike-result.md` records why: the `Bidirectional` mount needs a privileged pod per `(bucket, node)` pair, which conflicts with the zero added node-side privilege this design requires (§2), and it is one Kubernetes object per `(bucket, node)` pair rather than one per node.

A standalone, unprivileged per-node agent Pod calling the CSI node plugin's socket directly, considered next, is not used either: it would add one more Pod, Service, and ServiceAccount to every node for a job the plugin's own existing `csi-link` session already reaches, and putting the caller inside the gateway pod instead would cross the gateway's own trust boundary, the one component in this design that serves untrusted traffic directly. Extending the plugin itself, over a channel it already holds open and the operator already trusts for NFSExport assembly, adds neither.

The same spike confirmed, on a live cluster, the fact this design turns on: a `NodePublishVolume` bind mount placed under the node plugin's own `Bidirectional` `hostPath` subtree does propagate, via `HostToContainer`, into an already-running, separately scheduled pod, with correct reads and writes and no restart of that pod. Phase 0 P0-2 is resolved.

---

## 6. State Machine

```
  S3Bucket created
    │
    ▼
  Pending          ← no PersistentVolumeClaim yet
    │  PVC created
    ▼
  Provisioning     ← waiting on PVC Bound, then NFSExport Ready, then every node's CSI
                     node plugin confirming DeliverBucket
    │  all three satisfied
    ▼
  Ready            ← status.conditions all True
    │  spec changed in a way that invalidates provisioning (should not happen; every
    │  mutable field reconciles in place)
    ▼
  (re-enters Provisioning only via Degraded, never directly from Ready)

  deletion requested at any phase
    │
    ▼
  Deleting         ← unpublish and unstage on every delivered node, release the PVC,
                     remove finalizer
```

| Condition                                                              | Phase                 | Result                                                                                                                                                                                                                                   |
|------------------------------------------------------------------------|-----------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| PVC provisioning fails (StorageClass missing, capacity exhausted)      | Pending               | Stays Pending, event names the cause, no retry storm (exponential backoff)                                                                                                                                                               |
| `NFSExport` reaches `Degraded` (`design-pnfs-rwx.md` §13)              | Provisioning          | `S3Bucket` moves to `Degraded`, `ExportReady` condition `False` with the export's own reason                                                                                                                                             |
| A node's `csi-link` session is down, or its `DeliverBucket` call fails | Provisioning          | `S3Bucket` stays in `Provisioning` past its deadline, then `Degraded`, `MountDelivered` `False` naming the node                                                                                                                          |
| Operator restarts mid-`Provisioning`                                   | Provisioning          | Resumes from `status`: re-lists the PVC, the `NFSExport`, and the per-node delivery set rather than restarting from `Pending`                                                                                                            |
| `ObjectStore`'s gateway Deployment is deleted while buckets exist      | Provisioning or Ready | The reconciler has no node set to target. Existing deliveries are left in place (not unpublished) until the `S3Bucket` itself is deleted, since the volume and export remain valid independent of whether a gateway currently reads them |

---

## 7. Controller Design

### 7.1 Location

`S3BucketReconciler` registers into the existing `kubernetes/operator` manager, the same process that already reconciles `StorageCluster`, `StorageNode`, `Pool`, and `NFSExport` (and, by a separate design, `ObjectStore`). It is a new API group, `objectstore.simplyblock.io`, not an addition to `storage.simplyblock.io` (keeping this feature's API versioning and lifecycle independent of the existing group's), but it is not a new manager process.

This is what makes `DeliverBucket`/`WithdrawBucket` (§5.2, §5.3) straightforward: both need the in-process `PeerRegistry` the `csi-link` hub owns, and `S3BucketReconciler` calls it the same way `NFSExportReconciler` already does for NFSExport assembly, because it runs in the same process. The alternative, a separate manager, was considered and rejected: it would need either a second `PeerRegistry`-reaching mechanism built just for this (a new `csi-link` peer kind the hub relays through, or a dedicated API `S3BucketReconciler` calls), or the delivery piece split into its own controller inside the main operator while everything else stayed out. Both are more moving parts than the risk this reconciler actually carries justifies, once `DeliverBucket`/`WithdrawBucket` reuse `NFSExportReconciler`'s already-proven mechanism rather than inventing a new one. The cost accepted instead: this reconciler's RBAC (PVC create/delete, `NFSExport` and gateway-pod read) joins the main operator's existing `ServiceAccount`, the same union-of-permissions tradeoff every controller already in that manager carries.

### 7.2 Reconciliation Trigger

Watches: `S3Bucket` (primary), `PersistentVolumeClaim` and `PersistentVolume` (owned/referenced), `NFSExport` (read-only, to find the one bound to this bucket's volume), and the `ObjectStore`'s gateway Deployment's `Pod`s (read-only, for §5.3's node set).

### 7.3 Concurrency and Mutual Exclusion

One `S3Bucket` reconciles independently of another. Nothing here is cluster-wide singleton state the way `NFSExport`'s one-MDS-per-cluster binding is. Within one `S3Bucket`, the per-node delivery set (§5.3) is reconciled to the observed Deployment pod set on every pass, so two concurrent reconciles converge to the same result rather than racing: the standard controller-runtime guarantee, not new discipline.

### 7.4 Interaction with Existing Controllers

Reads but never writes `NFSExport` and the gateway Deployment's `Pod`s. Creates and owns `PersistentVolumeClaim`s. Calls `DeliverBucket`/`WithdrawBucket` on a node's CSI node plugin over its `csi-link` session to deliver or withdraw a bucket's mount, creating no `Pod` for it. Never creates or modifies an `NFSExport` directly: that remains entirely the CSI controller's responsibility per `design-pnfs-rwx.md` §9.3.

### 7.5 RBAC

| Resource                            | Verbs                            | Why                                                                                                           |
|-------------------------------------|----------------------------------|---------------------------------------------------------------------------------------------------------------|
| `s3buckets`, `s3buckets/status`     | get, list, watch, update, patch  | Primary reconciled kind                                                                                       |
| `persistentvolumeclaims`            | get, list, watch, create, delete | One PVC per bucket                                                                                            |
| `persistentvolumes`                 | get, list, watch                 | Read the bound PV's CSI volume handle                                                                         |
| `nfsexports.storage.simplyblock.io` | get, list, watch                 | Find the export bound to a bucket's volume. Never mutated here                                                |
| `pods`                              | get, list, watch                 | Reading the gateway Deployment's pods for their node placement (§5.3). No `Pod` is created for mount delivery |
| `deployments.apps`                  | get, list, watch                 | Read the `ObjectStore`'s gateway Deployment for its pod selector                                              |

The reconciler also needs a `PeerRegistry` (`registry.Conn(link.NodePeer(nodeName))`) to call `DeliverBucket`/`WithdrawBucket` (§5.2, §5.3). No new RBAC grant: this is not a Kubernetes API permission, it is the same in-process `csi-link` hub `NewLinkAssembler` already uses for NFSExport assembly, reachable because §7.1 runs this reconciler in the same manager that hosts the hub.

---

## 8. Backend API Requirements

None. This design calls no `sbcli` endpoint directly. Every control-plane interaction it depends on (lvol creation, NVMe-oF connect info) happens inside the existing CSI controller and `NFSExportReconciler` per `design-pnfs-rwx.md` §6.5.

---

## 9. Failure Modes and Fallback

| Failure                                                                                                                                                                                            | Detection                                                                                                                                               | Behavior                                                                                |
|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------|
| StorageClass does not exist or does not select `fstype: pnfs`                                                                                                                                      | PVC stays unbound, `events` on the PVC                                                                                                                  | `S3Bucket` stays `Pending`, event `PersistentVolumeClaimNotBound` on the `S3Bucket`     |
| `NFSExport` never found for the bound PV (CSI controller did not create one: StorageClass misconfigured)                                                                                           | Reconcile finds no `NFSExport` with a matching `spec.volumeRef` after a bound PVC                                                                       | `Degraded`, `ExportReady` `False`, reason `NoMatchingExport`                            |
| `ObjectStore` referenced by `spec.objectStoreRef` does not exist                                                                                                                                   | Deployment lookup fails                                                                                                                                 | `Degraded`, event `ObjectStoreNotFound`                                                 |
| A node's `csi-link` session is down, or its `DeliverBucket` call returns an error                                                                                                                  | The RPC fails or times out                                                                                                                              | `Provisioning` past deadline, then `Degraded`, `MountDelivered` `False` naming the node |
| A node's CSI node plugin reports a bucket delivered, but its stack record has drifted from what is actually mounted (after the plugin's own restart, a node reboot, or a fabric repair race, §5.2) | Not detectable from this reconciler's own view: it only observes what `DeliverBucket` last reported, not what a gateway pod's filesystem actually shows | Not covered by this design as written. See Open Question 6                              |

---

## 10. Observability

Addition to a surface that exists: the existing operator's `Pod`/`PersistentVolumeClaim` events and the Kubernetes `Event` API are reused patterns, applied to a new kind.

### Kubernetes Events

Target: the `S3Bucket` object, since it is the audit record a user (or the gateway binary, on its behalf) watches, and it outlives any individual PVC or per-node delivery it tracks.

| Event                                                                    | Type    | Reason                          |
|--------------------------------------------------------------------------|---------|---------------------------------|
| The bucket's PersistentVolumeClaim has not bound within the retry budget | Warning | `PersistentVolumeClaimNotBound` |
| No `NFSExport` was found referencing this bucket's bound volume          | Warning | `NoMatchingExport`              |
| The `ObjectStore` named in `spec.objectStoreRef` does not exist          | Warning | `ObjectStoreNotFound`           |
| A node's CSI node plugin could not stage or publish the bucket           | Warning | `MountDeliveryFailed`           |
| The bucket reached `Ready`                                               | Normal  | `BucketReady`                   |

### Prometheus Metrics

| Metric                                                | Labels                                        | Description                                                                                                                             |
|-------------------------------------------------------|-----------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------|
| `simplyblock_objectstore_bucket_phase_state`          | `bucket`, `objectstore`, `phase`              | Gauge, 1 for the bucket's current phase and 0 for the others. What a dashboard sums to show buckets stuck outside `Ready`               |
| `simplyblock_objectstore_bucket_provisioning_seconds` | `objectstore`                                 | Histogram of time from `Pending` to `Ready`, the direct measurement the `CreateBucket` synchronous-wait bound needs and currently lacks |
| `simplyblock_objectstore_mount_delivery_calls_total`  | `objectstore`, `node`, `operation`, `outcome` | Counter of `DeliverBucket`/`WithdrawBucket` call outcomes, by result                                                                    |

`simplyblock_objectstore_bucket_provisioning_seconds` is the load-bearing one: it is the only way to learn, from a running cluster rather than a guess, whether the gateway's synchronous `CreateBucket` wait bound is set to a number that reflects reality.

---

## 11. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts: [`tests/test-plan-bucket-provisioning.md`](../tests/test-plan-bucket-provisioning.md)

- **Unit:** the per-node delivery node-set diffing logic (§5.3) against a fake client, independent of any real CSI or pNFS behavior, and the `S3BucketPhase` transition table against every input combination in §6's failure table.
- **Integration:** the full reconcile loop against `envtest`, with the CSI driver, `NFSExportReconciler`, and a fake `PeerRegistry` faked at the boundary (a test double that marks a `PersistentVolumeClaim` Bound, creates a matching `NFSExport` in `Ready`, and acknowledges a `DeliverBucket` call, since the real CSI driver and a real `csi-link` session both need a live cluster). Proves the `S3Bucket` state machine and RBAC, not the real mount.
- **E2E:** a live cluster with the pNFS path actually deployed (Phase 0 P0-1), proving what integration tests cannot: that `DeliverBucket`/`WithdrawBucket` (restaging after a fabric blip or the plugin's own restart, converging to a new node set as gateway pods reschedule) behave correctly against the real CSI node plugin. `0004-direct-csi-mount-spike-result.md` already demonstrated the underlying propagation mechanism itself (Phase 0 P0-2). What remains for this class is the RPC's own correctness, not the raw mechanism.
- **Load / long-running:** not yet scoped. `simplyblock_objectstore_bucket_provisioning_seconds` (§10) is the metric such a test needs.

The risk concentrates in the E2E class: every unit and integration scenario can pass while `DeliverBucket`/`WithdrawBucket` still have a defect, because those harnesses fake the exact boundary the RPC operates across.

---

## 12. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                   | Owner                                               |
|-----|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------|
| 1   | ~~**Mount propagation (§5.4):** does `HostToContainer` propagation on the gateway pod actually see a mount-holder's `NodePublishVolume` bind mount?~~ **Resolved** (`0004-direct-csi-mount-spike-result.md`): yes, for a mount placed under the node plugin's own `Bidirectional` `hostPath` subtree. See §5.4.                                                                                                                            | Resolved                                            |
| 2   | ~~**`ObjectStore`'s shape:** what fields it exposes for this design to read a gateway Deployment's pod selector, and whether `S3Bucket` and `ObjectStore` are reconciled by the same manager or two.~~ **Resolved** (`design-objectstore.md`): no field read at all. The gateway Deployment and its pods are named deterministically from `ObjectStore.metadata.name`, labeled `objectstore.simplyblock.io/instance=<name>`. Same manager. | Resolved                                            |
| 3   | **Who creates an `S3Bucket`:** whether the gateway's `CreateBucket` handler needs its own Kubernetes RBAC to create one directly, or goes through a narrower-scoped intermediary. This design accepts an `S3Bucket` from any authorized caller and does not constrain who that is.                                                                                                                                                         | Gateway repository / RBAC hardening                 |
| 4   | **The `CreateBucket` synchronous-wait bound:** unmeasured. `simplyblock_objectstore_bucket_provisioning_seconds` (§10) is how it gets measured once this design is deployed somewhere the pNFS path exists.                                                                                                                                                                                                                                | This design, once P0-1 is available for measurement |
| 5   | ~~**Mount-holder resource footprint at scale:** one pod per `(bucket, node)` pair.~~ **Resolved** (§5.2, §5.4): no Pod of any kind. `DeliverBucket`/`WithdrawBucket` run inside the existing CSI node plugin, over its existing `csi-link` session.                                                                                                                                                                                        | Resolved                                            |
| 6   | **Stack-record drift:** the CSI node plugin's own stack record (§5.2) can disagree with what is actually mounted on the node after the plugin's restart, a node reboot, or a fabric-repair race, and the reconciler only observes what `DeliverBucket` last reported, not the node's real state (§9). Neither a reconciliation pass against drift nor its detection is designed here.                                                      | Open, no owner yet                                  |
| 7   | ~~**Where `S3BucketReconciler` runs:** `DeliverBucket`/`WithdrawBucket` need the in-process `PeerRegistry` the main operator's `csi-link` hub owns, which a separate manager would not have.~~ **Resolved** (`0005-s3bucket-joins-the-main-operator-manager.md`): `S3BucketReconciler` registers into the existing `kubernetes/operator` manager. See §7.1.                                                                                | Resolved                                            |

---

## Appendix A: `s3bucket_types.go`

```go
// Package v1alpha1 of the objectstore.simplyblock.io API group.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// S3BucketFinalizer keeps the record alive until its PVC is released and its
// per-node mount deliveries are withdrawn. Without it a direct delete orphans
// a mounted export and a volume nothing describes any longer.
const S3BucketFinalizer = "objectstore.simplyblock.io/s3bucket"

// S3BucketPhase is the lifecycle phase of a bucket's provisioning.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded;Deleting
type S3BucketPhase string

const (
	S3BucketPhasePending      S3BucketPhase = "Pending"
	S3BucketPhaseProvisioning S3BucketPhase = "Provisioning"
	S3BucketPhaseReady        S3BucketPhase = "Ready"
	S3BucketPhaseDegraded     S3BucketPhase = "Degraded"
	S3BucketPhaseDeleting     S3BucketPhase = "Deleting"
)

// S3BucketSpec is the desired state of one S3 bucket's backing volume.
type S3BucketSpec struct {
	// ObjectStoreRef names the ObjectStore whose gateway Deployment serves
	// this bucket. Immutable: a bucket does not move between gateways.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ObjectStoreRef string `json:"objectStoreRef"`

	// BucketName is the S3 bucket name as presented to clients, validated
	// against the S3 bucket-naming grammar rather than the Kubernetes
	// object-name grammar, since the two disagree on underscores and case.
	// Immutable.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`
	// +k8s:immutable
	BucketName string `json:"bucketName"`

	// CapacityBytes is the backing volume's requested size. An increase
	// reconciles to a PVC expansion; a decrease is rejected, the same
	// restriction the PVC it wraps already carries.
	// +kubebuilder:validation:Required
	CapacityBytes int64 `json:"capacityBytes"`

	// StorageClassName names the StorageClass the backing PVC is created
	// against. It must select the pnfs fstype (design-pnfs-rwx.md §9.2), or
	// the bucket never leaves Pending: versitygw's --bucketlinks mapping
	// depends on every gateway pod sharing this volume RWX. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	StorageClassName string `json:"storageClassName"`
}

// S3BucketStatus is what the bucket's provisioning currently is.
type S3BucketStatus struct {
	// Phase is the bucket's lifecycle position.
	// +optional
	Phase S3BucketPhase `json:"phase,omitempty"`

	// PhaseDeadline is when the current phase must be given up on. Persisted
	// so an operator restarted mid-Provisioning resumes the same deadline
	// rather than granting a fresh one every time it comes back up (W-03,
	// added during implementation: this appendix's first draft named the
	// deadline in §9's prose without adding the field that survives it, the
	// same shape NFSExportStatus.PhaseDeadline already carries).
	// +optional
	PhaseDeadline *metav1.Time `json:"phaseDeadline,omitempty"`

	// PersistentVolumeClaimName is the PVC this reconciler created for the
	// bucket's backing volume, recorded rather than recomputed so a later
	// change to the naming scheme cannot orphan an existing bucket's volume.
	// +optional
	PersistentVolumeClaimName string `json:"persistentVolumeClaimName,omitempty"`

	// NFSExportName is the NFSExport CR the CSI controller created for the
	// PVC (design-pnfs-rwx.md §9.3), found by matching its spec.volumeRef
	// against the bound PersistentVolume's CSI volume handle. Empty until
	// the PVC is Bound.
	// +optional
	NFSExportName string `json:"nfsExportName,omitempty"`

	// MountDeliveredNodes lists the nodes whose CSI node plugin has
	// confirmed this bucket staged and published, mirrored from the
	// ObjectStore's gateway Deployment's pod placement. Observed, not
	// desired.
	// +optional
	MountDeliveredNodes []string `json:"mountDeliveredNodes,omitempty"`

	// Conditions carries why a bucket is Degraded, which a phase alone
	// cannot express. Expected types: VolumeBound, ExportReady,
	// MountDelivered.
	// +optional
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=s3bkt
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=".spec.bucketName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=".status.persistentVolumeClaimName"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// S3Bucket is the desired existence of one S3 bucket: its backing volume,
// its pNFS export, and the per-node mount deliveries that make its gateway's pods
// see it.
type S3Bucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   S3BucketSpec   `json:"spec,omitempty"`
	Status S3BucketStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type S3BucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []S3Bucket `json:"items"`
}
