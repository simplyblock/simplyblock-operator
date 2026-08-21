# Design Document: Policy-Driven Snapshot Replication

**Status:** Implemented  
**Author:** Israel Geoffrey  
**Date:** 2026-08-19 (revised 2026-08-20)  
**GitHub Issue:** Refactor snapshot replication: introduce multi-target and policy-driven API

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API Design — New CRDs](#4-api-design--new-crds)
5. [Annotation Mechanism](#5-annotation-mechanism)
6. [Backend API Changes](#6-backend-api-changes)
7. [State Machine — ReplicationSlot](#7-state-machine--replicationslot)
8. [Operator Reconciler Flow](#8-operator-reconciler-flow)
9. [ReplicationOps — User-Driven Failover and Failback](#9-replicationops--user-driven-failover-and-failback)
10. [Observability](#10-observability)
11. [Testing Strategy](#11-testing-strategy)
12. [Open Questions](#12-open-questions)

---

## 1. Background

The current snapshot replication model is volume-centric: `replication_start` and
`replication_stop` are called directly on individual volumes, carrying a target cluster ID
as a parameter. This has several practical limitations:

| Limitation | Impact |
|---|---|
| No concept of a replication target as a resource | Cannot manage multiple target clusters (e.g. one for DR, one for live migration) |
| No shared cadence or retention policy | Every volume is configured individually; no consistent scheduling |
| Failover is per-volume only | No way to atomically fail over all volumes replicating to a given cluster |
| `replication_start` / `replication_stop` are imperative | No persistent record of desired state; operator cannot reconcile toward it |
| Operator has no visibility into replication intent | StorageClass and PVC have no first-class way to express replication requirements |

This design introduces a **three-tier CRD hierarchy** replacing the ad-hoc per-volume model:

| CRD | Cardinality | Responsibility |
|---|---|---|
| `ReplicationPair` | One per site pair | Declares source + target cluster; manages the backend `ReplicationTarget` |
| `ReplicationPolicy` | One per schedule | References a `ReplicationPair`; sets cadence, mode, retention; manages the backend `ReplicationPolicy` |
| `ReplicationSlot` | One per PVC | Per-volume runtime state machine; auto-created by the `PVCAnnotationWatcher` |

---

## 2. Goals and Non-Goals

### Goals

- **`ReplicationPair` CR** — reusable site-pair config (source + target cluster). Multiple
  `ReplicationPolicy` CRs may share the same pair. The pair controller creates and owns the
  backend `ReplicationTarget`.
- **`ReplicationPolicy` CR** — replication schedule and retention, referencing a
  `ReplicationPair` via `spec.pairRef`. The policy controller creates and owns the backend
  `ReplicationPolicy` using the pair's `backendTargetID`.
- **`ReplicationSlot` CR** — one per PVC, auto-created when a PVC with the
  `storage.simplyblock.io/replication-policy` annotation becomes Bound. Drives all
  per-volume backend calls and tracks live state.
- **StorageClass annotation** — opt all PVCs using a given StorageClass into a named
  `ReplicationPolicy` without per-PVC configuration.
- **PVC annotation** — opt a single PVC into a named `ReplicationPolicy`; overrides the
  StorageClass annotation.
- **Multiple replication targets** — support simultaneously replicating to different clusters
  via distinct `ReplicationPair` + `ReplicationPolicy` combinations.
- **`ReplicationOps` CR** — a one-shot user-driven CR (same pattern as `StorageClusterOps`)
  for imperative operations: `failover` and `failback`. The operator drives the backend
  calls and records the outcome; the user never calls the backend directly for these.
- Guard `replication/start` / `replication/stop` on policy-managed volumes so the operator
  remains the single source of truth.
- `PUT /{volume}` with `replication_policy_id: null` cleans up replication snapshots on
  **both** sides before stopping.

### Non-Goals

- Automatically migrating existing manually-started replications to the new model (follow-up).
- A retention/TTL policy for completed or detached `ReplicationSlot` CRs (follow-up).
- Cross-namespace `ReplicationPolicy` references.
- Changing the snapshot or transport protocol.

---

## 3. Architecture Overview

```
┌────────────────────────── Kubernetes Layer ─────────────────────────────┐
│                                                                          │
│  StorageClass                            PersistentVolumeClaim           │
│  annotations:                            annotations:                    │
│    storage.simplyblock.io/                 storage.simplyblock.io/       │
│      replication-policy: dr-policy           replication-policy: my-pol │
│        │  (applies to all PVCs)                │  (overrides SC)         │
└────────┼─────────────────────────────────────  ┼────────────────────────┘
         │ annotation ref                         │ annotation ref
         ▼                                        ▼
┌────────────────────────── Operator CRs ─────────────────────────────────┐
│                                                                          │
│  ReplicationPair CR    ◄── one per site pair; user-created               │
│    spec.sourceCluster, spec.targetCluster                                │
│    status.ready, status.backendTargetID                                  │
│    → check-or-create /replication/targets (shared per cluster pair)      │
│               │                                                          │
│               │ referenced by (many)                                     │
│               ▼                                                          │
│  ReplicationPolicy CR  ◄── one per schedule; user-created               │
│    spec.pairRef, spec.interval, spec.mode, spec.snapshotRetention        │
│    status.ready, status.backendPolicyID, status.slotCount                │
│    → create POST /replication/policies (owned by this CR)                │
│               │                                                          │
│               │ manages (1 per bound PVC)                                │
│               ▼                                                          │
│  ReplicationSlot CR  ◄── auto-created by PVCAnnotationWatcher           │
│    spec.policyRef, spec.pvcRef, spec.volumeID                            │
│    status.state: replicating | cutover_pending | failed_over | ...       │
│    → drives PUT /{vol} replication_policy_id, commit, failback           │
└──────────────┬──────────────────────────────────────────────────────────┘
               │ creates / tracks
               ▼
┌────────────────────────── Backend API ──────────────────────────────────┐
│                                                                          │
│  Cluster A (source)    ReplicationTarget    ReplicationPolicy            │
│  Volume (source lvol) ──────────────────────────────────────────►       │
│                          snapshot replication                            │
│  ◄────────────────────────────────────────────────────────────────────  │
│                             failback                  Cluster B (target) │
│                                                       Volume (target)    │
└──────────────────────────────────────────────────────────────────────────┘
```

**Deletion ordering** mirrors creation: deleting a `ReplicationPair` is blocked while any
`ReplicationPolicy` CR references it; deleting a `ReplicationPolicy` is blocked while any
`ReplicationSlot` CR exists for it.

---

## 4. API Design — New CRDs

### 4.1 ReplicationPair CR

Declares a reusable source-to-target cluster relationship. Creating this CR provisions the
backend `ReplicationTarget`; deleting it tears it down. Multiple `ReplicationPolicy` CRs
may share the same pair.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=relpair
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=".spec.sourceCluster"
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".spec.targetCluster"
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="BackendTargetID",type=string,JSONPath=".status.backendTargetID"
type ReplicationPair struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ReplicationPairSpec   `json:"spec,omitempty"`
    Status ReplicationPairStatus `json:"status,omitempty"`
}

type ReplicationPairSpec struct {
    // SourceCluster is the name of the local StorageCluster CR (the replication source).
    // +kubebuilder:validation:Required
    SourceCluster string `json:"sourceCluster"`

    // TargetCluster is the name or UUID of the remote cluster to replicate to.
    // +kubebuilder:validation:Required
    TargetCluster string `json:"targetCluster"`
}

type ReplicationPairStatus struct {
    // Ready is true when the backend ReplicationTarget has been provisioned.
    Ready bool `json:"ready,omitempty"`

    // BackendTargetID is the UUID of the backend ReplicationTarget resource.
    BackendTargetID string `json:"backendTargetID,omitempty"`

    // Message provides a human-readable description of the current state.
    Message string `json:"message,omitempty"`
}
```

### 4.2 ReplicationPolicy CR

Defines the replication schedule and retention for a given site pair. References a
`ReplicationPair` via `spec.pairRef`. Multiple policies can reference the same pair
with different schedules.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=repl
// +kubebuilder:printcolumn:name="Pair",type=string,JSONPath=".spec.pairRef"
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=".spec.interval"
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Slots",type=integer,JSONPath=".status.slotCount"
type ReplicationPolicy struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ReplicationPolicySpec   `json:"spec,omitempty"`
    Status ReplicationPolicyStatus `json:"status,omitempty"`
}

type ReplicationPolicySpec struct {
    // PairRef is the name of the ReplicationPair that defines the source and target clusters.
    // +kubebuilder:validation:Required
    PairRef string `json:"pairRef"`

    // Mode controls replication semantics.
    // failover:  target is a DR standby; volumes are read-only on the target.
    // migration: planned online cutover to the target cluster.
    // +kubebuilder:validation:Enum=failover;migration
    // +kubebuilder:default=failover
    Mode string `json:"mode,omitempty"`

    // Interval is how often a replication snapshot is taken (e.g. "5m", "1h").
    // +kubebuilder:default="5m"
    Interval string `json:"interval,omitempty"`

    // SnapshotRetention is the minimum number of snapshots to retain on the target.
    // +kubebuilder:validation:Minimum=2
    // +kubebuilder:default=3
    SnapshotRetention int32 `json:"snapshotRetention,omitempty"`
}

type ReplicationPolicyStatus struct {
    // Ready is true when the backend ReplicationPolicy has been created.
    Ready bool `json:"ready,omitempty"`

    // BackendPolicyID is the UUID of the backend ReplicationPolicy resource.
    BackendPolicyID string `json:"backendPolicyID,omitempty"`

    // SlotCount is the number of ReplicationSlot CRs currently managed by this policy.
    SlotCount int32 `json:"slotCount,omitempty"`

    // ActiveOpsRef is the name of the currently running ReplicationOps CR.
    // Empty when no operation is in progress.
    ActiveOpsRef string `json:"activeOpsRef,omitempty"`

    // Conditions holds standard Kubernetes condition types.
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

### 4.3 ReplicationSlot CR

One per PVC; auto-created by the `PVCAnnotationWatcher` when a PVC with the
`storage.simplyblock.io/replication-policy` annotation becomes Bound.
Drives all per-volume backend calls and reflects live replication state.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=relslot
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=".spec.policyRef"
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=".spec.pvcRef"
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=".status.state"
// +kubebuilder:printcolumn:name="Direction",type=string,JSONPath=".status.direction"
type ReplicationSlot struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ReplicationSlotSpec   `json:"spec,omitempty"`
    Status ReplicationSlotStatus `json:"status,omitempty"`
}

type ReplicationSlotSpec struct {
    // PolicyRef is the name of the ReplicationPolicy that governs this slot. Immutable.
    PolicyRef string `json:"policyRef"`

    // PVCRef is the name of the PVC being replicated. Immutable.
    PVCRef string `json:"pvcRef"`

    // VolumeID is the CSI volume handle of the source volume. Immutable.
    // Format: <clusterID>:<poolID>:<lvolID>
    VolumeID string `json:"volumeID"`
}

type ReplicationSlotStatus struct {
    // State is the current replication state for this slot.
    // +kubebuilder:validation:Enum=attaching;poll_attach;replicating;cutover_pending;cutover_done;failed_over;detaching;error
    State string `json:"state,omitempty"`

    // Direction is which side of the replication this cluster is on.
    // +kubebuilder:validation:Enum=source;target
    Direction string `json:"direction,omitempty"`

    // SourceLvolID is the UUID of the source volume on Cluster A.
    SourceLvolID string `json:"sourceLvolID,omitempty"`

    // TargetLvolID is the UUID of the replicated volume on Cluster B.
    TargetLvolID string `json:"targetLvolID,omitempty"`

    // TargetNQN is the NVMe NQN on the target cluster (populated after failover).
    TargetNQN string `json:"targetNQN,omitempty"`

    // LastReplicatedAt is the timestamp of the last successful replication snapshot.
    LastReplicatedAt *metav1.Time `json:"lastReplicatedAt,omitempty"`

    // Message provides a human-readable description of the current state.
    Message string `json:"message,omitempty"`

    // Conditions holds standard Kubernetes condition types.
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

### 4.4 Naming convention

`ReplicationSlot` CRs are named `<policy-name>-<pvc-name>` and owned by the PVC via
an `OwnerReference`. Deleting the PVC triggers garbage collection of the slot, which in
turn triggers detach (cleanup of replication snapshots on both sides) before the slot
object is removed.

---

## 5. Annotation Mechanism

### 5.1 StorageClass annotation

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: simplyblock-dr
  annotations:
    storage.simplyblock.io/replication-policy: dr-policy
provisioner: csi.simplyblock.io
```

When the `PVCAnnotationWatcher` sees a PVC using this StorageClass transition to `Bound`,
it reads the annotation and creates a `ReplicationSlot` CR referencing the named policy.

### 5.2 PVC annotation

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
  annotations:
    storage.simplyblock.io/replication-policy: migration-policy
```

This overrides the StorageClass annotation. It can be set at create time or added/changed
after the PVC is bound. Changing the policy on a bound PVC is treated as a detach +
re-attach (full copy on the backend).

### 5.3 Annotation removal

Removing the annotation from an existing PVC triggers detach: the operator sends
`PUT /storage-pools/{pool_id}/volumes/{volume_id}` with `replication_policy_id: null`,
which stops replication and deletes internal snapshots on both sides, then deletes the
`ReplicationSlot` CR.

### 5.4 Reconciliation trigger

The `PVCAnnotationWatcher` controller watches `PersistentVolumeClaim` events. On each
event it compares the annotation with the current `ReplicationSlot` CR (if any) and
creates, updates, or deletes accordingly. It also watches `StorageClass` events to detect
annotation changes inherited by existing PVCs.

---

## 6. Backend API Changes

### 6.1 New resources

#### Replication Targets — `GET|POST /replication/targets`

| Method | Path | Body | Notes |
|--------|------|------|-------|
| `GET` | `/replication/targets` | — | List all targets (returns array) |
| `POST` | `/replication/targets` | `target_name*`, `target_cluster_id*` (UUID), `target_pool_id` (UUID), `timeout_sec` | Create; returns `201` with full DTO |
| `GET` | `/replication/targets/{id}` | — | Detail |
| `DELETE` | `/replication/targets/{id}` | — | `400` while any policy references it |
| `POST` | `/replication/targets/{id}/failover` | — | Fail over all volumes on this target |

#### Replication Policies — `GET|POST /replication/policies`

| Method | Path | Body | Notes |
|--------|------|------|-------|
| `GET` | `/replication/policies` | — | List all policies (returns array) |
| `POST` | `/replication/policies` | `policy_name*`, `target_id*` (UUID), `interval_min>=1`, `mode=failover\|migration`, `keep_replicated>=2` | Create; returns `201` with full DTO |
| `GET` | `/replication/policies/{id}` | — | Detail |
| `DELETE` | `/replication/policies/{id}` | — | `400` while any volume follows it |
| `POST` | `/replication/policies/{id}/failover` | — | Fail over all volumes under this policy |

### 6.2 New volume endpoints — `PUT /storage-pools/{pool_id}/volumes/{volume_id}`

Policy attach and detach are now folded into the standard volume `PUT`:

| Field | Value | Effect |
|-------|-------|--------|
| `replication_policy_id` | UUID string | Attach (or change) policy. Change = detach + attach (full copy). `409` while a cutover is in flight |
| `replication_policy_id` | `null` | Detach policy. Stops replication, cancels tasks, deletes internal snapshots both sides. `409` while a cutover is in flight |
| _(omitted)_ | — | No change to existing policy |

Per-volume replication operations move under a `/replication/` sub-resource:

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/{volume_id}/replication/` | Returns `{replication_id, source_lvol_id, target_lvol_id, source_cluster_id, target_cluster_id, mode, state, direction, target_nqn, target_ns_id, is_source}`. `404` if no relationship |
| `POST` | `/{volume_id}/replication/start` | Start replication; body: `{replication_cluster_id?, mode?, interval_min?}` |
| `POST` | `/{volume_id}/replication/stop` | Stop replication |
| `POST` | `/{volume_id}/replication/trigger` | Force one snapshot outside cadence |
| `POST` | `/{volume_id}/replication/failover` | Unplanned failover of one volume |
| `POST` | `/{volume_id}/replication/commit` | Planned online cutover; returns `202` with `Location` pointing at the cutover task |
| `POST` | `/{volume_id}/replication/failback` | Start reverse replication; body: `{source_cluster_id?}` |

### 6.3 Behaviour changes on existing endpoints

| Endpoint | Change |
|----------|--------|
| `POST /{volume_id}/replication/start` | `409` on policy-managed volumes |
| `POST /{volume_id}/replication/stop` | `409` on policy-managed volumes |
| `GET /{volume_id}/connect` | Relationship-driven: `replicating=source`, `cutover_pending=BOTH`, `failed_over=target`, `cutover_done=post-move only` |
| `GET /{volume_id}` | `VolumeDTO` now includes `rep_info` |

---

## 7. State Machine — ReplicationSlot

The `ReplicationSlot` CR holds the per-volume replication lifecycle, replacing the old
imperative per-volume calls. All state transitions are driven by the `ReplicationSlot`
reconciler.

```
                    ┌─────────────────┐
                    │    attaching    │  PUT /volumes/{v} replication_policy_id set
                    └────────┬────────┘
                             │ backend accepted
                             ▼
                    ┌─────────────────┐
                    │  poll_attach    │  GET /{v}/replication → wait for state=replicating
                    └────────┬────────┘
                             │ confirmed
                             ▼
                    ┌─────────────────┐
          ┌────────►│   replicating   │◄──────────────────┐
          │         └────────┬────────┘                   │
          │                  │                            │
          │         replication_trigger / cadence         │
          │                  │                            │
          │         ┌────────▼────────┐                   │
          │         │cutover_pending  │  replication_commit│
          │         └────────┬────────┘   (failback step) │
          │                  │                            │
          │         ┌────────▼────────┐                   │
          │         │  cutover_done   │                   │
          │         └────────┬────────┘                   │
          │                  │ replication_failback        │
          │                  ▼                            │
          │         ┌─────────────────┐                   │
          │         │   failed_over   │◄── unplanned ─────┤
          │         └────────┬────────┘   failover        │
          │                  │ replication_failback        │
          │                  └────────────────────────────┘
          │
          │         ┌─────────────────┐
          └─── or ──│    detaching    │  PUT /volumes/{v} replication_policy_id=null
                    └────────┬────────┘  (annotation removed / PVC deleted)
                             │ success
                             ▼
                    ┌─────────────────┐
                    │    (deleted)    │
                    └─────────────────┘

          ┌─────────────────┐
          │      error      │  any backend call fails after retries
          └─────────────────┘
```

State is persisted in `ReplicationSlot.status.state`. The reconciler is idempotent:
re-entering any state retries the corresponding backend call.

---

## 8. Operator Reconciler Flow

### 8.1 ReplicationPair reconciler

Manages the backend `ReplicationTarget` for a given source/target cluster pair.

1. Resolve `spec.sourceCluster` → call `utils.ResolveClusterUUID` to get the local
   cluster UUID for backend API calls.
2. Call `GET /replication/targets` and search for an existing target with matching
   `target_cluster_id`. If found, reuse its ID. If not, call `POST /replication/targets`
   to create one. Store the UUID in `status.backendTargetID`.
3. Set `status.ready = true`.
4. On deletion: block while any `ReplicationPolicy` CR in the namespace still has
   `spec.pairRef` pointing at this pair. Once clear, call `DELETE /replication/targets/{id}`
   and remove the finalizer.

Multiple `ReplicationPolicy` CRs may reference the same `ReplicationPair`; the backend
target is shared and is only removed when the pair itself is deleted.

### 8.2 ReplicationPolicy reconciler

Manages the backend `ReplicationPolicy`, which couples a `ReplicationTarget` to a
schedule and retention rule.

1. Get the `ReplicationPair` CR named by `spec.pairRef`; if not found or
   `status.ready == false`, requeue and wait.
2. Call `utils.ResolveClusterUUID` using `pair.Spec.SourceCluster` to get the cluster
   UUID for backend API calls.
3. Add the finalizer if absent.
4. If `status.backendPolicyID` is empty, call `ensureBackendPolicy`:
   - `GET /replication/policies` — reuse an existing policy with the same name.
   - If absent, `POST /replication/policies` with `target_id = pair.Status.BackendTargetID`,
     `interval_min`, `mode`, `keep_replicated = spec.snapshotRetention`. Store UUID in
     `status.backendPolicyID`.
5. List owned `ReplicationSlot` CRs; update `status.slotCount`.
6. Set `status.ready = true`.
7. On deletion: block while any `ReplicationSlot` CR still has `spec.policyRef` pointing
   at this policy. Once clear, call `DELETE /replication/policies/{id}`. The backend
   `ReplicationTarget` is managed by the `ReplicationPair` controller — the policy
   controller does **not** delete it.

### 8.3 ReplicationSlot reconciler

Per-volume state machine. Dispatches on `status.state`:

- **`""`** (new): validate `spec.volumeID`, call `PUT /{volume}` with
  `replication_policy_id = policy.Status.BackendPolicyID`; set state → `attaching`.
- **`attaching`**: call `GET /{volume}/replication`; if backend returns a `404` (still
  pending) or state is `replicating`, advance to `poll_attach`.
- **`poll_attach`**: poll `GET /{volume}/replication` until `state == "replicating"`.
  Advance to `replicating`; populate `status.sourceLvolID`, `status.targetLvolID`,
  `status.direction`.
- **`replicating`**: sync `status.lastReplicatedAt` from backend snapshot timestamp;
  watch for externally triggered state changes (cutover, failover).
- **`cutover_pending`** / **`cutover_done`** / **`failed_over`**: reflect backend state
  into CR status; emit events; expose via conditions.
- **`detaching`**: call `PUT /{volume}` with `replication_policy_id: null`; remove
  finalizer on success (object is then GC'd).
- **`error`**: surface condition, back off, retry.

### 8.4 PVC annotation watcher

A lightweight controller watches `PersistentVolumeClaim` events:

- **Annotation present, PVC Bound**: create a `ReplicationSlot` CR with `spec.policyRef`
  set to the annotation value and `spec.volumeID` from the PV's CSI volume handle.
  Requeue while the PVC is still `Pending`.
- **Annotation added post-bind**: same — PVC is already Bound, slot is created immediately.
- **Annotation changed post-bind**: set existing slot state → `detaching`; wait for
  detach to complete; create a new slot against the new policy.
- **Annotation removed post-bind**: set slot state → `detaching`; slot is deleted on
  detach completion.
- **PVC deleted**: OwnerReference GC triggers slot deletion; finalizer on the slot ensures
  detach completes before the CR is removed.

The same logic applies when the annotation is inherited from the StorageClass.

---

## 9. ReplicationOps — User-Driven Failover and Failback

Failover and failback require human or automation judgement (a cluster is down, recovery
is complete). They are **not** reconciled automatically. Instead the user creates a
`ReplicationOps` CR — a one-shot imperative CR following the same pattern as
`StorageClusterOps` — and the operator drives the backend calls to completion.

### 9.1 ReplicationOps CR

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=replops
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=".spec.scope"
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=".spec.ref"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message"
type ReplicationOps struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ReplicationOpsSpec   `json:"spec,omitempty"`
    Status ReplicationOpsStatus `json:"status,omitempty"`
}

type ReplicationOpsSpec struct {
    // Action is the operation to perform. Immutable.
    // +kubebuilder:validation:Enum=failover;failback
    Action string `json:"action"`

    // Scope controls which volumes are affected. Immutable.
    // policy: all ReplicationSlots managed by the named ReplicationPolicy CR.
    // volume: a single ReplicationSlot (unplanned per-volume failover).
    // +kubebuilder:validation:Enum=policy;volume
    Scope string `json:"scope"`

    // Ref is the name of the target resource (ReplicationPolicy name for scope=policy;
    // ReplicationSlot name for scope=volume). Immutable.
    Ref string `json:"ref"`

    // SourceClusterID is used for failback only. Omit to recover to the original source.
    // +optional
    SourceClusterID string `json:"sourceClusterID,omitempty"`
}

type ReplicationOpsStatus struct {
    // Phase is the current lifecycle phase of this operation.
    // +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
    Phase string `json:"phase,omitempty"`

    // Message is a human-readable description of the current phase.
    Message string `json:"message,omitempty"`

    // Results holds a per-volume summary of the operation outcome.
    Results []ReplicationOpsResult `json:"results,omitempty"`
}

type ReplicationOpsResult struct {
    // SlotRef is the name of the ReplicationSlot CR.
    SlotRef string `json:"slotRef"`
    // Status is the outcome for this volume: succeeded, skipped, or failed.
    // +kubebuilder:validation:Enum=succeeded;skipped;failed
    Status string `json:"status"`
    // Detail is an optional human-readable note (error message or skip reason).
    Detail string `json:"detail,omitempty"`
    // TargetLvolID is the UUID of the volume on the target cluster (failover only).
    TargetLvolID string `json:"targetLvolID,omitempty"`
}
```

### 9.2 Unplanned failover (disaster)

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: ReplicationOps
metadata:
  name: failover-to-cluster-b
spec:
  action: failover
  scope: policy
  ref: dr-policy   # name of the ReplicationPolicy CR
```

The `ReplicationOps` reconciler:

1. Resolves the scope (policy or single slot) → collects affected `ReplicationSlot` CRs.
2. Calls `POST /replication/policies/{id}/failover` on the backend.
3. Updates each affected `ReplicationSlot.status.state → failed_over`, `direction: target`.
4. Records per-volume outcome in `status.results`.
5. Sets `status.phase → Succeeded` or `Failed`.

### 9.3 Planned online cutover (migration)

For `mode: migration` policies, the user creates:

```yaml
spec:
  action: failover
  scope: policy
  ref: migration-policy
```

The reconciler calls `POST /{vol}/replication/commit` (rather than the policy failover
endpoint) for each slot in the policy, performing an online cutover. The CSI driver reads
`GET /{vol}/connect` to get the new cluster's paths with no storage interruption.

### 9.4 Failback

After the source cluster recovers, the user creates:

```yaml
spec:
  action: failback
  scope: policy
  ref: dr-policy
  sourceClusterID: cluster-a   # optional; omit = recovered origin
```

The reconciler:

1. Calls `POST /{vol}/replication/failback` for each affected slot.
2. Monitors reverse replication until stable.
3. Calls `POST /{vol}/replication/commit` to complete the cutback.
4. Updates each `ReplicationSlot.status.direction → source`.

### 9.5 One active `ReplicationOps` per policy

Only one `ReplicationOps` may be in `Running` phase for a given `ReplicationPolicy` at a
time, enforced via `ReplicationPolicy.status.activeOpsRef` (same guard as
`StorageCluster.status.activeOpsRef`).

---

## 10. Observability

| Signal | Details |
|--------|---------|
| `ReplicationPair.status.ready` | `true` when backend target provisioned |
| `ReplicationPair.status.backendTargetID` | UUID of the shared backend target |
| `ReplicationPolicy.status.ready` | `true` when backend policy provisioned |
| `ReplicationPolicy.status.slotCount` | Number of actively managed slots |
| `ReplicationPolicy.status.activeOpsRef` | Name of the running `ReplicationOps` CR; empty when idle |
| `ReplicationSlot.status.state` | Current replication state per PVC |
| `ReplicationSlot.status.lastReplicatedAt` | Timestamp of last successful snapshot |
| `ReplicationSlot.status.conditions` | `Ready`, `Degraded`, `FailedOver` condition types |
| Kubernetes Events | `Attaching`, `Replicating`, `CutoverDone`, `FailedOver`, `DetachFailed` |
| `kubectl get relpair -n <ns>` | Short view of all site pairs and readiness |
| `kubectl get repl -n <ns>` | Short view of all policies, mode, and slot count |
| `kubectl get relslot -n <ns>` | Short view of all slots, state, and direction |
| `kubectl get replops -n <ns>` | Short view of all ops CRs, action, scope, and phase |

---

## 11. Testing Strategy

### Unit tests

- `ReplicationPair` reconciler: backend target creation, idempotency, deletion blocked while
  policies reference it, deletion succeeds once clear.
- `ReplicationPolicy` reconciler: waits for pair ready, backend policy creation, idempotency,
  deletion blocked while slots exist, deletion succeeds without touching backend target.
- `ReplicationSlot` reconciler: all state transitions, error + retry paths, finalizer
  lifecycle, `splitVolumeHandle` and `parseDurationToMinutes` helpers.
- `PVCAnnotationWatcher`: slot creation on bind, annotation changes, annotation removal.
- `ReplicationOps` reconciler: failover and failback for scope=policy and scope=volume,
  mutual exclusion via `activeOpsRef`.

### Integration / e2e tests

- **Happy path**: create `ReplicationPair` → create `ReplicationPolicy` referencing it →
  create StorageClass with annotation → provision PVC → verify `ReplicationSlot` reaches
  `replicating` → verify `lastReplicatedAt` updated.
- **PVC annotation override**: SC annotation set, PVC annotation different → verify slot
  references PVC annotation policy, not SC.
- **Annotation removal**: remove annotation from PVC → verify slot reaches `detaching`
  → verify backend `PUT` with `replication_policy_id: null` called → verify snapshots
  cleaned up on both sides.
- **Policy change**: update PVC annotation to different policy → verify old slot detaches
  before new slot attaches.
- **Failover via `ReplicationOps`**: create `ReplicationOps{action: failover, scope: policy}`
  → verify reconciler calls backend failover endpoint → all slots → `failed_over`
  → `ReplicationOps.status.phase → Succeeded` → `GET /{vol}/connect` returns target paths.
- **Failback via `ReplicationOps`**: create `ReplicationOps{action: failback, scope: policy}`
  → verify reconciler calls `replication/failback` then `replication/commit` per volume
  → all slots direction restored → `ReplicationOps.status.phase → Succeeded`.
- **Concurrency guard**: create two `ReplicationOps` for the same policy simultaneously →
  verify second is rejected while first is `Running` (`activeOpsRef` set).
- **Guard**: attempt `replication/start` on a policy-managed volume → verify `409`.
- **Deletion ordering**: delete `ReplicationPolicy` → verify blocked while slots exist →
  delete PVC (GC's slot) → verify policy GC'd → delete `ReplicationPair` → verify blocked
  while policies reference it → verify pair GC'd and backend target deleted.

---

## 12. Open Questions

1. **Multiple policies per PVC**: The current design restricts a PVC to one slot at a time.
   If a PVC needs to replicate to two targets simultaneously (e.g. DR + migration), should
   we allow multiple slots? This complicates the failover and connect semantics.

2. **`ReplicationSlot` retention after detach**: Should detached slots be deleted immediately
   or retained (with a TTL) for audit purposes? Aligns with the open question on
   `StorageNodeOps` retention.

3. **`replication/failback` source cluster discovery**: When `source_cluster_id` is omitted,
   the backend must know the original source. Is this reliable after a node failure? Should
   the operator persist `spec.originalSourceClusterID` on the slot CR?

4. **CSI driver integration point**: Does the CSI driver read `ReplicationSlot.status`
   directly, or does it call `GET /{vol}/connect` and rely solely on the backend? A direct
   CR read avoids an extra API round-trip but couples the driver to the operator data model.
