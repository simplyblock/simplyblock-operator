# Design Document: StorageClusterOps CR for Cluster-Level Operations

**Status:** Draft  
**Author:** Israel Geoffrey  
**Date:** 2026-08-05

---

## 1. Background

The `SimplyblocksStorageCluster` reconciler currently conflates two distinct concerns:

| Concern | Current location | Problem |
|---|---|---|
| Steady-state reconciliation (status sync, deletion, adoption) | `StorageClusterReconciler.Reconcile` | Mixed with long-running operations |
| Imperative operations (Activate, Expand, Shutdown, Restart) | `StorageCluster.spec.action` + inline reconcile handlers | No history, hard to track, retry risks duplicate backend calls |

Operations like `Activate` and `Expand` are multi-step: POST to the backend, then poll for
completion. This pattern is embedded directly in the reconciler alongside steady-state sync,
making it difficult to:

- Track the progress of a running operation independently
- Audit which operations ran, when, and with what outcome
- Retry safely after partial failure (backend POST succeeded but status patch failed → duplicate
  call risk on next reconcile if `Triggered` is not persisted)

`StorageNodeOps` proved a better model: a dedicated one-shot CR with a phase/status state
machine, a `Triggered` guard, and clear terminal states. `StorageClusterOps` applies the same
pattern to cluster-level operations.

---

## 2. Goals

- **`StorageClusterOps`** — a one-shot CR that targets a `SimplyblocksStorageCluster` and drives a
  cluster-level operation (`Activate`, `Expand`, `Shutdown`, `Restart`, `NodeRecycle`) to
  completion, then records the result.
- **`SimplyblocksStorageCluster` reconciler** — narrowed to steady-state only: status sync,
  deletion, adoption. Delegates all imperative operations to `StorageClusterOps`.
- Per-operation history, auditability, and safe retry without duplicate backend calls.
- Consistent operator pattern across node-level (`StorageNodeOps`) and cluster-level operations.

## 3. Non-Goals

- Changing the backend API protocol or operation semantics.
- Automatic migration of clusters currently mid-operation (handled by adoption logic).
- A retention/TTL policy for completed `StorageClusterOps` CRs (follow-up, same as `StorageNodeOps`).

---

## 4. Architecture Overview

```
SimplyblocksStorageCluster  (steady-state reconciler)
    │  referenced by
    └─► StorageClusterOps  (action: activate)   Phase: Running → Succeeded
    └─► StorageClusterOps  (action: expand)     Phase: Pending
```

`StorageClusterOps` is scoped to a single `SimplyblocksStorageCluster`. Only one `StorageClusterOps` can be
active per cluster at a time, enforced by the reconciler checking `status.activeOpsRef` before
accepting a new operation.

---

## 5. API Design

### 5.1 SimplyblocksStorageCluster (revised)

Remove all imperative fields from `spec` and `status`. Retain steady-state fields only.

```go
type StorageClusterSpec struct {
    // ... all existing steady-state fields unchanged ...

    // REMOVED: Action, NodeUUID (imperative fields move to StorageClusterOps.spec)
}

type StorageClusterStatus struct {
    // ... all existing steady-state status fields unchanged ...

    // ActiveOpsRef is the name of the currently active StorageClusterOps on this cluster.
    // Empty when no operation is in progress.
    ActiveOpsRef string `json:"activeOpsRef,omitempty"`

    // REMOVED: ActionStatus (phase/triggered/action move to StorageClusterOps.status)
}
```

### 5.2 StorageClusterOps (new CRD)

A one-shot operational CR targeting a single `SimplyblocksStorageCluster`. Analogous to a
Kubernetes `Job`.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type StorageClusterOps struct { ... }

type StorageClusterOpsSpec struct {
    // ClusterRef is the name of the target SimplyblocksStorageCluster. Immutable.
    // +k8s:immutable
    ClusterRef string `json:"clusterRef"`

    // Action is the operation to perform. Immutable.
    // +kubebuilder:validation:Enum=activate;expand;shutdown;restart;node-recycle
    // +k8s:immutable
    Action string `json:"action"`

    // NodeUUID is the target node UUID, required for node-recycle only.
    // +optional
    NodeUUID string `json:"nodeUUID,omitempty"`
}

// StorageClusterOpsPhase is the lifecycle phase of a StorageClusterOps.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type StorageClusterOpsPhase string

const (
    StorageClusterOpsPhasePending   StorageClusterOpsPhase = "Pending"
    StorageClusterOpsPhaseRunning   StorageClusterOpsPhase = "Running"
    StorageClusterOpsPhaseSucceeded StorageClusterOpsPhase = "Succeeded"
    StorageClusterOpsPhaseFailed    StorageClusterOpsPhase = "Failed"
)

type StorageClusterOpsStatus struct {
    // Phase is the high-level lifecycle phase.
    Phase StorageClusterOpsPhase `json:"phase,omitempty"`

    // Triggered indicates the backend POST has been sent for this operation.
    // Guards against duplicate backend calls on retry.
    Triggered bool `json:"triggered,omitempty"`

    // Message is a human-readable description of the current state or failure reason.
    Message string `json:"message,omitempty"`

    // StartedAt is when the operation began.
    StartedAt *metav1.Time `json:"startedAt,omitempty"`

    // CompletedAt is when the operation finished (successfully or not).
    CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}
```

---

## 6. Controller Changes

### 6.1 StorageClusterReconciler (revised)

Narrowed to **steady-state management only**:

1. Handle deletion (finalizer, backend delete call) — unchanged.
2. Adopt existing backend clusters (`adoptExistingCluster`) — unchanged.
3. Reconcile steady-state: sync `status.phase`, `status.uuid`, health fields from the backend — unchanged.
4. **Watch `StorageClusterOps`** — when a `StorageClusterOps` targeting this cluster is created, set
   `status.activeOpsRef`. When it completes, clear `status.activeOpsRef`.
5. **Remove** `reconcileActivate`, `reconcileExpand`, `failActivate`, `failExpand` and all
   `spec.action` / `status.actionStatus` handling.

### 6.2 StorageClusterOpsReconciler (new)

Drives all imperative cluster operations. Replaces the existing action handlers in
`StorageClusterReconciler`.

```
StorageClusterOps created →
    1. Check no other active ops on the target cluster
       (read SimplyblocksStorageCluster.status.activeOpsRef)
    2. Set status.activeOpsRef on the cluster CR
    3. Set Phase=Running, StartedAt=now
    4. Dispatch to handler based on spec.action:
       - activate   → POST /api/v2/clusters/{id}/activate → poll until active
       - expand     → POST /api/v2/clusters/{id}/expand   → poll until active
       - shutdown   → POST /api/v2/clusters/{id}/shutdown
       - restart    → POST /api/v2/clusters/{id}/restart
       - node-recycle → POST /api/v2/clusters/{id}/node-recycle/{nodeUUID}
    5. On completion → set Phase=Succeeded/Failed, CompletedAt=now
    6. Clear SimplyblocksStorageCluster.status.activeOpsRef
```

**Triggered guard:** Before issuing any backend POST, check `status.triggered`. If `true`,
skip the POST and proceed directly to polling. Set `status.triggered=true` immediately after
a successful POST using `ctrl.Result{Requeue: true}, nil` (not `ctrl.Result{}, err`) to
persist the flag without backoff delay and without risking a duplicate call.

**Mutual exclusion:** The reconciler checks `SimplyblocksStorageCluster.status.activeOpsRef`
before accepting a new operation. If set, the new `StorageClusterOps` stays in `Pending` phase and
requeues. A validating webhook may additionally reject a new `StorageClusterOps` if one is already
active on the same cluster.

---

## 7. State Machine

```
                    ┌──────────┐
   CR created  ───► │ Pending  │
                    └────┬─────┘
                         │ lock acquired, activeOpsRef set
                    ┌────▼─────┐
                    │ Running  │◄─── RequeueAfter polls (activate/expand)
                    └────┬─────┘
             ┌───────────┴───────────┐
        success                   failure
             │                         │
     ┌───────▼──────┐         ┌────────▼──────┐
     │  Succeeded   │         │    Failed     │
     └──────────────┘         └───────────────┘
```

Terminal phases (`Succeeded`, `Failed`) are no-ops on subsequent reconciles. The cluster
reconciler clears `activeOpsRef` when it observes the `StorageClusterOps` has reached a terminal phase.

---

## 8. Migration Strategy

### Phase 1 — Introduce StorageClusterOps alongside existing inline handling

- Add `StorageClusterOps` CRD and `StorageClusterOpsReconciler`.
- `SimplyblocksStorageCluster.spec.action` still accepted; the reconciler creates a `StorageClusterOps`
  CR on behalf of the user when `spec.action` is set (bridge shim).
- Both paths coexist; `StorageClusterOps` controller is the authoritative executor.

### Phase 2 — Remove legacy inline operation handling

- Remove `reconcileActivate`, `reconcileExpand`, and all `spec.action` / `status.actionStatus`
  fields from `SimplyblocksStorageCluster`.
- Remove bridge shim from `StorageClusterReconciler`.
- Update CRD YAML and Helm chart.
- Users must now create `StorageClusterOps` CRs directly (or via the CLI/UI).

---

## 9. Open Questions

**Q1: StorageClusterOps retention policy**  
How long should completed `StorageClusterOps` CRs be retained? Same question as `StorageNodeOps` —
indefinite retention for audit, or a `ttlSecondsAfterFinished`-style field? Not yet decided.

**Q2: Who creates StorageClusterOps for user-initiated actions?**  
In the current model, users set `spec.action` on the cluster CR. Post-migration, they must
create a `StorageClusterOps` CR. Should the CLI (`sbcli`) or a future admission webhook handle this
automatically to preserve the existing UX?

**Q3: Expand operation parameters**  
`Expand` may require additional parameters (e.g. target node list). Should `StorageClusterOpsSpec`
carry a typed `ExpandParams` sub-object, or pass them as a free-form map?

**Q4: Concurrent StorageClusterOps of different types**  
Should two independent, non-conflicting operations (e.g. a status-only poll and a node-recycle)
be allowed concurrently? The current model enforces one active op per cluster — is that too
restrictive?
