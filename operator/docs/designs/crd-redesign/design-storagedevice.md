# Design Document: The StorageDevice and Its Operations

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-30  
**Test Plan:** [`tests/test-plan-storagedevice.md`](../../tests/test-plan-storagedevice.md)

Both kinds are new. Nothing in this document exists, and §10 is what it replaces
rather than a migration. It also fills in the far side of the `StorageNode` owns
`StorageDevice` edge that [`design-crd-model.md`](design-crd-model.md) §9.3 draws.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [One Object Per Device](#3-one-object-per-device)
4. [StorageDevice: API](#4-storagedevice-api)
5. [StorageDevice: Controller](#5-storagedevice-controller)
6. [StorageDeviceOps](#6-storagedeviceops)
7. [Backend API Requirements](#7-backend-api-requirements)
8. [Observability](#8-observability)
9. [Testing Strategy](#9-testing-strategy)
10. [What This Replaces](#10-what-this-replaces)
11. [Open Questions](#11-open-questions)

Appendices:

- [Appendix A: `storagedevice_types.go`](#appendix-a-storagedevice_typesgo)
- [Appendix B: `storagedeviceops_types.go`](#appendix-b-storagedeviceops_typesgo)

---

## Overview

`StorageDevice` is one storage backend device belonging to one storage node,
expressed as a Kubernetes resource. `StorageDeviceOps` is a single operation against
one of them, and it is the narrowest blast radius in the ownership spine: restarting
one device rather than one node, or one node rather than a cluster.

**A device is whatever the backend presents as one, which is not only an NVMe drive.**
Every device in the field today is an NVMe SSD addressed by a PCI address and served
through an NVMe namespace, and 26.4 adds logical block devices, which is where a
spinning disk behind a SCSI or SATA controller becomes a device this kind represents.
So the kind is named for the layer it observes rather than for the transport underneath
it, and §4.2 is where the difference shows: what is common to every device is its
identity, its capacity, its role, and its phase, and what is not is how the host
addresses it.

The kind is the bottom of the spine
([`design-crd-model.md`](design-crd-model.md) §5) and the last one in the target
model without a design.

---

## 1. Background

Per-device state has one field today.
`StorageNode.status.resources.devices` counts how many of a node's devices are
online and how many it has
([`design-storagenode.md`](design-storagenode.md) §3.3). That is a summary and it
is the right summary, and it answers none of the questions an administrator asks
about a device.

**Which device is the one that failed.** A node reporting three of four cannot
say which. The answer exists in the control plane and reaches Kubernetes nowhere.

**How much capacity a device has, and how much of it is used.** Cluster capacity
is the sum of its devices, and a cluster approaching its threshold has one or two
devices doing the approaching. Neither the node nor the cluster reports per-device
capacity.

**What state a device is in beyond up or down.** The control plane distinguishes
a device that is new and not yet part of the layout, one that is being tested,
one that has been removed from service, and one that has failed, and all four
count identically toward `online/total`.

**Whether a device can be operated on individually.** Restarting a wedged device
today means restarting its node, which takes every other device on that node down
with it. That is the blast-radius argument
[`design-crd-model.md`](design-crd-model.md) §8.2 makes for having a level at all.

---

## 2. Goals and Non-Goals

### Goals

- Specify the kind, its identity, and how it is discovered rather than declared
  (§3, §4).
- Specify the operations a device has that a node's operations cannot express
  (§6).
- Specify what stays on the node, so that adding this kind does not make the
  node's summary redundant (§3).

### Non-Goals

- **Not device selection.** Which physical devices a node uses is
  `StorageNode.spec.config`, decided at creation and mostly immutable
  ([`design-storagenode.md`](design-storagenode.md) §3.1, §3.2). This kind
  reports the devices a node ended up with, and does not choose them.
- **Not partitioning.** How a device is carved into a journal slice and a storage
  slice, and what `enableJournalDevice` does, belong to the node's configuration.
- **Not SMART or firmware.** Device health beyond what the control plane reports
  is the host's business and a node-exporter's, not this operator's.
- **Not the API group's conventions.** The entity and action split, the enum
  casing, the lock, and the observed generation belong to
  [`design-crd-model.md`](design-crd-model.md) and are cited rather than restated.

---

## 3. One Object Per Device

A device is an entity of this API group and takes the group's shape for one: an object
with an identity, and an `Ops` companion for the operations against it
([`design-crd-model.md`](design-crd-model.md) §3). What that gives, and what it costs,
is worth stating once because both are properties later sections rely on.

**A device has a target.** `StorageDeviceOps` names one object, and that object's
identity is `(nodeRef, deviceID)` rather than a position in anything (§4.1). An
operation therefore keeps its target when the node's device list is reordered, and a
controller waiting on one device coming back is woken rather than diffing an array.

**A device has a lifecycle.** A drive that is pulled leaves a deletion, which is an
event with a timestamp and a name, and one only the operator can issue (§5.2, §5.3).

**The count on the node stays.** `status.resources.devices` is the summary, and a
summary is worth having beside an inventory: `kubectl get sn` showing `3/4` is what an
administrator scans, and the four objects are what they open when the ratio is wrong.
A summary and the things it summarizes answer different questions.

**The object count is bounded by hardware, which is why no retention is needed.** A
hundred-node fleet with eight devices each is eight hundred objects, and the number
changes when a drive is added or pulled rather than growing with time. That is the
difference between an object per physical part and an object per event, and it is why
this kind needs no retention policy while the `Ops` kinds do
([`design-persistentvolumeops.md`](design-persistentvolumeops.md) §11.2).

---

## 4. StorageDevice: API

Declared in `operator/api/v1alpha1/storagedevice_types.go`, short name `sd`. The
type is Appendix A.

### 4.1 Identity

**A device object is created by the operator, never by a user.** Its existence
follows from the node having the device, which is what makes it an observation
with an operable target rather than a declaration.

```go
// NodeRef names the StorageNode this device belongs to. The node owns this
// object by controller reference, so deleting the node deletes its devices.
// +kubebuilder:validation:Required
// +k8s:immutable
NodeRef string `json:"nodeRef"`

// DeviceID is the control plane's identifier for the device. With NodeRef it is
// the whole of this object's identity.
// +kubebuilder:validation:Required
// +k8s:immutable
DeviceID string `json:"deviceID"`
```

The object is named `<node>-<short-device-id>`, which keeps it a valid DNS label
and keeps a device's objects sorted next to its node's in a listing.

**The spec has two fields and neither is a setting.** Everything a user could
want to change about a device is on the node
([`design-storagenode.md`](design-storagenode.md) §3.1), and a spec that
duplicated it would be a second place to set the same thing. This is an entity
whose spec is pure identity, which is unusual in the group and correct here.

### 4.2 Status

`status.phase` is the operator's own view: `Online`, `Degraded`, `Unknown`, `Removed`,
or `Failed`. `status.deviceStatus` is the control plane's own string, kept in its
spelling for the reason
[`design-crd-model.md`](design-crd-model.md) §7.8 gives.

**`Degraded` and `Failed` are different and the difference is what the kind is
for.** A degraded device is serving and should not be: it is testing, resyncing,
or reporting errors. A failed device is not serving, and the cluster is operating
with less redundancy than it thinks it has until it is replaced.

**`Failed` is terminal.** A device does not leave it, whether it arrived there on the
control plane's judgment or on somebody's (§6). `Degraded` is the phase a device
recovers from and `Failed` is the phase it is replaced from, and keeping the two distinct
is what stops a suspect device rejoining the layout on the strength of one good
reconcile.

`status.capacity` groups the size, the used bytes, and the derived ratio.

`status.hardware` groups what the device is: its type, its serial number, its model,
the path the host sees it at, and its PCI address where it has one. Those are what
identify the part somebody has to walk into a datacenter and replace, and none of them
is recoverable from the control plane's device ID alone.

**Every field in the group is optional, and which ones are populated is what says how
the device is attached.** An NVMe drive reports a PCI address and a path such as
`/dev/nvme0n1`. A logical block device reports a path such as `/dev/sdb` and no PCI
address, because the address belongs to the controller it hangs off rather than to the
device. The control plane reports no device type, so the presence of `pciAddress` is the
distinction, and this design adds no type field to restate what one field's presence
already says.

**No role is restricted by how a device is attached.** A journal, a storage slice, or
both are what `status.role` reports, and nothing ties those to the transport. Whether a
journal on a spinning disk is a placement anybody wants is a question for whoever
configures the node rather than a constraint this kind enforces.

**`devicePath` is the host path rather than an NVMe namespace.** The value is
`/dev/nvme0n1` for an NVMe namespace and `/dev/sdb` for a logical block device, and the
field is named for what it holds in both cases: the path a person types into `smartctl`
or `dd`. Naming it for the NVMe spelling would make the field read as inapplicable on
half the devices it describes.

`status.role` says whether the device carries a journal, a storage slice, or
both, which is what `enableJournalDevice` decided at node creation and what makes
one device's failure worse than another's.

`status.activeOpsRef` is the operation lock
([`design-crd-model.md`](design-crd-model.md) §3.2).

### 4.3 Example

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageDevice
metadata:
  name: production-7f3a9c-5e0000
  namespace: simplyblock
  labels:
    storage.simplyblock.io/cluster: production
    storage.simplyblock.io/node: production-7f3a9c
    storage.simplyblock.io/worker: worker-3
  ownerReferences:
    - apiVersion: storage.simplyblock.io/v1alpha1
      kind: StorageNode
      name: production-7f3a9c
      controller: true
spec:
  nodeRef: production-7f3a9c
  deviceID: 5e0000a1-3b2c-4d5e-9f01-2a3b4c5d6e7f
status:
  phase: Online
  deviceStatus: online
  role: Storage
  capacity:
    totalBytes: 3840755982336
    usedBytes: 1920377991168
  hardware:
    pciAddress: "0000:5e:00.0"
    serialNumber: S4J9NX0R500123
    model: SAMSUNG MZQL23T8HCLS-00A07
    devicePath: /dev/nvme0n1
  observedGeneration: 1
```

A logical block device on the same node differs only in `status.hardware`, which is
the point of the grouping:

```yaml
status:
  phase: Online
  deviceStatus: online
  role: Storage
  capacity:
    totalBytes: 16000900661248
  hardware:
    serialNumber: WD-WMC4N0D9AXYZ
    model: WDC WUH721816ALE6L4
    devicePath: /dev/sdb
  observedGeneration: 1
```

**No PCI address, a different path, and everything above `hardware` unchanged.** The
phase, the role, the capacity, the lock, and the operations all mean the same thing on
a spinning disk as on a drive, which is why the transport lives in one group rather
than in the kind's shape.

**The worker label is what makes the kind usable in an incident.**
`kubectl get sd -l storage.simplyblock.io/worker=worker-3` is how somebody with a
failed drive in their hand finds what it was, and it is the reason the label
duplicates something already reachable through the node.

---

## 5. StorageDevice: Controller

`StorageDeviceReconciler`, in
`operator/internal/controllers/node/storagedevice_controller.go`.

### 5.1 Devices are discovered

Nothing declares a device, which is unusual for a custom resource and is the whole
of this kind's creation path: the node's reconciler lists the control plane's devices
for the node and reconciles the objects to match.

```
  StorageNode reconcile, or a device stream event
    │
    ▼
  List the node's devices
    │
    ▼
  For each device with no object   → create one, owned by the node
  For each object with a live device → update its status
  For each object with no device     → §5.2
```

**The device stream is scoped per cluster**, so one subscription serves every
device of every node in it ([`design-crd-model.md`](design-crd-model.md) §7.7),
which is what keeps eight hundred objects affordable.

### 5.2 A device that stops being reported

A device leaves the control plane's list when it is removed from the node, which
happens when somebody pulls it or when a `StorageDeviceOps` removes it.

**The object is deleted, and the event goes on the node.** A device object with
no device is a record of hardware that is no longer there, and unlike a backup or
a task it is not a record of anything that happened. `DeviceRemoved` is emitted on
the `StorageNode`, which is the object that still exists and the one an
administrator has open.

**A device that stops being reported without an operation having removed it is a
warning.** That is a drive that was pulled, or a node that lost sight of one, and
`DeviceDisappeared` says so rather than silently deleting the object.

**A device on a node that cannot be reached is `Unknown`, and its object stays.** An
offline node reports no devices, which is not the same statement as a node reporting
that a device is gone: the first is an absence of information and the second is
information. So the objects of an unreachable node are kept and moved to `Unknown`,
which says the operator cannot see the device rather than that the device is bad, and
they take whatever the node reports when it returns. Deleting and rebuilding them would
churn one object per device on every node restart and discard the identity §5.3 exists
to protect.

**A device already in a terminal phase keeps it.** `Failed` does not become `Unknown`
when the node goes away, because that phase records a judgment an unreachable node does
not revoke (§6). `Unknown` replaces `Online` and `Degraded`, which are observations of a
device that was serving.

### 5.3 Deletion is the operator's

**A user cannot delete a `StorageDevice`.** The object is discovered rather than
declared (§5.1), so it is not a request anybody made and not theirs to withdraw. A
validating webhook refuses `DELETE` from every identity except a service account in
the operator's namespace, which is the same identity test the node's own webhook
applies to its operator-written fields
([`design-storagenode.md`](design-storagenode.md) §3.2).

**The object carries state that only its own continuity holds.**
`status.activeOpsRef` is the lock a `StorageDeviceOps` acquired, the UID is what
anything referring to the device recorded, and the phase is an observation with a
history. A replacement object of the same name has none of that: the lock is released
without its holder knowing, an operation in flight names a target that no longer
exists, and every watcher sees a delete and a create while the hardware sat
untouched. Refusing the deletion is what keeps the object's identity as durable as
the device it describes.

**The operator deletes them for two reasons and no others.** The device stopped being
reported, which is §5.2, and the node was deleted, which garbage-collects them through
the owner reference. That edge is the one
[`design-crd-model.md`](design-crd-model.md) §9.3 lists as depending on this kind
existing, and this document establishes it.

**No finalizer.** With the delete refused there is nothing to gate: the operator's own
deletions are the only ones that happen, and each is a record being removed after the
thing it recorded is already gone. A finalizer would add a step to the one path that is
correct by construction.

**One case the webhook must let through, or it deadlocks a namespace.** Deleting the
namespace makes Kubernetes delete the objects in it, and those deletes do not come from
the operator: a webhook that refuses them leaves the namespace in `Terminating` forever.
So the rejection is conditional on the namespace not itself being terminating, which the
webhook establishes by reading it. Refusing a user's `kubectl delete sd` and refusing
the namespace controller's teardown look identical to a naive rule, and only one of them
is wanted.

---

## 6. StorageDeviceOps

Declared in `operator/api/v1alpha1/storagedeviceops_types.go`, short name
`sdops`, and reconciled by `StorageDeviceOpsReconciler` in
`operator/internal/controllers/node/storagedeviceops_controller.go`, which is the
node's package for the reason §5.1 gives. The type is Appendix B.

```go
// +kubebuilder:validation:Enum=Restart;SelfTest;Fail;Replace;Migrate
type StorageDeviceOpsAction string
```

| Action     | Steps                                                                  | Blast radius                                        |
|------------|------------------------------------------------------------------------|-----------------------------------------------------|
| `Restart`  | `Requesting` → `Awaiting`                                              | One device. Its node keeps serving from the rest    |
| `SelfTest` | `Validating` → `Requesting` → `Awaiting`                               | One device, out of service while the test runs      |
| `Fail`     | `Requesting` → `Awaiting`                                              | One device, out of the data path, still in the slot |
| `Replace`  | `Validating` → `Removing` → `AwaitingDevice` → `Adding` → `Rebuilding` | One device, and a rebuild onto the one that arrives |
| `Migrate`  | `Validating` → `Detaching` → `AwaitingMove` → `Attaching`              | One device, and two nodes for as long as it takes   |

**`Restart` is the action the kind exists for.** A wedged device today is
restarted by restarting its node, which takes every other device on that node
with it and costs the cluster a node's worth of redundancy for the duration. One
device is the narrowest thing that can be recycled, and choosing the narrowest
resource that achieves an outcome is the rule
[`design-crd-model.md`](design-crd-model.md) §8.2 states.

**`SelfTest` is named for the command it issues, not for SMART.** Both device
types §4.2 admits have a self-test: NVMe has a Device Self-test with a short and an
extended form, and a SCSI or SATA device has the SMART self-test with the same two.
`SmartCheck` would name the ATA and SCSI feature for an operation that also runs on
NVMe, which is the transport asymmetry §4.2 exists to remove. `spec.selfTest.mode`
carries `Short` or `Extended`, which is the distinction both device types make and
the one that decides whether the operation takes two minutes or two hours.

**A failed self-test does not fail the device.** The operator emits `SelfTestFailed`
with the verdict and leaves the device in the phase it was in, because `Failed` is
terminal and a test result is not a decision. Somebody reads the event and issues `Fail`
or `Replace`, which is one step more than an automatic transition and the only version
that does not turn a diagnostic into an irreversible one.

**Reading a device's health is not this operation.** SMART attributes, an NVMe
health log, wear indicators, and error counters are observations, so they belong in
`status` where every reconcile refreshes them, not behind an operation somebody has to
run to find out. `SelfTest` exercises the device and reports a verdict, which is
something that happens rather than something that is true. §7 records that the control
plane reports the attributes, so what a reconcile publishes and what a self-test
produces are two separate things and neither stands in for the other.

**A removal is never an action by itself.** Taking a device out of the layout is a
step of `Replace` and of `Migrate`, and taking it out of the data path is `Fail`, but
there is no action whose whole content is "this device is gone." Every removal has a
successor, and which successor it has is the thing worth recording: a device being
swapped, a device being moved, or a device being distrusted are three different
intentions that reach the layout the same way, and each action names which one it is.

**Decommissioning without a replacement is `Fail` and then pulling the drive.** `Fail`
rebuilds the redundancy elsewhere and stops the cluster reading from the device, and
the drive coming out is observed rather than requested: the node stops reporting it,
§5.2 deletes the object, and `DeviceDisappeared` records that nothing asked for it. The
cluster reaches the same state, by a path that records why it got there.

**`SelfTest` and `Replace` check redundancy before they act, and that is their
`Validating` step.** Losing a device reduces the cluster's fault tolerance, and losing
one while another is already failed can put it below what its erasure coding requires.
The step refuses when the cluster's reported fault tolerance would not survive it, and
emits `InsufficientRedundancy` rather than proceeding. A device under test is a device
out of service, which is why the test carries the same check as the swap.

**`Fail` takes a device out of the data path and leaves it in the slot.** An
administrator who knows a drive is bad has no way today to say so: the cluster keeps
reading from a device that is answering slowly or wrongly until the control plane
decides for itself. `Fail` moves it to `Failed` (§4.2), so the cluster rebuilds its
redundancy elsewhere and stops trusting it, while the hardware stays where it is until
somebody is ready to deal with it. It is the action that separates a drive being bad
from a drive being gone, which one action covering both would conflate.

**`Fail` proceeds at any redundancy, which is the one asymmetry in the set.**
`SelfTest` and `Replace` refuse when the cluster's fault tolerance does not survive
losing a device. `Fail` acts regardless, because its premise is that the device is
already untrustworthy: a check that refuses here refuses precisely when a suspect
device is doing the most harm, to protect redundancy the cluster believes it has and
does not. `InsufficientRedundancy` is emitted as a warning rather than as a refusal,
saying that the cluster is below its target and a replacement is the way out.

**What `Fail` is not is a diagnosis.** It records a decision somebody made, so a
device failed by hand and a device the control plane failed on its own reach the same
phase and are indistinguishable in it. `status.deviceStatus`, which keeps the control
plane's own spelling (§4.2), is where the two differ if the control plane distinguishes
them.

**`Failed` is terminal, so `Fail` has no inverse and the action is not reversible.**
A device that has been failed does not return to service: there is no `Recover`, the
control plane offers nothing to return it, and the two ways out of the phase are both
physical: the drive is swapped (`Replace`) or pulled, and pulling it deletes the object
(§5.2). The abort edge exists from `Pending` only, because once `Requesting` has issued
the call there is nothing to undo.

**That makes a mistaken `Fail` expensive, and the expense is the point.** Failing a
healthy device costs a physical replacement of working hardware, so the action is worth
the deliberation its name implies. What it buys is that a device the cluster has stopped
trusting stays untrusted. A phase a device can leave on its own evidence is a phase one
good reconcile empties, and the device it empties is the one the cluster judged
unreliable.

### 6.1 Replace is a removal paired with an arrival

**`Replace` is the field workflow: a drive has failed and another is going into its
slot.** The removal and the arrival are one action because the pairing is the part
that needs stating: it is what records that the cluster's redundancy is short
temporarily rather than permanently, and it is what makes the drive that arrives
identifiable as the replacement rather than as an unrelated addition.

**`AwaitingDevice` holds on a person, and holds without a deadline.** Somebody has
to walk to the machine, and how long that takes is not a property of the design. The
step reports `AwaitingPhysicalAction` with the removed device's serial number and
its path, which are what §4.2 records for exactly this moment, and it keeps holding
until a device appears or the operation is aborted.

**The arriving device is identified by being new, and ambiguity fails rather than
guesses.** While the operation holds, the node reports the devices it has, and a
device the cluster has not seen before is the replacement. Two new devices at once make the
association a guess, so the operation fails with `ReplacementAmbiguous` and leaves
both in place rather than adopting one. The slot the old device occupied is a stronger
signal and is not available on every device type: a logical block device may report no
PCI address (§4.2), so the identification cannot rest on one.

**`Rebuilding` is the step that takes the time, and it is the control plane's work.**
The operator waits for the cluster to report the new device carrying its share of the
layout. What it does not do is decide the rebuild rate or its priority, which are
cluster properties rather than per-operation ones.

### 6.2 Migrate moves the drive with what is on it

**`Migrate` moves a physical device to another storage node, with what is on it.**
The target is `spec.migrate.targetNodeRef`, a `StorageNode` in this namespace, and
the reason the action exists is that the data travels in the drive rather than over
the network. Evacuating a device, moving it empty, and rebuilding it into the target
is a `Replace` on the source node and another on the target, and it costs a full
rebuild. Re-homing the device with its chunks intact is what this action is for,
and it is also the thing that depends on the control plane most (§7).

**The steps are a detach, a hold on a person, and an attach.** `Validating` resolves
the target node, confirms it is online and has a free slot, and checks that the
source cluster's redundancy survives the device being out for the duration.
`Detaching` takes the device out of service so it can be pulled without an unclean
loss. `AwaitingMove` holds, deadlineless, for the same reason `AwaitingDevice` does.
`Attaching` is the target node reporting the device and the control plane accepting
it as that node's, with its contents.

**The operation's output is a second `StorageDevice` object, and the first is
deleted.** `spec.nodeRef` is immutable (§4.1), so a device that changes nodes cannot
keep its object: identity here is `(nodeRef, deviceID)` and one half of it changed.
`status.resultingDeviceRef` names the object on the target node, which is what makes
the move traceable from the operation after both objects have moved on. The operator
deletes the source object, which is one of the two deletions §5.3 permits.

**A migration that is abandoned mid-move leaves a device in neither node.** The drive
is out of the source and not in the target, and no object describes it, because §5.1
builds objects from what a node reports and no node reports it. The operation stays in
`AwaitingMove` and says so, which is the only honest state: the operator cannot see a
drive on somebody's desk. Aborting from `AwaitingMove` re-attaches it to the source
node, which is why that edge exists and why the abort is refused from `Attaching`
onward.

### 6.3 What admission validates

**A `DELETE` is refused from every step that declares no abort edge**, which is the
group rule reading this kind's graph
([`design-crd-model.md`](design-crd-model.md) §3.1). Those steps are `Requesting`,
`Awaiting`, `Removing`, `AwaitingDevice`, `Adding`, `Rebuilding`, `Detaching`, and
`Attaching`, leaving `Validating` and `AwaitingMove` as the two a delete is admitted
from, where `storage.simplyblock.io/storagedeviceops-finalizer` unwinds the operation
before it clears. The two deadlineless steps are what make the rule matter here rather than
elsewhere: `AwaitingDevice` and `AwaitingMove` both wait on somebody walking to a
machine, so an object in one of them can sit for days and is exactly what somebody
tidying up reaches for. Deleting from `AwaitingMove` is safe because the abort
re-attaches the drive to its source node (§6.2), and deleting from `AwaitingDevice`
is not, because the removal has already happened and the operation is the only record
that a slot is waiting to be filled.

**`spec.deviceRef` and `spec.migrate.targetNodeRef` are resolved at creation.** Both
name objects in this API group, both are immutable, and a reference that does not
resolve can therefore never be corrected, which is the rule
[`design-crd-model.md`](design-crd-model.md) states for every reference in the group.
The webhook rejects an operation naming a `StorageDevice` that does not exist and a
`Migrate` naming a `StorageNode` that does not.

**Readiness stays with the controller.** A target node that exists and is offline, a
device that is already `Failed`, and a cluster below its redundancy target are all facts
about now: the create is admitted and `Validating` decides them, with the events §8.1
lists. A target node in another cluster is the one exception worth checking at
admission, because a device cannot move between clusters and both sides of that
comparison are immutable.

**The action's parameter block has to match its action**, which is a statement about
one object and so belongs on the type rather than in the webhook:

```go
// +kubebuilder:validation:XValidation:rule="self.action == 'Migrate' ? has(self.migrate) : !has(self.migrate)",message="migrate is required for action Migrate and must be absent otherwise"
// +kubebuilder:validation:XValidation:rule="self.action == 'SelfTest' || !has(self.selfTest)",message="selfTest is meaningful only for action SelfTest"
```

### 6.4 There is no Add

**A device is not addable, and the control plane offers nothing to add one with.** A
device that appears is discovered (§5.1), so a new drive in a free slot becomes an object
without anybody asking. `Replace` and `Migrate` carry an arrival because each pairs one
with a departure that has to be understood as a single operation, and an arrival with no
departure needs no operation at all.

**If an add call appears, it belongs to `Replace` rather than to a sixth action.**
Adopting a device is `Replace`'s `Adding` step and `Migrate`'s `Attaching` step, so such
a call is a verb those steps already need.

---

## 7. Backend API Requirements

| Method | Endpoint                                                                     | Notes                                                                            |
|--------|------------------------------------------------------------------------------|----------------------------------------------------------------------------------|
| `GET`  | `/api/v2/clusters/{cluster}/storage-nodes/{node}/devices`                    | The device list the objects are built from                                       |
| `GET`  | `/api/v2/clusters/{cluster}/devices/?watch=true`                             | The device stream. Scoped per cluster (§5.1)                                     |
| `POST` | `/api/v2/clusters/{cluster}/storage-nodes/{node}/devices/{device}/restart`   | The `Restart` action                                                             |
| `POST` | `/api/v2/clusters/{cluster}/storage-nodes/{node}/devices/{device}/remove`    | `Replace`'s and `Migrate`'s removal step                                         |
| `POST` | `/api/v2/clusters/{cluster}/storage-nodes/{node}/devices/{device}/self-test` | The `SelfTest` action, with the mode in the body                                 |
| `POST` | `/api/v2/clusters/{cluster}/storage-nodes/{node}/devices/{device}/fail`      | The `Fail` action                                                                |
| `POST` | `/api/v2/clusters/{cluster}/storage-nodes/{node}/devices/{device}/detach`    | `Migrate`'s `Detaching` step                                                     |
| `POST` | `/api/v2/clusters/{cluster}/storage-nodes/{node}/devices/adopt`              | `Replace`'s `Adding` and `Migrate`'s `Attaching`, which name the arriving device |

**The `?watch=true` row is a Server-Sent-Events subscription rather than a request
that returns**, and it arrives with the control plane's SSE work rather than with
this design ([`design-crd-model.md`](design-crd-model.md) §7.7). Until that lands it
is the one external dependency this design cannot satisfy on its own.

**`Migrate` needs one capability nothing here can substitute for.** `Attaching` asks
the control plane to accept a device as a different node's *with its contents*, so the
chunks on it stay where they are and are re-homed rather than rebuilt. If the control
plane can only adopt a device as empty, `Migrate` collapses into two `Replace`
operations and a full rebuild, which §6.2 gives as the reason the action exists at
all. That is the one row in this table whose absence removes an action rather
than degrading it.

**`Replace` needs no endpoint of its own beyond the removal.** Its `Removing` step is
`remove`, its
`Adding` step is the adopt call, and its `Rebuilding` step is a wait on the device
stream. The action is a graph over calls the other actions already need.

**The device list and the hardware fields are confirmed, and the per-action verbs are
what remain.** The control plane exposes devices and reports a PCI address, a serial
number, and a model for each, so §5.1 has something to build objects from and
`status.hardware` has something to publish. What is not yet confirmed is one verb per
action: `self-test`, `fail`, `detach`, and the adopt call. Each is a request rather than
an observation, so a missing one removes an action and leaves the rest of the kind
standing.

---

## 8. Observability

Both kinds are new. The metrics are the more valuable half here, because a device
is the level at which capacity and failure are actually located.

### 8.1 Kubernetes events

Events about a device's own state go on the `StorageDevice`. Events about a
device appearing or disappearing go on the `StorageNode`, because at that moment
the device object is being created or deleted and an event on either is an event
nobody reads.

| Event                                                         | Type      | Reason                   | On                 |
|---------------------------------------------------------------|-----------|--------------------------|--------------------|
| A device was found and an object created                      | `Normal`  | `DeviceDiscovered`       | `StorageNode`      |
| A device was removed by an operation                          | `Normal`  | `DeviceRemoved`          | `StorageNode`      |
| A device stopped being reported with no operation having run  | `Warning` | `DeviceDisappeared`      | `StorageNode`      |
| The device failed                                             | `Warning` | `DeviceFailed`           | `StorageDevice`    |
| The device is degraded and still serving                      | `Warning` | `DeviceDegraded`         | `StorageDevice`    |
| The device recovered                                          | `Normal`  | `DeviceOnline`           | `StorageDevice`    |
| The device's node cannot be reached, so its state is unknown  | `Normal`  | `DeviceStateUnknown`     | `StorageDevice`    |
| The device is above its capacity threshold                    | `Warning` | `DeviceNearlyFull`       | `StorageDevice`    |
| The operation is waiting for another to release the lock      | `Normal`  | `OperationQueued`        | `StorageDeviceOps` |
| The operation acquired the lock and started                   | `Normal`  | `OperationStarted`       | `StorageDeviceOps` |
| The operation finished successfully                           | `Normal`  | `OperationSucceeded`     | `StorageDeviceOps` |
| The operation failed                                          | `Warning` | `OperationFailed`        | `StorageDeviceOps` |
| The operation was aborted and its unwind finished             | `Normal`  | `OperationAborted`       | `StorageDeviceOps` |
| A step's deadline expired                                     | `Warning` | `StepDeadlineExceeded`   | `StorageDeviceOps` |
| An action was refused because redundancy would not survive it | `Warning` | `InsufficientRedundancy` |                    |
| A self-test finished and the device reported a failure        | `Warning` | `SelfTestFailed`         |                    |
| The operation is waiting for somebody to swap or move a drive | `Normal`  | `AwaitingPhysicalAction` |                    |
| Two unknown devices appeared, so the replacement is ambiguous | `Warning` | `ReplacementAmbiguous`   |                    |
| The replacement was adopted and the rebuild started           | `Normal`  | `ReplacementAdopted`     |                    |
| The device was accepted by its target node                    | `Normal`  | `DeviceMoved`            | `StorageDeviceOps` |

**`DeviceDisappeared` is the one worth building first.** A drive pulled from a
running node is a real event with no current expression anywhere in Kubernetes:
the node's count drops from `4/4` to `4/3` and nothing says why or which.

### 8.2 Prometheus metrics

| Metric                                                 | Labels                               | Description                                                                |
|--------------------------------------------------------|--------------------------------------|----------------------------------------------------------------------------|
| `simplyblock_storagedevice_capacity_bytes`             | `cluster`, `node`, `device`          | Gauge of the device's size                                                 |
| `simplyblock_storagedevice_used_bytes`                 | `cluster`, `node`, `device`          | Gauge of what it holds, and the ratio is where a cluster actually fills up |
| `simplyblock_storagedevice_phase_state`                | `cluster`, `node`, `device`, `phase` | Gauge, 1 for the current phase                                             |
| `simplyblock_storagenode_devices_failed_count`         | `cluster`, `node`                    | Gauge of failed devices per node, which is the redundancy signal           |
| `simplyblock_storagenode_devices_count`                | `cluster`, `node`                    | Gauge of devices per node, so the ratio matches the node's own summary     |
| `simplyblock_storagedevice_operations_total`           | `cluster`, `action`, `result`        | Operations reaching a terminal phase                                       |
| `simplyblock_storagedevice_operation_duration_seconds` | `cluster`, `action`                  | Histogram of operation durations                                           |

**`used_bytes` per device is the metric this kind adds that nothing else can.**
Cluster capacity is reported as a total, and a cluster at seventy per cent with
one device at ninety-eight is a cluster about to have a problem that its own
thresholds cannot see. Capacity is not evenly distributed and the aggregate hides
that.

**`device_failed` per node is the redundancy alert.** Erasure coding survives a
bounded number of simultaneous losses, and the count of failed devices is the
input to whether the next loss is survivable. It is what `Replace`'s validation
reads (§6) and it is worth graphing whether or not anybody removes anything.

---

## 9. Testing Strategy

Scenarios live in
[`tests/test-plan-storagedevice.md`](../../tests/test-plan-storagedevice.md) and only
there.

The projection of §5.1 is a pure function and is unit-testable in full: a device
list in, a set of objects out, including the create, the update, and the delete
paths. So is the mapping from a control-plane device status to a typed phase, and
so is the redundancy check, which is arithmetic over the cluster's reported
fault tolerance and the count of failed devices.

The risk unit tests do not reach is the operations, and it is not evenly
distributed. `Restart` is testable against a mock and verifiable on a live cluster
by watching a device leave and rejoin. `Replace` destroys capacity and is only
honestly testable on hardware somebody is willing to lose. `SelfTest` takes a device
out of service, so exercising it on a cluster at its redundancy limit is the
scenario that proves the check in §6 works, and it is the one nobody will want to
run.

---

## 10. What This Replaces

Nothing is migrated, because neither kind exists.

| Today                                           | After                                                                    |
|-------------------------------------------------|--------------------------------------------------------------------------|
| `StorageNode.status.resources.devices`, a count | Kept, and joined by one object per device (§3)                           |
| No per-device identity                          | `StorageDevice`, named for its node and device (§4.1)                    |
| No per-device capacity anywhere                 | `status.capacity` and two metrics (§4.2, §8.2)                           |
| No way to tell which device failed              | `status.phase` per device, and `DeviceFailed` naming it (§8.1)           |
| Restarting a device means restarting its node   | `StorageDeviceOps` with `action: Restart` (§6)                           |
| A pulled drive is a count changing              | `DeviceDisappeared` on the node (§5.2)                                   |
| `StorageNode` owns nothing                      | It owns its devices, establishing `design-crd-model.md` §9.3's last edge |

**This closes the last undesigned edge in the ownership spine.**
[`design-crd-model.md`](design-crd-model.md) §9.3 lists `StorageNode` owns
`StorageDevice` as depending on the whole kind, and §5.3 establishes it.

---

## 11. Open Questions

**Q1: Which of the four per-action verbs the control plane offers.** §7 confirms the
device list and the hardware fields and leaves `self-test`, `fail`, `detach`, and the
adopt call unconfirmed. Each is one action's whole content, so a missing verb removes an
action and leaves the rest of the kind standing. The exception is the adopt call, which
`Migrate` needs in its stronger form: accepting a device as another node's with its
contents, rather than as an empty one.

**Q2: Whether an operation may target a device in `Unknown`.** §5.2 keeps the objects of
an unreachable node and moves them to `Unknown`, so an operation can name a device whose
state nobody can see. Refusing at `Validating` is the safe reading, and it blocks the
case where a node is unreachable because a device wedged it, which is exactly when
somebody wants to restart one. Admitting it means issuing a call about a device the
control plane may not reach either. The redundancy check has the same problem in smaller
form, being arithmetic over numbers that stopped being refreshed.

---

## Appendix A: `storagedevice_types.go`

The type as it is to be written. Everything the sections above show in Go is an
excerpt of this appendix, and this is the only place any type appears whole.

```go
// StorageDevicePhase is the operator's own view of a device. Degraded and Failed
// are deliberately distinct: a degraded device is serving and should not be,
// while a failed one is not serving and the cluster is running with less
// redundancy than it thinks until it is replaced.
// +kubebuilder:validation:Enum=Online;Degraded;Unknown;Removed;Failed
type StorageDevicePhase string

const (
	StorageDevicePhaseOnline   StorageDevicePhase = "Online"
	StorageDevicePhaseDegraded StorageDevicePhase = "Degraded"
	// Unknown is a device whose node cannot be reached, so its state is not
	// observable rather than bad. A terminal phase is not overwritten by it.
	StorageDevicePhaseUnknown StorageDevicePhase = "Unknown"
	StorageDevicePhaseRemoved  StorageDevicePhase = "Removed"
	StorageDevicePhaseFailed   StorageDevicePhase = "Failed"
)

// StorageDeviceRole is what the device carries. It is decided when the node is
// created, by spec.storageNodes.enableJournalDevice, and it is what makes one
// device's failure worse than another's.
// +kubebuilder:validation:Enum=Storage;Journal;Both
type StorageDeviceRole string

const (
	StorageDeviceRoleStorage StorageDeviceRole = "Storage"
	StorageDeviceRoleJournal StorageDeviceRole = "Journal"
	StorageDeviceRoleBoth    StorageDeviceRole = "Both"
)

// DeviceCapacity is how big the device is and how much of it is used. Cluster
// capacity is the sum of these, and a cluster at seventy per cent with one
// device at ninety-eight is a cluster whose own thresholds cannot see the
// problem.
type DeviceCapacity struct {
	// TotalBytes is the device's usable size.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`

	// UsedBytes is what it currently holds.
	// +kubebuilder:validation:Minimum=0
	// +optional
	UsedBytes *int64 `json:"usedBytes,omitempty"`
}

// DeviceHardware identifies the part. These are what somebody walking into a
// datacenter with a failed drive needs, and none of them is recoverable from the
// control plane's device ID alone. Every field is optional, because which ones a
// device has depends on how it is attached: an NVMe drive has a PCI address and a
// logical block device does not, which is the only signal of the difference the
// control plane reports.
type DeviceHardware struct {
	// PCIAddress is the device's address on the host ("0000:5e:00.0"), where it
	// has one. A logical block device may not: the address can belong to the
	// controller it hangs off rather than to the device.
	// +optional
	PCIAddress string `json:"pciAddress,omitempty"`

	// SerialNumber is what is printed on the drive.
	// +optional
	SerialNumber string `json:"serialNumber,omitempty"`

	// Model is the manufacturer's model string.
	// +optional
	Model string `json:"model,omitempty"`

	// DevicePath is where the host sees the device: "/dev/nvme0n1" for an NVMe
	// namespace, "/dev/sdb" for a logical block device. It is named for what it
	// holds rather than for the NVMe spelling, since it applies to both.
	// +optional
	DevicePath string `json:"devicePath,omitempty"`
}

// StorageDeviceSpec identifies the device this object reports on, and carries
// nothing else. Everything a user could change about a device is on the node, so
// a spec field here would be a second place to set the same thing.
type StorageDeviceSpec struct {
	// NodeRef names the StorageNode this device belongs to. The node owns this
	// object by controller reference, so deleting the node deletes its devices.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	NodeRef string `json:"nodeRef"`

	// DeviceID is the control plane's identifier for the device. With NodeRef it
	// is the whole of this object's identity.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	DeviceID string `json:"deviceID"`
}

// StorageDeviceStatus is everything observed about the device.
type StorageDeviceStatus struct {
	// Phase is the operator's own view of the device.
	// +optional
	Phase StorageDevicePhase `json:"phase,omitempty"`

	// DeviceStatus is the control plane's own string, in the control plane's
	// spelling, which is why it carries no Enum here.
	// +optional
	DeviceStatus string `json:"deviceStatus,omitempty"`

	// Role is what the device carries.
	// +optional
	Role StorageDeviceRole `json:"role,omitempty"`

	// Capacity is how big the device is and how much it holds.
	// +optional
	Capacity *DeviceCapacity `json:"capacity,omitempty"`

	// Hardware identifies the part. Which of its fields are set is what says how
	// the device is attached: only an NVMe device has a PCI address.
	// +optional
	Hardware *DeviceHardware `json:"hardware,omitempty"`

	// ActiveOpsRef names the StorageDeviceOps currently allowed to act on this
	// device. Empty when none is running.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the device moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from. On this kind it moves at most once, since every spec field is fixed
	// when the object is created.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sd
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.nodeRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=".status.role"
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=".status.capacity.totalBytes"
// +kubebuilder:printcolumn:name="Used",type=integer,JSONPath=".status.capacity.usedBytes"
// +kubebuilder:printcolumn:name="PCI",type=string,JSONPath=".status.hardware.pciAddress",priority=1
// +kubebuilder:printcolumn:name="Serial",type=string,JSONPath=".status.hardware.serialNumber",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageDevice is one storage backend device belonging to one storage node,
// whatever transport it sits behind: an NVMe drive, or from 26.4 a logical block
// device such as a spinning disk. It is the
// bottom of the ownership spine and the narrowest thing an operation can target.
//
// Objects are created by the operator from what the control plane reports and
// are never written by a user: the device's existence follows from the node
// having it, and which devices a node uses is decided in the node's own spec.
type StorageDevice struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageDeviceSpec   `json:"spec,omitempty"`
	Status StorageDeviceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageDeviceList contains a list of StorageDevice.
type StorageDeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageDevice `json:"items"`
}
```

---

## Appendix B: `storagedeviceops_types.go`

```go
// StorageDeviceOpsAction is the operation a StorageDeviceOps performs. There is no
// Add: a device that appears is discovered (§5.1), and the two actions that carry an
// arrival carry it because each pairs one with a departure (§6.4). There is no bare
// Remove either: a removal is a step of Replace and of Migrate, and taking a device
// out of the data path without replacing it is Fail (§6).
// +kubebuilder:validation:Enum=Restart;SelfTest;Fail;Replace;Migrate
type StorageDeviceOpsAction string

const (
	// Restart is the action the kind exists for: recycling one device rather
	// than its node, which is the narrowest blast radius available.
	StorageDeviceOpsActionRestart StorageDeviceOpsAction = "Restart"
	// SelfTest runs the device's own self-test, which both device types have:
	// NVMe as Device Self-test and SCSI or SATA as the SMART self-test. It is
	// not named for SMART, because SMART names the feature on only one of them.
	StorageDeviceOpsActionSelfTest StorageDeviceOpsAction = "SelfTest"
	// Fail marks a device failed and leaves it in the slot, so the cluster
	// rebuilds its redundancy elsewhere and stops reading from a device
	// somebody has judged untrustworthy. It carries no redundancy check, and
	// Failed is terminal: there is no inverse action (§6).
	StorageDeviceOpsActionFail StorageDeviceOpsAction = "Fail"
	// Replace removes a device and adopts the one that arrives in its place,
	// which is one action because the pairing is what makes the arrival
	// identifiable (§6.1).
	StorageDeviceOpsActionReplace StorageDeviceOpsAction = "Replace"
	// Migrate moves the physical device to another storage node with what is on
	// it, rather than evacuating it and rebuilding elsewhere (§6.2).
	StorageDeviceOpsActionMigrate StorageDeviceOpsAction = "Migrate"
)

// StorageDeviceOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StorageDeviceOpsPhase string

const (
	StorageDeviceOpsPhasePending   StorageDeviceOpsPhase = "Pending"
	StorageDeviceOpsPhaseRunning   StorageDeviceOpsPhase = "Running"
	StorageDeviceOpsPhaseSucceeded StorageDeviceOpsPhase = "Succeeded"
	StorageDeviceOpsPhaseFailed    StorageDeviceOpsPhase = "Failed"
	StorageDeviceOpsPhaseAborted   StorageDeviceOpsPhase = "Aborted"
)

// StorageDeviceOpsStep is one step of a running device operation. The enum is
// the union of every action's steps; which steps belong to which action is
// declared by the graph rather than by this type.
// +kubebuilder:validation:Enum=Validating;Requesting;Awaiting;Removing;AwaitingDevice;Adding;Rebuilding;Detaching;AwaitingMove;Attaching
type StorageDeviceOpsStep string

const (
	// Validating checks what the action needs before anything is issued: that
	// the cluster's redundancy survives losing this device for SelfTest and
	// Replace, and that the target node is online with a free slot for Migrate.
	// It is the only step those actions can be aborted from. Fail has no
	// Validating step, because it proceeds at any redundancy (§6).
	StorageDeviceOpsStepValidating StorageDeviceOpsStep = "Validating"

	// Restart, SelfTest, and Fail: one call, then a wait for the control plane
	// to report the device in the state the call asked for.
	StorageDeviceOpsStepRequesting StorageDeviceOpsStep = "Requesting"
	StorageDeviceOpsStepAwaiting   StorageDeviceOpsStep = "Awaiting"

	// Replace: the removal that its arrival is paired with.
	StorageDeviceOpsStepRemoving StorageDeviceOpsStep = "Removing"

	// Replace. AwaitingDevice holds without a deadline, because it waits on
	// somebody walking to the machine; Adding adopts the device that arrived;
	// Rebuilding waits for the cluster to report it carrying its share.
	StorageDeviceOpsStepAwaitingDevice StorageDeviceOpsStep = "AwaitingDevice"
	StorageDeviceOpsStepAdding         StorageDeviceOpsStep = "Adding"
	StorageDeviceOpsStepRebuilding     StorageDeviceOpsStep = "Rebuilding"

	// Migrate. Detaching takes the device out of service so it can be pulled,
	// AwaitingMove holds deadlineless like AwaitingDevice, and Attaching is the
	// target node reporting it and the control plane accepting it as that
	// node's. Abort is expressible from AwaitingMove and not from Attaching.
	StorageDeviceOpsStepDetaching    StorageDeviceOpsStep = "Detaching"
	StorageDeviceOpsStepAwaitingMove StorageDeviceOpsStep = "AwaitingMove"
	StorageDeviceOpsStepAttaching    StorageDeviceOpsStep = "Attaching"
)

// SelfTestSpec parameterizes the SelfTest action.
type SelfTestSpec struct {
	// Mode is which self-test to run. Both device types offer the same two, and
	// the difference is minutes against hours.
	// +kubebuilder:validation:Enum=Short;Extended
	// +kubebuilder:default=Short
	// +optional
	// +k8s:immutable
	Mode string `json:"mode,omitempty"`
}

// MigrateDeviceSpec parameterizes the Migrate action.
type MigrateDeviceSpec struct {
	// TargetNodeRef names the StorageNode the device is being moved to, in this
	// namespace. It is resolved at admission (§6.4).
	// +kubebuilder:validation:Required
	// +k8s:immutable
	TargetNodeRef string `json:"targetNodeRef"`
}

// StorageDeviceOpsSpec is one operation to perform against one StorageDevice.
type StorageDeviceOpsSpec struct {
	// DeviceRef names the StorageDevice this operation acts on. The operation
	// never owns its target, because deleting the record of an operation must
	// not delete the device record it operated on.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	DeviceRef string `json:"deviceRef"`

	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action StorageDeviceOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind. A
	// A Replace or Migrate past its removal cannot be unwound, which those actions'
	// graph declares rather than this field.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Force skips the redundancy check SelfTest and Replace perform. It
	// exists because a device that has already failed contributes no redundancy
	// to lose, and the check cannot always tell that from the control plane's
	// report. Fail ignores it, having no check to skip.
	// +optional
	Force *bool `json:"force,omitempty"`

	// SelfTest parameterizes action SelfTest and is ignored by the others.
	// +optional
	SelfTest *SelfTestSpec `json:"selfTest,omitempty"`

	// Migrate parameterizes action Migrate and is ignored by the others.
	// +optional
	Migrate *MigrateDeviceSpec `json:"migrate,omitempty"`
}

// StorageDeviceOpsStatus is the observed state of one device operation.
type StorageDeviceOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase StorageDeviceOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine. It is
	// persisted before the side effect that step performs.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','Requesting','Awaiting','Removing','AwaitingDevice','Adding','Rebuilding','Detaching','AwaitingMove','Attaching']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// FaultToleranceBefore is what the cluster reported it could survive when
	// Validating ran, recorded so that a refusal says what it was measured
	// against rather than only that it happened.
	// +optional
	FaultToleranceBefore *int32 `json:"faultToleranceBefore,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StartedAt is when the operation acquired its target's lock.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when it reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sdops
// +kubebuilder:printcolumn:name="Device",type=string,JSONPath=".spec.deviceRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageDeviceOps is a single operation performed against one StorageDevice. It
// is the narrowest blast radius in the ownership spine: restarting one device
// rather than its node, which would take every other device on that node with
// it.
type StorageDeviceOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageDeviceOpsSpec   `json:"spec,omitempty"`
	Status StorageDeviceOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageDeviceOpsList contains a list of StorageDeviceOps.
type StorageDeviceOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageDeviceOps `json:"items"`
}
```
