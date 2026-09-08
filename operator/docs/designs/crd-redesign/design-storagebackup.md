# Design Document: The Data-Protection Chain

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-30  
**Test Plan:** [`tests/test-plan-storagebackup.md`](../../tests/test-plan-storagebackup.md)

This document specifies the target model for the whole data-protection layer.
Four kinds are registered in a shape that predates the conventions of
[`design-crd-model.md`](design-crd-model.md), two of them are absorbed into one,
and §13 is the single record of what the rework changes against them.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [The Three Kinds](#3-the-three-kinds)
4. [StorageBackupPolicy](#4-storagebackuppolicy)
5. [StorageBackup](#5-storagebackup)
6. [StorageBackupOps](#6-storagebackupops)
7. [Every Reference Is Validated at Creation](#7-every-reference-is-validated-at-creation)
8. [Restore Produces a Claim](#8-restore-produces-a-claim)
9. [Retention Is the Control Plane's](#9-retention-is-the-control-planes)
10. [Backend API Requirements](#10-backend-api-requirements)
11. [Observability](#11-observability)
12. [Testing Strategy](#12-testing-strategy)
13. [Migration from the Registered API](#13-migration-from-the-registered-api)
14. [Open Questions](#14-open-questions)

Appendices:

- [Appendix A: `storagebackuppolicy_types.go`](#appendix-a-storagebackuppolicy_typesgo)
- [Appendix B: `storagebackup_types.go`](#appendix-b-storagebackup_typesgo)
- [Appendix C: `storagebackupops_types.go`](#appendix-c-storagebackupops_typesgo)

---

## Overview

Three kinds make up data protection. A `StorageBackupPolicy` schedules and
retains the backups of the claims it selects. A `StorageBackup` is one
point-in-time copy of one volume. A `StorageBackupOps` restores one into a new
claim.

They are specified together because they are one chain and because the rework
that makes them consistent is one change: of the four registered kinds, one
becomes the single action of a new third, one is retired because the store is the
inventory (§3), and the two that survive stop disagreeing about whether they
carry the `Storage` prefix.

---

## 1. Background

The backup kinds are the clearest illustration of the drift
[`design-crd-model.md`](design-crd-model.md) §1 opens with, because all four were
written at roughly the same time and still disagree with each other.

**They disagree about the prefix.** `StorageBackup` carries it. `BackupPolicy`,
`BackupRestore`, and `BackupImport` do not. Nothing distinguishes the one from
the three except which was written first.

**One of them is a one-shot operation modeled as an entity.** `BackupRestore` names a
target, does one thing, and terminates, which is the `Ops` contract exactly
([`design-crd-model.md`](design-crd-model.md) §3). Its name does not end in `Ops`, so a
reader has to open the type to learn which half of the model it belongs to.

**`StorageBackup.status` has twenty-two fields**, which is more than any other
status in the group and more than twice the next largest. It holds the claim's
namespace, the volume name, the pool name and UUID, the logical-volume ID and
name, the filesystem type, the snapshot ID and name, the source cluster UUID, the
backup ID, an S3 ID, a node ID, a previous backup ID, a size, an allowed-hosts
list, and two timestamps. Some of that is the backup. Most of it is a snapshot of
what the volume was when the backup was taken, which is worth keeping, and none
of it is grouped to say which is which.

**The cluster's backup configuration mixes a location with the copying's parameters.**
`spec.backup` names an endpoint and credentials, then carries `snapshotBackups`,
`withCompression`, `secondaryTarget`, and `localTesting` beside them, so the field that
says where backups go also says how they are made. Two of the four are toggles whose
names do not say so, which is why
[`design-crd-model.md`](design-crd-model.md) §9.6 notes its checker cannot see them.

---

## 2. Goals and Non-Goals

### Goals

- Specify the three kinds the target model has, and the absorption that turns
  four into three (§3).
- Specify what a `StorageBackup`'s status actually holds, grouped so that the
  backup and the volume it was taken from are distinguishable (§5).
- Specify what a restore produces, which is a claim, and who owns it (§8).
- Specify where retention lives, which is not this operator (§9).
- Rename the two kinds and the three booleans the conventions require (§13).

### Non-Goals

- **Not the backup mechanism.** How the control plane writes a backup to S3,
  what it does incrementally, and how it chains a backup to its predecessor are
  the control plane's, and this document specifies the operator's behavior at the
  boundary.
- **Not snapshots.** `VolumeSnapshot` is the upstream CSI kind and
  `VolumeGroupSnapshot` is the upstream group kind
  ([`design-crd-model.md`](design-crd-model.md) §8.5). A backup uses a snapshot
  as its source and is not one.
- **Not replication.** The four replication kinds are excluded from this model
  pending the CSI Addons redesign
  ([`design-crd-model.md`](design-crd-model.md) §2). A backup is a copy somewhere
  else, and replication is a live second copy, and the two are different
  problems.
- **Not the API group's conventions.** The entity and action split, the enum
  casing, the lock, and the observed generation belong to
  [`design-crd-model.md`](design-crd-model.md) and are cited rather than restated.

---

## 3. The Three Kinds

| Kind                  | Category | What it is                                                   |
|-----------------------|----------|--------------------------------------------------------------|
| `StorageBackupPolicy` | Entity   | Which claims are backed up, how often, and how many are kept |
| `StorageBackup`       | Entity   | One backup that exists in the cluster's store                |
| `StorageBackupOps`    | Action   | One operation against one backup, which is a restore         |

**The store is the inventory, and everything else follows from it.** A backup lives in
an S3 location, and that location is a property of the cluster:
`StorageCluster.spec.backup` names the bucket and the credentials, the operator walks
it, and every backup it finds becomes a `StorageBackup` (§5.1). A cluster with no store
configured has no backups to show, and a cluster given one has every backup in that
bucket, including the ones another cluster wrote.

**`StorageBackup` is an observation, not a request.** It records that a copy exists,
which is why the operator is the only thing that creates one and the only thing that
deletes one (§5.1). What decides that a copy is *taken* is `StorageBackupPolicy`, and
what the copy can be turned back into is a restore.

**The operation is a separate kind because it is a different category.** A policy and a
backup are things that are, while a restore is a thing that happens, with a phase, a step
machine, a write-ahead record, and a lock
([`design-crd-model.md`](design-crd-model.md) §3.1). `StorageBackupOps` carries one
action today and keeps `spec.action` regardless, for the reason every single-action kind
in the group does: the shape does not have to change the day it gains a second.

---

## 4. StorageBackupPolicy

Declared in `operator/api/v1alpha1/storagebackuppolicy_types.go`, short name
`sbp`, and reconciled by `StorageBackupPolicyReconciler` in
`operator/internal/controllers/backup/storagebackuppolicy_controller.go`. All three
of this document's controllers share that package, which is the data-protection band
([`design-crd-model.md`](design-crd-model.md) §7.10). The type is Appendix A.

### 4.1 What it selects

The registered policy names a cluster and nothing else, and the operator attaches
it to volumes through a control-plane call. What is missing is the Kubernetes
half: which claims the policy covers.

```go
// ClaimSelector selects the PersistentVolumeClaims this policy backs up. An
// absent selector selects nothing, which is the safe reading: a policy that
// silently covered every claim in the namespace would back up more than its
// author intended, and the failure would be a bill rather than an error.
// +optional
ClaimSelector *metav1.LabelSelector `json:"claimSelector,omitempty"`
```

**An absent selector selects nothing, and that is the whole argument for the
default.** The alternative reading, that an empty selector matches everything, is
what `metav1.LabelSelector` means in most Kubernetes APIs and is wrong here: the
cost of backing up too much is silent and recurring, and the cost of backing up
too little is an error somebody sees.

**The policy attaches and detaches as claims come and go.** A claim that starts
matching is attached, a claim that stops matching is detached, and
`status.attachedClaims` records what is currently covered. Detaching does not
delete the backups already taken, which is the same reasoning §9 gives for
retention: a policy governs what is taken, not what is kept.

### 4.2 Schedule and retention

`spec.schedule` is a cron expression, `spec.maxVersions` is how many backups to
keep, and `spec.maxAge` is how long to keep them. All three are passed to the
control plane, which does the scheduling and the pruning (§9).

**The operator does not run the schedule.** It reconciles the policy into the
control plane and reports what the control plane did. That is deliberate: a
backup schedule that stopped when the operator was down would be a backup
schedule nobody could rely on.

---

## 5. StorageBackup

Declared in `operator/api/v1alpha1/storagebackup_types.go`, short name `sb`, and
reconciled by `StorageBackupReconciler` in
`operator/internal/controllers/backup/storagebackup_controller.go`. The type is
Appendix B.

### 5.1 A backup object is discovered, not declared

**Every `StorageBackup` is created by the operator from what the store holds.** The
cluster names an S3 location and its credentials, the operator walks it, and one object
appears per backup it finds. Nothing about a backup is a request, so the spec is
identity and nothing else:

```go
// ClusterRef names the StorageCluster whose store this backup was found in. With
// BackupID it is the whole of this object's identity.
// +kubebuilder:validation:Required
// +k8s:immutable
ClusterRef string `json:"clusterRef"`

// BackupID is the identifier the store holds the backup under, and what a
// restore addresses.
// +kubebuilder:validation:Required
// +k8s:immutable
BackupID string `json:"backupID"`
```

**A user creates none and deletes none.** A validating webhook refuses both from every
identity except a service account in the operator's namespace, which is the same rule
[`design-storagedevice.md`](design-storagedevice.md) §5.3 applies to a device object and
for the same reason: the object is an observation of something the operator did not
invent and cannot conjure. Creating one by hand would claim a backup exists that the
store does not hold, and deleting one would hide a backup that is still there and still
restorable.

**Deleting the object does not delete the backup, which is why the delete is refused
rather than passed through.** A `StorageBackup` is a record. The copy is an object in
somebody's bucket, governed by that bucket's lifecycle policy and by the retention the
control plane applies (§9). An object that could be deleted would invite the reading
that deleting it frees the storage, and it does not.

**What a policy schedules and what the operator discovers meet in the same objects.**
`StorageBackupPolicy` tells the control plane which claims to back up and how often
(§4), the control plane writes the copies into the store, and the walk finds them. The
policy is the only thing that decides a backup is taken, so a policy and a discovery run
are two halves of one loop rather than two ways of creating an object.

### 5.2 Status, in three groups

The registered status has twenty-two ungrouped fields (§1). They divide cleanly
once the question is asked of each: is this the backup, or is it what the volume
was?

**Both groups are read from the store, which is what §14 Q3 does not yet specify.**
The walk of §5.1 produces these values, so what a backup's metadata looks like in the
bucket decides which of them can be populated at all. Until that is settled, the fields
below are what the design wants rather than what it knows it can fill.

**`status.backup` is the copy itself.** Its ID, its size, the S3 object behind
it, the backup it is incremental against, and when it started and finished.

**`status.source` is what the volume was when the copy was taken.** The
`PersistentVolume` name, the pool, the logical volume's ID and name, the
filesystem type, the snapshot it was taken from, and the cluster it lived in.

**Everything else is the operator's own view**: the phase, the message, the
observed generation, and `status.activeOpsRef`, which is the lock a restore holds
(§6).

**The source group is a snapshot and is never updated.** The pool a volume was in
when it was backed up is a fact about the backup, and rewriting it when the volume
moves would destroy the only record of where the data came from. That is the
property a restore depends on (§8), and it is why the group exists rather than
the fields simply being sorted.

```go
// BackupSource is what the volume was when the copy was taken. It is written
// once and never updated: the pool a volume was in when it was backed up is a
// fact about the backup, and a restore reads it to know what it is restoring.
type BackupSource struct {
	// PersistentVolumeName is the PV that was copied.
	// +optional
	PersistentVolumeName string `json:"persistentVolumeName,omitempty"`
	// ...
}
```

`status.phase` is `Pending`, `Creating`, `Available`, or `Failed`. `Available` is
the terminal success, and it is not called `Succeeded` because a backup is not an
operation: what matters afterward is that the copy can be restored, not that the
copying finished.

---

## 6. StorageBackupOps

Declared in `operator/api/v1alpha1/storagebackupops_types.go`, short name
`sbops`, and reconciled by `StorageBackupOpsReconciler` in
`operator/internal/controllers/backup/storagebackupops_controller.go`. The type is
Appendix C.

```go
// +kubebuilder:validation:Enum=Restore
type StorageBackupOpsAction string
```

| Action    | Steps                                                     | Target                              |
|-----------|-----------------------------------------------------------|-------------------------------------|
| `Restore` | `Validating` → `Restoring` → `AwaitingVolume` → `Binding` | A `StorageBackup` in this namespace |

**`spec.backupRef` is required, and names a `StorageBackup` in this namespace.** Every
backup in the cluster's store has an object (§5.1), so a restore always has one to
address and the reference is never optional.

**A restore cannot be aborted after its third step.** One that has created a logical
volume has produced something, and the graph declares no edge to `Aborted` from there,
which turns an abort arriving late into an `IllegalTransitionError` the controller
reports rather than a half-undone operation
([`design-crd-model.md`](design-crd-model.md) §3.1).

**`StorageBackupOpsValidator` refuses a `DELETE` from the same two steps**, which is
the group rule reading the same graph (§7 for the webhook,
[`design-crd-model.md`](design-crd-model.md) §3.1 for the rule). A restore in
`AwaitingVolume` or `Binding` has a logical volume the control plane created and a
claim this operation is on its way to binding, so removing the record leaves both
with nothing accounting for them. A restore in `Validating` or `Restoring` is deleted
freely, and the finalizer unwinds it.

**The lock is `StorageBackup.status.activeOpsRef`, and `Restore` is what takes it.**
It is the same field, under the same name, that every other entity with an `Ops`
companion carries ([`design-crd-model.md`](design-crd-model.md) §3.2), so a reader
who has learned it on a node or a pool already knows it here. Acquisition is an
optimistic-lock patch, which is what makes two operations reading an empty field
resolve to one winner and a 409 for the rest. Release checks that the field still
names the calling operation, and runs on every terminal path including
`storage.simplyblock.io/storagebackupops-finalizer`, so a `kubectl delete` on a
running restore cannot leave the backup locked by an object that no longer exists.

**A second restore of one backup waits rather than failing.** It is admitted, it
acquires nothing, and it holds at `Pending` with `OperationQueued` (§11.1) until
the first releases. That is queueing rather than rejection, and nothing bounds how
long it waits. Restoring one backup into two different claims is therefore
serialized, which is the group rule applied rather than a property this layer
needs: two such restores read the same object out of the store and do not contend
for anything the control plane owns. §14 Q5 is whether the backup is the right
thing to lock, or whether the claim a restore creates is.

---

## 7. Every Reference Is Validated at Creation

Three kinds here name other objects, and every field that does carries
`+k8s:immutable`. That combination is what makes admission the right place to check
them: a reference that is wrong at creation is wrong for the object's whole life,
because the field can never be corrected. The only remedy is to delete the object
and write it again, which is precisely what a rejected create would have asked for,
except that the rejection says so immediately and at no cost, while the alternative
is an object parked in `Failed` that somebody has to read, diagnose, and clean up.

A validating webhook per kind (`StorageBackupPolicyValidator`,
`StorageBackupValidator`, and `StorageBackupOpsValidator`) resolves each reference
and rejects the create when it does not resolve.

| Field                                      | Names                                       | Rejected when                            |
|--------------------------------------------|---------------------------------------------|------------------------------------------|
| `StorageBackupPolicy.spec.clusterRef`      | a `StorageCluster` in this namespace        | No such object                           |
| `StorageBackup.spec.clusterRef`            | a `StorageCluster` in this namespace        | No such object                           |
| `StorageBackupOps.spec.clusterRef`         | a `StorageCluster` in this namespace        | No such object                           |
| `StorageBackupOps.spec.backupRef`          | a `StorageBackup` in this namespace         | No such object, or its phase is `Failed` |
| `StorageBackupOps.spec.restore.targetPool` | a `StoragePool` in this namespace           | No such object                           |
| `StorageBackupOps.spec.restore.claimName`  | a claim to create, which must not exist yet | A claim of that name already exists (§8) |

**Existence is the webhook's and shape is the type's**, which is the division
[`design-persistentvolumeops.md`](design-persistentvolumeops.md) §4.3 draws. There is no
longer a presence rule to write, because one action means `spec.backupRef` is always
required, and a `Required` marker says that where nothing can be installed in front of
it. What remains for the webhook is whether the object the reference names exists, which
is a fact about a different object that CEL cannot see.

**Rejecting a `Failed` backup is a reference check rather than a state check.** A
`StorageBackup` whose phase is `Failed` has no copy behind it, so a restore naming it
is naming a record of something that does not exist. That is decided once and stays
decided: a backup does not recover from `Failed`, it is replaced by another backup.

**`spec.restore.claimName` is the one row where admission narrows a race it cannot
close.** A claim can be created between the operation's admission and its
`Validating` step, so the step keeps its own check and its `ClaimExists` event (§8).
Admission catches the ordinary mistake, restoring onto a name already in use, and
the step catches the interleaving. Both are needed, and the destructive case is the
reason: replacing a running workload's data with a backup's is the worst outcome this
document can produce, so it is guarded twice rather than once.

**Every lookup is namespace-local, which is what makes them affordable.**
`spec.claimRef` is a `corev1.LocalObjectReference` and every other reference is a
bare name meaning the same namespace, so each check is one `Get` rather than a
`List`. That is the group's convention for a namespaced kind, and the contrast is
`PersistentVolumeOps`, whose references carry a namespace because that kind is
cluster-scoped and a bare name has no namespace to mean
([`design-persistentvolumeops.md`](design-persistentvolumeops.md) §4.1).

**Two fields are deliberately not checked at admission.**
`StorageBackup.spec.snapshotName` names a snapshot in the control plane rather than
an object in Kubernetes, so checking it means a backend call on the admission path:
a create would then fail while the control plane is unreachable, which is a
different and worse failure than the one being prevented. `Creating` checks it, and
the snapshot's existence is a fact about now rather than one fixed at creation.
`StorageBackupPolicy.spec.claimSelector` is a selector rather than a reference, so
there is no object for it to resolve to, the API server validates its syntax, and
§4.1's reading that an absent selector selects nothing is reported by
`SelectorEmpty` rather than refused.

**`failurePolicy: Fail`**, for the reason
[`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §5 gives:
the webhook server runs inside the operator pod, so its availability tracks the
operator's, and while the operator is down nothing reconciles a backup anyway. A
window in which unresolvable immutable references are admitted is a window in which
objects that can only be deleted are created.

The webhooks need `get` on `storageclusters`, `storagebackups`, `storagepools`, and
`persistentvolumeclaims`, all of which the manager already reads to reconcile these
kinds.

---

## 8. Restore Produces a Claim

A restore's output is a `PersistentVolumeClaim` that a workload can mount, and
where that claim comes from is the design decision this section exists for.

**The operation creates the claim, from a template in its spec.**

```go
// RestoreSpec parameterizes the Restore action.
type RestoreSpec struct {
	// ClaimName is the PersistentVolumeClaim to create. It must not already
	// exist: a restore that adopted an existing claim would overwrite a volume
	// somebody else is using.
	// +kubebuilder:validation:Required
	ClaimName string `json:"claimName"`

	// TargetPool is the StoragePool to restore into, required because a
	// discovered backup may name a pool this cluster does not have (Appendix C).
	// +kubebuilder:validation:Required
	TargetPool string `json:"targetPool"`
	// ...
}
```

**The claim must not already exist, and that is a refusal rather than an
adoption.** Restoring over a claim a workload is using would replace its data
with the backup's, which is the single most destructive thing this document could
specify. `Validating` fails the operation with `ClaimExists` instead.

**The claim is not owned by the operation, and outlives it.** A restore's product
is data somebody asked to have back, and the operation is the record of having
produced it. Tying the two would mean deleting the audit record deleted the recovered
volume, which inverts what a restore is for: nobody restores a backup in order to
keep a `StorageBackupOps`. So the claim is an ordinary `PersistentVolumeClaim` from
the moment it exists, `status.claimName` records what the operation produced, and
deleting the operation leaves the data alone. Deleting the *claim* is the ordinary
path, governed by its own reclaim policy like any other volume.

**An unowned claim is not a leaked one, because it is only created once the data is
there.** `Binding` is the last step: `Restoring` posts the restore, `AwaitingVolume`
waits for the volume to exist, and only then is a claim written. A restore that fails
or is aborted before that leaves no claim to leak, and a claim that does exist has a
restored volume behind it.

**Without an owner reference, `Binding` needs another way to recognize its own
claim.** A crash between creating the claim and recording the bind would otherwise
restart into a claim of the right name that the step cannot distinguish from
somebody else's, and §7's refusal would then block the operation from finishing its
own work. Two things prevent that: `status.claimName` is written before the claim is
created, so a restarted step knows the name it was about to use, and the claim carries
`storage.simplyblock.io/restored-by` naming the operation. A claim bearing that label
with this operation's name is its own and is completed, and a claim without it is
somebody else's and is refused.

**The pool is named, never defaulted.** `status.source.poolName` records where the
volume lived (§5.2), and since 26.4 that may be a pool in another cluster entirely: a
backup is discovered from a store several clusters can share (§5.1), so the pool it came
from is not a pool this cluster is obliged to have. `spec.restore.targetPool` is
therefore required, and a restore says where the data lands rather than inheriting it
from wherever it was taken.

---

## 9. Retention Is the Control Plane's

`StorageBackupPolicy` carries `maxVersions` and `maxAge`, and the operator does
not enforce either.

**The control plane prunes, and the operator reconciles what it finds.** A backup
the control plane deleted becomes a `StorageBackup` whose subject is gone, and the
operator deletes the object to match. The stream of §10 is how it finds out, and a
level-triggered stream suits a mirror better than an edit log would: the question the
mirror asks is which backups exist now, so a create and a prune that coalesce into
one frame leave nothing to reconcile, which is the right answer rather than a missed
event. A pruned backup is expected, so its object is deleted silently rather than
marked `Failed`: the mirror distinguishes a backup that is gone because retention
removed it from one that is gone because something went wrong, and only the second is
worth an event.

**A `StorageBackup` created by hand is not pruned by a policy**, because it is
not attached to one. It is retained until somebody deletes it, and deleting the
object deletes the backup.

**A policy detached from a claim does not delete that claim's backups** (§4.1).
Retention governs how many copies are kept, and detaching governs whether new
ones are taken, and conflating them would make removing a label a data-deletion
event.

---

## 10. Backend API Requirements

| Method   | Endpoint                                                             | Notes                                                                                     |
|----------|----------------------------------------------------------------------|-------------------------------------------------------------------------------------------|
| `POST`   | `/api/v2/clusters/{cluster}/backups/backup-policies/`                | Creates a policy                                                                          |
| `PUT`    | `/api/v2/clusters/{cluster}/backups/backup-policies/{policy}`        | Applies a changed schedule or retention                                                   |
| `DELETE` | `/api/v2/clusters/{cluster}/backups/backup-policies/{policy}`        | 404 is success                                                                            |
| `POST`   | `/api/v2/clusters/{cluster}/backups/backup-policies/{policy}/attach` | Attaches a volume, called as claims start matching (§4.1)                                 |
| `POST`   | `/api/v2/clusters/{cluster}/backups/backup-policies/{policy}/detach` | Detaches one, and does not delete its backups (§9)                                        |
| `GET`    | `/api/v2/clusters/{cluster}/backups/export`                          | Lists backups, read once to seed the mirror of §9                                         |
| `GET`    | `/api/v2/clusters/{cluster}/backups/?watch=true`                     | The backup stream, which is how every change here is noticed                              |
| `GET`    | `/api/v2/clusters/{cluster}/backups/backup-policies/?watch=true`     | The policy stream, which carries what the control plane did                               |
| `POST`   | `/api/v2/clusters/{cluster}/backups/restore`                         | The `Restore` action's one call                                                           |
| `GET`    | The store itself, walked over S3                                     | One `StorageBackup` per backup found (§5.1). The walk and the metadata format are §14, Q3 |

**This layer streams like every other, because streamability is a property of the
store rather than of each endpoint.** Any object the control plane keeps in
FoundationDB is subscribable, so backups and backup policies are subscribed to and no
read here is a poll ([`design-crd-model.md`](design-crd-model.md) §7.7). The `export`
call survives as the seed for the mirror of §9 and as nothing else, since a
reconnecting stream carries its own snapshot.

**Both `?watch=true` rows are Server-Sent-Events subscriptions rather than requests
that return**, and they arrive with the control plane's SSE work rather than with this
design. Until that lands they are the external dependency this design cannot satisfy
on its own, and this layer is the one with no fallback: §5.1 builds a `StorageBackup`
from what the stream reports, so without it there is no inventory rather than a stale
one.

**The stream is what makes a backup this operator did not ask for legible.** §4.2
puts the schedule in the control plane, so most backups appear without the operator
requesting them, and until they were streamed the mirror learned about each one at
the next sweep. A backup that takes an hour now reports through the hour, and a
scheduled backup appears as it is created.

**Two consequences of a stream that a poll did not have**, both from
[`design-crd-model.md`](design-crd-model.md) §7.7. Delivery coalesces, so a step
must never wait for a specific transition: `Creating` completes on a predicate that
`Available` satisfies and that anything past it satisfies too, which matters because a
small backup can be `Available` before the operator has observed it start. And a
reconnect carries a full snapshot rather than a replay, which is the resync, and is
why nothing here schedules a periodic `export` to catch what a stream missed.

**Two streams per cluster, which is what the scope of the path parameters gives.**
Both endpoints are under `/clusters/{cluster}/backups`, so a deployment holds one
backup stream and one policy stream per cluster, alongside the streams
[`design-crd-model.md`](design-crd-model.md) §7.7 enumerates for the other kinds.

**One capability this design assumes and does not verify.** §5.2's
`status.backup.previousBackupID` and `size` are what make an incremental chain
legible, and the export endpoint is assumed to return both. If it does not, a
backup's cost and its dependency on its predecessor are invisible, which matters
because deleting a backup another one is incremental against is not obviously
safe.

---

## 11. Observability

The backup controllers emit no events and export no metrics. Both tables are new.

### 11.1 Kubernetes events

An event about a backup goes on the `StorageBackup`. An event about a policy goes
on the `StorageBackupPolicy`, which is what an administrator managing data
protection has open. An event about a restore goes on the `StorageBackupOps`,
which is the audit record.

| Event                                                        | Type      | Reason                 | On                    |
|--------------------------------------------------------------|-----------|------------------------|-----------------------|
| A claim started matching and was attached                    | `Normal`  | `ClaimAttached`        | `StorageBackupPolicy` |
| A claim stopped matching and was detached                    | `Normal`  | `ClaimDetached`        | `StorageBackupPolicy` |
| A claim matches but its volume is not backed by this cluster | `Warning` | `ClaimNotEligible`     | `StorageBackupPolicy` |
| The policy has no selector and therefore covers nothing      | `Warning` | `SelectorEmpty`        | `StorageBackupPolicy` |
| The backup completed and is restorable                       | `Normal`  | `BackupAvailable`      | `StorageBackup`       |
| A backup was found in the store and an object created        | `Normal`  | `BackupDiscovered`     | `StorageBackup`       |
| The store could not be walked                                | `Warning` | `StoreUnreachable`     | `StorageCluster`      |
| A backup left the store, so its object was removed           | `Normal`  | `BackupGone`           | `StorageCluster`      |
| The backup failed in the control plane                       | `Warning` | `BackupFailed`         | `StorageBackup`       |
| The backup's target is not configured on the cluster         | `Warning` | `BackupTargetMissing`  | `StorageBackup`       |
| A backup was pruned by the control plane                     | `Normal`  | `BackupPruned`         | `StorageBackupPolicy` |
| The operation is waiting for another to release the lock     | `Normal`  | `OperationQueued`      | `StorageBackupOps`    |
| The operation acquired the lock and started                  | `Normal`  | `OperationStarted`     | `StorageBackupOps`    |
| The operation finished successfully                          | `Normal`  | `OperationSucceeded`   | `StorageBackupOps`    |
| The operation failed                                         | `Warning` | `OperationFailed`      | `StorageBackupOps`    |
| The operation was aborted and its unwind finished            | `Normal`  | `OperationAborted`     | `StorageBackupOps`    |
| A step's deadline expired                                    | `Warning` | `StepDeadlineExceeded` | `StorageBackupOps`    |
| A restore was refused because the claim already exists       | `Warning` | `ClaimExists`          | `StorageBackupOps`    |
| A restore was refused because the source pool is gone        | `Warning` | `PoolNotFound`         | `StorageBackupOps`    |

**`SelectorEmpty` exists because of §4.1's default.** A policy with no selector
covers nothing, which is the safe reading and is also indistinguishable from a
policy that is working, so the event is what tells an author their policy is
inert.

**`BackupPruned` goes on the policy rather than the backup**, because the backup
object is being deleted at that moment and an event on a disappearing object is an
event nobody reads.

### 11.2 Prometheus metrics

| Metric                                                  | Labels                        | Description                                                              |
|---------------------------------------------------------|-------------------------------|--------------------------------------------------------------------------|
| `simplyblock_storagebackup_completions_total`           | `cluster`, `policy`, `result` | Backups reaching a terminal phase                                        |
| `simplyblock_storagebackup_duration_seconds`            | `cluster`, `policy`           | Histogram of the backup's own reported start-to-finish                   |
| `simplyblock_storagebackup_size_bytes`                  | `cluster`, `policy`           | Histogram of backup sizes, which is what a bill tracks                   |
| `simplyblock_storagebackup_age_seconds`                 | `cluster`, `claim`            | Gauge of the newest backup's age per claim, and the alert that matters   |
| `simplyblock_storagebackup_available_count`             | `cluster`, `claim`            | Gauge of restorable backups per claim                                    |
| `simplyblock_storagebackuppolicy_attached_claims_count` | `cluster`, `policy`           | Gauge of claims a policy currently covers                                |
| `simplyblock_storagebackup_operations_total`            | `cluster`, `action`, `result` | Restores reaching a terminal phase                                       |
| `simplyblock_storagebackup_operation_duration_seconds`  | `cluster`, `action`           | Histogram of operation durations, which is the recovery-time measurement |

**`backup_age_seconds` is the one to alert on, and it is the only metric here
that measures a promise rather than an activity.** A policy that stopped running
produces no failure and no event: it simply stops producing backups, and the only
thing that notices is the age of the newest one climbing past the schedule.

**`backup_duration_seconds` comes from the backup's own timestamps rather than from
observed transitions.** §5.2 records when the copy started and finished, and the
stream coalesces (§10), so a small backup can be `Available` before the operator ever
saw it `Creating`. Timing the phases the operator happened to observe would report
zero for the fastest backups and nothing at all for some, which is why the copy's own
reported interval is the measurement.

**`backupops_duration_seconds` for `Restore` is the recovery time objective,
measured.** Everything else in this document is about taking copies, and this is
the only number that says what getting one back costs.

---

## 12. Testing Strategy

Scenarios live in
[`tests/test-plan-storagebackup.md`](../../tests/test-plan-storagebackup.md) and only
there.

The selector logic of §4.1, the status grouping of §5.2, and the CEL rule that
makes `backupRef` conditional on the action are all unit-testable, and the
`Restore` graph's refusal to adopt an existing claim is one of the most important
assertions in this repository: it is the check standing between a restore and
overwriting a running workload's data.

The risk unit tests do not reach is the restore's data path. Proving that a
restored claim contains the data the backup held needs a real cluster, a real S3
target, a checksum taken before the backup and compared after the restore, and
that is the only scenario that verifies the layer does what it is for. A backup
that completes and cannot be restored is worse than no backup, and nothing short
of a round trip catches it.

---

## 13. Migration from the Registered API

| Registered                                                                          | This design                                                                 | Cost                                                                                                                                               |
|-------------------------------------------------------------------------------------|-----------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| `BackupPolicy`                                                                      | `StorageBackupPolicy` (§4)                                                  | A rename is a new CRD. Policies are user-authored, so existing ones must be converted                                                              |
| `BackupRestore`                                                                     | `StorageBackupOps` (§6)                                                     | Absorbed as `spec.action: Restore`, which is now the kind's only action                                                                            |
| `BackupImport`                                                                      | Retired                                                                     | The store is the inventory (§3), so a backup another cluster wrote needs no import                                                                 |
| `StorageBackup` declared to request a copy                                          | Discovered from the store (§5.1)                                            | Behavioral, and the largest change here. The object is an observation, and neither creatable nor deletable by a user                               |
| `StorageCluster.spec.backup`, an S3 target                                          | The same field, a `BackupStoreSpec` (`design-storagecluster.md` Appendix A) | The type is renamed away from a collision with `design-controlplane.md`'s, and gains the bucket, prefix, and region the walk needs                 |
| `spec.clusterName` on all four                                                      | `spec.clusterRef`                                                           | Spec rename, matching every other reference in the group                                                                                           |
| No claim selector on the policy                                                     | `spec.claimSelector` (§4.1)                                                 | Additive, and it is the Kubernetes half the policy never had                                                                                       |
| `status.attachedLvols`                                                              | `status.attachedClaims` (§4.1)                                              | Status regrouping, in Kubernetes terms rather than control-plane ones                                                                              |
| Twenty-two ungrouped status fields on `StorageBackup`                               | `status.backup` and `status.source` (§5.2)                                  | Status regrouping. The largest single readability change here                                                                                      |
| Untyped phases on all four                                                          | A typed phase per kind (§5.2, §6)                                           | Additive                                                                                                                                           |
| No step field on the two operations                                                 | `status.step` on `StorageBackupOps` (§6)                                    | The restore's four steps are improvised today                                                                                                      |
| No reference is checked anywhere                                                    | Every reference resolved at admission (§7)                                  | New. Each of these fields is immutable, so a wrong one could only ever be deleted rather than fixed                                                |
| No `observedGeneration` on any of the four                                          | Present on all three                                                        | Required by `design-crd-model.md` §7.9                                                                                                             |
| No exclusion between two restores of one backup                                     | `StorageBackup.status.activeOpsRef` (§6)                                    | New, and the same lock every other entity with an `Ops` companion carries. Two restores of one backup now queue rather than run together (§14, Q5) |
| No `shortName` on `BackupPolicy` or `StorageBackup`                                 | `sbp` and `sb`                                                              | Additive. `br` and `bi` are retired with their kinds                                                                                               |
| `spec.backup.snapshotBackups`, `withCompression`, `secondaryTarget`, `localTesting` | Removed (`design-storagecluster.md` Appendix A)                             | The store is a location, so how a copy is taken stays with the control plane                                                                       |
| No owner reference from a policy to its backups                                     | Established (§5.1)                                                          | Deleting a policy deletes the backup objects it created, not the backups themselves                                                                |
| Restore's claim owned by the operation                                              | Unowned, and created only at `Binding` (§8)                                 | Deleting the audit record no longer deletes the recovered volume, and a failed restore leaves no claim                                             |
| Polling every backend read                                                          | A `?watch=true` subscription (§10)                                          | Depends on `design-sse-push-notifications.md`, on the `sse` branch, as every other design in the group does                                        |
| No event, no metric                                                                 | Fourteen reasons and eight metrics (§11)                                    | New infrastructure                                                                                                                                 |

**The two kind removals are the breaking ones and they are not symmetrical with
the renames.** `BackupPolicy` becoming `StorageBackupPolicy` needs existing
policies converted, because a policy is user-authored state somebody meant. A
`BackupRestore` or `BackupImport` in flight when the CRDs change is an operation
that will not finish, and the honest answer is that both should be drained before
the upgrade rather than converted: a terminal one is an audit record whose loss is
tolerable, and a running one cannot be handed to a kind whose step machine did not
exist when it started.

---

## 14. Open Questions

**Q1: Whether deleting a backup that another is incremental against is safe.**
§5.2 records `previousBackupID`, so the chain is visible, and nothing in this
design uses it to refuse a deletion. Whether the control plane collapses the chain
on its own, or whether deleting a base backup invalidates every backup after it,
is not something this document can answer and is the question that decides whether
a guard is needed.

**Q2: What happens to a backup whose source pool is deleted.**
[`design-storagepool.md`](design-storagepool.md) §6 holds a pool's deletion while
volumes are bound, and a backup is not a bound volume. So a pool can be deleted
while backups taken from it still exist, and §8's restore then fails with
`PoolNotFound`. Whether that is acceptable, or whether backups should hold a
pool's deletion the way bound volumes do, is unsettled.

**Q3: How the store is walked and what a backup's metadata looks like.** §5.1 has the
operator list an S3 location and produce one object per backup, and neither half of that
is specified: not the layout it walks, not how one backup's objects are told from
another's, and not where the size, the source claim, the pool, the filesystem, and the
snapshot lineage §5.2 publishes are read from. A prefix convention plus a metadata
object per backup is the obvious shape and the control plane owns the format, so this is
a question for whoever writes it rather than one this document can settle. Until it is
settled, `status.source` and `status.backup` are fields with no stated source.

**Q4: How a one-off backup is requested.** §5.1 makes every `StorageBackup` a discovery,
so nothing in this design takes a backup on demand: a policy schedules them and the
control plane performs them. Somebody wanting one copy of one claim right now has a
policy with a schedule they do not want, or nothing. The candidates are an action on
`StorageBackupOps` whose target is a claim rather than a backup, which breaks the rule
that an `Ops` kind names one kind of target; a `StorageBackupPolicy` with a one-shot
schedule, which makes the policy a request object; and a field on the claim, which puts
storage policy in an application's object. None is obviously right, and the gap is real
enough to name.

**Q5: Whether the backup is the right thing a restore locks.** §6 puts
`activeOpsRef` on `StorageBackup` because that is the target `spec.backupRef` names
and because every entity with an `Ops` companion carries the field
([`design-crd-model.md`](design-crd-model.md) §3.2). The consequence is that two
restores of one backup into two different claims run one after the other, and
nothing about the store or the control plane requires that: both reads are of the
same immutable object. The alternative is to lock what a restore actually
contends for, which is the claim name it creates, and §7 already refuses a
`claimName` that exists. That leaves the backup an entity with an `Ops` companion
and no lock, which the group rule does not currently allow, so the question is for
`design-crd-model.md` as much as for this document.

---

## Appendix A: `storagebackuppolicy_types.go`

The type as it is to be written. Everything the sections above show in Go is an
excerpt of this appendix, and this is the only place any type appears whole.

```go
// StorageBackupPolicyPhase is where the operator has got to with this policy.
// +kubebuilder:validation:Enum=Pending;Active;Failed
type StorageBackupPolicyPhase string

const (
	StorageBackupPolicyPhasePending StorageBackupPolicyPhase = "Pending"
	StorageBackupPolicyPhaseActive  StorageBackupPolicyPhase = "Active"
	StorageBackupPolicyPhaseFailed  StorageBackupPolicyPhase = "Failed"
)

// StorageBackupPolicySpec schedules and retains the backups of the claims it
// selects. The operator reconciles it into the control plane and reports what
// the control plane did; it does not run the schedule itself, because a backup
// schedule that stopped when the operator was down would be one nobody could
// rely on.
type StorageBackupPolicySpec struct {
	// ClusterRef names the StorageCluster whose backup target this policy writes
	// to.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// ClaimSelector selects the PersistentVolumeClaims this policy backs up. An
	// absent selector selects nothing, which is deliberate: the cost of backing
	// up too much is silent and recurring, and the cost of backing up too little
	// is an error somebody sees.
	// +optional
	ClaimSelector *metav1.LabelSelector `json:"claimSelector,omitempty"`

	// Schedule is a cron expression the control plane runs the policy on.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// MaxVersions is how many backups of one claim to keep. Zero means no limit
	// by count.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxVersions *int32 `json:"maxVersions,omitempty"`

	// MaxAge is how long to keep a backup ("720h", "30d"). Empty means no limit
	// by age. Retention is enforced by the control plane, not here.
	// +optional
	MaxAge string `json:"maxAge,omitempty"`
}

// AttachedClaim is one claim the policy currently covers, in Kubernetes terms
// rather than in the control plane's.
type AttachedClaim struct {
	// Name is the PersistentVolumeClaim's name in this namespace.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// PersistentVolumeName is the volume behind it.
	// +optional
	PersistentVolumeName string `json:"persistentVolumeName,omitempty"`

	// AttachedAt is when the claim started matching the selector.
	// +optional
	AttachedAt *metav1.Time `json:"attachedAt,omitempty"`
}

// StorageBackupPolicyStatus is the observed state of the policy.
type StorageBackupPolicyStatus struct {
	// Phase is the operator's own view of this policy.
	// +optional
	Phase StorageBackupPolicyPhase `json:"phase,omitempty"`

	// PolicyID is the control plane's identifier for the policy.
	// +optional
	PolicyID string `json:"policyID,omitempty"`

	// AttachedClaims are the claims the policy currently covers. Detaching one
	// stops new backups being taken and deletes none of the existing ones.
	// +optional
	AttachedClaims []AttachedClaim `json:"attachedClaims,omitempty"`

	// LastBackupAt is when the control plane last completed a backup under this
	// policy, and it is what an age alert is computed from.
	// +optional
	LastBackupAt *metav1.Time `json:"lastBackupAt,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the policy moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbp
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=".spec.schedule"
// +kubebuilder:printcolumn:name="Claims",type=integer,JSONPath=".status.attachedClaims.length()"
// +kubebuilder:printcolumn:name="LastBackup",type=date,JSONPath=".status.lastBackupAt"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageBackupPolicy schedules and retains the backups of the claims it
// selects. It owns the StorageBackup objects taken under it, so deleting a
// policy deletes those records; the backups themselves are the control plane's
// and are pruned by its retention.
type StorageBackupPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageBackupPolicySpec   `json:"spec,omitempty"`
	Status StorageBackupPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageBackupPolicyList contains a list of StorageBackupPolicy.
type StorageBackupPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageBackupPolicy `json:"items"`
}
```

---

## Appendix B: `storagebackup_types.go`

```go
// StorageBackupPhase is where the operator has got to with this backup.
// Available is the terminal success rather than Succeeded, because what matters
// afterward is that the copy can be restored, not that the copying finished.
// +kubebuilder:validation:Enum=Pending;Creating;Available;Failed
type StorageBackupPhase string

const (
	StorageBackupPhasePending   StorageBackupPhase = "Pending"
	StorageBackupPhaseCreating  StorageBackupPhase = "Creating"
	StorageBackupPhaseAvailable StorageBackupPhase = "Available"
	StorageBackupPhaseFailed    StorageBackupPhase = "Failed"
)

// BackupSource is what the volume was when the copy was taken. It is written
// once and never updated: the pool a volume was in when it was backed up is a
// fact about the backup, and a restore reads it to know what it is restoring.
type BackupSource struct {
	// PersistentVolumeName is the PV that was copied.
	// +optional
	PersistentVolumeName string `json:"persistentVolumeName,omitempty"`

	// ClaimNamespace is the namespace the claim lived in.
	// +optional
	ClaimNamespace string `json:"claimNamespace,omitempty"`

	// PoolName and PoolUUID are the StoragePool the volume was in. A restore
	// defaults to this pool, and fails naming it when it no longer exists rather
	// than silently choosing another.
	// +optional
	PoolName string `json:"poolName,omitempty"`
	// +optional
	PoolUUID string `json:"poolUUID,omitempty"`

	// LvolID and LvolName identify the logical volume that was copied.
	// +optional
	LvolID string `json:"lvolID,omitempty"`
	// +optional
	LvolName string `json:"lvolName,omitempty"`

	// FSType is the filesystem the volume was formatted with, which a restore
	// needs in order to produce a mountable claim.
	// +optional
	FSType string `json:"fsType,omitempty"`

	// SnapshotID and SnapshotName identify the snapshot the backup was taken
	// from.
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`
	// +optional
	SnapshotName string `json:"snapshotName,omitempty"`

	// ClusterUUID is the cluster the volume lived in, which differs from the
	// backup's own cluster when another cluster wrote it.
	// +optional
	ClusterUUID string `json:"clusterUUID,omitempty"`
}

// BackupCopy is the copy itself: what it is, what it cost, and what it depends
// on.
type BackupCopy struct {
	// BackupID is the control plane's identifier for the copy.
	// +optional
	BackupID string `json:"backupID,omitempty"`

	// Size is the copy's size in bytes, which is what a bill tracks.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Size *int64 `json:"size,omitempty"`

	// PreviousBackupID names the backup this one is incremental against, where
	// the control plane reports one. It makes the chain legible, which matters
	// because deleting a backup another depends on is not obviously safe.
	// +optional
	PreviousBackupID string `json:"previousBackupID,omitempty"`

	// StartedAt and CompletedAt bound how long the copy took.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// StorageBackupSpec is the identity of one backup the operator found in a
// cluster's store, and nothing else. The object is created by the operator and by
// nobody else (§5.1), so there is no request here to carry: what the backup is of,
// how big it is, and where it came from are all observations and live in status.
type StorageBackupSpec struct {
	// ClusterRef names the StorageCluster whose store this backup was found in.
	// With BackupID it is the whole of this object's identity.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// BackupID is the identifier the store holds the backup under, and what a
	// restore addresses. It is the store's identifier rather than a name this
	// operator assigns, so the same backup is the same object however many
	// clusters have the location configured.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	BackupID string `json:"backupID"`
}

// StorageBackupStatus is the observed state of one backup, in three groups: the
// copy, what the volume was, and the operator's own view.
type StorageBackupStatus struct {
	// Phase is the operator's own view of this backup.
	// +optional
	Phase StorageBackupPhase `json:"phase,omitempty"`

	// APIStatus is the control plane's own lifecycle string, in the control
	// plane's spelling, which is why it carries no Enum here.
	// +optional
	APIStatus string `json:"apiStatus,omitempty"`

	// Backup is the copy itself.
	// +optional
	Backup *BackupCopy `json:"backup,omitempty"`

	// Source is what the volume was when the copy was taken.
	// +optional
	Source *BackupSource `json:"source,omitempty"`

	// ActiveOpsRef names the StorageBackupOps currently allowed to act on this
	// backup. Empty when none is running.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the backup moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sb
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=".status.source.claimName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=".status.backup.size"
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=".status.source.poolName",priority=1
// +kubebuilder:printcolumn:name="BackupID",type=string,JSONPath=".status.backup.backupID",priority=1
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=".status.backup.completedAt"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageBackup is one point-in-time copy of one volume. A backup created by a
// StorageBackupPolicy is owned by it; one created by hand is owned by nothing
// and outlives everything.
type StorageBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageBackupSpec   `json:"spec,omitempty"`
	Status StorageBackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageBackupList contains a list of StorageBackup.
type StorageBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageBackup `json:"items"`
}
```

---

## Appendix C: `storagebackupops_types.go`

```go
// StorageBackupOpsAction is the operation a StorageBackupOps performs. Restore acts
// on a StorageBackup, which is every backup in the cluster's store (§5.1).
// +kubebuilder:validation:Enum=Restore
type StorageBackupOpsAction string

const (
	StorageBackupOpsActionRestore StorageBackupOpsAction = "Restore"
)

// StorageBackupOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StorageBackupOpsPhase string

const (
	StorageBackupOpsPhasePending   StorageBackupOpsPhase = "Pending"
	StorageBackupOpsPhaseRunning   StorageBackupOpsPhase = "Running"
	StorageBackupOpsPhaseSucceeded StorageBackupOpsPhase = "Succeeded"
	StorageBackupOpsPhaseFailed    StorageBackupOpsPhase = "Failed"
	StorageBackupOpsPhaseAborted   StorageBackupOpsPhase = "Aborted"
)

// StorageBackupOpsStep is one step of a running backup operation. The enum stays
// flat as actions are added, because which steps belong to which action is
// declared by the graph rather than by this type.
// +kubebuilder:validation:Enum=Validating;Restoring;AwaitingVolume;Binding
type StorageBackupOpsStep string

const (
	// Both actions.
	StorageBackupOpsStepValidating StorageBackupOpsStep = "Validating"

	// Restore.
	StorageBackupOpsStepRestoring      StorageBackupOpsStep = "Restoring"
	StorageBackupOpsStepAwaitingVolume StorageBackupOpsStep = "AwaitingVolume"
	StorageBackupOpsStepBinding        StorageBackupOpsStep = "Binding"

)

// RestoreSpec parameterizes the Restore action and is ignored by the other.
type RestoreSpec struct {
	// ClaimName is the PersistentVolumeClaim to create. It must not already
	// exist: a restore that adopted an existing claim would replace a running
	// workload's data with the backup's.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClaimName string `json:"claimName"`

	// TargetPool is the StoragePool to restore into. It is required rather than
	// defaulted: a backup found in the store may have been written by another
	// cluster, so status.source.poolName names a pool this cluster need not have,
	// and which pool a volume lands in is a tenancy and QoS decision nobody
	// should make by omission.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	TargetPool string `json:"targetPool"`

	// ClaimLabels and ClaimAnnotations are applied to the created claim, so that
	// a restored volume can be selected by a policy or an application the same
	// way its original was.
	// +optional
	ClaimLabels map[string]string `json:"claimLabels,omitempty"`
	// +optional
	ClaimAnnotations map[string]string `json:"claimAnnotations,omitempty"`
}

// StorageBackupOpsSpec is one operation to perform against a backup.
type StorageBackupOpsSpec struct {
	// ClusterRef names the StorageCluster the operation runs against.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// BackupRef names the StorageBackup this operation acts on. Required, since
	// Restore is the only action and every backup in the store has an object
	// (§5.1).
	// +kubebuilder:validation:Required
	// +k8s:immutable
	BackupRef string `json:"backupRef"`

	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action StorageBackupOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind. A
	// restore cannot be aborted once it has created a logical volume, which the
	// action's graph declares rather than this field.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Restore parameterizes action Restore.
	// +optional
	Restore *RestoreSpec `json:"restore,omitempty"`
}

// StorageBackupOpsStatus is the observed state of one backup operation.
type StorageBackupOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase StorageBackupOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine. It is
	// persisted before the side effect that step performs.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','Restoring','AwaitingVolume','Binding']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// ClaimName is the PersistentVolumeClaim a Restore created, owned by this
	// operation.
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// BackupRef is the StorageBackup the restore read from, recorded so the
	// operation says what it restored after the object list has moved on.
	// +optional
	BackupRef string `json:"backupRef,omitempty"`

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
// +kubebuilder:resource:scope=Namespaced,shortName=sbops
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=".spec.backupRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=".status.claimName",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageBackupOps is a single operation performed against one StorageBackup. It
// runs to a terminal phase and stays afterward as the audit record of what was
// restored, into which pool, and how it ended.
type StorageBackupOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageBackupOpsSpec   `json:"spec,omitempty"`
	Status StorageBackupOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageBackupOpsList contains a list of StorageBackupOps.
type StorageBackupOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageBackupOps `json:"items"`
}
```
