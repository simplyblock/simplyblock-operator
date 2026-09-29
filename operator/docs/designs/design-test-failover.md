# Design Document: Non-Disruptive Test Failover

**Status:** Draft  
**Author:** Israel Geoffrey (geoffrey1330)  
**Date:** 2026-09-29  
**Test Plan:** [`tests/test-plan-test-failover.md`](../tests/test-plan-test-failover.md)

---

## Phasing Overview

| Phase       | Status  | Where the test runs                          | Recovery point                                   | New backend capability                                                  | Sections              |
|-------------|---------|----------------------------------------------|--------------------------------------------------|-------------------------------------------------------------------------|-----------------------|
| **Phase 1** | Planned | The source's own simplyblock cluster         | A snapshot of the source volume, on that cluster | None. Snapshot, clone, and delete all exist                             | §4, §5.1–§5.3, §6, §7 |
| **Phase 2** | Planned | A separate simplyblock cluster (a DR target) | The latest replicated snapshot on that target    | On-demand shipping of a recovery point to a named target backend (P0-4) | §5.4                  |

Phase 1 is the whole of the near-term feature and it stands alone. A single simplyblock cluster is all it needs, because the recovery point is a snapshot of the source volume taken on that same cluster, and cloning a snapshot into an isolated namespace is built entirely out of primitives that already ship. Phase 2 is for the day there is a second simplyblock cluster to fail a test over to. It reuses the same `TestFailover` object and the same isolation contract, and differs only in that the recovery point is a cross-cluster replicated snapshot and, for a separate backend, has to be shipped there first.

The single-cluster framing is deliberate. simplyblock replication requires two distinct clusters, so a single-cluster deployment has no cross-cluster replicated snapshot to recover from. Its recoverable point is a snapshot of the source volume, and that is what Phase 1 clones.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                         | Kind                    | Blocks  | Status                                                                                                                                                   |
|------|--------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------|----------------------------------------------------------------------------------------------------------------------------------------------------------|
| P0-1 | Take a snapshot of the source volume, and a group-consistent snapshot of a consistency group, without disturbing the running volume  | Control plane (`sbcli`) | Phase 1 | Shipped: `snapshot_controller.add` for a volume, and the group snapshot primitive (`bdev_lvol_snapshot_group`, design-consistency-groups.md) for a group |
| P0-2 | Clone a snapshot into a writable volume in a chosen pool, and return the clone's volume handle                                       | Control plane (`sbcli`) | Phase 1 | Shipped: `snapshot_controller.clone`, and the CSI clone-from-snapshot path                                                                               |
| P0-3 | Delete a snapshot and delete a volume, both idempotent                                                                               | Control plane (`sbcli`) | Phase 1 | Shipped                                                                                                                                                  |
| P0-4 | On-demand shipping of a specific snapshot or group generation to a named target storage cluster's backend, followed by a clone there | Control plane (`sbcli`) | Phase 2 | Not shipped. The long pole of Phase 2. Today's cross-cluster reach is the continuous replication engine or the S3 backup path, neither an on-demand push |

Phase 1 has no unmet backend prerequisite. Every primitive it stands on, taking a snapshot, cloning it, and deleting both, already ships, which is why the near-term feature is a new controller over existing calls rather than new storage work. Phase 2 is the exception: P0-4 is genuinely new, because moving one recovery point to an arbitrary cluster's backend on demand is a capability the engine does not have.

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

A test failover proves an application can be recovered from a point-in-time copy, in isolation, without disturbing the running production. On a single simplyblock cluster the recovery point is a snapshot of the source volume, taken on that same cluster. The drill takes it (or reuses one), clones it into a writable volume, and hands that clone to the operator as a bound PVC in an isolated namespace, `bubble` by default. The operator boots the application there, confirms the data is intact, and deletes the drill, which reclaims the clone and the snapshot it took. The source volume serves throughout, its data and its I/O never touched.

The feature is a new namespaced CRD, `TestFailover`, and its controller. Creating one runs the drill and leaves a bound PVC per source volume in the bubble namespace. Deleting one tears the drill down through a finalizer and reclaims what it created. Because the recovery point is a snapshot and the result is a clone, nothing the drill does mutates the source, which is what makes it non-disruptive.

`TestFailover` has one mode today and room for a second. In the same-cluster mode (Phase 1) the snapshot, the clone, and the bubble PVC all live on the source's own simplyblock cluster. In the cross-cluster mode (Phase 2, `spec.targetClusterID` set) the recovery point is a replicated snapshot on a separate DR-target cluster, and for a separate backend it is shipped there first. Both are driven by the same object and the same state machine, and differ only in where the recovery point comes from.

---

## 1. Background

simplyblock has a real failover, driven either by Ramen (`DRPC.spec.action: Failover`) or imperatively by the simplyblock-native `ReplicationOps` CR, and both reach a backend that promotes a replicated copy on a second cluster. That copy is, mechanically, a clone of the last replicated snapshot on the target, so the real failover is a clone-and-promote of a recovery point that lives on another cluster.

Two facts shape a test failover. First, replication is between two clusters: `add_target` refuses `target_cluster_id == cluster_id` with "A cluster cannot replicate to itself" (`simplyblock_core/controllers/replication_policy_controller.py`). So a single simplyblock cluster has no cross-cluster replicated snapshot, and its only recoverable point is a snapshot of the source volume itself. Second, the primitives to recover from such a snapshot already exist: `snapshot_controller.add` takes one, `snapshot_controller.clone` clones it into a writable volume in a chosen pool, and both a snapshot and a volume can be deleted. A clone of a snapshot is a first-class volume with its own handle, which is exactly what CSI static provisioning adopts as a `PersistentVolume`.

What is missing is the orchestration: an object that takes the recovery point, clones it into an isolated namespace, proves the copy is recoverable, and tears it down, all without touching the source. Ramen orchestrates none of it, because Ramen only fails over and relocates for real. This design is that object.

---

## 2. Goals and Non-Goals

### Goals

- A `TestFailover` CRD and controller that, from one object, produce a bound PVC per source volume in an isolated namespace, backed by a clone of a snapshot of the source.
- Non-disruptive by construction. The recovery point is a snapshot and the result is a clone, so the source volume's data and I/O are never touched, and the controller records a before-and-after fingerprint of the source so a regression is caught rather than assumed.
- Volume-scoped and consistency-group-scoped drills. A group drill takes one group-consistent snapshot and produces one PVC per member from it.
- Same-cluster today, cross-cluster later (§5.4), behind one object and one state machine.
- A finalizer-driven teardown that reclaims the clone, deletes the snapshot the drill took, and proves nothing test-labeled remains.
- Restart safety. The controller records which side effect each step issued, so a restart mid-drill resumes rather than repeats.

### Non-Goals

- **Bringing up the application.** The object produces bound PVCs and stops. The workload that consumes them is the operator's to deploy. An application lifecycle and its workload spec are a separate concern, out of scope here.
- **A test failback.** The drill is one-way. Tearing it down reclaims the clone. There is no promote-back.
- **Cross-cluster in Phase 1.** A separate DR-target cluster and its shipped recovery point are Phase 2 (§5.4), gated on a backend primitive that does not exist (P0-4).
- **Snapshot scheduling and evidence export.** A recurring schedule and a signed test report are a layer above this object and are out of scope here.
- **Replacing Ramen's own test paths.** Ramen has no non-disruptive test. This design does not add one to Ramen. It is a simplyblock-native object.

---

## 3. Architecture Overview

```
                        ┌───────────────────────────────────────────────┐
                        │ operator                                       │
                        │                                                │
   TestFailover CR ────▶│  ┌──────────────────────────────────────────┐ │
   (spec: scope, ref,   │  │ TestFailoverReconciler                    │ │
    recoveryPoint,      │  │  1. take or resolve recovery snapshot     │ │
    bubbleNamespace)    │  │       (P0-1, or an existing snapshot)     │ │
                        │  │  2. [Phase 2] ship point to target (P0-4) │ │
                        │  │  3. clone the snapshot         (P0-2)     │ │
                        │  │  4. adopt clone as static PV + bind PVC   │ │
                        │  │  5. Ready; hold until deleted             │ │
                        │  │  6. finalizer: reclaim clone + snapshot   │ │
                        │  └──────────────────────────────────────────┘ │
                        │        │ writes                    │ reads      │
                        │        ▼                           ▼            │
                        │  status.clones[]              source volume     │
                        │  status.report               (read-only,       │
                        │  bubble ns: PV + PVC          fingerprinted)    │
                        └────────┼───────────────────────────────────────┘
                                 │ REST (webapi.Client)
                                 ▼
                        ┌───────────────────────────────────────────────┐
                        │ control plane (sbcli)                          │
                        │  POST   .../volumes/{v}/snapshots       P0-1   │
                        │  POST   .../consistency-groups/{g}/snapshots P0-1 (group) │
                        │  POST   .../snapshots/{id}/clone        P0-2   │
                        │  DELETE .../snapshots/{id}              P0-3   │
                        │  DELETE .../volumes/{id}                P0-3   │
                        │  POST   .../replication/ship-snapshot   P0-4 (Phase 2) │
                        └───────────────────────────────────────────────┘
```

The controller runs on the cluster the test targets, which in Phase 1 is the source's own cluster. It reads the source only to fingerprint it, never to change it. The clone the backend returns is a first-class volume with its own handle, so the controller adopts it through ordinary CSI static provisioning: a `PersistentVolume` naming the clone's handle and a `PersistentVolumeClaim` bound to it in the bubble namespace. No Kubernetes `VolumeSnapshot` object is involved, because the snapshot and the clone are backend operations the controller drives directly.

The trust boundary is the control-plane REST API. The operator authenticates with its cluster secret, exactly as the `ReplicationOps` controller does. Phase 1 uses only endpoints that exist. Phase 2 adds one new server-side surface, P0-4.

---

## 4. API Design — New CRD

`TestFailover` is a namespaced object in the `storage.simplyblock.io` group. One object drives one drill. Its spec is immutable, because the object is a request and a drill whose target moved under the controller mid-flight has no coherent meaning. The full type is [Appendix A](#appendix-a-testfailover_typesgo), and the body shows only the fields an argument turns on.

### 4.1 `TestFailover` Spec

The spec names what to recover and where to put the result. `scope` and `ref` resolve the source: `Volume` names one source `PersistentVolumeClaim` in the CR's own namespace, `Group` names one consistency group and produces a group-consistent set.

```go
// Scope selects what the drill recovers. Immutable.
// +kubebuilder:validation:Enum=Volume;Group
// +k8s:immutable
// +kubebuilder:validation:Required
Scope TestFailoverScope `json:"scope"`

// RecoveryPoint optionally pins an existing snapshot (or group generation) to
// clone. When empty, the drill takes a fresh snapshot of the source for a
// well-defined point at drill time. Immutable.
// +k8s:immutable
// +optional
RecoveryPoint string `json:"recoveryPoint,omitempty"`

// TargetClusterID selects the cross-cluster mode (Phase 2): the cluster the
// drill runs on. Empty means the source's own cluster (Phase 1). Immutable.
// +k8s:immutable
// +optional
TargetClusterID string `json:"targetClusterID,omitempty"`
```

`recoveryPoint` is the choice between recovering from a point the operator already has and recovering from a fresh one. Empty is the common case and takes a snapshot at drill time, which is the closest thing to "fail over to now." `bubbleNamespace` defaults to `bubble` and is where every recovered PVC lands. Isolating the result in its own namespace is what keeps a drill from colliding with the source workload's PVCs, which carry the same names.

### 4.2 `TestFailover` Status

The status carries the state-machine position, the resolved recovery point, one entry per recovered volume, and a report. `phase` is the coarse lifecycle and `step` is the durable machine position with its deadline (§6). `clones` is the list the finalizer reclaims from, and `report` is the evidence a reader takes away.

```go
// Clones is one entry per recovered volume: the source it came from, the
// snapshot and clone the drill built, and the PVC bound to it in the bubble.
// +optional
// +listType=map
// +listMapKey=sourceRef
Clones []TestFailoverClone `json:"clones,omitempty"`

// Report is the drill's evidence: the point recovered, its age, and whether
// the source was untouched. Populated as the drill reaches Ready.
// +optional
Report *TestFailoverReport `json:"report,omitempty"`
```

An invariant the controller enforces and the status records: the drill is non-disruptive. `status.report.invariantsHeld` is set only when the fingerprint of the source taken before the drill matches the one taken after (§7.4). A drill that reached `Ready` with `invariantsHeld: false` is a defect, not a passing test.

The object owns a finalizer, `storage.simplyblock.io/testfailover-teardown`. Deletion runs the teardown state (§6) before the finalizer is removed, so a clone or a drill-taken snapshot is never orphaned by a delete that races the controller.

---

## 5. Core Mechanism

### 5.1 The recovery point (same-cluster)

The recovery point is a snapshot of the source volume on the source's own cluster. When `spec.recoveryPoint` is set, the controller uses that existing snapshot. When it is empty, the controller takes a fresh one (P0-1), which gives the drill a well-defined point at its start. Taking a snapshot does not disturb the running volume: it adds a point to the volume's chain and copies no data, and the drill deletes the snapshot it took on teardown. For a group drill (`scope: Group`) the controller takes a single group-consistent snapshot of the whole consistency group, so every member is captured at one point.

There is no promote and no commit here, because there is nothing to promote. A single cluster has no replicated copy to fail over to. The drill recovers from a snapshot, which is inherently a read of a past state and never a mutation of the present one.

### 5.2 Cloning and recovering the PVC into the bubble

The controller clones the recovery-point snapshot into a writable volume in a chosen pool (P0-2), and the backend returns the clone's volume handle. The clone is a first-class volume, so the controller adopts it the way any pre-existing backend volume is adopted: a static `PersistentVolume` whose `spec.csi.volumeHandle` is the clone's handle and whose `spec.claimRef` names a `PersistentVolumeClaim` in the bubble namespace, bound to it. No `VolumeSnapshot` is involved, because the snapshot and clone are backend operations the controller already drove. The PV carries `persistentVolumeReclaimPolicy: Retain`, so deleting the PVC does not delete the clone, which the controller reclaims at the backend on teardown (§5.3).

The result is one bound PVC per source volume in the bubble namespace, sized from the source PVC's request. For a group drill, one PVC per member from the one group snapshot, which is what makes the recovered set crash-consistent.

### 5.3 Teardown

Deleting the `TestFailover` runs the teardown state before the finalizer clears. The controller deletes each PVC and its static PV, then reclaims each clone at the backend, then deletes the snapshot it took (a snapshot named by `spec.recoveryPoint` was not the drill's to create, so it is left alone), then enumerates by the drill's `test-id` label to prove nothing remains. Only then is the finalizer removed. A teardown that cannot confirm a reclaim holds the object in `TearingDown` with the reason on `status.message`, rather than removing the finalizer and orphaning backend storage.

### 5.4 The cross-cluster recovery point (Phase 2)

The cross-cluster mode changes only where the recovery point comes from. When `spec.targetClusterID` names a separate simplyblock cluster, the drill runs there, and its recovery point is the latest replicated snapshot on that target rather than a fresh snapshot of the source. When the target's backend is not the source's backend, that point does not exist there yet and has to be shipped first (P0-4), after which the clone and recovery steps run on the target exactly as in §5.2.

This is the design's long pole, because the shipping primitive does not exist. The continuous replication engine is pair-scoped and continuous, aimed at the DR target, and the S3 backup path is not a cluster-to-cluster push. Neither is an on-demand "ship this one point to cluster X now." P0-4 is that new capability, and Phase 2 does not ship until it does.

---

## 6. State Machine

```
Pending
  │  spec admitted, finalizer added
  ▼
Provisioning ──(step: Snapshotting)──▶ take or resolve the recovery snapshot (P0-1)
  │                                      ← status.report.recoveryPoint set
  │  (step: Shipping)   [cross-cluster only] ship point to target (P0-4)
  │  (step: Cloning)    clone the snapshot (P0-2)
  │                                      ← status.clones[].cloneID set
  │  (step: Binding)    static PV + PVC per volume; wait Bound
  ▼
Ready ────────────────────────────────── every PVC Bound; report populated
  │  (holds here until the object is deleted)
  │  .metadata.deletionTimestamp set
  ▼
TearingDown ──(step: Releasing)──▶ delete PVCs + PVs, reclaim clones,
  │                                  delete drill-taken snapshots, prove no leftovers
  ▼
(finalizer removed, object gone)

Failed ◀── any step's deadline expires, or a backend call fails terminally
           (object stays; teardown still runs on delete)
```

The machine position lives in `status.step` as a snapshot carrying the state and the deadline that state expires at, so a restored controller times a stalled step out rather than waiting forever. `status.phase` is the coarse view for `kubectl get`. Each step records `status.step.triggered` once its side effect is issued, so a restart between issuing a clone and recording its handle does not clone twice.

| Condition                       | Step         | Result                                                                                             |
|---------------------------------|--------------|----------------------------------------------------------------------------------------------------|
| User deletes mid-drill          | any          | `TearingDown`: reclaim whatever `status.clones` records, then clear the finalizer                  |
| Operator restart                | any          | resume from `status.step`, and `triggered` prevents re-issuing the current step's side effect      |
| Source volume not found         | Snapshotting | `Failed`: the source `ref` resolves to nothing to snapshot                                         |
| Backend snapshot error          | Snapshotting | `Failed`. No clone exists, so teardown has nothing to reclaim                                      |
| Backend clone error             | Cloning      | `Failed`. Teardown on delete deletes the drill-taken snapshot                                      |
| PVC never binds                 | Binding      | `Failed` at the deadline. The clone and snapshot are recorded and reclaimed on delete              |
| Clone or snapshot reclaim fails | Releasing    | hold in `TearingDown` with the reason. The finalizer is not removed until the reclaim is confirmed |

---

## 7. Controller Design

### 7.1 Location

`internal/controller/testfailover_controller.go`, `TestFailoverReconciler`. It reuses the `webapi.Client` the replication controllers use for control-plane calls, and standard CSI static provisioning to bind a `PersistentVolume` to the clone the backend returns.

### 7.2 Reconciliation Trigger

Watches `TestFailover` and owns the `PersistentVolume` and `PersistentVolumeClaim` objects it creates, so their status changes requeue the owner. It requeues on its own step deadline so a stalled step is detected without an external event.

### 7.3 Concurrency and Mutual Exclusion

A drill does not mutate the source, so two drills against one source cannot corrupt it. What they can do is duplicate snapshots and clones, so the controller keys one active drill per resolved `(scope, ref, targetClusterID)` and refuses a second with an admission rule where CEL can express it and a `Failed` phase otherwise. This is a lighter lock than the `ReplicationOps` entity lock (`ActiveOpsRef`), because there is no production object whose single-writer invariant has to be defended.

### 7.4 Interaction with Existing Controllers

The drill is invisible to production by construction. It only reads the source, snapshots it, and clones the snapshot, none of which changes the source volume. To make "invisible" checkable rather than asserted, the controller fingerprints the source before the first side effect and again at `Ready`: the source PVC is still bound to the same PV and backend volume, and, where a replication relationship exists, its `ReplicationSlot` and any `VolumeReplication` or `VolumeGroupReplication` object are unchanged in state. A mismatch sets `status.report.invariantsHeld: false` and moves the object to `Failed`, because a drill that changed production has failed at its one core promise.

### 7.5 RBAC

New rules: `testfailovers` and `testfailovers/status` and `testfailovers/finalizers` (full), `create`/`delete`/`get`/`list`/`watch` on `persistentvolumes` (cluster-scoped) and on `persistentvolumeclaims` in the bubble namespace. No permission on the Kubernetes snapshot API is needed, because the design does not use it. No new permission on any production replication CR either: the controller reads them, and read is a permission the operator already holds.

---

## 8. Backend API Requirements

| Method | Endpoint                                                   | Notes                                                                                                                                                                  |
|--------|------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| POST   | `.../clusters/{c}/storage-pools/{p}/volumes/{v}/snapshots` | **Exists** (P0-1). Takes a snapshot of the source volume. Non-disruptive. Backed by `snapshot_controller.add`                                                          |
| POST   | `.../clusters/{c}/consistency-groups/{g}/snapshots`        | **Exists** (P0-1, group). One group-consistent snapshot across the group's members                                                                                     |
| POST   | `.../clusters/{c}/snapshots/{id}/clone`                    | **Exists** (P0-2). Clones the snapshot into a chosen pool and returns the clone's volume handle. Backed by `snapshot_controller.clone`                                 |
| DELETE | `.../clusters/{c}/snapshots/{id}`                          | **Exists** (P0-3). Deletes a snapshot the drill took. Idempotent: deleting an already-gone snapshot returns success                                                    |
| DELETE | `.../clusters/{c}/storage-pools/{p}/volumes/{id}`          | **Exists** (P0-3). Reclaims a clone. Idempotent                                                                                                                        |
| POST   | `.../clusters/{c}/replication/ship-snapshot`               | **New** (P0-4, Phase 2). Ships a named recovery point to a target cluster's backend. Idempotent per `(recovery-point, target)`. Long-running: returns a handle to poll |

Phase 1 uses only endpoints that exist. The two mutating calls the controller may retry after a restart, the snapshot take and the clone, are made idempotent by keying on the drill's `test-id`, so a retry that finds a matching snapshot or clone reuses it rather than making another. The exact v2 route spellings above mirror the existing snapshot and clone routes and are confirmed against the API at implementation time. P0-4's ship is a long-running call and returns a handle the controller polls, with a deadline that moves the step to `Failed` on expiry.

---

## 9. Configuration

| Field                  | Type   | Default  | Description                                                                                                                                            |
|------------------------|--------|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------|
| `spec.bubbleNamespace` | string | `bubble` | Namespace the recovered PVCs are created in. Immutable after creation                                                                                  |
| `spec.recoveryPoint`   | string | empty    | An existing snapshot to clone. Empty takes a fresh snapshot at drill time. Immutable                                                                   |
| `spec.targetClusterID` | string | empty    | Empty is the same-cluster mode. A cluster id selects cross-cluster mode (Phase 2). Immutable                                                           |
| `spec.ttlSeconds`      | int    | unset    | Optional maximum lifetime. When set, the drill is torn down after the deadline even without a delete, so a forgotten drill cannot hold a clone forever |

`ttlSeconds` is the only runtime-relevant knob and it is advisory: the controller reads it once at `Ready` and schedules a teardown. Changing the rest of the spec after creation is refused by immutability, because a drill whose target moved mid-flight has no coherent meaning.

---

## 10. Failure Modes and Fallback

| Failure                               | Detection               | Behavior                                                                                                                                                          |
|---------------------------------------|-------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Control plane unreachable             | REST call error         | Requeue with backoff, and the step deadline eventually moves the object to `Failed`. No partial state is committed                                                |
| Source volume or group not found      | snapshot call 404       | `Failed` at `Snapshotting` with the ref in `status.message`                                                                                                       |
| Pinned `recoveryPoint` snapshot gone  | clone call 404          | `Failed` at `Cloning`. A pinned point that vanished is the operator's to re-choose                                                                                |
| Bubble namespace has a name collision | PVC create conflict     | `Failed` at `Binding` with the conflicting name in `status.message`. The namespace isolates production, so a collision is with another drill, not with production |
| Fingerprint drift (source changed)    | Compare at `Ready`      | `Failed`, `invariantsHeld: false`. This is the guard, not an expected path                                                                                        |
| Reclaim cannot be confirmed           | delete call non-success | Hold in `TearingDown`, finalizer retained, reason on `status.message`. Never orphan a clone or a drill-taken snapshot by clearing the finalizer early             |

Every path degrades to a named state. The one path that must never degrade silently is the fingerprint guard: a drill that cannot prove it left the source untouched fails, rather than passing on the assumption that it did.

---

## 11. Observability

The operator has no metrics or events for a non-disruptive test today, because the capability does not exist. Everything below is new, on the new kind.

### Kubernetes Events

Events land on the `TestFailover` object, which the operator owns and which outlives each step it reports.

| Event                                                              | Type    | Reason             |
|--------------------------------------------------------------------|---------|--------------------|
| The recovery snapshot was taken at time T                          | Normal  | RecoveryPointTaken |
| The clone was built and the source was not touched                 | Normal  | CloneBuilt         |
| Every recovered PVC is bound and the drill is ready                | Normal  | BubbleReady        |
| The drill failed because the source changed during the drill       | Warning | InvariantViolated  |
| A clone or snapshot could not be reclaimed and teardown is holding | Warning | ReclaimPending     |

`InvariantViolated` and `ReclaimPending` are the two that matter most. The first says the drill stopped being non-disruptive, and the second is a teardown correctly refusing to orphan backend storage, which is a hold that would otherwise look like a hang.

### Prometheus Metrics

| Metric                                                | Labels                    | Description                                                                      |
|-------------------------------------------------------|---------------------------|----------------------------------------------------------------------------------|
| `simplyblock_testfailover_drills_total`               | `scope`, `mode`, `result` | Counter of completed drills by outcome (`ready`, `failed`, `invariant_violated`) |
| `simplyblock_testfailover_duration_seconds`           | `scope`, `mode`, `step`   | Histogram of time spent per step, for the recover-time estimate                  |
| `simplyblock_testfailover_active`                     | `mode`                    | Gauge of drills currently holding a clone, for capacity watch                    |
| `simplyblock_testfailover_recovery_point_age_seconds` | `scope`                   | Gauge of the recovery point's age at drill time (now minus the snapshot time)    |

The two load-bearing metrics are `simplyblock_testfailover_drills_total` with `result="invariant_violated"`, which is the alert that a test failover stopped being non-disruptive, and `simplyblock_testfailover_active`, which is the alert that clones are accumulating because teardowns are not completing.

---

## 12. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts:
[`tests/test-plan-test-failover.md`](../tests/test-plan-test-failover.md)

- **Unit:** the state-machine transitions and their deadlines, the fingerprint comparison (drift detected and no-drift accepted), the take-versus-pin recovery-point choice, the idempotency keying, and the scope-to-source resolution.
- **Integration:** the reconcile loop against `envtest` with a mock control plane. The whole same-cluster drill (snapshot, clone, adopt, bind, ready) and the teardown that reclaims and proves no leftovers. The restart case per step, asserting no second snapshot or clone. The non-disruptiveness guard, asserting a source change fails the drill.
- **E2E:** a live cluster where the source volume is snapshotted, cloned into `bubble`, a pod boots on the clone, and the recovered marker matches, with the source volume's I/O and data asserted untouched across the drill.
- **Load / long-running:** none in Phase 1.

The cross-cluster scenarios (§5.4) become testable only in Phase 2, when P0-4 exists. The risk concentrates in the non-disruptiveness guard (§7.4) and the teardown reclaim (§5.3): the first is the feature's core promise, and the second is where a bug leaks backend storage.

---

## 13. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                             | Owner               |
|-----|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|
| 1   | **Group snapshot accounting.** A group drill takes a group-consistent snapshot and a clone per member, consuming the cluster's lvstore object budget for the drill's life. Does the backend expose a per-clone reservation the controller can pre-check, or does a group drill risk failing at `Cloning` on a full lvstore with no admission-time warning?           | SPDK / Backend team |
| 2   | **Leftover proof under a disabled `LIST_VOLUMES`.** The CSI driver does not advertise `LIST_VOLUMES`, so the teardown's "prove no leftovers" step cannot cross-check backend volumes against Kubernetes objects through CSI. Is the label enumeration on Kubernetes objects sufficient, or is a backend enumeration needed to guarantee no leaked clone or snapshot? | SPDK / Backend team |
| 3   | **P0-4 shape (Phase 2).** Is on-demand shipping a new engine mode (a one-shot pair-and-transfer) or a distinct primitive? Its API shape (§8) is provisional until this is decided.                                                                                                                                                                                   | SPDK / Backend team |

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
	// TestFailoverScopeVolume recovers a single source volume named by a PVC.
	TestFailoverScopeVolume TestFailoverScope = "Volume"
	// TestFailoverScopeGroup recovers a consistency group from one snapshot.
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
// mode's steps; which steps belong to which mode is declared by the graph rather
// than by this type.
// +kubebuilder:validation:Enum=Snapshotting;Shipping;Cloning;Binding;Releasing
type TestFailoverStep string

const (
	TestFailoverStepSnapshotting TestFailoverStep = "Snapshotting"
	TestFailoverStepShipping     TestFailoverStep = "Shipping"
	TestFailoverStepCloning      TestFailoverStep = "Cloning"
	TestFailoverStepBinding      TestFailoverStep = "Binding"
	TestFailoverStepReleasing    TestFailoverStep = "Releasing"
)

// TestFailoverSpec is the request for one non-disruptive test-failover drill.
type TestFailoverSpec struct {
	// Scope selects what the drill recovers. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Scope TestFailoverScope `json:"scope"`

	// Ref names the source Scope resolves: a PersistentVolumeClaim in this CR's
	// namespace (scope=Volume) or a consistency group (scope=Group). Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Ref string `json:"ref"`

	// RecoveryPoint optionally pins an existing snapshot or group generation to
	// clone. Empty takes a fresh snapshot of the source at drill time. Immutable.
	// +optional
	// +k8s:immutable
	RecoveryPoint string `json:"recoveryPoint,omitempty"`

	// TargetClusterID selects the cross-cluster mode (Phase 2): the cluster the
	// drill runs on. Empty means the source's own cluster. Immutable.
	// +optional
	// +k8s:immutable
	TargetClusterID string `json:"targetClusterID,omitempty"`

	// BubbleNamespace is where the recovered PVCs are created. Immutable.
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
// snapshot and clone the drill built, and the PVC bound to it in the bubble.
type TestFailoverClone struct {
	// SourceRef is the source PVC (or group member) the recovered volume maps to.
	SourceRef string `json:"sourceRef"`
	// SnapshotID is the recovery-point snapshot. Marked when the drill took it,
	// so teardown deletes only what it created.
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`
	// SnapshotTaken is true when the drill created SnapshotID (rather than reusing
	// a pinned one), so teardown knows whether to delete it.
	// +optional
	SnapshotTaken bool `json:"snapshotTaken,omitempty"`
	// CloneID is the backend id of the writable clone.
	// +optional
	CloneID string `json:"cloneID,omitempty"`
	// PVCName is the bound PVC in the bubble namespace.
	// +optional
	PVCName string `json:"pvcName,omitempty"`
	// SizeBytes is the recovered volume's size.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// TestFailoverReport is the evidence a drill produces.
type TestFailoverReport struct {
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
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=`.spec.ref`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=`.status.step.state`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TestFailover is a one-way, non-disruptive test-failover drill. It recovers a
// source volume, or a consistency group, from a snapshot into an isolated
// namespace as bound PVCs, without touching the source: the recovery point is a
// snapshot and the result is a clone. Deleting the object reclaims the clones
// and the snapshots the drill took.
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
