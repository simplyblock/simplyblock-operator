# Design Document: The StoragePool and Its Operations

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-29  
**Test Plan:** [`tests/test-plan-storagepool.md`](../../tests/test-plan-storagepool.md)

This document specifies the target model. `StoragePool` is registered and in a
shape that predates the conventions of
[`design-crd-model.md`](design-crd-model.md), `StoragePoolOps` does not exist,
and §11 is the single record of what the rework changes against what ships.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [StoragePool: API](#3-storagepool-api)
4. [StoragePool: Controller](#4-storagepool-controller)
5. [A Class Is Assigned to a Pool](#5-a-class-is-assigned-to-a-pool)
6. [Deleting a Pool, and Deleting a Cluster That Has One](#6-deleting-a-pool-and-deleting-a-cluster-that-has-one)
7. [StoragePoolOps](#7-storagepoolops)
8. [Backend API Requirements](#8-backend-api-requirements)
9. [Observability](#9-observability)
10. [Testing Strategy](#10-testing-strategy)
11. [Migration from the Registered API](#11-migration-from-the-registered-api)
12. [Open Questions](#12-open-questions)

Appendices:

- [Appendix A: `storagepool_types.go`](#appendix-a-storagepool_typesgo)
- [Appendix B: `storagepoolops_types.go`](#appendix-b-storagepoolops_typesgo)

---

## Overview

`StoragePool` carves a `StorageCluster` into units with their own capacity limit
and QoS ceilings. It is the tenancy boundary, and it is also the join between two
worlds: the storage administrator declares pools, and the application developer
consumes a `StorageClass` assigned to one of them.

That join is the reason this kind is harder than its field count suggests. It is
held together by three labels, an opaque parameter map, and a finalizer, and none of
the three is a Kubernetes reference
([`design-crd-model.md`](design-crd-model.md) §8.3). §5 specifies what that costs
and §6 specifies what it means when something is deleted.

---

## 1. Background

`StoragePool` works, and its problems are shape rather than behavior.

**It carries the antipattern the Entity and Ops split exists to replace.**
`spec.action` is declared as "triggers an imperative pool operation" and is
marked `FIXME: Unused for now`. Nothing reads it.
[`design-crd-model.md`](design-crd-model.md) §3 rejects exactly this construction,
because an action field allows one operation at a time, keeps no history, and
cannot distinguish in-progress from done. The field is the reason
`StoragePoolOps` is in the target model.

**It has a `spec.status`.** Described as "an optional desired-status hint for
backend workflows" and also `FIXME: Unused for now`. A spec field named `status`
is the clearest possible violation of the rule that a field a user sets and a
controller sets are two fields, and it sits beside `status.status`, which is the
backend value the controller writes.

**Its QoS is expressed twice, in two vocabularies.** `spec.qos.iops` and
`spec.qos.throughput.{read,write,readWrite}` are the pool's ceilings, and
`spec.storageClassParameters.{qosRwIops,qosRwMbytes,qosRMbytes,qosWMbytes}` are
the per-volume defaults for volumes in that pool. Both are QoS, both are on the
same object, they use different names and different units, and nothing in either
says which one a reader is looking at. There is a third vocabulary below both of
them, in the `parameters` map a class carries, and §5.1 supersedes it:
`qos_rw_iops` is a total that reads as an access mode, and `qos_rw_mbytes` is a rate
that reads as a size.

**`spec.dhchap` is one of the eleven misnamed toggles**
([`design-crd-model.md`](design-crd-model.md) §9.6), and it is the one whose
misnaming has a security shape: a field spelled as a noun reads as a description
rather than a switch.

---

## 2. Goals and Non-Goals

### Goals

- Specify `StoragePool`'s spec and status, and separate the pool's own ceilings
  from the defaults it hands to the volumes in it (§3).
- Specify the `StorageClass` join in full: what creates the class, what the class
  carries back, and why none of it is a reference (§5).
- Decide what deleting a cluster does to a pool that still has bound volumes,
  which [`design-crd-model.md`](design-crd-model.md) §9.3 leaves open (§6).
- Specify `StoragePoolOps`, replacing the unused `spec.action` (§7).
- Remove the two dead fields, and say why removing a published field is
  affordable now and not later (§11).

### Non-Goals

- **Not the cluster.** `StorageCluster` is
  [`design-storagecluster.md`](design-storagecluster.md). This document specifies
  what a pool takes from it and what it does when the cluster goes away.
- **Not the volume.** A volume is a `PersistentVolumeClaim` and a
  `PersistentVolume` ([`design-crd-model.md`](design-crd-model.md) §5). What the
  CSI driver does with `parameters` is `csi-driver/` and its own designs.
- **Not DHCHAP.** The authentication mechanism is
  [`design-dhchap.md`](../design-dhchap.md). What this document owns is the field
  that turns it on and what that field is called.
- **Not the API group's conventions.** The entity and action split, the enum
  casing, the lock, and the observed generation belong to
  [`design-crd-model.md`](design-crd-model.md) and are cited rather than restated.

---

## 3. StoragePool: API

Declared in `operator/api/v1alpha1/storagepool_types.go`, short name `sp`. The
type is Appendix A. What follows quotes the field an argument turns on and no
more.

### 3.1 Spec

The spec divides into three groups, and the division is the fix for §1's third
finding: what the pool is, what the pool limits, and what the pool's volumes get
by default.

**Identity and placement.**

```go
// ClusterRef names the StorageCluster this pool is carved out of. The cluster
// also owns this object by controller reference, so deleting the cluster deletes
// its pools (§6).
// +kubebuilder:validation:Required
// +k8s:immutable
ClusterRef string `json:"clusterRef"`
```

`spec.allowedNodes` restricts which storage nodes may host the pool's volumes.
Empty means every node in the cluster, which is the usual case.

**The pool's own ceilings, under `spec.limits`.** These are what the pool as a
whole may consume, enforced by the control plane against the pool.

```go
// PoolLimits are the ceilings the pool as a whole is held to. They are the
// pool's budget, not a volume's: a volume's own limits are in
// spec.volumeDefaults, and the two use the same units so a reader can compare
// them.
type PoolLimits struct {
	// Capacity is the total capacity the pool may allocate ("10T", "500G").
	// +optional
	Capacity string `json:"capacity,omitempty"`

	// MaxVolumeSize is the largest single logical volume the pool will create.
	// +optional
	MaxVolumeSize string `json:"maxVolumeSize,omitempty"`

	// IOPS is the pool-wide IOPS ceiling. Zero is unlimited.
	// +kubebuilder:validation:Minimum=0
	// +optional
	IOPS *int32 `json:"iops,omitempty"`
	// ...
}
```

**The defaults its volumes get, under `spec.volumeDefaults`.** These are the values
a class assigned to this pool is expected to carry in its `parameters`, which is how
they reach the CSI driver (§5). The pool states them so that a class can be checked
against the pool it draws from, and §5.1 names the keys the QoS ceilings are written
under, which are renamed by this design and read in both spellings.

**Grouping is the whole change, and it is worth stating why.** The registered
spec has the pool's IOPS ceiling at `spec.qos.iops` and a volume's default IOPS
at `spec.storageClassParameters.qosRwIops`, one an integer and the other a
string, one enforced against the pool and the other against each volume. Both are
called QoS. A reader looking at either has no way to tell which they have, and
the two paths through which they reach the control plane are entirely different.
Naming the groups for what they limit is what makes that legible.

**`enableDHCHAP` replaces `dhchap`**
([`design-crd-model.md`](design-crd-model.md) §7.5). It is off by default, so it
takes the `enable` form, and it moves under `spec.volumeDefaults` because it is a
property of the volumes the pool creates rather than of the pool.

### 3.2 Immutability

| Field            | Optionality | Semantics                                                           |
|------------------|-------------|---------------------------------------------------------------------|
| `clusterRef`     | `Required`  | Immutable from creation. Which cluster a pool is in is its identity |
| `volumeDefaults` | `+optional` | Immutable once set, for the reason below                            |

**`spec.volumeDefaults` is immutable because `StorageClass.parameters` is.** The
Kubernetes API rejects an edit to a class's parameters, so a pool whose defaults
changed would have a class the operator cannot update and a spec that no longer
describes it. Changing a pool's defaults means creating a new pool, which is
stated here and enforced by a marker rather than by a doc comment
([`design-crd-model.md`](design-crd-model.md) §8.3 makes the same point about
the tenancy layer as a whole).

`spec.limits` is mutable. Raising a pool's capacity is an ordinary operation the
control plane supports, and it does not touch the class.

### 3.3 Status

`status.phase` is the operator's own view: `Pending`, `Ready`, or `Deleting`.
`status.uuid` is the backend pool UUID and the field the controller branches on.
`status.status` is the control plane's own lifecycle string, kept in the control
plane's spelling for the reason
[`design-crd-model.md`](design-crd-model.md) §7.8 gives.

`status.storageClassNames` lists the classes assigned to this pool, which is what
makes the assignment discoverable from the pool rather than by selecting labels
cluster-wide (§5). It is a list because a pool may have zero or more, and an empty
list is a pool nothing consumes yet.

`status.defaultStorageClassName` is set only on the default pool, and only to the class
the operator wrote for it (§4.4). It is what distinguishes a default class that was
never created from one that was created and then deleted, which is the difference
between doing the work and declining to redo it.

`status.limits` is what the control plane reports the pool's ceilings actually
are, which is not necessarily what `spec.limits` asked for.
`status.allowedNodes` is `spec.allowedNodes` resolved against the nodes that exist,
which is what the control plane is sent (§4.3). `status.activeOpsRef` is the
operation lock ([`design-crd-model.md`](design-crd-model.md) §3.2), and
`status.observedGeneration` is required by that document's §7.9.

### 3.4 What Admission Validates

**`spec.clusterRef` is resolved at admission, and a `StoragePoolValidator` rejects
a pool naming no cluster.** The reference is immutable (§3.2), so a pool that names
a cluster which does not exist can never be corrected: the only remedy is to delete
it and write it again, which is what the rejection asks for directly. It is the same
line [`design-persistentvolumeops.md`](design-persistentvolumeops.md) §4.3 draws:
a condition fixed for the object's whole life is a rejection, a condition true at one
moment and false at the next is a phase.

**A cluster that exists and is not finished is admitted, and the controller waits.**
`clusterRef` naming a `StorageCluster` with no `status.uuid` yet is a not-yet rather
than a mistake: the cluster is being created, the pool is being applied alongside it,
and a manifest that declares both at once is the ordinary way to bring up a
deployment. The pool is admitted, holds at `Pending`, and reports `ClusterNotReady`
(§9.1) until the cluster has a UUID to create the pool in. Rejecting that at
admission would make declaring a cluster and its pools in one apply fail on
ordering, which nothing about the intent justifies.

So the webhook answers one question, whether the named object exists, and everything
about the cluster's readiness belongs to the reconcile.

**`failurePolicy: Fail`**, for the reason
[`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §5 gives:
the webhook server runs in the operator pod, so while it is unavailable nothing
reconciles a pool anyway, and admitting a pool whose immutable reference cannot be
resolved creates an object that can only be deleted.

### 3.5 Examples

The smallest useful pool:

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: StoragePool
metadata:
  name: tenant-a
  namespace: simplyblock
spec:
  clusterRef: production
  limits:
    capacity: 10T
status:
  phase: Ready
  uuid: 4f2c8a11-6b3d-4e19-9a55-0c7e1d8f2b34
  storageClassNames:
    - fast-xfs
    - archive-ext4
  observedGeneration: 1
```

A pool with both kinds of limit, which is the pairing §3.1 exists to make
readable:

```yaml
spec:
  clusterRef: production
  allowedNodes:
    - production-7f3a9c
    - production-2b81de
  limits:
    capacity: 10T
    maxVolumeSize: 2T
    iops: 200000
    throughput:
      readWrite: 4096
  volumeDefaults:
    iops: 20000
    throughput:
      readWrite: 512
    filesystem: xfs
    enableCompression: true
    enableDHCHAP: true
```

The pool may do 200,000 IOPS and each volume in it defaults to 20,000, so ten
busy volumes saturate it. That relationship is the thing the registered spec
cannot express readably, and it is the question an administrator sizing a pool
actually has.

---

## 4. StoragePool: Controller

`StoragePoolReconciler`, in
`operator/internal/controllers/pool/storagepool_controller.go`.

### 4.1 Reconcile

```
┌──────────────────────────────────────────────────────────────┐
│                  Kubernetes Control Plane                    │
│   ┌──────────────────────────────────────────────────────┐   │
│   │                 StoragePoolReconciler                │   │
│   │  1. Deletion: held while referenced, else backend    │   │
│   │     DELETE, then the finalizer (§6)                  │   │
│   │  2. Ensure the finalizer                             │   │
│   │  3. status.uuid == "" → create the pool              │   │
│   │  4. Default pool: write its class once (§4.4)        │   │
│   │  5. Resolve spec.allowedNodes into status (§4.3)     │   │
│   │  6. Index the classes assigned to this pool (§5)     │   │
│   │  7. Sync status from the streamed pool object        │   │
│   └──────────────────────────────────────────────────────┘   │
│  StoragePool CR   spec.limits   status.uuid   status.phase   │
└──────────────────────────────────────────────────────────────┘
              │ HTTP (webapi client, service-account bearer token)
┌─────────────▼────────────────────────────────────────────────┐
│                  Simplyblock Control Plane                   │
│  POST   /api/v2/clusters/{cluster}/storage-pools/            │
│  GET    /api/v2/clusters/{cluster}/storage-pools/?watch=true │
│  DELETE /api/v2/clusters/{cluster}/storage-pools/{pool}      │
└──────────────────────────────────────────────────────────────┘
```

**Creation is single-shot by the same primitive the cluster uses.** Creating a
backend pool is not idempotent, so the transition that claims it is persisted
with an optimistic-lock patch before the `POST`
([`design-storagecluster.md`](design-storagecluster.md) §4.2). A pool is small
enough that its creation is one step rather than a graph, so it carries no
`status.step`: the claim, the call, and the status write are one pass.

**The controller writes one class and repairs none.** Classes are authored (§5), with
one exception: the default pool gets one written for it, once, and
`status.defaultStorageClassName` records that it was (§4.4). Everything else the
reconcile does with classes is read. It indexes the ones assigned to this pool and
publishes them as `status.storageClassNames`, which is what makes the assignment
visible from the pool and what §6 holds a deletion on. Nothing is recreated, including
the one it wrote.

### 4.2 Steady state

With a UUID present, the controller reads the pool from the control-plane store
rather than from the HTTP API. The pool stream is scoped per cluster
([`design-crd-model.md`](design-crd-model.md) §7.7), so one subscription serves
every pool of one `StorageCluster`. A reconcile that finds the status unchanged
returns without patching.

### 4.3 `spec.allowedNodes` Is Resolved Into Status

A node drained and removed through `StorageNodeOps` leaves its name in every pool
that listed it, and the list is desired state a person wrote.

**The controller resolves the list on every pass and publishes the result as
`status.allowedNodes`.** A name that no longer resolves to a `StorageNode` in the
pool's namespace is dropped from the resolved set, reported once with
`AllowedNodeMissing` (§9.1), and what the control plane is sent is the resolved set
rather than the authored one. A pool whose every allowed node has been removed
resolves to an empty set, which is not the same as an absent list: absent means every
node, and empty after resolution means the pool can place nothing, so the phase holds
and the event says which names failed.

**`spec.allowedNodes` itself is left exactly as authored, and the operator does not
prune it.** Rewriting a user's spec to match the world makes the object stop
recording what was asked for, and it destroys the case the field exists for: a node
removed for maintenance and added back under the same name should return to the pools
that named it, without anybody re-authoring them. The resolved set in status is what a
reader and the control plane both consult, so the stale name is inert rather than
harmful, which is the property the resolution provides and a prune would not.

### 4.4 A Default Pool Is Created With the Cluster

**Creating a `StorageCluster` creates one `StoragePool` in it.** A cluster with no
pool can hold no volumes, so the first pool is not a decision worth making a
prerequisite: the cluster's own creation path creates it, owns it by the same
controller reference every pool has (§6), and names it for the cluster it belongs to.

**It is an ordinary pool in every other respect, deletion included.** Its limits are
the cluster's defaults, it can be edited, and it deletes like any other pool: §6's two
holds are the only thing that stops it, so a default pool with a `PersistentVolume` in
it stays and one with nothing but the operator's own class goes. Nothing recreates it,
because a cluster that has outgrown one pool per tenant is not obliged to keep one.
What the default is for is the case where somebody wants storage from a cluster they
just created and has not yet decided how to divide it.

**One `StorageClass` is created with it, so the cluster is provisionable on arrival.**
A pool nothing can consume is not a usable default, so the cluster's creation path
writes both: the pool, and one class assigned to it by the three labels of §5. The
class needs no ordering against the backend, because its `parameters` name the cluster
UUID and the pool *name* rather than the pool's UUID, and both are known before the
pool exists in the control plane. A claim made in the window before it does fails at
provision time and succeeds afterward.

```yaml
kind: StorageClass
metadata:
  name: simplyblock-production
  labels:
    storage.simplyblock.io/namespace: simplyblock
    storage.simplyblock.io/cluster: production
    storage.simplyblock.io/pool: production-default
    storage.simplyblock.io/managed-by: storagecluster
provisioner: csi.simplyblock.io
parameters:
  cluster_id: 4f2c8a11-6b3d-4e19-9a55-0c7e1d8f2b34
  pool_name: production-default
```

**It carries the pool's `spec.volumeDefaults` and nothing invented.** Whatever the
default pool's defaults are is what the class states, in the keys of §5.1. The operator
does not guess a filesystem, a fabric, or a QoS ceiling that the pool did not already
name, because `StorageClass.parameters` is immutable and a guess would be one nobody
could edit afterward.

**`storage.simplyblock.io/managed-by` is what separates it from an authored class**,
and it is the whole of the difference. A class carrying that label was created by this
operator, so the operator may delete it. A class without it was written by somebody
else, so the operator may not (§6). One label, one rule, and it is the same
principle either way round: the operator cleans up what it created and refuses on
what it did not.

**It is not marked as Kubernetes' default class.** The
`storageclass.kubernetes.io/is-default-class` annotation makes every claim that names
no class bind through this driver, cluster-wide, and a cluster may already have a
default from another provisioner, and two of them is a state Kubernetes resolves
arbitrarily. So "default" here means provided rather than implicit. An administrator
who wants it to be the cluster's default adds one annotation, which is a decision they
can see the consequences of and this operator cannot.

**A deleted default class is not recreated.** `status.defaultStorageClassName` records
the name the operator wrote, and a class that is absent while that field is set was
deleted deliberately. Recreating it would be the operator arguing with an
administrator about a class it does not need to exist for the pool to work, which is
the opposite of §5's reason for making classes authored in the first place. The pool
keeps working, `status.storageClassNames` drops it, and nothing is repaired.

---

## 5. A Class Is Assigned to a Pool

A `StorageClass` and a `StoragePool` are two halves of one contract: the pool is the
capacity and its ceilings, the class is how a claim asks for some. What joins them is
the part of this design that is not a Kubernetes reference and has to behave like one.

**A class is authored, not generated, and a pool may have zero or more of them.**
Nothing about a pool implies a single way to consume it: one pool can back a class
with compression on and another with it off, a class formatted `ext4` and a class
formatted `xfs`, a permissive QoS ceiling for a batch tenant and a tight one for a
latency-sensitive one. A pool with no class at all is a valid state, capacity that
exists with nothing consuming it yet, and is what a freshly created cluster has
(§4.4).

**The assignment is explicit, not a name a controller recomputes.** The class carries
the pool it draws from in its own metadata:

```yaml
kind: StorageClass
metadata:
  name: fast-xfs                      # whatever its author calls it
  labels:
    storage.simplyblock.io/namespace: simplyblock
    storage.simplyblock.io/cluster: production
    storage.simplyblock.io/pool: tenant-a
provisioner: csi.simplyblock.io
parameters:
  cluster_id: 4f2c8a11-6b3d-4e19-9a55-0c7e1d8f2b34
  pool_name: tenant-a
  max_iops: "20000"                   # §5.1
```

The three labels are the assignment and the only thing that answers "which pool does
this class draw from," in either direction: a class names one pool, and the classes
assigned to a pool are a label selector rather than a computation. `parameters`
carries what the driver needs to reach the backend, and the operator validates that
the two agree.

**The class name carries nothing, and that is what makes zero-or-more possible.** A
class named for its pool (`simplyblock-<namespace>-<cluster>-<pool>`) puts the link
in the one field that must be unique, so one pool can have exactly one class, every
controller that needs the link recomputes the string, and asking a class which pool it
draws from means parsing its name. A label carries the link instead: two classes cannot
share a name and can share a label, the answer is a selector rather than a computation,
and a class may be called whatever its author finds useful.

**One class may be the operator's own, and it is labeled so.** The default pool gets
one class written for it at cluster creation (§4.4), carrying
`storage.simplyblock.io/managed-by: storagecluster` beside the three assignment labels.
It is an ordinary class in every respect a claim can observe. The label matters only to
§6, which lets the operator delete a class it created and refuses on one it did not.

**`status.storageClassNames` is the pool's side of it.** The controller lists the
classes carrying its three labels and publishes their names, so the assignment is
readable from the pool without a cluster-wide `kubectl get storageclass` and a mental
join. It is also what §6 holds a deletion on.

**A class may draw from a pool in any namespace, and that is what the scopes are
for.** `StorageClass` is cluster-scoped and `StoragePool` is namespaced, so the team
that writes classes needs no access to the pools, the clusters, or the namespaces they
name, and the teams that consume storage need no access to any of it either, because a
claim names a class and nothing else. That separation is the product: an operations team
declares what storage is available and on what terms, and an application team asks for
some without being able to see, edit, or delete the storage objects behind it.

**What follows from it is that a pool is not a per-namespace quota.** A pool's ceilings
are consumed by every claim that reaches it through any class, so a pool bounds the
capacity and the QoS of the volumes drawn from it and says nothing about which
namespaces those volumes are in. A deployment that wants per-namespace budgets gets
them by writing one pool per namespace and one class per pool, which is a policy the
operations team expresses in the objects rather than one this operator enforces on
their behalf.

**Neither side owns the other, and that is why §6 holds rather than cascades.** A
`StorageClass` is cluster-scoped and a `StoragePool` is namespaced, so the class
cannot be an owned child: Kubernetes garbage-collects a cluster-scoped object whose
owner is namespaced. The operator did not create the class either, so deleting one on
the pool's behalf would be destroying an object it does not own and cannot know the
intent of. What is left is to refuse: a pool with a class assigned to it does not
delete (§6).

**Nothing prevents a class from outliving the pool it names.** `parameters` is an
opaque map the API server does not validate, so a class whose pool is gone stays
admitted and fails at provision time with an error from the control plane. §6 is what
keeps that from happening through the ordinary path, and it is the only thing that
does: a class is not garbage-collected with its pool, because it is not owned by one.

### 5.1 The QoS Parameters Are Renamed, and Both Generations Are Read

The class's `parameters` map is the only place a volume's QoS ceilings are stated,
and the four keys it states them in are the least legible names in this product.

```yaml
qos_rw_iops: "5000"      # total IOPS, read and write together
qos_rw_mbytes: "500"     # total throughput, in MB per second
qos_r_mbytes: "300"      # read throughput
qos_w_mbytes: "200"      # write throughput
```

**Three things are wrong with them, and the first is specific to Kubernetes.**
`rw` means "read and write added together" and reads as `ReadWriteOnce`: in a
`StorageClass`, of all places, `qos_rw_iops` looks like the IOPS ceiling that
applies to `ReadWrite` volumes rather than the sum of two directions. `mbytes` does
not say per second, so `qos_rw_mbytes: "500"` is indistinguishable from a size, and
the same map carries `max_size`, which genuinely is one. And `qos_` names a
category rather than a quantity, while the control plane calls these same four
values `max_rw_iops`, `max_rw_mbytes`, `max_r_mbytes`, and `max_w_mbytes`, so the
prefix is a local invention that makes one value change names on its way through.

**The superseding names say the quantity, the direction, and the unit.**

| Was             | Is                         | Ceiling                                | Written from                               |
|-----------------|----------------------------|----------------------------------------|--------------------------------------------|
| `qos_rw_iops`   | `max_iops`                 | Operations per second, both directions | `spec.volumeDefaults.iops`                 |
| `qos_rw_mbytes` | `max_mbytes_per_sec`       | Throughput, both directions            | `spec.volumeDefaults.throughput.readWrite` |
| `qos_r_mbytes`  | `max_read_mbytes_per_sec`  | Read throughput                        | `spec.volumeDefaults.throughput.read`      |
| `qos_w_mbytes`  | `max_write_mbytes_per_sec` | Write throughput                       | `spec.volumeDefaults.throughput.write`     |

**"bytes" is spelled out on purpose.** A key in this map is lowercase and
underscore-separated, which cannot carry the capital `B` that separates a megabyte
from a megabit. So `mbps`, which is how the per-volume annotations spell this unit
today, is ambiguous in exactly the way a throughput ceiling must not be.
`mbytes_per_sec` costs six characters and cannot be read two ways.

**The names also stop hiding an asymmetry, which is the control plane's and not a
naming artifact.** Throughput has a per-direction ceiling and IOPS does not: the
control plane enforces one combined IOPS limit and nothing else, so there is no
`max_read_iops` to write and the absence is a fact about what can be enforced rather
than a gap in the vocabulary. Under the old spelling `qos_rw_iops` beside
`qos_r_mbytes` gave no hint of that, because both wore the same prefix. `max_iops`
sitting beside `max_read_mbytes_per_sec` says it: one has directions and the other
does not.

**Both generations are read, and not for a deprecation window.** The driver resolves
each ceiling by looking for the new key and falling back to the old one, and the old
one is supported for as long as any class carrying it exists, which is
indefinitely, because §3.2's reason cuts both ways: `StorageClass.parameters` is
immutable in the Kubernetes API, so a class an older operator generated can never be
rewritten into the new vocabulary. The alternative, deleting the class and creating
it again under the same name, is worse than the duplication: it fails every unbound
claim in the window and leaves bound volumes naming a class that briefly does not
exist (§5's second consequence).

**The operator writes the new keys only.** A class it generates carries one
generation, so nothing it creates from here on needs the fallback. Classes it
generated before carry the old generation until their pool is deleted and §6's
finalizer takes them with it, which is the only migration path the immutability
leaves and requires nothing of a user who is not changing pools anyway.

**A class carrying both spellings is a conflict rather than a merge, and it is
reported.** The new key wins, and a `QoSParameterConflict` warning is emitted, because
a class that sets `qos_rw_iops: "5000"` and `max_iops: "1000"` states two different
ceilings and quietly preferring one is how a volume ends up throttled at a number
nobody chose. It is emitted twice, on purpose. The pool controller emits it on the
`StoragePool` when it indexes a class assigned to that pool and finds both spellings
(§5), which is proactive: an administrator learns about it when the class is written
rather than when somebody's claim is provisioned. The driver emits it on the
`PersistentVolumeClaim` it is provisioning, which is reactive but lands where the
person who hit it is looking. Neither is redundant, because the two audiences are
different, and the class itself, cluster-scoped and shared, is a poor place for
either to look.

**The per-volume annotation overrides move with the vocabulary, as its third
generation.** A claim may override any of these four ceilings, and two spellings are
already in the tree: `simplyblock.io/qos-rw-iops` and, before it,
`simplybk/qos-rw-iops`, resolved by a fallback chain the driver already has. The new
generation takes the group's key prefix as well as the new names
([`design-crd-model.md`](design-crd-model.md) §7.3), since a rename is the one moment
when correcting the prefix costs nothing extra.

| Ceiling          | New annotation                                    | Older spellings still read                             |
|------------------|---------------------------------------------------|--------------------------------------------------------|
| IOPS             | `storage.simplyblock.io/max-iops`                 | `simplyblock.io/qos-rw-iops`, `simplybk/qos-rw-iops`   |
| Throughput       | `storage.simplyblock.io/max-mbytes-per-sec`       | `simplyblock.io/qos-rw-mbps`, `simplybk/qos-rw-mbytes` |
| Read throughput  | `storage.simplyblock.io/max-read-mbytes-per-sec`  | `simplyblock.io/qos-r-mbps`, `simplybk/qos-r-mbytes`   |
| Write throughput | `storage.simplyblock.io/max-write-mbytes-per-sec` | `simplyblock.io/qos-w-mbps`, `simplybk/qos-w-mbytes`   |

**One resolver serves both, which is what keeps three generations affordable.** The
driver already reads an annotation through an ordered list of keys and takes the first
that is set. A class parameter is the same problem with a different map, so it is the
same helper rather than a second one, and the order of the list is the whole of the
precedence rule: newest first, and the oldest spelling last.

---

## 6. Deleting a Pool, and Deleting a Cluster That Has One

[`design-crd-model.md`](design-crd-model.md) §9.3 leaves this open: making
`StorageCluster` own `StoragePool` by controller reference means deleting a cluster
deletes its pools, which could leave claims pointing at storage that is gone. It
calls that defensible and says it should not be established silently. This is the
decision.

**The owner reference is established, and the pool's finalizer is what makes it
safe.** The finalizer is `storage.simplyblock.io/storagepool-finalizer`. A pool
refuses to finish deleting while anything Kubernetes knows about still refers to it,
and there are two such things.

| Deleting           | With                                      | Result                                                                 |
|--------------------|-------------------------------------------|------------------------------------------------------------------------|
| A `StoragePool`    | An authored `StorageClass` assigned to it | Held. `StorageClassStillAssigned`, requeued, nothing deleted           |
| A `StoragePool`    | Only the operator's own default class     | The class is deleted, then the backend pool, then the finalizer clears |
| A `StoragePool`    | A `PersistentVolume` in it                | Held. `VolumesStillBound`, requeued, nothing deleted                   |
| A `StoragePool`    | Neither                                   | The backend pool is deleted and the finalizer clears                   |
| A `StorageCluster` | Pools nothing refers to                   | Cascades. Each pool deletes as above                                   |
| A `StorageCluster` | Pools a class or a volume refers to       | Held at the pool, so the cluster's own deletion is held behind it      |

**An authored class holds the deletion because the operator cannot clean it up.** The
class is cluster-scoped and somebody else wrote it (§5), so the operator neither owns
it nor knows why it exists, and deleting somebody's provisioning contract to let a pool
go is not a trade this operator gets to make. Refusing is the honest alternative: the
class is one object, whoever wrote it can delete it, and the pool then goes.

**The operator's own class does not hold, because deleting it is cleanup rather than a
decision.** A class carrying `storage.simplyblock.io/managed-by` was written by this
operator (§4.4), so the finalizer removes it and continues. The rule is symmetric and
worth stating as one: the operator deletes what it created and refuses on what it did
not, which is why one label is enough to tell the two cases apart. A default class
somebody has since edited is still the operator's by that label, and §12 Q1 is whether
that is the right reading.

**A bound volume holds it because the data is real.** This is the dangerous version
of the owner reference, a cluster deletion silently destroying tenant data, and it
does not happen: garbage collection deletes the object, the finalizer keeps it in
`Terminating`, and the cluster's own finalizer is behind it. The result is a
`kubectl delete storagecluster` that visibly does not finish, with an event naming
the pool and what is still in it.

**Held is correct, and unblocking is the administrator's.** Deleting the classes and
the claims releases the pool, which releases the cluster. That is a sequence somebody
has to perform deliberately, which is the property that makes the owner reference of
[`design-crd-model.md`](design-crd-model.md) §9.3 safe to establish.

**Only objects Kubernetes knows about hold a deletion.** The counts are a list of
`StorageClass` objects carrying the pool's labels and a list of `PersistentVolume`
objects in the pool, rather than a question to the control plane about how many
logical volumes it has. A control-plane volume with no `PersistentVolume` is unmanaged
([`design-storagenode.md`](design-storagenode.md) §8.1) and does not hold, and that
is the rule rather than a compromise: a hold is a promise to a person that something
they can see still needs them, and an object they cannot see, cannot list, and cannot
delete through Kubernetes cannot be that. The consequence is stated plainly. A pool
holding a volume provisioned out of band, or one whose `PersistentVolume` was
force-deleted, will delete, and the answer to it is that the backend refuses to
delete a pool with volumes in it, so the failure surfaces as a
`PoolDeletionFailed` from the control plane rather than as lost data.

---

## 7. StoragePoolOps

Declared in `operator/api/v1alpha1/storagepoolops_types.go`, short name `spops`,
and reconciled by `StoragePoolOpsReconciler` in
`operator/internal/controllers/pool/storagepoolops_controller.go`, beside the
entity's own. The type is Appendix B.

It takes `StoragePool.status.activeOpsRef` and releases it on every terminal path,
including through `storage.simplyblock.io/storagepoolops-finalizer`, which is the
shape [`design-crd-model.md`](design-crd-model.md) §3.2 gives every `Ops` kind.

**None of this kind's actions exists, and the kind is specified as a shape rather
than as work to do.** A pool has fewer operations than a node because most of what
changes about a pool is desired state: raising its capacity is an edit, restricting
its nodes is an edit, and §4.3 resolves the node list on every pass. What is left is
one candidate, kept here as a worked example of what a pool-level operation would
look like if one were needed.

```go
// +kubebuilder:validation:Enum=Rebalance
type StoragePoolOpsAction string
```

| Action      | Steps                      | Status        | What it is for                                                     |
|-------------|----------------------------|---------------|--------------------------------------------------------------------|
| `Rebalance` | `Validating` → `Migrating` | *Provisional* | Redistribute the pool's volumes after `spec.allowedNodes` narrowed |

**`Rebalance` is provisional and marked as such deliberately.** It is not implemented
and nothing depends on it. It is retained because it is the one pool-level operation
with a real motivation: `spec.allowedNodes` is mutable, narrowing it stops new volumes
landing on the removed nodes and does nothing to the volumes already there, and that
gap between desired and actual state is exactly what an `Ops` kind closes. Keeping the
example is cheaper than rediscovering the motivation later.

**If it is built, it is a fan-out and not a backend call.** The control plane has no
pool-granularity rebalance, and it does not need one: moving a volume is
`PersistentVolumeOps` with `action: Migrate`, so a pool rebalance is a `Validating`
step that works out which volumes sit on nodes no longer allowed and a `Migrating`
step that creates one `PersistentVolumeOps` per volume and waits for them, which is
the same fan-out a node drain performs
([`design-storagenode.md`](design-storagenode.md) §8.2). That makes it an operation
built entirely out of parts this group already has, and it is why the action stays on
this kind rather than moving to the volume layer: the *decision* is the pool's, and
only the execution is per-volume.

**There is no `Suspend` and no `Resume`.** The control plane has no such operation,
and neither does this design. Stopping a pool from accepting new volumes is done in
Kubernetes, by deleting or restricting the classes assigned to it (§5), which is now
possible without touching the pool, because a class is one of many and deleting one
takes nothing else with it.

**There is no `Resize`.** Capacity is `spec.limits.capacity` and changing it is an
edit the control plane applies, which is desired state working as intended
([`design-crd-model.md`](design-crd-model.md) §3).

---

## 8. Backend API Requirements

| Method   | Endpoint                                               | Notes                                                                |
|----------|--------------------------------------------------------|----------------------------------------------------------------------|
| `POST`   | `/api/v2/clusters/{cluster}/storage-pools/`            | Not idempotent, which is why §4.1 claims in Kubernetes first         |
| `GET`    | `/api/v2/clusters/{cluster}/storage-pools/?watch=true` | The pool stream every status write reads. Scoped per cluster         |
| `PUT`    | `/api/v2/clusters/{cluster}/storage-pools/{pool}`      | Applies a changed `spec.limits`                                      |
| `DELETE` | `/api/v2/clusters/{cluster}/storage-pools/{pool}`      | 404 is success, since a pool already gone is a pool deleted          |
| `GET`    | `/api/v2/clusters/{cluster}/storage-pools/{pool}/host` | The host enforcing the pool's QoS, published as `status.limits.host` |

**The `?watch=true` row is a Server-Sent-Events subscription rather than a request
that returns**, and it arrives with the control plane's SSE work rather than with
this design ([`design-crd-model.md`](design-crd-model.md) §7.7). Until that lands it
is the one external dependency this design cannot satisfy on its own.

**This layer needs no endpoint the control plane does not have.** Nothing here
suspends a pool, because a pool stops accepting volumes when the classes assigned to
it are restricted or removed (§7), which is a Kubernetes operation. Nothing here
rebalances one either: moving a volume is `PersistentVolumeOps`, so a pool rebalance
uses the volume endpoints
[`design-persistentvolumeops.md`](design-persistentvolumeops.md) §7 lists and adds
none of its own.

**`DELETE` is the one whose failure mode matters.** §6 holds a pool's deletion on
`StorageClass` and `PersistentVolume` objects, which is everything Kubernetes can see,
and the backend's own refusal to delete a pool that still has logical volumes is what
covers what it cannot. A `DELETE` that fails for that reason is reported as
`PoolDeletionFailed` and retried rather than treated as success. Only a 404 is
success, since a pool already gone is a pool deleted.

---

## 9. Observability

The pool controller emits no events and exports no metric. Both tables are new.

### 9.1 Kubernetes events

Events land on the `StoragePool`, which is what an administrator managing tenancy
has open, and on the `StoragePoolOps` for an operation.

| Event                                                    | Type      | Reason                      | On               |
|----------------------------------------------------------|-----------|-----------------------------|------------------|
| The pool was created in the control plane                | `Normal`  | `PoolCreated`               | `StoragePool`    |
| Creation was rejected by the control plane               | `Warning` | `PoolCreationFailed`        | `StoragePool`    |
| The cluster is not ready, so creation is held            | `Normal`  | `ClusterNotReady`           | `StoragePool`    |
| A `StorageClass` was assigned to this pool               | `Normal`  | `StorageClassAssigned`      | `StoragePool`    |
| The default class was created with the default pool      | `Normal`  | `StorageClassCreated`       | `StoragePool`    |
| A class assigned to this pool sets both QoS spellings    | `Warning` | `QoSParameterConflict`      | `StoragePool`    |
| An entry in `spec.allowedNodes` resolves to no node      | `Warning` | `AllowedNodeMissing`        | `StoragePool`    |
| Deletion is held because a class is still assigned       | `Warning` | `StorageClassStillAssigned` | `StoragePool`    |
| Deletion is held because volumes are still bound         | `Warning` | `VolumesStillBound`         | `StoragePool`    |
| The control plane refused to delete the pool             | `Warning` | `PoolDeletionFailed`        | `StoragePool`    |
| The pool's capacity limit has been reached               | `Warning` | `CapacityExhausted`         | `StoragePool`    |
| The operation is waiting for another to release the lock | `Normal`  | `OperationQueued`           | `StoragePoolOps` |
| The operation acquired the lock and started              | `Normal`  | `OperationStarted`          | `StoragePoolOps` |
| The operation finished successfully                      | `Normal`  | `OperationSucceeded`        | `StoragePoolOps` |
| The operation failed                                     | `Warning` | `OperationFailed`           | `StoragePoolOps` |
| The operation was aborted and its unwind finished        | `Normal`  | `OperationAborted`          | `StoragePoolOps` |
| A step's deadline expired                                | `Warning` | `StepDeadlineExceeded`      | `StoragePoolOps` |

**`VolumesStillBound` is the load-bearing one**, because it is the only thing
that explains a `kubectl delete storagecluster` that does not finish (§6). Without
it, the correct behavior is indistinguishable from a stuck finalizer, which is
the failure mode administrators reach for `--force` over.

**`StorageClassStillAssigned` and `VolumesStillBound` are the two that explain a
`kubectl delete` that does not finish**, and they are separate reasons rather than one
because the remedies are different: a class is deleted by whoever wrote it, and a
bound volume is released by deleting a claim. Reporting "still referenced" without
saying which kind of reference would leave an administrator guessing between the two.

**`AllowedNodeMissing` fires once per unresolved name rather than every pass.** §4.3
leaves `spec.allowedNodes` as authored, so a removed node's name stays there and would
otherwise produce an event on every reconcile forever. The event is what tells somebody
the name is inert, and repeating it would tell them nothing new.

**`QoSParameterConflict` is the proactive half of §5.1's conflict.** The driver emits
the same reason on the claim it is provisioning, and this one fires when the class is
first indexed, which is usually well before anybody's claim reaches it.

### 9.2 Prometheus metrics

| Metric                                               | Labels                                | Description                                                                    |
|------------------------------------------------------|---------------------------------------|--------------------------------------------------------------------------------|
| `simplyblock_storagepool_capacity_bytes`             | `cluster`, `pool`                     | Gauge of the pool's capacity limit                                             |
| `simplyblock_storagepool_used_bytes`                 | `cluster`, `pool`                     | Gauge of what it has allocated, so the ratio is the tenancy alert              |
| `simplyblock_storagepool_volumes_count`              | `cluster`, `pool`                     | Gauge of logical volumes in the pool                                           |
| `simplyblock_storagepool_bound_volumes_count`        | `cluster`, `pool`                     | Gauge of `PersistentVolume` objects bound to its class (§6)                    |
| `simplyblock_storagepool_phase_state`                | `cluster`, `pool`, `phase`            | Gauge, 1 for the current phase, so a pool stuck in `Deleting` is alertable     |
| `simplyblock_storagepool_storageclasses_count`       | `cluster`, `pool`                     | Gauge of the classes assigned to the pool, which is zero for an unconsumed one |
| `simplyblock_storagepool_operations_total`           | `cluster`, `pool`, `action`, `result` | Operations reaching a terminal phase                                           |
| `simplyblock_storagepool_operation_duration_seconds` | `cluster`, `pool`, `action`           | Histogram of operation durations                                               |

**`used_bytes` against `capacity_bytes` is the one a tenant operator watches**,
and it is the only metric in this group that answers a capacity-planning question
rather than a health one.

**`storageclass_missing` and the gap between `volumes` and `bound_volumes` are
the two integrity signals.** The first says the join's forward half is broken. The
second says the control plane holds volumes Kubernetes does not account for,
which is the unmanaged-volume condition that blocks a node drain
([`design-storagenode.md`](design-storagenode.md) §8.1) and is better noticed
before somebody tries to drain.

---

## 10. Testing Strategy

Scenarios live in
[`tests/test-plan-storagepool.md`](../../tests/test-plan-storagepool.md) and only
there.

Unit tests carry most of the weight: the class's name is a pure function, the
creation claim's 409 path is control flow over a fake client, and the
bound-volume count in §6 is a `List` with a label selector.

The risk that unit tests do not reach is entirely in §5 and §6, because both are
about what happens between objects that are not linked by a reference. Proving
that deleting a cluster holds behind a pool that holds behind a bound claim needs
a real API server and real garbage collection, and proving that an existing
volume keeps serving I/O after its class is gone needs a real data path.

---

## 11. Migration from the Registered API

| Registered                                                                             | This design                                                                                    | Cost                                                                                                    |
|----------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------|
| `spec.clusterName`                                                                     | `spec.clusterRef` (§3.1)                                                                       | Spec rename, matching every other reference in the group                                                |
| `spec.action`, unused                                                                  | `StoragePoolOps` (§7), whose one action is provisional                                         | Spec removal. It is the antipattern `design-crd-model.md` §3 rejects, and it does nothing               |
| `spec.status`, unused                                                                  | Removed (§1)                                                                                   | Spec removal. A spec field named `status` beside a `status.status`                                      |
| `spec.capacityLimit`, `spec.logicalVolumeMaxSize`                                      | `spec.limits.capacity`, `.maxVolumeSize` (§3.1)                                                | Spec regrouping                                                                                         |
| `spec.qos.*`                                                                           | `spec.limits.{iops,throughput}` (§3.1)                                                         | Spec regrouping, so that the pool's ceilings are named for what they limit                              |
| `spec.storageClassParameters.*`                                                        | `spec.volumeDefaults.*` (§3.1)                                                                 | Spec regrouping, and the units align with `spec.limits`                                                 |
| `spec.dhchap`                                                                          | `spec.volumeDefaults.enableDHCHAP` (§3.1)                                                      | Spec rename, owned by `design-crd-model.md` §9.6                                                        |
| `encryption`, `replicate` in the class parameters                                      | `enableEncryption`, `enableReplication`                                                        | Spec renames, from the same list                                                                        |
| `qos_rw_iops`, `qos_rw_mbytes`, `qos_r_mbytes`, `qos_w_mbytes` in the class parameters | `max_iops`, `max_mbytes_per_sec`, `max_read_mbytes_per_sec`, `max_write_mbytes_per_sec` (§5.1) | Parameter renames. The old keys are read indefinitely, because a class's parameters cannot be rewritten |
| `simplyblock.io/qos-*` overrides on a claim                                            | `storage.simplyblock.io/max-*` (§5.1)                                                          | Annotation renames, taking the group's key prefix. Both older spellings stay in the resolver            |
| Untyped `status`, no phase                                                             | `StoragePoolPhase` (§3.3)                                                                      | Additive                                                                                                |
| No `status.storageClassName`                                                           | `status.storageClassNames`, a list (§3.3)                                                      | Additive, and it makes the assignment discoverable from the pool                                        |
| One class per pool, named by convention                                                | Zero or more, assigned by label (§5)                                                           | Behavioral. The operator stops generating classes, so a class is authored and the pool never owns one   |
| Deletion does not check assigned classes                                               | Held with `StorageClassStillAssigned` (§6)                                                     | Behavioral, and it is what replaces deleting a class the operator does not own                          |
| No `clusterRef` validation                                                             | `StoragePoolValidator` (§3.4)                                                                  | New. An immutable reference to a cluster that does not exist is refused rather than parked in `Pending` |
| No default pool                                                                        | One created with the cluster (§4.4)                                                            | Additive. A cluster holds capacity without anybody authoring a pool first                               |
| No default class                                                                       | One written with the default pool, labeled `managed-by` (§4.4)                                 | Additive. A fresh cluster can provision, and the label is what lets §6 delete it                        |
| `spec.allowedNodes` names go stale silently                                            | Resolved into `status.allowedNodes` (§4.3)                                                     | Behavioral, and the authored list is left as written                                                    |
| No `observedGeneration`                                                                | Present (§3.3)                                                                                 | Required by `design-crd-model.md` §7.9                                                                  |
| No `shortName`                                                                         | `sp`                                                                                           | Additive                                                                                                |
| `spec.clusterName` as a string, no owner reference                                     | Owned by `StorageCluster` (§6)                                                                 | An owner reference is established, and §6 is the decision `design-crd-model.md` §9.3 wanted             |
| Deletion does not check bound volumes                                                  | Held with `VolumesStillBound` (§6)                                                             | Behavioral, and it is what makes the owner reference safe                                               |
| Polling every backend read                                                             | The pool stream (§4.2)                                                                         | Depends on `design-sse-push-notifications.md`, on the `sse` branch                                      |
| No event                                                                               | The reasons in §9.1                                                                            | New                                                                                                     |
| No metric                                                                              | The eight metrics of §9.2                                                                      | New infrastructure                                                                                      |

**Removing two published fields is affordable now and not later.** `spec.action`
and `spec.status` are in a shipped CRD, so removing them is breaking in the
narrow sense that an object setting them stops being accepted. Both are marked
`FIXME: Unused for now` and neither has ever had an effect, so nothing that set
them got any behavior from doing so. The group is at `v1alpha1`, which is the
version where that argument is available.

**The two regroupings are the breaking rows to sequence carefully.** A renamed
spec field is silently ignored on an object that still sets the old name, so a
pool whose `capacityLimit` moved to `limits.capacity` loses its limit rather than
failing to apply. `spec.volumeDefaults` is worse, because it is immutable once
set: a pool that applies with the old spelling gets an empty
`volumeDefaults`, which then cannot be corrected without deleting the pool.

---

## 12. Open Questions

**Q1: Whether an edited default class is still the operator's to delete.** §6 lets the
finalizer delete a class carrying `storage.simplyblock.io/managed-by`, and nothing stops
an administrator from editing that class into something they depend on while the label
stays. Deleting the pool would then remove a class somebody had adopted. Four
candidates, and the first makes the question disappear rather than answering it. A
validating webhook can refuse any edit to a class the operator manages, which makes it
read-only to everybody and leaves adoption inexpressible, so an administrator who wants
a variant copies it into a class of their own, which is one `kubectl get -o yaml` away and
is what they would end up with regardless. Otherwise: drop the label on any edit the
operator did not make, which needs a watch and a comparison; treat any edit as adoption
and refuse to delete afterward, which turns a convenience into something that can block
a pool's deletion; or leave it, on the grounds that a class assigned to a pool being
deleted was going to stop working anyway, which is what §6 does today.

---

## Appendix A: `storagepool_types.go`

The type as it is to be written. Everything the sections above show in Go is an
excerpt of this appendix, and this is the only place any type appears whole.

```go
// StoragePoolPhase is where the operator has got to with this pool.
// +kubebuilder:validation:Enum=Pending;Ready;Deleting
type StoragePoolPhase string

const (
	StoragePoolPhasePending  StoragePoolPhase = "Pending"
	StoragePoolPhaseReady    StoragePoolPhase = "Ready"
	StoragePoolPhaseDeleting StoragePoolPhase = "Deleting"
)

// ThroughputLimits are throughput ceilings in MiB/s. Zero is unlimited, which is
// the control plane's own convention for these values.
// ThroughputLimits are throughput ceilings in megabytes per second. The unit is
// the field's, not the value's, which is why the class keys these reach spell it
// out: a parameter map has no type to carry it (§5.1).
type ThroughputLimits struct {
	// Read is the read-only ceiling, written as max_read_mbytes_per_sec.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Read *int32 `json:"read,omitempty"`
	// Write is the write-only ceiling, written as max_write_mbytes_per_sec.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Write *int32 `json:"write,omitempty"`
	// ReadWrite is the ceiling on both directions together, written as
	// max_mbytes_per_sec. It is not an access mode, which is the confusion the
	// class key qos_rw_mbytes invited (§5.1).
	// +kubebuilder:validation:Minimum=0
	// +optional
	ReadWrite *int32 `json:"readWrite,omitempty"`
}

// PoolLimits are the ceilings the pool as a whole is held to. They are the
// pool's budget rather than a volume's: a volume's own defaults are in
// StoragePoolSpec.VolumeDefaults, and the two use the same units so that a
// reader can compare them.
type PoolLimits struct {
	// Capacity is the total capacity the pool may allocate ("10T", "500G").
	// +optional
	Capacity string `json:"capacity,omitempty"`

	// MaxVolumeSize is the largest single logical volume the pool will create.
	// +optional
	MaxVolumeSize string `json:"maxVolumeSize,omitempty"`

	// IOPS is the pool-wide IOPS ceiling. Zero is unlimited.
	// +kubebuilder:validation:Minimum=0
	// +optional
	IOPS *int32 `json:"iops,omitempty"`

	// Throughput is the pool-wide throughput ceiling.
	// +optional
	Throughput *ThroughputLimits `json:"throughput,omitempty"`
}

// VolumeDefaults are the defaults every volume in the pool is created with. They
// are written into the generated StorageClass's parameters and reach the CSI
// driver from there, which is why the whole block is immutable once set:
// StorageClass.parameters is immutable in the Kubernetes API.
type VolumeDefaults struct {
	// IOPS is each volume's ceiling on operations per second, both directions
	// together. Zero is unlimited. It is written into the class as max_iops
	// (§5.1).
	// +kubebuilder:validation:Minimum=0
	// +optional
	IOPS *int32 `json:"iops,omitempty"`

	// Throughput is each volume's throughput ceiling, in megabytes per second.
	// +optional
	Throughput *ThroughputLimits `json:"throughput,omitempty"`

	// Filesystem is what a volume is formatted with. The values are the kernel's
	// names for filesystems, which is the exception design-crd-model.md §7.8
	// carries for a word this group did not invent.
	// +kubebuilder:validation:Enum=ext4;xfs
	// +kubebuilder:default=xfs
	// +optional
	Filesystem string `json:"filesystem,omitempty"`

	// EnableCompression compresses logical volumes.
	// +optional
	EnableCompression *bool `json:"enableCompression,omitempty"`

	// EnableEncryption encrypts logical volumes, using the key store the cluster
	// names in spec.kms.
	// +optional
	EnableEncryption *bool `json:"enableEncryption,omitempty"`

	// EnableReplication replicates logical volumes.
	// +optional
	EnableReplication *bool `json:"enableReplication,omitempty"`

	// EnableDHCHAP authenticates NVMe-oF connections to this pool's volumes. See
	// design-dhchap.md for the mechanism.
	// +optional
	EnableDHCHAP *bool `json:"enableDHCHAP,omitempty"`

	// PriorityClass is the logical-volume priority class the control plane
	// places with.
	// +optional
	PriorityClass string `json:"priorityClass,omitempty"`

	// Fabric is the storage fabric a volume is served over, defaulting to the
	// cluster's.
	// +optional
	Fabric string `json:"fabric,omitempty"`

	// MaxNamespacesPerSubsystem caps how many namespaces share one NVMe-oF
	// subsystem.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxNamespacesPerSubsystem *int32 `json:"maxNamespacesPerSubsystem,omitempty"`

	// Tune2fsReservedBlocks is the reserved-block percentage passed to tune2fs
	// on an ext4 volume. Empty means the filesystem's own default, which is not
	// the same as "0".
	// +optional
	Tune2fsReservedBlocks string `json:"tune2fsReservedBlocks,omitempty"`
}

// StoragePoolSpec is the desired state of one tenancy unit within a cluster.
type StoragePoolSpec struct {
	// ClusterRef names the StorageCluster this pool is carved out of. The cluster
	// owns this object by controller reference, so deleting the cluster deletes
	// its pools, held behind each pool's own finalizer while volumes are bound.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// AllowedNodes restricts which storage nodes may host this pool's volumes.
	// Empty means every node in the cluster. Narrowing it stops new volumes
	// landing on the removed nodes and leaves the existing ones where they are.
	// +optional
	// +listType=set
	AllowedNodes []string `json:"allowedNodes,omitempty"`

	// Limits are the ceilings the pool as a whole is held to. Mutable: raising a
	// pool's capacity is an ordinary operation and it does not touch the
	// generated StorageClass.
	// +optional
	Limits *PoolLimits `json:"limits,omitempty"`

	// VolumeDefaults are what every volume in the pool is created with.
	// Immutable once set, because StorageClass.parameters is immutable in the
	// Kubernetes API: a pool whose defaults changed would have a class the
	// operator cannot update. Changing them means creating a new pool.
	// +optional
	// +k8s:immutable
	VolumeDefaults *VolumeDefaults `json:"volumeDefaults,omitempty"`
}

// PoolLimitsStatus is what the control plane reports the pool's ceilings
// actually are, which is not necessarily what Limits asked for.
type PoolLimitsStatus struct {
	// Host is the backend host enforcing the pool's QoS.
	// +optional
	Host string `json:"host,omitempty"`

	// +optional
	IOPS *int32 `json:"iops,omitempty"`

	// +optional
	Throughput *ThroughputLimits `json:"throughput,omitempty"`
}

// StoragePoolStatus is the observed state of one pool.
type StoragePoolStatus struct {
	// Phase is the operator's own view of this pool.
	// +optional
	Phase StoragePoolPhase `json:"phase,omitempty"`

	// UUID is the backend pool UUID. Empty means the pool has not been created.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// Status is the lifecycle the control plane reports, in the control plane's
	// own spelling, which is why it carries no Enum here.
	// +optional
	Status string `json:"status,omitempty"`

	// StorageClassNames are the classes assigned to this pool, found by the three
	// storage.simplyblock.io labels a class carries (§5). Empty means no class
	// draws from this pool yet, which is a valid state and what a freshly created
	// cluster's default pool has. Publishing it is what makes the assignment
	// readable from the pool, and what §6 holds a deletion on.
	// +optional
	StorageClassNames []string `json:"storageClassNames,omitempty"`

	// DefaultStorageClassName is the class the operator wrote for the default pool
	// (§4.4), set on that pool only. It records that the class was created, so a
	// class missing while this is set was deleted deliberately and is not written
	// again.
	// +optional
	DefaultStorageClassName string `json:"defaultStorageClassName,omitempty"`

	// Limits is what the control plane reports the ceilings to be.
	// +optional
	Limits *PoolLimitsStatus `json:"limits,omitempty"`

	// AllowedNodes is the resolved node list.
	// +optional
	// +listType=set
	AllowedNodes []string `json:"allowedNodes,omitempty"`

	// ActiveOpsRef names the StoragePoolOps currently allowed to act on this
	// pool. Empty when none is running.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the pool moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sp
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Capacity",type=string,JSONPath=".spec.limits.capacity"
// +kubebuilder:printcolumn:name="Classes",type=string,JSONPath=".status.storageClassNames"
// +kubebuilder:printcolumn:name="UUID",type=string,JSONPath=".status.uuid",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StoragePool carves a StorageCluster into a unit with its own capacity limit and
// QoS ceilings, and it is what a StorageClass is assigned to. It is therefore the
// join between the storage administrator's world and the application developer's, a
// join held together by three labels, an opaque parameter map, and a finalizer
// rather than by the API.
type StoragePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StoragePoolSpec   `json:"spec,omitempty"`
	Status StoragePoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StoragePoolList contains a list of StoragePool.
type StoragePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StoragePool `json:"items"`
}
```

---

## Appendix B: `storagepoolops_types.go`

```go
// StoragePoolOpsAction is the operation a StoragePoolOps performs. The kind holds
// one action and that action is provisional: nothing implements it and nothing
// depends on it (§7). It is declared because the motivation is real and cheaper to
// keep than to rediscover, not because it is work in progress.
// +kubebuilder:validation:Enum=Rebalance
type StoragePoolOpsAction string

const (
	// Rebalance moves the pool's volumes off nodes spec.allowedNodes no longer
	// lists. It is a fan-out of PersistentVolumeOps rather than a backend call:
	// the control plane has no pool-granularity rebalance and does not need one,
	// because moving a volume is already an operation this group has.
	StoragePoolOpsActionRebalance StoragePoolOpsAction = "Rebalance"
)

// StoragePoolOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StoragePoolOpsPhase string

const (
	StoragePoolOpsPhasePending   StoragePoolOpsPhase = "Pending"
	StoragePoolOpsPhaseRunning   StoragePoolOpsPhase = "Running"
	StoragePoolOpsPhaseSucceeded StoragePoolOpsPhase = "Succeeded"
	StoragePoolOpsPhaseFailed    StoragePoolOpsPhase = "Failed"
	StoragePoolOpsPhaseAborted   StoragePoolOpsPhase = "Aborted"
)

// StoragePoolOpsStep is one step of a running pool operation.
// +kubebuilder:validation:Enum=Validating;Migrating
type StoragePoolOpsStep string

const (
	// Validating works out which of the pool's volumes sit on nodes that are no
	// longer allowed, and writes the list before moving anything.
	StoragePoolOpsStepValidating StoragePoolOpsStep = "Validating"
	// Migrating creates one PersistentVolumeOps per volume in that list and waits
	// for each to reach a terminal phase, which is the same fan-out a node drain
	// performs.
	StoragePoolOpsStepMigrating StoragePoolOpsStep = "Migrating"
)

// StoragePoolOpsSpec is one operation to perform against one StoragePool.
type StoragePoolOpsSpec struct {
	// PoolRef names the StoragePool this operation acts on. The operation never
	// owns its target, because deleting the record of an operation must not
	// delete the pool it operated on.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	PoolRef string `json:"poolRef"`

	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action StoragePoolOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind.
	// +optional
	Abort bool `json:"abort,omitempty"`
}

// StoragePoolOpsStatus is the observed state of one pool operation.
type StoragePoolOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase StoragePoolOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine. It is
	// persisted before the side effect that step performs.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','Migrating']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

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
// +kubebuilder:resource:scope=Namespaced,shortName=spops
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=".spec.poolRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StoragePoolOps is a single operation performed against one StoragePool. It
// replaces the unused spec.action field on the pool, which is the construction
// design-crd-model.md §3 rejects because it allows one operation at a time,
// keeps no history, and cannot distinguish in-progress from done.
type StoragePoolOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StoragePoolOpsSpec   `json:"spec,omitempty"`
	Status StoragePoolOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StoragePoolOpsList contains a list of StoragePoolOps.
type StoragePoolOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StoragePoolOps `json:"items"`
}
```
