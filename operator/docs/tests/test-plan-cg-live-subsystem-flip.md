# Test Plan: Live Consistency-Group Subsystem Flip

Related design: [`designs/design-cg-live-subsystem-flip.md`](../designs/design-cg-live-subsystem-flip.md)
Harness: [`operator/internal/controllers/volume`](../../internal/controllers/volume), [`csi-driver/internal/csi`](../../../csi-driver/internal/csi), [`atlas-lib/volstack`](../../../atlas-lib/volstack), and [`atlas-lib/devmapper`](../../../atlas-lib/devmapper)

Scope: the operator, the CSI driver, `atlas-lib`, and the Kubernetes surface of
this repository. The control plane (`sbcli`), SPDK, and the host kernel are
dependencies, faked at the boundary: a mock control plane answers the
subsystem-move endpoints of design §9, a fake command runner stands in for
`dmsetup`, `nvme`, and LVM, a sysfs fixture stands in for the kernel's NVMe view,
and fake link peers stand in for CSI nodes. The selection rule of design §5 is
the control plane's, and is tested here only as the decisions the mock returns.

Scenario IDs are permanent: `U-` unit (no cluster: pure functions, fake
`client.Client`, mock HTTP, fake command runner), `I-` integration (the full
reconcile loop against `envtest`, a mock control plane, and fake link peers),
`E-` end-to-end (live cluster, real data path), `M-` manual (needs failure
injection or orchestration not yet automated). Types are `Positive`,
`Negative`, `Boundary`, and `Regression`. The `Test` column names the
implementing function, or `—` when the scenario is not yet covered. Every `—`
also appears in §8.

---

## 1. Unit Tests

Every row runs without a cluster. Numbering runs continuously across the groups
below.

### Indirect Plans for Every Shape (§6.1)

Files: `csi-driver/internal/csi/node/indirection_test.go`, `atlas-lib/volstack/plans/plans_test.go`

| #    | Scenario                                                                                                                                                                             | Type     | Test                                                    |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------------|
| U-01 | `planFor` builds `fabric` → `dmLinear` for raw block and `fabric` → `dmLinear` → `filesystem` for plain                                                                              | Positive | `TestPlanForBuildsTheIndirectRows`                      |
| U-02 | `planFor` builds `fabric` → `dmLinear` → `lvmPhysicalVolume` → `lvmVolumeGroup` → `lvmLogicalVolume` → `filesystem` for a VDO volume, and the raw-block VDO row without `filesystem` | Positive | `TestPlanForBuildsTheIndirectLVMRows`                   |
| U-03 | The VDO parameters (`client_compression`, `client_deduplication`, pool name) reach `lvmLogicalVolume` unchanged in the indirect LVM row                                              | Positive | `TestIndirectLVMCarriesTheVDOParametersUnchanged`       |
| U-04 | Every LVM invocation of an indirect VDO stack is scoped with `--devices /dev/mapper/sb-<lvol>`, and never names the `nvmeXnY` path                                                   | Negative | `TestIndirectLVMScopesLVMToTheMappingNeverTheNamespace` |
| U-05 | A striped volume gets no indirect form, with the flag on                                                                                                                             | Negative | —                                                       |
| U-06 | A fresh stage takes the indirect form with `SPDKCSI_DM_INDIRECTION` unset, which is the new default                                                                                  | Positive | `TestAFreshStageOfEveryShapeIsIndirectByDefault`        |
| U-07 | `SPDKCSI_DM_INDIRECTION=false` stages a fresh volume of every shape without `dmLinear`                                                                                               | Negative | `TestTurningTheIndirectionOffStagesEveryShapeDirect`    |
| U-08 | A staged volume follows its stack record: a record without `dmLinear` stays direct with the flag on, and a record with it stays indirect with the flag off                           | Boundary | `TestAttachShapeFollowsTheFlagOnlyForAFreshStage`       |
| U-09 | Teardown of an indirect VDO volume releases the mapping after the volume group, and leaves the data                                                                                  | Positive | `TestTeardownPlanOfAnIndirectVDOVolumeWalksTheMapping`  |
| U-10 | Teardown of an indirect plain volume walks the mapping                                                                                                                               | Positive | `TestTeardownPlanOfAnIndirectVolumeWalksTheMapping`     |

### The dmLinear Layer and Device-Mapper (§6.3, §6.4)

Files: `atlas-lib/volstack/layers/dmlinear_test.go`, `atlas-lib/devmapper/devmapper_test.go`, `atlas-lib/volstack/layers/contract_test.go`

| #    | Scenario                                                                                                             | Type     | Test                                                         |
|------|----------------------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------------------|
| U-11 | `Ensure` maps the whole device below and exposes `/dev/mapper/sb-<lvol>`                                             | Positive | `TestDMLinearEnsureMapsTheDeviceBelowAndExposesTheMapping`   |
| U-12 | `Heal` re-points a mapping whose namespace moved                                                                     | Positive | `TestDMLinearHealRepointsAMappingWhoseNamespaceMoved`        |
| U-13 | `Heal` with nothing below fails and leaves the mapping in place                                                      | Negative | `TestDMLinearHealWithNothingBelowFailsAndLeavesTheMapping`   |
| U-14 | A failed swap keeps the layer unhealthy                                                                              | Negative | `TestDMLinearAFailedSwapKeepsTheLayerUnhealthy`              |
| U-15 | `Release` removes the mapping and `Destroy` removes nothing                                                          | Positive | `TestDMLinearReleaseRemovesTheMappingAndDestroyKeepsNothing` |
| U-16 | `dmLinear` satisfies the layer contract                                                                              | Positive | `TestLayerContract`                                          |
| U-17 | The heal path's `Swap` suspends, reloads, and resumes in that order                                                  | Positive | `TestSwapSuspendsReloadsAndResumes`                          |
| U-18 | The heal path's failed reload resumes on the old table                                                               | Negative | `TestAFailedReloadResumesOnTheOldTable`                      |
| U-19 | The swap service's suspend runs `dmsetup suspend --nolockfs` and never `--noflush`                                   | Positive | `TestSuspendDrainsInFlightIOWithoutFreezingTheFilesystem`    |
| U-20 | The swap service's reload-and-resume, after a refused reload, leaves the mapping suspended and returns the error     | Negative | `TestARefusedReloadLeavesTheMappingSuspended`                |
| U-21 | A table that is not one whole-device linear segment is refused, so a swap never rewrites a mapping it did not create | Negative | `TestTableRefusesAMappingThatIsNotOneLinearSegment`          |

### The Swap Service (§6.2, §6.4)

File: `atlas-lib/volstack/swaprpc/server_test.go`

| #    | Scenario                                                                                                                                               | Type     | Test                                                                       |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------|----------|----------------------------------------------------------------------------|
| U-22 | `Describe` reports a staged indirect volume as swappable, with the NQN and namespace UUID it points at                                                 | Positive | `TestDescribeReportsAStagedIndirectVolumeAsSwappableAndWhereItPoints`      |
| U-23 | `Describe` reports a direct volume as staged and not swappable, and an unknown handle as not staged                                                    | Negative | `TestDescribeReportsADirectVolumeAsNotSwappableAndAnUnknownOneAsNotStaged` |
| U-24 | `Prepare` connects the target subsystem's paths from the entries given, and a repeat connects nothing new                                              | Positive | `TestPrepareConnectsTheTargetPathsInOrderWithTheNodesIdentity`             |
| U-25 | `Prepare` with an entry the host cannot reach returns the error and leaves the source paths untouched                                                  | Negative | `TestPrepareThatCannotReachTheTargetFailsAndLeavesTheSourceAlone`          |
| U-26 | `Suspend` suspends every named mapping and starts the watchdog at the deadline plus 5 seconds                                                          | Positive | `TestSuspendHoldsEveryNamedMappingAndStartsTheWatchdogAfterTheGrace`       |
| U-27 | `Suspend` repeated with the same deadline is a no-op: one `dmsetup suspend` per mapping                                                                | Boundary | `TestSuspendRepeatedWithTheSameDeadlineSuspendsOnce`                       |
| U-28 | `Suspend` with a deadline already in the past is refused and suspends nothing                                                                          | Boundary | `TestSuspendWithADeadlineAlreadyPassedSuspendsNothing`                     |
| U-29 | `Suspend` of a handle that is not swappable is refused before any mapping is suspended                                                                 | Negative | `TestSuspendOfAVolumeThatCannotSwapSuspendsNoneOfThem`                     |
| U-30 | `Resume` finds the namespace by UUID under the given NQN, reloads with its `major:minor`, resumes, and rewrites the `fabric` record's `nqn` and `nsid` | Positive | `TestResumeRepointsTheMappingAtTheTargetAndRecordsTheNewSubsystem`         |
| U-31 | `Resume` with the namespace absent issues `nvme ns-rescan` on that controller once, then finds it                                                      | Positive | `TestResumeRescansOnceWhenTheNamespaceHasNotAppeared`                      |
| U-32 | `Resume` where the namespace never appears returns an error at the deadline and leaves the mapping suspended                                           | Negative | `TestResumeWhoseNamespaceNeverAppearsFailsAtTheDeadlineAndStaysSuspended`  |
| U-33 | `Resume` with the source NQN re-points a mapping onto its source device, the rollback case                                                             | Positive | `TestResumeOnTheSourceIsTheRollback`                                       |
| U-34 | `Resume` repeated on a mapping already resumed on the right device runs no `dmsetup` command                                                           | Boundary | `TestResumeOfAMappingAlreadyResumedOnTheRightDeviceChangesNothing`         |
| U-35 | `Resume` finds a namespace whose UUID matches under another NQN and ignores it                                                                         | Negative | `TestResumeIgnoresTheSameNamespaceUnderAnotherSubsystem`                   |
| U-36 | `Release` disconnects a subsystem no staged volume points at                                                                                           | Positive | `TestReleaseDisconnectsASubsystemNoStagedVolumePointsAt`                   |
| U-37 | `Release` keeps a subsystem another staged volume on the host still points at                                                                          | Negative | `TestReleaseKeepsASubsystemAStagedVolumeStillPointsAt`                     |
| U-38 | The node registers `SwapService` on its link session and advertises `volume-swap` in its hello                                                         | Positive | `TestTheServiceAdvertisesTheVolumeSwapCapability`                          |

### Watchdog and Restart Recovery (§6.5, §6.6)

File: `atlas-lib/volstack/swaprpc/watchdog_test.go`

| #    | Scenario                                                                                                  | Type     | Test                                                                 |
|------|-----------------------------------------------------------------------------------------------------------|----------|----------------------------------------------------------------------|
| U-39 | The watchdog finds the namespace under the target NQN and resumes on it                                   | Positive | `TestTheWatchdogResumesOnTheTargetWhenTheNamespaceMoved`             |
| U-40 | The watchdog finds it only under the source NQN and resumes on it                                         | Positive | `TestTheWatchdogResumesOnTheSourceWhenTheNamespaceNeverLeft`         |
| U-41 | The watchdog finds it under neither, resumes on the current table, and emits `SubsystemSwapWatchdogFired` | Negative | `TestTheWatchdogResumesOnTheCurrentTableWhenTheNamespaceIsNowhere`   |
| U-42 | The watchdog does not fire before the deadline plus grace, and fires at it                                | Boundary | `TestSuspendHoldsEveryNamedMappingAndStartsTheWatchdogAfterTheGrace` |
| U-43 | A `Resume` that lands before the deadline stops the watchdog                                              | Negative | `TestAResumeBeforeTheDeadlineStopsTheWatchdog`                       |
| U-44 | The startup scan resolves every suspended `sb-*` mapping as the watchdog would                            | Positive | `TestTheStartupScanResolvesEverySuspendedMappingAsTheWatchdogWould`  |
| U-45 | The startup scan leaves a suspended mapping it does not own (not `sb-*`) alone                            | Negative | `TestTheStartupScanLeavesAMappingItDoesNotOwnAlone`                  |
| U-46 | The startup scan with no suspended mapping runs no `dmsetup` mutation                                     | Boundary | `TestTheStartupScanWithNothingSuspendedChangesNothing`               |

### Membership Watcher (§8.1)

File: `csi-driver/internal/csi/controller/cg_membership_watcher_test.go`

| #    | Scenario                                                                                                                                                                                                       | Type     | Test                                                       |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------------------------------------------------------------|
| U-47 | A label added to a bound PVC whose volume belongs to no group creates `PersistentVolumeOps` `cg-join-<lvol>` with action `JoinConsistencyGroup` and the label's group, and emits `ConsistencyGroupJoinStarted` | Positive | `TestALateLabelOnAVolumeInNoGroupRequestsTheJoinOperation` |
| U-48 | A member already in the group's subsystem gets no operation                                                                                                                                                    | Negative | `TestConvergedMembershipIsANoOp`                           |
| U-49 | A volume provisioned with the label is a member before the watcher sees it and gets no operation, also when it was placed outside the group's subsystem                                                        | Negative | `TestAVolumeAlreadyInAGroupGetsNoJoinOperation`            |
| U-50 | A failed operation is not recreated, and the next pass reports its phase only                                                                                                                                  | Negative | `TestAnExistingJoinRequestIsNeverRewritten`                |
| U-51 | The operation's phase is reported as an event on each pass, not as an error                                                                                                                                    | Positive | `TestEachPassReportsTheJoinOperationsPhase`                |
| U-52 | Two PVCs in two namespaces labeled for one group create two operations with distinct names                                                                                                                     | Positive | `TestTwoClaimsInTwoNamespacesGetTwoOperations`             |
| U-53 | With `SPDKCSI_CG_JOIN_OPERATION=false`, an off-pin join requests the pre-join `VolumeMigration` as today                                                                                                       | Negative | `TestOffPinJoinRequestsThePreJoinMigration`                |
| U-54 | Label removal detaches without creating an operation                                                                                                                                                           | Negative | `TestLabelRemovalDetachesFromALabelManagedGroup`           |
| U-55 | A conflicting label is surfaced and creates no operation                                                                                                                                                       | Negative | `TestConflictingLabelIsSurfacedNotActedOn`                 |
| U-56 | A foreign driver's volume is ignored                                                                                                                                                                           | Negative | `TestForeignDriverVolumesAreIgnored`                       |
| U-57 | A PVC that is not bound creates no operation                                                                                                                                                                   | Negative | `TestAnUnboundClaimGetsNoJoinOperation`                    |

### Planning and Migration Steps (§5.5, §7)

File: `operator/internal/controllers/volume/join_test.go`

| #    | Scenario                                                                                                                             | Type     | Test                                                          |
|------|--------------------------------------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------------------|
| U-58 | A group name that resolves to nothing holds the operation `Pending` with `ConsistencyGroupPending`, and requeues                     | Negative | `TestAGroupNameThatResolvesToNothingHoldsTheOperationPending` |
| U-59 | A plan refused for another group, another pool, or a sibling in another group fails the operation with `ConsistencyGroupJoinRefused` | Negative | `TestARefusedPlanFailsTheJoin`                                |
| U-60 | A preflight refused for capacity fails with `SubsystemLimitReached`, and creates no child migration (child count zero)               | Negative | `TestAPreflightRefusedForCapacityFailsBeforeAnyMigration`     |
| U-61 | An off-pin plan creates one child `Migrate` operation targeting the pinned node, with `creatorRef` naming the parent and its UID     | Positive | `TestAnOffPinPlanCreatesOneChildMigrationNamingItsCreator`    |
| U-62 | An on-pin plan skips `Migrating` and creates no child                                                                                | Boundary | `TestAnOnPinPlanSkipsTheMigration`                            |
| U-63 | A child that fails or aborts fails the parent with `ConsistencyGroupMigrationFailed`                                                 | Negative | `TestAFailedChildMigrationFailsTheJoin`                       |
| U-64 | An abort during `Migrating` sets `abort` on the child                                                                                | Positive | `TestAnAbortDuringMigratingAbortsTheChild`                    |
| U-65 | `Joining` calls the members endpoint once per pass, and a 409 fails the operation                                                    | Negative | `TestJoiningCallsTheMembersEndpointOnce`                      |

### Selection and Target Steps (§5, §7)

File: `operator/internal/controllers/volume/join_test.go`

| #    | Scenario                                                                                                        | Type     | Test                                                              |
|------|-----------------------------------------------------------------------------------------------------------------|----------|-------------------------------------------------------------------|
| U-66 | `SelectingSubsystem` sends the operation's UID as `request_id`, and a repeat after a restart sends the same one | Positive | `TestTheMoveIsCreatedWithTheOperationsUIDAsItsRequestID`          |
| U-67 | A move with an empty moving set succeeds with `ConsistencyGroupColocated` and calls no host                     | Boundary | `TestAMoveWithNothingToMoveSucceedsWithoutCallingAnyNode`         |
| U-68 | A move whose moving set is the joining volume alone records one volume, and its source NQN                      | Positive | `TestAFlipSuspendsEveryHostBeforeMovingAndResumesThemOnTheTarget` |
| U-69 | A whole-group move records every moving volume, and the target as new or existing as the mock returned it       | Positive | `TestAWholeGroupMoveRecordsEveryVolumeAndTheTarget`               |
| U-70 | A selection 409 for capacity fails with `SubsystemLimitReached`, keeps `joined: true`, and calls no host        | Negative | `TestASelectionRefusedForCapacityFailsAndKeepsTheMembership`      |
| U-71 | A 5xx from `prepare` is retried with backoff and calls no host                                                  | Negative | `TestAPrepareFailureIsRetriedWithoutTouchingANode`                |

### Coordinated Swap Steps (§6, §7)

File: `operator/internal/controllers/volume/join_swap_test.go`

| #    | Scenario                                                                                                            | Type                                                                | Test                                                              |
|------|---------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------|-------------------------------------------------------------------|
| U-72 | The consuming hosts are the nodes of the moving volumes' `VolumeAttachment` objects                                 | Positive                                                            | `TestTheConsumingHostsAreTheNodesOfTheVolumeAttachments`          |
| U-05 | A striped volume gets no indirect form                                                                              | The CSI node stages no striped shape, so there is nothing to refuse |                                                                   |
| U-73 | A connected host NQN with no matching node fails `AttachingPaths` with nothing suspended                            | Negative                                                            | —                                                                 |
| U-74 | A host with no link peer, or without `volume-swap`, fails with `SubsystemSwapUnavailable` naming the host           | Negative                                                            | `TestAHostWithoutTheSwapCapabilityFailsBeforeAnythingIsSuspended` |
| U-75 | A host reporting a moving volume as not swappable fails with `SubsystemSwapUnavailable` naming the PVC              | Negative                                                            | `TestAVolumeThatCannotSwapFailsNamingItsClaim`                    |
| U-76 | A moving set with no attached volume skips the host steps and calls `move` directly                                 | Boundary                                                            | `TestAMoveWithNoAttachedVolumeSkipsTheNodes`                      |
| U-77 | `suspendDeadline` is persisted before the first `Suspend`, and equals the step's start plus `suspendTimeoutSeconds` | Positive                                                            | `TestTheSuspendDeadlineIsPersistedBeforeTheFirstSuspend`          |
| U-78 | One host failing `Suspend` sends the operation to rollback, and the mock sees no `move` call                        | Negative                                                            | `TestOneHostFailingToSuspendRollsBackWithoutMoving`               |
| U-79 | Before `move`, a host whose instance UID changed sends the operation to rollback without a `move` call              | Negative                                                            | `TestAHostThatRestartedBeforeTheMoveRollsBack`                    |
| U-80 | Before `move`, a host reporting a volume no longer suspended sends the operation to rollback                        | Negative                                                            | `TestAHostNoLongerSuspendedBeforeTheMoveRollsBack`                |
| U-81 | `move` carries the persisted deadline, and a 409 for a passed deadline sends the operation to rollback              | Negative                                                            | `TestAMoveRefusedForItsDeadlineRollsBack`                         |
| U-82 | A partial `move` answered with `namespaces_at: source` records `Source` and resumes every host on the source NQN    | Negative                                                            | `TestAPartialMoveResumesEveryHostOnTheSource`                     |
| U-83 | A successful `move` records `Target` and resumes every host on the target NQN                                       | Positive                                                            | `TestAFlipSuspendsEveryHostBeforeMovingAndResumesThemOnTheTarget` |
| U-84 | A host that cannot find the namespace by the deadline triggers `rollback` and a resume on the source                | Negative                                                            | `TestAHostThatCannotFindTheNamespaceRollsBackToTheSource`         |
| U-85 | A 5xx from `commit` is retried, and no host is suspended again                                                      | Negative                                                            | `TestACommitFailureIsRetriedWithoutSuspendingAgain`               |
| U-86 | `ReleasingPaths` releases the source NQN after a commit and the target NQN after a rollback                         | Positive                                                            | `TestOneHostFailingToSuspendRollsBackWithoutMoving`               |
| U-87 | Success emits `SubsystemSwapCompleted` on each swapped PVC and `ConsistencyGroupColocated` on the joining PVC       | Positive                                                            | `TestAFlipSuspendsEveryHostBeforeMovingAndResumesThemOnTheTarget` |
| U-88 | Rollback emits `SubsystemSwapRolledBack` on each swapped PVC and fails the operation                                | Negative                                                            | `TestOneHostFailingToSuspendRollsBackWithoutMoving`               |

### Restart, Abort, Deletion, and Locks (§7, §8.3)

File: `operator/internal/controllers/volume/join_restart_test.go`

| #    | Scenario                                                                                                                                           | Type     | Test                                                        |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------|----------|-------------------------------------------------------------|
| U-89 | A reconciler restarted in each step resumes at the persisted step, and no endpoint or host call is issued twice for a step that completed          | Positive | `TestARestartedReconcilerRepeatsNoCompletedCall`            |
| U-90 | `abort` is honored from `Planning`, `SelectingSubsystem`, `PreparingTarget`, and `AttachingPaths`, and ends `Aborted` with prepared paths released | Positive | `TestAnAbortBeforeTheSuspendRollsTheMoveBackAndEndsAborted` |
| U-91 | `abort` set during `Suspending`, `MovingNamespaces`, or `Resuming` is ignored until the operation ends                                             | Negative | `TestAnAbortDuringTheWindowIsNotHonored`                    |
| U-92 | Deleting the operation with `namespacesAt: Target` commits before the finalizer is removed                                                         | Positive | `TestDeletingAJoinWhoseNamespacesMovedCommits`              |
| U-93 | Deleting the operation with `namespacesAt` unset or `Source` rolls back before the finalizer is removed                                            | Positive | `TestDeletingAJoinBeforeItsNamespacesMovedRollsBack`        |
| U-94 | The lock is taken on every PV in every source subsystem and the target subsystem, or on none                                                       | Positive | `TestTheLockCoversEverySourceAndTargetVolume`               |
| U-95 | One PV held by another operation leaves this one `Pending` with `deferredSince` and `SubsystemSwapDeferred`, holding no lock                       | Negative | `TestAVolumeHeldByAnotherOperationDefersTheSwap`            |
| U-96 | Every terminal path, failure and abort included, releases every lock this operation holds and none it does not                                     | Positive | `TestEveryTerminalPathReleasesOnlyItsOwnLocks`              |
| U-97 | A terminal operation re-reconciled issues no call                                                                                                  | Boundary | `TestATerminalJoinIssuesNoCall`                             |
| U-98 | A stale status whose `observedGeneration` lags the object is not acted on                                                                          | Negative | —                                                           |

### API and Admission (Appendix A)

Files: `operator/internal/controllers/volume/cel_validation_test.go`, and `operator/internal/controllers/driver/rbac_test.go` for U-104

| #     | Scenario                                                                                             | Type     | Test                                                    |
|-------|------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------------|
| U-99  | Action `JoinConsistencyGroup` without `joinConsistencyGroup`, or another action with it, is rejected | Negative | `TestJoinConsistencyGroupCELPairsTheActionWithItsBlock` |
| U-100 | `groupName` empty or longer than 63 characters is rejected, and 63 is accepted                       | Boundary | `TestJoinConsistencyGroupCELBoundsTheGroupName`         |
| U-101 | `suspendTimeoutSeconds` 0 and 31 are rejected, 1 and 30 are accepted, and unset defaults to 10       | Boundary | `TestJoinConsistencyGroupCELBoundsTheSuspendTimeout`    |
| U-102 | Editing `groupName` or `suspendTimeoutSeconds` after creation is rejected                            | Negative | `TestJoinConsistencyGroupCELFreezesTheRequest`          |
| U-103 | A `status.step.state` outside the extended step list is rejected                                     | Negative | `TestJoinConsistencyGroupCELAcceptsItsSteps`            |
| U-104 | The CSI controller's role grants `get`, `list`, `watch`, and `create` on `persistentvolumeops`       | Positive | `TestRulesMatchTheChart`                                |

### Coverage Added During Implementation (§6.2, §6.5, §8.1, §10)

Files: `operator/internal/webhook/persistentvolumeops_validator_test.go`, `csi-driver/internal/csi/controller/cg_join_operation_test.go`, `atlas-lib/volstack/swaprpc/swaprpc_test.go`, `atlas-lib/volstack/swaprpc/rescan_test.go`, `csi-driver/internal/csi/node/swap_test.go`

| #     | Scenario                                                                                              | Type     | Test                                                         |
|-------|-------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------------------|
| U-105 | The webhook refuses a `groupName` that is not a valid label value                                     | Negative | `TestPersistentVolumeOpsJoinChecksTheGroupNameAsALabelValue` |
| U-106 | `SPDKCSI_CG_JOIN_OPERATION` is on unless set to an off value                                          | Boundary | `TestTheJoinOperationIsOnUnlessTurnedOff`                    |
| U-107 | A full describe, prepare, suspend, resume, and release cycle works through the gRPC client and server | Positive | `TestAFullSwapRoundTripsOverTheWire`                         |
| U-108 | Not-swappable, deadline-passed, and namespace-not-found refusals keep their meaning across the wire   | Negative | `TestRefusalsKeepTheirMeaningAcrossTheWire`                  |
| U-109 | A rescan asks every live controller of the subsystem                                                  | Positive | `TestRescanAsksEveryLiveControllerOfTheSubsystem`            |
| U-110 | A rescan of a subsystem with no live controller fails                                                 | Negative | `TestRescanOfASubsystemWithNoLiveControllerFails`            |
| U-111 | A swap warning is recorded on the swapped volume's claim                                              | Positive | `TestASwapWarningLandsOnTheVolumesClaim`                     |

---

## 2. Integration Tests

Run the operator's reconcile loop against a real Kubernetes API via `envtest`,
with a mock control plane serving design §9 and fake link peers serving
`SwapService`.

### End-to-End Reconcile (§7)

File: `operator/internal/controllers/volume/join_controller_test.go`

| #    | Scenario                                                                                                                                                                                                         | Type     | Test                                                                     |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------------------------------|
| I-01 | An on-pin join with a free slot in the group's subsystem runs `Planning` to `Succeeded`, swaps one volume on one host, and the mock sees `members`, `subsystem-moves`, `prepare`, `move`, and `commit` once each | Positive | `TestAWholeGroupFlipsThroughTheAPIServer`                                |
| I-02 | A full group subsystem moves three members on two hosts into an existing subsystem, and every host is suspended before the mock sees `move`                                                                      | Positive | `TestAWholeGroupFlipsThroughTheAPIServer`                                |
| I-03 | A full group subsystem with no candidate moves the group into a fresh subsystem, and `targetIsNew` is set                                                                                                        | Positive | `TestAWholeGroupFlipsThroughTheAPIServer`                                |
| I-04 | An off-pin join creates the child `Migrate`, waits for it, and continues when the child is marked `Succeeded`                                                                                                    | Positive | `TestAnOffPinJoinWaitsForItsChildOnTheAPIServer`                         |
| I-05 | A capacity refusal at the preflight fails the operation with no child and no lock taken                                                                                                                          | Negative | `TestACapacityRefusalTakesNoLockAndNoChildOnTheAPIServer`                |
| I-06 | A running `Migrate` operation holding a PV in the target subsystem defers the join until it finishes                                                                                                             | Negative | —                                                                        |
| I-07 | An operator restart while `Suspending` resumes the step, and each host sees one `Suspend`                                                                                                                        | Negative | `TestAnOperatorRestartInsideTheWindowSuspendsEachHostOnceOnTheAPIServer` |
| I-08 | A fake host that restarts between `Suspending` and `MovingNamespaces` leads to rollback, with no `move` call                                                                                                     | Negative | `TestAHostRestartBeforeTheMoveRollsBackOnTheAPIServer`                   |
| I-09 | Joins into two different groups on two different nodes run concurrently without sharing a lock                                                                                                                   | Positive | —                                                                        |
| I-10 | Two `StorageCluster` objects in one Kubernetes cluster: a join in one never calls the other's control plane or its hosts                                                                                         | Negative | —                                                                        |

---

## 3. E2E Tests

Run against a live simplyblock cluster with Kubernetes. Every row runs fio in
verify mode on every moving volume across the flip, and passes only when fio
reports no I/O error and no verification failure, `nvme list-subsys` on every
consuming host shows no path to the source subsystem afterward, and the measured
window is under `suspendTimeoutSeconds`.

### Flip per Shape (§6.1)

| #    | Scenario                                                                                                                                             | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | A plain filesystem volume joins a group with a free slot in its subsystem                                                                            | Positive | —    |
| E-02 | A raw block volume joins a group with a free slot                                                                                                    | Positive | —    |
| E-03 | A VDO volume (`client_compression`, `client_deduplication`) joins a group with a free slot, and `vdostats` reports the same savings before and after | Positive | —    |
| E-04 | A raw-block VDO volume joins a group with a free slot                                                                                                | Positive | —    |

### Subsystem Selection (§5)

| #    | Scenario                                                                                                                                        | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-05 | The group's subsystem is full and another subsystem on the store has room: every member moves into it, and non-members stay where they were     | Positive | —    |
| E-06 | The group's subsystem is full and none has room: the group moves into a fresh subsystem, and an emptied source subsystem is deleted             | Positive | —    |
| E-07 | The group's subsystem holds exactly one free slot: only the joining volume moves                                                                | Boundary | —    |
| E-08 | The node is at its subsystem limit and a fresh subsystem is needed: the operation fails with `SubsystemLimitReached` before the volume migrates | Negative | —    |
| E-09 | An off-pin volume migrates to the pinned node, joins, and flips in one operation                                                                | Positive | —    |

### Topology (§5, §6)

| #    | Scenario                                                                                                                                                                                  | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-10 | A three-node cluster: the fresh subsystem's listeners are `optimized` on the leader and `non_optimized` on the secondaries, and a host's multipath uses the optimized path after the flip | Positive | —    |
| E-11 | A single-node cluster: the flip runs with one listener node                                                                                                                               | Boundary | —    |
| E-12 | Members in two namespaces on two hosts move in one window                                                                                                                                 | Positive | —    |
| E-13 | A member staged without `dmLinear` is attached: the operation fails at `AttachingPaths`, nothing is suspended, and restarting the named pod lets a retry succeed                          | Negative | —    |
| E-14 | A stretched cluster: colocation is refused until P0-9 ships, and the join completes without it                                                                                            | Negative | —    |

---

## 4. Integration Tests — Phase 3 (Planned)

| #       | Scenario                                                                                                                                 |
|---------|------------------------------------------------------------------------------------------------------------------------------------------|
| I-P3-01 | Make-before-break: the mock exports the target before removing the source, and the host steps run with the target device already present |
| I-P3-02 | Make-before-break: the window measured on a live cluster is the reload alone                                                             |

---

## 5. Manual Scenarios and Test Concepts

### M-01 — The CSI Node Restarts Inside the Suspend Window

**Design reference:** §6.6

**What to verify:** a node restart while its mappings are suspended never leaves
a mapping suspended, and never lets the namespace move while that host writes to
the source.

**Test concept:**

1. Start a join whose moving set includes a volume consumed on worker A, with
   fio verify running and `suspendTimeoutSeconds: 30`.
2. When the operation reaches `Suspending`, delete the CSI node pod on worker A.
3. Assert the operation rolls back without a `move` call, fio reports no
   verification failure, and `dmsetup info sb-<lvol>` on worker A shows the
   mapping active.

### M-02 — The Operator Restarts Inside the Suspend Window

**Design reference:** §6.5, §7

**What to verify:** the window stays bounded by the deadline when the
orchestrator disappears.

**Test concept:**

1. Start a join with fio verify running.
2. Scale the operator to zero when the operation reaches `MovingNamespaces`, and
   scale it back after 60 seconds.
3. Assert every host resumed by the deadline plus grace, the operation finishes
   or rolls back on its own once the operator returns, and fio reports no
   verification failure.

### M-03 — The Control Plane Stops Answering Mid-Move

**Design reference:** §11

**What to verify:** a lost control plane leads to a rollback and a resume on
every host, and a late `move` is refused.

**Test concept:**

1. Start a join with fio verify running.
2. Block the operator's route to the control plane when the operation reaches
   `MovingNamespaces`, and restore it after the deadline.
3. Assert every host resumed by the deadline plus grace, on whichever subsystem
   holds the namespace, and that the operation ends rolled back or committed
   with `namespacesAt` matching the control plane's record.

### M-04 — Rescan Latency over NVMe/TCP

**Design reference:** §6.4, §15 Q2

**What to verify:** the time from a namespace being added to a connected
subsystem to the host seeing it, which sets the default timeout.

**Test concept:**

1. Connect a host to an empty shared subsystem.
2. Add a namespace through the control plane 100 times, measuring the time to
   `/dev/nvmeXnY` with and without an immediate `nvme ns-rescan`.
3. Record the distribution in `simplyblock_csi_node_swap_rescan_seconds`, and
   report the p99 against the 10-second default.

### M-05 — A Host Outside Kubernetes Is Connected

**Design reference:** §2 Non-Goals, §11

**What to verify:** a host the operator cannot coordinate is never paused
blindly.

**Test concept:**

1. Connect a bare host to the source subsystem of a group member with
   `nvme connect`.
2. Label a PVC for that group so the group moves.
3. Assert the operation fails at `AttachingPaths` naming the host NQN, and the
   control plane sees no `move` call.

---

## 6. Axis Coverage

| Axis             | Values covered                                                                   | IDs                          | Not covered                                   |
|------------------|----------------------------------------------------------------------------------|------------------------------|-----------------------------------------------|
| Cluster topology | 1 node, 3 nodes                                                                  | E-11, E-10                   | 5+ nodes                                      |
| Namespace scope  | single, multi                                                                    | I-01, U-52, E-12             | —                                             |
| Cluster count    | 1 `StorageCluster`, 2 in one Kubernetes cluster                                  | I-01, I-10                   | cross-cluster (the feature is single-cluster) |
| Stack shape      | plain, raw block, VDO, raw-block VDO, striped (refused)                          | E-01, E-02, E-03, E-04, U-05 | —                                             |
| Subsystem state  | free slot, exactly one slot, full with candidate, full with fresh, at node limit | E-01, E-07, E-05, E-06, E-08 | —                                             |
| Consuming hosts  | none, one, several                                                               | U-76, I-01, I-02             | a host outside Kubernetes (M-05)              |
| Lifecycle        | restart in each step, abort, deletion, terminal                                  | U-89, U-90, U-92, U-93, U-97 | control-plane restart mid-move (M-03)         |
| Trigger          | watcher label, hand-written operation                                            | U-47, I-01                   | —                                             |
| Version skew     | CSI node without `volume-swap`, control plane without the move endpoints         | U-74, U-58                   | —                                             |
| Stretched        | refused                                                                          | E-14                         | colocation on a stretched cluster (P0-9)      |

---

## 7. Coverage Summary

| Class       | Scenarios | Covered | Not covered      |
|-------------|-----------|---------|------------------|
| Unit        | 111       | 108     | U-05, U-73, U-98 |
| Integration | 10        | 7       | I-06, I-09, I-10 |
| E2E         | 14        | 0       | E-01 … E-14      |
| Manual      | 5         | 0       | M-01 … M-05      |

Every unit row is covered except U-05, U-73, and U-98, and seven of the ten integration
rows are. The end-to-end and manual rows need a live cluster.

---

## 8. What Is Not Yet Covered

| #                | Gap                                                                    | Reason                                                                                                   |
|------------------|------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------|
| U-73             | A connected host NQN with no matching node                             | The control plane reports no connected host NQNs to the operator, so the cross-check has nothing to read |
| U-98             | A stale status not acted on                                            | Covered by the kind's shared status writer, which no join-specific test exercises                        |
| I-06, I-09, I-10 | A running migration deferring the join, concurrent joins, two clusters | Not written yet. U-95 covers the deferral against a fake client                                          |
| E-01 … E-14      | Live flips                                                             | Needs a live cluster with the new CSI node, operator, and control-plane images                           |
| M-01 … M-05      | Failure injection inside the window                                    | Needs a live cluster and manual timing                                                                   |
| —                | 5+ node clusters                                                       | No lab of that size. The selection rule does not depend on node count beyond the HA set                  |
| —                | Colocation on a stretched cluster                                      | P0-9 not shipped                                                                                         |
| —                | Control-plane restart mid-move                                         | Covered in concept by M-03 only                                                                          |
