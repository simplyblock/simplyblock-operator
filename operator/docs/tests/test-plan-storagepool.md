# Test Plan: StoragePool and StoragePoolOps

Related design: [`designs/crd-redesign/design-storagepool.md`](../designs/crd-redesign/design-storagepool.md)

Scope is the operator and the Kubernetes surface this repository builds. The
control plane (`sbcli`) is a dependency, faked at the boundary: what a row
asserts is the operator's response to an answer, never the control plane's own
behavior.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason.

Scenario text names the target spelling. The parent reference is
`spec.clusterName` today and `spec.clusterRef` after design §3.1, the pool's
ceilings are `spec.capacityLimit` and `spec.qos` today and `spec.limits` after,
and the volume defaults are `spec.storageClassParameters` today and
`spec.volumeDefaults` after.

| Class       | Prefix | Harness                                                                |
|-------------|--------|------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, and a mock backend |
| Integration | `I-`   | Full reconcile loop against `envtest` and a mock backend               |
| E2E         | `E-`   | Live simplyblock cluster, real data path                               |
| Manual      | `M-`   | Needs failure injection or orchestration not automated yet             |

---

## 1. Unit Tests

Pure functions and single reconcile calls against a fake client, with the control
plane replaced by a mock HTTP server.

### The Class Assignment (design §5)

File: `operator/internal/controllers/pool/assignment_test.go`

| #    | Scenario                                                                       | Type     | Test                                          |
|------|--------------------------------------------------------------------------------|----------|-----------------------------------------------|
| U-01 | A class carrying the three labels is indexed to the pool they name             | Positive | TestIndexesEveryClassAssignedToThePool        |
| U-02 | Two classes naming one pool are both indexed to it                             | Positive | TestIndexesEveryClassAssignedToThePool        |
| U-03 | The same pool name in two namespaces: each class indexes to its own pool       | Boundary | TestThePoolNameAloneIsNotTheAssignment        |
| U-04 | The same pool name in two clusters of one namespace: likewise                  | Boundary | TestTheClusterLabelSeparatesTwoPoolsOfOneName |
| U-05 | A class whose labels name no pool is indexed to nothing and fails no reconcile | Negative | TestAClassNamingNoPoolIsIndexedToNothing      |
| U-06 | A class with a class name that matches no convention is still indexed          | Positive | TestAClassNameCarriesNothing                  |

### Pool Creation (design §4.1)

File: `operator/internal/controllers/pool/storagepool_controller_test.go`

| #    | Scenario                                                                 | Type     | Test                                           |
|------|--------------------------------------------------------------------------|----------|------------------------------------------------|
| U-07 | No `status.uuid`: the pool is created and the UUID recorded              | Positive | TestCreatesThePoolAndRecordsItsUUID            |
| U-08 | The claim is persisted before the `POST` is issued                       | Positive | TestTheClaimIsPersistedBeforeTheCreateIsIssued |
| U-09 | A second reconciler at the same `resourceVersion`: 409, no second `POST` | Negative | TestOnlyOneOfTwoReconcilersClaimsTheCreation   |
| U-10 | The `POST` returns 5xx: retried, the UUID stays empty                    | Negative | —                                              |
| U-11 | The `POST` returns 4xx: `PoolCreationFailed` with the body in the event  | Negative | TestReportsACreationRefusal                    |
| U-12 | The cluster has no `status.uuid` yet: held with `ClusterNotReady`        | Negative | TestHoldsWhileTheClusterIsNotReady             |
| U-13 | The cluster does not exist: held, not failed                             | Negative | TestHoldsWhenTheClusterDoesNotExist            |
| U-14 | `status.uuid` present: creation is not attempted again                   | Negative | TestDoesNotCreateAPoolThatAlreadyHasAUUID      |

### The StorageClass (design §5)

File: `operator/internal/controllers/pool/storagepool_controller_test.go`, and
`dhchap_test.go` for the node selector and the ownership.

| #    | Scenario                                                                       | Type     | Test                                                 |
|------|--------------------------------------------------------------------------------|----------|------------------------------------------------------|
| U-15 | A non-default pool: no class is created by its reconcile, ever                 | Negative | TestANonDefaultPoolGetsNoGeneratedClass              |
| U-16 | A class assigned to the pool sets `cluster_id` and `pool_name` consistently    | Positive | TestGeneratedParametersInventNothing                 |
| U-17 | A class whose `parameters` disagree with its labels is reported, not rewritten | Negative | TestNeverRewritesAnAssignedClass                     |
| U-18 | The default pool: one class is written, labeled `managed-by`, and not repeated | Positive | TestTheDefaultPoolsClassIsWrittenOnceAndNotRecreated |
| U-19 | A class assigned after the pool is `Ready`: `StorageClassAssigned` is emitted  | Positive | TestPublishesTheClassesAssignedToThePool             |
| U-20 | The default class deleted out of band: not recreated, `status` drops it        | Negative | TestTheDefaultPoolsClassIsWrittenOnceAndNotRecreated |
| U-21 | The class carries no owner reference, since the scopes forbid one              | Negative | TestTheGeneratedClassCarriesNoOwnerReference         |
| U-22 | `status.storageClassNames` lists every assigned class and nothing else         | Positive | TestPublishesTheClassesAssignedToThePool             |
| U-23 | A class carrying both QoS spellings: new wins, `QoSParameterConflict` emitted  | Negative | TestReportsAClassThatStatesOneCeilingTwice           |

### The Allowed-Node Label (today's controller)

A DHCHAP-gated pool restricts its volumes to `spec.allowedNodes` through one
label per pool on each of them, whose key the generated class republishes as
`dhchap_node_selector`. The key is `kube.PoolNodeLabelKey(status.uuid)`, and its
length is the whole reason it is derived that way, so the rule is tested in
`atlas-lib/kube`.

| #    | Scenario                                                                                                     | Type       | Test                                           |
|------|--------------------------------------------------------------------------------------------------------------|------------|------------------------------------------------|
| U-54 | The label key is within the 63 bytes a label name may have, whatever a pool and its cluster are named (#505) | Regression | `TestPoolNodeLabelKey`                         |
| U-55 | The generated class's `dhchap_node_selector` is the same key that is written on the node                     | Positive   | `TestTheDHCHAPClassRepublishesTheNodeLabelKey` |

### Deletion and the Bound-Volume Hold (design §6)

File: `operator/internal/controllers/pool/deletion_test.go`

| #    | Scenario                                                                                 | Type     | Test                                                |
|------|------------------------------------------------------------------------------------------|----------|-----------------------------------------------------|
| U-24 | Deleting a pool with no bound volumes: class deleted, backend deleted, finalizer cleared | Positive | TestDeletesAPoolNothingRefersTo                     |
| U-25 | Deleting a pool with one bound volume: held, `VolumesStillBound` emitted                 | Negative | TestABoundVolumeHoldsTheDeletionAndNothingIsDeleted |
| U-26 | Nothing is deleted while held: the class and the backend pool both survive               | Negative | TestABoundVolumeHoldsTheDeletionAndNothingIsDeleted |
| U-27 | The last claim is deleted: the held pool deletion proceeds unattended                    | Positive | TestTheHoldClearsWhenTheLastVolumeGoes              |
| U-28 | A pool with no `status.uuid`: the finalizer clears without a backend call                | Boundary | TestAPoolWithNoUUIDReleasesWithoutABackendCall      |
| U-29 | The backend `DELETE` returns 404: treated as success                                     | Boundary | TestA404FromTheControlPlaneIsSuccess                |
| U-30 | The backend `DELETE` returns 5xx: retried, the finalizer is kept                         | Negative | TestAFailedBackendDeleteKeepsTheFinalizer           |
| U-31 | A `PersistentVolume` of another pool's class does not hold this pool                     | Negative | TestAnotherPoolsVolumeDoesNotHoldThisOne            |
| U-32 | A released `PersistentVolume` still counts as bound until it is deleted                  | Boundary | TestAReleasedVolumeStillHolds                       |
| U-33 | A control-plane volume with no `PersistentVolume` does not hold the deletion             | Boundary | TestAVolumeKubernetesCannotSeeDoesNotHold           |
| U-34 | Zero `PersistentVolume` objects: the count is 0 and the deletion proceeds                | Boundary | TestDeletesAPoolNothingRefersTo                     |

### Spec Grouping (design §3.1)

| #    | Scenario                                                                   | Type     | Test                                         |
|------|----------------------------------------------------------------------------|----------|----------------------------------------------|
| U-35 | `spec.limits` reaches the control plane as the pool's ceilings             | Positive | TestTheTwoLimitGroupsGoToDifferentPlaces     |
| U-36 | `spec.volumeDefaults` reaches the class and not the control plane          | Positive | TestTheTwoLimitGroupsGoToDifferentPlaces     |
| U-37 | Both set: neither is written into the other's destination                  | Negative | TestTheTwoLimitGroupsGoToDifferentPlaces     |
| U-38 | Neither set: the pool is created with the control plane's own defaults     | Boundary | —                                            |
| U-39 | `spec.limits.iops` of 0 is unlimited, not unset                            | Boundary | TestAZeroVolumeDefaultReachesTheClassAsZero  |
| U-40 | `spec.volumeDefaults.enableDHCHAP` reaches the class as the driver expects | Positive | TestTheDHCHAPClassRepublishesTheNodeLabelKey |

### StoragePoolOps (design §7)

File: `operator/internal/controllers/pool/storagepoolops_controller_test.go`

| #    | Scenario                                                                      | Type     | Test                                         |
|------|-------------------------------------------------------------------------------|----------|----------------------------------------------|
| U-41 | The lock is free: acquired, phase becomes `Running`                           | Positive | TestAcquiresAFreeLock                        |
| U-42 | Another operation holds the lock: this one stays `Pending`                    | Negative | TestWaitsForALockAnotherOperationHolds       |
| U-43 | Two reconcilers acquiring one free lock: the loser gets 409                   | Negative | —                                            |
| U-44 | Terminal re-reconcile: no side effect, the lock is released again             | Negative | TestATerminalOperationTouchesNothing         |
| U-45 | The operation is deleted while `Running`: the finalizer releases the lock     | Positive | TestDeletingARunningOperationReleasesTheLock |
| U-46 | `Rebalance` `Validating`: the volumes on no-longer-allowed nodes are listed   | Positive | —                                            |
| U-47 | `Rebalance` with nothing misplaced: it succeeds without creating an operation | Negative | —                                            |
| U-48 | `Rebalance` `Migrating`: one `PersistentVolumeOps` per listed volume          | Positive | —                                            |
| U-49 | `Rebalance`: a fanned-out migration failing fails the pool operation          | Negative | —                                            |
| U-50 | An unknown action: terminal failure with the action in the message            | Negative | —                                            |
| U-51 | The target pool does not exist: the operation fails with a not-found message  | Negative | TestAnOperationOnAMissingPoolFails           |
| U-52 | `Suspend` or `Resume` as `spec.action`: rejected by the action `Enum`         | Negative | —                                            |
| U-53 | Every declared state appears in the step `Enum` and in the CEL rule           | Boundary | TestTheStepEnumAndTheCELRuleAgree            |

### The Conversion to the Hub (design §11)

File: `operator/api/v1alpha1/storagepool_conversion_test.go`, and
`hub_roundtrip_test.go` for the direction storage takes while `v1alpha1` is the
storage version.

| #    | Scenario                                                                               | Type       | Test                                                 |
|------|----------------------------------------------------------------------------------------|------------|------------------------------------------------------|
| U-56 | The three top-level fields and `spec.qos` regroup into `spec.limits`                   | Positive   | `TestStoragePoolSpecRegroupsToTheHub`                |
| U-57 | `spec.storageClassParameters` and `dhchap` regroup into `spec.volumeDefaults`          | Positive   | `TestStoragePoolSpecRegroupsToTheHub`                |
| U-58 | The pool's ceilings and its volumes' defaults do not reach each other's group          | Negative   | `TestStoragePoolLimitsAndVolumeDefaultsStaySeparate` |
| U-59 | A group whose source is absent stays absent rather than becoming empty                 | Boundary   | `TestStoragePoolAbsentGroupsStayAbsent`              |
| U-60 | A ceiling of `"0"` converts to 0, which is unlimited and not unset                     | Boundary   | `TestStoragePoolZeroCeilingIsUnlimitedNotUnset`      |
| U-61 | An unparsable ceiling becomes absent rather than failing the conversion                | Negative   | `TestStoragePoolUnparsableCeilingBecomesAbsent`      |
| U-62 | `spec.action` and `spec.status` round-trip through their stash annotations             | Boundary   | `TestStoragePoolRemovedSpecFieldsRoundTrip`          |
| U-63 | A pool that set neither removed field gains no annotation                              | Negative   | `TestStoragePoolUnsetRemovedFieldsWriteNoAnnotation` |
| U-64 | Stashing does not write through to the object the webhook was handed                   | Negative   | `TestStoragePoolStashDoesNotMutateTheSource`         |
| U-65 | `status.qos` becomes `status.limits`                                                   | Positive   | `TestStoragePoolStatusRenamesQoSToLimits`            |
| U-66 | Hub → spoke → hub loses nothing, which is what a write costs while storage is v1alpha1 | Regression | `TestStoragePoolRoundTripsFromTheHub`                |
| U-67 | An empty hub spec round-trips as absent groups rather than as empty ones               | Boundary   | `TestStoragePoolEmptyGroupsRoundTripAsAbsent`        |

### The Stash for Fields `v1alpha1` Cannot Express (design §11)

File: `operator/api/v1alpha1/storagepool_stash_test.go`

Nine hub fields have no `v1alpha1` spelling, and while `v1alpha1` is the storage
version each is lost on *every write* rather than once at upgrade time. These are
the rows that say it does not happen.

| #     | Scenario                                                                         | Type       | Test                                              |
|-------|----------------------------------------------------------------------------------|------------|---------------------------------------------------|
| U-96  | All nine survive hub → spoke → hub through their annotations                     | Regression | `TestStoragePoolHubOnlyFieldsAreStashed`          |
| U-97  | A toggle stashed as `false` is restored as `false` rather than as absent         | Boundary   | `TestStoragePoolHubOnlyFieldsAreStashed`          |
| U-98  | The annotations are taken back out on the way up                                 | Positive   | `TestStoragePoolStashIsRemovedOnTheWayUp`         |
| U-99  | A pool stating none of them gains no metadata                                    | Negative   | `TestStoragePoolStashesNothingForAnEmptyHub`      |
| U-100 | A value the hub stops stating is cleared rather than restored from a stale stash | Negative   | `TestStoragePoolStaleStashIsCleared`              |
| U-101 | A hand-edited annotation costs its own field and no other                        | Negative   | `TestStoragePoolAnUndecodableStashIsDropped`      |
| U-102 | An annotation a user wrote is left alone                                         | Negative   | `TestStoragePoolStashLeavesOtherAnnotationsAlone` |

### The Admission Guard (design §3.4)

File: `operator/internal/webhook/storagepool_validator_test.go`

| #    | Scenario                                                                     | Type     | Test                                                     |
|------|------------------------------------------------------------------------------|----------|----------------------------------------------------------|
| U-68 | A cluster that exists and is finished: admitted                              | Positive | `TestStoragePoolValidator`                               |
| U-69 | A cluster that exists with no UUID: admitted, because it is a not-yet        | Boundary | `TestStoragePoolValidator`                               |
| U-70 | A cluster that does not exist: refused, naming it                            | Negative | `TestStoragePoolValidator`                               |
| U-71 | A cluster of that name in another namespace: refused                         | Negative | `TestStoragePoolValidator`                               |
| U-72 | An update or a delete is not the validator's business                        | Negative | `TestStoragePoolValidator`                               |
| U-73 | A pool whose namespace is carried by the request path rather than the object | Boundary | `TestStoragePoolValidatorFallsBackToTheRequestNamespace` |

### The QoS Vocabulary (design §5.1)

File: `atlas-lib/kube/qos_test.go`

| #    | Scenario                                                                  | Type     | Test                                                    |
|------|---------------------------------------------------------------------------|----------|---------------------------------------------------------|
| U-74 | The current class-parameter spelling wins over the older one              | Positive | `TestQoSParamPrefersTheCurrentSpelling`                 |
| U-75 | All four older class keys are still read                                  | Positive | `TestQoSParamReadsTheOlderSpelling`                     |
| U-76 | A key set to the empty string is unset, so the older key still answers    | Boundary | `TestQoSParamTreatsAnEmptyValueAsUnset`                 |
| U-77 | All three claim-annotation generations are read, newest first             | Positive | `TestQoSAnnotationReadsAllThreeGenerations`             |
| U-78 | A class stating one ceiling twice is reported as a conflict               | Negative | `TestQoSParamConflictsFindsBothSpellings`               |
| U-79 | A class stating each ceiling once is not a conflict, whichever generation | Negative | `TestQoSParamConflictsIgnoresASingleSpelling`           |
| U-80 | The key lists are handed out as copies, so precedence cannot be mutated   | Boundary | `TestQoSKeyListsAreCopies`                              |
| U-81 | `Properties` parses either generation into the same limits                | Positive | `TestPropertiesReadEitherQoSGeneration`                 |
| U-82 | An unparsable ceiling names the key it was actually written under         | Negative | `TestAnUnparsableCeilingNamesTheKeyItCameFrom`          |
| U-83 | The generated class carries one generation only, so it cannot conflict    | Negative | `TestTheGeneratedClassCarriesOnlyTheCurrentQoSSpelling` |

### The Collector (design §9.2)

File: `operator/internal/controllers/pool/collector_test.go`

| #    | Scenario                                                                | Type       | Test                                        |
|------|-------------------------------------------------------------------------|------------|---------------------------------------------|
| U-91 | One pass publishes what the pool is, what it holds, and its class count | Positive   | `TestOnePassPublishesThePoolsGauges`        |
| U-92 | A pool that goes away stops being reported                              | Regression | `TestAPoolThatGoesAwayStopsBeingReported`   |
| U-93 | `CapacityExhausted` fires once per crossing rather than once per pass   | Negative   | `TestCapacityExhaustedFiresOncePerCrossing` |
| U-94 | A pool with no capacity limit is never announced as exhausted           | Boundary   | `TestAnUnlimitedPoolIsNeverExhausted`       |
| U-95 | One pass queries each cluster once rather than once per pool            | Boundary   | `TestOnePassQueriesEachClusterOnce`         |

### `StoragePoolMetrics` (design §9.3)

File: `operator/internal/metricsapi/poolstorage_test.go`

| #    | Scenario                                                                | Type     | Test                                           |
|------|-------------------------------------------------------------------------|----------|------------------------------------------------|
| U-84 | A pool with an object and a sample is served, carrying both ids         | Positive | `TestPoolGetServesAPoolWithASample`            |
| U-85 | A pool the control plane has not measured is not served                 | Negative | `TestPoolGetRefusesAPoolWithNoSample`          |
| U-86 | A pool whose cluster cannot be resolved is not served                   | Negative | `TestPoolGetRefusesAPoolWhoseClusterIsMissing` |
| U-87 | A namespaced list answers with that namespace's pools and no others     | Positive | `TestPoolListIsConfinedToItsNamespace`         |
| U-88 | One list queries each cluster once rather than once per pool            | Boundary | `TestPoolListQueriesEachClusterOnce`           |
| U-89 | No reachable Prometheus serves no readings rather than readings of zero | Negative | `TestPoolListServesNothingWithoutASource`      |
| U-90 | The table carries `Used` beside `Provisioned`                           | Positive | `TestPoolTableCarriesUsedAndProvisioned`       |

---

## 2. Integration Tests

Full reconcile loop against a real Kubernetes API server via `envtest`. The join
of design §5 and the cascade of §6 are between objects that carry no reference,
so real garbage collection is the only way to exercise them.

| #    | Scenario                                                                                 | Type     | Test |
|------|------------------------------------------------------------------------------------------|----------|------|
| I-01 | `spec.clusterRef` omitted: rejected as `Required`                                        | Negative | —    |
| I-02 | `spec.clusterRef` changed after creation: rejected as immutable                          | Negative | —    |
| I-03 | `spec.volumeDefaults` unset at creation, set later: accepted, then frozen                | Boundary | —    |
| I-04 | `spec.volumeDefaults` changed after being set: rejected as immutable                     | Negative | —    |
| I-05 | `spec.limits` changed after creation: accepted                                           | Positive | —    |
| I-06 | `spec.volumeDefaults.filesystem` outside the enum: rejected                              | Negative | —    |
| I-07 | `spec.limits.iops` negative: rejected by the minimum                                     | Boundary | —    |
| I-08 | `spec.allowedNodes` with a duplicate: rejected by `listType=set`                         | Negative | —    |
| I-09 | `StoragePoolOps.spec.action` outside the enum: rejected                                  | Negative | —    |
| I-10 | `StoragePoolOps.spec.poolRef` changed after creation: rejected                           | Negative | —    |
| I-11 | Short names `sp` and `spops` resolve to the same lists as the full kinds                 | Positive | —    |
| I-12 | Deleting the `StorageCluster` cascades to a pool with no bound volumes                   | Positive | —    |
| I-13 | Deleting the `StorageCluster` with a pool holding bound volumes: both stay `Terminating` | Negative | —    |
| I-14 | Removing the claims then releases the pool, then the cluster, in that order              | Positive | —    |
| I-15 | The class outlives a force-removed finalizer, and `status.storageClassName` finds it     | Negative | —    |
| I-16 | Two pools of one cluster: two classes, neither deleting the other's                      | Positive | —    |
| I-17 | Two pools with the same name in two namespaces: two classes, no collision                | Positive | —    |
| I-18 | An operation on one pool does not lock another pool of the same cluster                  | Negative | —    |
| I-19 | The controller's role covers `storageclasses` at cluster scope                           | Positive | —    |

---

## 3. End-to-End Tests

A live simplyblock cluster with a real data path.

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| E-01 | A pool created: a claim against its class binds and serves I/O                | Positive | —    |
| E-02 | `spec.volumeDefaults.enableCompression`: a volume is created compressed       | Positive | —    |
| E-03 | `spec.volumeDefaults.enableDHCHAP`: the NVMe-oF connection authenticates      | Positive | —    |
| E-04 | `spec.limits.capacity` reached: a further claim fails with a legible reason   | Boundary | —    |
| E-05 | `spec.limits.iops` reached: I/O is throttled rather than failing              | Boundary | —    |
| E-06 | Raising `spec.limits.capacity`: the previously failing claim now binds        | Positive | —    |
| E-07 | Deleting a pool with a bound claim: held, and the workload keeps serving I/O  | Negative | —    |
| E-08 | The class is deleted out of band: an existing volume keeps serving I/O        | Boundary | —    |
| E-09 | The class is deleted out of band: a new claim fails until it is restored      | Negative | —    |
| E-10 | `action: Suspend`: existing volumes keep serving, new claims do not provision | Positive | —    |
| E-11 | `action: Resume`: new claims provision again                                  | Positive | —    |
| E-12 | Narrowing `spec.allowedNodes` then `action: Rebalance`: volumes move          | Positive | —    |
| E-13 | Sustained fio through a `Rebalance`: no I/O errors, checksums match after     | Positive | —    |

---

## 4. Manual Scenarios

### M-01: Deleting a cluster whose pool has bound volumes

**Design reference:** §6.

**What to verify:** the decision this design takes on
[`design-crd-model.md`](../designs/crd-redesign/design-crd-model.md) §9.3's open question. The
owner reference means a cluster delete cascades, and the pool's finalizer is the
only thing that stops it destroying tenant data.

**What to verify specifically:** that the hold is legible. Correct behavior here
looks exactly like a stuck finalizer, and an administrator who reaches for
`--force` gets the outcome the hold exists to prevent.

**Test concept:**

1. Create a cluster, a pool, and a bound claim with data on it.
2. `kubectl delete storagecluster production`.
3. Confirm the command does not return, the cluster is `Terminating`, and the
   pool is `Terminating` behind it.
4. Confirm `VolumesStillBound` names the claim, and that `kubectl describe` on
   either object leads to it.
5. Confirm the workload is still serving I/O throughout.
6. Delete the claim and confirm the pool, then the cluster, complete without
   further input.
7. Confirm the data was not destroyed at any point before step 6.

### M-02: A class orphaned by a force-removed finalizer

**Design reference:** §5.

**What to verify:** the failure mode §5 names, which is that nothing but §6's hold
keeps a class and its pool consistent, and formerly that only the finalizer
deleted the class, so any path that skips it leaves a class whose `parameters`
name a pool that no longer exists.

**Test concept:**

1. Create a pool with no bound volumes.
2. Remove its finalizer by hand, so the object is deleted without cleanup.
3. Confirm the `StorageClass` still exists.
4. Confirm a new claim against it fails at provision time with an error from the
   control plane rather than from Kubernetes.
5. Confirm the orphan is findable: its labels still name the namespace, cluster,
   and pool, which is what a cleanup would select on.

### M-03: A pool at its capacity limit under load

**Design reference:** §3.1, §9.2.

**What to verify:** what a tenant experiences when a pool fills, which is the
question `simplyblock_storagepool_used_bytes` against `capacity_bytes` exists to give
warning of.

**Test concept:**

1. Create a pool with a small `spec.limits.capacity`.
2. Fill it with claims until the next one cannot be satisfied.
3. Confirm the failing claim reports a legible reason rather than pending
   silently.
4. Confirm existing volumes keep serving I/O.
5. Confirm `CapacityExhausted` is emitted on the pool, once rather than per
   attempt.
6. Raise the limit and confirm the pending claim binds without intervention.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 102       | 93      | 9           |
| Integration | 19        | 0       | 19          |
| E2E         | 13        | 0       | 13          |
| Manual      | 3         | 0       | 3           |
| **Total**   | **137**   | **93**  | **44**      |

**The unit class carries what it can, and the nine it does not are three
kinds of gap.** Five are `Rebalance`, which design §7 marks provisional and the
controller refuses at run time. Two are enforced by the API server rather than by
the operator, so a fake client applies neither and they belong to the integration
class. Two are ordinary and simply unwritten: a 5xx on creation (`U-10`) and a
pool with neither group set (`U-38`).

**Nothing in the integration, end-to-end, or manual classes is covered**, and the
reason is the same for all three: `envtest`, a live cluster, and a workload.
`M-01` is the one that matters most, because the hold it verifies is the only
thing standing between an owner reference and a `kubectl delete storagecluster`
that destroys tenant data, and no unit test reaches real garbage collection.

---

## 6. What Is Not Yet Covered

| #           | Gap                                             | Reason                                                                                                           |
|-------------|-------------------------------------------------|------------------------------------------------------------------------------------------------------------------|
| U-10        | A 5xx on creation                               | The 4xx path is covered and the retry is the same branch; the row is ordinary and simply unwritten               |
| U-38        | Neither limit group set                         | Likewise. The absent-group conversion is covered by `U-59`, and this is its controller-side twin                 |
| U-43        | Two reconcilers racing for one operation lock   | The same optimistic patch as `U-09`, which is covered; the ops-side race is unwritten                            |
| U-46 … U-49 | Every `Rebalance` step                          | Design §7 marks the action provisional. The controller refuses it in a terminal phase, which is what is tested   |
| U-50        | An unknown action                               | The `Enum` marker refuses one before the controller sees it, so the scenario belongs with `I-09`                 |
| U-52        | `Suspend` or `Resume` refused by the enum       | Likewise: the API server enforces it, and a fake client applies no enum                                          |
| I-01 … I-11 | Every admission rule                            | Needs `envtest`, because CEL, `Required`, and `listType=set` are the API server's and a fake client applies none |
| I-12 … I-19 | The cascade, the join, and namespace isolation  | Needs `envtest` for real garbage collection and real cluster-scoped objects                                      |
| E-01 … E-13 | All end-to-end scenarios                        | Needs a live cluster. The e2e harness under `test/` is not committed yet                                         |
| E-10 … E-12 | `Rebalance`                                     | Design §7 marks the action provisional, and it is a fan-out of `PersistentVolumeOps` rather than a backend call  |
| M-01 … M-03 | The cascade, an orphaned class, and a full pool | Need a real cluster, a force-removed finalizer, and a workload filling a pool                                    |

### Axis coverage

| Axis              | Value                            | Scenarios        |
|-------------------|----------------------------------|------------------|
| Pools per cluster | One                              | Most scenarios   |
|                   | Two                              | U-02, I-16, I-18 |
| Namespace count   | Single                           | Most scenarios   |
|                   | Multiple, same pool name         | U-03, I-17       |
| Cluster count     | One                              | Most scenarios   |
|                   | Two, same pool name              | U-04             |
| Bound volumes     | Zero                             | U-24, U-34       |
|                   | One                              | U-25, E-07, M-01 |
|                   | Released but not deleted         | U-32             |
|                   | Unmanaged, no `PersistentVolume` | U-33             |
| Deletion path     | Pool deleted directly            | U-24, U-25       |
|                   | Cluster deleted, cascading       | I-12, I-13, M-01 |
|                   | Finalizer removed by hand        | I-15, M-02       |
| Class state       | Present                          | U-19             |
|                   | Missing, restored                | U-20, E-09       |
|                   | Orphaned                         | I-15, M-02       |
| Capacity          | Below the limit                  | E-01             |
|                   | At the limit                     | E-04, M-03       |
|                   | Raised after being reached       | E-06, M-03       |

**The deletion-path axis is what this document exists to settle.** All three of
its values have rows, and `M-01` is the one that matters, because the hold it
verifies is the only thing standing between an owner reference and a
`kubectl delete storagecluster` that destroys tenant data.

**The class-state axis has an orphan value and no automated coverage of it.**
`I-15` and `M-02` are the only rows, and the orphan is reachable by any path that
skips the finalizer, which includes a force delete and a namespace deletion that
outruns it.
