# Test Plan: StorageNode / StorageNodeOps Three-Tier Model

Related design: [`designs/design-storagenodeset-storagenode.md`](../designs/design-storagenodeset-storagenode.md)  
Regression script: [`regression_test/test_storagenode_ops.sh`](../../../../regression_test/test_storagenode_ops.sh)

Each scenario is classified as **Unit** (no cluster needed), **Integration** (live cluster,
non-destructive), or **E2E** (live cluster, may remove/modify nodes).

---

## Unit Tests — implemented

### `storagenode_controller_unit_test.go`

| Test | Scenario |
|---|---|
| `TestSyncOverrides_PropagatesNodeConfigs` | `nodeConfigs` entry propagated to `StorageNode.spec.overrides` |
| `TestSyncOverrides_NoopWhenWorkerNotInNodeConfigs` | No overrides when worker absent from `nodeConfigs` |
| `TestEffectiveNodeConfig_OverridesTakePrecedence` | Per-node override wins over fleet default |
| `TestEffectiveNodeConfig_FallsBackToFleetDefault` | Fleet default used when no override set |
| `TestEffectiveFailureDomain_OverrideTakesPrecedenceOverMap` | `overrides.failureDomain` beats `nodeFailureDomains` map |
| `TestEffectiveFailureDomain_FallsBackToMap` | `nodeFailureDomains[worker]` used when no override |
| `TestEffectiveFailureDomain_ZeroWhenNotSet` | Returns 0 when neither source is set |
| `TestCheckFailureDomain_BlocksWhenEnabledAndNotSet` | Node-add blocked when `enableFailureDomains=true` and no domain set |
| `TestCheckFailureDomain_AllowsWhenFailureDomainSet` | No error when failure domain is populated |
| `TestCheckFailureDomain_SkipsWhenFeatureDisabled` | No error when feature disabled |
| `TestEnsureRemoveOps_CreatesOpsWhenMissing` | `StorageNodeOps(action=remove)` created on deletion |
| `TestEnsureRemoveOps_IdempotentWhenAlreadyExists` | Second call is a no-op |
| `TestHandleDeletion_RemovesFinalizerWhenNeverProvisioned` | Unprovisioned node skips ops, finalizer removed immediately |

### `storagenodeops_controller_unit_test.go`

| Test | Scenario |
|---|---|
| `TestAcquireLock_SetsActiveOpsRefAndTransitionsToRunning` | Lock acquired, ops transitions to Running |
| `TestAcquireLock_RequeuesWhenAnotherOpsActive` | Lock blocked when `activeOpsRef` already set |
| `TestAcquireLock_RemoveDrainSetsValidatingSubPhase` | `action=remove` initialises sub-phase to Validating |
| `TestSucceedOps_SetsPhaseAndClearsLock` | Succeeded phase set, `activeOpsRef` cleared |
| `TestFailOps_SetsPhaseAndClearsLock` | Failed phase set with message, `activeOpsRef` cleared |
| `TestReleaseLock_OnlyClearsIfOwner` | Lock not cleared when called by non-owner |
| `TestAdvanceSubPhase_UpdatesSubPhaseAndResetsTrigger` | Sub-phase advances, `Triggered` reset to false |
| `TestDispatch_UnknownActionFails` | Unknown action immediately transitions to Failed |
| `TestResolveOpsSystemVolumeFilter_UsesDefaultWhenNoDrain` | Default regex matches `sb-fio-baseline-*` |
| `TestResolveOpsSystemVolumeFilter_UsesCustomPattern` | Custom regex overrides default |
| `TestResolveOpsSystemVolumeFilter_InvalidPatternReturnsError` | Invalid regex returns error |

### `simplyblockstoragenodeset_drain_unit_test.go` (standalone helpers)

| Test | Scenario |
|---|---|
| `TestRoundRobinDistributesEvenly` | Migration targets distributed evenly across online peers |
| `TestRoundRobinErrorsWhenNoTargetAvailable` | Error when no online peer available |
| `TestRoundRobinSkipsOfflineNodes` | Offline nodes excluded from round-robin |
| `TestMatchVolumesToPVs_PVManaged` | PV-managed volume classified correctly |
| `TestMatchVolumesToPVs_Pinned` | Pinned PVC classified correctly |
| `TestMatchVolumesToPVs_Unmanaged` | Unmanaged volume classified correctly |
| `TestMatchVolumesToPVs_SystemVolumeSkipped` | System volume excluded from all buckets |
| `TestMatchVolumesToPVs_EmptyNodeSkipsMigration` | Empty node produces no drain work |
| `TestMatchVolumesToPVs_OnlySystemVolumes` | System-volume-only node skips migration |
| `TestDrainMigrationNameNoCollisionOnLongPVNames` | Long PV names produce unique CR names |
| `TestDrainMigrationNameIsDNSValid` | Generated CR names are always valid DNS labels |

---

## Integration Tests — `regression_test/test_storagenode_ops.sh`

### StorageNode Lifecycle (non-destructive)

| Test # | Scenario | Classification |
|---|---|---|
| 1 | `StorageNode` CRs created for all `spec.workerNodes` | Integration |
| 2 | `StorageNode` status fields: `uuid`, `status=online`, `socketIndex`, `health` all populated | Integration |
| 3 | `StorageNodeSet.status` aggregation: `totalNodes` / `onlineNodes` match worker count | Integration |
| 4 | `nodeConfigs` overrides reflected in `StorageNode.spec.overrides` | Integration |
| 10 | `StorageNodeSet` wide columns (`-o wide`) show `OFFLINE`, `SUSPENDED`, `CREATING`, `REMOVED` | Integration |

### StorageNodeOps Lifecycle

| Test # | Scenario | Classification |
|---|---|---|
| 6 | Mutual exclusion: second ops requeues while first holds `activeOpsRef` | Integration |
| 7 | Unknown action → `Failed` phase immediately | Integration |
| 8 | `activeOpsRef` cleared after Succeeded and Failed outcomes | Integration |
| 9 | Events mirrored onto `StorageNode` CR (`kubectl describe storagenode` shows events) | Integration |

### Drain / Remove (destructive — removes a node)

| Test # | Scenario | Classification |
|---|---|---|
| 5 | Happy path remove: drain completes, `StorageNode.status=removed`, `phase=Succeeded` | E2E |

### Cluster Expansion

| Test # | Scenario | Classification |
|---|---|---|
| 11 | Manually created `StorageNode` CR references existing `StorageNodeSet`: survives reconciler, worker labeled, EndpointSlice updated, pod scheduled, node provisions, status in `StorageNodeSet.status.nodes[]` | E2E |

---

## Additional Test Scenarios (manual / to be automated)

### Parallel Node Add

| Scenario | Expected | Classification |
|---|---|---|
| Add 5 workers with `maxParallelNodeAdds=2` | At most 2 nodes in-flight simultaneously (`PostedAt` set, UUID empty) | E2E |
| Add FDB + non-FDB workers simultaneously | FDB workers sequential; non-FDB respect `MaxParallelNodeAdds` | E2E |
| Second FDB worker while first is in-flight | Second FDB worker requeues until first comes online | E2E |

### Per-Node Configuration (DaemonSet)

| Scenario | Expected | Classification |
|---|---|---|
| `nodeConfigs[worker].maxLogicalVolumeCount` differs per node | Per-node ConfigMap has correct `MAX_LVOL` per worker | Integration |
| `nodeConfigs[worker].spdkSystemMemory` differs per node | Init container receives correct `spdk_sys_mem` in POST | E2E |
| `nodeConfigs[worker].deviceNames` set | `--nvme-devices` arg in init container uses override | Integration |
| ConfigMap created before DaemonSet on fresh install | Init container finds non-empty env file; no `MAX_LVOL=0` failure | E2E |

### Failure Domain

| Scenario | Expected | Classification |
|---|---|---|
| `enableFailureDomains=true`, no `failureDomain` set | Node-add blocked with `FailureDomainMissing` event on `StorageNode` | Integration |
| `failureDomain` in `nodeConfigs` | Sent as `failure_domain` in POST; reflected in `StorageNode.spec.overrides` | E2E |
| `overrides.failureDomain` overrides `nodeFailureDomains` map | Per-node override wins | Integration |

### Drain Robustness (see also `test-plan-drain-remove.md`)

| Scenario | Expected | Classification |
|---|---|---|
| Pinned PVC blocks drain | `PinnedVolumeBlocking` event; drain stalls; unblocks after annotation removed | E2E |
| Cancel drain mid-Suspending | Node resumed, `activeOpsRef` cleared | E2E |
| Cancel drain mid-Migrating | In-flight `VolumeMigration` CRs deleted, node resumed | E2E |
| Cluster degraded during drain | `DrainPaused` event; resumes when cluster active | E2E |
| Operator restart mid-drain | Sub-phase (`SubPhase`) preserved; drain resumes from same phase | E2E |
| Remove empty node (no PVCs) | No `VolumeMigration` CRs created; completes directly | E2E |
| VolumeMigration fails | Failed CR deleted; new CR created with fresh round-robin target | E2E |

### Validation

| Scenario | Expected | Classification |
|---|---|---|
| Duplicate entry in `spec.workerNodes` | API server rejects with `duplicate value not allowed` | Integration |
| `nodeConfigs` key not in `spec.workerNodes` | API server rejects with `nodeConfigs keys must match a workerNode entry` | Integration |
| `nodeFailureDomains` value < 1 | API server rejects with validation message | Integration |

---

## What Is Not Yet Covered

| Gap | Reason |
|---|---|
| `StorageNodeOps` TTL / auto-cleanup of completed ops | Feature not yet implemented |
| Phase 3 migration (remove `status.nodes[]` from `StorageNodeSet`) | Not yet implemented |
| Scale-down triggered drain (shrink `spec.workerNodes` while nodes are online) | Not yet implemented |
| Multi-socket worker (2+ `StorageNode` CRs per worker) | Requires specific hardware in test environment |
| Node adoption on fresh operator install (backend node exists, no `StorageNode` CR) | `pollUUIDFromBackend` handles this but no explicit test |
