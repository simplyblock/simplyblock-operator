# Test Plan: Storage Node Drain-Remove (Issue #131)

## Coverage map

Each scenario is classified as **Unit** (no cluster needed, fake client + mock HTTP),
**E2E** (live cluster required), or **Manual** (complex orchestration, described for
@RaunakJalan to implement as a structured test concept).

---

## Unit tests — already implemented

File: `internal/controller/simplyblockstoragenodeset_drain_unit_test.go`

| Test | Scenario covered |
|---|---|
| `TestDrainSkipsWhenAlreadySuccess` | Terminal state early return — success |
| `TestDrainSkipsWhenAlreadyFailed` | Terminal state early return — failed |
| `TestDrainSkipsSuccessEvenWhenSubPhaseIsEmpty` | Stale reconcile after success does not reinitialise drain |
| `TestRoundRobinDistributesEvenly` | Migration targets distributed evenly across online peers |
| `TestRoundRobinErrorsWhenNoTargetAvailable` | No draining target is available |
| `TestRoundRobinSkipsOfflineNodes` | Offline nodes excluded from round-robin |
| `TestMatchVolumesToPVs_PVManaged` | PV-managed volume classified correctly |
| `TestMatchVolumesToPVs_Pinned` | Pinned PVC classified correctly |
| `TestMatchVolumesToPVs_Unmanaged` | Unmanaged volume fails pre-validation |
| `TestMatchVolumesToPVs_SystemVolumeSkipped` | System volume excluded from all buckets |
| `TestMatchVolumesToPVs_BothPinnedAndUnmanagedVisible` | Both blocking types surface together |
| `TestMatchVolumesToPVs_EmptyNodeSkipsMigration` | Empty storage node produces no drain work |
| `TestMatchVolumesToPVs_OnlySystemVolumes` | System-volume-only node skips migration |
| `TestDrainMigrateDoesNotRecreateExistingCRs` | Operator restart idempotency — VolumeMigration CRs not duplicated |
| `TestDrainMigrateFailedCRTriggersResumeAndFail` | Failed VolumeMigration CR handled correctly |
| `TestDrainMigrationNameIsDNSValid` | Generated CR names are always valid DNS labels |
| `TestDrainCancellationSkipsWhenActionStillActive` | Stale cache does not trigger spurious resume |
| `TestDrainValidateBlocksWhenClusterRebalancing` | Cluster in rebalancing fails pre-validation |
| `TestRapidActionToggleDoesNotLeakState` | Rapid set/clear/set does not leak status |

---

## E2E tests — implemented in `regression_test/test_drain_remove.sh`

| Test | Scenario covered |
|---|---|
| Test 1 — Happy path | Full drain-remove: suspend → migrate → verify → remove |
| Test 2 — Pinned PVC | Drain blocks with `PinnedVolumeBlocking` event; node stays online |
| Test 3 — Cancel mid-drain | Node resumes to `online`; `NodeResumed` event emitted |
| Test 4 — Operator restart | Sub-phase preserved across restart; no duplicate migrations |
| Test 5 — Migration failure | `state=failed`; node resumed to `online` |
| Test 6 — fio under drain | I/O uninterrupted from suspend through final removal confirmation |

---

## Scenarios requiring additional E2E / test concept work

The following scenarios require a live cluster with specific failure injection.
They are described below for implementation by @RaunakJalan.

---

### The suspended storage node fails mid-drain

**What to verify:** If the node being drained goes offline/unreachable after
suspension (e.g. the host crashes), `drainVerify` should detect remaining
volumes and either wait (requeue) or call `resumeAndFail`. If the node is
gone, the verify step should succeed (no volumes remain).

**Test concept:**
1. Start drain on node A.
2. Wait for `subPhase=Migrating`.
3. Power off / forcibly shut down the host for node A.
4. Observe that drain either completes (verify finds no volumes) or
   enters `state=failed` cleanly — never stuck in a loop.

---

### Cluster becomes degraded during draining

**What to verify:** The drain should continue — degraded cluster status does
not block migration. The cluster status reflects degraded but volumes migrate
successfully.

**Test concept:**
1. Start drain.
2. Kill one storage node (not the one being drained) to make cluster degraded.
3. Verify drain completes; cluster returns to active after removal.

---

### Cluster falls below FTT threshold during draining

**What to verify:** If concurrent failures push the cluster below its
`maxFaultTolerance` threshold, drain should pause or fail safely rather than
removing redundancy further.

**Open question:** Should the operator check FTT before issuing `remove`?
Currently it does not. This may require a backend API to query current FTT
headroom.

**Test concept:**
1. 3-node cluster with FTT=1.
2. Bring one node offline (cluster is now at FTT limit).
3. Trigger drain on a second node.
4. Verify that drain blocks or fails rather than removing the last redundant node.

---

### Cluster rebalancing pauses drain (resume after rebalancing)

**What to verify:** `drainValidate` blocks when `StorageCluster.Status.Rebalancing=true`.
Once rebalancing finishes (status flips to false), drain should automatically
resume on the next reconcile.

**Test concept (can be semi-automated):**
1. Force cluster into rebalancing state (trigger a node-recycle).
2. Apply `action=remove` during rebalancing.
3. Observe `subPhase=Validating` with `ClusterRebalancing` event.
4. Wait for rebalancing to complete.
5. Observe drain advances to `Suspending` automatically.

---

### Storage node never reaches suspended (timeout / connection errors)

**What to verify:** `drainSuspend` retries and eventually marks the action failed
if the node never acknowledges the suspend within a reasonable bound.

**Scenarios:**
- `POST /suspend` returns 4xx (e.g. 403, 404)
- `POST /suspend` returns 5xx
- `GET /storage-nodes/{id}` returns connection refused
- Node stays in `online` status for longer than expected

**Test concept (unit — mock HTTP):**
Register mock routes that return error codes and verify `drainSuspend`
returns `RequeueAfter` rather than panicking or advancing the sub-phase.

**Test concept (E2E):**
Block the backend port using iptables on the target node during suspend and
verify the operator retries cleanly.

---

### General drainVerify failure scenarios

**What to verify:**
- `drainVerify` requeues when non-system volumes remain.
- `drainVerify` passes when only system volumes remain.
- `drainVerify` passes when volume list is empty.
- `drainVerify` handles API errors gracefully (requeue, not crash).

**Status:** The system-volume filtering is covered by the unit tests
(`TestMatchVolumesToPVs_OnlySystemVolumes`). The API-error path needs a mock
HTTP unit test.

---

### Operator restart during Suspending before Triggered persisted

**What to verify:** If the operator restarts after POSTing suspend but before
`Triggered=true` is written to status, the next reconcile reads the node status
from the backend (not from `Triggered`) and skips a duplicate suspend POST.

**Status:** The `getNodeBackendStatus` pre-check in `drainSuspend` handles this
(already suspended → advance without POST). A unit test using a mock that
asserts the suspend endpoint is called at most once would confirm idempotency.

**Test concept (unit):**
1. Seed `Triggered=false` in status but have the mock return `status=suspended`.
2. Call `drainSuspend`.
3. Assert suspend POST was never called (verify mock call count = 0).

---

### Operator restart during storage node removal

**What to verify:** If the operator restarts while `subPhase=Removing`, the
next reconcile re-issues the DELETE. The backend must be idempotent (already
removed → 404 or 204), and the operator treats both as success.

**Status:** `drainRemove` accepts 200, 204, and 404 as success. A unit test
with each status code variant confirms this.

---

### Control plane rejects storage node suspension (4xx / 5xx)

**What to verify:** `drainSuspend` logs the error and requeues — it does not
advance the sub-phase or crash.

**Test concept (unit):**
Mock `POST /suspend` to return 403 / 503. Verify `drainSuspend` returns
`RequeueAfter > 0` and `subPhase` is still `Suspending`.

---

### Control plane rejects storage node removal (4xx / 5xx)

**What to verify:** `drainRemove` calls `resumeAndFail` on non-2xx/404
responses. The node is resumed and `state=failed`.

**Status:** Covered by the existing error path in `drainRemove`. A unit test
with mock status codes would confirm.

---

### A PVC's owning pod is in deletion

**What to verify:** A PVC whose pod has `deletionTimestamp` set is still
classified as PV-managed (the pod state does not affect volume bucketing).
The drain proceeds normally; the pod is in the process of being torn down.

**Test concept (unit):**
Create a PV/PVC pair where the pod has `DeletionTimestamp != nil`. Verify
`matchVolumesToPVs` still places the volume in `pvManaged`, not `unmanaged`.

---

### A PVC's owning pod is not running

**What to verify:** A volume attached to a non-Running pod (Pending, Failed,
Succeeded) is still PV-managed and migrated. The operator does not gate
migration on pod state.

**Test concept (unit):**
Create PV/PVC without a pod (or with a failed pod). Verify `matchVolumesToPVs`
classifies correctly.

---

### Draining target (nodeUUID in CR) changed during a running drain

**What to verify:** If a user patches `spec.nodeUUID` to a different value
while `subPhase=Migrating`, the operator should detect the mismatch
(ActionStatus.NodeUUID ≠ Spec.NodeUUID) and either fail or ignore the spec
change until the current drain completes.

**Current behaviour:** `performDrainAndRemove` checks
`ActionStatus.NodeUUID == Spec.NodeUUID` for the terminal-state guard.
A mismatch on a running drain is not currently protected — `drainMigrate`
uses `Spec.NodeUUID` for label queries, which would silently switch target.

**Recommended fix:** Add a guard in `performDrainAndRemove`:
```go
if snCR.Status.ActionStatus.NodeUUID != snCR.Spec.NodeUUID {
    // NodeUUID changed mid-drain — fail the current drain and start fresh.
    return r.resumeAndFail(ctx, apiClient, clusterUUID, snCR,
        "drain aborted: nodeUUID changed mid-operation")
}
```
