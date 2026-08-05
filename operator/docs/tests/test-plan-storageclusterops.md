# Test Plan: StorageClusterOps

Related design: [`designs/design-storageclusterops.md`](../designs/design-storageclusterops.md)

Each scenario is classified as **Unit** (no cluster needed), **Integration** (live cluster,
non-destructive), or **E2E** (live cluster, may alter cluster state).

---

## Unit Tests — implemented

### `storageclusterops_controller_unit_test.go`

| Test | Scenario |
|---|---|
| `TestStorageClusterOps_TerminalSucceeded_IsNoop` | Reconcile is a no-op when phase is already `Succeeded` |
| `TestStorageClusterOps_TerminalFailed_IsNoop` | Reconcile is a no-op when phase is already `Failed` |
| `TestStorageClusterOps_ClusterNotFound_Fails` | Missing `StorageCluster` reference sets phase to `Failed` with message |
| `TestStorageClusterOps_RequeuesWhenAnotherOpsActive` | `RequeueAfter` returned when another ops holds `activeOpsRef`; ref unchanged |
| `TestStorageClusterOps_AcquiresLockAndTransitionsOutOfPending` | Lock acquired, phase advances out of `Pending`, lock released when backend call fails |
| `TestStorageClusterOps_SucceedOps_SetsPhaseAndClearsLock` | `succeedOps` sets `Succeeded`, `CompletedAt`, and clears `activeOpsRef` |
| `TestStorageClusterOps_FailOps_SetsPhaseAndClearsLock` | `failOps` sets `Failed`, message, `CompletedAt`, and clears `activeOpsRef` |
| `TestStorageClusterOps_FailOps_NilCluster_DoesNotPanic` | `failOps` with nil cluster marks ops `Failed` without panicking |
| `TestStorageClusterOps_ReleaseLock_OnlyClearsIfOwner` | `releaseClusterLock` does not clear a lock owned by a different ops |
| `TestStorageClusterOps_ReleaseLock_NilCluster_DoesNotPanic` | `releaseClusterLock` with nil cluster is a no-op |
| `TestStorageClusterOps_UnknownAction_Fails` | Unknown `spec.action` immediately sets phase to `Failed` |
| `TestStorageClusterOps_NodeRecycle_MissingNodeUUID_Fails` | `action=node-recycle` without `nodeUUID` sets phase to `Failed` |

---

## Integration Tests

### StorageClusterOps Lifecycle (non-destructive)

| Scenario | Expected | Classification |
|---|---|---|
| Create `StorageClusterOps` with `action=activate` targeting an already-active cluster | Phase transitions `Pending → Running → Succeeded`; `activeOpsRef` set then cleared | Integration |
| Describe the ops CR | `kubectl describe storageclusterops` shows events for each phase transition | Integration |
| Use short name | `kubectl get scops` returns the same list as `kubectl get storageclusterops` | Integration |
| `activeOpsRef` on cluster after `Succeeded` | `StorageCluster.status.activeOpsRef` is empty after the ops completes | Integration |
| `activeOpsRef` on cluster after `Failed` | `StorageCluster.status.activeOpsRef` is empty after the ops fails | Integration |

### Mutual Exclusion

| Scenario | Expected | Classification |
|---|---|---|
| Create two `StorageClusterOps` for the same cluster simultaneously | Second ops stays `Pending` with `RequeueAfter`; first completes; second then runs | Integration |
| Delete the active ops mid-run | `activeOpsRef` remains until the running ops reconcile clears it; second ops then proceeds | Integration |

### Triggered Guard

| Scenario | Expected | Classification |
|---|---|---|
| Operator restarts mid-`activate` after POST succeeds | `Triggered=true` in status prevents duplicate POST on resume; ops polls and completes | Integration |
| Operator restarts mid-`expand` before POST | `Triggered=false`; POST is re-sent on resume | Integration |

### Validation

| Scenario | Expected | Classification |
|---|---|---|
| Submit ops with unrecognised `action` | Phase set to `Failed` with `unknown action` message; no cluster mutation | Integration |
| Submit `action=node-recycle` without `nodeUUID` | Phase set to `Failed` with `nodeUUID required` message | Integration |
| Reference a non-existent cluster in `spec.clusterRef` | Phase set to `Failed` with cluster-not-found message | Integration |

---

## E2E Tests

### Activate

| Scenario | Expected | Classification |
|---|---|---|
| Cluster is `inactive`; create `StorageClusterOps(action=activate)` | Backend `/activate` called; controller polls until `status=active`; `StorageCluster.status` updated; ops `Succeeded` | E2E |

### Expand

| Scenario | Expected | Classification |
|---|---|---|
| Cluster has unprovisioned capacity; create `StorageClusterOps(action=expand)` | Backend `/expand` called; controller polls until `status=active`; ops `Succeeded` | E2E |

### Shutdown

| Scenario | Expected | Classification |
|---|---|---|
| Create `StorageClusterOps(action=shutdown)` | Backend `/cluster/{uuid}/shutdown` called; ops `Succeeded` | E2E |

### Restart

| Scenario | Expected | Classification |
|---|---|---|
| Create `StorageClusterOps(action=restart)` | Backend `/cluster/{uuid}/restart` called; ops `Succeeded` | E2E |

### Node Recycle

| Scenario | Expected | Classification |
|---|---|---|
| Create `StorageClusterOps(action=node-recycle, nodeUUID=<uuid>)` | Backend `/cluster/{uuid}/node-recycle/{nodeUUID}` called; ops `Succeeded` | E2E |
| Provide an invalid `nodeUUID` | Backend returns non-2xx; ops transitions to `Failed` with error message | E2E |

---

## Additional Test Scenarios (manual / to be automated)

### Operator Restart Resilience

| Scenario | Expected | Classification |
|---|---|---|
| Operator restarts while ops is `Running` and backend call is in-flight | `Triggered` guard prevents re-POST; polling resumes; ops completes correctly | E2E |
| Operator restarts while ops is `Pending` (before lock acquired) | Lock acquired fresh on restart; no duplicate backend call | Integration |

### Concurrency and Edge Cases

| Scenario | Expected | Classification |
|---|---|---|
| Two clusters each with an active `StorageClusterOps` simultaneously | Each ops runs independently; no cross-cluster lock interference | Integration |
| Create `StorageClusterOps` for a cluster that has no UUID yet | Phase set to `Failed` with informative message | Integration |
| Backend returns transient 5xx during polling | Controller requeues and retries; ops eventually succeeds once backend recovers | E2E |

---

## What Is Not Yet Covered

| Gap | Reason |
|---|---|
| `StorageClusterOps` TTL / auto-cleanup of completed ops | Feature not yet implemented |
| Webhook validation (immutable `spec.clusterRef` and `spec.action`) | Admission webhook not yet wired up |
| `action=expand` with a custom capacity argument | API contract not yet defined |
| Backend timeout during long-running activate/expand | No deadline propagation in the polling loop yet |
