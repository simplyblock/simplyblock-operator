# Design Document: StorageClusterOps CR for Cluster-Level Operations

**Status:** Implemented  
**Author:** Israel Geoffrey  
**Date:** 2026-08-05  
**Last updated:** 2026-08-06

---

## 1. Background

The `SimplyblocksStorageCluster` reconciler previously conflated two distinct concerns:

| Concern | Previous location | Problem |
|---|---|---|
| Steady-state reconciliation (status sync, deletion, adoption) | `StorageClusterReconciler.Reconcile` | Mixed with long-running operations |
| Imperative operations (Activate, Expand, Shutdown, Restart) | `StorageCluster.spec.action` + inline reconcile handlers | No history, hard to track, retry risks duplicate backend calls |

`StorageNodeOps` proved a better model: a dedicated one-shot CR with a phase/status state
machine, a `Triggered` guard, and clear terminal states. `StorageClusterOps` applies the same
pattern to cluster-level operations.

Both migration phases are now complete. `StorageCluster.spec.action` and all inline action
handlers (`reconcileActivate`, `reconcileExpand`, `failActivate`, `failExpand`) have been
removed from `StorageClusterReconciler`. All imperative cluster operations are now exclusively
driven through `StorageClusterOps`.

---

## 2. Goals

- **`StorageClusterOps`** — a one-shot CR that targets a `SimplyblocksStorageCluster` and drives a
  cluster-level operation (`activate`, `expand`, `shutdown`, `start`, `restart`, `node-recycle`)
  to completion, then records the result.
- **`SimplyblocksStorageCluster` reconciler** — narrowed to steady-state only: status sync,
  deletion, adoption. No imperative operation handling.
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
    └─► StorageClusterOps  (action: activate)    Phase: Running → Succeeded
    └─► StorageClusterOps  (action: node-recycle) Phase: Pending
```

`StorageClusterOps` is scoped to a single `SimplyblocksStorageCluster`. Only one `StorageClusterOps`
can be active per cluster at a time, enforced via `status.activeOpsRef` on the cluster CR.

---

## 5. API Design

### 5.1 SimplyblocksStorageCluster (revised)

All imperative fields removed from `spec` and `status`. Only steady-state fields remain.

```go
type StorageClusterSpec struct {
    // All existing steady-state fields unchanged.
    // REMOVED: Action string, NodeRecycle *NodeRecycleSpec
    //          (moved to StorageClusterOps.spec)
}

type StorageClusterStatus struct {
    // All existing steady-state status fields unchanged.

    // ActiveOpsRef is the name of the currently active StorageClusterOps on this cluster.
    // Empty when no operation is in progress.
    ActiveOpsRef string `json:"activeOpsRef,omitempty"`

    // REMOVED: ActionStatus, NodeRecycleStatus
    //          (moved to StorageClusterOps.status)
}
```

### 5.2 StorageClusterOps (implemented CRD)

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=scops
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type StorageClusterOps struct { ... }

type StorageClusterOpsSpec struct {
    // ClusterRef is the name of the target SimplyblocksStorageCluster. Immutable.
    ClusterRef string `json:"clusterRef"`

    // Action is the operation to perform. Immutable.
    // +kubebuilder:validation:Enum=activate;expand;shutdown;start;restart;node-recycle
    Action string `json:"action"`

    // NodeRecycle configures behaviour specific to the node-recycle action.
    // Ignored for all other actions.
    // +optional
    NodeRecycle *NodeRecycleSpec `json:"nodeRecycle,omitempty"`
}

// NodeRecycleSpec configures the node-recycle action.
type NodeRecycleSpec struct {
    // RefreshSNodeAPI restarts the storage-node DaemonSet pod on each node after
    // the backend node is shut down and before it is restarted. Ensures the latest
    // image is running before the node comes back online.
    RefreshSNodeAPI bool `json:"refreshSNodeAPI,omitempty"`
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
    Phase       StorageClusterOpsPhase `json:"phase,omitempty"`
    Triggered   bool                   `json:"triggered,omitempty"`
    Message     string                 `json:"message,omitempty"`
    StartedAt   *metav1.Time           `json:"startedAt,omitempty"`
    CompletedAt *metav1.Time           `json:"completedAt,omitempty"`

    // NodeRecycleStatus tracks per-node progress for the node-recycle action.
    // Nil for all other actions. Survives operator restarts.
    NodeRecycleStatus *NodeRecycleStatus `json:"nodeRecycleStatus,omitempty"`
}

// NodeRecycleStatus tracks in-progress state for the node-recycle action.
type NodeRecycleStatus struct {
    PendingNodes   []string `json:"pendingNodes,omitempty"`
    ProcessedNodes []string `json:"processedNodes,omitempty"`
    // NodePhase: "shutting-down" | "snode-refresh" | "snode-refresh-wait" |
    //            "restarting" | "rebalancing"
    NodePhase      string   `json:"nodePhase,omitempty"`
    PhaseTriggered bool     `json:"phaseTriggered,omitempty"`
}
```

---

## 6. Controller Design

### 6.1 StorageClusterReconciler (revised)

Narrowed to steady-state management only:

1. Handle deletion (finalizer, backend delete call).
2. Adopt existing backend clusters (`adoptExistingCluster`).
3. Reconcile steady-state: sync `status.phase`, `status.uuid`, health fields from the backend.
4. **All imperative operations removed** — `reconcileActivate`, `reconcileExpand`,
   `failActivate`, `failExpand`, and all `spec.action` / `status.actionStatus` handling.

### 6.2 StorageClusterOpsReconciler

Drives all imperative cluster operations. Located in `storageclusterops_controller.go` and
`storageclusterops_noderecycle.go`.

**Reconcile loop:**

```
CR created →
    1. Handle deletion: release activeOpsRef, remove finalizer (see §6.3)
    2. Ensure finalizer present
    3. Terminal phase (Succeeded/Failed): remove finalizer, stop reconciling
    4. Check mutual exclusion: cluster.status.activeOpsRef == "" or == ops.Name
    5. Acquire lock: set cluster.status.activeOpsRef = ops.Name
    6. Transition Pending → Running
    7. Dispatch to action handler (see §6.4)
```

### 6.3 Finalizer

Each `StorageClusterOps` carries the finalizer `storage.simplyblock.io/storageclusterops`.
This ensures `kubectl delete` on a running ops clears `activeOpsRef` on the cluster before
the CR is garbage collected — preventing a permanently held lock.

Lifecycle:
- **Added** on first reconcile (before the ops is Running).
- **Removed** when the ops reaches a terminal phase (`Succeeded`/`Failed`).
- **Removed** when `DeletionTimestamp` is set: controller fetches the cluster, calls
  `releaseClusterLock`, removes the finalizer.

### 6.4 Action Handlers

| Action | Endpoint | Behaviour |
|---|---|---|
| `activate` | `POST /clusters/{id}/activate` | Write-ahead `Triggered=true`, poll until `status=active` |
| `expand` | `POST /clusters/{id}/expand` | Write-ahead `Triggered=true`, poll until `status=active` |
| `shutdown` | `POST /clusters/{id}/shutdown` | Write-ahead `Triggered=true`, poll until `status != active` |
| `start` | `POST /clusters/{id}/start` | Write-ahead `Triggered=true`, poll until `status=active` |
| `restart` | `POST /clusters/{id}/shutdown` → `POST /clusters/{id}/start` | Two-phase (see §6.5) |
| `node-recycle` | Per-node state machine | Multi-phase (see §7) |

> **Note:** There is no `/clusters/{id}/restart` API endpoint. The `restart` action is
> implemented as a sequenced shutdown + start (§6.5).

### 6.5 Restart Two-Phase Handler

The API has no single cluster-level restart endpoint, so `reconcileRestart` sequences
shutdown and start using `ops.Status.Message` as a sub-phase marker:

```
Triggered=false:
    POST /shutdown → Triggered=true, Message="shutting-down", requeue 10s

Triggered=true, Message="shutting-down":
    Poll cluster status; when status != active →
    POST /start → Message="starting", requeue 10s

Triggered=true, Message="starting":
    Poll cluster status; when status=active → Succeeded
```

**Triggered guard:** `Triggered=true` is persisted via `Status().Patch` *before* the next
API call is made, so operator restarts cannot issue duplicate backend calls.

---

## 7. Node-Recycle State Machine

Node-recycle is a multi-phase per-node state machine that iterates all storage nodes in
the cluster sequentially. Progress is tracked entirely in `ops.Status.NodeRecycleStatus`
so the reconciler can resume after a restart or requeue.

### 7.1 Initialisation

On first reconcile (`Triggered=false`):
1. Set `Triggered=true`, `NodeRecycleStatus=nil`, `Message="Initialising node-recycle"`.
2. On next reconcile: list all cluster storage nodes from the API.
3. Populate `NodeRecycleStatus.PendingNodes` (ordered list of UUIDs) and set
   `NodePhase=shutting-down`.

### 7.2 Per-Node Phases

For each node at the head of `PendingNodes`:

```
shutting-down ──► [snode-refresh ──► snode-refresh-wait ──►] restarting ──► rebalancing
                   (if NodeRecycle.RefreshSNodeAPI=true)
```

| Phase | What happens |
|---|---|
| `shutting-down` | POST `/storage-nodes/{id}/shutdown`; skip if already `in_shutdown`, `offline`, or `in_restart`. Poll until `offline` or `in_restart`. |
| `snode-refresh` | Delete the DaemonSet pod for this node (forces image pull). Advance to `snode-refresh-wait`. |
| `snode-refresh-wait` | Poll until the replacement pod is `Ready`. |
| `restarting` | POST `/storage-nodes/{id}/restart`; skip if already `in_restart` or `online`. Poll until `online`. |
| `rebalancing` | GET cluster; poll until `is_re_balancing=false`. |

When `rebalancing` completes: move node UUID from `PendingNodes` to `ProcessedNodes`, reset
`NodePhase` to `shutting-down` for the next node.

When `PendingNodes` is empty: Succeeded.

### 7.3 Write-Ahead Pattern

`PhaseTriggered` is persisted to `ops.Status` **before** any irreversible API call
(shutdown/restart POST, pod delete). If the operator restarts mid-phase, the
already-triggered flag prevents a duplicate API call.

### 7.4 Progress Visibility

`ops.Status.Message` is updated at each phase transition:

```
Node 2/5 (uuid): restarting
```

`kubectl describe scops <name>` shows the current node and phase without needing to inspect
the cluster CR.

---

## 8. Mutual Exclusion

`cluster.status.activeOpsRef` holds the name of the currently active `StorageClusterOps`.

- Set when the ops acquires the lock (before transitioning to Running).
- Cleared by `releaseClusterLock` when the ops reaches Succeeded or Failed, and also
  on CR deletion (via finalizer).
- A second ops on the same cluster stays in `Pending` and requeues every 10s until the
  lock is free.

`releaseClusterLock` is idempotent: it only clears `activeOpsRef` if it still matches
the calling ops name, so concurrent reconciles cannot accidentally clear a different op's lock.

---

## 9. State Machine

```
                    ┌──────────┐
   CR created  ───► │ Pending  │
                    └────┬─────┘
                         │ lock acquired, activeOpsRef set
                    ┌────▼─────┐
                    │ Running  │◄─── RequeueAfter polls
                    └────┬─────┘
             ┌───────────┴───────────┐
          success                 failure
             │                        │
    ┌────────▼─────┐        ┌─────────▼─────┐
    │  Succeeded   │        │    Failed     │
    └──────────────┘        └───────────────┘
         │                        │
         └──── finalizer removed ─┘
              activeOpsRef cleared
```

Terminal phases are no-ops on subsequent reconciles (finalizer is removed immediately on
transition, so the next reconcile after removal exits cleanly).

---

## 10. Migration Strategy

### Phase 1 — Introduce StorageClusterOps ✅ Complete

- Added `StorageClusterOps` CRD and `StorageClusterOpsReconciler`.
- `SimplyblocksStorageCluster.spec.action` still accepted as a bridge shim.

### Phase 2 — Remove legacy inline operation handling ✅ Complete

- Removed `reconcileActivate`, `reconcileExpand`, and all `spec.action` / `status.actionStatus`
  fields from `SimplyblocksStorageCluster`.
- Removed `NodeRecycleSpec` from `StorageCluster.spec`.
- Removed `NodeRecycleStatus` from `StorageCluster.status` (moved to `StorageClusterOps.status`).
- Removed bridge shim from `StorageClusterReconciler`.
- Updated CRD YAML and Helm chart.
- Users create `StorageClusterOps` CRs directly.

---

## 11. Open Questions

**Q1: StorageClusterOps retention policy**  
How long should completed `StorageClusterOps` CRs be retained? Same question as `StorageNodeOps` —
indefinite retention for audit, or a `ttlSecondsAfterFinished`-style field? Not yet decided.

**Q3: Expand operation parameters**  
`Expand` may require additional parameters (e.g. target node list). Should `StorageClusterOpsSpec`
carry a typed `ExpandParams` sub-object, or pass them as a free-form map?

**Q4: Concurrent StorageClusterOps of different types**  
Should two independent, non-conflicting operations (e.g. a status-only poll and a node-recycle)
be allowed concurrently? The current model enforces one active op per cluster — is that too
restrictive?
