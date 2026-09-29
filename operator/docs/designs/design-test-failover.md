# Design Document: Non-Disruptive Test Failover

**Status:** Draft  
**Author:** Israel Geoffrey (geoffrey1330)  
**Date:** 2026-09-29  
**Test Plan:** [`tests/test-plan-test-failover.md`](../tests/test-plan-test-failover.md)

---

## Phasing Overview

| Phase       | Status  | Where the bubble runs           | Recovery point                                | New capability                                                   | Sections              |
|-------------|---------|---------------------------------|-----------------------------------------------|------------------------------------------------------------------|-----------------------|
| **Phase 1** | Planned | The source's own cluster        | A fresh snapshot of the source volume         | Cross-cluster read and placement from the hub (OCM)              | §4, §5.1–§5.4, §6, §7 |
| **Phase 2** | Planned | The DR target cluster           | The replicated snapshot already on the target | None beyond Phase 1                                              | §5.5                  |
| **Phase 3** | Planned | A cluster that holds no replica | A point shipped there on demand               | On-demand shipping of a recovery point to a named backend (P0-6) | §5.6                  |

The phases differ on one axis: where the recovery point already lives relative to where the bubble runs. In Phase 1 the bubble runs where the source is, so the point is a fresh snapshot on the same backend. In Phase 2 the bubble runs on the DR target, where replication has already landed a snapshot, so no data moves. In Phase 3 the bubble runs somewhere with no copy, the only case that needs data shipped on demand and the design's long pole.

The hub coordinates every phase. It never has to run the source or the bubble itself, and both may be any managed cluster. What Phase 1 already needs, and every later phase inherits, is the ability to read the source object on its cluster and place the bubble object on the recovery cluster, both from the hub, which OCM provides.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                 | Kind                    | Blocks  | Status                                                                                                                                                   |
|------|----------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------|----------------------------------------------------------------------------------------------------------------------------------------------------------|
| P0-1 | Take a snapshot of the source volume, and a group-consistent snapshot of a consistency group, without disturbing the running volume          | Control plane (`sbcli`) | Phase 1 | Shipped: `snapshot_controller.add` for a volume, and the group snapshot primitive (`bdev_lvol_snapshot_group`, design-consistency-groups.md) for a group |
| P0-2 | Clone a snapshot into a writable volume in a chosen pool, and return the clone's volume handle                                               | Control plane (`sbcli`) | All     | Shipped: `snapshot_controller.clone`, and the CSI clone-from-snapshot path                                                                               |
| P0-3 | Delete a snapshot and delete a volume, both idempotent                                                                                       | Control plane (`sbcli`) | All     | Shipped                                                                                                                                                  |
| P0-4 | Resolve the latest replicated snapshot on a DR-target backend for a source relationship                                                      | Control plane (`sbcli`) | Phase 2 | Shipped: `lvol_controller.latest_replicated_snapshot` and `replication_policy_controller.latest_replicated_generation`, with v2 endpoints                |
| P0-5 | From the hub, read an object on a managed cluster (`ManagedClusterView`) and place objects on it (`ManifestWork`), each with status feedback | Ecosystem (OCM)         | Phase 1 | Available: both are OCM primitives, once the cluster is a registered `ManagedCluster` with a working view controller (04-bootstrap-ocm.sh)               |
| P0-6 | On-demand shipping of a specific snapshot or group generation to a named target backend that holds no copy, followed by a clone there        | Control plane (`sbcli`) | Phase 3 | Not shipped. The long pole of Phase 3. Today's cross-cluster reach is the continuous replication engine or the S3 backup path, neither an on-demand push |

Phase 1 has no unmet storage prerequisite: it takes, clones, and deletes snapshots with calls that ship. Its one non-storage need is OCM (P0-5), which the DR setup already establishes, and it is needed from Phase 1 because the hub, as coordinator, reaches the source and the bubble on their own clusters through it. Phase 2 adds nothing new, because the recovery point is already on the target backend (P0-4). Phase 3 is the exception: P0-6 is genuinely new, because moving one recovery point to a backend that holds no copy of it, on demand, is a capability the engine does not have.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API Design — New CRD](#4-api-design--new-crd)
5. [Core Mechanism](#5-core-mechanism)
6. [State Machine](#6-state-machine)
7. [Controller Design](#7-controller-design)
8. [Backend API Requirements](#8-backend-api-requirements)
9. [Configuration](#9-configuration)
10. [Failure Modes and Fallback](#10-failure-modes-and-fallback)
11. [Observability](#11-observability)
12. [Testing Strategy](#12-testing-strategy)
13. [Open Questions](#13-open-questions)
14. [Appendix A: `testfailover_types.go`](#appendix-a-testfailover_typesgo)

---

## Overview

A test failover proves an application can be recovered from a point-in-time copy, in isolation, without disturbing the running production. The recovery point is always a snapshot and the result is always a clone, so the source is never touched. What varies is where the bubble runs, which is what a real DR test cares about: recovering onto the site a real failover would move to.

The feature is a new CRD, `TestFailover`, and its controller, both on the hub, which coordinates the drill. The object names the source, by the cluster it runs on and its PVC, and a place to recover it, the `bubbleCluster`. The controller reads the source PVC on its cluster to learn its volume, resolves the recovery point on the bubble's backend, clones it there, and places the clone on the bubble cluster as a bound PVC in an isolated namespace, `bubble` by default. The operator boots the application there, confirms the data, and deletes the drill, which reclaims the clone and any snapshot the drill took. The source serves throughout.

`bubbleCluster` selects the topology. Naming the source's own cluster recovers there from a fresh snapshot (Phase 1). Naming the DR target recovers from the replicated snapshot already sitting on its backend (Phase 2), a genuine "fail over to the target site" test with no data moved. Naming a cluster that holds no copy is the case that needs the point shipped there first (Phase 3). All three are one object, one controller, and one state machine, differing only in where the point comes from and where the PVC is placed.

---

## 1. Background

simplyblock has a real failover, driven either by Ramen (`DRPC.spec.action: Failover`) or imperatively by the simplyblock-native `ReplicationOps` CR. Both promote a replicated copy on the DR target and land the recovered workload there. That copy is, mechanically, a clone of the last replicated snapshot on the target, so a real failover is a clone-and-promote of a recovery point that already lives on the target's backend.

Three facts shape a test failover. First, replication is between two clusters: `add_target` refuses `target_cluster_id == cluster_id` with "A cluster cannot replicate to itself" (`simplyblock_core/controllers/replication_policy_controller.py`). So the source's own cluster has no replicated copy, and its recoverable point is a fresh snapshot of the source volume. The DR target does have one, which `lvol_controller.latest_replicated_snapshot` resolves without triggering anything. Second, the primitives to recover from either point already exist: `snapshot_controller.add` takes a snapshot, `snapshot_controller.clone` clones one into a writable volume, and both a snapshot and a volume can be deleted. A clone is a first-class volume with its own handle, which CSI static provisioning adopts as a `PersistentVolume`. Third, the hub already reaches its managed clusters both ways in this deployment: it reads an object on one through an OCM `ManagedClusterView`, the way Ramen reads a spoke's status, and writes one through a `ManifestWork`, the way Ramen places a workload.

What is missing is the orchestration: an object that finds the source on its cluster, resolves the right recovery point, clones it on the right backend, places the bubble PVC on the right cluster, proves the copy is recoverable, and tears it down, all without touching the source. Ramen orchestrates none of it, because Ramen only fails over and relocates for real. This design is that object.

---

## 2. Goals and Non-Goals

### Goals

- A `TestFailover` CRD and controller that, from one object on the hub, produce a bound PVC per source volume in an isolated namespace, on the cluster the drill recovers onto.
- Locate the source from the hub. The object names the source by its cluster and PVC, and the controller reads that PVC through OCM to learn its volume, so nobody has to hand-extract a backend handle.
- Recover onto the DR target site, from the replicated snapshot already there, with the bubble PVC placed on that cluster. This is the case that makes a test failover a real rehearsal of the DR target.
- Non-disruptive by construction. The recovery point is a snapshot and the result is a clone, so the source's data and I/O are never touched, and the controller records a before-and-after fingerprint of the source and its replication relationship so a regression is caught rather than assumed.
- Volume-scoped and consistency-group-scoped drills. A group drill recovers one PVC per member from one group-consistent point.
- Same-cluster today, DR target next, an arbitrary cluster later (§5), behind one object and one state machine.
- A finalizer-driven teardown that reclaims the clone, deletes any snapshot the drill took, removes the placed PVC from the bubble cluster, and proves nothing test-labeled remains.
- Restart safety. The controller records which side effect each step issued, so a restart mid-drill resumes rather than repeats.

### Non-Goals

- **Bringing up the application.** The object produces bound PVCs and stops. The workload that consumes them is the operator's to deploy. An application lifecycle and its workload spec are a separate concern, out of scope here.
- **A test failback.** The drill is one-way. Tearing it down reclaims the clone. There is no promote-back.
- **Recovering onto a cluster with no copy in Phase 1 or 2.** A bubble cluster whose backend holds no replica needs the point shipped there, which is Phase 3 (§5.6), gated on a backend primitive that does not exist (P0-6).
- **Snapshot scheduling and evidence export.** A recurring schedule and a signed test report are a layer above this object and are out of scope here.
- **Replacing Ramen's own test paths.** Ramen has no non-disruptive test. This design does not add one to Ramen. It is a simplyblock-native object that reuses OCM only as the transport for reading the source and placing the bubble.

---

## 3. Architecture Overview

```
   TestFailover CR ──▶┌──────────────────────────────────────────────────────┐
   (on the hub)       │ hub operator: TestFailoverReconciler                 │
   spec: scope,       │  1. read source PVC on sourceCluster (ManagedClusterView) → handle │
     sourceCluster,   │  2. resolve recovery point on the bubble's backend    │
     sourceNamespace, │       fresh snapshot (P0-1) | replicated (P0-4)       │
     sourceRef,       │  3. [Phase 3] ship point to that backend (P0-6)      │
     bubbleCluster,   │  4. clone the point on that backend        (P0-2)     │
     bubbleNamespace  │  5. place PV + PVC on bubbleCluster (ManifestWork)   │
                      │  6. Ready; hold until deleted                         │
                      │  7. finalizer: remove PVC, reclaim clone + snapshot   │
                      └──┬──────────────┬───────────────────────┬────────────┘
        ManagedClusterView│              │ REST (webapi.Client)  │ ManifestWork
        (read source)     ▼              ▼                       ▼ (place bubble)
        ┌────────────────────────┐ ┌──────────────────┐ ┌────────────────────────┐
        │ source cluster         │ │ control plane    │ │ bubble cluster         │
        │  PVC + PV (volumeHandle)│ │  snapshot P0-1   │ │  work-agent applies:   │
        │  projected to the hub  │ │  clone    P0-2   │ │   PersistentVolume     │
        └────────────────────────┘ │  delete   P0-3   │ │   PersistentVolumeClaim│
                                    │  latest-repl P0-4│ │  (bound to the clone   │
                                    │  ship P0-6 (P3)  │ │   on its backend)      │
                                    └──────────────────┘ └────────────────────────┘
```

The controller runs on the hub, which coordinates the drill and runs neither the source nor the bubble. It learns the source's volume by reading the source PVC and its PV on `sourceCluster` through an OCM `ManagedClusterView`, which projects their current state back to the hub. It reads the source only to fingerprint it, never to change it. The clone is built on the bubble cluster's own backend, and the bubble cluster's CSI driver adopts it through a static `PersistentVolume` naming the clone's handle, with a `PersistentVolumeClaim` bound to it in the bubble namespace.

Placement is uniform: the controller delivers the bubble PV and PVC to `bubbleCluster` as an OCM `ManifestWork`, whose work-agent applies them and reports the bind result back through the `ManifestWork` status. The hub addresses both the source and the bubble this way, including its own cluster when it is self-managed. The trust boundary for storage is the control-plane REST API, reached with the admin bearer token the operator already uses. The trust boundary for cross-cluster read and write is OCM, which the DR setup already establishes (04-bootstrap-ocm.sh).

---

## 4. API Design — New CRD

`TestFailover` is a namespaced object in the `storage.simplyblock.io` group, created on the hub. One object drives one drill. Its spec is immutable, because the object is a request and a drill whose target moved under the controller mid-flight has no coherent meaning. The full type is [Appendix A](#appendix-a-testfailover_typesgo), and the body shows only the fields an argument turns on.

### 4.1 `TestFailover` Spec

The spec names the source by where it runs and what it is, and names where to recover it. `sourceCluster` and `sourceRef` are what let the hub find the source without anyone extracting a backend handle by hand.

```go
// SourceCluster is the OCM ManagedCluster the source runs on. The hub reads the
// source there through a ManagedClusterView. Immutable.
// +kubebuilder:validation:Required
// +k8s:immutable
SourceCluster string `json:"sourceCluster"`

// SourceRef names the source on SourceCluster: a PersistentVolumeClaim in
// SourceNamespace (scope=Volume), or a consistency group (scope=Group).
// Immutable.
// +kubebuilder:validation:Required
// +k8s:immutable
SourceRef string `json:"sourceRef"`

// BubbleCluster is the OCM ManagedCluster to recover onto: the source's own
// cluster for an in-place test, the DR target, or another cluster. Immutable.
// +kubebuilder:validation:Required
// +k8s:immutable
BubbleCluster string `json:"bubbleCluster"`
```

`sourceCluster` answers "where is the PVC to test," and the controller reads it there rather than requiring a handle. `bubbleCluster` selects the topology (§5) and carries the "recover onto the target site" intent. `bubbleNamespace` defaults to `bubble` and is the namespace on the bubble cluster where every recovered PVC lands, isolated so it cannot collide with the source workload's PVCs, which carry the same names.

### 4.2 `TestFailover` Status

The status carries the state-machine position, the resolved source and recovery point, one entry per recovered volume, and a report. `phase` is the coarse lifecycle and `step` is the durable machine position with its deadline (§6). `clones` is the list the finalizer reclaims from, and `report` is the evidence a reader takes away.

```go
// Clones is one entry per recovered volume: the source it came from, the
// snapshot and clone the drill built, and the PVC placed on the bubble cluster.
// +optional
// +listType=map
// +listMapKey=sourceRef
Clones []TestFailoverClone `json:"clones,omitempty"`

// Report is the drill's evidence: the source and point recovered, its age, the
// cluster it ran on, and whether the source was untouched. Populated as the
// drill reaches Ready.
// +optional
Report *TestFailoverReport `json:"report,omitempty"`
```

An invariant the controller enforces and the status records: the drill is non-disruptive. `status.report.invariantsHeld` is set only when the fingerprint of the source and its replication relationship taken before the drill matches the one taken after (§7.4). A drill that reached `Ready` with `invariantsHeld: false` is a defect, not a passing test.

The object owns a finalizer, `storage.simplyblock.io/testfailover-teardown`. Deletion runs the teardown state (§6) before the finalizer is removed, so a clone, a drill-taken snapshot, or a placed PVC is never orphaned by a delete that races the controller.

---

## 5. Core Mechanism

### 5.1 Locating the source

The hub does not run the source, so it reads it. The controller creates an OCM `ManagedClusterView` on `sourceCluster` for the source PVC named by `sourceRef` in `sourceNamespace`, and for the PV it is bound to, which projects their current state back to the hub. From the PV's `spec.csi.volumeHandle` it learns the source volume's backend handle, the identity every later step keys on. For a group drill, `sourceRef` names a consistency group, whose member volumes the control plane resolves from the group id on `sourceCluster`'s backend, so the drill recovers the whole set.

Reading the source is also where the non-disruptiveness fingerprint begins: the handle, the PVC's binding, and the replication relationship's state are captured here and compared again at the end (§7.4).

### 5.2 Resolving the recovery point

The recovery point is always a snapshot, and where it comes from follows `bubbleCluster`. When `bubbleCluster` is the source's own cluster the point is a snapshot of the source volume there: an existing one when `spec.recoveryPoint` is set, or a fresh one the drill takes (P0-1) for a well-defined point at its start. When `bubbleCluster` names the DR target, the point is the latest replicated snapshot already on that cluster's backend, which `latest_replicated_snapshot` resolves from the source handle (P0-4) without triggering anything. For a group drill the point is one group-consistent snapshot, taken fresh on the source cluster or resolved as the latest replicated generation on the target.

Taking a snapshot does not disturb the running volume: it adds a point to the volume's chain and copies no data, and the drill deletes any snapshot it took on teardown. Resolving the replicated point touches nothing, because replication already produced it. Neither path promotes or commits anything, which is what keeps the source and the live replication relationship untouched.

### 5.3 Cloning the point on the bubble's backend

The controller clones the recovery-point snapshot into a writable volume on the bubble cluster's own backend (P0-2), and the backend returns the clone's volume handle. For an in-place drill that backend is the source's. For a DR-target drill it is the target's, where the replicated snapshot already sits, so the clone is local to the point and no data crosses a cluster boundary. The clone is a first-class volume, tagged with the drill's `test-id` for reclaim.

### 5.4 Placing the bubble PVC

The clone is adopted as a static `PersistentVolume` whose `spec.csi.volumeHandle` is the clone's handle, with a `PersistentVolumeClaim` bound to it in the bubble namespace, sized from the source's request. The controller delivers the PV and PVC to `bubbleCluster` as an OCM `ManifestWork` (P0-5), and that cluster's work-agent applies them and reports the PVC bound through the `ManifestWork` status. The PV carries `persistentVolumeReclaimPolicy: Retain`, so deleting the PVC does not delete the clone, which the controller reclaims at the backend on teardown.

The result is one bound PVC per source volume in the bubble namespace on the bubble cluster. For a group drill, one PVC per member from the one point, which is what makes the recovered set crash-consistent.

### 5.5 Recovering onto the DR target (Phase 2)

The DR-target case is why the source is named by cluster and PVC rather than assumed local. The source runs on one cluster and the bubble on the DR target, so the controller, from the hub, reads the source PVC on its cluster (§5.1), resolves its handle to the replicated snapshot on the target's backend (P0-4), clones it there (§5.3), and places the bubble PVC on the target through `ManifestWork` (§5.4). Nothing is shipped, because replication already put the point on the target. This is a true rehearsal of the site that would take over in a real failover, and it leaves the running replication relationship exactly as it was.

### 5.6 Shipping to a cluster with no copy (Phase 3)

A bubble cluster whose backend holds no replica of the source needs the point moved there before it can be cloned. The controller calls the shipping verb (P0-6), which replicates the specific recovery point to that backend as a cloneable object, and then the clone and placement steps run there as in §5.3 and §5.4.

This is the design's long pole, because the shipping primitive does not exist. The continuous replication engine is pair-scoped and aimed at the DR target, and the S3 backup path is not a cluster-to-cluster push. Neither is an on-demand "ship this one point to cluster X now." P0-6 is that new capability, and Phase 3 does not ship until it does.

### 5.7 Teardown

Deleting the `TestFailover` runs the teardown state before the finalizer clears. The controller removes the PVC and its static PV from the bubble cluster by deleting their `ManifestWork`, removes the `ManagedClusterView` it created on the source, reclaims each clone at the backend, then deletes any snapshot it took (a snapshot named by `spec.recoveryPoint`, or a replicated snapshot it only resolved, was not the drill's to create, so it is left alone), then enumerates by the drill's `test-id` label to prove nothing remains. Only then is the finalizer removed. A teardown that cannot confirm a reclaim holds the object in `TearingDown` with the reason on `status.message`, rather than removing the finalizer and orphaning backend storage.

---

## 6. State Machine

```
Pending
  │  spec admitted, finalizer added
  ▼
Provisioning ──(step: ResolvingSource)──▶ read source PVC/PV on sourceCluster
  │                                         via ManagedClusterView → handle
  │  (step: ResolvingPoint)  fresh snapshot (P0-1) or replicated (P0-4)
  │                                         ← status.report.recoveryPoint set
  │  (step: Shipping)   [Phase 3 only]  ship point to the bubble's backend (P0-6)
  │  (step: Cloning)    clone on the bubble's backend (P0-2)
  │                                         ← status.clones[].cloneID set
  │  (step: Placing)    deliver PV + PVC to bubbleCluster via ManifestWork;
  │                     wait Bound
  ▼
Ready ────────────────────────────────── every PVC Bound; report populated
  │  (holds here until the object is deleted)
  │  .metadata.deletionTimestamp set
  ▼
TearingDown ──(step: Releasing)──▶ delete ManifestWork + ManagedClusterView,
  │                                  reclaim clones, delete drill-taken snapshots
  ▼
(finalizer removed, object gone)

Failed ◀── any step's deadline expires, or a backend, read, or placement call
           fails terminally (object stays; teardown still runs on delete)
```

The machine position lives in `status.step` as a snapshot carrying the state and the deadline that state expires at, so a restored controller times a stalled step out rather than waiting forever. `status.phase` is the coarse view for `kubectl get`. Each step records `status.step.triggered` once its side effect is issued, so a restart between cloning and recording the handle does not clone twice, and a restart between placing and confirming does not place twice.

| Condition                       | Step            | Result                                                                                             |
|---------------------------------|-----------------|----------------------------------------------------------------------------------------------------|
| User deletes mid-drill          | any             | `TearingDown`: reclaim whatever `status.clones` records, then clear the finalizer                  |
| Operator restart                | any             | resume from `status.step`, and `triggered` prevents re-issuing the current step's side effect      |
| Source PVC not found on cluster | ResolvingSource | `Failed`: the view returns nothing for `sourceRef` on `sourceCluster`                              |
| Source cluster not managed      | ResolvingSource | `Failed`: `sourceCluster` is not a registered `ManagedCluster`                                     |
| No replicated point on target   | ResolvingPoint  | `Failed` for a Phase 2 drill: replication has landed nothing on the bubble's backend yet           |
| Backend clone error             | Cloning         | `Failed`. Teardown on delete deletes any drill-taken snapshot                                      |
| Bubble cluster not managed      | Placing         | `Failed`: `bubbleCluster` is not a registered `ManagedCluster`                                     |
| PVC never binds                 | Placing         | `Failed` at the deadline. The clone and snapshot are recorded and reclaimed on delete              |
| Clone or snapshot reclaim fails | Releasing       | hold in `TearingDown` with the reason. The finalizer is not removed until the reclaim is confirmed |

---

## 7. Controller Design

### 7.1 Location

`internal/controller/testfailover_controller.go`, `TestFailoverReconciler`, on the hub. It reuses the `webapi.Client` the replication controllers use for control-plane calls, an OCM `ManagedClusterView` to read the source, and an OCM `ManifestWork` to place the bubble.

### 7.2 Reconciliation Trigger

Watches `TestFailover` and owns the `ManagedClusterView` and `ManifestWork` it creates for each drill, reconciling on their status feedback, which carries the source's projected state and the placed PVC's bind state back to the hub. It requeues on its own step deadline so a stalled step is detected without an external event.

### 7.3 Concurrency and Mutual Exclusion

A drill does not mutate the source, so two drills against one source cannot corrupt it. What they can do is duplicate snapshots and clones, so the controller keys one active drill per resolved `(scope, sourceCluster, sourceRef, bubbleCluster)` and refuses a second with an admission rule where CEL can express it and a `Failed` phase otherwise. This is a lighter lock than the `ReplicationOps` entity lock (`ActiveOpsRef`), because there is no production object whose single-writer invariant has to be defended.

### 7.4 Interaction with Existing Controllers

The drill is invisible to production by construction. It only reads the source, resolves or takes a snapshot, and clones the snapshot, none of which changes the source volume or the live replication. To make "invisible" checkable rather than asserted, the controller fingerprints the source at `ResolvingSource` and again at `Ready`: the source PVC is still bound to the same volume, and the relationship's `ReplicationSlot`, and any `VolumeReplication` or `VolumeGroupReplication` object, are unchanged in state and lag. A mismatch sets `status.report.invariantsHeld: false` and moves the object to `Failed`, because a drill that changed production has failed at its one core promise.

### 7.5 RBAC

New rules: `testfailovers` and `testfailovers/status` and `testfailovers/finalizers` (full), and `create`/`delete`/`get`/`list`/`watch` on `managedclusterviews` and `manifestworks` in a cluster's namespace on the hub. The source's projection and the bubble's PV and PVC are handled by the managed clusters' own agents, so the hub operator needs no direct `persistentvolume`, `persistentvolumeclaim`, or snapshot-API permission. No new permission on any production replication CR either: the controller reads them, and read is a permission the operator already holds.

### 7.6 Cross-Cluster Read and Placement

The hub reaches its managed clusters through OCM, the transport already established for DR. To read the source, the controller creates a `ManagedClusterView` in `sourceCluster`'s namespace naming the PVC and PV, and the view controller on that cluster projects them back into the view's status. To place the bubble, the controller creates a `ManifestWork` in `bubbleCluster`'s namespace carrying the PV and PVC, and the work-agent on that cluster applies them and reports their status back, which is how the hub learns the bubble is bound without a direct connection to either cluster's API server. Both clusters must be registered `ManagedCluster`s (04-bootstrap-ocm.sh). Teardown deletes both objects, and OCM garbage-collects the applied PV and PVC on the bubble cluster.

---

## 8. Backend API Requirements

| Method | Endpoint                                                            | Notes                                                                                                                                                                    |
|--------|---------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| POST   | `.../clusters/{c}/storage-pools/{p}/volumes/{v}/snapshots`          | **Exists** (P0-1). Takes a snapshot of the source volume. Non-disruptive. Backed by `snapshot_controller.add`                                                            |
| POST   | `.../clusters/{c}/consistency-groups/{g}/snapshots`                 | **Exists** (P0-1, group). One group-consistent snapshot across the group's members                                                                                       |
| GET    | `.../clusters/{c}/replication/relationships/{lvol}/latest-snapshot` | **Exists** (P0-4). Resolves the latest replicated snapshot on the DR-target backend. Pure read. Group form: `latest-generation`                                          |
| POST   | `.../clusters/{c}/snapshots/{id}/clone`                             | **Exists** (P0-2). Clones the snapshot into a chosen pool and returns the clone's volume handle. Backed by `snapshot_controller.clone`                                   |
| DELETE | `.../clusters/{c}/snapshots/{id}`                                   | **Exists** (P0-3). Deletes a snapshot the drill took. Idempotent                                                                                                         |
| DELETE | `.../clusters/{c}/storage-pools/{p}/volumes/{id}`                   | **Exists** (P0-3). Reclaims a clone. Idempotent                                                                                                                          |
| POST   | `.../clusters/{c}/replication/ship-snapshot`                        | **New** (P0-6, Phase 3). Ships a named recovery point to a backend that holds no copy. Idempotent per `(recovery-point, target)`. Long-running: returns a handle to poll |

Phases 1 and 2 use only endpoints that exist. The mutating calls the controller may retry after a restart, the snapshot take and the clone, are made idempotent by keying on the drill's `test-id`, so a retry that finds a matching snapshot or clone reuses it. The exact v2 route spellings mirror the existing snapshot and clone routes and are confirmed against the API at implementation time. P0-6's ship is a long-running call and returns a handle the controller polls, with a deadline that moves the step to `Failed` on expiry. The source read and bubble placement are OCM, not backend calls (§7.6).

---

## 9. Configuration

| Field                  | Type   | Default               | Description                                                                                                                                            |
|------------------------|--------|-----------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------|
| `spec.sourceCluster`   | string | (required)            | The OCM `ManagedCluster` the source runs on. Immutable                                                                                                 |
| `spec.sourceNamespace` | string | (required for Volume) | The namespace of the source PVC. Immutable                                                                                                             |
| `spec.sourceRef`       | string | (required)            | The source PVC (Volume) or consistency group (Group) on `sourceCluster`. Immutable                                                                     |
| `spec.bubbleCluster`   | string | (required)            | The OCM `ManagedCluster` to recover onto. Immutable                                                                                                    |
| `spec.bubbleNamespace` | string | `bubble`              | Namespace on the bubble cluster the recovered PVCs are created in. Immutable                                                                           |
| `spec.recoveryPoint`   | string | empty                 | An existing snapshot to clone. Empty resolves the point per `bubbleCluster` (§5.2). Immutable                                                          |
| `spec.ttlSeconds`      | int    | unset                 | Optional maximum lifetime. When set, the drill is torn down after the deadline even without a delete, so a forgotten drill cannot hold a clone forever |

`ttlSeconds` is the only runtime-relevant knob and it is advisory: the controller reads it once at `Ready` and schedules a teardown. Changing the rest of the spec after creation is refused by immutability, because a drill whose target moved mid-flight has no coherent meaning.

---

## 10. Failure Modes and Fallback

| Failure                            | Detection                    | Behavior                                                                                                             |
|------------------------------------|------------------------------|----------------------------------------------------------------------------------------------------------------------|
| Control plane unreachable          | REST call error              | Requeue with backoff, and the step deadline eventually moves the object to `Failed`. No partial state is committed   |
| Source cluster not managed         | ManagedClusterView error     | `Failed` at `ResolvingSource`: `sourceCluster` must be registered on the hub                                         |
| Source PVC not found               | view projects nothing        | `Failed` at `ResolvingSource` with the `sourceRef` in `status.message`                                               |
| No replicated point on the target  | latest-snapshot 404          | `Failed` at `ResolvingPoint` for a Phase 2 drill. Replication has landed nothing on the bubble's backend yet         |
| Bubble cluster not managed         | ManifestWork placement error | `Failed` at `Placing`: the target must be registered on the hub                                                      |
| PVC never binds on the bubble      | ManifestWork status timeout  | `Failed` at `Placing`. The clone is recorded and reclaimed on delete                                                 |
| Fingerprint drift (source changed) | Compare at `Ready`           | `Failed`, `invariantsHeld: false`. This is the guard, not an expected path                                           |
| Reclaim cannot be confirmed        | delete call non-success      | Hold in `TearingDown`, finalizer retained, reason on `status.message`. Never orphan a clone, snapshot, or placed PVC |

Every path degrades to a named state. The one path that must never degrade silently is the fingerprint guard: a drill that cannot prove it left the source untouched fails, rather than passing on the assumption that it did.

---

## 11. Observability

The operator has no metrics or events for a non-disruptive test today, because the capability does not exist. Everything below is new, on the new kind.

### Kubernetes Events

Events land on the `TestFailover` object, which lives on the hub and outlives each step it reports.

| Event                                                         | Type    | Reason                |
|---------------------------------------------------------------|---------|-----------------------|
| The source resolved to volume X on cluster Y                  | Normal  | SourceResolved        |
| The recovery point resolved to snapshot X at time T           | Normal  | RecoveryPointResolved |
| The clone was built on the bubble's backend, source untouched | Normal  | CloneBuilt            |
| The bubble PVC is bound on cluster X and the drill is ready   | Normal  | BubbleReady           |
| The drill failed because the source changed during the drill  | Warning | InvariantViolated     |
| A clone, snapshot, or placed PVC could not be reclaimed       | Warning | ReclaimPending        |

`InvariantViolated` and `ReclaimPending` are the two that matter most. The first says the drill stopped being non-disruptive, and the second is a teardown correctly refusing to orphan storage or a peer object, which is a hold that would otherwise look like a hang.

### Prometheus Metrics

| Metric                                                | Labels                     | Description                                                                      |
|-------------------------------------------------------|----------------------------|----------------------------------------------------------------------------------|
| `simplyblock_testfailover_drills_total`               | `scope`, `phase`, `result` | Counter of completed drills by outcome (`ready`, `failed`, `invariant_violated`) |
| `simplyblock_testfailover_duration_seconds`           | `scope`, `phase`, `step`   | Histogram of time spent per step, for the recover-time estimate                  |
| `simplyblock_testfailover_active`                     | `phase`                    | Gauge of drills currently holding a clone, for capacity watch                    |
| `simplyblock_testfailover_recovery_point_age_seconds` | `scope`                    | Gauge of the recovery point's age at drill time (now minus the snapshot time)    |

The two load-bearing metrics are `simplyblock_testfailover_drills_total` with `result="invariant_violated"`, which is the alert that a test failover stopped being non-disruptive, and `simplyblock_testfailover_active`, which is the alert that clones are accumulating because teardowns are not completing.

---

## 12. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts:
[`tests/test-plan-test-failover.md`](../tests/test-plan-test-failover.md)

- **Unit:** the state-machine transitions and their deadlines, the fingerprint comparison (drift detected and no-drift accepted), the source resolution from a `ManagedClusterView` projection, the recovery-point resolution per `bubbleCluster`, the idempotency keying, and the scope-to-source resolution.
- **Integration:** the reconcile loop against `envtest`, a mock control plane, and a mock OCM (`ManagedClusterView` and `ManifestWork` with status feedback). The whole in-place drill and the DR-target drill, and the teardown that reclaims and proves no leftovers. The restart case per step. The non-disruptiveness guard, asserting a source change fails the drill.
- **E2E:** a live two-cluster DR setup where the replicated point on the target is cloned there, the bubble PVC is placed on the target, a pod boots on it, and the recovered marker matches, with the source and its replication lag asserted untouched.
- **Load / long-running:** none in Phase 1 or 2.

The Phase 3 scenarios (§5.6) become testable only when P0-6 exists. The risk concentrates in the non-disruptiveness guard (§7.4), the cross-cluster read and placement and their status feedback (§7.6), and the teardown reclaim (§5.7): the first is the feature's core promise, and the last is where a bug leaks backend storage or a peer object.

---

## 13. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                       | Owner               |
|-----|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|
| 1   | **Group point on the replica.** `latest_replicated_generation` resolves a group generation on the DR target, but a group-consistent point on a replica was flagged as unconfirmed. Is the generation crash-consistent across members on the target, or only on the source?                                     | SPDK / Backend team |
| 2   | **Clone accounting.** A drill's clone consumes the bubble backend's lvstore object budget for the drill's life. Does the backend expose a per-clone reservation the controller can pre-check, or does a drill risk failing at `Cloning` on a full lvstore with no admission-time warning?                      | SPDK / Backend team |
| 3   | **Leftover proof under a disabled `LIST_VOLUMES`.** The CSI driver does not advertise `LIST_VOLUMES`, so teardown cannot cross-check backend volumes against Kubernetes objects through CSI. Is the label enumeration sufficient, or is a backend enumeration needed to guarantee no leaked clone or snapshot? | SPDK / Backend team |
| 4   | **P0-6 shape (Phase 3).** Is on-demand shipping a new engine mode (a one-shot pair-and-transfer) or a distinct primitive? Its API shape (§8) is provisional until this is decided.                                                                                                                             | SPDK / Backend team |

---

## Appendix A: `testfailover_types.go`

```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestFailoverScope selects what a drill recovers.
// +kubebuilder:validation:Enum=Volume;Group
type TestFailoverScope string

const (
	// TestFailoverScopeVolume recovers a single source volume.
	TestFailoverScopeVolume TestFailoverScope = "Volume"
	// TestFailoverScopeGroup recovers a consistency group from one point.
	TestFailoverScopeGroup TestFailoverScope = "Group"
)

// TestFailoverPhase is the coarse lifecycle phase of a drill.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Failed;TearingDown
type TestFailoverPhase string

const (
	TestFailoverPhasePending      TestFailoverPhase = "Pending"
	TestFailoverPhaseProvisioning TestFailoverPhase = "Provisioning"
	TestFailoverPhaseReady        TestFailoverPhase = "Ready"
	TestFailoverPhaseFailed       TestFailoverPhase = "Failed"
	TestFailoverPhaseTearingDown  TestFailoverPhase = "TearingDown"
)

// TestFailoverStep is one step of a running drill. The enum is the union of every
// phase's steps; which steps belong to which phase is declared by the graph rather
// than by this type.
// +kubebuilder:validation:Enum=ResolvingSource;ResolvingPoint;Shipping;Cloning;Placing;Releasing
type TestFailoverStep string

const (
	TestFailoverStepResolvingSource TestFailoverStep = "ResolvingSource"
	TestFailoverStepResolvingPoint  TestFailoverStep = "ResolvingPoint"
	TestFailoverStepShipping        TestFailoverStep = "Shipping"
	TestFailoverStepCloning         TestFailoverStep = "Cloning"
	TestFailoverStepPlacing         TestFailoverStep = "Placing"
	TestFailoverStepReleasing       TestFailoverStep = "Releasing"
)

// TestFailoverSpec is the request for one non-disruptive test-failover drill.
type TestFailoverSpec struct {
	// Scope selects what the drill recovers. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Scope TestFailoverScope `json:"scope"`

	// SourceCluster is the OCM ManagedCluster the source runs on. The hub reads
	// the source there through a ManagedClusterView. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	SourceCluster string `json:"sourceCluster"`

	// SourceNamespace is the namespace of the source PVC on SourceCluster.
	// Required for scope=Volume. Immutable.
	// +optional
	// +k8s:immutable
	SourceNamespace string `json:"sourceNamespace,omitempty"`

	// SourceRef names the source on SourceCluster: a PersistentVolumeClaim in
	// SourceNamespace (scope=Volume), or a consistency group (scope=Group).
	// Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	SourceRef string `json:"sourceRef"`

	// BubbleCluster is the OCM ManagedCluster to recover onto: the source's own
	// cluster for an in-place test, the DR target, or another cluster. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	BubbleCluster string `json:"bubbleCluster"`

	// RecoveryPoint optionally pins an existing snapshot to clone. Empty resolves
	// the point per BubbleCluster: a fresh source snapshot when it is the source's
	// own cluster, or the latest replicated point on a DR target. Immutable.
	// +optional
	// +k8s:immutable
	RecoveryPoint string `json:"recoveryPoint,omitempty"`

	// BubbleNamespace is the namespace on the bubble cluster where the recovered
	// PVCs are created. Immutable.
	// +kubebuilder:default=bubble
	// +optional
	// +k8s:immutable
	BubbleNamespace string `json:"bubbleNamespace,omitempty"`

	// TTLSeconds is an optional maximum lifetime: the drill is torn down after it
	// even without a delete, so a forgotten drill cannot hold a clone forever.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLSeconds *int64 `json:"ttlSeconds,omitempty"`
}

// TestFailoverClone is one recovered volume: the source it came from, the
// snapshot and clone the drill built, and the PVC placed on the bubble cluster.
type TestFailoverClone struct {
	// SourceRef is the source volume (or group member) the recovered volume maps to.
	SourceRef string `json:"sourceRef"`
	// SourceHandle is the source volume's backend handle, read from its PV.
	// +optional
	SourceHandle string `json:"sourceHandle,omitempty"`
	// SnapshotID is the recovery-point snapshot.
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`
	// SnapshotTaken is true when the drill created SnapshotID (rather than
	// resolving a replicated one or reusing a pinned one), so teardown knows
	// whether to delete it.
	// +optional
	SnapshotTaken bool `json:"snapshotTaken,omitempty"`
	// CloneID is the backend id of the writable clone.
	// +optional
	CloneID string `json:"cloneID,omitempty"`
	// PVCName is the bound PVC in the bubble namespace on the bubble cluster.
	// +optional
	PVCName string `json:"pvcName,omitempty"`
	// SizeBytes is the recovered volume's size.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// TestFailoverReport is the evidence a drill produces.
type TestFailoverReport struct {
	// BubbleCluster is the cluster the drill recovered onto.
	// +optional
	BubbleCluster string `json:"bubbleCluster,omitempty"`
	// RecoveryPoint is the snapshot or group generation the drill recovered.
	// +optional
	RecoveryPoint string `json:"recoveryPoint,omitempty"`
	// RecoveryPointTime is when that point was taken.
	// +optional
	RecoveryPointTime *metav1.Time `json:"recoveryPointTime,omitempty"`
	// RecoveryPointAgeSeconds is the drill time minus the recovery-point time.
	// +optional
	RecoveryPointAgeSeconds int64 `json:"recoveryPointAgeSeconds,omitempty"`
	// InvariantsHeld is true only when the source fingerprint taken before the
	// drill matches the one taken at Ready. A Ready drill with this false is a
	// defect.
	// +optional
	InvariantsHeld bool `json:"invariantsHeld,omitempty"`
}

// TestFailoverStepSnapshot is the durable position of the drill's machine.
type TestFailoverStepSnapshot struct {
	// +optional
	State TestFailoverStep `json:"state,omitempty"`
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
}

// TestFailoverStatus is the observed state of a drill.
type TestFailoverStatus struct {
	// +optional
	Phase TestFailoverPhase `json:"phase,omitempty"`
	// +optional
	Step TestFailoverStepSnapshot `json:"step,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Triggered records that the current step's side effect was issued, so a
	// restart does not repeat it.
	// +optional
	Triggered bool `json:"triggered,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=sourceRef
	Clones []TestFailoverClone `json:"clones,omitempty"`
	// +optional
	Report *TestFailoverReport `json:"report,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	ReadyAt *metav1.Time `json:"readyAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=tfo
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceRef`
// +kubebuilder:printcolumn:name="On",type=string,JSONPath=`.spec.sourceCluster`
// +kubebuilder:printcolumn:name="Bubble",type=string,JSONPath=`.spec.bubbleCluster`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=`.status.step.state`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TestFailover is a one-way, non-disruptive test-failover drill. It recovers a
// source volume, or a consistency group, from a snapshot into an isolated
// namespace on a chosen cluster as bound PVCs, without touching the source: the
// recovery point is a snapshot and the result is a clone. The hub reads the
// source on its cluster and places the bubble on the recovery cluster through
// OCM. Deleting the object reclaims the clones and any snapshots the drill took.
type TestFailover struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TestFailoverSpec   `json:"spec,omitempty"`
	Status TestFailoverStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TestFailoverList contains a list of TestFailover.
type TestFailoverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TestFailover `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TestFailover{}, &TestFailoverList{})
}
```
