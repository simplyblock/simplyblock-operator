# Test Plan: StorageNode and StorageNodeOps

Related design: [`designs/crd-redesign/design-storagenode.md`](../designs/crd-redesign/design-storagenode.md)

Supersedes `test-plan-storagenode-ops.md` and `test-plan-drain-remove.md`, both
removed in the same change. Their scenarios were prose rows without permanent
identifiers, and they are re-expressed here with IDs, keeping the original wording
wherever it survived the rework.

Scope is the operator, its webhooks, and the Kubernetes surface this repository
builds. The control plane (`sbcli`) and SPDK are dependencies, faked at the
boundary: what a row asserts is the operator's response to a control-plane
answer, never the control plane's own behavior.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason. A struck-through ID is a scenario that does not
describe the shipped system, kept because review history cites it, and its
`Test` column names the row that replaced it.

Both kinds ship at `v1alpha2` and every row names that spelling: `spec.clusterRef`
for the parent, `spec.config` for the per-node block, `spec.slot` for the socket
ordinal, `spec.nodeRef` for the operation's target, `status.step` for its
position, and PascalCase action values (`Remove`, `Migrate`). The reconcilers live
in `operator/internal/controllers/node/`, and the retired `StorageNodeSet` that
several rows were first written against is gone: the workload objects belong to
the `StorageCluster`, and the node's own object name is derived by the
`ClusterDeploymentConfig` expansion that creates it.

| Class       | Prefix | Harness                                                                          |
|-------------|--------|----------------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, and a scripted control plane |
| Integration | `I-`   | Full reconcile loop against `envtest` and a mock backend                         |
| E2E         | `E-`   | Live simplyblock cluster, real data path                                         |
| Manual      | `M-`   | Needs failure injection or orchestration not automated yet                       |

---

## 1. Unit Tests

Pure functions and single reconcile calls against a fake client, with the control
plane replaced by the scripted surface in
`operator/internal/controllers/node/fixtures_test.go`, which answers what a case
states and records what was asked of it. No Kubernetes API server is involved.

### Entity: Object Naming (design §3.1)

File: `operator/internal/controllers/deployment/nodename_test.go`

The name is derived from the cluster, the worker, and the slot through atlas-lib's
formula, so it is deterministic and the same three inputs always produce it again.
The rows that described a random identifier are struck: an expansion finds the
node it already created by the worker and slot its spec records, so the name is a
function of those three inputs and of nothing else.

| #        | Scenario                                                             | Type     | Test                                      |
|----------|----------------------------------------------------------------------|----------|-------------------------------------------|
| U-01     | A generated name is non-empty, lowercase, and at most 63 characters  | Positive | `TestANodeNameFitsTheLabelItIsCopiedInto` |
| U-02     | A parent name long enough to overflow is truncated to a valid label  | Boundary | `TestANodeNameFitsTheLabelItIsCopiedInto` |
| ~~U-03~~ | ~~Two calls with the same parent name produce different names~~      | Positive | Superseded by U-265                       |
| U-04     | A generated name contains only DNS-label characters                  | Positive | `TestANodeNameFitsTheLabelItIsCopiedInto` |
| ~~U-05~~ | ~~Uppercase and underscores in a worker hostname are replaced~~      | Negative | Superseded by U-265                       |
| ~~U-06~~ | ~~Leading and trailing hyphens are stripped from a sanitized label~~ | Boundary | Superseded by U-265                       |
| ~~U-07~~ | ~~A name collision on create is retried with a fresh identifier~~    | Negative | Superseded by U-266                       |
| ~~U-08~~ | ~~The generated name encodes neither the worker nor the socket~~     | Negative | Superseded by U-265                       |
| U-265    | The same cluster, worker, and slot derive the same name every time   | Positive | —                                         |
| U-266    | Two workers whose names share a long prefix do not collide           | Boundary | —                                         |

### Entity: Provisioning Gates (design §4.2)

Files: `operator/internal/controllers/node/provisioning_test.go`,
`slot_race_test.go`, `provisioningslots_test.go`, `resolve_retry_test.go`

| #     | Scenario                                                                             | Type       | Test                                                |
|-------|--------------------------------------------------------------------------------------|------------|-----------------------------------------------------|
| U-09  | `enableFailureDomains` set and no fault group declared: provisioning is held         | Negative   | `TestANodeWithNoFaultGroupIsHeldRatherThanRefused`  |
| U-10  | `enableFailureDomains` set and a fault group present: provisioning proceeds          | Positive   | `TestANodeThatDeclaresItsFaultGroupPasses`          |
| U-11  | `enableFailureDomains` unset: the fault group is not required                        | Negative   | `TestANodeThatDeclaresItsFaultGroupPasses`          |
| U-12  | A `failureDomain` label of `0`: a label like any other, not read as unset            | Boundary   | —                                                   |
| U-13  | Held provisioning emits `FailureDomainMissing` and issues no `POST`                  | Negative   | `TestANodeWithNoFaultGroupIsHeldRatherThanRefused`  |
| U-14  | The worker's storage-node API answers: the host check passes                         | Positive   | —                                                   |
| U-15  | The worker's storage-node API is unreachable: held, no `POST`                        | Negative   | —                                                   |
| U-16  | TLS is enabled and the CA is missing: the host check fails informatively             | Negative   | —                                                   |
| U-17  | The host check retries until the endpoint answers                                    | Positive   | —                                                   |
| U-18  | Nothing in flight and one slot free: exactly one of three waiting nodes takes it     | Boundary   | `TestOnlyOneNodeTakesAFreeSlot`                     |
| U-19  | Siblings past `Posting` without a UUID hold the slot                                 | Positive   | `TestANodeInFlightFillsTheCap`                      |
| U-20  | The node counts every sibling but itself                                             | Boundary   | —                                                   |
| U-21  | Two nodes on one worker count as one in-flight worker, not two                       | Boundary   | —                                                   |
| U-22  | A worker already in flight does not block another worker under the limit             | Positive   | `TestACapOfTwoAdmitsTwo`                            |
| U-23  | `maxParallelNodeAdds` reached: the node holds at `AwaitingSlot` and issues no `POST` | Negative   | `TestANodeInFlightFillsTheCap`                      |
| U-24  | Workers hosting a FoundationDB pod are identified                                    | Positive   | —                                                   |
| U-25  | A FoundationDB worker holds while another FoundationDB worker is in flight           | Negative   | —                                                   |
| U-26  | A FoundationDB worker holds even when `maxParallelNodeAdds` allows more              | Boundary   | —                                                   |
| U-27  | A non-FoundationDB worker is not held by a FoundationDB worker in flight             | Negative   | —                                                   |
| U-267 | The same waiting node wins the slot on every pass, so nobody overtakes it            | Positive   | `TestTheChoiceIsStable`                             |
| U-391 | Three nodes reconciling against a cache holding only themselves: one slot admits one | Regression | `TestTheCapHoldsWhenNodesCannotSeeEachOther`        |
| U-392 | The same, with two slots free: two are admitted and the third is not                 | Regression | `TestACapOfTwoHoldsWhenNodesCannotSeeEachOther`     |
| U-393 | A slot taken against a cluster read the holder has not seen is refused               | Regression | `TestASlotTakenFromAStaleReadIsRefused`             |
| U-394 | Taking a slot records the worker and the object that took it on the cluster          | Positive   | `TestTakingASlotIsRecordedOnTheCluster`             |
| U-395 | Re-entering `AwaitingSlot` with a slot held keeps the one entry                      | Boundary   | `TestASlotIsNotTakenTwice`                          |
| U-396 | A node releases the slot it holds and leaves another node's alone                    | Boundary   | `TestASlotIsReleasedOnlyByItsHolder`                |
| U-397 | A slot whose `StorageNode` no longer exists is reaped, so the cap reopens            | Regression | `TestASlotWhoseNodeIsGoneIsReaped`                  |
| U-398 | A slot whose holder already has its UUID is reaped                                   | Regression | `TestASlotWhoseNodeIsFinishedIsReaped`              |
| U-399 | A worker's second socket takes no second slot and resolves against the first         | Regression | `TestASecondSocketDoesNotTakeASecondSlot`           |
| U-400 | A backend node appearing while a node queues for a slot is adopted, not re-added     | Regression | `TestANodeWaitingForASlotAdoptsTheNodeThatAppeared` |
| U-401 | No backend node for the worker: the queue takes its slot as before                   | Negative   | `TestANodeWithNoBackendNodeStillTakesItsSlot`       |
| U-402 | A slot is held while the control plane still reports the node in_creation            | Regression | `TestASlotIsHeldUntilTheAddIsFinished`              |
| U-403 | The slot goes back once the node leaves in_creation                                  | Positive   | `TestTheSlotGoesBackWhenTheNodeLeavesCreation`      |
| U-404 | Every enrolled worker has a per-node entry, across a node set that grows mid-pass    | Regression | `TestEveryEnrolledWorkerHasAnEntry`                 |
| U-405 | A node stating no journal count leaves ha_jm_count to the control plane              | Regression | `TestAnUnstatedJournalCountIsLeftToTheControlPlane` |
| U-406 | An unstated count is absent from the request rather than sent as zero                | Boundary   | `TestAnUnstatedJournalCountIsNotOnTheWire`          |
| U-407 | A stated journal count is sent as it stands                                          | Positive   | `TestAStatedJournalCountIsSent`                     |

### Entity: The Provisioning Claim (design §4.2)

File: `operator/internal/controllers/node/provisioning_test.go`

The claim is the transition into `Posting`, written as an optimistic-lock patch so
that two objects for one worker cannot both conclude the add is theirs to make.

| #     | Scenario                                                                              | Type     | Test                                                      |
|-------|---------------------------------------------------------------------------------------|----------|-----------------------------------------------------------|
| U-28  | The transition into `Posting` is persisted before the `POST` is issued                | Positive | —                                                         |
| U-29  | A second reconciler at the same `resourceVersion`: 409, backs off, issues no `POST`   | Negative | —                                                         |
| U-30  | A sibling socket already at `Posting`: this object enters `Resolving` without posting | Negative | —                                                         |
| U-31  | A sibling socket at `Resolving`: this object enters `Resolving` without posting       | Negative | —                                                         |
| U-32  | The `POST` returns 5xx: the step stays `Posting` and is retried                       | Negative | —                                                         |
| U-33  | The `POST` returns 4xx: the step stays `Posting`, and the body is in the event        | Negative | —                                                         |
| U-34  | The `POST` times out: the step is not advanced and no second `POST` is issued         | Negative | —                                                         |
| U-35  | The `POST` succeeds: the step advances to `Resolving`                                 | Positive | `TestPostingIssuesTheAddOnce`                             |
| U-268 | The add carries the node's own images, memory, and journal settings                   | Positive | `TestTheAddCarriesWhatTheNodeSaysAboutItself`             |
| U-269 | A node stating no journal settings is added with the documented defaults              | Boundary | `TestAnUnstatedJournalIsTheDefaultRatherThanNothing`      |
| U-270 | A fault group that is a number is sent as one, and a named group is not sent          | Boundary | `TestOnlyAFaultGroupThatIsANumberIsSentToTheControlPlane` |
| U-271 | A step no provisioning path declares: refused rather than stalled                     | Negative | `TestAStepNoProvisioningPathDeclaresIsRefused`            |

### Entity: UUID Resolution and Adoption (design §4.2, §4.3)

Files: `operator/internal/controllers/node/provisioning_test.go`,
`resolve_retry_test.go`

| #        | Scenario                                                                           | Type     | Test                                                |
|----------|------------------------------------------------------------------------------------|----------|-----------------------------------------------------|
| U-36     | The worker's internal IP is resolved from the Kubernetes `Node`                    | Positive | —                                                   |
| U-37     | The Kubernetes `Node` carries no internal address: held, not failed                | Negative | —                                                   |
| U-38     | One backend node at the worker's IP: matched to `slot` 0                           | Positive | `TestANodeAlreadyAtTheWorkersAddressIsAdopted`      |
| U-39     | Two backend nodes at one IP: sorted by RPC port and matched by `slot`              | Positive | —                                                   |
| U-40     | `slot` beyond the number of backend nodes at the IP: held, not matched             | Boundary | —                                                   |
| U-41     | No backend node at the worker's IP: `Resolving` holds                              | Negative | `TestResolvingWaitsWhileTheAddIsStillRunning`       |
| U-42     | The control plane errors during resolution: held, and the step is not advanced     | Negative | —                                                   |
| U-43     | The `Resolving` deadline expires: the node is marked failed rather than polling on | Boundary | —                                                   |
| U-44     | An upgrade Secret is present: the node adopts without a `POST`                     | Positive | `TestAnUpgradeAdoptionDivertsBeforeTheHostIsProbed` |
| ~~U-45~~ | ~~An upgrade Secret is present but empty: falls through to the normal path~~       | Negative | Superseded by U-272                                 |
| U-46     | A backend node already exists at the worker's IP: adopted rather than added        | Positive | `TestANodeAlreadyAtTheWorkersAddressIsAdopted`      |
| U-47     | Adoption records the backend UUID and leaves `status.phase` at `Online`            | Positive | —                                                   |
| U-272    | The upgrade Secret's presence is the whole signal, whatever it holds               | Boundary | `TestAnUpgradeAdoptionDivertsBeforeTheHostIsProbed` |
| U-273    | The `node_add` left the task window without producing a node: the add is reissued  | Negative | `TestResolvingAsksAgainWhenTheAddIsOver`            |
| U-274    | No `node_add` in the window at all: the add is reissued rather than waited out     | Boundary | `TestResolvingAsksAgainWhenNoAddIsInTheWindow`      |
| U-275    | An unrelated task in the window does not hold `Resolving` open                     | Negative | `TestAnUnrelatedRunningTaskDoesNotHoldResolving`    |

### Entity: Steady-State Sync (design §4.4)

File: `operator/internal/controllers/node/syncstatus_test.go`

| #     | Scenario                                                                       | Type     | Test                                                          |
|-------|--------------------------------------------------------------------------------|----------|---------------------------------------------------------------|
| U-48  | A streamed node object writes `status.status`, `health`, and the resources     | Positive | `TestAPushedNodeIsReadFromTheStreamRatherThanAskedFor`        |
| U-49  | Nothing changed since the last pass: the reconcile issues no status patch      | Negative | `TestAPassThatFoundNothingNewWritesNothing`                   |
| U-50  | The control-plane assigned fault group differs from the requested one          | Positive | —                                                             |
| U-51  | A malformed streamed object: an error rather than a nil dereference            | Negative | —                                                             |
| U-52  | `status.observedGeneration` matches `metadata.generation` after a sync         | Positive | —                                                             |
| U-236 | A node reporting 3 of 4 devices online: `devices.online` 3, `devices.total` 4  | Positive | —                                                             |
| U-237 | A node whose devices have not been reported: `devices` absent, not `{0, 0}`    | Boundary | —                                                             |
| U-238 | A node with zero devices online: `devices.online` is 0 and present             | Boundary | —                                                             |
| U-249 | The first capacity sample: `resources.capacity` written with its `sampledAt`   | Positive | `TestAFirstCapacityReadingIsAlwaysWritten`                    |
| U-250 | A sample under the one-percent threshold: the reconcile issues no patch        | Negative | `TestASampleIsWrittenOnlyWhenItSaysSomethingNew`              |
| U-251 | A sample over it: the new used size and the new sample time are written        | Positive | `TestASampleIsWrittenOnlyWhenItSaysSomethingNew`              |
| U-252 | The total changed because a device joined: written whatever the used delta is  | Boundary | `TestASampleIsWrittenOnlyWhenItSaysSomethingNew`              |
| U-253 | The capacity source is unreachable: the node is published without `capacity`   | Negative | `TestAFailingCapacitySourceStillLeavesTheNodePublished`       |
| U-254 | A node the exporter has never measured: `capacity` absent rather than zeros    | Boundary | `TestANodeNobodySampledCarriesNoCapacity`                     |
| U-276 | A node the stream has delivered costs no control-plane request to read back    | Positive | `TestAPushedNodeIsReadFromTheStreamRatherThanAskedFor`        |
| U-277 | A node the stream has not delivered is read from the control plane instead     | Boundary | `TestANodeTheStreamHasNotDeliveredIsAskedFor`                 |
| U-278 | Each control-plane status maps to the phase this operator publishes            | Positive | `TestThePhaseIsThisOperatorsReadingOfWhatTheControlPlaneSays` |
| U-279 | A status this operator has never heard of: `Failed` rather than a guess        | Negative | `TestThePhaseIsThisOperatorsReadingOfWhatTheControlPlaneSays` |
| U-280 | A stored UUID the control plane has forgotten: the object goes back to Pending | Boundary | `TestANodeTheControlPlaneHasForgottenGoesBackToProvisioning`  |
| U-281 | A reading that did move is written rather than held back by the threshold      | Positive | `TestAPassThatFoundSomethingNewWritesIt`                      |

### Entity: Deletion (design §4.5)

File: `operator/internal/controllers/node/syncstatus_test.go`

| #        | Scenario                                                                         | Type       | Test                                                |
|----------|----------------------------------------------------------------------------------|------------|-----------------------------------------------------|
| U-53     | A node that never got a UUID: the finalizer is removed with no operation raised  | Boundary   | `TestANodeThatWasNeverProvisionedIsDeletedOutright` |
| U-54     | An online node deleted: a `Remove` operation is raised and owned by the node     | Positive   | `TestANodeWithDataOnItIsDrainedBeforeItGoes`        |
| U-55     | A second reconcile while the drain runs: no second operation is created          | Negative   | —                                                   |
| ~~U-56~~ | ~~`status.activeOpsRef` still set: the finalizer is held~~                       | Negative   | Superseded by U-282                                 |
| U-57     | The raised operation failed: the finalizer is held rather than force-removed     | Negative   | —                                                   |
| U-58     | A suspended node deleted: a `Remove` operation is still raised                   | Positive   | —                                                   |
| ~~U-59~~ | ~~An offline node deleted: no operation is raised and the finalizer is removed~~ | Boundary   | Superseded by U-282                                 |
| U-282    | A drain that has not reached a terminal phase holds the finalizer                | Regression | `TestANodeWithDataOnItIsDrainedBeforeItGoes`        |
| U-283    | The drain finished: the finalizer is released and the object goes                | Positive   | `TestANodeWithDataOnItIsDrainedBeforeItGoes`        |
| U-284    | A cordoned worker raises a `HostMaintenance` operation the node owns             | Positive   | `TestACordonedWorkerRaisesItsMaintenanceWindow`     |
| U-285    | A second pass finds the window it raised rather than raising another             | Negative   | `TestACordonedWorkerRaisesItsMaintenanceWindow`     |

`U-282` is the regression row for
`2026-09-17-node-teardown-reads-an-unheld-lock-as-a-finished-drain`: the teardown
held the finalizer while `status.activeOpsRef` was set, and a drain raised one
line earlier has not taken that lock yet, so the first pass read "not started" as
"finished" and deleted the object while its backend node was still running.
`U-56` and `U-59` are struck because the lock is no longer the signal and a node
with a UUID always gets its drain, whatever the control plane reports about it.

### Entity: The Admission Guard (design §3.1, §3.2, §3.4)

File: `operator/internal/webhook/storagenode_validator_test.go`

| #     | Scenario                                                                            | Type     | Test                                                |
|-------|-------------------------------------------------------------------------------------|----------|-----------------------------------------------------|
| U-60  | A user changing `spec.workerNode`: denied with the migration hint                   | Negative | `TestTheOperatorOnlyFieldsAreRefusedToEveryoneElse` |
| U-61  | The operator's service account changing `spec.workerNode`: allowed                  | Positive | `TestTheOperatorOnlyFieldsAreRefusedToEveryoneElse` |
| U-62  | An update that does not touch `spec.workerNode`: allowed without inspection         | Negative | `TestAnUpdateTouchingNoGuardedFieldIsAdmitted`      |
| U-63  | A create rather than an update: allowed, since there is no old value                | Boundary | `TestTheOperatorMayCreateANodeSizedAgainstTheFleet` |
| U-64  | A service account in another namespace named like the operator's: denied            | Negative | —                                                   |
| U-239 | A user changing `spec.config.pcieAllowList`: denied                                 | Negative | `TestTheOperatorOnlyFieldsAreRefusedToEveryoneElse` |
| U-240 | The operator merging `newSsdPcie` into `spec.config.pcieAllowList`: allowed         | Positive | `TestTheOperatorOnlyFieldsAreRefusedToEveryoneElse` |
| U-241 | A user changing `spec.config.sizing.vcpuCount`: denied                              | Negative | `TestTheOperatorOnlyFieldsAreRefusedToEveryoneElse` |
| U-242 | The operator re-sizing `spec.config.sizing`: allowed                                | Positive | `TestTheOperatorOnlyFieldsAreRefusedToEveryoneElse` |
| U-243 | An update touching none of the guarded fields: admitted without inspection          | Negative | `TestAnUpdateTouchingNoGuardedFieldIsAdmitted`      |
| U-255 | A `config.deviceNames` entry that is a path on an `NVMe` cluster: denied            | Negative | `TestDeviceNamesMustBeOfTheClusterSClass`           |
| U-256 | A `config.deviceNames` of PCI addresses on an `NVMe` cluster: admitted              | Positive | `TestDeviceNamesOfTheClusterSClassAreAdmitted`      |
| U-257 | A `config.deviceNames` entry that is an address on a `LogicalBlock` cluster: denied | Negative | `TestDeviceNamesMustBeOfTheClusterSClass`           |
| U-258 | A list holding an address and a path: denied whichever class the cluster is         | Negative | `TestDeviceNamesMustBeOfTheClusterSClass`           |
| U-259 | A bare device name: read as a path and classed as block, not as unknown             | Boundary | `TestDeviceNamesOfTheClusterSClassAreAdmitted`      |
| U-260 | `config.pcieDenyList` set on a `LogicalBlock` cluster: denied                       | Negative | `TestThePCIFiltersAreRefusedOnALogicalBlockCluster` |
| U-261 | `config.pcieDenyList` set on an `NVMe` cluster: admitted                            | Positive | `TestThePCIFiltersAreRefusedOnALogicalBlockCluster` |
| U-286 | A cluster stating no device class is read as `NVMe`                                 | Boundary | `TestAClusterWithNoStatedClassIsNVMe`               |
| U-287 | A node naming no `StorageCluster`: refused at admission                             | Negative | `TestANodeNamingNoClusterIsRefused`                 |
| U-305 | A user's node whose sizing differs from the fleet's: refused at create              | Negative | `TestAUserSNodeMustAgreeWithTheFleetSSizing`        |
| U-306 | The operator's node sized against the fleet mid-roll: admitted                      | Positive | `TestTheOperatorMayCreateANodeSizedAgainstTheFleet` |

### Operation: The Deletion Guard (design §7.4)

File: `operator/internal/webhook/storagenodeops_validator_test.go`

| #     | Scenario                                                                        | Type     | Test                                                         |
|-------|---------------------------------------------------------------------------------|----------|--------------------------------------------------------------|
| U-288 | A delete at a step the graph declares no abort edge from: refused               | Negative | `TestStorageNodeOpsDeleteIsRefusedWhereNoAbortEdgeExists`    |
| U-289 | The refusal names what the record still owes, rather than only refusing         | Positive | `TestTheRefusalAtPromotingSaysWhatTheRecordStillOwes`        |
| U-290 | A delete at a step with an abort edge: admitted                                 | Positive | `TestStorageNodeOpsDeleteIsAllowedWhereTheAbortEdgeExists`   |
| U-291 | A delete of a terminal operation: admitted, since it is a record                | Positive | `TestStorageNodeOpsDeleteIsAllowedOnceTerminal`              |
| U-292 | A delete before the operation started: admitted                                 | Boundary | `TestStorageNodeOpsDeleteIsAllowedBeforeTheOperationStarted` |
| U-293 | A delete with no object to read: admitted rather than failing closed on nothing | Boundary | `TestStorageNodeOpsDeleteIsAllowedWithNoObjectToRead`        |
| U-294 | A create is not this guard's business                                           | Negative | `TestStorageNodeOpsCreateIsNotThisGuardsBusiness`            |
| U-295 | The refusal table and the graph's own unabortable steps agree                   | Positive | `TestTheNodeRefusalTableAndTheGraphAgree`                    |
| U-296 | Every undeletable step states what it is doing, so a refusal is actionable      | Positive | `TestEveryUndeletableNodeStepStatesWhatItIsDoing`            |

### Entity: Conversion to and from `v1alpha1` (design §15.1)

File: `operator/api/v1alpha1/storagenode_conversion_test.go`

| #     | Scenario                                                                        | Type       | Test                                                       |
|-------|---------------------------------------------------------------------------------|------------|------------------------------------------------------------|
| U-297 | `spec.clusterRef` is read from the controller owner the stored object carries   | Positive   | `TestStorageNodeReadsItsClusterFromTheControllerOwner`     |
| U-298 | An object with no controller owner converts with no cluster rather than failing | Boundary   | `TestStorageNodeWithNoControllerConvertsWithNoCluster`     |
| U-299 | The device summary is read in the order it was written, not the documented one  | Regression | `TestStorageNodeDeviceSummaryIsReadInTheOrderItWasWritten` |
| U-300 | An unparsable device summary produces no device block rather than zeros         | Boundary   | `TestStorageNodeUnparsableDeviceSummaryHasNoBlock`         |
| U-301 | A failure-domain label survives the trip down to the retired version            | Positive   | `TestStorageNodeFailureDomainLabelSurvivesTheTripDown`     |
| U-302 | A failure-domain index becomes its digits on the way up                         | Positive   | `TestStorageNodeFailureDomainIndexBecomesItsDigits`        |
| U-303 | The four per-node fields that reach nothing survive the round trip              | Boundary   | `TestStorageNodeDeadPerNodeFieldsSurviveTheRoundTrip`      |
| U-304 | The sizing block survives the trip down                                         | Positive   | `TestStorageNodeSizingSurvivesTheTripDown`                 |

### Workload: DaemonSet, Services, and RBAC (design §5.1)

Files: `operator/internal/controllers/node/rotation_test.go`,
`enrollment_test.go`, `operator/internal/utils/storage_node_workload_test.go`

The objects belong to the `StorageCluster` now, and the reconcile that writes them
is `StorageNodeWorkloadReconciler`. Rows that named the retired `StorageNodeSet`
name the cluster instead.

| #     | Scenario                                                                                                  | Type       | Test                                                             |
|-------|-----------------------------------------------------------------------------------------------------------|------------|------------------------------------------------------------------|
| U-65  | No DaemonSet present: one is created                                                                      | Positive   | `TestARotatedCertificateRollsTheStoragePods`                     |
| U-66  | A DaemonSet present: it is updated in place rather than recreated                                         | Positive   | `TestARotatedCertificateRollsTheStoragePods`                     |
| U-67  | TLS disabled: the pod template carries no serving-certificate mount                                       | Negative   | —                                                                |
| U-68  | TLS enabled: the pod template mounts the serving certificate                                              | Positive   | —                                                                |
| U-69  | The cert-manager provider: the Certificate is created alongside                                           | Positive   | —                                                                |
| U-70  | User-supplied container resources override the defaults                                                   | Positive   | `TestBuildStorageNodeDaemonSetUserResourcesOverrideDefaults`     |
| U-71  | The ServiceAccount carries an owner reference to its parent                                               | Positive   | —                                                                |
| U-72  | ClusterRoleBinding names include the namespace, so two namespaces do not collide                          | Positive   | `TestBuildStorageNodeSetClusterRoleBindingNameIncludesNamespace` |
| U-73  | Namespace-specific ClusterRoleBindings are created                                                        | Positive   | —                                                                |
| U-74  | The SPDK proxy Service is created                                                                         | Positive   | —                                                                |
| U-75  | Every workload object carries an owner reference to the `StorageCluster`                                  | Positive   | —                                                                |
| U-76  | Deleting the `StorageCluster` cascades to every workload object                                           | Positive   | —                                                                |
| U-264 | No image on the cluster: the image is taken from the singleton `ControlPlane`, read at the stored version | Regression | —                                                                |
| U-307 | Neither the cluster nor the `ControlPlane` states an image: the write is refused                          | Negative   | `TestAWorkloadWithNoImageAnywhereIsRefused`                      |
| U-308 | The config generator mounts `/dev` and `/sys`, which the init container reads                             | Positive   | `TestBuildStorageNodeDaemonSetConfigGeneratorMountsDevAndSys`    |
| U-309 | A conflict on the DaemonSet write is retried against a fresh read                                         | Regression | `TestTheDaemonSetWriteRetriesAConflict`                          |

### Workload: Storage-Plane Labels (design §5.2)

File: `operator/internal/controllers/node/enrollment_test.go`

| #    | Scenario                                                                        | Type     | Test                                            |
|------|---------------------------------------------------------------------------------|----------|-------------------------------------------------|
| U-77 | Workers gain the cluster's storage-plane label                                  | Positive | `TestTheWorkloadEnrollsEveryWorkerItHasANodeOn` |
| U-78 | A node that has come online gets its per-slot storage-node-uuid label           | Positive | —                                               |
| U-79 | A node with no UUID yet contributes no slot label                               | Negative | —                                               |
| U-80 | The slot label key is stable across a UUID change, and only the value moves     | Positive | —                                               |
| U-81 | A worker hosting nodes of two clusters carries two non-colliding slot keys      | Positive | —                                               |
| U-82 | A stale slot label whose node is gone is removed                                | Positive | —                                               |
| U-83 | The node `List` fails: the reconcile aborts and deletes no label                | Negative | —                                               |
| U-84 | A worker with no storage node: its slot labels are removed, the others are kept | Boundary | —                                               |

### Workload: Per-Node Configuration (design §5.3)

File: `operator/internal/controllers/node/pernodeconfig_test.go`

| #     | Scenario                                                                                   | Type     | Test                                                   |
|-------|--------------------------------------------------------------------------------------------|----------|--------------------------------------------------------|
| U-85  | Cluster sizing values appear in a worker's env file                                        | Positive | —                                                      |
| U-86  | Cluster sizing is identical in every worker's entry                                        | Positive | —                                                      |
| U-87  | Every key the init container reads is present, including the empty ones                    | Boundary | —                                                      |
| U-88  | The cluster is missing its required sizing: the write is refused with a named error        | Negative | —                                                      |
| U-89  | Every worker gets an entry carrying the cluster sizing                                     | Positive | —                                                      |
| U-90  | Two nodes with different device filters get different entries                              | Positive | —                                                      |
| U-91  | A node with no `spec.config`: its entry carries the empty values, not missing keys         | Boundary | —                                                      |
| U-92  | A device list containing a shell metacharacter is quoted rather than interpolated          | Negative | —                                                      |
| U-262 | `VCPU_COUNT` and `MAX_HUGE_PAGES_SIZE` come from the node's own sizing                     | Positive | —                                                      |
| U-263 | Two nodes mid-roll: their entries differ in those two keys and agree on `MAX_SUBSYS_COUNT` | Boundary | —                                                      |
| U-244 | A `deviceNames` entry that is a PCI address reaches the node as one                        | Positive | —                                                      |
| U-245 | A `deviceNames` entry that is a device path reaches the node as one                        | Positive | —                                                      |
| U-246 | A mixed `deviceNames` list: both forms reach the node, in the order given                  | Boundary | —                                                      |
| U-247 | `deviceNames` set alongside `pcieAllowList`: the explicit list wins (design §3.1)          | Boundary | —                                                      |
| U-93  | The ConfigMap is written before the DaemonSet on a fresh reconcile                         | Positive | —                                                      |
| U-94  | A node deleted: its entry is removed from the ConfigMap                                    | Positive | —                                                      |
| U-310 | A relocation clones the source worker's entry onto the target                              | Positive | `TestTheTargetInheritsTheSourcesEntryWithTheNewDrives` |
| U-311 | Cloning from a worker that has no entry is refused rather than writing an empty one        | Negative | `TestCloningFromAWorkerWithNoEntryIsRefused`           |
| U-312 | Cloning before the ConfigMap exists is not a failure: the ordinary pass writes it          | Boundary | `TestNothingToCloneFromIsNotAFailure`                  |
| U-313 | The source worker's own entry is left alone by the clone                                   | Negative | `TestTheTargetInheritsTheSourcesEntryWithTheNewDrives` |

### Workload: Endpoints and Certificate Rotation (design §5.4)

Files: `operator/internal/controllers/node/rotation_test.go`, `migrate_test.go`,
`operator/internal/utils/storage_node_workload_test.go`

| #     | Scenario                                                                           | Type     | Test                                                       |
|-------|------------------------------------------------------------------------------------|----------|------------------------------------------------------------|
| U-95  | The per-pod address is built from the worker's hostname label and the namespace    | Positive | `TestStorageNodeSetAPIAddress`                             |
| U-96  | The EndpointSlice check matches the address builder's output exactly               | Positive | `TestPreparingWaitsForTheNameToResolveAndNotOnlyForThePod` |
| U-97  | A dotted worker hostname is truncated to a valid endpoint label                    | Boundary | `TestBuildSpdkProxyEndpointSlice_DottedNodeNameTruncates`  |
| U-98  | Two workers whose first label segment collides: the build fails rather than merges | Negative | `TestBuildSpdkProxyEndpointSlice_CollidingFirstLabelFails` |
| U-99  | SPDK proxy EndpointSlices are built from the running pods                          | Positive | —                                                          |
| U-100 | Two pods sharing a first label segment: reported rather than silently merged       | Negative | —                                                          |
| U-101 | The RPC port is read from the pod's environment, falling back to its name          | Boundary | —                                                          |
| U-102 | The serving certificate's revision is stamped onto the pod template                | Positive | `TestARotatedCertificateRollsTheStoragePods`               |
| U-103 | The certificate Secret rotates: the DaemonSet rolls                                | Positive | `TestARotatedCertificateRollsTheStoragePods`               |
| U-104 | The TLS serving environment reaches the container                                  | Positive | —                                                          |
| U-105 | A Secret that is not the storage-node-api certificate: the predicate ignores it    | Negative | —                                                          |
| U-106 | The certificate Secret changes: every affected object in the namespace is enqueued | Positive | —                                                          |
| U-107 | Certificates and Services are reconciled together for the cert-manager provider    | Positive | —                                                          |
| U-314 | TLS disabled: nothing is stamped, so no pod is rolled by the absence               | Boundary | `TestWithoutTLSNothingIsStampedOnTheTemplate`              |

### Operation: Lifecycle and Lock (design §7.1, §11)

Files: `operator/internal/controllers/node/opslock_test.go`, `advance_test.go`,
`watches_test.go`, `remove_deadlock_test.go`

| #     | Scenario                                                                            | Type     | Test                                                       |
|-------|-------------------------------------------------------------------------------------|----------|------------------------------------------------------------|
| U-108 | The lock is free: it is acquired and the phase becomes `Running`                    | Positive | `TestTakingTheLockIsWhatStartsTheOperation`                |
| U-109 | Another operation holds the lock: this one stays `Pending` and requeues             | Negative | `TestAnOperationWaitsForTheOneHoldingTheNode`              |
| U-110 | An operation acquiring the lock enters its graph's initial step with a deadline     | Positive | `TestTheFirstPassArmsTheStepAMachineIsBornIn`              |
| U-111 | Success: the phase is `Succeeded` and the lock is cleared                           | Positive | `TestFinishingWritesTheOutcomeAndLetsTheNodeGo`            |
| U-112 | Failure: the phase is `Failed` with a message, and the lock is cleared              | Positive | `TestAStepThatOutlivedItsDeadlineFailsTheOperation`        |
| U-113 | A release by a non-owner: the lock is left alone                                    | Negative | `TestALateReleaseDoesNotUnlockSomebodyElsesNode`           |
| U-114 | Advancing persists the next step and its deadline before the side effect            | Positive | `TestAFinishedStepEntersTheNextWithItsOwnDeadline`         |
| U-115 | An unknown action: the operation fails terminally with the reason in the message    | Negative | `TestAStepThatBelongsToNoActionEndsTheOperation`           |
| U-116 | The target node does not exist: the operation fails with a not-found message        | Negative | `TestAnOperationAgainstAMissingNodeFails`                  |
| U-117 | A terminal operation re-reconciled: no side effect, and the lock is released again  | Negative | `TestATerminalOperationStillReleasesALockItLeftBehind`     |
| U-118 | Two reconcilers acquiring one free lock: the loser gets 409 and requeues            | Negative | —                                                          |
| U-119 | The operation is deleted while `Running`: the finalizer releases the lock           | Positive | `TestDeletingAnOperationUnlocksTheNodeFirst`               |
| U-120 | Operations on two different nodes run without contending                            | Positive | —                                                          |
| U-121 | The cluster is not active: the operation holds and emits `ClusterNotReady`          | Negative | —                                                          |
| U-122 | The cluster is rebalancing: the operation holds rather than proceeding              | Negative | `TestAnOperationHoldsWhileItsClusterIsRebalancing`         |
| U-123 | The cluster becomes active: the held operation resumes with no further input        | Positive | —                                                          |
| U-124 | A node event wakes a queued operation before its requeue interval elapses           | Positive | `TestANodeEventWakesTheOperationsWaitingOnIt`              |
| U-315 | The holder re-reading its own lock keeps it, since every pass goes through this     | Boundary | `TestTheHolderKeepsItsOwnLock`                             |
| U-316 | The finalizer is taken on the pass before anything is locked                        | Positive | `TestAnOperationTakesItsFinalizerBeforeItTakesAnything`    |
| U-317 | A removal runs whatever the cluster says, because removal is how a cluster recovers | Boundary | `TestRemovalDoesNotWaitOnTheCluster`                       |
| U-318 | Every other action waits on the cluster gate                                        | Negative | `TestEveryOtherActionWaitsOnTheCluster`                    |
| U-319 | A removal proceeds against a rebalancing cluster rather than holding                | Boundary | `TestARemovalRunsAgainstARebalancingCluster`               |
| U-320 | `status.observedGeneration` advances, which is how an observed abort is visible     | Positive | `TestTheObservedGenerationMovesWhenTheOperationIsLookedAt` |
| U-321 | A cluster event wakes its own nodes and nobody else's                               | Positive | `TestAClusterEventWakesItsOwnNodes`                        |
| U-322 | A worker event wakes the nodes that run on it, which is how a cordon arrives        | Positive | `TestAWorkerEventWakesTheNodesOnIt`                        |

### Operation: The Single-Step Actions (design §7.3)

File: `operator/internal/controllers/node/actions_test.go`

| #     | Scenario                                                                         | Type     | Test                                                     |
|-------|----------------------------------------------------------------------------------|----------|----------------------------------------------------------|
| U-125 | `Suspend`: the call is issued, and the step completes when the node is suspended | Positive | `TestACallIsIssuedWhenTheNodeIsNotThereYet`              |
| U-126 | `Resume`: the step completes when the node is online                             | Positive | `TestTheWaitIsOverWhenTheNodeReportsWhatTheActionWasFor` |
| U-127 | `Shutdown`: the step completes when the node is offline                          | Positive | `TestTheWaitIsOverWhenTheNodeReportsWhatTheActionWasFor` |
| U-128 | `Restart`: `reattachVolume` and `force` are passed through when set              | Positive | `TestOnlyTheFlagsTheOperationStatesAreSent`              |
| U-129 | The node is already at the target state: the call is not issued at all           | Negative | `TestACallIsSkippedWhenTheNodeIsAlreadyThere`            |
| U-130 | The call returns 5xx: the step is retried and the phase does not advance         | Negative | —                                                        |
| U-131 | The call returns 4xx: the step is retried, and the body reaches the event        | Negative | —                                                        |
| U-132 | The call is retried after a timeout: the endpoint is called at most once more    | Negative | —                                                        |
| U-133 | The node never reaches the target state: the step's deadline expires and fails   | Boundary | `TestAStepThatOutlivedItsDeadlineFailsTheOperation`      |
| U-134 | A late response after the deadline expired: ignored, no second commit            | Negative | —                                                        |
| U-323 | A restart has no state to skip on, so it is issued against an online node        | Boundary | `TestARestartIsIssuedAgainstAnOnlineNode`                |
| U-324 | An unstated flag is not sent, since not sending is not the same as sending false | Boundary | `TestOnlyTheFlagsTheOperationStatesAreSent`              |
| U-325 | An operation against a node with no backend UUID: terminal, not retried          | Negative | `TestAnUnprovisionedNodeEndsTheOperation`                |

### Operation: Volume Classification (design §8.1)

File: `operator/internal/controllers/node/classify_test.go`

| #     | Scenario                                                                                   | Type     | Test                                                 |
|-------|--------------------------------------------------------------------------------------------|----------|------------------------------------------------------|
| U-135 | A volume matching a `PersistentVolume`: classified as PV-managed                           | Positive | `TestAVolumeKubernetesAccountsForIsMovable`          |
| U-136 | A volume whose claim carries the pin annotation: classified as pinned                      | Negative | `TestAPinnedVolumeBlocksRatherThanMoving`            |
| U-137 | A volume matching no `PersistentVolume`: classified as unmanaged                           | Negative | `TestAVolumeNothingAccountsForBlocks`                |
| U-138 | A volume matching the system filter: excluded from every bucket                            | Negative | `TestABenchmarkVolumeIsTheDrainsToDelete`            |
| U-139 | A node with no volumes: no drain work is produced and no error is returned                 | Boundary | `TestADrainIsDoneWhenTheNodeHoldsNothingMovable`     |
| U-140 | A node holding only system volumes: nothing is migrated                                    | Boundary | `TestABenchmarkVolumeIsTheDrainsToDelete`            |
| U-141 | The default system filter matches the rebalancer's benchmark volume names                  | Positive | `TestTheDefaultFilterMatchesTheBenchmarkVolumesOnly` |
| U-142 | A custom `systemVolumeFilterRegex` replaces the default                                    | Positive | `TestAStatedFilterReplacesTheDefault`                |
| U-143 | A malformed `systemVolumeFilterRegex`: the operation fails with the parse error            | Negative | `TestAPatternThatDoesNotCompileEndsTheOperation`     |
| U-144 | A volume that is both pinned and unmanaged: both blockers are reported                     | Boundary | —                                                    |
| U-145 | A claim in another namespace with the same name: not read as this one's pin                | Negative | —                                                    |
| U-146 | An empty `systemVolumeFilterRegex`: falls back to the default rather than matching nothing | Boundary | —                                                    |
| U-326 | A claim that cannot be read: counted where it blocks, and the census says so               | Negative | `TestAClaimThatCannotBeReadMakesTheCensusIncomplete` |
| U-327 | A volume whose delete the backend has accepted is not counted                              | Boundary | `TestAVolumeAlreadyBeingDeletedIsNotCounted`         |
| U-328 | A peer's volumes in the same pools are not this node's to move                             | Negative | `TestAPeersVolumesAreNotThisNodesToMove`             |
| U-329 | A `PersistentVolume` of another driver accounts for nothing                                | Negative | `TestAnotherDriversVolumeAccountsForNothing`         |
| U-330 | A `PersistentVolume` with no claim cannot be pinned, so it is movable                      | Boundary | `TestAnUnclaimedVolumeIsMovable`                     |
| U-331 | A volume handle naming no volume indexes nothing                                           | Boundary | `TestAnEmptyVolumeHandleIndexesNothing`              |

### Operation: The Remove Graph (design §8.2, §8.3)

Files: `operator/internal/controllers/node/drain_test.go`, `peertargets_test.go`,
`advance_test.go`, `remove_gone_test.go`

| #     | Scenario                                                                                 | Type       | Test                                                                                   |
|-------|------------------------------------------------------------------------------------------|------------|----------------------------------------------------------------------------------------|
| U-147 | Validation clear: the step advances to `Suspending`                                      | Positive   | `TestValidationWritesTheTotalTheDrainIsMeasuredAgainst`                                |
| U-148 | Pinned volumes present: the step holds and no suspend is issued                          | Negative   | `TestAPinnedVolumeStopsTheDrainBeforeItSuspendsAnything`                               |
| U-149 | Unmanaged volumes present: the step holds and no suspend is issued                       | Negative   | `TestAnUnmanagedVolumeStopsTheDrain`                                                   |
| U-150 | The pin is removed: the next reconcile advances without further input                    | Positive   | —                                                                                      |
| U-151 | The node is already suspended: no suspend call is issued and the step advances           | Negative   | `TestTheSuspendIsSkippedWhenTheNodeIsAlreadyOutOfService`                              |
| U-152 | Migration targets are spread evenly across the online peers                              | Positive   | `TestTheVolumesAreSpreadOverEveryOnlinePeer`                                           |
| U-153 | No online peer: the step holds and emits `NoMigrationTarget`                             | Negative   | `TestADrainWithNowhereToMoveToHolds`                                                   |
| U-154 | Offline peers are excluded from target selection                                         | Negative   | `TestAnOfflinePeerIsNotATarget`                                                        |
| U-155 | Exactly one online peer: every volume goes to it                                         | Boundary   | —                                                                                      |
| U-156 | Every migration completed: the step advances to `Verifying`                              | Positive   | `TestADrainIsDoneWhenTheNodeHoldsNothingMovable`                                       |
| U-157 | Verification finds non-system volumes: the step holds and retries                        | Negative   | `TestVerificationHoldsWhileAUsersVolumeIsStillThere`                                   |
| U-158 | Verification finds only system volumes: they are deleted, then the step advances         | Positive   | `TestVerificationDeletesTheBenchmarkVolumesAndRereads`                                 |
| U-159 | A system-volume delete returns 404: treated as success                                   | Boundary   | —                                                                                      |
| U-160 | A system-volume delete is rejected: the node is resumed and the operation fails          | Negative   | `TestABenchmarkVolumeThatCannotBeDeletedEndsTheDrain`                                  |
| U-161 | The node delete returns 200, 204, or 404: the operation succeeds                         | Boundary   | `TestAnAcceptedRemovalFinishesTheDrain`                                                |
| U-162 | The node delete returns 5xx: retried, and the operation does not fail                    | Negative   | —                                                                                      |
| U-163 | The node delete is rejected: the node is resumed and the operation fails                 | Negative   | `TestARefusedRemovalEndsTheDrain`, `TestAStepThatOutlivedItsDeadlineFailsTheOperation` |
| U-164 | A resume that itself fails: the operation still reaches `Failed`, with an event          | Negative   | —                                                                                      |
| U-165 | `spec.abort` set during `Validating`: `Aborted` with no resume call issued               | Boundary   | —                                                                                      |
| U-166 | `spec.abort` set during `MigratingVolumes`: migrations deleted, node resumed             | Positive   | `TestAnAbortAtAnAbortableStepStopsAndResumesTheNode`                                   |
| U-167 | A step's deadline expires mid-drain: the node is resumed and the operation fails         | Boundary   | `TestAStepThatOutlivedItsDeadlineFailsTheOperation`                                    |
| U-168 | `status.drain.volumesTotal` is written once and not recomputed on later passes           | Positive   | `TestValidationWritesTheTotalTheDrainIsMeasuredAgainst`                                |
| U-169 | A drain with zero volumes: `volumesTotal` is 0 and the step advances immediately         | Boundary   | `TestADrainIsDoneWhenTheNodeHoldsNothingMovable`                                       |
| U-332 | A node the control plane has forgotten: every step of the removal is done                | Regression | `TestARemovalOfAGoneNodeIsDoneAtEveryStep`                                             |
| U-333 | An incomplete census is retried rather than reported as a blocker                        | Negative   | `TestAnIncompleteCensusIsRetriedRatherThanReportedAsABlocker`                          |
| U-334 | A node already offline is past the state a suspend produces, so none is issued           | Boundary   | `TestTheSuspendIsSkippedWhenTheNodeIsAlreadyOutOfService`                              |
| U-335 | An online node is suspended, and the step waits for the backend to report it             | Positive   | `TestAnOnlineNodeIsSuspendedAndWaitedFor`                                              |
| U-336 | An empty node passes verification                                                        | Boundary   | `TestAnEmptyNodePassesVerification`                                                    |
| U-337 | A suspended peer is not a migration target either                                        | Negative   | `TestASuspendedPeerIsNotATarget`                                                       |
| U-338 | The drained node is never chosen as its own target                                       | Negative   | `TestTheDrainedNodeIsNeverItsOwnTarget`                                                |
| U-339 | The peer assignment is the same on every pass, so a retry is deliberate                  | Positive   | `TestTheAssignmentIsTheSameOnEveryPass`                                                |
| U-340 | The peers come from the stream once it has delivered, and the control plane is not asked | Positive   | `TestThePeersComeFromTheStreamOnceItHasDelivered`                                      |
| U-341 | A stream that has not delivered falls back to the control plane                          | Boundary   | `TestAnUndeliveredStreamFallsBackToTheControlPlane`                                    |

### Operation: The Fan-Out (design §8.4)

Files: `operator/internal/controllers/node/drain_test.go`, `remove_fanout_test.go`

| #     | Scenario                                                                                                 | Type       | Test                                                      |
|-------|----------------------------------------------------------------------------------------------------------|------------|-----------------------------------------------------------|
| U-170 | Generated migration names are valid DNS labels                                                           | Positive   | `TestEveryMovableVolumeIsGivenAMove`                      |
| U-171 | Two long volume names sharing a prefix produce distinct migration names                                  | Boundary   | —                                                         |
| U-172 | An operator restart mid-drain does not recreate existing migration objects                               | Negative   | `TestAVolumeAlreadyMovingIsNotGivenASecondMove`           |
| U-173 | A completed migration is deleted and the counter is written first                                        | Positive   | `TestFinishedMovesAreRecordedAndThenReaped`               |
| U-174 | A failed migration is deleted and replaced against a fresh target                                        | Positive   | `TestAFailedMoveIsRetriedRatherThanFailingTheDrain`       |
| U-175 | A migration deleted out of band is recreated rather than counted as complete                             | Negative   | `TestEveryMovableVolumeIsGivenAMove`                      |
| U-176 | Every migration carries `spec.creatorRef` with the operation's UID                                       | Positive   | `TestTheFanOutRecordsItsCreatorWithoutOwningTheOperation` |
| U-177 | The same volume name in two namespaces produces two distinct objects                                     | Boundary   | —                                                         |
| U-248 | Every migration carries `storage.simplyblock.io/drain-node`, and a `List` on the label finds the fan-out | Positive   | `TestTheFanOutIsFoundAgainByItsLabel`                     |
| U-342 | The cluster-scoped kind carries no owner reference, which garbage collection would follow                | Regression | `TestTheFanOutRecordsItsCreatorWithoutOwningTheOperation` |
| U-343 | With the registered kind in use, the move is owned by the drain as it always was                         | Positive   | `TestTheLegacyFanOutIsStillOwnedByItsDrain`               |
| U-344 | Deleting a drain reaps the fan-out it raised                                                             | Positive   | `TestDeletingADrainStopsWhatItFannedOut`                  |
| U-345 | A move still copying holds the deletion rather than being reaped mid-copy                                | Negative   | `TestADrainBeingDeletedWaitsForAMoveStillRunning`         |
| U-346 | A retried move is announced, so a drain that keeps retrying is not read as stalled                       | Positive   | `TestAFailedMoveIsRetriedRatherThanFailingTheDrain`       |

### Operation: The Migrate Graph (design §9)

Files: `operator/internal/controllers/node/migrate_test.go`,
`pernodeconfig_test.go`

| #     | Scenario                                                                         | Type     | Test                                                          |
|-------|----------------------------------------------------------------------------------|----------|---------------------------------------------------------------|
| U-178 | The target worker's configuration is cloned from the source                      | Positive | `TestPreparingWritesTheTargetsConfigurationAndLabelsIt`       |
| U-179 | The topology re-point moves the node onto the target worker                      | Positive | `TestThePromoteIsFollowedByTheTopologyRepoint`                |
| U-180 | `newSsdPcie` addresses are merged into the effective allow list                  | Positive | `TestTheTargetInheritsTheSourcesEntryWithTheNewDrives`        |
| U-181 | Merging two PCI lists produces no duplicates                                     | Positive | `TestTheBoundDrivesJoinTheListInTheOrderItWasWritten`         |
| U-182 | A quoted shell list is parsed back into its elements                             | Positive | `TestAListIsReadBackTheWayItWasWritten`                       |
| U-183 | An empty PCI list merged with additions yields just the additions                | Boundary | `TestAnEntryWithNoAllowListGetsOne`                           |
| U-184 | The target worker does not exist: the operation fails informatively              | Negative | `TestATargetThatCannotHostTheNodeIsRefused`                   |
| U-185 | The target worker is the node's current worker: the step is already done         | Boundary | `TestARelocationToTheHostTheNodeIsOnIsAlreadyDone`            |
| U-186 | `spec.migrate` absent for `action: Migrate`: the operation is terminal           | Negative | `TestARelocationWithNoTargetEndsTheOperation`                 |
| U-187 | The target's pod is not `Ready`: `Preparing` holds and no restart is issued      | Negative | `TestPreparingWritesTheTargetsConfigurationAndLabelsIt`       |
| U-188 | The pod is `Ready` but not yet in the EndpointSlice: `Preparing` still holds     | Boundary | `TestPreparingWaitsForTheNameToResolveAndNotOnlyForThePod`    |
| U-189 | The pod is `Ready` and in the EndpointSlice: the step advances                   | Positive | `TestPreparingWaitsForTheNameToResolveAndNotOnlyForThePod`    |
| U-190 | The relocation restart is forced by default                                      | Positive | `TestTheRelocationRestartIsAimedAtTheTargetAndForced`         |
| U-191 | An explicit `spec.force: false` is honored rather than overridden                | Negative | `TestAStatedForceOutranksTheRelocationsDefault`               |
| U-192 | The node is still online after the restart call: `Relocating` holds              | Negative | `TestTheRelocationRestartIsAimedAtTheTargetAndForced`         |
| U-193 | The node has left online: the step advances to `AwaitingNode`                    | Positive | `TestTheRelocationIsOverWhenTheNodeHasLeftOnline`             |
| U-194 | The node returns online: the step advances to `Promoting`                        | Positive | `TestTheWaitForTheRelocatedNodeEndsWhenItIsBack`              |
| U-195 | The promote is issued exactly once across several reconciles                     | Negative | `TestAPromoteThatAlreadyLandedIsNotIssuedAgain`               |
| U-196 | `spec.abort` during `Preparing`: `Aborted`, and no control-plane call was made   | Positive | —                                                             |
| U-197 | `spec.abort` during `Promoting`: refused by the graph, and the operation runs on | Negative | `TestAnAbortThatArrivedTooLateIsRefusedAndTheOperationRunsOn` |
| U-198 | The topology re-point happens after the promote, never before                    | Positive | `TestThePromoteIsFollowedByTheTopologyRepoint`                |
| U-199 | The source worker loses its storage-plane labels when no node remains on it      | Positive | —                                                             |
| U-200 | The source worker keeps its labels when another node still runs there            | Boundary | —                                                             |
| U-347 | A target worker that is not `Ready`: refused rather than waited on               | Negative | `TestATargetThatCannotHostTheNodeIsRefused`                   |
| U-348 | The promote is refused while the node has not come back                          | Boundary | `TestAPromoteIsRefusedWhileTheNodeIsNotBack`                  |
| U-349 | The restart names the target's own per-pod address                               | Positive | `TestTheRelocationRestartIsAimedAtTheTargetAndForced`         |
| U-350 | A worker that has reported no readiness condition is not read as `Ready`         | Boundary | `TestAWorkerThatHasNotReportedIsNotReady`                     |
| U-351 | A migration binding no drive leaves the allow list byte for byte as it was       | Boundary | `TestAMigrationThatBindsNoDriveLeavesTheListAlone`            |

### Operation: The Host Maintenance Graph (design §10)

Files: `operator/internal/controllers/node/hostmaintenance_test.go`,
`selfbudget_test.go`, `syncstatus_test.go`

| #     | Scenario                                                                            | Type       | Test                                                     |
|-------|-------------------------------------------------------------------------------------|------------|----------------------------------------------------------|
| U-201 | A worker becomes unschedulable: a `HostMaintenance` operation is raised             | Positive   | `TestACordonedWorkerRaisesItsMaintenanceWindow`          |
| U-202 | The worker is uncordoned before the operation starts: it completes as a no-op       | Negative   | —                                                        |
| U-203 | A second reconcile while one is running: no second operation is created             | Negative   | `TestACordonedWorkerRaisesItsMaintenanceWindow`          |
| U-204 | The concurrency limit is reached: the operation holds at `Holding`                  | Negative   | `TestOneWorkerAtATimeIsWhatAnUnstatedConcurrencyMeans`   |
| U-205 | Two nodes on one worker: the pair counts as one worker against the limit            | Boundary   | `TestASiblingSocketOfTheSameWorkerIsNotASecondWorker`    |
| U-206 | The limit is the cluster's effective value rather than what it asked for            | Boundary   | `TestTheClusterSaysHowManyWorkersMayBeDownAtOnce`        |
| U-207 | A blocking budget is created before the shutdown call                               | Positive   | `TestTheEvictionIsBlockedBeforeTheNodeIsTakenDown`       |
| U-208 | The node reports offline: the budget is relaxed                                     | Positive   | `TestReleasingRelaxesTheBudgetAndWaitsForThePodToGo`     |
| U-209 | The budget is relaxed only after the node is offline, never before                  | Negative   | —                                                        |
| U-210 | The storage pod is gone: the step advances to `AwaitingHost`                        | Positive   | `TestReleasingRelaxesTheBudgetAndWaitsForThePodToGo`     |
| U-211 | The worker's API answers again: the restart is issued                               | Positive   | `TestTheNodeIsRestartedOnceTheHostIsBack`                |
| U-212 | The node is already online when `Restarting` is entered: no restart call is issued  | Negative   | `TestTheNodeIsRestartedOnceTheHostIsBack`                |
| U-213 | The node comes back online: the budget and the pod label are removed                | Positive   | `TestCleanupLeavesTheWorkerDrainableAgain`               |
| U-214 | The operation fails: the budget is still removed, so the worker stays drainable     | Negative   | `TestCleanupLeavesTheWorkerDrainableAgain`               |
| U-215 | A stale operator self-budget from a previous crash is cleaned up                    | Negative   | `TestAStaleSelfBudgetIsCleanedUp`                        |
| U-216 | The `AwaitingHost` deadline expires: the operation fails and the node stays offline | Boundary   | —                                                        |
| U-352 | A window still at `Holding` occupies no slot, so a deployment cannot deadlock       | Boundary   | `TestAWindowStillWaitingHoldsNoSlot`                     |
| U-353 | A finished window occupies no slot either                                           | Boundary   | `TestAFinishedWindowHoldsNoSlot`                         |
| U-354 | A window in another cluster does not hold this one back                             | Negative   | `TestAWindowInAnotherClusterDoesNotHoldThisOneBack`      |
| U-355 | An offline node needs no shutdown, and none is issued                               | Boundary   | `TestAnOfflineNodeNeedsNoShutdown`                       |
| U-356 | A node mid-restart is waited for rather than shut down into an in-flight restart    | Boundary   | `TestANodeMidRestartIsWaitedForRatherThanShutDown`       |
| U-357 | The manager holds its own eviction on the worker it is draining                     | Positive   | `TestTheManagerHoldsItselfOnTheWorkerItIsDraining`       |
| U-358 | The manager holds nothing on somebody else's worker                                 | Negative   | `TestTheManagerDoesNotHoldItselfOnSomebodyElsesWorker`   |
| U-359 | A manager that does not know where it runs holds nothing                            | Boundary   | `TestAManagerThatDoesNotKnowWhereItRunsHoldsNothing`     |
| U-360 | Releasing the manager's own budget is what lets the drain finish                    | Positive   | `TestReleasingTheManagerIsWhatLetsTheDrainFinish`        |
| U-361 | The self-budget carries the labels its group is selected by                         | Positive   | `TestTheSelfBudgetCarriesTheGroupsLabels`                |
| U-362 | The window holds the manager before anything that can fail                          | Regression | `TestTheWindowHoldsTheManagerBeforeAnythingThatCanFail`  |
| U-363 | The window lets the manager go when it lets the storage pod go                      | Regression | `TestTheWindowLetsTheManagerGoWhenItLetsTheStoragePodGo` |
| U-364 | A step of another action reached under this one: terminal rather than stalled       | Negative   | `TestAStepOfAnotherActionEndsTheWindow`                  |

### Ops Shape: The Step Machine (design §6.3)

File: `operator/internal/controllers/node/graphs_test.go`

Seven graphs are declared as one `statemachine.MultiConfig`, and the provisioning
machine is declared beside them. The three lists that have to agree are the
graph's states, the `Enum` marker on `status.step.state`, and the CEL rule the
CRD carries.

| #     | Scenario                                                                           | Type       | Test                                                     |
|-------|------------------------------------------------------------------------------------|------------|----------------------------------------------------------|
| U-217 | Every declared graph builds, including the ones the action under test does not use | Positive   | `TestEveryActionDeclaresAGraph`                          |
| U-218 | An action with no declared graph: refused rather than stalled                      | Negative   | `TestAStepThatBelongsToNoActionEndsTheOperation`         |
| U-219 | `Remove` transitioning to `Promoting`: rejected as an illegal transition           | Negative   | `TestAStepOfAnotherActionIsRejected`                     |
| U-220 | `Migrate` transitioning to `Removing`: rejected as an illegal transition           | Negative   | `TestAStepOfAnotherActionIsRejected`                     |
| U-221 | `HostMaintenance` transitioning to `Suspending`: rejected                          | Negative   | `TestAStepOfAnotherActionIsRejected`                     |
| U-222 | An empty `status.step`: restores to the action's declared initial state            | Boundary   | `TestTheFirstPassArmsTheStepAMachineIsBornIn`            |
| U-223 | A step value that belongs to a different action: restoration fails informatively   | Negative   | `TestAStepOfAnotherActionIsRejected`                     |
| U-224 | A step value outside the enum: restoration fails rather than stalling              | Negative   | `TestAStepThisOperatorCannotResumeEndsTheOperation`      |
| U-225 | The snapshot round-trips through `Snapshot` and `FromSnapshot` unchanged           | Positive   | —                                                        |
| U-226 | A deadline persisted and restored is the same absolute instant                     | Positive   | —                                                        |
| U-227 | A deadline that passed while the operator was down: restores as expired            | Boundary   | `TestAStepThatOutlivedItsDeadlineFailsTheOperation`      |
| U-228 | A step with no deadline: restores with none rather than with a zero instant        | Boundary   | —                                                        |
| U-229 | A terminal step: `IsTerminal` is true and no transition is attempted               | Boundary   | `TestTheLastStepFinishingEndsTheOperation`               |
| U-230 | The outer phase machine is separate from the step machine                          | Positive   | —                                                        |
| U-231 | Every state each graph declares appears in the step `Enum` marker                  | Boundary   | `TestTheStepEnumCoversEveryDeclaredState`                |
| U-232 | Every state each graph declares appears in the `status.step` CEL rule              | Boundary   | `TestTheCELRuleCoversEveryDeclaredState`                 |
| U-233 | The CEL rule names no value the graphs do not declare                              | Negative   | `TestTheCELRuleCoversEveryDeclaredState`                 |
| U-234 | A stored step from another action: refused, naming the declared set                | Negative   | `TestAStepOfAnotherActionIsRejected`                     |
| U-235 | A restore that fails: the operation is `Failed` with the error, not requeued       | Negative   | `TestAStepThisOperatorCannotResumeEndsTheOperation`      |
| U-365 | The provisioning graph's states cover its own `Enum` and CEL rule                  | Boundary   | `TestTheNodeStepEnumAndRuleCoverTheProvisioningGraph`    |
| U-366 | No step past the point of no return declares an abort edge                         | Negative   | `TestNoStepPastThePointOfNoReturnIsAbortable`            |
| U-367 | The steps an abort stops from are the ones that unwind cleanly                     | Positive   | `TestTheStepsAnAbortStopsCleanly`                        |
| U-368 | Every drain step past the suspend owes the resume                                  | Boundary   | `TestTheDrainStepsPastTheSuspendUnwind`                  |
| U-369 | Every step carries a budget, so none is the step that cannot time out              | Boundary   | `TestEveryStepHasABudget`                                |
| U-370 | The remove graph validates before it suspends                                      | Positive   | `TestTheRemoveGraphValidatesBeforeItSuspends`            |
| U-371 | The migrate graph splits the restart from the wait                                 | Positive   | `TestTheMigrateGraphSplitsTheRestartFromTheWait`         |
| U-372 | The host maintenance graph is the six-step window                                  | Positive   | `TestTheHostMaintenanceGraphIsTheSixStepWindow`          |
| U-373 | The four single-step actions share one line                                        | Positive   | `TestTheSingleStepActionsShareOneLine`                   |
| U-374 | Adoption is reachable from every gate before the add, the slot queue included      | Boundary   | `TestAdoptionIsReachableFromEveryGateBeforeTheAdd`       |
| U-375 | `AwaitingSlot` may go straight to `Resolving` when a sibling claimed the worker    | Boundary   | `TestAwaitingSlotMayGoStraightToResolving`               |
| U-384 | A worker that is not Ready or is cordoned holds `Posting` at `AwaitingWorker`      | Regression | `TestAWorkerThatWentAwayHoldsTheNodeRatherThanFailingIt` |
| U-385 | The held step carries its own budget rather than the one it was diverted from      | Regression | `TestTheHeldNodeGetsAFreshBudget`                        |
| U-386 | The worker coming back starts the path again at `CheckingHost`                     | Positive   | `TestAWorkerThatCameBackRestartsThePath`                 |
| U-387 | A held node emits `WorkerAway` rather than holding silently                        | Positive   | `TestTheHeldNodeStaysAndSaysSo`                          |
| U-388 | A node that never claimed its worker takes no slot while the worker is away        | Negative   | `TestAnUnclaimedNodeTakesNoSlotWhileItsWorkerIsAway`     |
| U-389 | A held node keeps its claim, so the node-add cap stays closed                      | Regression | `TestAHeldNodeKeepsItsClaim`                             |
| U-390 | A sibling posts no add while another worker is held at `AwaitingWorker`            | Negative   | `TestASiblingWaitsWhileAnotherWorkerIsHeld`              |

`U-384` to `U-390` are the reboot. The storage pool's MachineConfig is applied by
rebooting the machine, so the first node of a fresh cluster is cordoned, drained
and rebooted in the middle of being added (2026-09-20). `U-389` and `U-390` are
the pair that matter most: a held node keeps its claim, so the node-add cap stays
closed and no second worker is given a configuration change while the first is
still coming back. `U-388` is the other direction, and it is why only the two
steps that have claimed the worker divert — a node that never posted must not be
read by its sibling as an add that already happened.

### Entity: Pod Placement (design §13.1)

File: `operator/internal/controllers/node/podscheduling_test.go`

A pod the scheduler refused is the one Kubernetes-side failure the control plane
cannot see, and the scheduler has already written down why.

| #     | Scenario                                                  | Type     | Test                                       |
|-------|-----------------------------------------------------------|----------|--------------------------------------------|
| U-376 | An unplaceable pod is announced on the node it belongs to | Positive | `TestAnUnplaceablePodIsAnnouncedOnItsNode` |
| U-377 | A placed pod is announced nowhere                         | Negative | `TestAPlacedPodIsAnnouncedNowhere`         |
| U-378 | Another worker's scheduling failure is not this node's    | Negative | `TestAnotherWorkersFailureIsNotThisNodes`  |
| U-379 | A gated pod is a wait rather than a failure               | Boundary | `TestAGatedPodIsAWaitRatherThanAFailure`   |

### Workload: Device Partitioning (design §5.3)

File: `operator/internal/controllers/node/partitions_test.go`

| #     | Scenario                                                             | Type       | Test                                                  |
|-------|----------------------------------------------------------------------|------------|-------------------------------------------------------|
| U-380 | The partition count matches what the deployed fleets were built with | Regression | `TestThePartitionCountMatchesWhatFleetsWereBuiltWith` |

### Node Streams (design §4.4)

File: `operator/internal/controllers/node/unregister_test.go`

| #     | Scenario                                                                  | Type     | Test                                         |
|-------|---------------------------------------------------------------------------|----------|----------------------------------------------|
| U-381 | A node whose object goes closes its device stream, with the cluster known | Positive | `TestTheDeviceStreamClosesWithTheCluster`    |
| U-382 | The same, with the cluster already gone                                   | Boundary | `TestTheDeviceStreamClosesWithoutTheCluster` |
| U-383 | A node that never resolved a UUID closes nothing                          | Boundary | `TestANodeWithNoIdClosesNothing`             |

## 2. Integration Tests

Full reconcile loop against a real Kubernetes API server via `envtest`, with the
control plane still mocked. These cover what a fake client cannot: real
admission, real `resourceVersion` semantics, and real watch delivery.

**This package has no `envtest` suite.** The one this class was written against
lived beside the retired reconcilers in `operator/internal/controller/` and went
with them, so every row below is uncovered and §6 carries the class as one gap
rather than as thirty.

### Admission and Validation (design §3.1, §3.2, §3.4, §6.1)

| #    | Scenario                                                                                                                       | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | `spec.clusterRef` omitted at creation: rejected as `Required`                                                                  | Negative | —    |
| I-02 | `spec.clusterRef` changed after creation: rejected as immutable                                                                | Negative | —    |
| I-46 | `spec.clusterRef` naming no `StorageCluster`: the create is rejected                                                           | Negative | —    |
| I-47 | `spec.clusterRef` naming a cluster with no `status.uuid`: admitted, holds with `ClusterNotReady`                               | Boundary | —    |
| I-48 | `spec.clusterRef` naming a cluster in another namespace: the create is rejected                                                | Negative | —    |
| I-49 | `config.sizing.vcpuCount` differing from the cluster's: the create is rejected                                                 | Negative | —    |
| I-50 | A node manifest setting `config.sizing.maxSubsystemCount`: pruned rather than stored                                           | Boundary | —    |
| I-51 | `config.sizing` matching the cluster's exactly: admitted                                                                       | Positive | —    |
| I-52 | The operator re-sizing one node mid-roll: admitted, since the identity is exempt                                               | Positive | —    |
| I-53 | `spec.config.failureDomain` of `rack-b`: accepted                                                                              | Positive | —    |
| I-54 | A `failureDomain` of 64 characters, and one holding a slash: both rejected                                                     | Boundary | —    |
| I-03 | `spec.socketId` omitted at creation, set later: accepted, then frozen                                                          | Boundary | —    |
| I-04 | `spec.socketId` set at creation, cleared later: rejected                                                                       | Boundary | —    |
| I-05 | `spec.config.failureDomain` of `-rack`: rejected by the label pattern                                                          | Boundary | —    |
| I-06 | `spec.config.failureDomain` of `0`: accepted, since a digit is a valid label                                                   | Boundary | —    |
| I-07 | `spec.config.spdkSystemMemory` of `"4X"`: rejected by the pattern                                                              | Negative | —    |
| I-08 | `spec.workerNode` changed by a non-operator identity: rejected by the webhook                                                  | Negative | —    |
| I-09 | The webhook is unavailable: the update is rejected rather than admitted                                                        | Negative | —    |
| I-10 | `spec.action` outside the enum: rejected by admission before the controller sees it                                            | Negative | —    |
| I-11 | `spec.nodeRef` changed after creation: rejected as immutable                                                                   | Negative | —    |
| I-12 | `spec.migrate.targetWorkerNode` changed after creation: rejected as immutable                                                  | Negative | —    |
| I-13 | `spec.abort` set on a `Running` operation: accepted, since it is the mutable field                                             | Positive | —    |
| I-14 | `spec.remove.systemVolumeFilterRegex` unset: defaulted by the API server                                                       | Boundary | —    |
| I-15 | Short names `sn` and `snops` resolve to the same lists as the full kinds                                                       | Positive | —    |
| I-29 | `spec.slot` of -1: rejected by the minimum                                                                                     | Boundary | —    |
| I-30 | `spec.config` omitted at creation: rejected as `Required`                                                                      | Negative | —    |
| I-31 | `spec.config.sizing` omitted at creation: rejected as `Required`                                                               | Negative | —    |
| I-32 | `spec.config.deviceNames` changed after creation: rejected as immutable                                                        | Negative | —    |
| I-33 | `spec.config.journalManager` changed after creation: rejected as immutable                                                     | Negative | —    |
| I-34 | `spec.config.failureDomain` unset at creation, set later: accepted, then frozen                                                | Boundary | —    |
| I-35 | `spec.config.expand` changed after creation: rejected as immutable                                                             | Negative | —    |
| I-36 | `spec.config.spdkImage` changed: accepted, since a phased rollout is why it is per node                                        | Positive | —    |
| I-37 | The deployment config is deleted: every node reconciles unchanged                                                              | Positive | —    |
| I-38 | The deployment config's device filter is edited: existing nodes are untouched                                                  | Negative | —    |
| I-39 | A node created after that edit carries the new filter                                                                          | Positive | —    |
| I-40 | `spec.config.deviceNames` holding a PCI address: accepted                                                                      | Positive | —    |
| I-41 | `spec.config.deviceNames` holding a device path: accepted                                                                      | Positive | —    |
| I-42 | `spec.config.deviceNames` holding a bare device name: accepted, as a path under `/dev`                                         | Boundary | —    |
| I-43 | `spec.config.deviceNames` holding both a PCI address and a path: accepted by the schema, then refused by the webhook (`U-258`) | Boundary | —    |
| I-44 | `spec.config.deviceNames` holding a malformed PCI address: rejected by the item pattern                                        | Negative | —    |
| I-45 | `spec.config.deviceNames` holding a path with a space: rejected by the item pattern                                            | Negative | —    |

### Controller Behavior Under a Real API Server (design §4, §7, §11)

| #    | Scenario                                                                        | Type     | Test |
|------|---------------------------------------------------------------------------------|----------|------|
| I-16 | Reconciling a not-found resource returns no requeue                             | Negative | —    |
| I-17 | Two operations for one node: the second stays `Pending`, then runs              | Positive | —    |
| I-18 | The lock is released: the queued operation wakes from the node watch            | Positive | —    |
| I-19 | `kubectl delete` on a `Running` operation: `activeOpsRef` is cleared            | Positive | —    |
| I-20 | Operations on two nodes of one cluster run concurrently without interference    | Positive | —    |
| I-21 | Operations on nodes of two clusters run concurrently without interference       | Positive | —    |
| I-22 | An operation targeting a node with no `status.uuid`: fails informatively        | Negative | —    |
| I-23 | An operation in namespace A cannot lock a same-named node in namespace B        | Negative | —    |
| I-24 | Deleting the `StorageCluster` cascades to its nodes and to the workload objects | Positive | —    |
| I-25 | Two reconcilers racing the `Posting` claim: exactly one `POST` is issued        | Negative | —    |
| I-26 | Two nodes with the same name in two namespaces: both workloads are independent  | Positive | —    |
| I-27 | The namespace is deleted mid-drain: the operation terminates and releases       | Negative | —    |
| I-28 | The controller's role covers every object the workload reconcile touches        | Positive | —    |

---

## 3. End-to-End Tests

A live simplyblock cluster with a real control plane and a real data path. Every
row here changes cluster state, and the destructive ones say so.

### Provisioning and Adoption (design §4)

| #    | Scenario                                                                          | Type     | Test |
|------|-----------------------------------------------------------------------------------|----------|------|
| E-01 | A node object created: provisioned to `Ready` with a UUID within one cycle        | Positive | —    |
| E-02 | Five workers with `maxParallelNodeAdds` of 2: at most two are ever in flight      | Boundary | —    |
| E-03 | FoundationDB and non-FoundationDB workers together: the former stay sequential    | Positive | —    |
| E-04 | A two-socket worker: two nodes provisioned, each matched to its own backend node  | Positive | —    |
| E-05 | A pre-existing backend node with no object: adopted, keeping its UUID and volumes | Positive | —    |
| E-06 | Adoption of a Helm-deployed fleet through the upgrade Secret                      | Positive | —    |
| E-07 | The per-node ConfigMap on a fresh install: the init container finds a full entry  | Negative | —    |

### Operations (design §7, §8, §9, §10)

| #    | Scenario                                                                                 | Type     | Test |
|------|------------------------------------------------------------------------------------------|----------|------|
| E-08 | `Suspend` then `Resume`: the node returns to `online` and accepts new volumes            | Positive | —    |
| E-09 | `Restart` with `reattachVolume`: volumes are reattached and I/O resumes                  | Positive | —    |
| E-10 | Happy-path `Remove` on a node with volumes: drained, removed, `Succeeded`                | Positive | —    |
| E-11 | `Remove` on an empty node: no migrations created, and it completes directly              | Boundary | —    |
| E-12 | `Remove` blocked by a pinned claim: the node stays online until the pin is removed       | Negative | —    |
| E-13 | Sustained fio during a `Remove`: no I/O errors, and checksums match after                | Positive | —    |
| E-14 | A migration fails mid-drain: it is replaced and the drain completes                      | Negative | —    |
| E-15 | `Remove` aborted mid-drain: the node returns to `online` and accepts volumes             | Negative | —    |
| E-16 | `Migrate` to another worker: the node keeps its UUID and its volumes follow              | Positive | —    |
| E-17 | Sustained fio during a `Migrate`: no I/O errors, and checksums match after               | Positive | —    |
| E-18 | `Migrate` with `newSsdPcie`: the added devices appear and survive a later restart        | Positive | —    |
| E-19 | A worker cordoned and drained: the node is shut down, evicted, and returns online        | Positive | —    |
| E-20 | Two workers cordoned at once on a three-node cluster: the second holds for its slot      | Boundary | —    |
| E-21 | Sustained fio through a host maintenance window: no I/O errors on the peers              | Positive | —    |
| E-22 | Transient 5xx from the control plane mid-operation: retried, and the operation completes | Negative | —    |
| E-23 | A drain on a three-node cluster with one peer already offline: the operation blocks      | Boundary | —    |
| E-24 | A drain on a single-node cluster: blocked with `NoMigrationTarget`, node untouched       | Boundary | —    |
| E-25 | A drain of a node holding 100 or more volumes: completes, and the duration is recorded   | Boundary | —    |
| E-26 | A node added mid-drain: the drain's volume set is unaffected                             | Negative | —    |

---

## 4. Manual Scenarios

### M-01: The operator is killed between the write-ahead patch and the backend call

**Design reference:** §7.2.

**What to verify:** that the persisted step does what the removed `status.triggered`
flag used to, which no unit test can show because it requires the process to
actually die between two statements.

**Test concept:**

1. Create a `StorageNodeOps` with `action: Suspend` against an online node.
2. Kill the operator pod the moment the `Requesting` step is persisted, before the
   control plane records a suspend request.
3. Restart the operator and watch the operation resume.
4. Confirm in the control-plane audit log that exactly one suspend was requested,
   or none.
5. Repeat for the `Posting` step of a node's own provisioning, where a duplicate
   call adds a second backend node rather than repeating a no-op.

**Current behavior:** the flag is written after the call, so a crash between the
two leaves the flag false and the call made, which is precisely the case the flag
exists to prevent. Design §7.2 removes the failure mode by reading the target's
state instead.

### M-02: The drained node's host dies mid-drain

**Design reference:** §8.2, §8.3.

**What to verify:** that a drain whose subject disappears reaches a terminal state
rather than looping. If the node is genuinely gone, verification finds no volumes
and the removal succeeds. If it is unreachable but present, the step holds until
its deadline and then resumes and fails.

**Test concept:**

1. Start a `Remove` on node A of a three-node cluster.
2. Wait for `status.step.state` to reach `MigratingVolumes`.
3. Power off the host running node A.
4. Confirm the operation reaches `Succeeded` or `Failed` within the step deadline,
   and never sits in a loop.
5. Confirm the surviving nodes report no volumes stranded on A.

### M-03: Cutting the cluster to its fault-tolerance limit during a drain

**Design reference:** §8.3, §11.

**What to verify:** what the operator does when a drain would take the cluster
below its redundancy floor. The design's stance is that the cluster gate of §7.1
holds the operation, and this scenario is what proves the gate is reached before
the removal rather than after it.

**Current behavior:** the gate checks the cluster's status and its rebalancing
flag, not its fault-tolerance headroom, so a drain on a cluster at its limit
proceeds. Design §16, Q7 owns the question of where that check belongs.

**Test concept:**

1. A three-node cluster with a fault tolerance of one.
2. Take node C offline out of band, which puts the cluster at its limit.
3. Create a `Remove` for node B.
4. Confirm the operation holds rather than removing the last redundant node, and
   that the reason is on the object as an event.
5. Bring node C back and confirm the operation proceeds without further input.

### M-04: A relocation restart whose start is never observed

**Design reference:** §9, §12.

**What to verify:** the one negative completion predicate in the design. The
`Relocating` step completes when the node has left `online`, and a coalescing
stream can deliver `online` before and `online` after without ever delivering the
window in between.

**Test concept:**

1. Start a `Migrate` for a node onto another worker.
2. Throttle or suspend the operator's stream subscription across the restart
   window, so the departure from `online` is not delivered.
3. Confirm the operation either holds until its deadline or completes, and in
   particular that no promote is issued while the restart is in flight.
4. Confirm the relocated devices are not left in `new`.

**Open question:** a restart generation on the node object would make the
observation positive and remove the scenario. Design §16, Q4.

### M-05: An OS upgrade rolling across every worker

**Design reference:** §10.

**What to verify:** the whole maintenance flow under the conditions it exists for,
which is the only way to exercise the interaction between the operator, the
eviction, the kubelet, and the reboot.

**Test concept:**

1. A five-node cluster with a concurrency limit of one.
2. Trigger a rolling OS upgrade across every worker.
3. Confirm exactly one maintenance operation is `Running` at a time, and the rest
   hold at `Holding` with `MaintenanceQueued`.
4. Confirm each node returns to `online` before the next worker is cordoned.
5. Run fio throughout and confirm no I/O errors and matching checksums.
6. Confirm no budget and no drain label survives the run.

---


---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 375       | 270     | 105         |
| Integration | 54        | 0       | 54          |
| E2E         | 26        | 0       | 26          |
| Manual      | 5         | 0       | 5           |
| **Total**   | **460**   | **270** | **190**     |

Eight further scenarios are struck through: they describe a system this one no
longer is, and each names the row that replaced it. They are excluded from the
counts.

Two hundred and seven distinct test functions cover the two hundred and seventy
covered scenarios, because a table-driven test satisfies one identifier per
subtest and several rows are two halves of one assertion.

Every covered scenario is a unit test, and the concentration is the shape of the
package rather than an accident of effort: both reconcilers are written so that
every decision is a predicate over state a fake client and a scripted control
plane can present, which is what makes the operation machine testable without a
cluster. What that buys is also its limit. Admission, `resourceVersion`
semantics, watch delivery, and garbage collection are the API server's, and no
row that depends on one of them is covered.

---

## 6. What Is Not Yet Covered

| #                                        | Gap                                                                      | Reason                                                                                                                                                                                          |
|------------------------------------------|--------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-12                                     | A failure-domain label of `0` read as set rather than unset              | The label form retires the zero-versus-unset ambiguity the integer had, and this row is what keeps a label spelled `0` from reintroducing it                                                    |
| U-14 … U-17                              | The worker's storage-node API probe                                      | `HostAnswers` makes a real HTTP request, so a unit test would either reach the network or assert a stub. It needs `envtest` with a served endpoint, or an e2e row                               |
| U-20, U-21                               | Counting in-flight workers rather than objects                           | The cap is covered by worker. That two sockets of one host count once is the multi-socket case, and the slot suite uses one node per worker                                                     |
| U-24 … U-27                              | The FoundationDB serialization rules                                     | `hostsFoundationDB` has no test, and it is the rule that keeps the control plane's own store from losing quorum during an expansion                                                             |
| U-28 … U-34                              | The optimistic-lock claim, and the `POST`'s failure modes                | The claim is structural: it is the transition into `Posting`, and nothing asserts the ordering or the 409. The control-plane failure paths need a scripted client that fails                    |
| U-36, U-37                               | The worker's internal address                                            | `workerAddress` is exercised through adoption and asserted nowhere                                                                                                                              |
| U-39, U-40, U-42, U-43                   | Positional resolution for multi-socket workers                           | The RPC-port ordering the slot match depends on is an assumption nothing asserts                                                                                                                |
| U-47                                     | What adoption writes                                                     | The divert to `Adopting` is covered and `resolveUUID`'s write is not                                                                                                                            |
| U-50 … U-52                              | The assigned fault group, a malformed reading, and `observedGeneration`  | `applyReading` writes all three and no row asserts them                                                                                                                                         |
| U-55, U-57, U-58                         | Deletion's remaining cases                                               | `U-57` is the open decision below rather than an unwritten test                                                                                                                                 |
| U-64                                     | A look-alike service account in another namespace                        | The identity match is string-based, and nothing asserts it cannot be spoofed by a namespace name                                                                                                |
| U-67 … U-69, U-71, U-73 … U-76           | The workload's TLS mounts, its RBAC, and its ownership                   | The DaemonSet's shape is covered by the builder's own tests, and what the reconcile does with it is covered only for the image and the certificate revision                                     |
| U-78 … U-84                              | The per-slot storage-node-uuid labels                                    | The most consequential labels in the operator, and only the cluster-wide one is covered. A wrong key here breaks CSI provisioning                                                               |
| U-85 … U-94, U-262, U-263                | The per-node configuration's contents                                    | The clone is covered and the render is not: the assertions that named every key died with the retired set, and `ReconcileConfig` is exercised only through enrollment                           |
| U-99 … U-101                             | The SPDK proxy EndpointSlices                                            | The builders are covered in `internal/utils` and the reconcile that applies them is not                                                                                                         |
| U-104 … U-107                            | The TLS environment, the Secret predicate, and the cert-manager provider | `reconcileCertificates` is the least covered path in the workload reconcile                                                                                                                     |
| U-118, U-120, U-121, U-123               | The lock's concurrent cases, and the inactive cluster                    | Two reconcilers racing one free lock needs real `resourceVersion` semantics. The rebalancing half of the gate is covered and the inactive half is not                                           |
| U-130 … U-132, U-134                     | The single-step actions' control-plane failures                          | The scripted control plane can refuse a call, and no row yet distinguishes a 4xx from a 5xx or asserts what reaches the event                                                                   |
| U-144 … U-146                            | Classification boundaries                                                | The buckets are covered individually and their overlaps are not. `U-146` records that an empty pattern falls back to the default, which is the shipped behavior and was written as its opposite |
| U-150, U-155, U-159, U-162, U-164, U-165 | The drain's remaining edges                                              | `U-162` is written against a behavior the code does not have: a refused removal is terminal whatever its status, so either the row or `drainRemove` has to move                                 |
| U-171, U-177                             | Migration naming under collision and across namespaces                   | The formula is atlas-lib's and is tested there. What is missing is the assertion that this caller's inputs cannot collide                                                                       |
| U-196, U-199, U-200                      | The relocation's abort, and the source worker's labels                   | `ReleaseWorker` decides whether a worker keeps its labels and no row exercises the two-socket case                                                                                              |
| U-202, U-209, U-216                      | The maintenance window's remaining edges                                 | An uncordon mid-window, the ordering of the release against the node going offline, and the deadline that is a detection mechanism rather than a recovery one                                   |
| U-225, U-226, U-228, U-230               | The snapshot's round trip                                                | Covered by `atlas-lib/statemachine`'s own suite for the type, and not by this package for the values it stores                                                                                  |
| U-236 … U-238                            | The device summary                                                       | `applyReading` writes the pair and the absent case, and the rows that pin the ordering the string form got backward are unwritten                                                               |
| U-244 … U-247                            | The widened `deviceNames`                                                | The field takes a PCI address and a device path in one list, and nothing asserts either form reaches the node                                                                                   |
| U-264 … U-266                            | The image fallback, and the name's determinism                           | The fallback to the singleton `ControlPlane` is exercised only through its refusal, and the name formula's determinism is asserted nowhere                                                      |
| I-01 … I-54                              | Every integration row                                                    | This package has no `envtest` suite. CEL, `Required`, defaulting, real conflicts, watch delivery, and garbage collection are all the API server's, and a fake client has none                   |
| E-01 … E-26                              | All end-to-end scenarios                                                 | Needs a live cluster. The e2e harness under `test/` does not cover this kind yet                                                                                                                |
| M-01 … M-05                              | Crash consistency, host death, degraded clusters, and the roll           | Need process kills, host power control, and a real OS upgrade                                                                                                                                   |
| Metrics                                  | The eleven metrics of design §13.2                                       | The series are declared in `metrics.go` and observed by the reconcilers, and no row asserts a label set or a value                                                                              |
| Events                                   | The twenty reasons of design §13.1                                       | Nine reasons are asserted through the rows above. The rest are raised and unasserted                                                                                                            |
| Retention                                | Nothing deletes a terminal `StorageNodeOps`                              | Feature does not exist. Design §16, Q6                                                                                                                                                          |

### Two decisions this plan is holding

**`U-57`, a failed removal and the finalizer.** The row states that a `Failed`
removal holds the node's finalizer, which is what the deleted
`TestHandleDeletion_FailedRemoveOpsBlocksFinalizerRemoval` asserted after the
2026-08-13 incident: `Failed` means the backend node was never removed, so
letting the object go orphans a live node. The shipped teardown releases on any
terminal phase and argues for it in prose, on the grounds that an object nobody
can delete is worse. Both positions are defensible and they contradict, so the
row stays uncovered until one of them is written down as the rule.

**The failure-domain balance gate.** The removal's own pre-check,
`fdRemovalBalanceCheck`, went with the rework and has no equivalent: balance now
rests entirely on the control plane's `check_fd_admission_for_remove`, which
`drainRemove` reads as a terminal refusal. No row in this plan describes an
operator-side check, because the operator no longer makes one. `M-03` is the
scenario that would find out whether the control plane's refusal arrives early
enough to matter.

### Axis coverage

The axes are the ones that actually break this operator. A blank cell is a
combination nothing exercises.

| Axis                      | Value                      | Scenarios                                                       |
|---------------------------|----------------------------|-----------------------------------------------------------------|
| Cluster node count        | Single node                | U-155, E-24                                                     |
|                           | Two nodes                  | U-155, U-337                                                    |
|                           | Three nodes                | U-152, U-339, E-13, E-20, E-23, M-02, M-03                      |
|                           | Five or more               | E-02, E-25, M-05                                                |
|                           | Asymmetric node sizes      | —                                                               |
| Sockets per worker        | One                        | Every scenario except those below                               |
|                           | Two or more                | U-21, U-30, U-31, U-39, U-40, U-205, E-04                       |
| Namespace count           | Single namespace           | Every scenario except those below                               |
|                           | Multiple namespaces        | U-72, U-145, U-177, I-23, I-26                                  |
| simplyblock cluster count | One cluster                | Every scenario except those below                               |
|                           | Several in one Kubernetes  | U-81, U-328, U-354, I-21                                        |
|                           | Cross-cluster              | — (not applicable: no node operation spans Kubernetes clusters) |
| Failure domains           | Not enabled                | U-11                                                            |
|                           | Enabled and set            | U-10, U-270                                                     |
|                           | Enabled and unset          | U-09, U-13                                                      |
|                           | Partially set              | —                                                               |
| Object scale              | Zero volumes               | U-139, U-169, U-336, E-11                                       |
|                           | A handful                  | U-152, U-335, E-10, E-13                                        |
|                           | 100 or more                | E-25                                                            |
| Lifecycle and restart     | Mid-step restart           | U-172, U-195, M-01                                              |
|                           | Terminal re-reconcile      | U-117, U-315                                                    |
|                           | Deletion mid-operation     | U-119, U-344, U-345, I-19, I-27                                 |
|                           | Host death mid-operation   | U-332, M-02                                                     |
| Reading source            | The stream, once delivered | U-276, U-340                                                    |
|                           | The poll, until it is      | U-277, U-341                                                    |
|                           | A node neither one has     | U-280                                                           |
| Device class              | NVMe, matching             | U-256, U-261                                                    |
|                           | Block, matching            | U-259                                                           |
|                           | An entry of the other one  | U-255, U-257                                                    |
|                           | A list holding both        | U-258                                                           |
|                           | A filter for the other one | U-260                                                           |
|                           | Unstated                   | U-286                                                           |
| Capacity sampling         | A first reading            | U-249                                                           |
|                           | Below the write threshold  | U-250                                                           |
|                           | Above it, or a new total   | U-251, U-252                                                    |
|                           | Unreachable or unmeasured  | U-253, U-254                                                    |
| Actor                     | Operator-raised operation  | U-54, U-201, U-284                                              |
|                           | User-created operation     | Every operation scenario except those three                     |
|                           | Webhook path               | U-60 … U-64, U-288 … U-296, I-08, I-09                          |

**The asymmetric-node row is the significant blank.** Every drain and every
migration picks a target from the online peers by round-robin, which spreads by
count rather than by capacity, so a cluster whose nodes differ in size
concentrates on the smallest one exactly as readily as on the largest. `U-339`
pins the spread as deliberate and nothing exercises what it costs, and design
§8.2 does not claim otherwise.

**The partially set failure-domain row is the second.** A cluster with
`enableFailureDomains` set where some nodes declare a group and others do not is
the state a half-finished expansion leaves behind, and the gate of §4.2 is
per-node, so the cluster runs in a mixed state nothing reports on.

**The multi-namespace rows are covered thinly and matter more than the count
suggests.** Two namespaces are two independent deployments, and the workload
objects are namespaced while the storage-plane labels and the ClusterRoleBindings
are not. `U-72` covers the binding names, `U-354` covers a maintenance window not
reaching across a cluster boundary, and nothing covers what happens when two
namespaces claim the same worker.
