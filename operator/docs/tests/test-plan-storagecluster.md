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
§6 with its reason. A struck-through ID is a scenario the rework removed rather
than left uncovered, and the row says what removed it.

Both kinds now carry the design's shape at `storage.simplyblock.io/v1alpha2`, so
scenario text names one spelling and it is the one in the code: `Activate` rather
than `activate`, `rollingRestart.refreshSNodeAPI` rather than
`nodeRollingRestart.refreshSNodeAPI`, and `status.rollingRestart` rather than
`status.nodeRollingRestartStatus`.

| Class       | Prefix | Harness                                                                |
|-------------|--------|------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, and a mock backend |
| Integration | `I-`   | Full reconcile loop against `envtest` and a mock backend               |
| E2E         | `E-`   | Live simplyblock cluster, real data path                               |
| Manual      | `M-`   | Needs failure injection or orchestration not automated yet             |

---

## 1. Unit Tests

Pure functions and single reconcile calls against a fake client, with the control
plane replaced by the `fakeControlPlane` double in
`operator/internal/controllers/cluster/helpers_test.go`. Every method the double
does not script fails the test, which is what stops a reconciler reaching the
control plane where a row did not expect it to. No Kubernetes API server is
involved.

### Entity Reconciler: Deletion and Finalizer (design §4.5)

File: `operator/internal/controllers/cluster/storagecluster_controller_test.go`

| #    | Scenario                                                                             | Type     | Test                                                       |
|------|--------------------------------------------------------------------------------------|----------|------------------------------------------------------------|
| U-01 | No deletion timestamp: the deletion branch is a pass-through                         | Negative | `TestTheFinalizerIsAddedOnTheFirstReconcile`               |
| U-02 | Backend unreachable while `status.uuid` is set: requeue, finalizer kept              | Negative | `TestARefusedDeleteKeepsTheFinalizer`                      |
| U-03 | Backend `DELETE` succeeds: finalizer removed                                         | Positive | `TestDeletionRemovesBothFinalizerSpellings`                |
| U-04 | Finalizer absent: added on first reconcile                                           | Positive | `TestTheFinalizerIsAddedOnTheFirstReconcile`               |
| U-05 | Deleting a CR that never got a `status.uuid`: finalizer removed with no backend call | Boundary | `TestDeletingAnUncreatedClusterAsksTheControlPlaneNothing` |
| U-68 | An object carrying the older `cluster-finalizer`: both spellings removed             | Boundary | `TestDeletionRemovesBothFinalizerSpellings`                |

`U-68` is the row the finalizer rename creates, and it is the one change in the
migration that wedges rather than degrades: an operator removing only the new key
would leave every object an older one created in `Terminating` forever (design
§4.5).

### Entity Reconciler: Dispatch (design §4.1)

File: `operator/internal/controllers/cluster/storagecluster_controller_test.go`

| #    | Scenario                                                     | Type     | Test                                                       |
|------|--------------------------------------------------------------|----------|------------------------------------------------------------|
| U-06 | Finalizer added on the first reconcile of a new CR           | Positive | `TestTheFinalizerIsAddedOnTheFirstReconcile`               |
| U-07 | `status.uuid` present: reconcile takes the steady-state path | Positive | `TestTheEffectiveRestartLimitIsClampedToTheFaultTolerance` |
| U-08 | CR not found: reconcile returns without requeue              | Negative | `TestReconcilingAClusterThatIsGoneDoesNothing`             |

### Entity Reconciler: Creation (design §4.2)

File: `operator/internal/controllers/cluster/storagecluster_controller_test.go`

| #    | Scenario                                                                   | Type     | Test                                                |
|------|----------------------------------------------------------------------------|----------|-----------------------------------------------------|
| U-09 | Readiness check fails: the machine holds, no `POST` issued                 | Negative | `TestAControlPlaneThatIsNotReadyHoldsTheCreation`   |
| U-10 | Creation `POST` fails and no cluster of that name exists: requeue          | Negative | `TestARefusedCreationCarriesTheControlPlanesAnswer` |
| U-11 | Creation response unparsable: requeue                                      | Negative | —                                                   |
| U-12 | Creation succeeds: status populated and the credentials Secret written     | Positive | `TestACreatedClusterReachesSteadyState`             |
| U-13 | Creation succeeds: `status.erasureCodingScheme` rendered from NDCS/NPCS    | Positive | `TestACreatedClusterReachesSteadyState`             |
| U-14 | Second reconciler at the same `resourceVersion`: 409, backs off, no `POST` | Negative | `TestAStaleReconcilerCannotClaimTheCreationTwice`   |
| U-15 | `status.phase` reaches `Online` and `status.step` clears once created      | Positive | `TestACreatedClusterReachesSteadyState`             |
| U-16 | CSI credentials Secret upserted into the operator namespace                | Positive | `TestACreatedClusterReachesSteadyState`             |
| U-84 | Backup store named but its Secret missing: holds at `ResolvingConfig`      | Negative | `TestAMissingBackupSecretHoldsTheCreation`          |

### Entity Reconciler: Adoption (design §4.3)

File: `operator/internal/controllers/cluster/storagecluster_controller_test.go`

| #    | Scenario                                                                     | Type     | Test                                                  |
|------|------------------------------------------------------------------------------|----------|-------------------------------------------------------|
| U-17 | Upgrade Secret present and readable: cluster adopted without a `POST`        | Positive | `TestAnUpgradeSecretAdoptsRatherThanCreating`         |
| U-18 | Upgrade Secret present but `uuid` or `secret` empty: falls through to create | Negative | `TestAnIncompleteUpgradeSecretFallsThroughToCreation` |
| U-19 | Creation `POST` fails but the cluster exists by name: adopted instead        | Positive | `TestAFailedPostAdoptsAClusterThatAlreadyExists`      |
| U-20 | Adoption writes the per-cluster Secret with a controller reference           | Positive | `TestTheClusterSecretIsOwnedByTheCluster`             |

### Entity Reconciler: Steady-State Sync (design §4.4)

File: `operator/internal/controllers/cluster/storagecluster_controller_test.go`

| #    | Scenario                                                                                  | Type     | Test                                                       |
|------|-------------------------------------------------------------------------------------------|----------|------------------------------------------------------------|
| U-21 | Backend reports the same status, NQN, and rebalancing flag: no patch issued               | Boundary | —                                                          |
| U-22 | Backend status changed: status patched and requeued                                       | Positive | `TestTheEffectiveRestartLimitIsClampedToTheFaultTolerance` |
| U-23 | Backend read fails: requeue, status left untouched                                        | Negative | —                                                          |
| U-24 | Per-cluster Secret missing: sync proceeds without the credentials upsert                  | Boundary | `TestTheTaskWindowHoldsOnlyWhatIsRunning`                  |
| U-69 | `status.phase` follows the control plane's lifecycle string                               | Positive | `TestThePhaseFollowsTheControlPlanesStatus`                |
| U-70 | `status.tasks` holds running and pending only, capped at 20, in the control plane's order | Boundary | `TestTheTaskWindowHoldsOnlyWhatIsRunning`                  |
| U-71 | A task that leaves the window emits `TaskCompleted`                                       | Positive | `TestATaskLeavingTheWindowEmitsAnEvent`                    |
| U-72 | The task read fails: the recorded window is kept rather than emptied                      | Negative | `TestAFailedTaskReadLeavesTheWindowAlone`                  |

`U-72` is the row worth reading twice. An empty `status.tasks` means nothing is
running, and a failed read does not say that, so a control plane that cannot be
asked leaves the last reading in place.

### Spec Derivation Helpers (design §3.1)

File: `operator/internal/controllers/cluster/derivation_test.go`

| #    | Helper                        | Scenario                                                 | Type     | Test                                         |
|------|-------------------------------|----------------------------------------------------------|----------|----------------------------------------------|
| U-25 | `backupConfig`                | Credentials resolved from the referenced Secret          | Positive | `TestTheBackupStoreResolvesItsCredentials`   |
| U-26 | `backupConfig`                | Secret missing a required key                            | Negative | `TestABackupSecretMissingAKeyIsRefused`      |
| U-27 | `effectiveConcurrentRestarts` | `spec` value below fault tolerance: `spec` value wins    | Positive | `TestEffectiveConcurrentRestarts`            |
| U-28 | `effectiveConcurrentRestarts` | `spec` value above fault tolerance: clamped to tolerance | Boundary | `TestEffectiveConcurrentRestarts`            |
| U-29 | `effectiveConcurrentRestarts` | `spec` value equal to fault tolerance: neither clamps    | Boundary | `TestEffectiveConcurrentRestarts`            |
| U-30 | `effectiveConcurrentRestarts` | Both `nil`: defaults to 1                                | Boundary | `TestEffectiveConcurrentRestarts`            |
| U-31 | `effectiveConcurrentRestarts` | Fault tolerance zero or `nil`: no clamp applied          | Boundary | `TestEffectiveConcurrentRestarts`            |
| U-32 | `capacityThreshold`           | `nil` threshold block yields zero                        | Boundary | `TestAnUnstatedThresholdIsZeroOnTheWire`     |
| U-33 | `stripeDataChunks`            | `nil` stripe block yields 1, not zero                    | Boundary | `TestStripeChunksDefaultToOneRatherThanZero` |
| U-34 | `StripeParityChunks`          | `nil` stripe block yields 1, not zero                    | Boundary | `TestStripeChunksDefaultToOneRatherThanZero` |
| U-35 | `vaultConfig`                 | A key store that resolves to a restricted address        | Negative | `TestAVaultURLThatIsNotExternalIsRefused`    |
| U-73 | `vaultConfig`                 | An absent or empty block sends nothing                   | Boundary | `TestAnAbsentKeyStoreSendsNothing`           |
| U-74 | `backupConfig`                | The four removed how-a-copy-is-taken fields are not sent | Positive | `TestTheBackupStoreResolvesItsCredentials`   |

### Operation Reconciler: Lifecycle and Lock (design §6.1, §8)

File: `operator/internal/controllers/cluster/storageclusterops_controller_test.go`

| #        | Scenario                                                                                                                                                                                | Type     | Test                                                            |
|----------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|-----------------------------------------------------------------|
| U-36     | Phase already `Succeeded`: reconcile does no work                                                                                                                                       | Negative | `TestATerminalOperationReleasesALockItStillHolds`               |
| U-37     | Phase already `Failed`: reconcile does no work                                                                                                                                          | Negative | `TestATerminalOperationDoesNotReleaseSomebodyElsesLock`         |
| U-38     | `spec.clusterRef` names no cluster: phase becomes `Failed` with a message                                                                                                               | Negative | `TestAnOperationAgainstNoClusterFails`                          |
| U-39     | Another operation holds `activeOpsRef`: stays `Pending`, ref unchanged                                                                                                                  | Negative | `TestAnOperationWaitsBehindAnotherOnesLock`                     |
| U-40     | Lock free: acquired, phase leaves `Pending`                                                                                                                                             | Positive | `TestAnOperationTakesTheClusterLockAndReleasesItWhenItFinishes` |
| U-41     | A finished operation sets `Succeeded`, `completedAt`, and clears the lock                                                                                                               | Positive | `TestAnOperationTakesTheClusterLockAndReleasesItWhenItFinishes` |
| U-42     | A failed operation sets `Failed`, the message, and clears the lock                                                                                                                      | Positive | `TestAnUnknownStepFailsTheOperationRatherThanRequeuing`         |
| ~~U-43~~ | `failOps` with a `nil` cluster does not panic. The function now reads the cluster itself, so there is no nil to pass                                                                    | —        | —                                                               |
| U-44     | Releasing leaves a lock owned by a different operation alone                                                                                                                            | Boundary | `TestATerminalOperationDoesNotReleaseSomebodyElsesLock`         |
| ~~U-45~~ | `releaseClusterLock` with a `nil` cluster is a no-op. Superseded for the same reason as `U-43`; a target that no longer exists is the case that remains, and it returns without writing | —        | —                                                               |
| U-46     | Unrecognized `spec.action`: phase becomes `Failed` immediately                                                                                                                          | Negative | —                                                               |
| U-47     | Deletion while `Running`: lock released before the finalizer is removed                                                                                                                 | Positive | `TestDeletingARunningOperationReleasesTheLock`                  |
| U-48     | Terminal phase reached but lock still held: released by the later reconcile                                                                                                             | Boundary | `TestATerminalOperationReleasesALockItStillHolds`               |
| U-49     | Two operations pass the free-check together: the loser gets a 409 and requeues                                                                                                          | Boundary | —                                                               |

### Operation Reconciler: Actions (design §6.3, §6.4)

File: `operator/internal/controllers/cluster/storageclusterops_controller_test.go`

| #        | Scenario                                                                                                                          | Type     | Test                                                        |
|----------|-----------------------------------------------------------------------------------------------------------------------------------|----------|-------------------------------------------------------------|
| ~~U-50~~ | `Activate`: `triggered` persisted before the `POST` is issued. Design §6.2 removed the flag, and the persisted step is the record | —        | —                                                           |
| ~~U-51~~ | `Activate` with `triggered` already true: polls without re-posting. Superseded by `U-75`                                          | —        | —                                                           |
| U-52     | `Shutdown` completes on `status != active`, not on `status == active`                                                             | Boundary | `TestAShutdownIssuesOneCallAndWaitsForTheCluster`           |
| U-53     | `Restart` first step: `POST /shutdown` from `ShuttingDown`                                                                        | Positive | `TestARestartShutsDownThenStarts`                           |
| U-54     | `Restart` second step: cluster left `active`, `POST /start` issued                                                                | Positive | `TestARestartShutsDownThenStarts`                           |
| U-55     | `Restart` last step: cluster `active` again, phase becomes `Succeeded`                                                            | Positive | `TestARestartShutsDownThenStarts`                           |
| U-56     | `Restart` while the cluster has not yet left `active`: holds, no `POST /start`                                                    | Boundary | `TestARestartShutsDownThenStarts`                           |
| U-75     | `Activate` against a cluster already `active`: no call issued at all                                                              | Boundary | `TestAnActivateAgainstAnActiveClusterIssuesNoCall`          |
| U-76     | Control plane briefly unreachable: the operation requeues rather than failing                                                     | Negative | `TestAnUnreachableControlPlaneRequeuesRatherThanFailing`    |
| U-77     | `CancelTask`: the cancel is issued once and the operation waits for the task to leave `status.tasks`                              | Positive | `TestCancelTaskWaitsForTheTaskToLeaveTheList`               |
| U-78     | `CancelTask` naming a task already gone: succeeds with no call                                                                    | Boundary | `TestCancelingATaskThatIsAlreadyGoneSucceedsWithoutCalling` |
| U-79     | `CancelTask` with no `spec.cancelTask.taskID`: terminal rather than requeued                                                      | Negative | `TestCancelTaskWithNoTaskIDFails`                           |
| U-95     | `Activate` on a cluster with fewer nodes than its scheme needs: held, `StripeNodesNotReady`, no call                              | Negative | `TestAnActivationBelowTheStripesMinimumIsHeld`              |
| U-96     | The same cluster once the missing node exists: the activation is requested                                                        | Positive | `TestAnActivationWithTheNodesTheStripeNeedsIsRequested`     |
| U-97     | `Activate` on a cluster already `active` and below the minimum: not held, because its layout is already a fact                    | Boundary | `TestAReactivationOfALiveClusterIsNotHeld`                  |
| U-98     | The node minimum itself, scheme by scheme, including the 1+0 that needs one node                                                  | Boundary | `TestTheActivationNodeCountRule`                            |

`U-95` through `U-98` are in
`operator/internal/controllers/cluster/erasurecoding_test.go`, beside the rule
they exercise, and `U-95` is the only place this is refused. The control plane's
own activation gate counts devices, `ndcs+npcs+1` of them, and never nodes, so a
cluster whose fleet is too small for its stripe activates and serves from it
([`design-storagecluster.md`](../designs/crd-redesign/design-storagecluster.md) §3.1).

### Operation Reconciler: Rolling Restart (design §7)

File: `operator/internal/controllers/cluster/rollingrestart_test.go`

| #        | Scenario                                                                                      | Type     | Test                                                     |
|----------|-----------------------------------------------------------------------------------------------|----------|----------------------------------------------------------|
| ~~U-57~~ | First reconcile sets `triggered` and transitions to `Running`. Design §6.2 removed the flag   | —        | —                                                        |
| U-58     | The walk is planned once from the node list, in the order it will restart them                | Positive | `TestARollingRestartWalksEveryNodeAndFinishes`           |
| U-59     | A peer node is not `online`: the walk holds, no shutdown issued, `PeerNodeNotOnline` emitted  | Negative | `TestARollingRestartHoldsWhileAPeerIsOffline`            |
| U-60     | All peers `online`: shutdown issued for the node at `nodeIndex`                               | Positive | `TestARollingRestartWalksEveryNodeAndFinishes`           |
| U-61     | Node already `in_shutdown`, `offline`, or `in_restart`: shutdown call skipped                 | Boundary | `TestAnAlreadyOfflineNodeIsNotShutDownAgain`             |
| U-62     | `refreshSNodeAPI` false: `RefreshingPod` and `AwaitingPod` skipped entirely                   | Boundary | `TestARollingRestartWalksEveryNodeAndFinishes`           |
| U-63     | `refreshSNodeAPI` true: pod deleted, then awaited `Ready` before `RestartingNode`             | Positive | —                                                        |
| U-64     | `Rebalancing` completes: `nodeIndex` increments and the machine resets                        | Positive | `TestARollingRestartWalksEveryNodeAndFinishes`           |
| U-65     | A cluster with no nodes: phase becomes `Succeeded` with nothing shut down                     | Boundary | `TestARollingRestartOfAnEmptyClusterSucceeds`            |
| U-66     | Single-node cluster: the walk has no peers to check and completes                             | Boundary | `TestAnAlreadyOfflineNodeIsNotShutDownAgain`             |
| ~~U-67~~ | `phaseTriggered` already true: the irreversible call is not repeated. Superseded by `U-SM-23` | —        | —                                                        |
| U-80     | The peer hold resolves without intervention once the peer returns                             | Positive | `TestARollingRestartResumesWhenThePeerComesBack`         |
| U-81     | A node added mid-walk is not restarted                                                        | Boundary | `TestANodeAddedMidWalkIsNotRestarted`                    |
| U-82     | A node removed mid-walk is skipped rather than waited on                                      | Boundary | `TestANodeRemovedMidWalkIsSkipped`                       |
| U-83     | `status.message` locates the walk as `Node n/m (uuid): Step`                                  | Positive | `TestTheWalkReportsItsPosition`                          |
| U-85     | The walk reads the storage-node stream's cache rather than the control plane                  | Positive | `TestTheWalkReadsTheNodeStreamRatherThanTheControlPlane` |
| U-86     | An unsynced node cache is not read: the control plane answers until the snapshot lands        | Boundary | `TestAnUnsyncedNodeCacheFallsBackToTheControlPlane`      |
| U-87     | The rebalancing wait reads the cluster stream rather than the node list                       | Positive | `TestTheRebalancingWaitReadsTheClusterStream`            |
| U-88     | No step between a node's shutdown and its restart is abortable                                | Negative | `TestAnAbortIsRefusedWhileTheNodeIsDown`                 |
| U-89     | A step that has taken nothing down is abortable                                               | Positive | `TestAnAbortIsHonoredWhereNothingIsDown`                 |
| U-90     | A conflicted lock release is reported rather than swallowed                                   | Negative | `TestAConflictedReleaseIsReportedRatherThanSwallowed`    |
| U-91     | Adoption by name does not persist an empty credential or mark the cluster configured          | Negative | `TestAdoptionByNameDoesNotPersistAnEmptyCredential`      |
| U-92     | A backup store with no bucket is refused, naming the conversion annotation                    | Negative | `TestABackupStoreWithNoBucketIsRefused`                  |
| U-93     | The creation machine resumes from every step it declares, and from no other                   | Boundary | `TestTheCreationMachineRestoresFromEveryDeclaredStep`    |
| U-94     | A step is never persisted without the deadline that bounds it                                 | Negative | `TestAStepIsNeverPersistedWithoutItsDeadline`            |

### Creation State Machine (design §4.2)

File: `operator/internal/controllers/cluster/storagecluster_controller_test.go`

| #       | Scenario                                                                                            | Type     | Test                                              |
|---------|-----------------------------------------------------------------------------------------------------|----------|---------------------------------------------------|
| U-CM-01 | The creation graph is closed: every edge names a declared step                                      | Positive | `TestACreatedClusterReachesSteadyState`           |
| U-CM-02 | Transition into `Claiming` is persisted with an optimistic lock, and a second reconciler gets a 409 | Negative | `TestAStaleReconcilerCannotClaimTheCreationTwice` |
| U-CM-03 | A 409 on the claim leaves `status.step` untouched and issues no backend call                        | Negative | `TestAStaleReconcilerCannotClaimTheCreationTwice` |
| U-CM-04 | `CheckingControlPlane` to `Adopting` when the upgrade Secret is present and complete                | Positive | `TestAnUpgradeSecretAdoptsRatherThanCreating`     |
| U-CM-05 | `CheckingControlPlane` to `ResolvingConfig` when the upgrade Secret is absent                       | Positive | `TestACreatedClusterReachesSteadyState`           |
| U-CM-06 | `Creating` to `Adopting` when the `POST` fails and the cluster exists by name                       | Positive | `TestAFailedPostAdoptsAClusterThatAlreadyExists`  |
| U-CM-07 | `Creating` to `Persisting` on a successful `POST`                                                   | Positive | `TestACreatedClusterReachesSteadyState`           |
| U-CM-08 | `Claiming` to `Persisting` directly is an `IllegalTransitionError`                                  | Negative | —                                                 |
| U-CM-09 | Restoring at `Creating` runs no entry hook, so the `POST` is not repeated                           | Boundary | —                                                 |
| U-CM-10 | `status.step.deadline` expired on `CheckingControlPlane`: reported rather than requeued forever     | Boundary | —                                                 |
| U-CM-11 | `status.phase` reaches `Online` only once `status.uuid` is persisted                                | Positive | `TestACreatedClusterReachesSteadyState`           |
| U-CM-12 | A converted `subPhase` restores as `step.state` with no deadline                                    | Boundary | `TestStorageClusterSubPhaseReadsIntoTheStep`      |

`U-CM-12`'s test is in `operator/api/v1alpha1/storagecluster_conversion_test.go`,
because the conversion is where the old string becomes the new object.

### Ops Shape: Step Machine (design §5.3)

Files: `operator/internal/controllers/cluster/graphs_test.go` and the two reconciler
test files beside it.

| #       | Scenario                                                                                                            | Type     | Test                                                            |
|---------|---------------------------------------------------------------------------------------------------------------------|----------|-----------------------------------------------------------------|
| U-SM-01 | `MultiConfig` declares a graph for each of the seven actions, and building any machine validates all seven          | Positive | `TestEveryActionDeclaresAGraph`                                 |
| U-SM-02 | A bad edge in the `RollingRestart` graph fails when a machine is built for `Activate`                               | Negative | `TestEveryActionDeclaresAGraph`                                 |
| U-SM-03 | An `Activate` op cannot transition to `Rebalancing`: `IllegalTransitionError`                                       | Negative | `TestAStepOfAnotherActionIsRejected`                            |
| U-SM-04 | An action absent from the `MultiConfig`: `ErrUnknownAction` rather than a stall                                     | Negative | —                                                               |
| U-SM-05 | `spec.action` unrecognized after a downgrade: the operation fails rather than panicking the controller              | Negative | `TestAnUnknownStepFailsTheOperationRatherThanRequeuing`         |
| U-SM-06 | Empty `status.step` restores to the graph's initial state without an entry hook firing                              | Positive | `TestAnOperationTakesTheClusterLockAndReleasesItWhenItFinishes` |
| U-SM-07 | Unrecognized non-empty `status.step` is an error, not a reset to initial                                            | Negative | `TestAnUnknownStepFailsTheOperationRatherThanRequeuing`         |
| U-SM-08 | Restoring runs no entry hook, so a step already acted on does not repeat its call                                   | Boundary | `TestAnActivateAgainstAnActiveClusterIssuesNoCall`              |
| U-SM-09 | `Restart`: `ShuttingDown` to `Starting` is legal, and the reverse is not                                            | Boundary | `TestRestartSequencesAShutdownAndAStart`                        |
| U-SM-10 | `RollingRestart`: `Rebalancing` is terminal, so the graph declares no next-node edge                                | Positive | `TestTheRollingRestartGraphIsAcyclicAndTerminatesAtRebalancing` |
| U-SM-11 | `RollingRestart` with `refreshSNodeAPI` false: `ShuttingDownNode` to `RestartingNode` directly                      | Boundary | `TestShuttingDownANodeMayGoStraightToRestarting`                |
| U-SM-12 | `status.step.deadline` in the past restores as expired, so the operation fails on the first pass                    | Boundary | `TestAStepThatOutlivedItsDeadlineFailsTheOperation`             |
| U-SM-13 | `status.step` with a state but no deadline yields no requeue from `RequeueAfter`                                    | Boundary | —                                                               |
| U-SM-14 | `Aborted` is terminal, and an aborted operation releases the cluster lock                                           | Positive | `TestAnAbortFromAnAbortableStepEndsTheOperation`                |
| U-SM-15 | The phase machine and the step machine are separate, and a step transition does not move the phase                  | Boundary | —                                                               |
| U-SM-16 | A `nodePhase` string converted into `step.state` restores with no deadline rather than an expired one               | Boundary | `TestStorageClusterOpsConvertToRenamesRollingRestart`           |
| U-SM-17 | `Rebalancing` reached: `Reset` returns the machine to `CheckingPeers` and clears the deadline                       | Positive | `TestTheRollingRestartGraphIsAcyclicAndTerminatesAtRebalancing` |
| U-SM-18 | `Reset` runs no entry hook, so the peer check is performed by the next pass rather than by the reset                | Boundary | `TestTheRollingRestartGraphIsAcyclicAndTerminatesAtRebalancing` |
| U-SM-19 | `nodeIndex` increments once per completed node, and `nodes` is never modified after the walk starts                 | Positive | `TestANodeAddedMidWalkIsNotRestarted`                           |
| U-SM-20 | `nodeIndex` reaching `len(nodes)` moves the phase to `Succeeded`                                                    | Positive | `TestARollingRestartWalksEveryNodeAndFinishes`                  |
| U-SM-21 | `nodeIndex` of zero with a non-empty `nodes` is the first node, not an unset walk                                   | Boundary | `TestTheWalkReportsItsPosition`                                 |
| U-SM-22 | The step is persisted before the side effect that step performs                                                     | Positive | —                                                               |
| U-SM-23 | A step recorded whose call never fired: the call is made again and is a no-op because the target is already past it | Boundary | `TestAnAlreadyOfflineNodeIsNotShutDownAgain`                    |
| U-SM-24 | Every state each graph declares appears in the step `Enum` marker                                                   | Positive | `TestTheStepEnumCoversEveryDeclaredState`                       |
| U-SM-25 | Every state each graph declares appears in the `status.step` CEL rule                                               | Positive | `TestTheCELRuleCoversEveryDeclaredState`                        |
| U-SM-26 | The CEL rule names no value the graphs do not declare                                                               | Negative | `TestTheCELRuleCoversEveryDeclaredState`                        |
| U-SM-27 | A stored step belonging to another action: rejected rather than accepted                                            | Negative | `TestAStepOfAnotherActionIsRejected`                            |
| U-SM-28 | A restore that fails: the operation is `Failed` with the error, not requeued                                        | Negative | `TestAnUnknownStepFailsTheOperationRatherThanRequeuing`         |
| U-SM-29 | Every step named abortable is one some graph declares                                                               | Positive | `TestEveryAbortableStepIsDeclared`                              |
| U-SM-30 | An abort from a step with no abort edge is refused and the operation runs on                                        | Negative | `TestAnAbortThatArrivesTooLateIsRefusedAndTheOperationRunsOn`   |
| U-SM-31 | Every declared step has a budget, so its duration is measurable                                                     | Positive | `TestEveryStepHasABudget`                                       |

### Conversion (design §12, `design-property-renames.md` §3)

File: `operator/api/v1alpha1/storagecluster_conversion_test.go`, with the
hub-first direction in `operator/api/v1alpha1/hub_roundtrip_test.go`.

| #       | Scenario                                                                                              | Type     | Test                                                          |
|---------|-------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------------------|
| U-CV-01 | `maxHugePagesSize`, `hashicorpVaultSettings`, and `backup.localEndpoint` reach their new names        | Positive | `TestStorageClusterConvertToRenamesAndRegroups`               |
| U-CV-02 | An absent key store stays absent rather than becoming an empty, immutable block                       | Boundary | `TestStorageClusterConvertToLeavesAnAbsentKMSAbsent`          |
| U-CV-03 | `migrationEnabled` inverts into `disableMigration`, and an unstated value stays unstated              | Boundary | `TestStorageClusterMigrationToggleInverts`                    |
| U-CV-04 | The two block-level switches move up to the spec and back down again                                  | Positive | `TestStorageClusterSwitchesMoveUpAndBackDown`                 |
| U-CV-05 | A spec that states nothing gains no empty blocks                                                      | Boundary | `TestStorageClusterAnEmptySpecGainsNoBlocks`                  |
| U-CV-06 | `MetricsBackend`'s three values recase in both directions                                             | Positive | `TestStorageClusterMetricsBackendConvertsBothWays`            |
| U-CV-07 | The four removed fields survive a round trip through the hub                                          | Positive | `TestStorageClusterRemovedFieldsSurviveTheHub`                |
| U-CV-08 | A threshold beyond `int32` is stashed whole rather than truncated                                     | Boundary | `TestStorageClusterAWideThresholdIsNotTruncated`              |
| U-CV-09 | A threshold that fits costs no annotation                                                             | Boundary | `TestStorageClusterANarrowThresholdIsNotStashed`              |
| U-CV-10 | Realignment stays on for a cluster that never turned it off                                           | Positive | `TestStorageClusterRealignmentStaysOnForAClusterNobodyEdited` |
| U-CV-11 | Realignment turned off stays off, and a hub object's own absence is kept                              | Negative | `TestStorageClusterRealignmentRespectsWhatWasStated`          |
| U-CV-12 | A whole cluster round-trips spoke to hub to spoke                                                     | Positive | `TestStorageClusterRoundTripsThroughTheHub`                   |
| U-CV-13 | A whole cluster round-trips hub to spoke to hub, which is what storage costs today                    | Positive | `TestStorageClusterRoundTripsFromTheHub`                      |
| U-CV-14 | The seven action values recase in both directions                                                     | Positive | `TestStorageClusterOpsActionConvertsBothWays`                 |
| U-CV-15 | The walk's two shapes convert by arithmetic, both directions                                          | Positive | `TestStorageClusterOpsConvertToRenamesRollingRestart`         |
| U-CV-16 | A finished walk reads back with nothing pending                                                       | Boundary | `TestStorageClusterOpsAFinishedWalkHasNothingPending`         |
| U-CV-17 | `Aborted` narrows to `Failed` on the way down and is restored on the way up                           | Boundary | `TestStorageClusterOpsAbortedNarrowsAndIsRestored`            |
| U-CV-18 | `CancelTask` and its parameter block survive being stored as `v1alpha1`                               | Boundary | `TestStorageClusterOpsCancelTaskSurvivesStorage`              |
| U-CV-19 | The legacy lowercase `subPhase` is mapped onto the hub's step rather than copied                      | Positive | `TestStorageClusterTheLegacySubPhaseIsNormalized`             |
| U-CV-20 | A `subPhase` no graph declares is dropped rather than carried through                                 | Negative | `TestStorageClusterAnUndeclaredSubPhaseIsDropped`             |
| U-CV-21 | `spec.deviceClass` defaults to `NVMe` on the way up, since a CRD default does not run on a conversion | Boundary | `TestStorageClusterTheDeviceClassDefaultsOnTheWayUp`          |
| U-CV-22 | A deliberate `LogicalBlock` survives being stored and read back                                       | Positive | `TestStorageClusterADeliberateDeviceClassSurvives`            |
| U-CV-23 | A removed-field annotation is cleared when the field it notes states nothing                          | Negative | `TestStorageClusterARemovedFieldsNoteGoesWhenItsValueDoes`    |
| U-CV-24 | A removed-field annotation is cleared when the block it belongs to is absent                          | Negative | `TestStorageClusterARemovedFieldsNoteGoesWhenItsBlockDoes`    |
| U-CV-25 | The realignment switch reaches the hub unstated and needs no conversion note                          | Negative | `TestStorageClusterRealignmentIsCarriedWithoutANote`          |

`U-CV-10` and `U-CV-11` are the pair `design-property-renames.md` §3.4 asks for,
and `U-CV-25` is what the pair rests on. `disableDataRealignment` inverts the
registered `enabled` and keeps its default, so a cluster that never mentioned
realignment has to reach the hub still not mentioning it: any value written there
is a statement the cluster did not make, and it is what an off default would have
cost. `U-CV-25` asserts both halves without naming the field, because what is
wanted is that nothing is said rather than that a particular word is said.

---

## 2. Integration Tests

Full reconcile loop against a real Kubernetes API server via `envtest`, started
once per package by `apiServer` in
`operator/internal/controllers/cluster/suite_test.go`, with the control plane
still mocked. These cover what a fake client cannot: real admission, real
`resourceVersion` semantics, and real watch delivery.

Every row is written against `v1alpha2`, which is the storage version. A read at
any other version goes through a conversion webhook `envtest` does not run, so a
`v1alpha1` row would fail on an unreachable webhook rather than on what it is
about.

File: `operator/internal/controllers/cluster/cel_validation_test.go`

| #    | Scenario                                                                                                 | Type     | Test                                                                  |
|------|----------------------------------------------------------------------------------------------------------|----------|-----------------------------------------------------------------------|
| I-01 | Reconciling a not-found resource returns no requeue                                                      | Negative | `TestReconcilingAClusterThatIsGoneDoesNothing` (unit)                 |
| I-02 | `spec.stripe` changed after creation: rejected by the CEL rule                                           | Negative | —                                                                     |
| I-03 | `spec.fabricType` omitted at creation, set later: accepted, then frozen                                  | Boundary | —                                                                     |
| I-04 | `spec.enableFailureDomains` omitted at creation, set later: rejected                                     | Boundary | —                                                                     |
| I-05 | `spec.enableNodeAffinity` set at creation, changed later: rejected                                       | Negative | —                                                                     |
| I-06 | `spec.maxSubsystemCount` omitted: creation rejected as `Required`                                        | Negative | —                                                                     |
| I-07 | `spec.maxSubsystemCount` of 9 and of 76: both rejected by the range                                      | Boundary | —                                                                     |
| I-08 | `spec.maxSubsystemCount` of 10 and of 75: both accepted                                                  | Boundary | —                                                                     |
| I-09 | `spec.vcpuCount` of 3 rejected, of 4 accepted                                                            | Boundary | `TestStorageClusterVCPUCountMinimum`                                  |
| I-10 | `spec.maxConcurrentWorkerRestarts` of 0: rejected by the minimum                                         | Boundary | —                                                                     |
| I-11 | `spec.volumeMigrationSettings.dataRealignment.minMoves` of 0: rejected                                   | Boundary | —                                                                     |
| I-12 | `spec.action` on `StorageClusterOps` outside the enum: rejected by admission                             | Negative | —                                                                     |
| I-13 | `spec.clusterRef` changed after creation: rejected as immutable                                          | Negative | —                                                                     |
| I-14 | Two `StorageClusterOps` for one cluster: the second stays `Pending`, then runs                           | Positive | —                                                                     |
| I-15 | Lock released: the queued operation wakes from the cluster watch, not the requeue                        | Positive | —                                                                     |
| I-16 | `kubectl delete` on a `Running` operation: `activeOpsRef` cleared                                        | Positive | —                                                                     |
| I-17 | Operations on two different clusters run concurrently without interference                               | Positive | —                                                                     |
| I-18 | Operation targeting a cluster with no `status.uuid` yet: fails informatively                             | Negative | —                                                                     |
| I-19 | Short name `scops` resolves to the same list as the full kind                                            | Positive | —                                                                     |
| I-20 | `spec.deviceClass` omitted at creation: the stored object reads `NVMe`                                   | Boundary | `TestStorageClusterDeviceClassIsImmutableFromCreation`                |
| I-21 | `spec.deviceClass` created as `LogicalBlock`, changed to `NVMe`: rejected as immutable                   | Negative | `TestStorageClusterDeviceClassIsImmutableFromCreation`                |
| I-22 | `spec.deviceClass` created as `LogicalBlock` and reapplied unchanged: accepted                           | Positive | —                                                                     |
| I-23 | `spec.deviceClass` outside the enum: rejected                                                            | Negative | —                                                                     |
| I-24 | `spec.deviceClass` omitted, then set to `LogicalBlock`: rejected, since the default is already the value | Boundary | `TestStorageClusterDeviceClassIsImmutableFromCreation`                |
| I-25 | `spec.kms` absent at creation, set later: accepted; changed or cleared after: rejected                   | Boundary | `TestStorageClusterKMSIsImmutableOnceSet`                             |
| I-26 | `status.step.state` outside the declared set: rejected by the CEL rule                                   | Negative | `TestStorageClusterStepRejectsAnUnknownValue`                         |
| I-27 | `enableAtomic4kWrites` without `enableChecksumValidation`: rejected                                      | Negative | `TestStorageClusterCELRejectsAtomic4kWritesWithoutChecksumValidation` |
| I-28 | Either checksum field changed or cleared after creation: rejected as immutable                           | Negative | `TestStorageClusterChecksumValidationFieldsAreImmutable`              |
| I-29 | The default pool a cluster is created with picks up the CRD's declared defaults                          | Positive | `TestTheDefaultPoolIsFormattedXFS`                                    |
| I-30 | Each of the seven supported schemes at creation: accepted; 3+1, 8+2, 2+0, 4+0, 1+3, and 16+4: rejected   | Negative | `TestStorageClusterCELAcceptsOnlyTheSupportedErasureCodingSchemes`    |
| I-31 | A stripe stating one half only: read as the control plane's default for the other, and accepted          | Boundary | `TestStorageClusterCELReadsAnUnstatedHalfAsTheDefault`                |

`I-01` is answered by a unit test rather than an integration one: a not-found read
needs no API server to be a not-found read, and the row is kept because the ID is
permanent.

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
| E-13 | `kubectl get scm` on a live cluster returns a capacity reading                    | Positive | —    |

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

**Current behavior:** the step is re-entered, the cluster is read, and the call is
made only if the cluster is not already where the call would put it. `U-75` and
`U-SM-23` prove the skip against a scripted control plane; what this scenario adds
is the crash itself.

### M-02: Rolling restart across a peer going offline

**Design reference:** §7.2.

**What to verify:** the safety property of the action, which is that a node is
never shut down while a peer is already offline, because doing so can exceed the
cluster's fault tolerance and lose data. `U-59` and `U-80` prove the gate and its
release against a scripted fleet; what this scenario adds is a real node failing
mid-walk.

**Test concept:**

1. Start `action: RollingRestart` on a cluster of at least three nodes.
2. While node 2 is in `RestartingNode`, force node 3 offline out of band.
3. Confirm the walk holds before shutting down node 3's successor, that
   `status.message` names the node it is holding before, and that a
   `PeerNodeNotOnline` event is raised.
4. Bring node 3 back online and confirm the walk resumes without intervention.
5. Confirm no shutdown was issued while a peer was offline.

**What the hold costs** is now measurable rather than anecdotal:
`simplyblock_storagecluster_rolling_restart_peer_hold_seconds` records it, and the
step's own deadline is what separates a walk holding on a degraded cluster from a
stalled controller.

### M-03: Two reconcilers racing to create one cluster

**Design reference:** §4.2.

**What to verify:** that the optimistic-lock claim prevents two backend clusters.
`U-14` covers the 409 path against a fake client, and this scenario covers it
against a real API server under real concurrency.

**Test concept:**

1. Run two operator replicas with leader election disabled.
2. Create a `StorageCluster` and let both reconcile it simultaneously.
3. Confirm exactly one cluster exists in the control plane, by name.
4. Confirm the loser logged a back-off and issued no `POST`.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 179       | 163     | 16          |
| Integration | 29        | 10      | 19          |
| E2E         | 13        | 0       | 13          |
| Manual      | 3         | 0       | 3           |
| **Total**   | **224**   | **173** | **51**      |

The unit count excludes the six struck-through rows, which the rework removed
rather than left uncovered, and includes the `U-CM-`, `U-SM-`, `U-CP-`, and
`U-CV-` blocks, none of which is future work any more.

One hundred and twenty distinct test functions cover those scenarios, because a
table-driven test satisfies one ID per subtest and several scenarios are two
assertions of one test. Every name this plan cites exists: a row pointing at a
test that does not is worse than a row pointing at nothing.

What the coverage concentrates on is what corrupts state rather than what fails
safely: the creation claim, the three lock-release paths, the write-ahead skip,
and the rolling restart's peer gate. Everything still uncovered is either an
admission rule the API server enforces (which needs `envtest`), a crash (which
needs a process kill), or a data path (which needs a cluster).

---

## 6. What Is Not Yet Covered

| #                                    | Gap                                                          | Reason                                                                                                                                                                                                                                                     |
|--------------------------------------|--------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-11                                 | An unparsable creation response                              | The control-plane surface parses the response, so a malformed one is an ordinary error on `U-10`'s path. The row is kept because the ID is permanent                                                                                                       |
| U-21, U-23                           | The no-change early return, and a failed cluster read        | `writeStatus` skips the patch on equality and the read failure requeues; neither is asserted, and the early return is what keeps an idle cluster from writing                                                                                              |
| U-46                                 | An unrecognized `spec.action`                                | The `Enum` marker refuses it at admission, so it is reachable only after a downgrade. `U-SM-05` covers the same path through an unrecognized step                                                                                                          |
| U-49                                 | Two operations passing the free-check together               | Needs two stale copies of one object racing, which is `I-14`'s territory: a fake client honors `resourceVersion` but does not schedule the race                                                                                                            |
| U-63                                 | The pod refresh                                              | Needs a Kubernetes Node carrying the control plane's management IP and a storage-node pod scheduled on it, which is fixture-heavy enough to belong in `envtest`                                                                                            |
| U-CM-08 … U-CM-10                    | Illegal creation edges, restore-without-hook, and the expiry | The graph's own guarantees, exercised indirectly by every creation test but not asserted on their own                                                                                                                                                      |
| U-SM-04, U-SM-13, U-SM-15, U-SM-22   | Four machine properties                                      | `U-SM-22` needs a crash and is `M-01`. The other three are `atlas-lib/statemachine`'s own guarantees, tested there                                                                                                                                         |
| I-02 … I-08, I-10 … I-19, I-22, I-23 | The remaining admission and lock rules                       | Needs `envtest` rows that do not exist yet. The apiserver is already started once per package, so each is a test rather than a harness                                                                                                                     |
| E-01 … E-13                          | All end-to-end scenarios                                     | Needs a live cluster. The e2e harness under `test/` does not cover this band                                                                                                                                                                               |
| E-08                                 | Deleting a cluster whose pool has bound volumes              | The behavior is not decided, let alone tested. `design-crd-model.md` §9.3 and its open question own it                                                                                                                                                     |
| M-01 … M-03                          | Crash-consistency, peer degradation, and the creation race   | Need process kills and out-of-band node failure                                                                                                                                                                                                            |
| Operation retention                  | Nothing deletes a terminal `StorageClusterOps`               | Feature does not exist. Design §13, Q2                                                                                                                                                                                                                     |
| `Expand`'s parameters                | What an expansion takes, and whether it implies a rebalance  | Design §13, Q1. The action is served and posts no body, and it is the one action with no state to skip on                                                                                                                                                  |
| `CancelTask`'s endpoint              | The control plane offers no cancel                           | Design §9 and §12.1. The action is served end to end and its one call reports whatever the control plane answers, which today is a refusal                                                                                                                 |
| U-CP-04, U-CP-10, U-CP-12            | Three push-driven rows                                       | `U-CP-04` is `U-21` at the stream rather than at the poll, and is unasserted for the same reason; `U-CP-10` is a Kubernetes object rather than a streamed one and needs the fixtures `U-63` needs; `U-CP-12` is the multi-namespace axis, blank throughout |

### Push-Driven Reconciliation (design §4.4)

All three subscriptions of design §9 are served. The rows below are the
properties of reading them, and they divide between the two new subscriptions'
own tests and the controllers that consume them.

Files: `operator/internal/cpinformer/subscriptions/cluster_test.go`,
`.../task_test.go`, and
`operator/internal/controllers/cluster/streams_test.go`.

| #       | Scenario                                                                                              | Type     | Test                                                   |
|---------|-------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------------|
| U-CP-01 | An `updated` cluster event enqueues exactly one reconcile for the matching `StorageCluster`           | Positive | `TestAClusterUpdateMovesTheCacheAndTriggers`           |
| U-CP-02 | An event for a cluster UUID no CR references enqueues nothing                                         | Negative | `TestAnUnregisteredClusterIsCachedButNotTriggered`     |
| U-CP-03 | Status is written from the streamed DTO without a read of the control plane                           | Positive | `TestTheEntityReadsTheClusterStream`                   |
| U-CP-04 | Streamed state identical to `status`: no patch issued                                                 | Boundary | —                                                      |
| U-CP-05 | A step whose predicate is already satisfied by the first snapshot advances without waiting            | Boundary | `TestAnOperationReadsTheClusterStream`                 |
| U-CP-06 | Coalesced delivery skipping `offline` and `in_restart`: a step waiting for `offline` accepts `online` | Boundary | `TestAnAlreadyOfflineNodeIsNotShutDownAgain`           |
| U-CP-07 | Coalesced delivery skipping past `rebalancing`: the walk advances to the next node                    | Boundary | `TestTheRebalancingWaitReadsTheClusterStream`          |
| U-CP-08 | A reconnect snapshot replaces its scope rather than merging into it                                   | Boundary | `TestAClusterSnapshotReplacesRatherThanMerges`         |
| U-CP-09 | An unsynced scope is not read: the control plane answers until the snapshot lands                     | Boundary | `TestAnUnsyncedClusterCacheFallsBackToTheControlPlane` |
| U-CP-10 | `AwaitingPod` still uses pod readiness rather than a stream, because a pod is a Kubernetes object     | Boundary | —                                                      |
| U-CP-11 | Cluster creation still probes `/_meta/ready` directly, because no stream carries readiness            | Boundary | `TestAControlPlaneThatIsNotReadyHoldsTheCreation`      |
| U-CP-12 | Two `StorageCluster` CRs in different namespaces served by the one root-scoped subscription           | Boundary | —                                                      |
| U-CP-13 | `status.tasks` is filled from the task stream rather than from a read per pass                        | Positive | `TestTheTaskWindowIsBuiltFromTheTaskStream`            |
| U-CP-14 | The cluster stream takes no path parameter and its scope is empty                                     | Positive | `TestTheClusterStreamIsRootScoped`                     |
| U-CP-15 | A cluster snapshot caches every cluster and marks the one scope synced                                | Positive | `TestAClusterSnapshotCachesAndSyncs`                   |
| U-CP-16 | Unregistering a cluster stops the subscription naming its object                                      | Negative | `TestUnregisteringAClusterStopsItsTriggers`            |
| U-CP-17 | The task stream is scoped per cluster                                                                 | Positive | `TestTheTaskStreamIsScopedPerCluster`                  |
| U-CP-18 | A task snapshot caches every task and marks the cluster's scope synced                                | Positive | `TestATaskSnapshotCachesAndSyncs`                      |
| U-CP-19 | Only `done` and a canceled flag finish a task; `suspended` is one that is waiting                     | Boundary | `TestOnlyDoneAndCanceledTasksAreFinished`              |
| U-CP-20 | A task reaching a terminal status still triggers, because leaving the window is a change              | Positive | `TestATaskReachingDoneStillTriggers`                   |
| U-CP-21 | A task of an unadopted cluster is cached and not triggered                                            | Negative | `TestTasksOfAnUnregisteredClusterAreNotTriggered`      |
| U-CP-22 | A reconnect snapshot drops a task that ended while the stream was down                                | Boundary | `TestATaskSnapshotReplacesRatherThanMerges`            |
| U-CP-23 | A `CancelTask` reads the task stream, and does not finish on a suspended task                         | Boundary | `TestCancelTaskReadsTheTaskStream`                     |
| U-CP-24 | An unsynced task cache is not read as "no task is running"                                            | Boundary | `TestAnUnsyncedTaskCacheFallsBackToTheControlPlane`    |

`U-CP-09` and `U-CP-24` are the rows with teeth. An unsynced cache is empty,
and empty is not an answer: read as one it reports a live cluster gone, a
shutdown complete, and every `CancelTask` finished the moment it is issued,
because that action's completion condition is the task's absence.

### Axis coverage

The axes are the ones that actually break this operator. A blank cell is a
combination nothing exercises.

| Axis                      | Value                   | Scenarios                                       |
|---------------------------|-------------------------|-------------------------------------------------|
| Namespace count           | Single namespace        | Every scenario                                  |
|                           | Multiple namespaces     | — (see below)                                   |
| Cluster node count        | Zero nodes              | U-65                                            |
|                           | Single node             | U-66, U-81, E-12                                |
|                           | Two nodes               | U-58 … U-64, U-80, U-82                         |
|                           | Three nodes             | E-06, M-02                                      |
|                           | Larger than three       | —                                               |
| simplyblock cluster count | Single cluster          | Every scenario except I-17                      |
|                           | Multiple clusters       | I-17                                            |
|                           | Cross-cluster           | — (not applicable: no operation spans clusters) |
| Operation concurrency     | One operation           | U-36 … U-48, E-01 … E-07                        |
|                           | Two, same cluster       | U-39, U-44, U-49, I-14, I-15                    |
|                           | Two, different clusters | I-17                                            |

**The multi-namespace row is the significant blank.** `ControlPlane` is a
singleton per namespace, so two namespaces are two independent deployments, and
nothing verifies that a `StorageClusterOps` in one namespace cannot acquire the
lock on a same-named `StorageCluster` in another. The controller resolves
`spec.clusterRef` within `ops.Namespace`, so the isolation is expected to hold,
which is exactly the kind of expectation that deserves one test.

**The larger-than-three-node row matters for the rolling restart.** The walk is
sequential and unbounded in duration, so a sixteen-node cluster spends sixteen
times as long holding the cluster lock as a one-node cluster.
`simplyblock_storagecluster_operation_lock_wait_seconds` now measures what that
does to a queued operation, which turns the question from an argument into a
reading, but nothing yet exercises the size.
