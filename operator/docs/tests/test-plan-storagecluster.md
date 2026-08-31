# Test Plan: StorageCluster and StorageClusterOps

Related design: [`designs/crd-redesign/design-storagecluster.md`](../designs/crd-redesign/design-storagecluster.md)

Supersedes `test-plan-storageclusterops.md`, removed in the same change, whose
scenarios were prose rows without permanent identifiers. They are re-expressed here
with IDs, and the operation scenarios keep their original wording where it survived.

Scope is the operator, its webhooks, and the Kubernetes surface this repository
builds. The control plane (`sbcli`) is a dependency, faked at the boundary: what a
row asserts is the operator's response to an answer, never the control plane's own
behavior.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the `Test`
column means nothing implements the scenario yet, and every such row reappears in
§6 with its reason.

Scenario text names the target spelling, because the design is a target-model
document and a row that cited two spellings would need updating twice. Every
action value is lowercase or kebab-case today and PascalCase after the rename in
design §5.3, so `activate` is `Activate` and `node-rolling-restart` is
`RollingRestart`. The rolling restart's spec block is
`nodeRollingRestart.refreshSNodeAPI` today and `rollingRestart.refreshSNodeAPI`
after, and its status block is `status.nodeRollingRestartStatus` today and
`status.rollingRestart` after.

| Class       | Prefix | Harness                                                                |
|-------------|--------|------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, and a mock backend |
| Integration | `I-`   | Full reconcile loop against `envtest` and a mock backend               |
| E2E         | `E-`   | Live simplyblock cluster, real data path                               |
| Manual      | `M-`   | Needs failure injection or orchestration not automated yet             |

---

## 1. Unit Tests

Pure functions and single reconcile calls against a fake client, with the control
plane replaced by `webapimock.NewSpecServerFromFile` validating against
`shared/openapi.json`. No Kubernetes API server is involved.

### Entity Reconciler: Deletion and Finalizer (design §4.5)

File: `operator/internal/controllers/cluster/storagecluster_controller_unit_test.go`

| #    | Scenario                                                                             | Type     | Test                             |
|------|--------------------------------------------------------------------------------------|----------|----------------------------------|
| U-01 | No deletion timestamp: `handleDeletion` is a pass-through                            | Negative | `TestClusterHandleDeletionPaths` |
| U-02 | Backend unreachable while `status.uuid` is set: requeue, finalizer kept              | Negative | `TestClusterHandleDeletionPaths` |
| U-03 | Backend `DELETE` succeeds: finalizer removed                                         | Positive | `TestClusterHandleDeletionPaths` |
| U-04 | Finalizer absent: added on first reconcile                                           | Positive | `TestClusterEnsureFinalizer`     |
| U-05 | Deleting a CR that never got a `status.uuid`: finalizer removed with no backend call | Boundary | —                                |

### Entity Reconciler: Dispatch (design §4.1)

File: `operator/internal/controllers/cluster/storagecluster_controller_unit_test.go`

| #    | Scenario                                                      | Type     | Test                                       |
|------|---------------------------------------------------------------|----------|--------------------------------------------|
| U-06 | Finalizer added on the first reconcile of a new CR            | Positive | `TestStorageClusterReconcileTopLevelPaths` |
| U-07 | `status.uuid` present: reconcile takes the periodic sync path | Positive | `TestStorageClusterReconcileTopLevelPaths` |
| U-08 | CR not found: reconcile returns without requeue               | Negative | `TestStorageClusterReconcileTopLevelPaths` |

### Entity Reconciler: Creation (design §4.2)

File: `operator/internal/controllers/cluster/storagecluster_controller_unit_test.go`

| #    | Scenario                                                                   | Type     | Test                                            |
|------|----------------------------------------------------------------------------|----------|-------------------------------------------------|
| U-09 | Readiness check fails: requeue, no `POST` issued                           | Negative | `TestStorageClusterReconcileCreationPaths`      |
| U-10 | Creation `POST` fails and no cluster of that name exists: requeue          | Negative | `TestStorageClusterReconcileCreationPaths`      |
| U-11 | Creation response unparseable: requeue                                     | Negative | `TestStorageClusterReconcileCreationPaths`      |
| U-12 | Creation succeeds: status populated and the credentials Secret written     | Positive | `TestStorageClusterReconcileCreationPaths`      |
| U-13 | Creation succeeds: `status.erasureCodingScheme` rendered from NDCS/NPCS    | Positive | `TestStorageClusterReconcileCreationPaths`      |
| U-14 | Second reconciler at the same `resourceVersion`: 409, backs off, no `POST` | Negative | `TestReconcileCreateOptimisticLockPreventsRace` |
| U-15 | `status.phase` cleared once creation completes                             | Positive | `TestReconcileCreateOptimisticLockPreventsRace` |
| U-16 | CSI credentials Secret upserted into the operator namespace                | Positive | `TestUpsertCSICredentialsSecret`                |

### Entity Reconciler: Adoption (design §4.3)

File: `operator/internal/controllers/cluster/storagecluster_controller_unit_test.go`

| #    | Scenario                                                                     | Type     | Test |
|------|------------------------------------------------------------------------------|----------|------|
| U-17 | Upgrade Secret present and readable: cluster adopted without a `POST`        | Positive | —    |
| U-18 | Upgrade Secret present but `uuid` or `secret` empty: falls through to create | Negative | —    |
| U-19 | Creation `POST` fails but the cluster exists by name: adopted instead        | Positive | —    |
| U-20 | Adoption writes the per-cluster Secret with a controller reference           | Positive | —    |

### Entity Reconciler: Steady-State Sync (design §4.4)

File: `operator/internal/controllers/cluster/storagecluster_controller_unit_test.go`

| #    | Scenario                                                                    | Type     | Test |
|------|-----------------------------------------------------------------------------|----------|------|
| U-21 | Backend reports the same status, NQN, and rebalancing flag: no patch issued | Boundary | —    |
| U-22 | Backend status changed: status patched and requeued after 30s               | Positive | —    |
| U-23 | Backend `GET` fails: requeue, status left untouched                         | Negative | —    |
| U-24 | Per-cluster Secret missing: sync proceeds without the credentials upsert    | Boundary | —    |

### Spec Derivation Helpers (design §3.1)

File: `operator/internal/controllers/cluster/storagecluster_backup_test.go`

| #    | Function                      | Scenario                                                 | Type     | Test                                    |
|------|-------------------------------|----------------------------------------------------------|----------|-----------------------------------------|
| U-25 | `buildBackupConfig`           | Credentials resolved from the referenced Secret          | Positive | `TestBuildBackupConfig`                 |
| U-26 | `buildBackupConfig`           | Secret missing a required key                            | Negative | `TestBuildBackupConfigMissingSecretKey` |
| U-27 | `effectiveConcurrentRestarts` | `spec` value below fault tolerance: `spec` value wins    | Positive | —                                       |
| U-28 | `effectiveConcurrentRestarts` | `spec` value above fault tolerance: clamped to tolerance | Boundary | —                                       |
| U-29 | `effectiveConcurrentRestarts` | `spec` value equal to fault tolerance: neither clamps    | Boundary | —                                       |
| U-30 | `effectiveConcurrentRestarts` | Both `nil`: defaults to 1                                | Boundary | —                                       |
| U-31 | `effectiveConcurrentRestarts` | Fault tolerance zero or `nil`: no clamp applied          | Boundary | —                                       |
| U-32 | `capacityThreshold`           | `nil` threshold block yields zero                        | Boundary | —                                       |
| U-33 | `stripeDataChunks`            | `nil` stripe block yields 1, not zero                    | Boundary | —                                       |
| U-34 | `stripeParityChunks`          | `nil` stripe block yields 1, not zero                    | Boundary | —                                       |
| U-35 | `buildHashicorpVaultConfig`   | Non-HTTP base URL rejected                               | Negative | —                                       |

### Operation Reconciler: Lifecycle and Lock (design §6.1, §8)

File: `operator/internal/controllers/cluster/storageclusterops_controller_unit_test.go`

| #    | Scenario                                                                       | Type     | Test                                                           |
|------|--------------------------------------------------------------------------------|----------|----------------------------------------------------------------|
| U-36 | Phase already `Succeeded`: reconcile is a no-op                                | Negative | `TestStorageClusterOps_TerminalSucceeded_IsNoop`               |
| U-37 | Phase already `Failed`: reconcile is a no-op                                   | Negative | `TestStorageClusterOps_TerminalFailed_IsNoop`                  |
| U-38 | `spec.clusterRef` names no cluster: phase becomes `Failed` with a message      | Negative | `TestStorageClusterOps_ClusterNotFound_Fails`                  |
| U-39 | Another operation holds `activeOpsRef`: stays `Pending`, ref unchanged         | Negative | `TestStorageClusterOps_RequeuesWhenAnotherOpsActive`           |
| U-40 | Lock free: acquired, phase leaves `Pending`, released when the call fails      | Positive | `TestStorageClusterOps_AcquiresLockAndTransitionsOutOfPending` |
| U-41 | `succeedOps` sets `Succeeded`, `completedAt`, and clears the lock              | Positive | `TestStorageClusterOps_SucceedOps_SetsPhaseAndClearsLock`      |
| U-42 | `failOps` sets `Failed`, the message, `completedAt`, and clears the lock       | Positive | `TestStorageClusterOps_FailOps_SetsPhaseAndClearsLock`         |
| U-43 | `failOps` with a `nil` cluster does not panic                                  | Boundary | `TestStorageClusterOps_FailOps_NilCluster_DoesNotPanic`        |
| U-44 | `releaseClusterLock` leaves a lock owned by a different operation alone        | Boundary | `TestStorageClusterOps_ReleaseLock_OnlyClearsIfOwner`          |
| U-45 | `releaseClusterLock` with a `nil` cluster is a no-op                           | Boundary | `TestStorageClusterOps_ReleaseLock_NilCluster_DoesNotPanic`    |
| U-46 | Unrecognized `spec.action`: phase becomes `Failed` immediately                 | Negative | `TestStorageClusterOps_UnknownAction_Fails`                    |
| U-47 | Deletion while `Running`: lock released before the finalizer is removed        | Positive | —                                                              |
| U-48 | Terminal phase reached but lock still held: released by the later reconcile    | Boundary | —                                                              |
| U-49 | Two operations pass the free-check together: the loser gets a 409 and requeues | Boundary | —                                                              |

### Operation Reconciler: Actions (design §6.3, §6.4)

File: `operator/internal/controllers/cluster/storageclusterops_controller_unit_test.go`

| #        | Scenario                                                                                                                          | Type     | Test |
|----------|-----------------------------------------------------------------------------------------------------------------------------------|----------|------|
| ~~U-50~~ | `Activate`: `triggered` persisted before the `POST` is issued. Design §6.2 removes the flag, and the persisted step is the record | —        | —    |
| ~~U-51~~ | `Activate` with `triggered` already true: polls without re-posting. Superseded by `U-SM-08`                                       | —        | —    |
| U-52     | `Shutdown` completes on `status != active`, not on `status == active`                                                             | Boundary | —    |
| U-53     | `Restart` first pass: `POST /shutdown`, message becomes `shutting-down`                                                           | Positive | —    |
| U-54     | `Restart` second pass: cluster left `active`, `POST /start` issued                                                                | Positive | —    |
| U-55     | `Restart` third pass: cluster `active` again, phase becomes `Succeeded`                                                           | Positive | —    |
| U-56     | `Restart` while the cluster has not yet left `active`: holds, no `POST /start`                                                    | Boundary | —    |

### Operation Reconciler: Rolling Restart (design §7)

File: `operator/internal/controllers/cluster/storageclusterops_controller_unit_test.go`

| #        | Scenario                                                                                                    | Type     | Test                                                   |
|----------|-------------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------------|
| U-57     | First reconcile sets `triggered` and transitions to `Running`                                               | Positive | `TestStorageClusterOps_NodeRollingRestart_Initialises` |
| U-58     | Second reconcile populates `pendingNodes` from the node list                                                | Positive | —                                                      |
| U-59     | A peer node is not `online`: the walk holds, no shutdown issued                                             | Negative | —                                                      |
| U-60     | All peers `online`: shutdown issued for the head of `pendingNodes`                                          | Positive | —                                                      |
| U-61     | Node already `in_shutdown`, `offline`, or `in_restart`: shutdown `POST` skipped                             | Boundary | —                                                      |
| U-62     | `refreshSNodeAPI` false: `snode-refresh` phases skipped entirely                                            | Boundary | —                                                      |
| U-63     | `refreshSNodeAPI` true: pod deleted, then awaited `Ready` before `restarting`                               | Positive | —                                                      |
| U-64     | `rebalancing` completes: node moves to `processedNodes`, phase resets                                       | Positive | —                                                      |
| U-65     | `pendingNodes` empty: phase becomes `Succeeded`                                                             | Boundary | —                                                      |
| U-66     | Single-node cluster: the walk has no peers to check and completes                                           | Boundary | —                                                      |
| ~~U-67~~ | `phaseTriggered` already true: the irreversible call is not repeated. Superseded by `U-SM-08` and `U-SM-23` | —        | —                                                      |

### Creation State Machine (design §4.2, not yet built)

File: not yet created. These become required when the creation path becomes a
declared graph. They are excluded from the counts in §5.

| #       | Scenario                                                                                            |
|---------|-----------------------------------------------------------------------------------------------------|
| U-CM-01 | The creation graph is closed: every edge names a declared step                                      |
| U-CM-02 | Transition into `Claiming` is persisted with an optimistic lock, and a second reconciler gets a 409 |
| U-CM-03 | A 409 on the claim leaves `status.step` untouched and issues no backend call                        |
| U-CM-04 | `CheckingControlPlane` to `Adopting` when the upgrade Secret is present and complete                |
| U-CM-05 | `CheckingControlPlane` to `ResolvingConfig` when the upgrade Secret is absent                       |
| U-CM-06 | `Creating` to `Adopting` when the `POST` fails and the cluster exists by name                       |
| U-CM-07 | `Creating` to `Persisting` on a successful `POST`                                                   |
| U-CM-08 | `Claiming` to `Persisting` directly is an `IllegalTransitionError`                                  |
| U-CM-09 | Restoring at `Creating` runs no entry hook, so the `POST` is not repeated                           |
| U-CM-10 | `status.step.deadline` expired on `CheckingControlPlane`: reported rather than requeued forever     |
| U-CM-11 | `status.phase` reaches `Online` only once `status.uuid` is persisted                                |
| U-CM-12 | A converted `subPhase` of `"creating"` restores as `step.state=Claiming` with no deadline           |

---

### Push-Driven Reconciliation (design §4.4, not yet built)

File: not yet created. These become required when the controllers move off polling.
They are excluded from the counts in §5.

The `U-CP-` block covers this controller's use of the control-plane stream. The
stream itself, its reconnect behavior, and the store are the `cpinformer` package's
own tests on the `sse` branch, not this plan's.

| #       | Scenario                                                                                              |
|---------|-------------------------------------------------------------------------------------------------------|
| U-CP-01 | An `updated` cluster event enqueues exactly one reconcile for the matching `StorageCluster`           |
| U-CP-02 | An event for a cluster UUID no CR references enqueues nothing                                         |
| U-CP-03 | Status is written from the streamed DTO without a `GET` to the control plane                          |
| U-CP-04 | Streamed state identical to `status`: no patch issued                                                 |
| U-CP-05 | A step whose predicate is already satisfied by the first snapshot advances without waiting            |
| U-CP-06 | Coalesced delivery skipping `offline` and `in_restart`: a step waiting for `offline` accepts `online` |
| U-CP-07 | Coalesced delivery skipping past `rebalancing`: the walk advances to the next node                    |
| U-CP-08 | A reconnect snapshot re-satisfies a step already advanced past: no repeated side effect               |
| U-CP-09 | Stream dead and the backstop requeue fires: the controller reads the control plane directly           |
| U-CP-10 | `snode-refresh-wait` still uses pod readiness rather than the stream                                  |
| U-CP-11 | Cluster creation still probes `/_meta/ready` directly rather than through a stream                    |
| U-CP-12 | Two `StorageCluster` CRs in different namespaces served by the one root-scoped subscription           |

---

### Ops Shape: Step Machine (design §5.3, not yet built)

File: not yet created. These scenarios become required when `StorageClusterOps`
adopts the `Ops` shape, and they carry no `Type` column until the work is scoped.
They are excluded from the counts in §5, which measure the shape as it stands.

The `U-SM-` block is numbered independently, because it lands as one piece of work
rather than alongside the rows above.

| #       | Scenario                                                                                                            |
|---------|---------------------------------------------------------------------------------------------------------------------|
| U-SM-01 | `MultiConfig` declares a graph for each of the six actions, and building any machine validates all six              |
| U-SM-02 | A bad edge in the `RollingRestart` graph fails when a machine is built for `Activate`                               |
| U-SM-03 | An `Activate` op cannot transition to `Rebalancing`: `IllegalTransitionError`                                       |
| U-SM-04 | An action absent from the `MultiConfig`: `ErrUnknownAction` rather than a stall                                     |
| U-SM-05 | `spec.action` unrecognized after a downgrade: the operation fails rather than panicking the controller              |
| U-SM-06 | Empty `status.step` restores to the graph's initial state without an entry hook firing                              |
| U-SM-07 | Unrecognized non-empty `status.step` is an error, not a reset to initial                                            |
| U-SM-08 | `Restore` runs no entry hook, so a step already acted on does not repeat its call                                   |
| U-SM-09 | `Restart`: `ShuttingDown` to `Starting` is legal, and the reverse is not                                            |
| U-SM-10 | `RollingRestart`: `Rebalancing` is terminal, so the graph declares no next-node edge                                |
| U-SM-11 | `RollingRestart` with `refreshSNodeAPI` false: `ShuttingDownNode` to `RestartingNode` directly                      |
| U-SM-12 | `status.step.deadline` in the past restores as expired, so `TimeoutReached` fires on the first pass                 |
| U-SM-13 | `status.step` with a state but no deadline yields no requeue from `RequeueAfter`                                    |
| U-SM-14 | `Aborted` is terminal, and an aborted operation releases the cluster lock                                           |
| U-SM-15 | The phase machine and the step machine are separate, and a step transition does not move the phase                  |
| U-SM-17 | `Rebalancing` reached: `Reset` returns the machine to `CheckingPeers` and clears the deadline                       |
| U-SM-18 | `Reset` runs no entry hook, so the peer check is performed by the next pass rather than by the reset                |
| U-SM-19 | `nodeIndex` increments once per completed node, and `nodes` is never modified after the walk starts                 |
| U-SM-20 | `nodeIndex` reaching `len(nodes)` moves the phase to `Succeeded`                                                    |
| U-SM-21 | `nodeIndex` of zero with a non-empty `nodes` is the first node, not an unset walk                                   |
| U-SM-22 | The step is persisted before the side effect that step performs                                                     |
| U-SM-23 | A step recorded whose call never fired: the call is made again and is a no-op because the target is already past it |
| U-SM-24 | Every state each graph declares appears in the step `Enum` marker                                                   |
| U-SM-25 | Every state each graph declares appears in the `status.step` CEL rule                                               |
| U-SM-26 | The CEL rule names no value the graphs do not declare                                                               |
| U-SM-27 | A stored step belonging to another action: `ErrUnknownState`, naming the declared set                               |
| U-SM-28 | A restore that fails: the operation is `Failed` with the error, not requeued                                        |
| U-SM-16 | A `subPhase` string converted into `step.state` restores with no deadline rather than an expired one                |

---

## 2. Integration Tests

Full reconcile loop against a real Kubernetes API server via `envtest`, driven by
`TestControllers` in `operator/internal/controllers/cluster/suite_test.go`, with the control
plane still mocked. These cover what a fake client cannot: real admission, real
`resourceVersion` semantics, and real watch delivery.

File: `operator/internal/controllers/cluster/storagecluster_controller_test.go`

| #    | Scenario                                                                              | Type     | Test              |
|------|---------------------------------------------------------------------------------------|----------|-------------------|
| I-01 | Reconciling a not-found resource returns no requeue                                   | Negative | `TestControllers` |
| I-02 | `spec.stripe` changed after creation: rejected by the CEL rule                        | Negative | —                 |
| I-03 | `spec.fabricType` omitted at creation, set later: accepted, then frozen               | Boundary | —                 |
| I-04 | `spec.enableFailureDomains` omitted at creation, set later: rejected                  | Boundary | —                 |
| I-05 | `spec.enableNodeAffinity` set at creation, changed later: rejected                    | Negative | —                 |
| I-06 | `spec.maxSubsystemCount` omitted: creation rejected as `Required`                     | Negative | —                 |
| I-07 | `spec.maxSubsystemCount` of 9 and of 76: both rejected by the range                   | Boundary | —                 |
| I-08 | `spec.maxSubsystemCount` of 10 and of 75: both accepted                               | Boundary | —                 |
| I-09 | `spec.vcpuCount` of 5 rejected, of 6 accepted                                         | Boundary | —                 |
| I-10 | `spec.maxConcurrentWorkerRestarts` of 0: rejected by the minimum                      | Boundary | —                 |
| I-11 | `spec.volumeMigrationSettings.dataRealignment.minMoves` of 0: rejected                | Boundary | —                 |
| I-12 | `spec.action` on `StorageClusterOps` outside the enum: rejected by admission          | Negative | —                 |
| I-13 | `spec.clusterRef` changed after creation: rejected as immutable                       | Negative | —                 |
| I-14 | Two `StorageClusterOps` for one cluster: the second stays `Pending`, then runs        | Positive | —                 |
| I-15 | Lock released: the queued operation wakes from the cluster watch, not the 10s requeue | Positive | —                 |
| I-16 | `kubectl delete` on a `Running` operation: `activeOpsRef` cleared                     | Positive | —                 |
| I-17 | Operations on two different clusters run concurrently without interference            | Positive | —                 |
| I-18 | Operation targeting a cluster with no `status.uuid` yet: fails informatively          | Negative | —                 |
| I-19 | Short name `scops` resolves to the same list as the full kind                         | Positive | —                 |

---

## 3. End-to-End Tests

A live simplyblock cluster with a real control plane and a real data path. Every
row here changes cluster state.

| #    | Scenario                                                                          | Type     | Test |
|------|-----------------------------------------------------------------------------------|----------|------|
| E-01 | Inactive cluster, `action: Activate`: polls to `active`, operation `Succeeded`    | Positive | —    |
| E-02 | Unprovisioned capacity present, `action: Expand`: polls to `active`               | Positive | —    |
| E-03 | `action: Shutdown`: cluster leaves `active`, operation `Succeeded`                | Positive | —    |
| E-04 | `action: Start` on a shut-down cluster: returns to `active`                       | Positive | —    |
| E-05 | `action: Restart`: shutdown then start sequenced, operation `Succeeded`           | Positive | —    |
| E-06 | `action: RollingRestart` on three nodes: each walked in turn                      | Positive | —    |
| E-07 | `RollingRestart` with `refreshSNodeAPI: true`: the new image is running after     | Positive | —    |
| E-08 | Cluster deleted while a pool still has bound volumes                              | Negative | —    |
| E-09 | Transient 5xx from the control plane during polling: retried, operation completes | Negative | —    |
| E-10 | Credentials Secret deleted out of band: restored by the next periodic sync        | Positive | —    |
| E-11 | Adoption of a Helm-deployed cluster through the upgrade Secret                    | Positive | —    |
| E-12 | Single-node cluster: `RollingRestart` completes                                   | Boundary | —    |

---

## 4. Manual Scenarios

### M-01: Operator killed between the write-ahead patch and the backend call

**Design reference:** §6.2.

**What to verify:** that the persisted step does what the removed flag used to,
which no unit test can show because it requires the process to actually die between
two statements.

**Test concept:**

1. Create a `StorageClusterOps` with `action: Shutdown` against an active cluster.
2. Kill the operator pod the moment the shutdown step is persisted, before the
   control plane records a shutdown request.
3. Restart the operator and watch the operation resume.
4. Confirm in the control-plane audit log that exactly one shutdown was requested,
   or none.

**Current behavior:** a crash after the patch and before the call leaves an
operation waiting for something never requested, with no timeout, and recovery is to
delete and recreate the CR. Design §6.2 removes that failure mode: the step is
re-attempted because the target is not yet past it, and the call is a no-op if it
did land.

### M-02: Rolling restart across a peer going offline

**Design reference:** §7.2.

**What to verify:** the safety property of the action, which is that a node is
never shut down while a peer is already offline, because doing so can exceed the
cluster's fault tolerance and lose data. `scopsNodeRollingRestartClusterHealthy`
has no test at any level today.

**Test concept:**

1. Start `action: RollingRestart` on a cluster of at least three nodes.
2. While node 2 is in `restarting`, force node 3 offline out of band.
3. Confirm the walk holds before shutting down node 3's successor, and that
   `status.message` reads `waiting for peer nodes`.
4. Bring node 3 back online and confirm the walk resumes without intervention.
5. Confirm no shutdown was issued while a peer was offline.

**Open question:** the hold has no timeout and emits no event, so a walk stopped
on a degraded cluster is indistinguishable from a stalled controller without
reading `status.message`. Design §3.1.

### M-03: Two reconcilers racing to create one cluster

**Design reference:** §4.2.

**What to verify:** that the optimistic-lock claim prevents two backend clusters.
`TestReconcileCreateOptimisticLockPreventsRace` covers the 409 path against a
fake client, and this scenario covers it against a real API server under real
concurrency.

**Test concept:**

1. Run two operator replicas with leader election disabled.
2. Create a `StorageCluster` and let both reconcile it simultaneously.
3. Confirm exactly one cluster exists in the control plane, by name.
4. Confirm the loser logged a back-off and issued no `POST`.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 67        | 29      | 38          |
| Integration | 19        | 1       | 18          |
| E2E         | 12        | 0       | 12          |
| Manual      | 3         | 0       | 3           |
| **Total**   | **101**   | **30**  | **71**      |

Twenty-nine of the thirty covered scenarios are unit tests, and they concentrate
on the entity's creation and deletion paths and on the operation's lock
mechanics. That is the right place for them, because a defect on those paths
corrupts state instead of failing safely. Every covered scenario outside a unit
test is `I-01`.

Twenty-one distinct test functions cover those thirty scenarios, because a
table-driven test satisfies one ID per subtest.

---

## 6. What Is Not Yet Covered

| #                     | Gap                                                                | Reason                                                                                                                                                                                                                                                                                                                              |
|-----------------------|--------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-05                  | Deleting a CR that never acquired a UUID                           | Path exists in `handleDeletion` and is unexercised                                                                                                                                                                                                                                                                                  |
| U-17 … U-20           | All three adoption routes                                          | `adoptExistingCluster` has no test reference at all, and adoption is the migration path off Helm                                                                                                                                                                                                                                    |
| U-21 … U-24           | Steady-state sync, including the no-change early return            | `syncStatus` has no test reference, and the early return is what keeps an idle cluster from writing                                                                                                                                                                                                                                 |
| U-27 … U-31           | `effectiveConcurrentRestarts` in all five states                   | Pure function computing a safety limit, untested. The clamp is what keeps a drain inside fault tolerance                                                                                                                                                                                                                            |
| U-32 … U-34           | Nil-block defaults in the threshold and stripe helpers             | `stripeDataChunks` returning 1 rather than 0 for a nil block is load-bearing and unasserted                                                                                                                                                                                                                                         |
| U-35                  | Vault URL rejection                                                | `buildHashicorpVaultConfig` has no test reference                                                                                                                                                                                                                                                                                   |
| U-47 … U-49           | Deletion-while-running, late release, and the acquire race         | The 409 acquire path is asserted only for creation, not for the operation lock                                                                                                                                                                                                                                                      |
| U-50 … U-56           | Every action handler, including the whole `Restart` sequence       | `reconcileRestart` has no test reference, and its three passes are driven by a message string (design §6.4)                                                                                                                                                                                                                         |
| U-58 … U-67           | The rolling restart walk past initialization                       | Only initialization is covered. The peer gate, the refresh sub-phases, and node advancement are untested                                                                                                                                                                                                                            |
| I-02 … I-13           | Every immutability and range rule                                  | Needs `envtest`, because CEL and `Required` are enforced by the API server and a fake client applies neither                                                                                                                                                                                                                        |
| I-14 … I-19           | Lock behavior under a real API server                              | Needs `envtest` for real `resourceVersion` conflicts and real watch delivery                                                                                                                                                                                                                                                        |
| E-01 … E-12           | All end-to-end scenarios                                           | Needs a live cluster. The e2e harness under `test/` is not committed yet                                                                                                                                                                                                                                                            |
| E-08                  | Deleting a cluster whose pool has bound volumes                    | The behavior is not decided, let alone tested. `design-crd-model.md` §9.3 and its open question own it                                                                                                                                                                                                                              |
| M-01 … M-03           | Crash-consistency, peer degradation, and the creation race         | Need process kills and out-of-band node failure                                                                                                                                                                                                                                                                                     |
| Operation retention   | Nothing deletes a terminal `StorageClusterOps`                     | Feature does not exist. Design §13, Q2                                                                                                                                                                                                                                                                                              |
| Metrics               | The nine metrics of design §10.2                                   | Designed, not built. Nothing exports a metric for either kind today                                                                                                                                                                                                                                                                 |
| `U-SM-01` … `U-SM-28` | The whole `Ops` shape: typed action, `status.step`, and the graphs | Planned, not built. `atlas-lib/statemachine` has no consumer in either component yet, and design §6.2 is what the write-ahead interaction rests on. `U-SM-24` to `U-SM-26` are what the shared `statemachine.KubeSnapshot` makes necessary, since the step values then live in the graph, in the `Enum` marker, and in the CEL rule |
| `U-CM-01` … `U-CM-12` | The creation path as a declared graph                              | Planned, not built. `status.phase` and `status.subPhase` are untyped strings today, and the graph exists only as prose                                                                                                                                                                                                              |
| `U-CP-01` … `U-CP-12` | Push-driven reconciliation                                         | Planned, not built. The `cpinformer` core exists on the `sse` branch with a Volume pilot, and neither controller here is wired to it                                                                                                                                                                                                |

### Axis coverage

The axes are the ones that actually break this operator. A blank cell is a
combination nothing exercises.

| Axis                      | Value                   | Scenarios                                       |
|---------------------------|-------------------------|-------------------------------------------------|
| Namespace count           | Single namespace        | U-01 … U-67, I-01 … I-19, E-01 … E-12           |
|                           | Multiple namespaces     | — (see below)                                   |
| Cluster node count        | Single node             | U-66, E-12                                      |
|                           | Three nodes             | E-06, M-02                                      |
|                           | Larger than three       | —                                               |
| simplyblock cluster count | Single cluster          | Every scenario except I-17                      |
|                           | Multiple clusters       | I-17                                            |
|                           | Cross-cluster           | — (not applicable: no operation spans clusters) |
| Operation concurrency     | One operation           | U-36 … U-46, E-01 … E-07                        |
|                           | Two, same cluster       | U-39, U-49, I-14, I-15                          |
|                           | Two, different clusters | I-17                                            |

**The multi-namespace row is the significant blank.** `ControlPlane` is a
singleton per namespace, so two namespaces are two independent deployments, and
nothing verifies that a `StorageClusterOps` in one namespace cannot acquire the
lock on a same-named `StorageCluster` in another. The controller resolves
`spec.clusterRef` within `ops.Namespace`, so the isolation is expected to hold,
which is exactly the kind of expectation that deserves one test.

**The larger-than-three-node row matters for the rolling restart.** The walk is
sequential and unbounded in duration, so a sixteen-node cluster spends sixteen
times as long holding the cluster lock as a one-node cluster, and nothing
establishes what that does to a queued operation waiting on the same lock.
