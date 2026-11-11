# Test Plan: FoundationDB Host Maintenance

Related design: [`designs/design-fdb-host-maintenance.md`](../designs/design-fdb-host-maintenance.md)

Scope is the operator, its webhooks, and the Kubernetes surface this repository
builds. FoundationDB, the FoundationDB operator, and `fdbcli` are dependencies,
faked at the boundary: a row asserts this operator's response to a
`FoundationDBCluster` status, a ConfigMap, a Job condition, or a pod deletion
the test performed, never the dependency's own behavior. The behaviors the
design relies on from the FoundationDB operator are its Phase 0 prerequisites,
and the rows that would prove them live in that operator's repository.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §8 with its reason. A struck row names the row that superseded it.

| Class       | Prefix | Harness                                                                        |
|-------------|--------|--------------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, Job conditions set by hand |
| Integration | `I-`   | Full reconcile loop against `envtest` with the FoundationDB CRDs installed     |
| E2E         | `E-`   | Live simplyblock deployment, real FoundationDB, `kubectl drain`                |
| Manual      | `M-`   | Needs failure injection or orchestration not automated yet                     |

Axes selected for this plan, from the `test-scenarios` skill: cluster topology
(one, two, three, four, and five FoundationDB hosts), the worker's shape
(FoundationDB only, or converged with a storage node), the drain shape (one
cordon and drain, a pool-wide cordon with serial drains, two drains at once, a
cordon with no drain, a node replaced rather than uncordoned), the drain signal
(sentinel eviction, neighbor eviction, none), and lifecycle (operator restart in
every step, uncordon in every step, deletion mid-window). Namespace scope is
excluded, because the control plane is a singleton per Kubernetes cluster and
its namespace is the only one the window reads. Cluster count is excluded for
the same reason.

---

## 1. Unit Tests

Pure functions and single reconcile calls against a fake client. A Job's
`Complete` or `Failed` condition, the `FoundationDBCluster` status, and a pod's
`DisruptionTarget` condition are set by the test. Numbering runs continuously
across the groups below.

### Choosing the Coordinators (design §5.5)

File: `operator/internal/controllers/controlplane/coordinators_test.go`

| #    | Scenario                                                                                                                                                       | Type     | Test                                                  |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|-------------------------------------------------------|
| U-01 | Four hosts, three coordinators on three of them, the drained host holds one: the set keeps the two off the host and adds one on the fourth, placement `Spread` | Positive | `TestTheVacatedSlotMovesToTheHostWithoutACoordinator` |
| U-02 | The drained host holds no coordinator: no set is written and the placement is `Unchanged`                                                                      | Negative | `TestAHostWithoutACoordinatorChangesNothing`          |
| U-03 | Entries already off the host are kept verbatim, so the written set differs from the current one in exactly the vacated entries                                 | Positive | `TestEntriesOffTheHostAreKeptVerbatim`                |
| U-04 | Candidates are taken in the class order `log`, `storage`, `transaction`, and within a class by process group ID                                                | Positive | `TestCandidatesAreTakenByClassThenByID`               |
| U-05 | A process group with a `processGroupConditions` entry is not a candidate                                                                                       | Negative | `TestAnUnhealthyOrDepartingGroupIsNotACandidate`      |
| U-06 | A process group with a `removalTimestamp` is not a candidate                                                                                                   | Negative | `TestAnUnhealthyOrDepartingGroupIsNotACandidate`      |
| U-07 | A `cluster_controller` or `stateless` process group is not a candidate                                                                                         | Negative | `TestOnlyStatefulClassesAreCandidates`                |
| U-08 | Three hosts, three coordinators, one on the drained host: the third entry lands on a host that already holds one, placement `SharedHost`                       | Boundary | `TestThreeHostsShareAHost`                            |
| U-09 | Two hosts, three coordinators, the drained host holds two: both land on the remaining host, placement `SharedHost`                                             | Boundary | `TestTwoHostsPutBothSlotsOnTheRemainingHost`          |
| U-10 | One host: no candidate exists, the placement is `Unchanged`, and the counts are reported                                                                       | Boundary | `TestOneHostCannotMoveAnything`                       |
| U-11 | Five hosts and five coordinators (`triple`), two on the drained host: both land on the two hosts without one, placement `Spread`                               | Positive | `TestFiveCoordinatorsSpreadOverTheFreeHosts`          |
| U-12 | Fewer stateful processes off the host than entries to fill: placement `Unchanged`, nothing is written                                                          | Negative | `TestTooFewCandidatesWriteNothing`                    |
| U-13 | The address is `<cluster>-<id>.<cluster>.<namespace>.svc.<dnsDomain>:<port>` with the port read from the current cluster file                                  | Positive | `TestTheAddressIsBuiltFromTheClusterFilePort`         |
| U-14 | A cluster file whose entries carry different ports is rejected rather than guessed                                                                             | Negative | `TestMixedPortsAreRefused`                            |
| U-15 | The drained host's zone is matched on `faultDomain`, not on the Node name, so a Node whose hostname label differs is handled                                   | Positive | `TestTheHostIsMatchedOnTheFaultDomain`                |
| U-16 | An empty process-group list, and an empty cluster file, each produce `Unchanged` and no panic                                                                  | Boundary | `TestEmptyInputsChangeNothing`                        |

### Issuing a Command (design §5.4)

File: `operator/internal/controllers/controlplane/fdbcommand_test.go`

| #    | Scenario                                                                                                                                                                          | Type                | Test                                                    |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|---------------------------------------------------------|
| U-17 | The Job runs `fdbMonitorImage`, mounts the ConfigMap key at `/etc/foundationdb/fdb.cluster`, and its command is `fdbcli -C /etc/foundationdb/fdb.cluster --timeout 30 --exec "…"` | Positive            | `TestACommandJobRunsFdbcliAgainstTheMountedClusterFile` |
| U-18 | The Job carries the FoundationDB node selector and tolerations, `restartPolicy: Never`, `backoffLimit: 3`, and the two deadlines                                                  | Positive            | `TestACommandJobTakesTheDatabasesPlacementAndItsLimits` |
| U-19 | The Job is owned by the operation                                                                                                                                                 | Positive            | `TestACommandJobIsOwnedByTheWindow`                     |
| U-20 | A step whose Job already exists reads it and creates no second one                                                                                                                | Negative            | `TestEnsureJobCreatesOnceAndReadsAfterward`             |
| U-21 | The Job name is `<operation>-<command>` and is a valid DNS label for the longest operation name the hash allows                                                                   | Boundary            | `TestJobNamesAreDNSLabels`                              |
| U-22 | A Job with neither condition holds the step, `Complete` completes it, and `Failed` completes it degraded                                                                          | Positive + Negative | `TestAJobIsRunningCompleteOrFailed`                     |

### Holding and Releasing (design §5.2, §5.8, §5.9, §5.10)

File: `operator/internal/controllers/controlplane/hostmaintenance_test.go`

| #    | Scenario                                                                                                                                     | Type     | Test                                                              |
|------|----------------------------------------------------------------------------------------------------------------------------------------------|----------|-------------------------------------------------------------------|
| U-23 | `Holding` labels every FoundationDB pod on the worker with the window's label and creates `sb-fdb-maintenance-<hash>` at `maxUnavailable: 0` | Positive | `TestHoldingLabelsTheWorkersFDBPodsAndRaisesTheBudget`            |
| U-24 | `Holding` does not label a FoundationDB pod on another worker, nor a non-FoundationDB pod on this one                                        | Negative | `TestHoldingLabelsTheWorkersFDBPodsAndRaisesTheBudget`            |
| U-25 | `Holding` does not complete before the disruption controller has looked at the budget and it allows zero disruptions                         | Boundary | `TestHoldingCompletesOnTheBudgetReportAndTheDrainAndTakesTheLock` |
| U-26 | `Holding` with no FoundationDB pod on the worker creates the budget over nothing and completes once a drain is observed                      | Boundary | `TestHoldingOnAWorkerWithoutFDBPodsCompletesOnTheDrain`           |
| U-27 | `Releasing` deletes the budget and removes the window's label, and leaves the storage node's label and budget in place                       | Positive | `TestReleasingTakesOnlyTheWindowsMarkersDown`                     |
| U-28 | `Releasing` completes only once the budget is gone                                                                                           | Boundary | `TestReleasingWaitsForTheBudgetToGo`                              |
| U-29 | `Cleanup` removes a label the release left and completes                                                                                     | Positive | `TestCleanupRemovesALeftoverLabelAndSucceeds`                     |
| U-30 | A window that fails in `MovingCoordinators` runs the teardown: budget gone, label gone                                                       | Negative | `TestAWindowThatFailsTakesItsMarkersDown`                         |
| U-31 | A window aborted in `Holding` runs the teardown                                                                                              | Negative | `TestAWindowAbortedWhileHoldingTakesItsMarkersDown`               |
| U-32 | A teardown whose budget delete fails twice emits `MaintenanceMarkersLeft` and the window still ends                                          | Negative | `TestATeardownThatCannotRemoveTheBudgetSaysSo`                    |
| U-33 | Deleting a running window runs the teardown and releases the lock when the window holds it                                                   | Negative | `TestDeletingARunningWindowTearsDownAndReleases`                  |

### Maintenance Mode (design §5.7)

File: `operator/internal/controllers/controlplane/hostmaintenance_test.go`

| #    | Scenario                                                                                                                            | Type     | Test                                                   |
|------|-------------------------------------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------------|
| U-34 | `EnteringMaintenance` issues `maintenance on <zone> <seconds>` with the `AwaitingHost` deadline and records `maintenanceUntil`      | Positive | `TestEnteringMaintenanceSetsTheZoneForTheHostBudget`   |
| U-35 | The zone is the Node's `kubernetes.io/hostname` label, recorded in status, and not the Node name                                    | Positive | `TestHoldingLabelsTheWorkersFDBPodsAndRaisesTheBudget` |
| U-36 | A failed `maintenance on` completes the step with `MaintenanceModeNotSet` and an empty `maintenanceUntil`                           | Negative | `TestAFailedMaintenanceOnCompletesTheStepWithoutIt`    |
| U-37 | `LeavingMaintenance` whose `maintenance status` names the window's zone issues `maintenance off` and emits `MaintenanceModeCleared` | Positive | `TestLeavingMaintenanceClearsTheWindowsOwnZone`        |
| U-38 | A failed `maintenance off` completes the step with `MaintenanceModeLeft`                                                            | Negative | `TestLeavingMaintenanceReportsWhatItCouldNotClear`     |
| U-39 | A Node with no hostname label fails `Holding` with a message naming the label                                                       | Negative | `TestHoldingFailsOnANodeWithoutAHostnameLabel`         |

### Moving and Verifying (design §5.5, §5.6)

File: `operator/internal/controllers/controlplane/hostmaintenance_test.go`

| #    | Scenario                                                                                                                                     | Type       | Test                                                    |
|------|----------------------------------------------------------------------------------------------------------------------------------------------|------------|---------------------------------------------------------|
| U-40 | The chosen set is written to `status.hostMaintenance.coordinators` before the Job is created                                                 | Positive   | `TestMovingCoordinatorsWritesTheSetThenWaitsForTheFile` |
| U-41 | The step holds while the cluster file still names a process on the host, and completes when it does not                                      | Positive   | `TestMovingCoordinatorsWritesTheSetThenWaitsForTheFile` |
| U-42 | A `Complete` Job with a cluster file that still names the host holds the step, and the deadline fails the window with `CoordinatorsReverted` | Negative   | `TestARevertedSetIsReportedAndFailsOnTheDeadline`       |
| U-43 | Placement `Unchanged` from a too-small cluster completes the step with `CoordinatorsNotMoved` and no Job                                     | Negative   | `TestATooSmallClusterMovesNothingAndSaysSo`             |
| U-44 | A `Failed` Job completes the step with placement `Unchanged` and `CoordinatorsNotMoved`                                                      | Negative   | `TestAFailedCoordinatorsCommandLeavesTheSetUnchanged`   |
| U-45 | A `SharedHost` set completes the step with `CoordinatorsMoved` naming the shared host                                                        | Positive   | —                                                       |
| U-46 | The window's zone and set survive an operator restart in this step, and the Job is read rather than recreated                                | Regression | `TestARestartInMovingCoordinatorsReadsTheExistingJob`   |

### Awaiting the Processes (design §5.9)

File: `operator/internal/controllers/controlplane/hostmaintenance_test.go`

| #        | Scenario                                                                                                                               | Type     | Test                                                           |
|----------|----------------------------------------------------------------------------------------------------------------------------------------|----------|----------------------------------------------------------------|
| ~~U-47~~ | ~~The step holds while the Node is unschedulable, with `AwaitingHostReturn`~~ Superseded by U-69 and U-70                              | —        | —                                                              |
| U-48     | The step holds while a recorded process group carries `MissingProcesses`                                                               | Negative | `TestAwaitingHostWaitsForTheRecordedProcessGroups`             |
| ~~U-49~~ | ~~The step completes when the Node is schedulable and no process group in the zone carries the condition~~ Superseded by U-70 and U-71 | —        | —                                                              |
| U-50     | A window whose recorded process groups have all left the status completes the step                                                     | Boundary | `TestAwaitingHostCompletesWithTheNodeGoneAndTheGroupsReplaced` |
| U-51     | The deadline fails the window, and the teardown finds nothing to take down                                                             | Negative | `TestAwaitingHostFailsOnItsDeadlineHoldingNoLock`              |

### The Trigger (design §5.1)

File: `operator/internal/controllers/controlplane/controlplane_controller_test.go`

| #    | Scenario                                                                                                                      | Type                | Test                                                  |
|------|-------------------------------------------------------------------------------------------------------------------------------|---------------------|-------------------------------------------------------|
| U-52 | A cordoned Node with a FoundationDB pod raises `<controlplane>-maintenance-<hash>` with the worker in `spec.hostMaintenance`  | Positive            | `TestACordonedFDBHostGetsAWindow`                     |
| U-53 | A cordoned Node with no FoundationDB pod raises nothing                                                                       | Negative            | `TestOnlyACordonedFDBHostGetsAWindow`                 |
| U-54 | A schedulable Node raises nothing                                                                                             | Negative            | `TestOnlyACordonedFDBHostGetsAWindow`                 |
| U-55 | A second cordon of the same worker finds the existing window and raises no second one                                         | Negative            | `TestASecondPassOverTheSameCordonRaisesNothingNew`    |
| U-56 | An uncordon aborts a window in `Holding` and leaves one in `EnteringMaintenance` running                                      | Positive + Negative | `TestAnUncordonAbortsOnlyAWindowStillHolding`         |
| U-57 | An uncordon deletes a window in a terminal phase                                                                              | Positive            | `TestAnUncordonOrAGoneNodeDeletesAFinishedWindow`     |
| U-58 | The hash of a worker whose name is a long fully qualified domain name keeps the operation name a DNS label                    | Boundary            | `TestTheWindowNameIsADNSLabelForALongWorkerName`      |
| U-59 | The Node watch maps a Node to the ControlPlane of the namespace that holds a FoundationDB pod on it, and to nothing otherwise | Positive + Negative | `TestANodeMapsToTheControlPlaneWhoseFDBPodsItCarries` |
| U-60 | A window created by hand with a worker that hosts no FoundationDB pod runs and completes with nothing moved                   | Boundary            | `TestAHandWrittenWindowUnderACordonIsLeftToRun`       |

### Admission and the Graph (design §4, §6)

File: `operator/internal/controllers/controlplane/controlplaneops_test.go`

| #        | Scenario                                                                                                                                                 | Type     | Test                                                             |
|----------|----------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------------------------------------------------------------------|
| U-61     | The `HostMaintenance` graph is a line with `Holding` as its only abortable step                                                                          | Positive | `TestTheWindowsGraphIsALineAbortableOnlyWhileHolding`            |
| U-62     | An abort requested in `EnteringMaintenance` or later is refused                                                                                          | Negative | `TestTheWindowsGraphIsALineAbortableOnlyWhileHolding`            |
| U-63     | `action: HostMaintenance` without `spec.hostMaintenance` is rejected by the CEL rule                                                                     | Negative | `TestControlPlaneOpsCELRequiresTheWorkerForHostMaintenance`      |
| U-64     | A change to `spec.hostMaintenance.workerNode` on an existing operation is rejected                                                                       | Negative | `TestControlPlaneOpsCELFreezesTheWorker`                         |
| U-65     | A `HostMaintenance` against a managed control plane is rejected by the validating webhook                                                                | Negative | `TestAHostMaintenanceWindowAgainstAManagedControlPlaneIsRefused` |
| ~~U-66~~ | ~~A second window for another worker sits `Pending` with `OperationQueued` while the first holds the lock, and its budget is not up~~ Superseded by U-72 | —        | —                                                                |
| ~~U-67~~ | ~~A `Restart` created while a window is in `AwaitingHost` waits for the lock~~ Superseded by U-73                                                        | —        | —                                                                |
| U-68     | A finished window reports `Completed`, `Succeeded`, and the placement it recorded                                                                        | Positive | `TestAFinishedWindowReportsItsPlacement`                         |

### Awaiting the Processes, Continued (design §5.1, §5.9)

File: `operator/internal/controllers/controlplane/hostmaintenance_test.go`

| #    | Scenario                                                                                                                      | Type                | Test                                                           |
|------|-------------------------------------------------------------------------------------------------------------------------------|---------------------|----------------------------------------------------------------|
| U-69 | The step holds with `AwaitingHostReturn` while a recorded process group is missing, whatever the Node says                    | Positive            | `TestAwaitingHostWaitsForTheRecordedProcessGroups`             |
| U-70 | The step completes when every recorded group's replacement pod is ready and none is missing, while the Node is still cordoned | Positive            | `TestAwaitingHostWaitsForTheRecordedProcessGroups`             |
| U-71 | The step completes when no recorded process group is missing and the Node has been deleted                                    | Positive            | `TestAwaitingHostCompletesWithTheNodeGoneAndTheGroupsReplaced` |
| U-74 | A Node deletion deletes a terminal window, and leaves a window in `AwaitingHost` running                                      | Positive + Negative | `TestAnUncordonOrAGoneNodeDeletesAFinishedWindow`              |
| U-91 | `Holding` records the zone and the process groups whose fault domain it is, before the budget is created                      | Positive            | `TestHoldingRecordsTheZoneBeforeTheBudget`                     |
| U-92 | A recorded process group the upstream operator replaced, so that it left the status, counts as returned                       | Boundary            | `TestAwaitingHostCompletesWithTheNodeGoneAndTheGroupsReplaced` |

### The Lock (design §7.3)

File: `operator/internal/controllers/controlplane/controlplaneops_test.go`

| #    | Scenario                                                                                                                                                                                  | Type                | Test                                                    |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|---------------------------------------------------------|
| U-72 | Two windows for two workers both hold with their budgets up. The second to observe its drain waits in `Holding` with `MoveQueued` while the first moves, and takes the lock when it frees | Positive            | `TestAWindowWhoseDrainArrivesDuringAMoveQueuesBehindIt` |
| U-73 | A `Restart` created while a window is in `AwaitingHost` runs, and one created while a window is in `MovingCoordinators` waits for the lock                                                | Positive + Negative | —                                                       |
| U-83 | The lock is taken on the transition out of `Holding` and given back on the transition out of `Releasing`, and the phase is `Running` from `Holding`                                       | Positive            | `TestAWindowStartsWithoutTheLock`                       |
| U-84 | A window that fails in `MovingCoordinators` releases the lock from its terminal transition, and one that fails in `AwaitingHost` holds none                                               | Negative            | `TestAwaitingHostFailsOnItsDeadlineHoldingNoLock`       |
| U-93 | The three existing actions still hold the lock from their first step to their last                                                                                                        | Regression          | `TestOnlyTheMoveHoldsTheLock`                           |

### Observing the Drain (design §5.3)

File: `operator/internal/controllers/controlplane/hostmaintenance_test.go` and `operator/internal/controllers/controlplane/fdbcommand_test.go`

| #     | Scenario                                                                                                                                                                                                                                                                                                                                 | Type       | Test                                                        |
|-------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|-------------------------------------------------------------|
| U-75  | `Holding` creates the sentinel Job `<operation>-sentinel`: `fdbMonitorImage` with `sleep infinity`, `spec.nodeName` set to the worker, `backoffLimit: 0`, owned by the operation, and no label the budget selects                                                                                                                        | Positive   | `TestTheSentinelIsAPinnedPauseJobNothingRecreates`          |
| U-76  | The sentinel pod's deletion timestamp, or its absence after the Job recorded a pod, writes `drainObservedAt` and `drainObservedBy: Sentinel` on a `Holding` window                                                                                                                                                                       | Positive   | `TestTheSentinelsEvictionIsTheDrain`                        |
| U-77  | A second signal does not overwrite an observation already recorded                                                                                                                                                                                                                                                                       | Negative   | `TestTheFirstObservationIsNotOverwritten`                   |
| U-78  | A sentinel deleted while the window is past `Holding` writes nothing, and a sentinel that never got a pod writes nothing                                                                                                                                                                                                                 | Negative   | `TestASignalAfterHoldingWritesNothing`                      |
| U-79  | A non-FoundationDB pod on the worker carrying `DisruptionTarget` with reason `EvictionByEvictionAPI` writes `drainObservedBy: PodDisruption`                                                                                                                                                                                             | Positive   | `TestANeighborsEvictionIsTheDrain`                          |
| U-80  | The same condition on a pod on another worker, or with another reason, writes nothing                                                                                                                                                                                                                                                    | Negative   | `TestADisruptionThatIsNotAnEvictionIsNotTheDrain`           |
| U-81  | `Holding` completes only when the budget reports zero allowed disruptions and a drain has been observed, in either order                                                                                                                                                                                                                 | Boundary   | `TestHoldingNeedsBothTheReportAndTheDrain`                  |
| U-82  | `Holding` with no observation waits to its deadline, fails, and the teardown deletes the budget                                                                                                                                                                                                                                          | Negative   | `TestAHoldingWindowNothingReachesReleasesOnItsDeadline`     |
| U-94  | A sentinel Job that cannot be created leaves the window `Holding` with the budget up and `EvictionHeld`, and the neighbor signal still completes it                                                                                                                                                                                      | Negative   | `TestASentinelThatCannotBeCreatedStillLeavesTheHoldWorking` |
| U-96  | The `Holding` deadline is measured from the latest `drainObservedAt` across the windows of the namespace, and from the window's own creation while none exists                                                                                                                                                                           | Boundary   | `TestTheHoldingDeadlineFollowsTheLastDrainInTheNamespace`   |
| U-97  | A window older than the deadline keeps holding when another window observed a drain inside it, and fails when none did                                                                                                                                                                                                                   | Boundary   | `TestTheHoldingDeadlineFollowsTheLastDrainInTheNamespace`   |
| U-98  | A drain observed by one window re-enqueues every other window in `Holding` and re-arms its deadline                                                                                                                                                                                                                                      | Positive   | `TestADrainObservedByOneWindowWakesTheOthers`               |
| U-99  | A budget whose owner has no scale subresource carries `SyncFailed` and no pod count, and `Holding` still completes on it. Regression: the lab's first cordon on 2026-11-11 sat in `Holding` forever                                                                                                                                      | Regression | `TestHoldingAcceptsABudgetTheControllerCannotCount`         |
| U-100 | The pods from before the drain, still running on a cordoned worker, do not count as returned, nor do replacements that are not ready, and the pods from before the drain count once the worker is schedulable again. Regression: the lab's first drain on 2026-11-11 cleared the zone nine seconds before `MissingProcesses` was written | Regression | `TestAwaitingHostWaitsForThePodsTheDrainEvictedToReturn`    |
| U-101 | A pass on a window the cache served stale fails its status write on a conflict and emits nothing, so a step's event appears once on the record. Regression: both lab runs on 2026-11-11 showed `MaintenanceModeSet` twice                                                                                                                | Regression | `TestAStalePassDoesNotRepeatAStepsEvent`                    |

### Leaving Hosts and the Zone Takeover (design §5.5, §5.7)

File: `operator/internal/controllers/controlplane/coordinators_test.go` and `operator/internal/controllers/controlplane/hostmaintenance_test.go`

| #    | Scenario                                                                                                                                         | Type                | Test                                                         |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|--------------------------------------------------------------|
| U-85 | A process group on the zone of another window that has observed its drain is not a candidate, and one on a cordoned host whose window has not is | Positive + Negative | `TestALeavingHostIsNotATarget`                               |
| U-86 | `EnteringMaintenance` while another window's zone is active completes and emits `MaintenanceZoneTakenOver` on that window                        | Positive            | `TestEnteringMaintenanceTakesTheZoneOverFromAnEarlierWindow` |
| U-87 | `LeavingMaintenance` whose `maintenance status` names another zone issues no `maintenance off` and completes                                     | Negative            | `TestLeavingMaintenanceLeavesAnotherZoneAlone`               |
| U-88 | `LeavingMaintenance` whose `maintenance status` reports no zone issues nothing and completes                                                     | Boundary            | `TestLeavingMaintenanceLeavesAnotherZoneAlone`               |
| U-95 | A `maintenance status` output the step cannot parse completes the step with `MaintenanceModeLeft`                                                | Negative            | `TestLeavingMaintenanceReportsWhatItCouldNotClear`           |

### The Job's Placement (design §5.4)

File: `operator/internal/controllers/controlplane/fdbcommand_test.go`

| #    | Scenario                                                                                                                                | Type     | Test                                                  |
|------|-----------------------------------------------------------------------------------------------------------------------------------------|----------|-------------------------------------------------------|
| U-89 | The Job tolerates `node.kubernetes.io/unschedulable` with effect `NoSchedule` and carries a required node affinity away from the worker | Positive | `TestACommandJobToleratesTheCordonAndAvoidsTheWorker` |
| U-90 | On a cluster where the worker is the only node matching the selector, the affinity is dropped                                           | Boundary | `TestACommandJobOnTheOnlyNodeCarriesNoAffinity`       |

### Handing the Roles Off (design §5.12)

Files: `operator/internal/controllers/controlplane/hostmaintenance_handoff_test.go`, `controlplaneops_test.go`, `install_test.go`, and `operator/internal/webhook/controlplaneops_validator_test.go`

| #     | Scenario                                                                                                                                                   | Type       | Test                                                                                 |
|-------|------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|--------------------------------------------------------------------------------------|
| U-102 | `ExcludingProcesses` writes the recorded groups whose class is not `storage` to `excludedProcesses` before the Job, and the command names each by locality | Positive   | `TestExcludingProcessesHandsTheLogAndStatelessRolesOff`                              |
| U-103 | A `Complete` `exclude` Job completes the step with `ProcessesHandedOff`, and the window moves to `Releasing`                                               | Positive   | `TestExcludingProcessesHandsTheLogAndStatelessRolesOff`                              |
| U-104 | A `Failed` `exclude` Job completes the step with `ProcessesNotHandedOff`, and `excludedProcesses` is kept for the include                                  | Negative   | `TestAFailedExclusionReleasesAndKeepsTheListForTheInclude`                           |
| U-105 | A host whose recorded groups are all `storage` completes the step with no Job and no event                                                                 | Boundary   | `TestAStorageOnlyHostExcludesNothing`                                                |
| U-106 | An operator restart in `ExcludingProcesses` reads the existing Job rather than creating a second                                                           | Regression | `TestARestartInExcludingProcessesReadsTheExistingJob`                                |
| U-107 | `IncludingProcesses` issues `include` over the recorded list and completes with `ProcessesIncluded`                                                        | Positive   | `TestIncludingProcessesLiftsTheRecordedExclusion`                                    |
| U-108 | A `Failed` `include` Job completes the step with `ProcessesLeftExcluded`, whose message carries the `include` command                                      | Negative   | `TestAFailedIncludeNamesTheCommandToRunByHand`                                       |
| U-109 | A window that recorded no exclusion issues no `include` Job                                                                                                | Boundary   | `TestAWindowThatExcludedNothingIncludesNothing`                                      |
| U-110 | A window that fails after `ExcludingProcesses` creates the `include` Job from its terminal teardown                                                        | Negative   | `TestAWindowThatFailsAfterExcludingStillIncludes`                                    |
| U-111 | `ExcludingProcesses` holds the lock, `IncludingProcesses` does not, and the two sit between the steps §6 places them between                               | Positive   | `TestOnlyTheMoveHoldsTheLock`, `TestTheWindowsGraphIsALineAbortableOnlyWhileHolding` |
| U-112 | The `FoundationDBCluster` is applied with `processCounts.stateless` equal to the log count                                                                 | Positive   | `TestStatelessProcessesMatchTheLogCount`                                             |
| U-113 | The delete guard refuses a delete in `ExcludingProcesses` and `IncludingProcesses`, and the refusal table agrees with the graph                            | Positive   | `TestTheControlPlaneRefusalTableAndTheGraphAgree`                                    |

---

## 2. Integration Tests

Full reconcile loop against `envtest` with the FoundationDB CRDs applied from
`helm-charts/charts/simplyblock-operator/templates/`, a `FoundationDBCluster`
whose status the test writes, and Jobs whose
conditions the test sets. The eviction API is real, so a budget's effect is
asserted by an `Eviction` request.

### The Window End to End (design §6)

| #    | Scenario                                                                                                                                                                | Type       | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| I-01 | Cordon a Node hosting a FoundationDB pod → a window is raised, the budget goes up, and an `Eviction` of the pod is refused with 429                                     | Positive   | —    |
| I-02 | The test marks the `maintenance-on` and `coordinators` Jobs `Complete` and rewrites the cluster file → the window reaches `Releasing`, and the `Eviction` succeeds      | Positive   | —    |
| I-03 | The test clears `MissingProcesses` and marks the `maintenance-status` and `maintenance-off` Jobs `Complete` → the window ends `Succeeded`, and the pod carries no label | Positive   | —    |
| I-04 | Uncordon while `Holding` → the window ends `Aborted` with no budget left                                                                                                | Negative   | —    |
| I-05 | The cluster file is not rewritten → the window holds in `MovingCoordinators` and fails on the deadline with the budget gone afterward                                   | Negative   | —    |
| I-06 | A converged worker: both the storage node's budget and this window's hold the pod, and the `Eviction` succeeds only once both are gone                                  | Positive   | —    |
| I-07 | The manager is restarted in every step → the window resumes with no second Job and no second budget                                                                     | Regression | —    |
| I-08 | The drain of I-01 evicts the sentinel, which stamps the window with `drainObservedBy: Sentinel`, and the window leaves `Holding`                                        | Positive   | —    |
| I-09 | Two Nodes cordoned, both windows `Holding` with budgets up. An `Eviction` on the second Node's pod moves that window first, and the first Node's window holds on        | Positive   | —    |
| I-10 | The Node is deleted while the window is in `AwaitingHost` → the window completes when the recorded process groups report, and the record is deleted afterward           | Positive   | —    |

---

## 3. E2E Tests

A live simplyblock deployment on four workers with FoundationDB on all of them,
a management-API transaction load from a client that reports commit errors, and
`kubectl drain --ignore-daemonsets` on a FoundationDB host. Data correctness is
asserted on the control plane's own records: a set of volumes created before
the drain is listed unchanged after it.

### A Drain of a FoundationDB Host (design §5)

| #        | Scenario                                                                                                                                                                                                                                                        | Type     | Test |
|----------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01     | Drain the host of a coordinator → the drain waits on `sb-fdb-maintenance-<hash>`, the cluster file names three processes off the host before any pod is evicted, placement `Spread`                                                                             | Positive | —    |
| E-02     | During E-01, `fdbcli status json` reports three reachable coordinators throughout, and `maintenance_zone` is the host for the length of the drain                                                                                                               | Positive | —    |
| E-03     | During E-01, `status json` reports no data movement for the host's storage server, and the commit error count is bounded to the recoveries of the move and the eviction                                                                                         | Positive | —    |
| E-04     | Uncordon the host → the pods return, the window ends `Succeeded`, `maintenance_zone` is empty, and the volumes created before the drain are listed unchanged                                                                                                    | Positive | —    |
| E-05     | Drain a host that holds no coordinator → placement `Unchanged`, the drain proceeds after the budget, maintenance mode is still set and cleared                                                                                                                  | Negative | —    |
| E-06     | Drain the host that runs the operator pod → the window completes from the rescheduled manager                                                                                                                                                                   | Positive | —    |
| E-07     | Drain a converged worker → both windows run, the SPDK node goes offline before the FoundationDB pods are evicted, both end `Succeeded`                                                                                                                          | Positive | —    |
| E-08     | Three FoundationDB hosts: drain one → placement `SharedHost`, the FoundationDB operator logs its not-enough-processes error and does not revert, and after the processes return it re-elects a one-per-host set                                                 | Positive | —    |
| ~~E-09~~ | ~~Cordon two FoundationDB hosts at once → the second window is `Pending`, its drain proceeds unheld, and it runs after the first ends~~ Superseded by E-11                                                                                                      | —        | —    |
| E-10     | Cordon without draining, then uncordon → the window holds with no command issued and ends `Aborted`, with no pod evicted and nothing changed in the database                                                                                                    | Boundary | —    |
| E-11     | Cordon every worker, then drain them one at a time in an order other than the cordon's → every window holds at once, each drain is held under sixty seconds, `DrainObserved` timestamps follow the drain order, and three coordinators are reachable throughout | Positive | —    |
| E-12     | E-11 with each window's sentinel deleted by hand before its drain → every window reports `drainObservedBy: PodDisruption` and the holds stay under sixty seconds                                                                                                | Positive | —    |
| E-13     | Drain two FoundationDB hosts at the same moment → the second window emits `MoveQueued`, its candidates exclude the first host, and both drains complete                                                                                                         | Positive | —    |
| E-14     | On a cluster with cloud volumes, drain and terminate a host → its pods return on a replacement node, and the window ends `Succeeded` with the Node gone                                                                                                         | Positive | —    |
| E-15     | Drain the host of a log process under a raw FoundationDB client committing and reading every half second with a 5 s timeout and no retry → no commit or read fails, every recovery happens before the budget comes down, and none follows the eviction          | Positive | —    |

---

## 4. Manual Scenarios and Test Concepts

### M-01 — The FoundationDB operator reverts the set

**Design reference:** design §5.6, §10

**What to verify:** a reverted set is detected and the drain is still released.

**Test concept:**
1. Run E-01 against a FoundationDB operator build whose coordinator validity
   accepts the set and whose re-election then changes it anyway, or edit the
   ConfigMap by hand after the Job completes to put the host's process back.
2. Assert the window holds in `MovingCoordinators`, fails on the deadline with
   `CoordinatorsReverted`, and the budget is gone afterward.
3. Assert `kubectl drain` completes.

### M-02 — The host's processes do not come back

**Design reference:** design §5.9, §10

**What to verify:** a lost host fails the window without leaving a marker, and
FoundationDB expires the maintenance zone.

**Test concept:**
1. Run E-01 and power the host off after the drain completes.
2. Wait past the `AwaitingHost` deadline, lowered for the run.
3. Assert the window is `Failed`, no budget and no label exist, and
   `status json` reports no maintenance zone once the recorded
   `maintenanceUntil` has passed.

### M-03 — Control-plane transactions through the recoveries

**Design reference:** design §14 Q3

**What to verify:** the management API's writes during the move, the
hand-off, and the eviction either succeed or return a retryable error, and no
write is reported successful and then absent. E-15 is the same run with a raw
client beside it, which is what separates a latency spike from a timeout.

**Test concept:**
1. Drive a loop of volume creates through the management API, each followed by
   a read of what it created.
2. Run E-01.
3. Assert every create that returned success is read back, and classify every
   error returned by its FoundationDB error code.

### M-04 — A drain no signal reaches

**Design reference:** design §5.3, §10

**What to verify:** a worker whose only evictable pods are the held FoundationDB
pods, drained after its sentinel was removed, is held to the `Holding` deadline
and then released, and the update tooling's PodDisruptionBudget timeout reports
it.

**Test concept:**
1. On a worker that runs only DaemonSet pods beside FoundationDB, delete the
   window's sentinel Job and lower the `Holding` deadline for the run.
2. Drain the worker.
3. Assert the drain is refused by the budget until the deadline, that the window
   then fails with the budget deleted, and that the drain completes afterward.

### M-05 — A node group update longer than the Holding deadline

**Design reference:** design §5.3, §6

**What to verify:** windows waiting for a drain that arrives more than six hours
after the cordon are still holding when it does.

**Test concept:**
1. Cordon a pool of FoundationDB hosts and drain them serially with a pause
   between drains, so that the last drain starts more than the `Holding`
   deadline after the cordon, with the deadline lowered for the run.
2. Assert every window is still `Holding` with its budget up when its drain
   arrives, and that each `DrainObserved` restarted the others' clocks.
3. Pause longer than the deadline after one drain, and assert the remaining
   windows fail and release only then.

---

## 5. Axis Coverage

| Axis                | Values covered                                                                                                            | IDs                                                        | Not covered                                           |
|---------------------|---------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------|-------------------------------------------------------|
| FoundationDB hosts  | 1, 2, 3, 4, 5                                                                                                             | U-10, U-09, U-08, U-01, U-11, E-08                         | more than five hosts, and a `three_data_hall` cluster |
| Worker shape        | FoundationDB only, converged with a storage node                                                                          | U-27, I-06, E-06, E-07                                     | a worker with two FoundationDB pods of one class      |
| Coordinator count   | 1 (`single`), 3 (`double`), 5 (`triple`)                                                                                  | U-10, U-01, U-11                                           | 9 (multi-region)                                      |
| Drain shape         | one drain, pool-wide cordon with serial drains, two at once, cordon only, replacement, an update longer than the deadline | E-01, E-11, E-13, E-10, E-14, I-09, I-10, U-72, U-97, M-05 | a drain that bypasses the eviction API                |
| Drain signal        | eviction request, neighbor eviction, none                                                                                 | U-75, I-08, U-79, E-12, U-82, M-04                         | both signals arriving in the same reconcile           |
| Lifecycle           | restart per step, uncordon per step, Node deletion, deletion of the window                                                | U-46, U-106, I-07, U-56, U-57, U-74, I-10, U-33            | the `ControlPlane` deleted mid-window                 |
| Classes on the host | log and storage, log and stateless and storage, storage only                                                              | U-102, U-105, E-01, E-15                                   | a host with a stateless process and no log            |

---

## 6. Coverage Summary

Struck rows are not counted.

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 109       | 107     | U-45, U-73  |
| Integration | 10        | 0       | I-01 … I-10 |
| E2E         | 14        | 0       | E-01 … E-15 |
| Manual      | 5         | 0       | M-01 … M-05 |

---

## 7. Coverage by Design Section

| Design section           | IDs                                                                 |
|--------------------------|---------------------------------------------------------------------|
| §4 Data model            | U-63, U-64, U-65, U-68                                              |
| §5.1 Trigger             | U-52 … U-60, U-74, I-01, I-04, I-10                                 |
| §5.2 Holding             | U-23 … U-26, U-81, U-91, I-01, U-99                                 |
| §5.3 Observing the drain | U-75 … U-82, U-94, U-96 … U-98, I-08, E-11, E-12, M-04, M-05        |
| §5.4 Commands            | U-17 … U-22, U-89, U-90                                             |
| §5.5 Choosing            | U-01 … U-16, U-43, U-45, U-85, E-01, E-05, E-08, E-13               |
| §5.6 Verifying           | U-41, U-42, I-02, I-05, M-01                                        |
| §5.7 Maintenance mode    | U-34 … U-39, U-86 … U-88, U-95, E-02, E-04, M-02, U-101             |
| §5.8 Releasing           | U-27, U-28, I-02                                                    |
| §5.9 Awaiting and ending | U-29 … U-33, U-48, U-50, U-51, U-69 … U-71, U-92, I-03, E-14, U-100 |
| §5.10 Converged worker   | U-27, I-06, E-07                                                    |
| §5.11 Rolling update     | E-11, E-12, I-09                                                    |
| §5.12 Hand-off           | U-102 … U-113, E-15, M-03                                           |
| §6 State machine         | U-61, U-62, U-46, U-96, U-97, I-07                                  |
| §7.3 Lock                | U-72, U-73, U-83, U-84, U-93, E-13                                  |

---

## 8. What Is Not Yet Covered

| #           | Gap                                                                                    | Reason                                                                                                                                             |
|-------------|----------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| U-45        | A `SharedHost` set completing the step with `CoordinatorsMoved` naming the shared host | The placement is proven at the choice (U-08). The step's event text for it is not asserted                                                         |
| U-73        | A `Restart` running during `AwaitingHost` and waiting during a move                    | The lock scope is proven on the window's own transitions (U-83, U-84). The other action's view of it is not asserted                               |
| I-01 … I-10 | Every integration scenario                                                             | No `envtest` suite carries the FoundationDB CRDs yet                                                                                               |
| E-01 … E-15 | Every end-to-end scenario                                                              | Needs the implementation. E-08 needs a lab with exactly three FoundationDB hosts, and E-14 a lab on cloud volumes whose drained node is replaced   |
| M-01 … M-05 | The manual scenarios                                                                   | Failure injection in the FoundationDB operator, a powered-off host, a client harness, a worker with no unheld pod, and a multi-hour serial drain   |
| —           | More than five FoundationDB hosts, and a `three_data_hall` cluster                     | The operator never applies either shape                                                                                                            |
| —           | A worker with two FoundationDB pods of one class                                       | The anti-affinity is preferred, not required, so it can happen on a small cluster. The choice treats such pods as one host, and nothing asserts it |
| —           | Nine coordinators                                                                      | Multi-region FoundationDB is not applied by the operator                                                                                           |
| —           | A drain that bypasses the eviction API                                                 | No budget holds a direct delete, and a delete evicts no sentinel. The design names it a non-goal                                                   |
| —           | Both drain signals arriving in the same reconcile                                      | The first write wins and the second is a no-op by U-77. Which one wins is not observable from outside, and nothing asserts it                      |
| —           | The `ControlPlane` deleted mid-window                                                  | The missing-target path is the shared `Ops` behavior, tested on the existing actions                                                               |
