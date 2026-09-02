# Test Plan: StorageDevice and StorageDeviceOps

Related design: [`designs/crd-redesign/design-storagedevice.md`](../designs/crd-redesign/design-storagedevice.md)

Scope is the operator and the Kubernetes surface this repository builds. The
control plane (`sbcli`) is a dependency, faked at the boundary: what a row
asserts is the operator's response to a device list, never how a device is
managed.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason.

Both kinds are new, so every row is a specification rather than a gap against
shipped behavior. Nothing here has a shipped spelling to name.

**What this plan rests on is narrower than it was.** Design §7 confirms the device
list and the hardware fields, so the kind has something to build objects from and
something to publish. What is still unconfirmed is one control-plane verb per action:
`self-test`, `fail`, `detach`, and the adopt call. The operation rows below therefore
name endpoints nothing in this repository calls yet.

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

File: `operator/internal/controllers/node/storagedevice_projection_test.go`

| #    | Scenario                                                                 | Type     | Test |
|------|--------------------------------------------------------------------------|----------|------|
| U-01 | A node with four devices, no objects: four objects are created           | Positive | —    |
| U-02 | Each object is named `<node>-<short-device-id>` and is a valid DNS label | Positive | —    |
| U-03 | Each object carries a controller reference to its `StorageNode`          | Positive | —    |
| U-04 | Each object carries the cluster, node, and worker labels                 | Positive | —    |
| U-05 | A device whose object exists: the object is updated, not recreated       | Negative | —    |
| U-06 | A node with no devices: no object is created and none is removed         | Boundary | —    |
| U-07 | A node with one device: one object                                       | Boundary | —    |
| U-08 | Two nodes with a device of the same ID: two objects, no collision        | Boundary | —    |
| U-09 | A device list that has not changed: no status patch is issued            | Negative | —    |
| U-10 | A device ID long enough to overflow the name: truncated and still unique | Boundary | —    |

### Status Mapping (design §4.2)

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| U-11 | An online device maps to phase `Online`                                       | Positive | —    |
| U-12 | A failed device maps to `Failed`                                              | Positive | —    |
| U-13 | A device under test maps to `Degraded`, not `Failed`                          | Boundary | —    |
| U-14 | A new device not yet in the layout maps to `Degraded`                         | Boundary | —    |
| U-15 | A removed device maps to `Removed`                                            | Positive | —    |
| U-16 | An unrecognized device status: preserved verbatim in `status.deviceStatus`    | Positive | —    |
| U-17 | `status.capacity` carries the total and used bytes                            | Positive | —    |
| U-18 | The control plane returns no capacity: the fields stay absent, not zero       | Boundary | —    |
| U-19 | `status.hardware` carries the PCI address, serial, model, and namespace path  | Positive | —    |
| U-20 | The control plane returns no hardware fields: the block stays absent          | Boundary | —    |
| U-58 | An NVMe device: a PCI address and an `/dev/nvme*` path                        | Positive | —    |
| U-59 | A logical block device: no PCI address, a `/dev/sd*` path                     | Positive | —    |
| U-60 | A `Failed` device on an offline node: the phase is kept, not `Unknown`        | Boundary | —    |
| U-21 | `status.role` reflects whether the device carries a journal, storage, or both | Positive | —    |
| U-22 | A used-bytes value above the total: reported as given, not clamped            | Boundary | —    |

### A Device That Stops Being Reported (design §5.2)

| #    | Scenario                                                                       | Type     | Test |
|------|--------------------------------------------------------------------------------|----------|------|
| U-23 | A device removed by an operation: the object is deleted, `DeviceRemoved`       | Positive | —    |
| U-24 | A device that disappears with no operation: deleted, `DeviceDisappeared`       | Negative | —    |
| U-25 | Both events land on the `StorageNode`, not on the disappearing object          | Positive | —    |
| U-26 | A device list that is empty because the control plane errored: nothing deleted | Negative | —    |
| U-27 | A control-plane error is not read as every device disappearing                 | Negative | —    |
| U-28 | An offline node reporting no devices: objects kept and moved to `Unknown`      | Positive | —    |
| U-29 | A device that reappears: its object is recreated with the same name            | Boundary | —    |

### Deletion (design §5.3)

| #    | Scenario                                                                    | Type     | Test |
|------|-----------------------------------------------------------------------------|----------|------|
| U-30 | A user deleting an object: the webhook rejects it                           | Negative | —    |
| U-31 | The operator's service account deleting an object: admitted                 | Positive | —    |
| U-32 | A delete while the namespace is terminating: admitted, so teardown finishes | Boundary | —    |
| U-33 | Deleting a `StorageNode` deletes its device objects                         | Positive | —    |
| U-61 | The object carries no finalizer, so the operator's own delete is immediate  | Boundary | —    |
| U-62 | An operator delete does not call the control plane                          | Negative | —    |

### StorageDeviceOps: Restart and Test (design §6)

File: `operator/internal/controllers/node/storagedeviceops_controller_unit_test.go`

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

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| I-01 | `spec.nodeRef` omitted: rejected as `Required`                                | Negative | —    |
| I-02 | `spec.deviceID` omitted: rejected as `Required`                               | Negative | —    |
| I-03 | `spec.nodeRef` changed after creation: rejected as immutable                  | Negative | —    |
| I-04 | `spec.deviceID` changed after creation: rejected as immutable                 | Negative | —    |
| I-05 | `spec.action` outside the enum: rejected                                      | Negative | —    |
| I-06 | `spec.deviceRef` changed after creation: rejected as immutable                | Negative | —    |
| I-07 | Short names `sd` and `sdops` resolve to the same lists as the full kinds      | Positive | —    |
| I-08 | Deleting a `StorageNode` garbage-collects its device objects                  | Positive | —    |
| I-09 | Deleting a `StorageCluster` cascades through its nodes to their devices       | Positive | —    |
| I-10 | Selecting by the worker label returns that worker's devices only              | Positive | —    |
| I-11 | Selecting by the node label returns that node's devices only                  | Positive | —    |
| I-12 | Two nodes in two namespaces with the same device ID: neither collides         | Negative | —    |
| I-13 | The print columns render node, phase, role, and capacity without a `describe` | Positive | —    |
| I-14 | The controller's role covers creating and deleting device objects             | Positive | —    |

---

## 3. End-to-End Tests

A live cluster with real storage hardware. Two of these destroy capacity and are
marked accordingly.

| #    | Scenario                                                                           | Type     | Test |
|------|------------------------------------------------------------------------------------|----------|------|
| E-01 | A node comes up: one object appears per device, with capacity and hardware         | Positive | —    |
| E-02 | The hardware fields match what the host reports for the same device                | Positive | —    |
| E-03 | Writing to the cluster: `usedBytes` climbs on the devices holding the data         | Positive | —    |
| E-04 | Capacity is unevenly distributed: one device is near full while the cluster is not | Boundary | —    |
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
| Unit        | 57        | 0       | 57          |
| Integration | 14        | 0       | 14          |
| E2E         | 13        | 0       | 13          |
| Manual      | 3         | 0       | 3           |
| **Total**   | **87**    | **0**   | **87**      |

Nothing is covered, and nothing can be: neither kind exists. The device API it reads
from does (design §7), so this plan is a specification of a kind that can be built
rather than one that may turn out not to be, and the operation rows are the ones whose
endpoints still have to appear.

---

## 6. What Is Not Yet Covered

| #                       | Gap                                                          | Reason                                                                                                                 |
|-------------------------|--------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------|
| U-01 … U-10             | Object projection                                            | The kind does not exist. These are the rows to write first, because the projection is pure                             |
| U-11 … U-22             | Status mapping                                               | The kind does not exist. Design §7 confirms the hardware fields are reported, so `U-19` and `U-20` are unblocked       |
| U-23 … U-29             | A device that stops being reported                           | The kind does not exist. `U-26` to `U-28` are the rows that keep a control-plane error from deleting a fleet's objects |
| U-30 … U-33, U-61, U-62 | Deletion, the protection, and the cascade                    | The kind does not exist                                                                                                |
| U-34 … U-46             | `Restart` and `Test`                                         | `StorageDeviceOps` does not exist, and design §7 records the endpoints as unverified                                   |
| U-47 … U-57             | `Remove` and the redundancy check                            | The kind does not exist. `U-48` to `U-51` are the arithmetic that stands between a removal and data loss               |
| I-01 … I-14             | Admission, cascade, and label selection                      | Needs `envtest`, because `Required`, immutability, and garbage collection are the API server's                         |
| E-01 … E-13             | All end-to-end scenarios                                     | Needs a live cluster with real storage hardware. The e2e harness under `test/` is not committed yet                    |
| E-02                    | Hardware fields matching the host                            | Design §7 confirms the fields are reported, so this row verifies they match reality kind is useful in an incident      |
| E-10, E-11              | `Remove` end to end                                          | Destroys capacity. Needs hardware somebody is willing to lose                                                          |
| M-01 … M-03             | A pulled drive, a removal at the limit, and a device restart | Need physical access, a cluster at its redundancy limit, and a sustained workload                                      |
| Metrics                 | The seven metrics of design §8.2                             | Designed, not built                                                                                                    |
| Events                  | The twelve reasons of design §8.1                            | Designed, not built                                                                                                    |
| Q1                      | Whether the control plane exposes devices                    | Design §11 records it as unverified. Every row here assumes it does                                                    |
| Q3                      | Whether `Remove` should rewrite the node's device list       | Design §11 leaves it open, so no row asserts what happens to `spec.config.deviceNames` after a removal                 |
| Deletion                | Objects survive an offline node                              | Design §5.2 settles it: they are kept and moved to `Unknown`, which `U-28` and `U-60` assert                           |

### Axis coverage

| Axis                  | Value                        | Scenarios        |
|-----------------------|------------------------------|------------------|
| Devices per node      | Zero                         | U-06             |
|                       | One                          | U-07             |
|                       | Four                         | U-01, E-01, M-03 |
|                       | Eight hundred across a fleet | E-13             |
| Device state          | Online                       | U-11             |
|                       | Degraded, testing or new     | U-13, U-14       |
|                       | Failed                       | U-12, U-51       |
|                       | Removed                      | U-15             |
|                       | Unrecognized                 | U-16             |
| Device role           | Storage                      | U-21             |
|                       | Journal                      | U-57             |
| Redundancy headroom   | Spare                        | U-47, E-10       |
|                       | Exactly at the limit         | U-49, E-11, M-02 |
|                       | One below the limit          | U-50             |
|                       | Forced past the check        | U-52, M-02       |
|                       | Unreported                   | U-54             |
| Disappearance cause   | Removed by an operation      | U-23             |
|                       | Pulled physically            | U-24, E-05, M-01 |
|                       | Control-plane error          | U-26, U-27       |
|                       | Node offline                 | U-28, E-12       |
| Capacity distribution | Even                         | E-03             |
|                       | One device near full         | E-04             |
| Namespace count       | Single                       | Most scenarios   |
|                       | Multiple                     | I-12             |

**The redundancy-headroom axis is the one that matters and it has five values,
all covered.** `U-48` to `U-52` and `M-02` are the arithmetic and the act that
stand between a device removal and losing data, and the row sitting exactly at the
limit is the one an implementation is most likely to get wrong by using `>=` where
it meant `>`.

**The disappearance-cause axis exists because three of its four values look
identical to the operator.** A device removed deliberately, a device pulled, and
a control-plane error all present as a device missing from a list, and only the
first should be silent. `U-26` and `U-27` are what stop one bad response deleting
a fleet's worth of objects.
