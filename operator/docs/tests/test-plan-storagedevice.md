# Test Plan: StorageDevice and StorageDeviceOps

Related design: [`designs/crd-redesign/design-storagedevice.md`](../designs/crd-redesign/design-storagedevice.md)

Scope is the operator and the Kubernetes surface this repository builds. The
control plane (`sbcli`) is a dependency, faked at the boundary: what a row
asserts is the operator's response to a device list, never how a device is
managed.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason.

`StorageDevice`, its mirror, the readings of design §4.4, and the observability of
design §8 are implemented, and their rows name the tests that cover them.
`StorageDeviceOps` is not, so every operation row is a specification: what is still
unconfirmed is one control-plane verb per action, `self-test`, `fail`, `detach`, and
the adopt call, and those rows name endpoints nothing in this repository calls yet.

A row whose behavior the design has since dropped is struck through in place with
the ID that superseded it, because these numbers are cited from review history.

| Class       | Prefix | Harness                                                                |
|-------------|--------|------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, and a mock backend |
| Integration | `I-`   | Full reconcile loop against `envtest` and a mock backend               |
| E2E         | `E-`   | Live simplyblock cluster with real storage hardware                    |
| Manual      | `M-`   | Needs failure injection or hardware nobody minds losing                |

---

## 1. Unit Tests

The projection of design §5.1 is a pure function, so most of this kind's behavior
lands here: a device list in, a set of objects out.

### Object Projection (design §5.1)

Files: `operator/internal/controller/storagedevice_controller_unit_test.go`,
`operator/internal/controller/storagedevice_controller_reporting_test.go`, and
`operator/internal/cpinformer/subscriptions/device_test.go` for the naming the
subscription performs.

| #    | Scenario                                                                 | Type       | Test                                                     |
|------|--------------------------------------------------------------------------|------------|----------------------------------------------------------|
| U-01 | A node with four devices, no objects: four objects are created           | Positive   | —                                                        |
| U-02 | Each object is named `<node>-<short-device-id>` and is a valid DNS label | Positive   | `TestDeviceSubscriptionSnapshotCachesSyncsAndTriggers`   |
| U-03 | Each object carries a controller reference to its `StorageNode`          | Positive   | `TestStorageDeviceReconcileCreatesAndUpdates`            |
| U-04 | Each object carries the cluster, node, and worker labels                 | Positive   | `TestTheMirrorLabelsADeviceWithItsClusterNodeAndWorker`  |
| U-05 | A device whose object exists: the object is updated, not recreated       | Negative   | `TestStorageDeviceReconcileCreatesAndUpdates`            |
| U-06 | A node with no devices: no object is created and none is removed         | Boundary   | —                                                        |
| U-07 | A node with one device: one object                                       | Boundary   | `TestStorageDeviceReconcileCreatesAndUpdates`            |
| U-08 | Two nodes with a device of the same ID: two objects, no collision        | Boundary   | —                                                        |
| U-09 | A device list that has not changed: no status patch is issued            | Negative   | —                                                        |
| U-10 | A device ID long enough to overflow the name: truncated and still unique | Boundary   | —                                                        |
| U-77 | A label somebody stripped is restored, and a foreign label is kept       | Boundary   | `TestTheMirrorRestoresALabelSomebodyRemoved`             |
| U-81 | The object is created in the owning node's namespace, not the operator's | Regression | `TestTheMirrorCreatesTheDeviceInTheOwningNodesNamespace` |
| U-82 | A device whose node object is absent: no ownerless object is created     | Negative   | `TestStorageDeviceReconcileWaitsForItsNode`              |
| U-79 | A status write rejected with a conflict is retried, not surfaced         | Regression | `TestStorageDeviceStatusUpdateRetriesOnConflict`         |
| U-80 | A spec write rejected with a conflict is retried                         | Regression | `TestStorageDeviceSpecUpdateRetriesOnConflict`           |

### Status Mapping (design §4.2)

| #               | Scenario                                                                              | Type     | Test                                                                                                           |
|-----------------|---------------------------------------------------------------------------------------|----------|----------------------------------------------------------------------------------------------------------------|
| U-11            | An online device maps to phase `Online`                                               | Positive | `TestDevicePhaseFromStatus`                                                                                    |
| U-12            | A failed device maps to `Failed`                                                      | Positive | `TestDevicePhaseFromStatus`                                                                                    |
| U-13            | A device under test maps to `Degraded`, not `Failed`                                  | Boundary | `TestDevicePhaseFromStatus`                                                                                    |
| U-14            | A new device not yet in the layout maps to `Degraded`                                 | Boundary | `TestDevicePhaseFromStatus`                                                                                    |
| U-15            | A removed device maps to `Removed`                                                    | Positive | `TestDevicePhaseFromStatus`                                                                                    |
| U-16            | An unrecognized device status: preserved verbatim in `status.deviceStatus`            | Positive | `TestStorageDeviceReconcileCreatesAndUpdates`                                                                  |
| U-18            | The control plane reports no size: `capacity` stays absent, not zero                  | Boundary | `TestADeviceWithNoReportedSizeCarriesNoCapacity`                                                               |
| U-20            | The control plane reports no hardware fields: the block stays absent                  | Boundary | `TestADeviceWithNoHardwareFieldsCarriesNoHardware`                                                             |
| U-60            | A `Failed` device on an unreachable node: the phase and its reason are kept           | Boundary | `TestUnknownDoesNotOverwriteATerminalPhase`                                                                    |
| U-67            | `status.capacity` carries the device's size and nothing else                          | Positive | `TestStorageDeviceReconcileCreatesAndUpdates`                                                                  |
| U-68            | `status.hardware` carries the PCI address, serial, model, and NVMe controller         | Positive | `TestStorageDeviceReconcileCreatesAndUpdates`                                                                  |
| U-71            | `status.role` is `Journal` for a `JM_DEV` device and `Storage` for every other        | Positive | `TestDeviceRoleFromStatus`                                                                                     |
| U-72            | An online device with a failing health signal is `Degraded`, and the message names it | Positive | `TestTheMirrorSaysWhyTheDeviceIsDegraded`                                                                      |
| U-73            | An unrecognized status is `Unknown`, and the message quotes the status back           | Positive | `TestTheMirrorSaysWhyTheDeviceIsUnrecognized`                                                                  |
| U-76            | The mirror's own status rewrite does not clear `status.activeOpsRef`                  | Negative | `TestTheMirrorDoesNotClearTheOperationLock`                                                                    |
| ~~U-17~~        | ~~`status.capacity` carries the total and used bytes~~                                | —        | Superseded by U-67 and U-93: the used size left the status (design §4.2)                                       |
| ~~U-19~~        | ~~`status.hardware` carries the PCI address, serial, model, and namespace path~~      | —        | Superseded by U-68: the control plane reports no host path (design §4.2)                                       |
| ~~U-21~~        | ~~`status.role` reflects whether the device carries a journal, storage, or both~~     | —        | Superseded by U-71: the control plane reports one or the other, never both                                     |
| ~~U-22~~        | ~~A used-bytes value above the total: reported as given, not clamped~~                | —        | Superseded by U-96: the figure is served rather than stored                                                    |
| ~~U-58~~        | ~~An NVMe device: a PCI address and an `/dev/nvme*` path~~                            | —        | Superseded by U-68                                                                                             |
| ~~U-59~~        | ~~A logical block device: no PCI address, a `/dev/sd*` path~~                         | —        | Superseded by U-20                                                                                             |
| ~~U-63 … U-65~~ | ~~`sampledAt`, and the one-percent write threshold~~                                  | —        | Superseded by U-93 … U-102: a device cannot be resized, so there is no stream of samples to damp (design §4.2) |
| ~~U-66~~        | ~~No reachable metrics source: `capacity` absent, the rest of the status written~~    | —        | Superseded by U-95: the size comes from the stream, so no source costs the readings alone                      |

### A Device That Stops Being Reported (design §5.2)

| #    | Scenario                                                                      | Type     | Test                                                   |
|------|-------------------------------------------------------------------------------|----------|--------------------------------------------------------|
| U-23 | A device last reported as `removed`: the object is deleted, `DeviceRemoved`   | Positive | `TestARemovedDeviceIsDeletedWithoutAWarning`           |
| U-24 | A device that was serving until it vanished: deleted, `DeviceDisappeared`     | Negative | `TestAPulledDriveIsDeletedAndWarnedAbout`              |
| U-25 | Both events land on the `StorageNode`, not on the disappearing object         | Positive | `TestAPulledDriveIsDeletedAndWarnedAbout`              |
| U-26 | A scope whose first snapshot has not arrived: nothing is deleted              | Negative | `TestStorageDeviceReconcileWaitsForSyncBeforeDeleting` |
| U-27 | A cold cache is not read as every device disappearing                         | Negative | `TestStorageDeviceReconcileWaitsForSyncBeforeDeleting` |
| U-28 | An unreachable node reporting no devices: objects kept and moved to `Unknown` | Positive | `TestAnUnreachableNodesDevicesBecomeUnknownAndAreKept` |
| U-29 | A device that reappears: its object is recreated with the same name           | Boundary | —                                                      |
| U-78 | A restarting node's devices survive the restart rather than churning          | Boundary | `TestARestartingNodesDevicesAreKept`                   |
| U-83 | A synced scope on an online node saying nothing: the object is deleted        | Positive | `TestStorageDeviceReconcileDeletesWhenGoneAndSynced`   |
| U-84 | The `Unknown` transition is announced once, as `DeviceStateUnknown`           | Positive | `TestAnUnreachableNodesDevicesBecomeUnknownAndAreKept` |

### Deletion (design §5.3)

File: `operator/internal/webhook/storagedevice_validator_test.go`

| #    | Scenario                                                                   | Type     | Test                                                |
|------|----------------------------------------------------------------------------|----------|-----------------------------------------------------|
| U-30 | A user deleting an object: the webhook rejects it, with a reason           | Negative | `TestAUserMayNotDeleteAStorageDevice`               |
| U-31 | The operator's service account deleting an object: admitted                | Positive | `TestTheOperatorMayDeleteAStorageDevice`            |
| U-32 | The namespace controller's teardown of a terminating namespace: admitted   | Boundary | `TestTheNamespaceControllersTeardownIsAdmitted`     |
| U-33 | Deleting a `StorageNode` deletes its device objects                        | Positive | —                                                   |
| U-61 | The object carries no finalizer, so the operator's own delete is immediate | Boundary | —                                                   |
| U-62 | An operator delete does not call the control plane                         | Negative | —                                                   |
| U-85 | A service account of the same name in another namespace: refused           | Negative | `TestAServiceAccountElsewhereMayNotDelete`          |
| U-86 | A user's delete in a terminating namespace: admitted, whoever asked        | Boundary | `TestAUsersDeleteInATerminatingNamespaceIsAdmitted` |
| U-87 | A namespace whose state cannot be read: refused rather than admitted       | Boundary | `TestAnUnreadableNamespaceDoesNotOpenTheGuard`      |
| U-88 | A create, an update, or a connect: not this webhook's business             | Negative | `TestOtherOperationsAreNotThisWebhooksBusiness`     |

### Events and Gauges (design §8)

File: `operator/internal/controller/storagedevice_collector_test.go`, and the
mirror's own file for the events it emits.

| #     | Scenario                                                                     | Type     | Test                                                   |
|-------|------------------------------------------------------------------------------|----------|--------------------------------------------------------|
| U-89  | A device found for the first time: `DeviceDiscovered`, on the node           | Positive | `TestDiscoveryIsAnnouncedOnTheNode`                    |
| U-74  | A phase change is announced once, and a settled device announces nothing     | Positive | `TestAPhaseChangeIsAnnouncedOnceOnTheDevice`           |
| U-75  | A device that was `Online` when first seen is not announced as recovered     | Negative | `TestRecoveryIsAnnouncedButDiscoveryIsNot`             |
| U-90  | The size and the phase of each device are published as gauges                | Positive | `TestTheCollectorPublishesEachDevicesSizeAndPhase`     |
| U-91  | A device's phase gauge is 1 for its phase and 0 for every other              | Boundary | `TestTheCollectorPublishesEachDevicesSizeAndPhase`     |
| U-92  | The per-node device count and failed count match the objects                 | Positive | `TestTheCollectorCountsANodesDevicesAndItsFailedOnes`  |
| U-93  | What a device holds is published from Prometheus, one query per cluster      | Positive | `TestTheCollectorPublishesUsedBytesFromPrometheus`     |
| U-94  | No Prometheus: no used-bytes series, and every other gauge still published   | Negative | `TestWithoutPrometheusTheRestIsStillPublished`         |
| U-95  | A Prometheus that errors: the gauges that do not come from it are unaffected | Negative | `TestABrokenPrometheusDoesNotStopTheOtherGauges`       |
| U-96  | A device that went away leaves no series behind                              | Boundary | `TestTheCollectorDropsTheSeriesOfADeviceThatIsGone`    |
| U-97  | A device over its cluster's warning threshold: `DeviceNearlyFull`, once      | Positive | `TestADeviceOverItsClustersThresholdIsWarnedAboutOnce` |
| U-98  | A device that empties and fills again: a second crossing and a second event  | Boundary | `TestADeviceOverItsClustersThresholdIsWarnedAboutOnce` |
| U-99  | A device under the threshold: nothing is announced                           | Negative | `TestADeviceUnderTheThresholdIsNotWarnedAbout`         |
| U-100 | A cluster declaring no threshold: the default applies rather than no warning | Boundary | `TestAClusterWithNoThresholdFallsBackToTheDefault`     |

### The Readings (design §4.4)

File: `operator/internal/metricsapi/devicestorage_test.go`

| #     | Scenario                                                             | Type     | Test                                          |
|-------|----------------------------------------------------------------------|----------|-----------------------------------------------|
| U-101 | A device object with a sample: the reading joins the two             | Positive | `TestDeviceGetJoinsTheObjectWithItsSample`    |
| U-102 | The reading's timestamp is the control plane's sample time           | Positive | `TestDeviceGetJoinsTheObjectWithItsSample`    |
| U-103 | A device object with no sample: not found rather than zeros          | Boundary | `TestDeviceGetIsNotFoundWithoutASample`       |
| U-104 | No capacity source at all: nothing is served, by name or by list     | Negative | `TestDeviceReadingsAreAbsentWithoutAProvider` |
| U-105 | A name that is not a device's: not found, whatever Prometheus holds  | Negative | `TestDeviceGetIsNotFoundWithoutAnObject`      |
| U-106 | A namespaced list answers with that namespace's devices only         | Positive | `TestDeviceListIsConfinedToItsNamespace`      |
| U-107 | A cluster-wide list answers with every namespace's devices           | Positive | `TestDeviceListAcrossNamespaces`              |
| U-108 | Four devices of one cluster: one capacity query, not four            | Boundary | `TestDeviceListQueriesEachClusterOnce`        |
| U-109 | A Prometheus that errors: an empty list rather than a failed request | Negative | `TestDeviceListSurvivesABrokenProvider`       |
| U-110 | `kubectl get sdm` renders a row per reading, with a cell per column  | Positive | `TestDeviceTableRendersTheReading`            |
| U-111 | The resource is namespaced and answers to `sdm`                      | Positive | `TestDeviceStorageIdentity`                   |

### The Served Versions (design §4.4)

File: `operator/internal/metricsapi/scheme_test.go`

| #     | Scenario                                                                   | Type     | Test                                      |
|-------|----------------------------------------------------------------------------|----------|-------------------------------------------|
| U-112 | Each kind of the metrics group is registered under exactly its own version | Positive | `TestSchemeKnowsBothVersions`             |
| U-113 | `v1alpha2` is the preferred version discovery reports                      | Positive | `TestTheNewerVersionIsPreferred`          |
| U-114 | A reading round-trips through the codec with its kind intact               | Positive | `TestCodecRoundTripsADeviceReading`       |
| U-115 | Both versions install, each with its own resource                          | Positive | `TestBothVersionsInstall`                 |
| U-116 | The merged OpenAPI definitions cover both versions' kinds                  | Boundary | `TestOpenAPIDefinitionsCoverBothVersions` |
| U-117 | Every served version has an `APIService` whose CA bundle is injected       | Boundary | `TestEveryServedVersionGetsItsCABundle`   |

### StorageDeviceOps: Restart and Test (design §6)

File: `operator/internal/controller/storagedeviceops_controller_unit_test.go`,
which does not exist yet.

| #    | Scenario                                                                   | Type     | Test |
|------|----------------------------------------------------------------------------|----------|------|
| U-34 | The lock is free: acquired, phase becomes `Running`                        | Positive | —    |
| U-35 | Another operation holds the device's lock: this one stays `Pending`        | Negative | —    |
| U-36 | Two operations on two devices of one node run without contending           | Positive | —    |
| U-37 | Terminal re-reconcile: no side effect, the lock is released again          | Negative | —    |
| U-38 | The operation is deleted while `Running`: the finalizer releases the lock  | Positive | —    |
| U-39 | `Restart`: the call is issued and the step completes when the device is up | Positive | —    |
| U-40 | `Restart` on a device already restarting: no second call is issued         | Negative | —    |
| U-41 | `Restart` affects one device, and its node's other devices stay online     | Negative | —    |
| U-42 | The target device does not exist: the operation fails with a not-found     | Negative | —    |
| U-43 | The target device's node is offline: held, not failed                      | Negative | —    |
| U-44 | `Test` runs the redundancy check before taking the device out of service   | Positive | —    |
| U-45 | An unknown action: terminal failure with the action in the message         | Negative | —    |
| U-46 | Every declared state appears in the step `Enum` and in the CEL rule        | Boundary | —    |

### StorageDeviceOps: Remove and the Redundancy Check (design §6)

| #    | Scenario                                                                           | Type     | Test |
|------|------------------------------------------------------------------------------------|----------|------|
| U-47 | `Remove` with redundancy to spare: `Validating` advances, the device is removed    | Positive | —    |
| U-48 | `Remove` that would exhaust redundancy: `InsufficientRedundancy`, nothing removed  | Negative | —    |
| U-49 | `Remove` exactly at the redundancy limit: refused, since the next loss is fatal    | Boundary | —    |
| U-50 | `Remove` one below the limit: allowed                                              | Boundary | —    |
| U-51 | `Remove` of an already-failed device: allowed, since it contributes no redundancy  | Boundary | —    |
| U-52 | `spec.force` set: the redundancy check is skipped and the removal proceeds         | Negative | —    |
| U-53 | `status.faultToleranceBefore` records what the refusal was measured against        | Positive | —    |
| U-54 | The cluster reports no fault tolerance: the removal is refused, not assumed safe   | Boundary | —    |
| U-55 | An abort during `Validating`: `Aborted`, and nothing was removed                   | Positive | —    |
| U-56 | An abort during `Removing`: refused by the graph, the operation runs on            | Negative | —    |
| U-57 | `Remove` of a journal device: the same check applies, and the role is in the event | Boundary | —    |

---

## 2. Integration Tests

Full reconcile loop against a real Kubernetes API server via `envtest`.

| #    | Scenario                                                                          | Type     | Test |
|------|-----------------------------------------------------------------------------------|----------|------|
| I-01 | `spec.nodeRef` omitted: rejected as `Required`                                    | Negative | —    |
| I-02 | `spec.deviceID` omitted: rejected as `Required`                                   | Negative | —    |
| I-03 | `spec.nodeRef` changed after creation: rejected as immutable                      | Negative | —    |
| I-04 | `spec.deviceID` changed after creation: rejected as immutable                     | Negative | —    |
| I-05 | `spec.action` outside the enum: rejected                                          | Negative | —    |
| I-15 | `metrics.simplyblock.io/v1alpha2` answers a `storagedevicemetrics` list           | Positive | —    |
| I-16 | A `view` binding in the device's namespace can read the readings                  | Positive | —    |
| I-06 | `spec.deviceRef` changed after creation: rejected as immutable                    | Negative | —    |
| I-07 | Short names `sd` and `sdops` resolve to the same lists as the full kinds          | Positive | —    |
| I-08 | Deleting a `StorageNode` garbage-collects its device objects                      | Positive | —    |
| I-09 | Deleting a `StorageCluster` cascades through its nodes to their devices           | Positive | —    |
| I-10 | Selecting by the worker label returns that worker's devices only                  | Positive | —    |
| I-11 | Selecting by the node label returns that node's devices only                      | Positive | —    |
| I-12 | Two nodes in two namespaces with the same device ID: neither collides             | Negative | —    |
| I-13 | The print columns render node, phase, role, status, and size without a `describe` | Positive | —    |
| I-14 | The controller's role covers creating and deleting device objects                 | Positive | —    |

---

## 3. End-to-End Tests

A live cluster with real storage hardware. Two of these destroy capacity and are
marked accordingly.

| #    | Scenario                                                                           | Type     | Test |
|------|------------------------------------------------------------------------------------|----------|------|
| E-01 | A node comes up: one object appears per device, with capacity and hardware         | Positive | —    |
| E-02 | The hardware fields match what the host reports for the same device                | Positive | —    |
| E-03 | Writing to the cluster: the readings climb on the devices holding the data         | Positive | —    |
| E-04 | Capacity is unevenly distributed: one device is near full while the cluster is not | Boundary | —    |
| E-14 | A device crossing its cluster's threshold: `DeviceNearlyFull` on the device        | Positive | —    |
| E-05 | A device is pulled from a running node: `DeviceDisappeared`, the object is deleted | Negative | —    |
| E-06 | The node's own `3/4` summary and the four objects agree                            | Positive | —    |
| E-07 | `action: Restart` on a wedged device: it recycles and rejoins                      | Positive | —    |
| E-08 | The node's other devices keep serving I/O throughout that restart                  | Positive | —    |
| E-09 | `action: Test`: the device leaves service, is tested, and returns                  | Positive | —    |
| E-10 | `action: Remove` on a cluster with redundancy to spare (destructive)               | Positive | —    |
| E-11 | `action: Remove` on a cluster at its redundancy limit: refused (destructive setup) | Negative | —    |
| E-12 | A node restart: the objects survive rather than churning                           | Boundary | —    |
| E-13 | An eight-hundred-device fleet: listing and watching stay affordable                | Boundary | —    |

---

## 4. Manual Scenarios

### M-01: A drive is pulled from a running node

**Design reference:** §5.2, §8.1.

**What to verify:** the event that today has no expression anywhere in
Kubernetes. A drive pulled from a running node currently changes a node's count from
`4/4` to `4/3`, and nothing says which drive or why.

**Test concept:**

1. Note which device object corresponds to a physical drive, using
   `status.hardware.serialNumber`.
2. Pull that drive from the running node.
3. Confirm `DeviceDisappeared` is emitted on the `StorageNode` and names the
   device.
4. Confirm the object is deleted and the node's summary drops to `4/3`.
5. Confirm the remaining three objects are untouched and the node keeps serving.
6. Reinsert the drive and confirm the object is recreated with the same name.

### M-02: A device removed at the redundancy limit

**Design reference:** §6.

**What to verify:** the check that stands between a device removal and data loss.
Removing a device reduces fault tolerance, and removing one while another has
already failed can put a cluster below what its erasure coding requires.

**This scenario destroys capacity and should run on hardware nobody minds
losing.**

**Test concept:**

1. A cluster whose erasure coding tolerates one loss.
2. Fail one device, so the cluster is at its limit.
3. Create a `StorageDeviceOps` with `action: Remove` for a second, healthy device.
4. Confirm the operation reaches `Failed` with `InsufficientRedundancy`.
5. Confirm `status.faultToleranceBefore` records what the refusal was measured
   against.
6. Confirm the device is still in the layout and the cluster still serves I/O.
7. Repeat with `spec.force` set and confirm the removal proceeds, which is the
   behavior the field exists for and the one that needs a deliberate act.

### M-03: Restarting one device rather than its node

**Design reference:** §6.

**What to verify:** the blast-radius argument the kind exists for. A wedged
device is recycled today by restarting its node, which takes every other device on that
node down with it and costs the cluster a node's worth of redundancy.

**Test concept:**

1. A node with four devices, all serving, under a sustained fio workload.
2. Wedge one device, or pick one and restart it.
3. Create a `StorageDeviceOps` with `action: Restart` for that device.
4. Confirm the other three devices' objects stay `Online` throughout.
5. Confirm fio reports no I/O error and no verification failure.
6. Compare against restarting the whole node: record the redundancy the cluster
   loses in each case, which is the number that justifies the kind.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 105       | 72      | 33          |
| Integration | 16        | 0       | 16          |
| E2E         | 14        | 0       | 14          |
| Manual      | 3         | 0       | 3           |
| **Total**   | **138**   | **72**  | **66**      |

Superseded rows are excluded from the counts: their behavior is gone rather than
untested.

**What is covered is `StorageDevice` and what reads it, and what is not is
`StorageDeviceOps`.** The unit rows for the mirror, the guard, the collector, and
the readings name tests that run in the suite. Everything needing a cluster is
uncovered for the reason it always was, and the operation rows wait on design §11
Q1.

---

## 6. What Is Not Yet Covered

| #                 | Gap                                                                 | Reason                                                                                                                                                     |
|-------------------|---------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-01, U-07, U-08  | A whole node's devices at once, and a device id shared by two nodes | The mirror reconciles one object per device (design §5.1), so a node's four devices are four reconciles rather than one pass with a list to assert against |
| U-06, U-09        | An empty list, and a list that has not moved                        | Both are the reconcile that does nothing, which is asserted by its absence rather than by an output                                                        |
| U-10              | A device id long enough to overflow the name                        | `StorageDeviceShortIDLength` truncates to eight characters, and no row exercises two ids that agree in the first eight                                     |
| U-29              | A device that reappears                                             | Needs the delete and the recreate in one case, which the per-object reconciler makes two reconciles with a cache change between them                       |
| U-33, U-61, U-62  | The cascade, the absent finalizer, and what a delete does not call  | Garbage collection is the API server's, so `I-08` is where it belongs                                                                                      |
| U-34 … U-57       | Every `StorageDeviceOps` row                                        | The kind does not exist (design §6), and design §11 Q1 records the endpoints as unconfirmed                                                                |
| I-01 … I-16       | Admission, cascade, label selection, and the aggregated routes      | Needs `envtest`, because `Required`, immutability, and garbage collection are the API server's                                                             |
| E-01 … E-14       | All end-to-end scenarios                                            | Needs a live cluster with real storage hardware. The e2e harness under `test/` is not committed yet                                                        |
| E-02              | Hardware fields matching the host                                   | The fields are published from the stream, so this row is what checks they describe the drive somebody is holding                                           |
| E-10, E-11        | `Remove` end to end                                                 | Destroys capacity. Needs hardware somebody is willing to lose                                                                                              |
| M-01 … M-03       | A pulled drive, a removal at the limit, and a device restart        | Need physical access, a cluster at its redundancy limit, and a sustained workload                                                                          |
| Operation metrics | The two `operations_*` metrics of design §8.2                       | They have an action and a result to report only once §6 exists                                                                                             |
| Operation events  | Every `StorageDeviceOps` row of design §8.1                         | Same                                                                                                                                                       |
| Q1                | Which per-action verbs the control plane offers                     | Design §11 records it as unconfirmed, and the operation rows assume all four                                                                               |
| Q2                | Whether an operation may target a device in `Unknown`               | Design §11 leaves it open, so no row asserts what `Validating` does with one                                                                               |
| Q3                | Whether the control plane can name the operation behind a removal   | Design §11 leaves it open. `U-23` and `U-24` assert the proxy the stream supports, which is the last status the device was reported in                     |

### Axis coverage

| Axis                  | Value                          | Scenarios        |
|-----------------------|--------------------------------|------------------|
| Devices per node      | Zero                           | U-06             |
|                       | One                            | U-07             |
|                       | Four                           | U-01, E-01, M-03 |
|                       | Eight hundred across a fleet   | E-13             |
| Device state          | Online                         | U-11             |
|                       | Degraded, testing or new       | U-13, U-14       |
|                       | Failed                         | U-12, U-51       |
|                       | Removed                        | U-15             |
|                       | Unrecognized                   | U-16             |
| Device role           | Storage                        | U-71             |
|                       | Journal                        | U-71, U-57       |
| Redundancy headroom   | Spare                          | U-47, E-10       |
|                       | Exactly at the limit           | U-49, E-11, M-02 |
|                       | One below the limit            | U-50             |
|                       | Forced past the check          | U-52, M-02       |
|                       | Unreported                     | U-54             |
| Disappearance cause   | Last reported as removed       | U-23             |
|                       | Pulled physically              | U-24, E-05, M-01 |
|                       | A cold cache                   | U-26, U-27       |
|                       | Node unreachable               | U-28, E-12       |
|                       | Node restarting                | U-78             |
| Capacity distribution | Even                           | E-03             |
|                       | One device near full           | E-04             |
| Capacity sampling     | A device with a sample         | U-93, U-101      |
|                       | A device with none             | U-103            |
|                       | No reachable source            | U-94, U-104      |
|                       | A source that errors           | U-95, U-109      |
| Occupancy against the | Under it                       | U-99             |
| cluster's threshold   | Over it, first crossing        | U-97             |
|                       | Over it, still over            | U-98             |
|                       | Over it, no threshold declared | U-100            |
| Delete identity       | A user                         | U-30             |
|                       | The operator's service account | U-31             |
|                       | A service account elsewhere    | U-85             |
|                       | The namespace controller       | U-32             |
| Namespace state       | Live                           | U-30, U-31       |
|                       | Terminating                    | U-32, U-86       |
|                       | Unreadable                     | U-87             |
| Namespace count       | Single                         | Most scenarios   |
|                       | Multiple                       | I-12             |

**The redundancy-headroom axis is the one that matters and it has five values,
all specified and none covered.** `U-48` to `U-52` and `M-02` are the arithmetic
and the act that stand between a device removal and losing data, and the row
sitting exactly at the limit is the one an implementation is most likely to get
wrong by using `>=` where it meant `>`. Every row of it waits on design §6.

**The disappearance-cause axis exists because four of its five values look
identical to the operator.** A device removed deliberately, a device pulled, a
cold cache, and a node that is unreachable or restarting all present as a device
missing from a list, and only the first two are a device that is gone. `U-26`,
`U-27`, `U-28`, and `U-78` are what stop the other three deleting a fleet's worth
of objects.

**The delete-identity and namespace-state axes are one guard read two ways.**
Design §5.3 refuses a deletion by who asked and admits one by what is happening to
the namespace, so the two axes cross: the namespace controller is admitted because
of the namespace and the operator because of the identity, and a user is admitted
only where the namespace is already going away.
