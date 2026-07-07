# Design Document: Storage Node Removal with Draining Phase (Issue #131)

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [Volume Classification](#4-volume-classification)
5. [Pre-Drain Validation](#5-pre-drain-validation)
6. [State Machine](#6-state-machine)
7. [Operator Changes](#7-operator-changes)
8. [Data Model Changes](#8-data-model-changes)
9. [Backend API Requirements](#9-backend-api-requirements)
10. [Observability](#10-observability)
11. [Testing Strategy](#11-testing-strategy)
12. [Open Questions and Discussion](#12-open-questions-and-discussion)

---

## 1. Background

Simplyblock supports removing storage nodes from a storage cluster. To remove a
node safely, all logical volumes must be migrated to other nodes before the node
is shut down and removed. The existing `StorageNode.spec.action = remove` path
does not implement this — it expects the node to already be offline.

This design extends the `remove` action with a multi-step drain phase driven by
the **`StorageNodeReconciler`** (existing controller). The reconciler suspends the
node to prevent new volume placement, then creates `VolumeMigration` CRs (Issue
#130) for each eligible volume and advances through drain sub-phases on every
reconcile invocation — it never blocks in a polling loop.

If volume migration fails or the user cancels the `remove` action, the operator
**resumes** the node to restore it to operational state.

---

## 2. Goals and Non-Goals

### Goals

- Extend the existing `remove` action with a non-blocking, restartable drain
  workflow driven by the `StorageNodeReconciler`.
- Validate that the drain can complete (no unresolvable pinned or unmanaged
  volumes) before suspending the node. The node is only suspended once we are
  confident the drain can proceed to completion.
- Suspend the node after validation to prevent new volume placement while volumes are being migrated.
- Wait for suspend confirmation before starting volume migration.
- Migrate only PV-managed volumes via `VolumeMigration` CRs.
- Block and surface a clear error for non-PV-managed (manually created) volumes.
- Filter out benchmark/system volumes (auto-created for rebalancing) using a
  configurable regex.
- Support un-pinning volumes as part of the drain via `spec.unpinBeforeDrain`.
- Verify zero eligible volumes remain before triggering the backend remove.
- **Resume the node** if migration fails mid-drain or the user cancels the action.
- No new CRD or controller.

### Non-Goals

- Migrating non-PV-managed volumes automatically — these require manual
  intervention.
- Migrating volumes across different storage clusters.
- Replacing the `node-recycle` path.
- Changing volume placement at creation time.

---

## 3. Architecture Overview

```
StorageNode.spec.action = "remove"
           │
           ▼
┌──────────────────────────────────────────────────────────────────┐
│              StorageNodeReconciler (extended)                    │
│                                                                  │
│  Sub-phases (stored in actionStatus, advanced per reconcile):   │
│                                                                  │
│  1. Validating   – classify volumes; block if pinned/unmanaged  │
│                    node NOT suspended until this passes          │
│  2. Suspending   – POST /action?action=suspend                   │
│                    poll until node status = suspended            │
│  3. Migrating    – create VolumeMigration CRs for PV volumes    │
│                    watch owned VolumeMigration objects           │
│                    (requeued immediately on completion)          │
│  4. Verifying    – GET /volumes/ on node; must be empty          │
│  5. Removing     – POST /action?action=remove                   │
│  6. Completed                                                    │
│                                                                  │
│  On failure or cancellation → POST /action?action=resume         │
│  .Owns(&VolumeMigration{}) → triggered on migration completion  │
└──────────────────────────────────────────────────────────────────┘
           │ creates
┌──────────▼───────────────────────────────────────────────────────┐
│   VolumeMigration CRs  (one per PV-managed volume)              │
│   VolumeMigrationReconciler (#130) handles each independently   │
└──────────────────────────────────────────────────────────────────┘
           │ HTTP
┌──────────▼───────────────────────────────────────────────────────┐
│  SimplyBlock Backend                                             │
│  POST /storage-nodes/{id}/action?action=suspend                  │
│  GET  /storage-nodes/{id}/                                       │
│  GET  /storage-pools/{id}/volumes/?node={id}                    │
│  POST /storage-nodes/{id}/action?action=resume                   │
│  POST /storage-nodes/{id}/action?action=remove                  │
└──────────────────────────────────────────────────────────────────┘
```

---

## 4. Volume Classification

When the `remove` action is first received, the reconciler lists all backend
volumes on the target node and classifies each into one of four buckets:

| Bucket | Criteria | Action |
|---|---|---|
| **PV-managed** | Backend volume UUID matches a Kubernetes PV in the cluster | Create `VolumeMigration` CR |
| **Pinned** | Corresponding PVC carries `simplyblock.io/pinned-volume` annotation | Block; optionally un-pin if `spec.unpinBeforeDrain=true` |
| **System / benchmark** | Volume name matches `spec.systemVolumeFilterRegex` | Skip (do not migrate, do not block) |
| **Unmanaged** | No matching Kubernetes PV found | Block; requires manual resolution |

### System Volume Filtering

Benchmark volumes (auto-created by the latency rebalancer from Issue #130) must
not block drain. A configurable regex on the `StorageNode` spec filters them:

```yaml
spec:
  systemVolumeFilterRegex: "^sb-fio-baseline-.*"  # default
```

The default pattern covers rebalancing benchmark volumes. Operators can extend it
to cover other system-managed volumes.

### Pinned Volume Handling

Pinned volumes are an **operator-only concept** — the control plane has no
awareness of the `simplyblock.io/pinned-volume` annotation. The operator checks
PVC annotations directly.

Two options for the operator user:

1. **Manual**: un-pin the PVC annotation, reapply the `remove` action → drain
   proceeds automatically on the next reconcile.
2. **Automatic**: set `spec.unpinBeforeDrain: true` → the operator removes the
   annotation from all pinned PVCs on this node before suspending.

> **Important:** The node is only suspended after all blocking conditions
> (pinned + unmanaged volumes) are resolved. Suspending with known blockers serves
> no purpose and misleads the user about progress.

---

## 5. Pre-Drain Validation

Validation runs **before suspending the node**. Since pinned or unmanaged volumes
will definitively block migration, there is no benefit in suspending the node
until we know the drain can actually complete. The node is suspended only after
validation passes.

```
Validating (first — before any node modification):
  1. List all volumes on the node (backend API)
  2. For each volume:
     a. Check volume name against systemVolumeFilterRegex → skip if matches
     b. Check for matching PV → PV-managed
     c. Check PVC annotation → pinned
     d. Else → unmanaged
  3. If any pinned AND spec.unpinBeforeDrain=false:
       → emit Warning event listing pinned PVC names
       → set Blocked condition; requeue every 60s
       → STOP — node is NOT suspended; drain does not start
  4. If any unmanaged:
       → emit Warning event listing volume UUIDs
       → set Blocked condition; requeue every 60s
       → STOP — node is NOT suspended; requires manual intervention
  5. If spec.unpinBeforeDrain=true:
       → operator removes the annotation from all pinned PVCs
       → continue to Suspending
  6. All clear → advance to Suspending

Suspending (only after validation passes):
  1. POST /action?action=suspend
  2. Poll GET /storage-nodes/{id} until status == "suspended"
  3. Only after confirmed suspended → advance to Migrating
```

The node remains unmodified while blocked — the user resolves blockers by removing
pin annotations manually, setting `spec.unpinBeforeDrain: true`, or manually
migrating/deleting unmanaged volumes.

---

## 6. State Machine

Each sub-phase is stored in `actionStatus.subPhase` and advanced on each reconcile.
There are **no blocking polling loops** — the reconciler always returns quickly and
requeues.

```
  action=remove received
    │
    ▼
  Validating     ← classify volumes; block (no suspend yet) if pinned or unmanaged
    │  all clear (or unpinBeforeDrain=true resolved pins)
    ▼
  Suspending     ← POST /action?action=suspend
    │              poll until node status = "suspended"
    │  confirmed suspended
    ▼
  Migrating      ← create VolumeMigration CRs for PV-managed volumes
    │                Owns(VolumeMigration) → requeued on each completion
    │  all VolumeMigration CRs Completed
    ▼
  Verifying      ← GET /volumes/ on node — filter system volumes — assert empty
    │  confirmed empty (or only system volumes remain)
    ▼
  Removing       ← POST /action?action=remove
    │  backend confirms node removed
    ▼
  Completed
```

**Failure and cancellation — resume the node:**

If the drain fails at any point after `Suspending`, or the user removes the
`remove` action from the CR, the operator calls `POST /action?action=resume` to
restore the node to operational state before marking the action as failed or
clearing it.

| Condition | Sub-phase | Result |
|---|---|---|
| Pinned volumes found | Validating | `Blocked` condition; requeue every 60s; node untouched |
| `VolumeMigration` Failed | Migrating | Resume node → `actionStatus.state = failed` |
| Verify finds non-system volumes | Verifying | Log volume UUIDs; requeue (retry) |
| Backend remove fails | Removing | Resume node → `actionStatus.state = failed` |
| Action timeout | Suspending or later | Resume node → `actionStatus.state = failed` |
| User clears `spec.action` | Any post-suspend phase | Resume node → clear action status |

---

## 7. Operator Changes

### 7.1 Sub-Phase Tracking

`actionStatus` gains a `subPhase` string field to track which drain step is
active. This persists across operator restarts:

```go
// Persisted in StorageNode.status.actionStatus
SubPhase string `json:"subPhase,omitempty"` // Validating|Suspending|Migrating|Verifying|Removing
```

### 7.2 Async Reconcile Pattern

Each sub-phase handler returns a `ctrl.Result` immediately — never blocks:

```go
func (r *StorageNodeReconciler) performDrainAndRemove(
    ctx context.Context, snCR *v1alpha1.StorageNode, ...,
) (ctrl.Result, error) {
    switch snCR.Status.ActionStatus.SubPhase {
    case "", "Validating":
        return r.drainValidate(ctx, snCR, ...)
    case "Suspending":
        return r.drainSuspend(ctx, snCR, ...)
    case "Migrating":
        return r.drainMigrate(ctx, snCR, ...)
    case "Verifying":
        return r.drainVerify(ctx, snCR, ...)
    case "Removing":
        return r.drainRemove(ctx, snCR, ...)
    }
}
```

### 7.3 Resume on Failure or Cancellation

Any failure after the `Suspending` phase, or a user clearing `spec.action`,
triggers a resume before the action is marked failed or cleared:

```go
func (r *StorageNodeReconciler) resumeAndFail(
    ctx context.Context, snCR *v1alpha1.StorageNode,
    clusterUUID, nodeUUID, reason string,
) (ctrl.Result, error) {
    // Best-effort resume — log if it fails but don't block the failure path
    if err := r.callResumeAPI(ctx, clusterUUID, nodeUUID); err != nil {
        log.Error(err, "Failed to resume node after drain failure", "nodeUUID", nodeUUID)
    }
    return r.setActionFailed(ctx, snCR, reason)
}
```

### 7.4 VolumeMigration Lifecycle

The reconciler owns created `VolumeMigration` CRs so it is triggered immediately
when any migration completes:

```go
// In SetupWithManager:
.Owns(&simplyblockv1alpha1.VolumeMigration{})
```

**CR deletion policy:**

| State | Action |
|---|---|
| `Completed` | Delete immediately — progress is tracked in `actionStatus` counters; the CR has no further purpose |
| `Failed` | Retain until the drain itself is marked `failed`, so the operator can inspect which volume failed and why |
| `In-flight` (drain aborted) | Delete immediately — the `VolumeMigrationReconciler` handles the in-flight cancellation |

Deleting completed CRs immediately prevents accumulation on nodes with many
volumes and keeps the namespace clean. The `volumesMigrated` and `volumesPending`
counters in `actionStatus` are the source of truth for drain progress — not the
presence of `VolumeMigration` CRs.

### 7.5 New RBAC

```go
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations,verbs=get;list;watch;create;delete
```

---

## 8. Data Model Changes

### 8.1 StorageNode Spec — New Fields

```go
// SystemVolumeFilterRegex is a Go regular expression matched against backend
// volume names. Matching volumes are excluded from drain migration and from
// the final verification check. This covers benchmark volumes created by the
// latency rebalancer and any other operator-internal volumes.
// Defaults to "^sb-fio-baseline-.*".
// +optional
SystemVolumeFilterRegex *string `json:"systemVolumeFilterRegex,omitempty"`
```

### 8.2 ActionStatus — SubPhase

```go
// SubPhase tracks the active drain step within the remove action.
// One of: Validating, Suspending, Migrating, Verifying, Removing.
// +optional
SubPhase string `json:"subPhase,omitempty"`
```

### 8.3 Example Status During Migration

```yaml
status:
  actionStatus:
    action: remove
    state: running
    subPhase: Migrating
    message: "Migrating: 3 of 7 volumes migrated"
  conditions:
    - type: Blocked
      status: "False"
      reason: ValidationPassed
```

---

## 9. Backend API Requirements

| Method | Endpoint | Notes |
|---|---|---|
| `POST` | `/storage-nodes/{id}/action?action=suspend` | Idempotent; suspends node — rejects new volume placement |
| `GET` | `/storage-nodes/{id}/` | Poll for `status: suspended` confirmation |
| `GET` | `/storage-pools/{poolID}/volumes/?node={nodeID}` | List volumes on node |
| `POST` | `/storage-nodes/{id}/action?action=resume` | Restores node to operational state on failure or cancellation |
| `POST` | `/storage-nodes/{id}/action?action=remove` | Final removal after successful drain |

The suspend endpoint must be idempotent — calling it on an already-suspended node
returns success so safe reconcile retries are possible.

---

## 10. Observability

### Kubernetes Events

| Event | Type | Reason |
|---|---|---|
| Pinned volumes blocking drain | `Warning` | `PinnedVolumeBlocking` |
| Unmanaged volumes blocking drain | `Warning` | `UnmanagedVolumeBlocking` |
| Node suspended; drain started | `Normal` | `DrainStarted` |
| VolumeMigration created | `Normal` | `MigrationCreated` |
| VolumeMigration completed | `Normal` | `VolumeMigrated` |
| VolumeMigration failed | `Warning` | `MigrationFailed` |
| Verification passed | `Normal` | `VerificationPassed` |
| Node resumed after failure/cancellation | `Warning` | `NodeResumed` |
| Node removed | `Normal` | `NodeRemoved` |

### Prometheus Metrics

| Metric | Labels | Description |
|---|---|---|
| `simplyblock_node_drain_sub_phase` | `node_uuid`, `sub_phase` | Current sub-phase as a gauge |
| `simplyblock_node_drain_volumes_total` | `node_uuid` | PV-managed volumes to migrate |
| `simplyblock_node_drain_volumes_migrated` | `node_uuid` | Volumes successfully migrated |
| `simplyblock_node_drain_blocked_volumes` | `node_uuid`, `reason` | Count of blocking volumes by type |

---

## 11. Testing Strategy

### Unit Tests

**Positive cases:**
- `drainValidate`: all volumes PV-managed → advances to Suspending
- `drainValidate`: pinned volume + `unpinBeforeDrain=true` → annotation removed, advances
- `drainValidate`: volume name matches `systemVolumeFilterRegex` → excluded from blockers
- `drainSuspend`: node status becomes `suspended` → advances to Draining
- `drainMigrate`: all `VolumeMigration` CRs reach Completed → advances to Verifying
- `drainVerify`: backend returns only system volumes → passes, advances to Removing

**Negative cases:**
- `drainValidate`: pinned volume + `unpinBeforeDrain=false` → `Blocked` condition; node NOT suspended
- `drainValidate`: unmanaged volume found → `Blocked` condition with volume UUID; node NOT suspended
- `drainMigrate`: one `VolumeMigration` fails → resume called → `actionStatus.state = failed`
- `drainVerify`: non-system volume remains → requeue (retry)
- User clears `spec.action` mid-drain → resume called → action cleared
- Operator restart mid-drain → resumes from correct `SubPhase`

### Integration Tests

- Full drain of a node with 5 PV-managed volumes: assert node suspended, all
  `VolumeMigration` CRs created, all complete, node removed.
- Node with 1 pinned + 4 normal volumes: assert drain blocks without suspending,
  un-pin, assert drain resumes and completes.
- Node with 1 unmanaged volume: assert drain blocks with clear error; node not suspended.
- Migration failure mid-drain: assert resume is called; node returns to `online`.
- User cancels (clears `spec.action`) during Migrating: assert migrations cancelled,
  resume called, node returns to `online`.
- Operator pod restart during `Migrating` sub-phase: assert `SubPhase` is restored
  and drain continues without duplicate migrations.

### E2E Tests

- End-to-end node removal on a live 3-node cluster: PVCs remain accessible
  throughout drain; no I/O errors on surviving nodes.
- Drain of a node under load: sustained fio workload on PVCs being migrated;
  assert no I/O failures during migration.
- Cancellation E2E: cancel mid-drain; assert node resumes and accepts new volumes.

### Long-Term / Load Tests

- Drain of a node with 100+ volumes: assert no VolumeMigration CR limit issues;
  measure time-to-complete vs volume count.
- Concurrent drain of two nodes: assert both complete without interfering; verify
  `maxFaultTolerance` is respected if enforced.

---

## 12. Open Questions and Discussion

**Q1: How are unmanaged volumes handled long-term?**  
Currently they block the drain and require manual intervention. Should the design
include a `forceDeleteUnmanaged: true` flag that destroys non-PV-managed volumes?

**Q2: Should drain respect `maxFaultTolerance`?**  
If two nodes are being drained concurrently, concurrent `VolumeMigration` operations
could temporarily reduce redundancy below the fault tolerance threshold. Should the
drain controller check cluster replication state before creating migrations?

**Q3: What is the right action timeout for drain?**  
A node with many large volumes may take hours to drain. Should the timeout be
configurable per action, or should there be no operator-level timeout?

**Q4: Benchmark volume naming stability**  
The default regex `^sb-fio-baseline-.*` assumes a stable naming convention. Should
system volumes be identified by a label/annotation applied at creation time instead?

**Q5: What happens to in-flight migrations if the `remove` action is cancelled?**  
Should the operator delete owned `VolumeMigration` CRs (aborting in-flight
migrations) or let them complete before resuming the node?

**Q6: Resume reliability**  
The `resumeAndFail` path is best-effort — if resume itself fails, the node is left
suspended. Should the operator retry resume indefinitely until it succeeds, or alert
and leave it for manual intervention?
