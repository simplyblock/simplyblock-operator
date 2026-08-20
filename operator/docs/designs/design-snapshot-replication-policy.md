# Design Document: Policy-Driven Snapshot Replication

**Status:** Proposed  
**Author:** Israel Geoffrey  
**Date:** 2026-08-19  
**GitHub Issue:** Refactor snapshot replication: introduce multi-target and policy-driven API

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API Design — New CRDs](#4-api-design--new-crds)
5. [Annotation Mechanism](#5-annotation-mechanism)
6. [Backend API Changes](#6-backend-api-changes)
7. [State Machine — ReplicationPair](#7-state-machine--replicationpair)
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

This design introduces two first-class resources — **`ReplicationPolicy` CR** and
**`ReplicationPair` CR** — together with a `StorageClass` / `PVC` annotation mechanism,
replacing the ad-hoc per-volume model.

---

## 2. Goals and Non-Goals

### Goals

- **`ReplicationPolicy` CR** — a cluster-scoped description of replication intent: target
  cluster, cadence, mode (`failover` | `migration`), and snapshot retention. Multiple
  policies may reference the same target.
- **`ReplicationPair` CR** — one per PVC, managed by `ReplicationPolicy`. Tracks the live
  relationship between a source lvol and its target lvol and drives all backend calls.
- **StorageClass annotation** — opt all PVCs using a given StorageClass into a named
  `ReplicationPolicy` without per-PVC configuration.
- **PVC annotation** — opt a single PVC into a named `ReplicationPolicy`; overrides the
  StorageClass annotation.
- **Multiple replication targets** — support simultaneously replicating to different clusters
  (e.g. DR to cluster-b, live migration to cluster-c) via distinct `ReplicationPolicy` CRs
  pointing at distinct `ReplicationTarget` backend resources.
- **`ReplicationOps` CR** — a one-shot user-driven CR (same pattern as `StorageClusterOps`)
  for imperative operations: `failover` and `failback`. The operator drives the backend
  calls and records the outcome; the user never calls the backend directly for these.
- Guard `replication/start` / `replication/stop` on policy-managed volumes so the operator
  remains the single source of truth.
- `PUT /{volume}` with `replication_policy_id: null` cleans up replication snapshots on
  **both** sides before stopping.

### Non-Goals

- Automatically migrating existing manually-started replications to the new model (follow-up).
- A retention/TTL policy for completed or detached `ReplicationPair` CRs (follow-up).
- Cross-namespace `ReplicationPolicy` references.
- Changing the snapshot or transport protocol.

---

## 3. Architecture Overview

```
┌────────────────────────── Kubernetes Layer ──────────────────────────┐
│                                                                       │
│  StorageClass                          PersistentVolumeClaim          │
│  annotations:                          annotations:                   │
│    replication.simplyblock.io/policy:    replication.simplyblock.io/  │
│      dr-policy                             policy: my-policy          │
│        │  (applies to all PVCs)              │  (overrides SC)        │
└────────┼─────────────────────────────────────┼───────────────────────┘
         │ annotation ref                       │ annotation ref
         ▼                                      ▼
┌────────────────────────── Operator CRs ──────────────────────────────┐
│                                                                       │
│  ReplicationPolicy CR  (new)                                          │
│    spec.target, spec.interval, spec.mode, spec.keepReplicated         │
│    → check-or-create /replication/targets (shared per cluster)        │
│    → create POST /replication/policies   (owned by this CR)           │
│               │                                                       │
│               │ manages (1 per PVC)                                   │
│               ▼                                                       │
│  ReplicationPair CR  (new)                                            │
│    status.sourceLvolID, status.targetLvolID                           │
│    status.state: replicating | cutover_pending | failed_over | ...    │
│    → drives PUT /{vol} replication_policy_id, commit, failback        │
└──────────────┬───────────────────────────────────────────────────────┘
               │ creates / tracks
               ▼
┌────────────────────────── Backend API ───────────────────────────────┐
│                                                                       │
│  Cluster A (source)    ReplicationTarget    ReplicationPolicy         │
│  Volume (source lvol) ─────────────────────────────────────────────► │
│                          snapshot replication                         │
│  ◄────────────────────────────────────────────────────────────────── │
│                             failback                  Cluster B (target)│
│                                                       Volume (target) │
└───────────────────────────────────────────────────────────────────────┘
```

The operator reconciles `ReplicationPolicy` CRs to ensure the backend `ReplicationTarget`
for the given cluster exists (creating it only once, shared across policies that reference
the same cluster) and that a backend `ReplicationPolicy` owned by this CR exists. It
reconciles `ReplicationPair` CRs to attach, monitor, detach, and report on per-volume
replication relationships.

---

## 4. API Design — New CRDs

### 4.1 ReplicationPolicy CR

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=repl
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".spec.target"
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=".spec.interval"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Pairs",type=integer,JSONPath=".status.pairCount"
type ReplicationPolicy struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ReplicationPolicySpec   `json:"spec,omitempty"`
    Status ReplicationPolicyStatus `json:"status,omitempty"`
}

type ReplicationPolicySpec struct {
    // Target is the name or UUID of the remote cluster to replicate to.
    // Immutable after creation.
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="target is immutable"
    Target string `json:"target"`

    // Interval is how often a replication snapshot is taken (e.g. "5m", "1h").
    // +kubebuilder:default="5m"
    Interval string `json:"interval,omitempty"`

    // Mode controls replication semantics.
    // failover:   target is a DR standby; volumes are read-only on the target.
    // migration:  planned online cutover to the target cluster.
    // +kubebuilder:validation:Enum=failover;migration
    // +kubebuilder:default=failover
    Mode string `json:"mode,omitempty"`

    // KeepReplicated is the minimum number of snapshots to retain on the target.
    // +kubebuilder:validation:Minimum=2
    // +kubebuilder:default=3
    KeepReplicated int32 `json:"keepReplicated,omitempty"`
}

type ReplicationPolicyStatus struct {
    // Ready is true when the backend ReplicationTarget and ReplicationPolicy exist.
    Ready bool `json:"ready,omitempty"`

    // BackendTargetID is the UUID of the backend ReplicationTarget resource.
    BackendTargetID string `json:"backendTargetID,omitempty"`

    // BackendPolicyID is the UUID of the backend ReplicationPolicy resource.
    BackendPolicyID string `json:"backendPolicyID,omitempty"`

    // PairCount is the number of ReplicationPair CRs currently managed by this policy.
    PairCount int32 `json:"pairCount,omitempty"`

    // ActiveOpsRef is the name of the currently running ReplicationOps CR.
    // Empty when no operation is in progress.
    ActiveOpsRef string `json:"activeOpsRef,omitempty"`

    // Conditions holds standard Kubernetes condition types.
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

### 4.2 ReplicationPair CR

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=relpair
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=".spec.policyRef"
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=".spec.pvcRef"
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=".status.state"
// +kubebuilder:printcolumn:name="Direction",type=string,JSONPath=".status.direction"
type ReplicationPair struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ReplicationPairSpec   `json:"spec,omitempty"`
    Status ReplicationPairStatus `json:"status,omitempty"`
}

type ReplicationPairSpec struct {
    // PolicyRef is the name of the ReplicationPolicy that owns this pair. Immutable.
    PolicyRef string `json:"policyRef"`

    // PVCRef is the name of the PVC being replicated. Immutable.
    PVCRef string `json:"pvcRef"`

    // VolumeID is the backend lvol UUID of the source volume. Immutable.
    VolumeID string `json:"volumeID"`
}

type ReplicationPairStatus struct {
    // State is the current replication state for this pair.
    // +kubebuilder:validation:Enum=attaching;replicating;cutover_pending;cutover_done;failed_over;detaching;error
    State string `json:"state,omitempty"`

    // Direction is which side of the pair this cluster is on.
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

### 4.3 Naming convention

`ReplicationPair` CRs are named `<policy-name>-<pvc-name>` and owned by the PVC via
an `OwnerReference`. Deleting the PVC triggers garbage collection of the pair, which
in turn triggers detach (cleanup of replication snapshots on both sides) before the
pair object is removed.

---

## 5. Annotation Mechanism

### 5.1 StorageClass annotation

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: simplyblock-dr
  annotations:
    replication.simplyblock.io/policy: dr-policy
provisioner: csi.simplyblock.io
```

When the operator (or CSI driver) provisions a PVC using this StorageClass, it reads
the annotation and creates a `ReplicationPair` CR referencing the named policy.

### 5.2 PVC annotation

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
  annotations:
    replication.simplyblock.io/policy: migration-policy
```

This overrides the StorageClass annotation. It can be set at create time or added/
changed after the PVC is bound. Changing the policy on a bound PVC is treated as a
detach + re-attach (full copy on the backend).

### 5.3 Annotation removal

Removing the annotation from an existing PVC triggers detach: the operator sends
`PUT /storage-pools/{pool_id}/volumes/{volume_id}` with `replication_policy_id: null`,
which stops replication and deletes internal snapshots on both sides, then deletes the
`ReplicationPair` CR.

### 5.4 Reconciliation trigger

The `StorageNodeSet` reconciler (or a new lightweight `PVCAnnotationWatcher` controller)
watches for PVC `CREATE`, `UPDATE`, and `DELETE` events. On each event it compares the
annotation with the current `ReplicationPair` CR (if any) and creates, updates, or
deletes accordingly.

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

## 7. State Machine — ReplicationPair

```
                    ┌─────────────────┐
                    │    attaching    │  PUT /volumes/{v} replication_policy_id set
                    └────────┬────────┘
                             │ success
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
          │         │   failed_over   │ replication/      │
          │         │  (unplanned) ──►│                   │
          │         └────────┬────────┘                   │
          │                  │ replication_failback        │
          │                  └────────────────────────────┘
          │
          │         ┌─────────────────┐
          └─── or ──│    detaching    │  PUT /volumes/{v} replication_policy_id=null
                    └────────┬────────┘  (annotation removed)
                             │ success
                             ▼
                    ┌─────────────────┐
                    │    (deleted)    │
                    └─────────────────┘

          ┌─────────────────┐
          │      error      │  any backend call fails after retries
          └─────────────────┘
```

State is persisted in `ReplicationPair.status.state`. The reconciler is idempotent:
re-entering any state retries the corresponding backend call.

---

## 8. Operator Reconciler Flow

### 8.1 ReplicationPolicy reconciler

1. Read `spec.target` → call `GET /replication/targets` and search for an existing target
   with matching `target_cluster_id`. If found, reuse its ID. If not, call
   `POST /replication/targets` to create one. Store the UUID in `status.backendTargetID`.
   Multiple `ReplicationPolicy` CRs pointing at the same cluster share one backend target.
2. Ensure a backend `ReplicationPolicy` owned by this CR exists (create via
   `POST /replication/policies` if absent, or reconcile cadence/mode/retention if changed);
   store UUID in `status.backendPolicyID`.
3. Count owned `ReplicationPair` CRs; update `status.pairCount`.
4. Set `status.ready = true`.
5. On deletion: check `status.pairCount == 0` before deleting backend resources.
   Return `Requeue` if pairs still exist.
   - Always delete the owned backend `ReplicationPolicy`.
   - Delete the backend `ReplicationTarget` only if no other `ReplicationPolicy` CR in
     the namespace still references the same `spec.target` cluster ID (check by listing
     CRs, not by the backend `400` guard alone).

### 8.2 ReplicationPair reconciler

1. Resolve the owning `ReplicationPolicy` CR; fail fast if not ready.
2. Dispatch on `status.state`:
   - **`""`** (new): call `PUT /{volume}` with `replication_policy_id`; set state → `attaching`.
   - **`attaching`**: poll `GET /{volume}/replication`; advance to `replicating` when
     `state == "replicating"`.
   - **`replicating`**: sync `status.lastReplicatedAt` from backend; watch for
     externally triggered state changes (cutover, failover).
   - **`cutover_pending`** / **`cutover_done`** / **`failed_over`**: reflect backend
     state into CR status; expose via events and conditions.
   - **`detaching`**: call `PUT /{volume}` with `replication_policy_id: null`; delete CR on success.
   - **`error`**: surface condition, back off, retry.
3. On PVC deletion (via OwnerReference GC): finalizer ensures detach completes before
   the pair CR is garbage-collected.

### 8.3 PVC annotation watcher

A lightweight controller watches `PersistentVolumeClaim` events and handles both
provision-time and post-bind annotation changes:

- **Annotation present at provision time**: once the PVC transitions to `Bound` (i.e. a
  PV exists and `pv.spec.csi.volumeHandle` is available), create the `ReplicationPair` CR
  and `PUT /{vol}` with `replication_policy_id` to start replication. Requeue and do nothing
  while the PVC is still `Pending`.
- **Annotation added post-bind**: same as above — PVC is already `Bound`, so the pair CR
  is created immediately.
- **Annotation changed post-bind** (different policy): set existing pair state →
  `detaching` (triggers `PUT /{vol}` with `replication_policy_id: null` + backend snapshot
  cleanup), wait for detach to complete, then create a new pair against the new policy.
  This is a full copy on the backend.
- **Annotation removed post-bind**: set pair state → `detaching`; backend cleans up
  snapshots on both sides; pair CR is deleted on completion.
- **PVC deleted**: OwnerReference GC triggers pair deletion; finalizer on the pair ensures
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
    // target:  all volumes whose ReplicationPair references the named ReplicationPolicy target.
    // policy:  all volumes managed by the named ReplicationPolicy CR.
    // volume:  a single ReplicationPair (unplanned per-volume failover).
    // +kubebuilder:validation:Enum=target;policy;volume
    Scope string `json:"scope"`

    // Ref is the name of the target resource (ReplicationPolicy name for scope=policy or
    // scope=target; ReplicationPair name for scope=volume). Immutable.
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
    // PairRef is the name of the ReplicationPair CR.
    PairRef string `json:"pairRef"`
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
  scope: target        # or: policy / volume
  ref: dr-policy       # name of the ReplicationPolicy CR
```

The `ReplicationOps` reconciler:

1. Resolves the scope (target, policy, or single pair) → collects affected `ReplicationPair` CRs.
2. Calls `POST /replication/targets/{id}/failover` or `POST /replication/policies/{id}/failover`
   on the backend.
3. Updates each affected `ReplicationPair.status.state → failed_over`, `direction: target`.
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

The reconciler calls `POST /{vol}/replication/commit` (rather than the target failover
endpoint) for each volume in the policy, performing an online cutover. The CSI driver
reads `GET /{vol}/connect` to get the new cluster's paths with no storage interruption.

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

1. Calls `POST /{vol}/replication/failback` for each affected volume.
2. Monitors reverse replication until stable.
3. Calls `POST /{vol}/replication/commit` to complete the cutback.
4. Updates each `ReplicationPair.status.direction → source`.

### 9.5 One active `ReplicationOps` per policy

Only one `ReplicationOps` may be in `Running` phase for a given `ReplicationPolicy` at a
time, enforced via `ReplicationPolicy.status.activeOpsRef` (same guard as
`StorageCluster.status.activeOpsRef`).

---

## 10. Observability

| Signal | Details |
|--------|---------|
| `ReplicationPolicy.status.ready` | `true` when backend resources provisioned |
| `ReplicationPolicy.status.pairCount` | Number of actively managed pairs |
| `ReplicationPair.status.state` | Current replication state per PVC |
| `ReplicationPair.status.lastReplicatedAt` | Timestamp of last successful snapshot |
| `ReplicationPair.status.conditions` | `Ready`, `Degraded`, `FailedOver` condition types |
| Kubernetes Events | `Attaching`, `Replicating`, `CutoverDone`, `FailedOver`, `DetachFailed` |
| `ReplicationPolicy.status.activeOpsRef` | Name of the running `ReplicationOps` CR; empty when idle |
| `kubectl get repl -n <ns>` | Short view of all policies and readiness |
| `kubectl get relpair -n <ns>` | Short view of all pairs, state, and direction |
| `kubectl get replops -n <ns>` | Short view of all ops CRs, action, scope, and phase |

---

## 11. Testing Strategy

### Unit tests

- `ReplicationPolicy` reconciler: backend resource creation, idempotency, deletion guard.
- `ReplicationPair` reconciler: all state transitions, error + retry paths.
- `effectiveConcurrentRestarts` and other helper functions.

### Integration / e2e tests

- **Happy path**: create StorageClass with annotation → provision PVC → verify
  `ReplicationPair` reaches `replicating` state → trigger snapshot → verify
  `lastReplicatedAt` updated.
- **PVC annotation override**: SC annotation set, PVC annotation different → verify
  pair references PVC annotation policy, not SC.
- **Annotation removal**: remove annotation from PVC → verify pair reaches `detaching`
  → verify backend `PUT` with `replication_policy_id: null` called → verify snapshots cleaned up on both sides.
- **Policy change**: update PVC annotation to different policy → verify old pair
  detaches before new pair attaches.
- **Failover via `ReplicationOps`**: create `ReplicationOps{action: failover, scope: policy}`
  → verify reconciler calls backend failover endpoint → all pairs → `failed_over`
  → `ReplicationOps.status.phase → Succeeded` → `GET /{vol}/connect` returns target paths.
- **Failback via `ReplicationOps`**: create `ReplicationOps{action: failback, scope: policy}`
  → verify reconciler calls `replication/failback` then `replication/commit` per volume
  → all pairs direction restored → `ReplicationOps.status.phase → Succeeded`.
- **Concurrency guard**: create two `ReplicationOps` for the same policy simultaneously →
  verify second is rejected while first is `Running` (`activeOpsRef` set).
- **Guard**: attempt `replication/start` on a policy-managed volume → verify `409`.
- **Deletion**: delete PVC → verify finalizer holds pair until detach completes →
  verify backend snapshots cleaned up → verify pair GC'd.

---

## 12. Open Questions

1. **Multiple policies per PVC**: The current design restricts a PVC to one policy at a
   time (one `ReplicationPair`). If a PVC needs to replicate to two targets simultaneously
   (e.g. DR + migration), should we allow multiple pairs? This complicates the failover
   and connect semantics.

2. **`ReplicationPair` retention after detach**: Should detached pairs be deleted
   immediately or retained (with a TTL) for audit purposes? Aligns with the open question
   on `StorageNodeOps` retention.

3. **`replication/failback` source cluster discovery**: When `source_cluster_id` is
   omitted, the backend must know the original source. Is this reliable after a node
   failure? Should the operator persist `spec.originalSourceClusterID` on the pair CR?

4. **CSI driver integration point**: Does the CSI driver read `ReplicationPair.status`
   directly, or does it call `GET /{vol}/connect` and rely solely on the backend? A
   direct CR read avoids an extra API round-trip but couples the driver to the operator
   data model.
