# Design Document: PersistentVolumeOps

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-30  
**Test Plan:** [`tests/test-plan-persistentvolumeops.md`](../../tests/test-plan-persistentvolumeops.md)

This document specifies the target model. `VolumeMigration` is registered and is
absorbed into this kind ([`design-crd-model.md`](design-crd-model.md) §9.1), and
§10 is the single record of what the rework changes against it.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [An Ops Kind Whose Target This Group Does Not Define](#3-an-ops-kind-whose-target-this-group-does-not-define)
4. [PersistentVolumeOps: API](#4-persistentvolumeops-api)
5. [The Step Machine](#5-the-step-machine)
6. [Mutual Exclusion Through an Annotation on the Volume](#6-mutual-exclusion-through-an-annotation-on-the-volume)
7. [Backend API Requirements](#7-backend-api-requirements)
8. [Observability](#8-observability)
9. [Testing Strategy](#9-testing-strategy)
10. [Migration from the Registered API](#10-migration-from-the-registered-api)
11. [Ownership and Retention](#11-ownership-and-retention)
12. [Open Questions](#12-open-questions)

Appendices:

- [Appendix A: `persistentvolumeops_types.go`](#appendix-a-persistentvolumeops_typesgo)

---

## Overview

`PersistentVolumeOps` is a single operation performed against one
`PersistentVolume`. It has one action today, `Migrate`, which moves a volume's
backing logical volume to a different storage node, and which the registered API
calls `VolumeMigration`.

It is the one `Ops` kind in the group whose target is a core Kubernetes type
rather than a kind this API group defines, and almost everything unusual about it
follows from that: it locks its target with an annotation rather than a status
field, it is cluster-scoped because its target is, it cannot be owned by the
operation that created it, and it has to work out which cluster it is operating in
rather than being told.

---

## 1. Background

`VolumeMigration` works and is the most exercised operation in this repository.
Its problems are shape.

**Its name is the one signal a reader has, and it gives the wrong one.**
[`design-crd-model.md`](design-crd-model.md) §3 makes the `Ops` suffix mechanical:
a kind ending in `Ops` is one-shot, names one target, and terminates.
`VolumeMigration` is all three and says none of it, which is why §1 of that
document lists it as the one action kind whose name does not end in `Ops`.

**Its phase and its step are one enum.** `Pending;Validating;Running;Completed;Failed;Aborted`
puts `Validating`, which is a step, beside `Completed`, which is an outcome. The
result is that the operation's own progress and the position of its work are the
same field, so neither can be read without the other's values in mind
([`design-crd-model.md`](design-crd-model.md) §9.5).

**`Completed` is the odd one out among terminal successes.** Every other `Ops`
kind in the group reaches `Succeeded`, and this one reaches `Completed`, for no
reason beyond having been written separately.

**Its status carries two arrays that are working state rather than observation.**
`status.connections` holds the NVMe-oF paths the migration created, and
`status.validationJobs` holds the Jobs it started to check them. Both are real and
both are needed, and they are in status because there was nowhere else to put
them.

---

## 2. Goals and Non-Goals

### Goals

- Specify the kind `VolumeMigration` becomes, with the `Ops` shape every other
  operation in the group has (§4).
- Split the merged enum into a phase and a step, and say what each holds (§4.2,
  §5).
- Specify how an operation on a core type is scoped, locked, and prevented from
  colliding with another, given that its target cannot carry a lock field (§3,
  §6).
- Name the kind for its target rather than for its action, so that a second action
  would not require renaming it, while planning none (§4.1).
- Say where an operation came from and when it stops existing, given that the scope
  forbids an owner reference and one rebalance produces one object per volume moved
  (§11).

### Non-Goals

- **Not the migration algorithm.** What makes a migration safe, how the control
  plane copies the data, and what the rebalancer's scoring is belong to
  [`design-auto-rebalancing.md`](../design-auto-rebalancing.md). This document
  specifies the kind, its phases, and its lifecycle.
- **Not who creates one.** A migration is created by the auto-rebalancer, by a
  node drain ([`design-storagenode.md`](design-storagenode.md) §8.2), or by hand.
  What each of those does with the outcome is specified where it lives.
- **Not the volume itself.** A volume is a `PersistentVolumeClaim` and a
  `PersistentVolume`, which is core Kubernetes
  ([`design-crd-model.md`](design-crd-model.md) §5). This group deliberately does
  not define a volume kind.
- **Not the API group's conventions.** The entity and action split, the enum
  casing, and the observed generation belong to
  [`design-crd-model.md`](design-crd-model.md) and are cited rather than restated.

---

## 3. An Ops Kind Whose Target This Group Does Not Define

Every other `Ops` kind names an entity this API group owns, and gets four things
from that: a status field on its target to lock, an owner reference from the
operation that created it, a namespace shared with the entity it acts on, and a
cluster it is told rather than has to find. This one gets none of the four.

**Its target cannot carry a lock field.** `StorageCluster` and `StorageNode` each
carry `status.activeOpsRef` ([`design-crd-model.md`](design-crd-model.md) §3.2), and
a `PersistentVolume` is a core type this operator does not define and must not add
fields to. The lock therefore moves to an annotation on the volume, which keeps the
optimistic-lock property the status field has and costs a write to an object this
operator does not own. §6 is that trade in full.

**It is cluster-scoped, and it is the only kind in this group that is.** A
`PersistentVolume` is cluster-scoped, and an operation on one that lived in a
namespace would make the lock of §6 ambiguous: the annotation names its holder, and
a holder identified by name alone has to be unique somewhere, which for a namespaced
kind it is not. So the kind takes its target's scope, and the lock's value stays a
name rather than becoming a namespace and a name. The other kinds staying namespaced
is not an inconsistency waiting to be fixed. Each of them names an entity this
group defines, and those are namespaced.

**Being cluster-scoped costs it the owner reference its creator would otherwise
hold.** A cluster-scoped object cannot be owned by a namespaced one: Kubernetes
treats such a reference as unresolvable and garbage-collects the dependent. A drain
therefore cannot own the operations it fans out by controller reference, which is
what [`design-storagenode.md`](design-storagenode.md) §8.4 relies on both to be
woken by each completion and to cascade a delete. That is a consequence of the scope
rather than a choice. What replaces it is a reference written out in the spec, a label
to select on, and the creator's finalizer to perform the cascade, which §11.1
specifies.

**Its cluster is derived rather than declared, and the volume itself is where it is
derived from.** A `StorageNodeOps` knows its cluster from its node. This one reads
`spec.csi.volumeHandle` off the `PersistentVolume`: that is the CSI volume ID, it
is `<clusterID>:<poolID>:<volumeID>`, and it is stamped on the volume at
provisioning and immutable for the volume's life. `atlas-lib`'s
`lvol.VolumeHandle.Split()` is the parser, and it is the whole resolution. No
`StorageClass` is consulted, so a class edited, replaced, or deleted out of band
changes nothing about an existing volume's addressability, and the cluster, pool,
and volume UUIDs the later steps need all come out of one immutable field.

**What can go wrong there is a volume that is not one of this driver's.** A
`PersistentVolume` with no `spec.csi`, one provisioned by a different driver, or one
whose handle is not three UUIDs cannot be addressed, and the operation fails with
`ClusterUnresolvable` rather than guessing. That is a check on the target rather
than a join that can rot.

**`spec.persistentVolumeName` is a name, not a reference.** There is no reference
type for a cluster-scoped object, and naming the `PersistentVolume` rather than the
claim is deliberate: a claim can be deleted while its volume is retained, and the
operation is about the volume.

---

## 4. PersistentVolumeOps: API

Declared in `operator/api/v1alpha1/persistentvolumeops_types.go`, short name
`pvops`, and reconciled by `PersistentVolumeOpsReconciler` in
`operator/internal/controllers/volume/persistentvolumeops_controller.go`. The
package is the volume band's, which it shares with the auto-rebalancer that creates
most of these objects and with the claim controller
([`design-crd-model.md`](design-crd-model.md) §7.10). The type is Appendix A.

### 4.1 Spec

```go
// PersistentVolumeName names the PersistentVolume this operation acts on. It is
// a name rather than a reference because a PersistentVolume is cluster-scoped,
// and it names the volume rather than the claim because a claim can be deleted
// while its volume is retained.
// +kubebuilder:validation:Required
// +k8s:immutable
PersistentVolumeName string `json:"persistentVolumeName"`

// Action is the operation to perform.
// +kubebuilder:validation:Required
// +k8s:immutable
Action PersistentVolumeOpsAction `json:"action"`
```

`spec.migrate.targetNodeRef` names the `StorageNode` to move to, under the
per-action block every `Ops` kind in the group uses. `spec.abort` is the only
mutable field.

**`spec.migrate.targetNodeRef` is a `StorageNode` name, not a backend UUID.** The
registered kind takes `targetNodeUUID`, which is the control plane's identifier
and means a user has to look it up. Naming the Kubernetes object is what makes a
hand-written migration writable, and the controller resolves the UUID from
`status.uuid` on the node.

**It carries a namespace as well as a name, and it is the only reference in the
group that does.** Every `*Ref` elsewhere is a bare string (`clusterRef`,
`nodeRef`, `poolRef`, `activeOpsRef`), which works because both objects are
namespaced and a bare name means the same namespace. A `StorageNode` is namespaced
and this kind is not (§3), so there is no namespace for a bare name to mean, and two
clusters in two namespaces may each hold a node called `worker-3`. Core Kubernetes
carries the namespace in exactly this direction, which is why `pv.spec.claimRef` and
`volumeSnapshotContent.spec.volumeSnapshotRef` are full references rather than names,
and it is the same reason `spec.creatorRef` carries one (§11.1).

**Being explicit makes one wrong state expressible, and §4.3 rejects it.** The
namespace could instead be derived, because the volume's cluster UUID resolves to a
`StorageCluster` whose namespace is where its nodes live, which would make a target
in the wrong cluster impossible to write. That is rejected because a hand-written
spec should be readable without performing a join to learn which object it names, and
because the derivation needs an index from backend cluster UUID to object. Naming a
node whose cluster is not the volume's is therefore writable, and refused at
admission with the two cluster UUIDs in the message.

**One action, and no second one is planned.** The kind is named for a
`PersistentVolume` rather than for a migration so that adding an action later would
not rename it, and the naming rule alone justifies the kind at one action
([`design-crd-model.md`](design-crd-model.md) §3). Three candidates were considered
and none is being built: a resize, which `PersistentVolumeClaim` and the CSI expand
path already express for every case anybody has asked for; a re-encryption, which
has no backend operation to drive; and a forced disconnect of stale NVMe-oF paths,
which §5's `Verifying` step and §5.1's finalizer make part of every operation rather
than an operation of its own. `spec.action` and the per-action block stay because
they are the group's shape for an `Ops` kind, not because a second value is queued
behind them.

### 4.2 Status

```go
// PersistentVolumeOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type PersistentVolumeOpsPhase string
```

`Succeeded` replaces `Completed`, so that every `Ops` kind in the group has the
same five phases and an alert on completion matches one value rather than two.

`status.step` is the position of the running action's machine (§5), and
`status.migration` groups everything that is about the migration rather than
about the operation: the backend migration's UUID, the volume and pool UUIDs, the
subsystem NQN, the source node, the member count, the connections it created, and
the validation Jobs it started.

**The two arrays stay, and they stay in status.** `connections` and
`validationJobs` are working state that has to survive a process restart, and
status is where a controller's durable working state lives
([`design-crd-model.md`](design-crd-model.md) §3.1). Grouping them under
`status.migration` at least says they belong to the action rather than to the
operation.

`status.deferredSince` records when the operation was first held, which is what
the rebalancer reads to decide whether a migration has been waiting long enough
to give up on.

### 4.3 What Admission Rejects, and What the Controller Reports Instead

A validating admission webhook, `PersistentVolumeOpsValidator`, stands in front of
the kind. The line it draws is between what cannot change and what can: a condition
fixed for the object's whole life is a rejection, and a condition true at one moment
and false at the next is a phase and an event. An admission decision is made once and
never revisited, so anything time-varying decided there would be decided wrongly for
most of the object's life.

**Rejected at create, because no reconcile could ever make it work:**

| Rejected                                                                    | Because                                                                                             |
|-----------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------|
| `spec.persistentVolumeName` names no `PersistentVolume`                     | Nothing creates the operation before its volume, so this is a typo                                  |
| The volume has no `spec.csi`                                                | An in-tree or hostPath volume has no logical volume to move                                         |
| `spec.csi.driver` is not `csi.simplyblock.io`                               | Another driver's volume, which this operator has no means to act on                                 |
| `spec.csi.volumeHandle` does not parse as `<clusterID>:<poolID>:<volumeID>` | Nothing to address the backend with (§3)                                                            |
| `spec.migrate.targetNodeRef` names no `StorageNode` in that namespace       | A migration to a node that does not exist has no target at all                                      |
| The target node's cluster is not the volume's cluster                       | A volume cannot move between clusters, and carrying the namespace makes the mistake writable (§4.1) |

**The agreement between `spec.action` and its parameter block is a CEL rule rather
than a webhook check**, because CEL can see it: it is a statement about one object's
own fields, and the group's floor is that such a rule lives on the type where nothing
can be installed in front of it
([`design-storagebackup.md`](design-storagebackup.md) uses the same rule for the same
reason). The webhook is left with what CEL cannot reach, which is every row above:
each of them is a fact about a *different* object.

**The driver check is the one that earns the webhook.** Every other rejection above
is a malformed request, while a volume belonging to another CSI driver is a
well-formed request against the wrong object, and it is the mistake a human writing
one by hand is most likely to make: `kubectl get pv` lists every volume in the
cluster and says nothing about which of them this operator can move. Rejecting at
admission answers that at the moment the mistake is made, with the driver name in the
message, rather than leaving an object that sits in `Failed` for someone to read.

**The webhook also stands in front of `DELETE`, and refuses what would strand the
volume.** `Validating` and `Migrating` declare an abort edge and `Verifying` does not
(§5), so a delete arriving during `Verifying` is refused with the step named, and one
arriving earlier is admitted and unwound by the finalizer of §5.1. The rule is the
group's rather than this kind's
([`design-crd-model.md`](design-crd-model.md) §3.1): the same graph decides for both
channels, so a deletion never expresses a stop that `spec.abort` could not.
`Verifying` is precisely the step worth guarding here, because it is the one holding
the validation Jobs and their NVMe-oF paths, and the production defect of §5 is those
paths outliving the object that recorded them.

**The cross-cluster check waits when it cannot conclude.** A `StorageCluster` whose
`status.uuid` is not yet recorded has not been created in the backend, so there is
nothing to compare the volume's cluster UUID against. That is a not-yet rather than a
mismatch, so the create is admitted and the condition becomes the controller's, in
the same bucket as the ones below.

**Not rejected, because each of these is a fact about now:** a target node that is
offline, a target node that is already the volume's current node, a volume whose lock
another operation holds, and a control plane that cannot be reached. The first two
are `TargetNodeNotReady` and `TargetNodeIsSource` (§8.1), the third is `Pending` and
`OperationQueued` (§6), and the fourth is a requeue. A node that is offline when the
drain fans out its migrations may well be online by the time the fifteenth of them
acquires the lock, which is exactly why that check belongs to the step that needs it
rather than to the create.

**`ClusterUnresolvable` covers the window admission cannot.** A volume that is
unaddressable at create is refused above, so the phase is reachable only when the
volume becomes unaddressable afterward: deleted, or replaced, between the operation's
admission and the step that resolves it. That window is real, and a resolution failure
needs a phase to fail into, which is why the reason exists rather than being folded
into the rejection.

**`failurePolicy: Fail`**, for the reason `StorageNodeValidator` and
[`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §5 both
already give: the webhook server runs inside the operator pod, so its availability
tracks the operator's, and while the operator is down no migration would run anyway.
The webhook needs `get` on `persistentvolumes`, which is the same access §6's lock
already requires in stronger form.

---

## 5. The Step Machine

One action, so one graph, and it is declared as a `MultiConfig` anyway. Not because
a second action is coming, since none is planned (§4.1), but because every other `Ops`
controller in the group drives its steps through a `MultiConfig` keyed by action, and
a kind that read differently for having one entry would make a reader check whether
the difference meant something. A `MultiConfig` with one entry costs nothing.

```
Migrate
    Validating ──► Migrating ──► Verifying
```

| Step         | Side effect on entry                                             | Complete when                               |
|--------------|------------------------------------------------------------------|---------------------------------------------|
| `Validating` | `POST` the migration, then start a Job per new NVMe-oF path      | Every validation Job succeeded              |
| `Migrating`  | Continue the migration, which is what starts the data copy       | The control plane reports the copy finished |
| `Verifying`  | Delete the validation Jobs and confirm no path is left connected | No validation Job and no stale path remain  |

**`Verifying` is new and it exists because of a defect that reached production.**
A migration's validation Jobs connect NVMe-oF paths to check the target is
reachable, and nothing disconnected them. The paths outlived the Jobs, poisoned
the data path, and blocked every later migration on that volume. Making the
cleanup a declared step rather than a deferred call means a crash between the copy
finishing and the cleanup restarts into `Verifying` rather than into nothing.

**An abort is expressible from `Validating` and `Migrating` and not from
`Verifying`.** Before the copy finishes there is a backend migration to cancel.
After it, the volume has already moved and there is nothing to undo. The graph
declares the edges, so an abort arriving late is an `IllegalTransitionError` the
controller reports rather than a half-undone move
([`design-crd-model.md`](design-crd-model.md) §3.1).

**Each step carries a deadline, and `Migrating`'s is computed rather than fixed.**
A copy takes as long as there is data to copy, and the number of members in the
migrated subsystem is what says how much there is: a migration is addressed by the
subsystem rather than by one volume, so every sibling volume in it moves along with
the named one. The bound is therefore a base plus a term per member, read from
`status.migration.memberCount`, which is the shape `atlas-lib/statemachine`'s own
worked example uses with a different count in place of that one.

### 5.1 What a Delete Does

**A delete is refused during `Verifying` and admitted before it**, which is §4.3's
rule and the graph's answer rather than this section's: `Verifying` declares no abort
edge, so there is nothing an unwind could do and the operation is what finishes the
work. Admission is where that is said, because a refusal names the step and the
reason at the moment the request is made.

**An admitted delete is unwound by a finalizer, not dropped.** An operation removed
mid-`Validating` would leave the validation Jobs running and their NVMe-oF paths
connected, with the only object that recorded them gone, which is the production
defect of §5 reached by a different route and a worse version of it, because nothing
is left to clean up from. So a non-terminal object carries
`storage.simplyblock.io/persistentvolumeops-finalizer`, and it clears once the backend
migration is canceled, the Jobs are deleted, no path is left connected, and the
volume's lock is released (§6).

**Stopping a migration is still `spec.abort`**
([`design-crd-model.md`](design-crd-model.md) §3.1). The two requests are different
and only one of them leaves a record: `spec.abort` produces an `Aborted` object
saying the migration was stopped and how far it got, where a delete leaves nothing
behind to ask. Stopping one and keeping the record is therefore `spec.abort` alone,
and stopping one and discarding the record is `spec.abort` followed by a delete.

**The operation follows its volume, so a volume being deleted takes its migration
with it.** A claim deleted under a `Delete` reclaim policy makes the CSI driver
delete the backing logical volume, and migrating a volume that is being deleted is
work whose result nobody will read. The controller watches the `PersistentVolume`,
and a volume that is gone or terminating drives the operation to `Aborted` and then
removes it: the backend migration is canceled, the validation Jobs and their paths
are cleaned up, and the object goes. This is the controller reaching a terminal phase
on its own rather than a delete standing in for one
([`design-crd-model.md`](design-crd-model.md) §3.1), and the phase is `Aborted`
rather than `Failed` because a migration whose volume went away did not go wrong.
Cancel-then-delete is the order that matters, since
a logical volume with a migration still running against it is not a volume the
control plane can cleanly delete.

**Under `Retain` the volume outlives the claim, and so does the operation.** This is
the case §4.1 names the volume rather than the claim for: the logical volume still
exists, still occupies capacity on a node that may be draining, and is still worth
moving. The signal is the volume's deletion rather than the claim's.

**A finalizer that cannot finish blocks the delete rather than dropping the paths.**
The object stays, `status.message` says which cleanup is outstanding, and
`CleanupBlocked`, `StalePathCleaned`, and `stale_paths_total` (§8) are what surface
it. Leaving a visibly stuck object is the intended outcome: a path connected with
nothing tracking it blocks every later migration on the volume, and has.

---

## 6. Mutual Exclusion Through an Annotation on the Volume

Two migrations of one volume at once would have two backend migrations copying the
same logical volume to two places. Every other `Ops` kind prevents that with
`status.activeOpsRef` on its target
([`design-crd-model.md`](design-crd-model.md) §3.2), and a `PersistentVolume` is a
core type this operator must not add a field to (§3). So the lock moves from status
to metadata and keeps everything else: the annotation
`storage.simplyblock.io/active-ops` on the `PersistentVolume` names the operation
currently allowed to act on the volume, and is absent when none is.

**It is a lock rather than a note because it has the three properties
[`design-crd-model.md`](design-crd-model.md) §3.2 requires of one.** Acquisition is
an optimistic-lock patch, so two reconcilers that both read the volume unlocked
produce a 409 for all but one of them. Release is idempotent and checks ownership,
clearing the annotation only while it still names the caller, so a late release
cannot steal the lock from whoever acquired it next. And release runs on every
terminal path including deletion, which is the finalizer of §5.1 rather than a
second mechanism.

**The lock is self-describing, and that is what answers the objection to putting it
here.** The risk in a lock outside the operation's own object is a holder that dies
between taking it and recording that it took it, leaving a volume locked by nobody.
That case does not arise, because the annotation's value is the holder's name and
the kind is cluster-scoped (§3), so the name is unique cluster-wide and identifies
the holder completely. Nothing has to be recorded for the lock to be readable: any
reconciler gets the named operation and learns which of three situations it is in.
The operation exists and is non-terminal, so the lock is valid and its holder
re-adopts it on the next pass. The operation is terminal, so the release did not run
and the lock is cleared by whoever noticed. The operation does not exist at all, so
it was deleted with its finalizer forced or removed out of band, and the lock is
broken by the same optimistic-lock patch, which is what keeps two operations from
both breaking it and both concluding they won.

**Having a real lock is what lets this kind queue like the rest of the group.** A
second operation for a locked volume is admitted by the API server, acquires
nothing, and stays at `status.phase: Pending` until the lock frees, which is the
behavior [`design-crd-model.md`](design-crd-model.md) §3.2 describes and the reason
nothing had to build queueing. It is also what `status.deferredSince`, the
`OperationQueued` event, and `simplyblock_persistentvolume_operation_queued_seconds` (§8) already assume:
they describe an operation that waits, and waiting is only expressible once there is
something to wait on.

**Exclusion is not the webhook's job, though the webhook exists (§4.3).** It
rejects a volume this operator cannot act on at all, which is permanent, and says
nothing about a volume that is merely busy, which is not: rejecting at admission and
queueing at `Pending` are different products, and this group's answer is queueing. A
drain fanning out fifty migrations across a handful of volumes wants them to proceed
in turn, not to have forty-odd creates fail and need retrying by whatever issued
them. A webhook that rejected the busy case as well would also be a worse version of
the lock it sits in front of, since a list has a window two simultaneous creates fit
through and the optimistic-lock patch does not.

**A field selector index on `spec.persistentVolumeName` is still needed, for the
other direction.** Releasing a lock has to wake whatever was waiting on it, so the
controller finds the operations queued for a volume by name rather than by listing
every operation in the cluster on every release.

**The cost is that this operator patches metadata on an object it did not create,
and it is worth stating rather than eliding.** It needs `patch` on
`persistentvolumes` in the manager's ClusterRole, and a `kubectl get pv -o yaml`
shows a simplyblock key on a volume this API group does not own. Three things bound
it. The write touches neither `spec` nor `status`, so it cannot conflict with the
provisioner or the volume's own controllers. The key sits under the group's
`storage.simplyblock.io/` prefix, so it is identifiable at a glance and matchable by
one RBAC or admission rule
([`design-crd-model.md`](design-crd-model.md) §7.3). And annotating core objects is
established rather than new here: the placement and realignment keys already live on
`PersistentVolumeClaim` objects the operator did not create either.

**A `PersistentVolume` deleted mid-operation takes the lock with it**, which is
correct for the lock and says nothing about the copy, since the backing logical
volume outlives the Kubernetes object. §5.1 is what the operation does about it.

---

## 7. Backend API Requirements

| Method   | Endpoint                                                                      | Notes                                                                   |
|----------|-------------------------------------------------------------------------------|-------------------------------------------------------------------------|
| `POST`   | `/api/v2/clusters/{cluster}/storage-pools/{pool}/volumes/{volume}/migrations` | Creates the migration and returns the new paths                         |
| `PUT`    | `/api/v2/clusters/{cluster}/.../migrations/{migration}/continue`              | Starts the copy, which is what `Migrating` does on entry                |
| `GET`    | `/api/v2/clusters/{cluster}/.../migrations/{migration}`                       | One migration, read on entry and after a restart rather than on a timer |
| `DELETE` | `/api/v2/clusters/{cluster}/.../migrations/{migration}`                       | Cancels it, which is the abort path from `Validating` and `Migrating`   |
| `GET`    | `/api/v2/clusters/{cluster}/storage-pools/{pool}/volumes/?watch=true`         | The volume stream a completion is observed on                           |

**The `?watch=true` row is a Server-Sent-Events subscription rather than a request
that returns**, and it arrives with the control plane's SSE work rather than with
this design ([`design-crd-model.md`](design-crd-model.md) §7.7). Until that lands it
is the one external dependency this design cannot satisfy on its own.

**Progress is read from the store the stream feeds, not by calling `GET` on a
timer.** [`design-crd-model.md`](design-crd-model.md) §7.7 makes that the rule for
every design in this group, and the two reads above are what remains of the direct
calls: a `GET` on entry to a step, so it acts on state rather than on an assumption,
and a `GET` after a restart, when a recovered `status.migration.migrationUUID` has to
be matched against a store the operator has only begun to fill. Neither is a poll,
and `Migrating`'s completion is observed on the stream.

**One property this design depends on and the control plane has been observed to
violate.** The `continue` call's final step must not be reported as failed when it
in fact succeeded: a five-second read timeout on a call that took slightly longer
caused the operator to retry a transfer that had already committed, and the retry
copied nothing while the source was unfrozen. `Migrating`'s completion condition is
therefore a predicate over the migration's reported state rather than the return
of the call that started it, which is the same rule
[`design-crd-model.md`](design-crd-model.md) §7.7 states for every step in the
group and the reason it is stated.

---

## 8. Observability

`VolumeMigration` is the one kind in this group with existing metrics: the
auto-rebalancer exports `simplyblock_rebalancer_migrations_total` and its
neighbors. Those describe the rebalancer's decisions rather than the operation's
lifecycle, so the table below is additions beside them rather than a replacement.

### 8.1 Kubernetes events

Events land on the `PersistentVolumeOps`, which is the audit record, and are
mirrored onto the `PersistentVolumeClaim` where one exists, because a claim is
what an application owner has open and a volume being moved is something they may
notice.

**An event on a cluster-scoped object is still a namespaced object itself, and it
lands in `default`.** An `Event` takes its namespace from the object it is about, and
client-go substitutes `default` when that is empty, which for a cluster-scoped kind is
always. So the operation's own events sit in a namespace nothing else about this
product uses, while the claim's mirrored copies sit with the workload.
`kubectl describe pvops` finds them either way, while a namespace-scoped
`kubectl get events` in the operator's own namespace finds neither, which is
surprising enough to be worth writing down and is the second reason the mirror onto
the claim exists.

| Event                                                            | Type      | Reason                 | On                    |
|------------------------------------------------------------------|-----------|------------------------|-----------------------|
| The operation is waiting because another holds the volume's lock | `Normal`  | `OperationQueued`      | `PersistentVolumeOps` |
| The operation acquired the volume's lock and started             | `Normal`  | `OperationStarted`     | `PersistentVolumeOps` |
| The volume carries no usable CSI volume handle to address it     | `Warning` | `ClusterUnresolvable`  | `PersistentVolumeOps` |
| The target node is not online                                    | `Warning` | `TargetNodeNotReady`   | `PersistentVolumeOps` |
| The target node is the volume's current node                     | `Warning` | `TargetNodeIsSource`   | `PersistentVolumeOps` |
| A validation Job failed, so the target's paths are unusable      | `Warning` | `ValidationFailed`     | `PersistentVolumeOps` |
| The copy started                                                 | `Normal`  | `MigrationStarted`     | `PersistentVolumeOps` |
| The operation finished successfully                              | `Normal`  | `OperationSucceeded`   | `PersistentVolumeOps` |
| The operation failed                                             | `Warning` | `OperationFailed`      | `PersistentVolumeOps` |
| The operation was canceled                                       | `Normal`  | `OperationAborted`     | `PersistentVolumeOps` |
| A step's deadline expired                                        | `Warning` | `StepDeadlineExceeded` | `PersistentVolumeOps` |
| A path was left connected and has been cleaned up                | `Warning` | `StalePathCleaned`     | `PersistentVolumeOps` |
| A delete is held because the cleanup has not finished            | `Warning` | `CleanupBlocked`       | `PersistentVolumeOps` |

**`StalePathCleaned` is a warning even though the operator fixed it**, because a
path surviving its Job means the cleanup did not run when it should have, and §5
exists because that condition blocked every later migration on the volume.

### 8.2 Prometheus metrics

| Metric                                                                | Labels                        | Description                                                          |
|-----------------------------------------------------------------------|-------------------------------|----------------------------------------------------------------------|
| `simplyblock_persistentvolume_operations_total`                       | `cluster`, `action`, `result` | Operations reaching a terminal phase                                 |
| `simplyblock_persistentvolume_operation_duration_seconds`             | `cluster`, `action`           | Histogram from creation to a terminal phase                          |
| `simplyblock_persistentvolume_operation_step_duration_seconds`        | `cluster`, `action`, `step`   | Histogram per step, which is where a slow migration is actually slow |
| `simplyblock_persistentvolume_operation_step_deadline_exceeded_total` | `cluster`, `action`, `step`   | Steps that ran out of time                                           |
| `simplyblock_persistentvolume_operation_queued_seconds`               | `cluster`                     | Histogram of time held behind another operation on the same volume   |
| `simplyblock_persistentvolume_operation_active_count`                 | `cluster`, `node`             | Gauge of non-terminal operations, by the node they are moving to     |
| `simplyblock_persistentvolume_stale_paths_total`                      | `cluster`                     | Paths found connected after their validation Job was gone            |

**`step_duration_seconds` split by step is the one that would have found the
production defect.** A migration whose `Validating` step is fast and whose
`Migrating` step is fast, but which takes half an hour, is a migration spending its
time somewhere the merged phase enum could not name.

**`stale_paths_total` should be flat at zero.** Any value above it means §5's
`Verifying` step is not doing its job, and the consequence is not visible until a
later migration on the same volume fails.

---

## 9. Testing Strategy

Scenarios live in
[`tests/test-plan-persistentvolumeops.md`](../../tests/test-plan-persistentvolumeops.md)
and only there.

The cluster resolution of §3, the webhook's rejections of §4.3, the lock's
acquisition, release, and stale-lock break of §6, and the step graph's abort edges
are all unit-testable. The webhook's table pairs cleanly: one admitted volume per
rejected one, and a foreign-driver volume beside a simplyblock volume that differs
only in `spec.csi.driver`. The lock is the
one worth a table of its own: a 409 on a contended acquire, a release that declines
to clear an annotation naming somebody else, a holder that is terminal, and a holder
that no longer exists are four distinct outcomes and each is reachable with a fake
client. So is the phase-and-step split, which
is mostly a matter of asserting that no step value ever appears in
`status.phase`.

The risk unit tests do not reach is the data path, and it is the whole point of
the kind. A migration that reports `Succeeded` and lost writes is
indistinguishable from a correct one at every level above the bytes, and this
repository has field evidence of exactly that: a migration whose cutover froze the
source more than once lost writes silently, and a batch transfer that skipped its
in-flight drain corrupted data that every status field said was fine. Those are
end-to-end scenarios with fio verification and nothing less.

---

## 10. Migration from the Registered API

| Registered                                        | This design                                                    | Cost                                                                                                                                     |
|---------------------------------------------------|----------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------|
| `VolumeMigration`                                 | `PersistentVolumeOps` (§4)                                     | A rename is a new CRD. In-flight migrations should be drained rather than converted                                                      |
| `spec.pvName`                                     | `spec.persistentVolumeName` (§4.1)                             | Spec rename, spelled out rather than abbreviated                                                                                         |
| `spec.targetNodeUUID`                             | `spec.migrate.targetNodeRef`, a namespace and a name (§4.1)    | Spec regrouping. It names the Kubernetes object rather than the backend UUID, and carries a namespace because the kind is cluster-scoped |
| No `spec.action`                                  | `PersistentVolumeOpsAction` (§4.1)                             | Additive, and it is what makes a second action possible                                                                                  |
| One enum merging phase and step                   | `status.phase` and `status.step` (§4.2, §5)                    | Status split. `design-crd-model.md` §9.5 records it as one of three                                                                      |
| `Completed`                                       | `Succeeded`                                                    | Enum value rename, so every `Ops` kind in the group agrees                                                                               |
| Fourteen flat status fields                       | `status.migration` plus the phase spine (§4.2)                 | Status regrouping                                                                                                                        |
| The CSI volume handle split inline, in two places | `lvol.VolumeHandle.Split()` (§3)                               | No behavior change. The same three-part parse exists once, in `atlas-lib`, with the errors already written                               |
| No cleanup step                                   | `Verifying` (§5)                                               | The defect that reached production: validation paths outlived their Jobs                                                                 |
| No finalizer, and nothing guards a delete         | A `DELETE` webhook (§4.3) and a finalizer (§5.1)               | New. A delete during `Verifying` is refused, and an earlier one unwinds rather than orphaning Jobs and paths                             |
| Namespaced                                        | Cluster-scoped (§3)                                            | Scope change, so a rename is required anyway, and the exclusion of §6 becomes total                                                      |
| Owned by its creator, by controller reference     | `spec.creatorRef` with a UID, plus a finalizer cascade (§11.1) | A cluster-scoped object cannot have a namespaced owner, so the reference becomes explicit and the cascade a controller's                 |
| No exclusion between two operations on one volume | The `active-ops` annotation on the volume (§6)                 | New, and the same optimistic lock every other `Ops` kind takes, in metadata rather than status                                           |
| Nothing rejects an operation on a foreign volume  | `PersistentVolumeOpsValidator` (§4.3)                          | New. A volume another CSI driver provisioned is refused at create rather than failing later                                              |
| No `observedGeneration`                           | Present                                                        | Required by `design-crd-model.md` §7.9                                                                                                   |
| Nothing deletes a terminal operation              | `opsRetention` and `opsHistoryLimit` on the cluster (§11.2)    | New, and required by the volume: one rebalance produces one object per volume moved                                                      |
| `vmig`                                            | `pvops`                                                        | Short name changes with the kind                                                                                                         |
| Rebalancer metrics only                           | The seven metrics of §8.2                                      | Additions beside the existing rebalancer ones                                                                                            |
| No event                                          | The twelve reasons of §8.1                                     | New                                                                                                                                      |

**In-flight operations should be drained rather than converted.** A running
`VolumeMigration` holds a backend migration and a set of NVMe-oF paths, and the
step machine it would restore into did not exist when it started. Letting them
finish before the CRDs change is the only handling that does not risk leaving a
path connected with nothing tracking it.

---

## 11. Ownership and Retention

Two things about the object rather than about the operation: where it came from, now
that the scope of §3 forbids an owner reference, and when it stops existing, now that
one rebalance produces one object per volume moved.

### 11.1 The Creator Reference

**`spec.creatorRef` names the object that created this one, and carries its UID.**
Kubernetes has this exact shape in core twice, and solves it the same way both times
rather than with an owner reference, because an owner reference is unavailable for
the reason §3 gives. A `PersistentVolume` points at its claim through
`spec.claimRef`, an `ObjectReference` holding a namespace, a name, and a UID. A
cluster-scoped `VolumeSnapshotContent` points at its namespaced `VolumeSnapshot`
through `spec.volumeSnapshotRef`, the same three fields. Both pair the reference with
a policy field saying what a deletion means and a finalizer on each side, and in both
cases a controller performs the cascade the garbage collector cannot. The reference
and the finalizers carry over here unchanged. The policy field does not need to,
because the volume already has one: `persistentVolumeReclaimPolicy` is what decides
whether a deleted claim takes the operation with it (§5.1), so this kind adds no
policy of its own.

**The UID is the load-bearing part.** A reference by kind, namespace, and name alone
lets a re-created creator adopt the previous creator's operations: a `StorageNodeOps`
deleted and recreated under the same name would inherit a fan-out it did not issue
and cascade a delete over it. `spec.claimRef.uid` exists in core Kubernetes for that
reason, and matching on it is what makes the cascade address one creator rather than
one name.

**A label carries the managing kind alongside it, because a reference cannot be
selected on.** `storage.simplyblock.io/managed-by` holds the kind of the controller
that created the operation, `storagenodeops` for a drain's fan-out, and it is what a
watch mapping and a `List` use to find candidates without reading every object in the
cluster. It is the group's one key for this
([`design-crd-model.md`](design-crd-model.md) §7.3), so it narrows rather than
identifies: the label finds the operations some drain created and the UID in
`spec.creatorRef` says which drain, which is the same division `pvc.spec.volumeName`
and `pv.spec.claimRef.uid` have in core.

**The cascade is the creator's finalizer, and it aborts before it deletes.**
A `StorageNodeOps` being deleted sets `spec.abort` on every operation whose
`creatorRef` matches it by UID, waits for each to reach a terminal phase, and deletes
them then. It authored those specs at creation, so writing the one mutable field on
them is the creator acting as their author rather than a controller editing somebody
else's spec. Aborting first is what makes the cascade stop the work: a delete alone
would be held by each member's own finalizer until its migration finished anyway
(§5.1). This is what [`design-storagenode.md`](design-storagenode.md) §8.4 loses when
the controller reference goes, restored explicitly: the wake-up is the watch, and the
cascade is the finalizer.

**`spec.creatorRef` is absent on an operation written by hand**, which is the case
that has no creator to point at, and nothing cascades over it.

### 11.2 Retention

**A terminal operation is deleted once `status.completedAt` is older than
`StorageCluster.spec.opsRetention`**, defaulting to seven days. The setting lives on the
cluster because retention is a fleet policy rather than a per-operation one, and this
kind can read it because it resolves its cluster from the volume handle (§3). One
setting covers the operations of every kind rather than one field per kind, so a
deployment states its audit-retention policy once and every `Ops` controller reads it.

**A non-terminal operation is never deleted by retention.** A migration that has
been running for eight days is the case somebody needs to see, and a policy that
removed it would hide the only interesting one.

**A count bound holds beside the duration, because this kind is the one where a
single event produces many objects.** A fifty-volume drain creates fifty operations
in a minute, and seven days of them makes a `kubectl get pvops` unreadable long
before it troubles the API server. So the last `StorageCluster.spec.opsHistoryLimit`
terminal operations per volume are kept and older ones are deleted, defaulting to
three, which is the bound `CronJob.spec.successfulJobsHistoryLimit` applies for the
same reason: objects a controller creates repeatedly are useful as the last few
rather than as a week of them. The field selector index of §6 is what makes a
per-volume count affordable.

**Duration and count are both floors, and whichever removes an object first wins.**
An operation is deleted when it is older than `opsRetention` or when
`opsHistoryLimit` newer terminal operations exist for its volume. A drain's
fifty objects therefore collapse to three per volume within a reconcile, while a
single hand-written migration survives its seven days.

**Nothing deletes a terminal operation whose creator still exists and still lists
it**, which is the ordering that keeps retention from racing a cascade. The creator's
finalizer (§11.1) deletes its own fan-out, and retention only ever removes what
nothing is tracking.

---

## 12. Open Questions

None. Every decision this kind turns on is taken in the sections above, from the
single action and the three candidates declined against it (§4.1) to both retention
bounds and their defaults (§11.2).

The section is here rather than absent because an absent one cannot be told from an
oversight. A question that arrives later belongs in it.

---

## Appendix A: `persistentvolumeops_types.go`

The type as it is to be written. Everything the sections above show in Go is an
excerpt of this appendix, and this is the only place any type appears whole.

```go
// PersistentVolumeOpsAction is the operation a PersistentVolumeOps performs.
// The kind is named for its target rather than for the action so that it can carry
// more than one; it carries one, and no second one is planned (§4.1).
// +kubebuilder:validation:Enum=Migrate
type PersistentVolumeOpsAction string

const (
	PersistentVolumeOpsActionMigrate PersistentVolumeOpsAction = "Migrate"
)

// PersistentVolumeOpsPhase is the operation's own progress. Succeeded rather
// than Completed, so that every Ops kind in this group reports the same five
// phases and an alert on completion matches one value.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type PersistentVolumeOpsPhase string

const (
	PersistentVolumeOpsPhasePending   PersistentVolumeOpsPhase = "Pending"
	PersistentVolumeOpsPhaseRunning   PersistentVolumeOpsPhase = "Running"
	PersistentVolumeOpsPhaseSucceeded PersistentVolumeOpsPhase = "Succeeded"
	PersistentVolumeOpsPhaseFailed    PersistentVolumeOpsPhase = "Failed"
	PersistentVolumeOpsPhaseAborted   PersistentVolumeOpsPhase = "Aborted"
)

// PersistentVolumeOpsStep is one step of a running volume operation. The
// registered kind merges these with the phases above into one enum, so that
// Validating sits beside Completed.
// +kubebuilder:validation:Enum=Validating;Migrating;Verifying
type PersistentVolumeOpsStep string

const (
	// Validating creates the backend migration and starts a Job per new NVMe-oF
	// path to check the target is reachable.
	PersistentVolumeOpsStepValidating PersistentVolumeOpsStep = "Validating"
	// Migrating continues the migration, which is what starts the data copy. It
	// is not called Running, because status.phase already has that value and a
	// step sharing a phase's name is the confusion this split exists to end.
	PersistentVolumeOpsStepMigrating PersistentVolumeOpsStep = "Migrating"
	// Verifying deletes the validation Jobs and confirms no path was left
	// connected. It is a declared step rather than a deferred call because a
	// crash between the copy finishing and the cleanup must restart into it.
	PersistentVolumeOpsStepVerifying PersistentVolumeOpsStep = "Verifying"
)

// StorageNodeReference locates a StorageNode from a cluster-scoped object. Every
// other reference in this API group is a bare name, which works because both
// objects are namespaced and a name means the same namespace. A cluster-scoped
// object has no namespace for a bare name to mean, so this one carries both, for
// the same reason pv.spec.claimRef does in core Kubernetes (§4.1).
type StorageNodeReference struct {
	// Namespace is where the StorageNode lives, which is the namespace of the
	// StorageCluster that owns it.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Name is the StorageNode object's name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// MigrateVolumeSpec parameterizes the Migrate action.
type MigrateVolumeSpec struct {
	// TargetNodeRef locates the StorageNode to move the volume's backing logical
	// volume to. It names the Kubernetes object rather than the backend UUID, so
	// that a migration can be written by hand without looking one up; the
	// controller resolves the UUID from the node's status. The node's cluster must
	// be the volume's, which the webhook checks rather than the type (§4.3). The
	// marker sits on the reference rather than on its fields, so the pair is
	// immutable together and a target cannot be half-changed.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	TargetNodeRef StorageNodeReference `json:"targetNodeRef"`
}

// CreatorReference names the object that created a PersistentVolumeOps. It exists
// because a cluster-scoped object cannot be owned by a namespaced one, so the
// reference core Kubernetes uses in the same situation is written out instead: a
// PersistentVolume names its claim through spec.claimRef and a
// VolumeSnapshotContent names its snapshot through spec.volumeSnapshotRef, both
// with a UID (§11.1).
type CreatorReference struct {
	// Kind is the creating object's kind, which is StorageNodeOps for a drain.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Namespace and Name locate it. The storage.simplyblock.io/managed-by label
	// carries the kind alone, since a reference cannot be selected on and a label
	// value admits no namespace separator (design-crd-model.md §7.3).
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// UID is what makes the reference address one creator rather than one name: a
	// creator deleted and recreated under the same name must not inherit the
	// fan-out it did not issue.
	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`
}

// PersistentVolumeOpsSpec is one operation to perform against one
// PersistentVolume. The rule keeps the action and its parameter block in agreement,
// which is a statement about this object alone and so belongs on the type rather
// than in the webhook (§4.3).
// +kubebuilder:validation:XValidation:rule="self.action == 'Migrate' ? has(self.migrate) : !has(self.migrate)",message="migrate is required for action Migrate and must be absent otherwise"
type PersistentVolumeOpsSpec struct {
	// PersistentVolumeName names the PersistentVolume this operation acts on. It
	// is a name rather than a reference because a PersistentVolume is
	// cluster-scoped, and it names the volume rather than the claim because a
	// claim can be deleted while its volume is retained.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	PersistentVolumeName string `json:"persistentVolumeName"`

	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action PersistentVolumeOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind. It is
	// expressible from Validating and Migrating and not from Verifying, which the
	// action's graph declares rather than this field: once the copy has
	// finished, the volume has moved and there is nothing to undo.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Migrate parameterizes action Migrate.
	// +optional
	Migrate *MigrateVolumeSpec `json:"migrate,omitempty"`

	// CreatorRef names the object that created this one, and its finalizer is what
	// deletes this one when it goes (§11.1). Absent on an operation written by
	// hand, which has no creator to cascade from. Immutable, because an operation
	// changing whose fan-out it belongs to would change who cascades over it.
	// +optional
	// +k8s:immutable
	CreatorRef *CreatorReference `json:"creatorRef,omitempty"`
}

// MigrationConnection is one NVMe-oF path the migration created on the target.
type MigrationConnection struct {
	// +optional
	NQN string `json:"nqn,omitempty"`
	// +optional
	Address string `json:"address,omitempty"`
	// +optional
	Port *int32 `json:"port,omitempty"`
}

// ValidationJob is one Job started to check a path is reachable. It is tracked
// so that Verifying can delete every one it started, including after a restart.
// Namespace is recorded for the same reason spec.migrate.targetNodeRef carries one
// (§4.1): a Job is namespaced and this object is not, so a name alone would not
// locate it after a restart.
type ValidationJob struct {
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// +optional
	NQN string `json:"nqn,omitempty"`
}

// MigrationStatus is everything about the migration rather than about the
// operation. It is durable working state: a controller that restarts mid-copy
// reads it to find the backend migration it started and the Jobs it has to clean
// up.
type MigrationStatus struct {
	// MigrationUUID is the control plane's identifier for the copy.
	// +optional
	MigrationUUID string `json:"migrationUUID,omitempty"`

	// ClusterUUID, PoolUUID, and VolumeUUID are the three parts of the volume's
	// CSI volume handle (lvol.VolumeHandle), recorded so that later steps address
	// the backend without re-reading the PersistentVolume, and so that a failed
	// operation says which volume it was working on after the PV is gone.
	// +optional
	ClusterUUID string `json:"clusterUUID,omitempty"`
	// +optional
	PoolUUID string `json:"poolUUID,omitempty"`
	// +optional
	VolumeUUID string `json:"volumeUUID,omitempty"`

	// SubsystemNQN is the volume's NVMe-oF subsystem.
	// +optional
	SubsystemNQN string `json:"subsystemNQN,omitempty"`

	// SourceNodeUUID is where the volume was before the move, recorded so that a
	// failure says what it was and not only what it was going to be.
	// +optional
	SourceNodeUUID string `json:"sourceNodeUUID,omitempty"`

	// MemberCount is how many volumes (namespaces) the migrated NVMe-oF subsystem
	// holds, as the control plane reports it. A migration is addressed by the
	// subsystem rather than by one volume, so more than one member means the
	// sibling volumes move along with the named one, and the count is both the
	// operation's blast radius and the term Migrating's deadline scales by (§5).
	// +kubebuilder:validation:Minimum=0
	// +optional
	MemberCount *int32 `json:"memberCount,omitempty"`

	// Connections are the paths the migration created on the target. Verifying
	// confirms none of them is left connected.
	// +optional
	Connections []MigrationConnection `json:"connections,omitempty"`

	// ValidationJobs are the Jobs started to check those paths.
	// +optional
	ValidationJobs []ValidationJob `json:"validationJobs,omitempty"`
}

// PersistentVolumeOpsStatus is the observed state of one volume operation.
type PersistentVolumeOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase PersistentVolumeOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine, as the shared
	// statemachine.KubeSnapshot (design-crd-model.md §3.1). The rule is what an
	// Enum marker would do if a marker could reach a field of a shared type. It is
	// persisted before the side effect that step performs.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','Migrating','Verifying']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Migration is everything about the migration rather than about the
	// operation.
	// +optional
	Migration *MigrationStatus `json:"migration,omitempty"`

	// DeferredSince is when the operation was first held, which is what the
	// auto-rebalancer reads to decide whether a migration has waited long enough
	// to give up on.
	// +optional
	DeferredSince *metav1.Time `json:"deferredSince,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StartedAt is when the operation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when it reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pvops
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=".spec.persistentVolumeName"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".spec.migrate.targetNodeRef.name"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// PersistentVolumeOps is a single operation performed against one
// PersistentVolume. It is the one Ops kind in this group whose target is a core
// Kubernetes type rather than a kind this group defines, so it cannot take a lock
// on its target, is cluster-scoped because its target is, cannot be owned by the
// namespaced operation that created it, and derives its cluster, pool, and volume
// from the volume's CSI handle rather than being told.
type PersistentVolumeOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PersistentVolumeOpsSpec   `json:"spec,omitempty"`
	Status PersistentVolumeOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PersistentVolumeOpsList contains a list of PersistentVolumeOps.
type PersistentVolumeOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PersistentVolumeOps `json:"items"`
}
```
