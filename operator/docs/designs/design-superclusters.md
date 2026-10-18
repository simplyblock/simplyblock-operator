# Design Document: Superclusters

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-10-18  
**Target Release:** simplyblock 26.4, between beta2 and beta3  
**Test Plan:** [`tests/test-plan-superclusters.md`](../tests/test-plan-superclusters.md)

---

## Phasing Overview

| Phase                   | Status  | Scope                                                                                              | Blocked by       |
|-------------------------|---------|----------------------------------------------------------------------------------------------------|------------------|
| **Phase 1** (§4–§6)     | Planned | The aggregate on `StorageCluster`, per-member workload names, and expansion from a deployment      | —                |
| **Phase 2** (§7)        | Planned | Pools fanned out to members, the supercluster StorageClass, and member selection in the CSI driver | —                |
| **Phase 3** (§8.1–§8.3) | Planned | `PersistentVolumeOps` action `Relocate`: one volume moved to another member                        | P0-3, P0-4, P0-5 |
| **Phase 4** (§8.4)      | Planned | `StorageClusterOps` action `Evacuate`: a member emptied and removed from its aggregate             | Phase 3          |

Each phase ships on its own. Phase 1 produces a supercluster that reports one
status and lets an administrator name the members directly, which is already
useful for a fleet whose hardware cannot share one cluster. Phase 2 is what makes
it behave as one cluster to a workload. Phase 3 and Phase 4 add mobility between
members and depend on control-plane behavior that is not yet established.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                       | Kind                    | Blocks                        | Status               |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|-------------------------------|----------------------|
| P0-1 | One control plane hosts several clusters in one FoundationDB, each addressed as `/api/v2/clusters/{cluster_id}/…` (§3)                             | Control plane (`sbcli`) | Phase 1                       | Available            |
| P0-2 | Per-cluster capacity read, `GET /api/v2/clusters/{cluster_id}/capacity` (§7.2, §10)                                                                | Control plane (`sbcli`) | Phase 2                       | Available            |
| P0-3 | After a cross-cluster move commits with `delete_source`, the original handle `{source}:{pool}:{lvol}` still resolves for every CSI call (§8.3)     | Control plane (`sbcli`) | Phase 3                       | Unknown              |
| P0-4 | A defined way for a consuming host to attach the target cluster's paths before the cutover flips ANA states (§8.2)                                 | Control plane (`sbcli`) | Phase 3                       | Unknown              |
| P0-5 | Cross-cluster move of an encrypted volume carries its data encryption key, which the KMS stores under `cluster/{cluster_id}/lvol/{lvol_id}` (§9.4) | Control plane (`sbcli`) | Phase 3 for encrypted volumes | Unknown              |
| P0-6 | Data-plane reachability between the members' storage networks, since the move attaches the target node's transfer hub over NVMe-oF (§8.2)          | Node OS                 | Phase 3                       | Deployment           |
| P0-7 | The limit of 32 storage nodes per cluster, which the product states and neither sbcli nor this operator enforces today (§5.2)                      | Control plane (`sbcli`) | Phase 1                       | Stated, not enforced |

Without P0-1 there is no feature. Without P0-2 member selection has nothing to
rank on and Phase 2 cannot ship. P0-3 is the one that decides Phase 3: a
`PersistentVolume`'s `spec.csi.volumeHandle` is immutable and names the member
the volume was created in, so a move that leaves that handle dangling turns every
later expand, snapshot, and delete into a failure, and Phase 3 does not ship until
the handle is known to survive. Without P0-4 a move interrupts I/O on the
consuming host. Without P0-5 an encrypted volume is refused as a move candidate,
which is a reported restriction rather than a blocker. P0-6 is a property of the
site, checked by the move's validation step rather than assumed.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API: A Supercluster Is a StorageCluster with an Aggregate](#4-api-a-supercluster-is-a-storagecluster-with-an-aggregate)
5. [Expansion from a ClusterDeploymentConfig](#5-expansion-from-a-clusterdeploymentconfig)
6. [Members Share a Namespace](#6-members-share-a-namespace)
7. [Volume Placement](#7-volume-placement)
8. [Moving Volumes Between Members](#8-moving-volumes-between-members)
9. [Feature Treatment](#9-feature-treatment)
10. [Controller Design](#10-controller-design)
11. [Backend API Requirements](#11-backend-api-requirements)
12. [Failure Modes](#12-failure-modes)
13. [Observability](#13-observability)
14. [Testing Strategy](#14-testing-strategy)
15. [Migration and Compatibility](#15-migration-and-compatibility)
16. [Open Questions](#16-open-questions)

Appendices:

- [Appendix A: `storagecluster_types.go`](#appendix-a-storagecluster_typesgo)
- [Appendix B: `storagepool_types.go`](#appendix-b-storagepool_typesgo)
- [Appendix C: `persistentvolumeops_types.go`](#appendix-c-persistentvolumeops_typesgo)
- [Appendix D: `storageclusterops_types.go`](#appendix-d-storageclusterops_typesgo)
- [Appendix E: `operatorops_types.go`](#appendix-e-operatorops_typesgo)

---

## Overview

A supercluster is an aggregate of ordinary simplyblock clusters, its members,
presented to Kubernetes as one cluster. Every member is a complete cluster with
its own nodes, erasure-coding layout, failure domains, and storage-node workload.
The supercluster adds no data path of its own. What it adds is one name that
pools, StorageClasses, backups, and dashboards refer to, one status that rolls
up the members, and a placement decision at volume creation that picks the member
a new volume lands in.

**The supercluster is a `StorageCluster` whose `spec.aggregate` lists its
members.** The aggregate is the discriminator: a cluster with one is a
supercluster, and a cluster without one is an ordinary cluster. Members are
ordinary `StorageCluster` objects in the same namespace, unchanged in spec. The
aggregate being a field of the existing kind is what lets every reference that
already names a cluster (`StoragePool.spec.clusterRef`,
`StorageBackupPolicy.spec.clusterRef`, a StorageClass) name a supercluster with
no new reference type.

**A `ClusterDeploymentConfig` with more than one node set produces a
supercluster.** Each node set becomes one member, named `<cluster>-<nodeset>`,
and the document's `spec.cluster.name` becomes the supercluster's name. A
simplyblock cluster holds at most 32 storage nodes, so the discovery generator
splits a larger fleet into node sets of near-equal size: forty workers become
two members of twenty. A cluster also runs one storage-node DaemonSet with one
set of interfaces, so machines that differ there are drafted into separate
members.

**A volume lives in exactly one member for its whole life, unless it is moved.**
The CSI driver picks the member at `CreateVolume` and writes it into the volume
handle as it does today, so every later call on the volume reaches the member
directly. Snapshots, clones, and expansion stay inside that member. Moving a
volume to another member is an explicit operation (§8) built on the control
plane's cross-cluster replication in migration mode, which already moves a volume
between clusters of one control plane with an ANA flip instead of a reconnect.

| Concern                    | Owned by                            | Section |
|----------------------------|-------------------------------------|---------|
| Which clusters aggregate   | `StorageCluster.spec.aggregate`     | §4      |
| How members come to exist  | `ClusterDeploymentConfig` node sets | §5      |
| Which member a volume gets | CSI driver, at `CreateVolume`       | §7      |
| Moving a volume            | `PersistentVolumeOps` `Relocate`    | §8      |
| Emptying a member          | `StorageClusterOps` `Evacuate`      | §8.4    |

---

## 1. Background

**One control plane already manages several clusters.** sbcli keeps every
cluster in one FoundationDB, scopes every resource under
`/api/v2/clusters/{cluster_id}/…`, and lists them at `GET /api/v2/clusters/`
(`simplyblock_core/cluster_ops.py`, `add_cluster`). Nothing relates one cluster to
another except a pairwise replication relationship. There is no cluster group,
no parent cluster, and no placement decision that picks a cluster: the cluster is
fixed by the request path and the pool.

**The operator models one cluster per namespace, and one cluster per deployment
document.** `operator/internal/webhook/storagecluster_validator.go` refuses a
second `StorageCluster` in a namespace, because the storage-node workload carries
constant names (`atlas-lib/kube.StorageNodeSetAPIServiceName` and the TLS secret
beside it). A `ClusterDeploymentConfig` may list several node sets with several
groups each, but every group expands into one cluster, and validation refuses
groups that disagree on interfaces (`conflictingInterfaces` in
`operator/internal/controllers/deployment/validation.go`) or on device class
(the CEL rule on `ClusterDeploymentConfigSpec`). A fleet with two kinds of
machine is therefore two documents, two namespaces, and two clusters that
Kubernetes sees as unrelated.

**The CSI driver can already route between clusters.** `resolveClusterSelection`
(`csi-driver/internal/csi/controller/placement.go`) picks a cluster from an
explicit `cluster_id` parameter or from a `zone_cluster_map` or
`region_cluster_map` matched against the scheduler's topology, and the CSI
secret `simplyblock-csi-secret-v2` already lists every cluster the control plane
knows, with endpoint and credentials. The volume handle
`{clusterUUID}:{pool}:{lvolUUID}` (`atlas-lib/lvol/handle.go`) carries the
cluster, so every later call finds it without a lookup. What is missing is a
selection that is not topology: one StorageClass whose volumes spread over
several clusters by capacity.

**Volumes can already cross a cluster boundary.** sbcli's replication in
`mode=migration`, followed by `POST …/replication/commit`, copies a volume to a
cluster of the same control plane and cuts it over by flipping ANA states, so
that the target paths become optimized and the source paths inaccessible
(`services/replication_final_step.py`). The copy keeps the source NQN and NSID.
The operator does not drive this today: `PersistentVolumeOps` `Migrate` moves a
volume between nodes of one cluster.

---

## 2. Goals and Non-Goals

### Goals

- Aggregate several clusters of one control plane into one `StorageCluster` that
  pools, StorageClasses, and backup policies can name (§4).
- Report one status for the aggregate: a phase rolled up from the members, the
  combined capacity, and the lowest fault tolerance among them (§4.3).
- Expand a deployment document with several node sets into a supercluster and
  its members, with member names derived from the document's cluster name (§5).
- Hold every cluster to 32 storage nodes, and have discovery draft a larger
  fleet as evenly sized members within that limit (§5.2, §5.5).
- Draft members from the Kubernetes topology: a member within one region, each
  zone spread over every member, and volumes placed in their consumer's region
  (§5.7, §7.2).
- Let a supercluster and its members share a namespace without the workload name
  collision that limits a namespace to one cluster today (§6).
- Place each new volume in one member, chosen by a declared strategy and by
  capacity, with `CreateVolume` idempotent across members (§7).
- Move a single volume between members of one supercluster without a reconnect
  (§8), and empty a member so that it can leave the aggregate (§8.4).
- State, for every feature that exists, whether it works per member unchanged,
  needs aggregation, or is restricted, and refuse the restricted ones at
  admission rather than at the backend (§9).

### Non-Goals

- **Not a data path spanning members.** A volume's data is in one member. There
  is no striping, mirroring, or erasure coding across members, and a member's
  failure is that member's volumes' failure.
- **Not automatic rebalancing between members.** The volume rebalancer works
  within one cluster ([`design-auto-rebalancing.md`](design-auto-rebalancing.md))
  and stays that way. Moving volumes between members is an explicit operation.
  Capacity-driven relocation is a later design that would sit on §8.
- **Not a control-plane concept.** sbcli sees members as independent clusters,
  and `sbctl` lists them as such. Whether the control plane should learn the
  aggregate is §16 Q1.
- **Not aggregation across control planes or across Kubernetes clusters.** Every
  member is a cluster of the same control plane, deployed in the same Kubernetes
  cluster and namespace. Fleets spanning Kubernetes clusters are the management
  hub's ([`design-management-hub.md`](design-management-hub.md)).
- **Not per-member layout overrides in a deployment document.** The document's
  `spec.cluster` template applies to every member it creates (§5.1). A member
  needing a different erasure-coding layout or vCPU count is written as its own
  `StorageCluster` and named in the aggregate by hand (§16 Q2).
- **Not nesting.** A supercluster's members are ordinary clusters. A supercluster
  cannot be a member.

---

## 3. Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────────┐
│                       Kubernetes Control Plane                           │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ SuperclusterReconciler (StorageCluster with spec.aggregate)        │  │
│  │  1. resolve members, stamp status.role on the aggregate            │  │
│  │  2. read each member's status and capacity, roll up status         │  │
│  │  3. publish the aggregate entry into simplyblock-csi-secret-v2     │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ StorageClusterReconciler (members and standalone clusters)         │  │
│  │  unchanged, except the workload names of a member (§6)             │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ StoragePoolReconciler                                              │  │
│  │  a pool on a supercluster creates one backend pool per member      │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ PersistentVolumeOps (Relocate) / StorageClusterOps (Evacuate)      │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│  StorageCluster (aggregate)  StorageCluster (member) × N  StoragePool    │
└──────────────────────────────────────────────────────────────────────────┘
       │ HTTP (webapi client)                     │ simplyblock-csi-secret-v2
       │                                          ▼
       │                      ┌────────────────────────────────────────────┐
       │                      │ CSI controller: resolveClusterSelection    │
       │                      │ supercluster → member → handle             │
       │                      └───────────────────┬────────────────────────┘
┌──────▼──────────────────────────────────────────▼────────────────────────┐
│                 simplyblock control plane (one FoundationDB)             │
│  GET  /api/v2/clusters/{member}/capacity                                 │
│  POST /api/v2/clusters/{member}/storage-pools/                           │
│  POST /api/v2/clusters/{member}/storage-pools/{pool}/volumes/            │
│  POST …/volumes/{volume}/replication/start   (mode=migration)            │
│  POST …/volumes/{volume}/replication/commit                              │
└──────────────────────────────────────────────────────────────────────────┘
```

**The supercluster has no backend cluster of its own.** Its `status.uuid` stays
empty, and every call the operator or the CSI driver makes goes to a member's
UUID. About forty-five call sites resolve a cluster name to `status.uuid`
through `operator/internal/utils/objects.go`, and §15 is the audit that keeps any
of them from sending an empty UUID to the control plane.

**The CSI driver does not read custom resources.** It learns the aggregate from
the secret it already reads, which the supercluster controller writes, so a
member joining or leaving changes placement without changing any StorageClass
(whose parameters are immutable).

---

## 4. API: A Supercluster Is a StorageCluster with an Aggregate

### 4.1 Spec

Two fields are added to `StorageClusterSpec`. The full types are in
[Appendix A](#appendix-a-storagecluster_typesgo).

```go
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	Aggregate []AggregateMember `json:"aggregate,omitempty"`

	// +optional
	Placement *AggregatePlacement `json:"placement,omitempty"`
```

**`aggregate` is the whole discriminator, and no boolean stands beside it.** A
non-empty list already says that a cluster is a supercluster, so the API carries
that fact once and has no pair of fields that can disagree.

**An entry is a struct rather than a bare name**, because a member has one
property of its own: whether new volumes may be placed in it.
`disablePlacement: true` takes a member out of selection while leaving its
volumes, pools, and status where they are, which is the first step of emptying
it (§8.4) and the switch an administrator reaches for when one member is close to
full.

**The minimum is two members.** An aggregate of one is a cluster with an extra
name, and none of the behavior in this document applies to it. The
maximum of thirty-two bounds the per-member work every reconcile does and the
size of the secret entry the CSI driver parses on every `CreateVolume`.

**`placement.strategy` decides how the CSI driver ranks eligible members.**
`MostAvailableCapacity` (the default) picks the member with the most free
physical capacity. `LeastProvisioned` picks the member with the lowest ratio of
provisioned to physical capacity, which is the better choice where thin
provisioning is heavy and free capacity is a poor predictor. §7.2 has the
algorithm.

### 4.2 What a supercluster's spec may carry

A supercluster has no nodes, so most of `StorageClusterSpec` describes nothing.
Exactly these fields are admitted beside `aggregate`: `placement`,
`warningThreshold`, and `criticalThreshold`. The thresholds apply to the combined
capacity in §4.3 and drive the supercluster's own capacity events. Every other
field is refused with one message naming the field. The webhook enforces this
rule rather than CEL, because CEL cannot enumerate the keys an object carries,
and a rule naming each forbidden field goes stale the first time a field is
added to the spec. Members carry their own full
specs, which a deployment document writes identically (§5.1).

`maxSubsystemCount` and `vcpuCount` are `Required` today. They become optional in
the schema, and a CEL rule requires both whenever `aggregate` is absent, so that
an ordinary cluster is held to exactly what it was. Relaxing a required field is
a non-breaking change for writers, and both fields are already pointers in Go.

**`aggregate` is mutable in two directions only.** An entry may be added, which
is how a supercluster grows by a member (§5.6). An entry may be removed only when
the member it names holds no volumes, which admission checks against the member's
pools (§4.4) and which §8.4 produces deliberately. Turning a supercluster into an
ordinary cluster, or an ordinary cluster into a supercluster, is refused, because
a supercluster's pools exist only in its members and an ordinary cluster's
nodes and volumes exist only in itself.

### 4.3 Status

| Field                      | On             | Meaning                                                                                  |
|----------------------------|----------------|------------------------------------------------------------------------------------------|
| `status.role`              | every cluster  | `Standalone`, `Supercluster`, or `Member`                                                |
| `status.superclusterRef`   | a member       | The name of the supercluster whose aggregate lists it                                    |
| `status.members[]`         | a supercluster | Per member: name, UUID, phase, whether placement may choose it, and its capacity reading |
| `status.capacity`          | a supercluster | Sums of the members' total, used, and provisioned bytes                                  |
| `status.maxFaultTolerance` | a supercluster | The lowest `maxFaultTolerance` among the members                                         |

Every field is reconstructable from the members and the control plane, which is
the rule for status ([`design-crd-model.md`](crd-redesign/design-crd-model.md)
§7.9). `role` and `superclusterRef` on a member are written by the member's own
reconciler, which lists the superclusters in its namespace, so no controller
writes another object's status.

**The phase rolls up.** A supercluster uses the existing `StorageClusterPhase`
values, so a dashboard reading the phase column needs no new vocabulary.

| Members                                                         | Supercluster phase |
|-----------------------------------------------------------------|--------------------|
| Any member not yet resolved, or still `Pending` or `Creating`   | `Provisioning`     |
| Every member `Online`                                           | `Online`           |
| At least one member serving, at least one not `Online`          | `Degraded`         |
| No member serving, at least one `Suspended`, none `Unavailable` | `Suspended`        |
| No member serving, otherwise                                    | `Unavailable`      |

"Serving" is `Online`, `Degraded`, `Rebalancing`, or `Shrinking`. A member that
is rebalancing makes the supercluster `Degraded` rather than `Rebalancing`,
because a rebalance in one member says nothing about the others, and the member
row in `status.members` keeps the member's own phase.

A new printcolumn `Role` sits after `Phase`, and a priority-one `Members` column
reads `status.memberCount`.

### 4.4 Admission

The StorageCluster webhook (`operator/internal/webhook/storagecluster_validator.go`)
gains these rules, each refusing with a message that names the member:

| Rule                                                                                 | Reason                                                         |
|--------------------------------------------------------------------------------------|----------------------------------------------------------------|
| Every name in `aggregate` resolves to a `StorageCluster` in the same namespace       | `clusterRef` resolution is namespace-local everywhere else     |
| No member is itself a supercluster                                                   | No nesting (§2)                                                |
| No member is listed by a second supercluster                                         | A volume's member has to have one answer to "which aggregate"  |
| Every member is on the same `ControlPlane`                                           | Cross-member moves need one FoundationDB (P0-1)                |
| Removing an entry requires the member's pools to report no bound volumes             | Removal by omission must not strand data (§4.2)                |
| Deleting a `StorageCluster` that an aggregate lists is refused                       | A member leaves the aggregate first, which is the guarded path |
| A namespace holds one standalone cluster, or one supercluster and its members (§6.2) | The workload names (§6)                                        |

The second and third rules are checked again on the member side, so that a
create racing an aggregate edit cannot slip between them.

### 4.5 Example

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: StorageCluster
metadata:
  name: prod
  namespace: simplyblock
spec:
  aggregate:
    - name: prod-dense
    - name: prod-fast
    - name: prod-archive
      disablePlacement: true
  placement:
    strategy: MostAvailableCapacity
  warningThreshold:
    capacity: 80
  criticalThreshold:
    capacity: 90
status:
  role: Supercluster
  phase: Degraded
  maxFaultTolerance: 1
  capacity:
    totalBytes: 1319413953331200
    usedBytes: 412316860416000
    provisionedBytes: 989560464998400
  members:
    - name: prod-dense
      uuid: 4f0c…
      phase: Online
      placementEligible: true
    - name: prod-fast
      uuid: 9b21…
      phase: Degraded
      placementEligible: true
    - name: prod-archive
      uuid: c7e8…
      phase: Online
      placementEligible: false
  observedGeneration: 3
```

---

## 5. Expansion from a ClusterDeploymentConfig

### 5.1 Node sets become members

The expansion machine
([`design-clusterdeploymentconfig.md`](crd-redesign/design-clusterdeploymentconfig.md)
§4.2) reads a document with one node set exactly as it does today: one cluster,
named `spec.cluster.name`. A document with two or more node sets produces a
supercluster, with one member per node set.

**A node set is the unit a member is made of, and a group stays what it is.** A
`NodeGroup` is a hardware signature: discovery puts two workers in one group only
when their device class, management interface name, and device addresses all
match ([`discovery-generator-rules.md`](../tests/discovery-generator-rules.md)
§6.1), which on hardware with persistent device names means one group per worker.
A node set carries no sizing and holds any number of groups, so it is the level
that can be sized to a cluster.

**A member keeps fault tolerance across racks.** Failure domains are a group's
field (`NodeGroup.failureDomain`), not a node set's, so a node set whose groups
name three racks is a member spread over three racks. The discovery generator
deals workers into node sets by failure domain for exactly this reason (§5.5).

**The `spec.cluster` template applies to every member.** Sizing is uniform within
a cluster because one DaemonSet serves every node of it, and it stays uniform
within each member. Across members it is the same by construction, since one
template is copied into each. The interface and device-class checks that today
span the whole document now span one node set: two node sets may differ in
`mgmtInterface`, `dataInterfaces`, and device class, because they become two
clusters with two DaemonSets. Two groups within one node set that disagree remain
a finding, as `TestGroupsThatDisagreeAboutInterfacesAreReported` asserts today.
The device-class rule is CEL on `ClusterDeploymentConfigSpec` today and spans
every node set, so it narrows to `self.nodeSets.all(s, …)` over each set's
groups, and `DeviceClassOf` in `expansion.go` stamps each member with its own
node set's class.

**A worker belongs to one member.** The existing rule that a worker appears in
one group (`duplicateWorkers` in `validation.go`) is what guarantees it, and it
is unchanged.

### 5.2 Size limits

A simplyblock cluster holds at most 32 storage nodes. A worker runs one storage
node per slot, and the slot count comes from the template
(`slotsOf(socketsToUse, nodesPerSocket)` in
`operator/internal/controllers/deployment/expansion.go`), so it is the same for
every worker of the document. A node set's storage-node count is therefore its
worker count times the slots per worker, and a node set may list at most
`floor(32 / slots)` workers.

| Limit                               | Value                    | Refused at approval with |
|-------------------------------------|--------------------------|--------------------------|
| Storage nodes per node set (member) | 32                       | `MemberTooLarge`         |
| Node sets per document (members)    | 32                       | `TooManyMembers`         |
| Member name                         | 63 characters, DNS label | `MemberNameInvalid`      |

**The storage-node limit holds for an ordinary cluster too.** A one-node-set
document listing more than 32 storage nodes is refused with `MemberTooLarge`,
because the limit is a cluster's, whether or not the cluster is a member.
The two limits of 32 are independent: one bounds a member's nodes, and the other
bounds the members of an aggregate (§4.1).

### 5.3 Naming

| Object                  | Name                      | Example (`spec.cluster.name: prod`, node set `discovered-1`) |
|-------------------------|---------------------------|--------------------------------------------------------------|
| Supercluster            | `spec.cluster.name`       | `prod`                                                       |
| Member `StorageCluster` | `<cluster>-<nodeset>`     | `prod-discovered-1`                                          |
| Backend cluster         | the member's name         | `prod-discovered-1`                                          |
| `StorageNode`           | as today, from the member | `prod-discovered-1-<worker>-<slot>`                          |

Validation computes every member name before approval and refuses the document
with `MemberNameInvalid`, naming the node set, when one does not fit. The
supercluster's name is also a backend-unique name in practice, because sbcli
refuses duplicate cluster names across its database and the members carry the
supercluster's name as a prefix. `StorageNode.spec.nodeSet` already records the
node set, so every node names the member it was created for.

**The suffix is the node set name**, because a member name is read in every
event, log line, and `sbctl` listing for the life of the cluster, and a reviewer
renaming `discovered-1` to `rack-a-dense` before approval is renaming the member.
§16 Q3 records what happens when a name is too long.

### 5.4 The expansion machine

The steps keep their names. What each does with more than one member:

```
  approved, control plane ready
    │
    ▼
  Validating        ← per node set: interfaces, device class, failure domains,
                      storage nodes ≤ 32, member name fits, one region
                      (§5.2, §5.3, §5.7)
    │  clean
    ▼
  CreatingCluster   ← create the members first, then the supercluster listing them
    │
    ▼
  AwaitingCluster   ← wait for every member's status.uuid
    │
    ▼
  CreatingNodes     ← per member: map failure domains, one StorageNode per
                      worker per slot
    │
    ▼
  Activating        ← one StorageClusterOps Activate per member, named after
                      the document and the member
    │
    ▼
  phase: Expanded
```

**Members are created before the supercluster**, so that the aggregate's
admission rule that every member resolves (§4.4) holds on the first write. The
step is idempotent by name in both halves: a crash between the two creates the
supercluster on the next pass and no second member.

**`status.clusterRef` names the supercluster**, and `status.nodeRefs` lists the
nodes of all members, so the document's printcolumn names the object an
administrator works with.

**Activation runs per member and finishes with the last.** A member whose nodes
are all online is activated without waiting for the others, because members are
independent clusters and an activation is per cluster.

**The erasure-coding minimum is checked per member.** Each scheme needs a minimum
number of storage nodes (eight for 4+2), and the existing check that compares the
document's node count against it now compares each node set's count, since each
member stripes only across its own nodes.

### 5.5 The discovery generator sizes members

Discovery (`operator/internal/discovery`) writes the draft a reviewer approves,
so it is where the size limit is met rather than discovered at approval. The
generator splits each tier into node sets of near-equal size, each within the
limit. Forty workers with one slot each become two node sets of twenty. Members
of equal size fill at the same rate, take the same share of new volumes, and
each cost the aggregate the same share of its capacity when one fails.

The split runs after grouping and after the role split
([`discovery-generator-rules.md`](../tests/discovery-generator-rules.md) §6.4),
once per role tier:

1. **Partition by what a member must share.** Workers in different regions, or
   whose groups differ in management interface name, data interfaces, or device
   class, cannot share a cluster, so each such class is split separately (§5.7).
2. **Count the members.** With `s` slots per worker and `n` workers in a
   partition, a member holds at most `c = floor(32 / s)` workers, and the
   partition needs `k = ceil(n / c)` members.
3. **Size them evenly.** `n mod k` members hold `ceil(n / k)` workers and the
   rest hold `floor(n / k)`, so no two members differ by more than one worker.
4. **Deal by failure domain.** Workers are sorted by failure domain, read from
   the Kubernetes topology (§5.7), then by name, and dealt round-robin across the
   `k` node sets, so that every failure domain present in the partition is spread
   over every member.
5. **Group within each node set.** Hardware grouping runs again per node set, so
   group names stay unique within a set as the API requires.
6. **Name the node sets.** A tier that needs one node set keeps today's name
   (`infra`, `discovered`, or `control-plane`). A tier that needs several numbers
   them from one, as `discovered-1`, `discovered-2`, and so on, in dealing order.
   A tier spanning several regions carries the region before the number, as
   `discovered-eu-west-1-1`.

| Workers | Slots per worker | Members | Workers per member |
|---------|------------------|---------|--------------------|
| 20      | 1                | 1       | 20                 |
| 32      | 1                | 1       | 32                 |
| 33      | 1                | 2       | 17, 16             |
| 40      | 1                | 2       | 20, 20             |
| 40      | 2                | 3       | 14, 13, 13         |
| 65      | 1                | 3       | 22, 22, 21         |

**The split replaces the overflow split.** A node set within the storage-node
limit lists at most 32 workers and therefore at most 32 groups, so the
sixty-four-group overflow that `nodesetlimit_test.go` covers cannot occur. That
builder and its tests are replaced by the size split and its tests.

**A partition below the erasure-coding minimum is not padded.** The generator
never moves a worker into a member it cannot share hardware with. A partition
too small for the scheme is drafted as it is, and validation reports it with the
scheme's minimum, which is today's behavior for a small fleet.

**The role split drafts one member per tier.** A fleet with disks in both its
infrastructure nodes and its workers is drafted as an `infra` block and a
`discovered` block, so that a reviewer chooses a tier by deleting a block.
Approving both approves a supercluster of two members, and the draft's
`status.message` names the members a reviewer is about to approve.

### 5.6 Growth

Growth stays a second document naming the existing cluster in `spec.clusterRef`
([`design-clusterdeploymentconfig.md`](crd-redesign/design-clusterdeploymentconfig.md)
§6).

| `clusterRef` names  | The growth document's node sets    | Expansion does                                                                     |
|---------------------|------------------------------------|------------------------------------------------------------------------------------|
| An ordinary cluster | Any number                         | Adds every node set's nodes to that cluster, as today, within 32 storage nodes     |
| A supercluster      | All named like existing members    | Adds nodes to those members, each within 32 storage nodes                          |
| A supercluster      | Some named like no existing member | Creates those members and appends them to `spec.aggregate`                         |
| A member            | Any number                         | Treated as growth of that ordinary cluster. Node set names are not used for naming |

The storage-node limit counts the nodes a member already has: a growth document
adding eight workers to a member of thirty is refused with `MemberTooLarge`,
naming both numbers.

Appending to `spec.aggregate` is the one place the expansion writes a spec it did
not create in the same pass. It is an addition, which is all a growth document
is allowed to do, and it is written with an optimistic-lock patch so that a
concurrent edit by an administrator is retried rather than overwritten.

### 5.7 Kubernetes topology

The generator reads two labels off every worker's `Node` object, both
well-known Kubernetes topology labels that cloud providers and most on-premises
installers set:

| Label                           | Read as             | Effect on the draft                                                           |
|---------------------------------|---------------------|-------------------------------------------------------------------------------|
| `topology.kubernetes.io/region` | The member boundary | A member never spans two regions (§5.5 step 1)                                |
| `topology.kubernetes.io/zone`   | The failure domain  | Copied into `NodeGroup.failureDomain`, and dealt across members (§5.5 step 4) |

**A region bounds a member** because a member stripes every write synchronously
across its own nodes, so a member spanning regions puts inter-region latency in
every write. A supercluster may hold members of several regions, which is what a
Kubernetes cluster stretched over regions gets, and placement then keeps volumes
in their consumer's region (§7.2).

**A zone is a failure domain**, which is what
[`design-clusterdeploymentconfig.md`](crd-redesign/design-clusterdeploymentconfig.md)
§8.2 specifies and what discovery does not do yet
([`discovery-generator-rules.md`](../tests/discovery-generator-rules.md) §12).
This design builds it, because the dealing in §5.5 has nothing to deal by without
it. Dealing spreads every zone over every member of its region, so each member
can survive the loss of a zone to the extent its erasure-coding scheme allows,
and a zone outage degrades every member a little rather than taking one member
down entirely. A fleet of thirty workers in zone `a` and ten in zone `b` becomes
two members of fifteen and five each.

**A finer domain comes from a label the run names.** Racks and power feeds have
no well-known label. `OperatorOps` `Discover` gains `failureDomainLabel`
([Appendix E](#appendix-e-operatorops_typesgo)), a label key that replaces the
zone label as the failure domain's source, for fleets whose installer labels
racks (`example.com/rack`). The value is copied verbatim and is held to the
`failureDomain` pattern, so a value that does not fit is a finding naming the
worker rather than a silent rewrite.

**Missing topology is reported, never guessed.**

| Workers in one partition           | Draft                                                                           |
|------------------------------------|---------------------------------------------------------------------------------|
| All carry the failure-domain label | Failure domains set, and `enableFailureDomains` set on the template             |
| None carries it                    | Failure domains unset, `enableFailureDomains` unset, members dealt by name only |
| Some carry it                      | Drafted, with the finding `TopologyIncomplete` naming the unlabeled workers     |
| None carries the region label      | One unnamed region, which is today's behavior                                   |

A partial labeling is a finding rather than a refusal, because the draft is a
proposal for a reviewer to correct, and labeling the missing workers and running
discovery again is the correction.

**Approval checks the region boundary on every document**, including one written
by hand. `Validating` reads the region label of every worker a node set lists and
refuses the document with `MemberSpansRegions`, naming the node set and the
regions, when they disagree. A worker without the label matches any region, so a
fleet with no region labels is never refused for it.

---

## 6. Members Share a Namespace

### 6.1 Per-member workload names

Three workload objects carry constant names today: the storage-node API Service
(`simplyblock-storage-node-api`), its EndpointSlice, and the serving certificate's
TLS secret. Node hostnames are built from the Service name, so the names are part
of every node's identity in the control plane.

**A member's workload objects carry the member's name**, as
`simplyblock-storage-node-api-<member>` and the TLS secret beside it. The
functions that produce these names move from constants to
`atlas-lib/kube` functions taking the cluster, because both the operator and the
storage-node bootstrap build hostnames from them, and two copies of that
derivation are two hostnames that disagree after the first edit.

**A cluster keeps the shared names when it already owns them.** Renaming the
Service of a running cluster changes every node's hostname, which is a node
re-registration rather than a rename. So the rule reads off what exists: a
cluster uses the shared names if it is the controller owner of the shared
Service, or if it is not a member and no cluster owns it. Every other cluster
uses per-member names. A standalone cluster adopted into an aggregate therefore
keeps its names, and at most one cluster in a namespace ever holds them, which is
what the shared names can express.

The rule is evaluated at every reconcile and never changes its answer for a
cluster whose workload exists, because ownership of the Service does not move.

### 6.2 Admission

The one-cluster-per-namespace rule becomes: a namespace holds either one
standalone cluster, or one supercluster together with the members it lists and
nothing else. A standalone cluster created beside a supercluster is refused,
because it would compete for the shared names with no aggregate to justify it. A
member created before its supercluster is admitted when an approved
`ClusterDeploymentConfig` in the namespace names it as a member, which is the
order §5.4 creates them in.

---

## 7. Volume Placement

### 7.1 Pools, StorageClasses, and the CSI configuration

**A `StoragePool` whose `clusterRef` names a supercluster is created in every
member**, under the pool's name, by the pool controller. The StorageClass
parameter `pool_name` is a name and not a UUID, so one class reaches the same pool
in whichever member the driver picks. The pool's `status.members[]` records the
backend UUID and phase per member
([Appendix B](#appendix-b-storagepool_typesgo)), and the pool is `Ready` only when
every placement-eligible member has it. A member added later receives every pool
of its supercluster before it is placement-eligible (§10.1).

**Pool limits are per member.** A pool's `limits` become the ceilings of each
member's backend pool, so a pool limited to 100k IOPS on a three-member
supercluster can deliver up to 300k IOPS in aggregate. The control plane enforces
QoS per backend pool, and splitting one ceiling across members would cap a member
at a share it may not need while another member sits idle. §16 Q4 is whether an
aggregate ceiling is wanted anyway.

**`allowedNodes` is split by member.** Each member's backend pool receives the
allowed nodes that are its own workers.

**The generated StorageClass names the supercluster.** The pool controller
writes `supercluster: <namespace>/<name>` in place of `cluster_id`.
Admission of a hand-written StorageClass is not this operator's, so the CSI
driver refuses a class carrying both parameters.

**The supercluster controller writes an `aggregates` list into
`simplyblock-csi-secret-v2`**, beside the existing `clusters` list:

```json
{
  "clusters": [
    {"cluster_id": "4f0c…", "cluster_endpoint": "…", "cluster_secret": "…", "local": true},
    {"cluster_id": "9b21…", "cluster_endpoint": "…", "cluster_secret": "…", "local": true}
  ],
  "aggregates": [
    {
      "name": "simplyblock/prod",
      "strategy": "MostAvailableCapacity",
      "members": [
        {"cluster_id": "4f0c…", "placement": true, "region": "eu-west-1", "zones": ["eu-west-1a", "eu-west-1b"]},
        {"cluster_id": "9b21…", "placement": true, "region": "eu-west-1", "zones": ["eu-west-1a", "eu-west-1b"]},
        {"cluster_id": "c7e8…", "placement": false, "region": "eu-central-1", "zones": ["eu-central-1a"]}
      ]
    }
  ]
}
```

The key is `<namespace>/<name>` rather than a generated identifier, because it is
reconstructable from the object and stable across a backup and restore of the
custom resources. Every member is also an ordinary entry in `clusters`, which is
what lets every call after `CreateVolume` find a member from the handle alone.

### 7.2 Choosing a member at CreateVolume

`resolveClusterSelection` gains one branch, taken when the class carries
`supercluster`:

1. Look up the aggregate in the secret. An unknown name fails with
   `InvalidArgument`, naming the class.
2. Restrict to members with `placement: true`.
3. Prefer members in the volume's topology. The aggregate entry records each
   member's region and zones, read from its workers' `Node` labels. When the class
   uses `WaitForFirstConsumer` and the request carries a preferred topology,
   candidates narrow to members in the preferred segment's region, then to the
   requisite segments' regions, and fall back to every eligible member only when
   neither leaves a candidate with room. A class carrying `zone_cluster_map` or
   `region_cluster_map` restricts by the map instead, as it does today.
4. Read each remaining member's capacity (`GET /api/v2/clusters/{id}/capacity`),
   in parallel, with a deadline of two seconds per member. A member that does not
   answer is dropped for this call and logged.
5. Drop members whose free capacity is below the requested size, and members
   above their own `criticalThreshold`.
6. Rank by the strategy and take the first. Ties break by cluster UUID, so that
   the choice is deterministic for a test and for a retry.
7. No candidate left fails with `ResourceExhausted`, naming the aggregate and the
   reason each member was dropped.

The chosen member's UUID is the first component of the volume handle, exactly
as a `cluster_id` class produces today.

### 7.3 CreateVolume is idempotent across members

The external-provisioner retries `CreateVolume` with the same name until it gets
an answer, and a retry may rank members differently, because capacity changed in
between. Without a check, a retry after a timeout creates a second volume in a
second member and the first is leaked.

**Before ranking, the driver looks for the name in every member of the
aggregate**, including members with `placement: false`, through the existing
lookup by name in the class's pool. A hit returns that volume, so the first
member to receive the volume is the member it keeps. The lookup costs one call
per member on every `CreateVolume`, which the thirty-two-member maximum bounds.

### 7.4 What a volume knows about its aggregate

Nothing. The handle names the member, and every operation after creation reaches
the member directly. Listing snapshots already walks every cluster in the
secret. This is what lets a member keep working with the supercluster object
deleted, and what keeps the aggregate out of the data path.

---

## 8. Moving Volumes Between Members

### 8.1 API

`PersistentVolumeOps` gains a second action, `Relocate`
([Appendix C](#appendix-c-persistentvolumeops_typesgo)). The kind was named for
its target so that it could carry one.

```go
	// +kubebuilder:validation:Required
	// +k8s:immutable
	TargetClusterRef string `json:"targetClusterRef"`
```

Admission requires that the target is a member of the same supercluster as the
volume's current member, that it is a different member, and that it is
placement-eligible. A volume in a member of no supercluster is refused: moving
between unrelated clusters is replication's business
([`design-csi-addons-replication.md`](design-csi-addons-replication.md)).

The lock is the same annotation on the subsystem's volumes that `Migrate` takes
([`design-persistentvolumeops.md`](crd-redesign/design-persistentvolumeops.md)
§6), so a `Migrate` and a `Relocate` of one volume exclude each other. Volumes
sharing a subsystem move together, because the control plane requires them to
replicate to the same target node, and the operation lists every volume it moves
in `status.relocate.volumes`.

### 8.2 The step machine

```
  action=Relocate received
    │
    ▼
  Validating       ← target eligible, capacity for every volume of the subsystem,
    │                no group membership (§9.3), not encrypted unless P0-5 holds
    │  clean
    ▼
  Replicating      ← POST …/replication/start  mode=migration,
    │                replication_cluster_id=<target>
    │  status reports in sync
    ▼
  AttachingPaths   ← a Job on every consuming node attaches the target's paths
    │                (P0-4), as Migrate's validation Jobs do today
    │  every Job succeeded
    ▼
  Committing       ← POST …/replication/commit  delete_source=true  (write-ahead
    │                status.relocate.committed before the call)
    │  202, task location recorded
    ▼
  Verifying        ← task finished; the original handle resolves to the target
    │                (P0-3); Jobs deleted
    ▼
  phase: Succeeded
```

| Condition                                         | Step                        | Result                                                               |
|---------------------------------------------------|-----------------------------|----------------------------------------------------------------------|
| `abort: true`                                     | Before `Committing`         | `POST …/replication/stop`, Jobs deleted, phase `Aborted`             |
| `abort: true`                                     | `Committing` or later       | Ignored with an event: a committed cutover cannot be undone          |
| Operator restart                                  | Any                         | Resumes from `status.step`. `committed` prevents a second commit     |
| Start or commit returns 5xx or times out          | `Replicating`, `Committing` | Retried with backoff, no step advance                                |
| Start returns 4xx                                 | `Replicating`               | Phase `Failed`, the control plane's message in `status.message`      |
| A consuming node's Job fails                      | `AttachingPaths`            | Phase `Failed` after `stop`. The volume stays in its member          |
| Commit task reports failure                       | `Verifying`                 | Phase `Failed`, with the control plane's relationship state reported |
| The original handle does not resolve after commit | `Verifying`                 | Phase `Failed` with `HandleUnresolved`, which is the P0-3 gap        |

### 8.3 The handle survives the move

A PV's `spec.csi.volumeHandle` is immutable and names the source member. The
control plane keeps the source NQN on the copy, and it already reports a
failed-over volume's target through `target-lvol-id` in the connect response,
which the CSI node plugin uses for device lookup
(`csi-driver/internal/controlplane/client.go`). Relocation needs the same
property for every controller call: expand, snapshot, clone, delete, and
connect, addressed to the source member with the original volume UUID, have to
reach the volume in the target member. That is P0-3, and until the control plane
states that it holds, `Verifying` asserts it rather than assuming it.

### 8.4 Emptying a member (Phase 4)

`StorageClusterOps` gains `Evacuate`
([Appendix D](#appendix-d-storageclusterops_typesgo)), targeting a member. It
sets the member's `disablePlacement` through the aggregate, raises one
`PersistentVolumeOps` `Relocate` per subsystem with a `creatorRef` naming the
operation, as a node drain does today
([`design-persistentvolumeops.md`](crd-redesign/design-persistentvolumeops.md)
§11.1), and chooses each target by the aggregate's placement strategy. When every
child has succeeded and the member's pools report no bound volumes, it removes
the member's entry from the aggregate. Deleting the now-standalone cluster is a
separate, deliberate act.

`Evacuate` reuses the bounded-concurrency walk of a drain: at most
`maxConcurrentRelocations` children run at once (default 1), and a child that
fails holds the walk with an event instead of skipping the volume.

---

## 9. Feature Treatment

Every feature is in one of four classes. A per-member feature works unchanged,
because the handle names the member. An aggregated feature is extended by this
design to span members. A restricted feature works within one member and is
refused across members. An out-of-scope feature is refused when it names a
supercluster.

| #   | Feature                                         | Treatment    | Behavior in a supercluster                                                                                       | Phase |
|-----|-------------------------------------------------|--------------|------------------------------------------------------------------------------------------------------------------|-------|
| 1   | Volume creation                                 | Aggregated   | Member chosen by §7.2                                                                                            | 2     |
| 2   | `CreateVolume` retry                            | Aggregated   | Name looked up in every member first (§7.3)                                                                      | 2     |
| 3   | Volume expansion                                | Per member   | Expands in place. Fails with `ResourceExhausted` when the member is full, and relocation is the remedy           | —     |
| 4   | Snapshots                                       | Per member   | Taken in the volume's member. Snapshot names are unique per backend cluster, which is unchanged                  | —     |
| 5   | Clone from a volume or restore from a snapshot  | Restricted   | Lands in the source's member regardless of placement (§9.2)                                                      | 2     |
| 6   | Volume group snapshot, consistency group        | Restricted   | All volumes of a group must share a member (§9.3)                                                                | 2     |
| 7   | Shared subsystems (`max_namespace_per_subsys`)  | Restricted   | Volumes sharing a subsystem share a member and move together (§8.1)                                              | 3     |
| 8   | Node-to-node migration, rebalancer, realignment | Per member   | Run per member. The supercluster is skipped by the rebalancer's watch                                            | 1     |
| 9   | Pools and QoS                                   | Aggregated   | One backend pool per member, limits per member (§7.1)                                                            | 2     |
| 10  | DHCHAP and `allowedNodes`                       | Aggregated   | Keys are per backend pool and therefore per member. Allowed nodes split by member (§7.1)                         | 2     |
| 11  | Encryption and KMS                              | Per member   | Members share one KMS configuration through the template. Moves of encrypted volumes wait on P0-5 (§9.4)         | 3     |
| 12  | Backup policies and restore                     | Aggregated   | A policy on a supercluster fans out per member. A restore places by §7.2 (§9.5)                                  | 2     |
| 13  | Replication (`ReplicationPair`, CSI-Addons)     | Out of scope | A pair naming a supercluster is refused. Members may be named directly (§16 Q5)                                  | —     |
| 14  | `TestFailover`                                  | Per member   | Works from volume handles and is unaffected                                                                      | —     |
| 15  | `NFSExport` and pNFS                            | Restricted   | An export's volume is in one member, and the metadata server runs per member. Striping across members is refused | 2     |
| 16  | Dual mode and arbitration                       | Per member   | Arbitration is per cluster and unchanged. A supercluster of Dual members is admitted                             | —     |
| 17  | `StorageClusterOps` (restart, shutdown, expand) | Restricted   | Refused on a supercluster. Raised on each member, except `Evacuate` (§9.6)                                       | 1     |
| 18  | `StorageNodeOps` (drain, remove, restart)       | Per member   | A node is a member's node, unchanged                                                                             | —     |
| 19  | Capacity thresholds and alerts                  | Aggregated   | The supercluster's thresholds apply to the combined capacity, and each member keeps its own                      | 1     |
| 20  | Failure domains                                 | Per member   | Indexed per member in the member's `status.failureDomains`                                                       | —     |
| 21  | `StorageSiteDeployment`                         | Aggregated   | Mirrors the supercluster with its member UUIDs instead of one UUID (§9.7)                                        | 2     |
| 22  | `StorageClusterMetrics` and Prometheus          | Aggregated   | Member series gain a `supercluster` label (§13)                                                                  | 1     |
| 23  | Control-plane upgrade                           | Per member   | One control plane serves every member, and its upgrade is unaffected                                             | —     |

### 9.1 What "per member" guarantees

A per-member feature reads the cluster from the volume handle, a `clusterRef`
naming a member, or a node's own `clusterRef`. None of them can reach a
supercluster, because the supercluster has no UUID and no nodes. The audit in §15
is what makes that true for the `clusterRef` call sites rather than assumed.

### 9.2 Clones and restores stay in their source member

The control plane places a clone on its snapshot's node, logical volume store,
and pool, and has no cross-cluster clone except through replication. A clone
therefore lands in its source member whatever the class says, which the driver
already does (`clone.go` uses the snapshot's cluster). When the class names a
supercluster, the driver checks that the source member belongs to it and refuses
the clone with `InvalidArgument` otherwise, so that a clone cannot leave the
aggregate its class promises. A clone into a full member fails with
`ResourceExhausted` and the source member named.

### 9.3 Groups need one member

The CSI driver already refuses a volume group whose members live in more than one
cluster (`parseGroupMembers` in `csi-driver/internal/csi/controller/volumegroup.go`).
With placement spreading volumes, that refusal becomes likely rather than
exceptional. Until §16 Q6 settles an affinity mechanism, the behavior is the
refusal, with the members named in the error so that the remedy (relocating the
stragglers to one member) is visible. Relocation refuses a volume that belongs to
a group, because moving one member of a consistency group breaks the group.

### 9.4 Encryption

Every member carries the KMS configuration of the template, so a volume's keys
are reachable from any member's nodes. The data encryption key is stored per
cluster, under `cluster/{cluster_id}/lvol/{lvol_id}`, so a moved encrypted volume
needs its key carried to the target member's path. Relocation refuses encrypted
volumes with `EncryptedVolumeNotMovable` until P0-5 is established.

### 9.5 Backups

A `StorageBackupPolicy` naming a supercluster fans out into one backend policy
per member, recorded per member in the policy's status. Backup identifiers are
not scoped by cluster in the control plane, so a backup taken in one member can
be restored into another, and a restore through a supercluster class places the
new volume by §7.2. A backup store is one per cluster, and the template writes
the same store to every member.

### 9.6 Cluster operations

A `StorageClusterOps` targeting a supercluster is refused, except `Evacuate`,
which targets a member and edits the aggregate. A rolling restart of a whole
supercluster is a rolling restart of each member, raised member by member, since
an order and a failure policy across members are not defined. §16 Q7 records
whether a fan-out action is wanted.

### 9.7 StorageSiteDeployment

A site deployment mirrors one `StorageCluster` and its UUID back to the hub,
because a StorageClass on the hub side names `cluster_id` and `pool_name`. For a
supercluster it mirrors the aggregate's name and the member UUIDs, and the hub's
class names the aggregate. That needs the hub's CSI secret to carry the
aggregate entry, which is a change to
[`design-management-hub.md`](design-management-hub.md) and is §16 Q8.

---

## 10. Controller Design

### 10.1 Location and triggers

**`SuperclusterReconciler`** lives in
`operator/internal/controllers/cluster/supercluster_controller.go`. It
reconciles `StorageCluster` objects with a non-empty `spec.aggregate`, and
`StorageClusterReconciler` gains the inverse predicate, so each object has
exactly one reconciler. It watches member `StorageCluster` objects and maps each
to the supercluster listing it, and it polls capacity every 60 seconds.

Each reconcile:

1. Resolves every member. An unresolved member is reported in
   `status.members` with phase `Pending` and an event, and does not stop the
   others from being read.
2. Reads each member's phase and maximum fault tolerance from its status, and
   its capacity from the control plane.
3. Records each member's region and zones, read from the `topology.kubernetes.io`
   labels of the `Node` objects its storage nodes run on.
4. Decides placement eligibility per member: listed without
   `disablePlacement`, phase serving, and holding every pool of the supercluster.
5. Writes the aggregate entry into `simplyblock-csi-secret-v2` with the same
   merge the cluster controller uses for its own entry (`mergeCSICredentials`),
   so the two writers never overwrite each other's keys.
6. Writes status with an optimistic-lock patch and `observedGeneration`.

**`StorageClusterReconciler`** writes `status.role` and `status.superclusterRef`
for every ordinary cluster, by listing superclusters in its namespace, and
chooses the workload names by §6.1.

**`StoragePoolReconciler`** branches on whether the pool's cluster is a
supercluster and, if so, creates and updates one backend pool per member. A
member listed later receives every pool before the supercluster calls it
placement-eligible.

### 10.2 Concurrency and mutual exclusion

The supercluster holds no lock of its own. Its reconcile is read-mostly: the only
external write is the secret entry, which is idempotent. A member's
`status.activeOpsRef` lock is untouched, since every operation still runs on one
member. `Evacuate` takes the member's lock for its duration, so an evacuation and
a rolling restart of one member exclude each other as two operations on a cluster
do today.

### 10.3 Interaction with existing controllers

| Controller              | Interaction                                                                              |
|-------------------------|------------------------------------------------------------------------------------------|
| Volume rebalancer       | Predicate excludes superclusters. Members are rebalanced as ordinary clusters            |
| Arbitration (`twonode`) | Requires `status.uuid`, which a supercluster never has. Predicate excludes it explicitly |
| Backup policy           | Fans out per member (§9.5)                                                               |
| Deployment expansion    | Creates members and supercluster (§5.4), appends members on growth (§5.6)                |
| `PersistentVolumeOps`   | `Relocate` reads the aggregate from the volume's member's `superclusterRef`              |

### 10.4 RBAC

No new verbs. Every resource the new controller reads or writes
(`storageclusters`, `storageclusters/status`, `storagepools`, `nodes` for their
topology labels, and the CSI secret) is already granted to the manager.

---

## 11. Backend API Requirements

| Method | Endpoint                                                                                      | Notes                                                                                                                            |
|--------|-----------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------|
| GET    | `/api/v2/clusters/`                                                                           | Exists. Used to check that every member is on the same control plane                                                             |
| GET    | `/api/v2/clusters/{cluster_id}/capacity`                                                      | Exists. Polled by the supercluster controller and read by the CSI driver at `CreateVolume`                                       |
| POST   | `/api/v2/clusters/{cluster_id}/storage-pools/`                                                | Exists. Called once per member per pool. Idempotent by name: a 409 on an existing pool is success                                |
| POST   | `/api/v2/clusters/{cluster_id}/storage-pools/{pool_id}/volumes/{volume_id}/replication/start` | Exists. Body `replication_cluster_id`, `mode: migration`. A repeat on a running relationship must not start a second             |
| GET    | `…/volumes/{volume_id}/replication/status`                                                    | Exists. Polled in `Replicating`                                                                                                  |
| POST   | `…/volumes/{volume_id}/replication/stop`                                                      | Exists. Abort before commit                                                                                                      |
| POST   | `…/volumes/{volume_id}/replication/commit`                                                    | Exists. Body `delete_source: true`. Returns 202 and a task location. Must refuse a second commit rather than run it twice        |
| GET    | `/api/v2/clusters/{cluster_id}/tasks/{task_id}`                                               | Exists. Polled in `Verifying`                                                                                                    |
| —      | The original handle resolves after commit                                                     | **Not established (P0-3).** Every controller call on the source member with the original volume UUID must reach the moved volume |
| —      | Host attachment of target paths before cutover                                                | **Not established (P0-4)**                                                                                                       |
| —      | Key carried with an encrypted volume                                                          | **Not established (P0-5)**                                                                                                       |

The CSI driver authenticates to each member with that member's entry in
`clusters`, as today. No admin token crosses a cluster boundary.

---

## 12. Failure Modes

| Failure                                              | Detection                        | Behavior                                                                                                                                                                |
|------------------------------------------------------|----------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| A member is unavailable                              | Member phase                     | Supercluster `Degraded`. The member is not placement-eligible. Its volumes are unavailable, others unaffected                                                           |
| Every member is unavailable                          | Member phases                    | Supercluster `Unavailable`. `CreateVolume` fails with `ResourceExhausted` naming each reason                                                                            |
| A member's capacity read times out at `CreateVolume` | Two-second deadline              | Member skipped for this call, logged. Other members still chosen                                                                                                        |
| Every eligible member is full                        | Capacity filter                  | `ResourceExhausted`, with the supercluster's `AggregateCapacityExhausted` event                                                                                         |
| A listed member does not exist                       | Resolve step                     | Reported `Pending` in `status.members`, event `MemberNotFound`, others reconciled                                                                                       |
| A pool fails to create in one member                 | Backend error                    | That member stays placement-ineligible, pool status names the member, retried                                                                                           |
| The CSI secret write fails                           | API error                        | Requeued. The driver keeps placing with the previous entry, which can name a member that has since been cordoned. Placement on a cordoned member is allowed, not unsafe |
| The supercluster object is deleted                   | Finalizer                        | Held while any `StoragePool` names it. Once released, members become standalone and keep serving their volumes                                                          |
| Operator restart mid-expansion                       | Persisted step                   | Members and supercluster created by name, idempotent (§5.4)                                                                                                             |
| Operator restart mid-relocation                      | Persisted step, `committed` flag | Resumes without a second start or commit (§8.2)                                                                                                                         |

---

## 13. Observability

Both tables extend surfaces that exist: the StorageCluster controller already
emits events on the cluster object, and the operator's registry already exports
per-cluster metrics labeled `cluster`.

### 13.1 Kubernetes events

Aggregate events land on the supercluster `StorageCluster`, which is the object
an administrator reads and the one that outlives any member. Relocation events
land on the `PersistentVolumeOps`, which is the audit record of the move.

| Event                                                                           | Type    | Reason                       | On                    |
|---------------------------------------------------------------------------------|---------|------------------------------|-----------------------|
| A member named in the aggregate does not resolve to a `StorageCluster`          | Warning | `MemberNotFound`             | Supercluster          |
| A member left placement because it is not serving, lacks a pool, or is cordoned | Normal  | `MemberPlacementChanged`     | Supercluster          |
| A member returned to placement                                                  | Normal  | `MemberPlacementChanged`     | Supercluster          |
| The combined capacity crossed the warning or critical threshold                 | Warning | `AggregateCapacityThreshold` | Supercluster          |
| No member has room for a requested volume                                       | Warning | `AggregateCapacityExhausted` | Supercluster          |
| A member was appended to the aggregate by a growth document                     | Normal  | `MemberAdded`                | Supercluster          |
| A member was removed from the aggregate after evacuation                        | Normal  | `MemberRemoved`              | Supercluster          |
| An abort was ignored because the cutover was already committed                  | Warning | `RelocationAbortIgnored`     | `PersistentVolumeOps` |
| The relocated volume's original handle does not resolve after the cutover       | Warning | `HandleUnresolved`           | `PersistentVolumeOps` |
| An evacuation is holding because a child relocation failed                      | Warning | `EvacuationHeld`             | `StorageClusterOps`   |

`AggregateCapacityExhausted` is raised by the CSI driver through the supercluster
it read from the class, which is the one event the driver emits on a custom
resource. It is rate-limited to one per minute per supercluster.

### 13.2 Prometheus metrics

| Metric                                       | Labels                              | Description                                                            |
|----------------------------------------------|-------------------------------------|------------------------------------------------------------------------|
| `simplyblock_supercluster_members`           | `supercluster`, `state`             | Gauge of members by `eligible`, `cordoned`, `notServing`, `missing`    |
| `simplyblock_supercluster_capacity_bytes`    | `supercluster`, `kind`              | Gauge of combined `total`, `used`, and `provisioned` bytes             |
| `simplyblock_csi_placement_decisions_total`  | `supercluster`, `cluster`, `result` | Counter of `CreateVolume` placements by chosen member, or `exhausted`  |
| `simplyblock_csi_placement_duration_seconds` | `supercluster`                      | Histogram of member selection time, lookup and capacity reads included |
| `simplyblock_relocation_operations_total`    | `supercluster`, `result`            | Counter of `Relocate` operations by `succeeded`, `failed`, `aborted`   |
| `simplyblock_relocation_duration_seconds`    | `supercluster`                      | Histogram from `Replicating` to `Succeeded`                            |

Existing per-cluster series gain a `supercluster` label on members, empty on
standalone clusters, so a dashboard groups a supercluster's members with one
`sum by (supercluster)`.

**Three of these are load-bearing.** `simplyblock_csi_placement_decisions_total`
by `cluster` is how a skewed strategy is seen: a member receiving every new
volume while another sits empty is a strategy or a capacity-reading problem, and
nothing else shows it. `simplyblock_csi_placement_duration_seconds` is the alert
for a slow member, because every member's capacity read sits on the
`CreateVolume` path and one slow member slows every provision. The `notServing`
series of `simplyblock_supercluster_members` is the alert that a supercluster is
running on fewer members than it was built with.

---

## 14. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts:
[`tests/test-plan-superclusters.md`](../tests/test-plan-superclusters.md)

- **Unit:** the phase rollup table (§4.3), the size limits and member naming
  (§5.2, §5.3), node-set-to-member derivation (§5.1), the generator's balanced
  split with its worked table (§5.5), the topology reading and its findings
  (§5.7), the workload naming rule (§6.1), and the
  member ranking with every filter and tie-break (§7.2), each against fixtures,
  plus the webhook rules of §4.4 and §6.2 with a fake client.
- **Integration:** the expansion of a multi-node-set document into members and a
  supercluster, growth adding a member, pool fan-out, and the secret entry,
  against `envtest` and a mock control plane. The `Relocate` step machine with
  every failure row of §8.2, including restart in each step.
- **E2E:** a three-member supercluster on a live control plane, volumes spread by
  a supercluster class, `CreateVolume` retried after an injected timeout without
  a duplicate, and one relocation under fio in verify mode once P0-3 and P0-4 hold.

The risk concentrates in §7.3 and §8.2. A duplicate volume from a retry is silent
capacity loss, and a relocation that commits twice or leaves a handle dangling is
data unavailability, so neither group's scenarios may be cut.

---

## 15. Migration and Compatibility

**Nothing changes for an ordinary cluster.** A cluster without `aggregate` is
admitted, reconciled, and named exactly as before, including its workload names,
which it keeps because it owns them (§6.1).

**`ClusterDeploymentConfig` and discovery need no migration.** Neither has
shipped in a release: both arrive in 26.4, and this design lands between its
second and third beta. Every change §5 makes to them (node sets as members, the
size limit, the balanced split, and the topology reading) amends an unreleased
API and unreleased generator behavior, so no document or draft exists in the
field to convert.

**The required-field relaxation is a schema change on the storage version.**
`maxSubsystemCount` and `vcpuCount` move from `Required` to a CEL rule. v1alpha1
`StorageCluster` objects convert through the hub. The new fields have no v1alpha1
counterpart and travel in the conversion stash annotation, as other hub-only
fields do.

**The `clusterRef` audit.** Every call site that resolves a cluster name to
`status.uuid` (`ResolveClusterUUID`, `ResolveClusterIdentifier`, and direct reads
of `Status.UUID`) is classified as one of: never reached by a supercluster
(admission refuses it), fanned out by this design (pools, backup policies), or
refused with a typed `ErrAggregateCluster` from the resolver. The resolver change
is what makes an unaudited site fail with a named error rather than sending an
empty UUID to the control plane.

**Spell-check terms.** `supercluster` and `superclusters` are new terms for the
repository's spell check.

---

## 16. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                            | Owner                       |
|-----|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------|
| 1   | **A control-plane aggregate:** should sbcli record the aggregate, so that `sbctl` and the web API present it as one cluster, enforce volume-name uniqueness across members, and answer "where is this volume" across members? Nothing in this design depends on it, but P0-3 might be answered most cleanly by it.  | Backend team                |
| 2   | **Per-member layout overrides:** should a deployment document be able to state a different `vcpuCount`, `maxSubsystemCount`, or `stripe` per node set? Members exist partly because hardware differs, and a dense node and a fast node may want different sizing.                                                   | Operator team               |
| 3   | **Member suffix for long node set names:** when `<cluster>-<nodeset>` exceeds 63 characters, should validation refuse (the current rule) or fall back to an ordinal suffix?                                                                                                                                         | Operator team               |
| 4   | **Aggregate pool ceilings:** is a QoS ceiling across all members of a pool wanted, given that the control plane enforces QoS per backend pool, so the operator has to split and rebalance shares to provide one?                                                                                                    | Product                     |
| 5   | **Replication with a supercluster on either side:** a pair whose source is a supercluster means one relationship per member, and a supercluster target needs a member mapping. Which shape, and is it needed in the first release?                                                                                  | Operator team, Backend team |
| 6   | **Placement affinity for groups:** volumes that will form a consistency group or a volume group must share a member. Candidates are a StorageClass parameter keying affinity on the claim's namespace (through the provisioner's `--extra-create-metadata`), or an explicit affinity key. Neither has been settled. | Operator team               |
| 7   | **Fan-out cluster operations:** should a rolling restart on the supercluster walk its members, and in which order and with what failure policy?                                                                                                                                                                     | Operator team               |
| 8   | **Site deployments:** the management hub's class names `cluster_id`. Mirroring a supercluster needs the hub's CSI secret to carry the aggregate entry, which is a change in the hub's design.                                                                                                                       | Operator team               |
| 9   | **Volume names across members:** volume names are unique per backend pool, so two members may hold the same name. The CSI driver names volumes after unique PV names, so this does not arise through Kubernetes. Is a uniqueness guarantee wanted for volumes created outside Kubernetes?                           | Backend team                |

---

## Appendix A: `storagecluster_types.go`

The additions to `StorageCluster`, in the order they land in the file. The
existing fields are specified by
[`design-storagecluster.md`](crd-redesign/design-storagecluster.md) Appendix A,
which this appendix does not restate.

```go
// AggregatePlacementStrategy is how the CSI driver ranks the members of a
// supercluster when it places a new volume.
// +kubebuilder:validation:Enum=MostAvailableCapacity;LeastProvisioned
type AggregatePlacementStrategy string

const (
	// AggregatePlacementMostAvailableCapacity places a volume in the member with
	// the most free physical capacity.
	AggregatePlacementMostAvailableCapacity AggregatePlacementStrategy = "MostAvailableCapacity"

	// AggregatePlacementLeastProvisioned places a volume in the member with the
	// lowest ratio of provisioned to physical capacity.
	AggregatePlacementLeastProvisioned AggregatePlacementStrategy = "LeastProvisioned"
)

// StorageClusterRole is what a StorageCluster is within an aggregate.
// +kubebuilder:validation:Enum=Standalone;Supercluster;Member
type StorageClusterRole string

const (
	// StorageClusterRoleStandalone is an ordinary cluster that no supercluster
	// lists.
	StorageClusterRoleStandalone StorageClusterRole = "Standalone"

	// StorageClusterRoleSupercluster is a cluster that aggregates others and has
	// no nodes of its own.
	StorageClusterRoleSupercluster StorageClusterRole = "Supercluster"

	// StorageClusterRoleMember is an ordinary cluster that a supercluster lists.
	StorageClusterRoleMember StorageClusterRole = "Member"
)

// AggregateMember names one cluster of a supercluster.
type AggregateMember struct {
	// Name is the member StorageCluster, in this cluster's namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// DisablePlacement stops new volumes being placed in this member. Volumes
	// already in it stay where they are and keep working. Set it before emptying
	// a member, or when a member is nearly full.
	// +optional
	DisablePlacement *bool `json:"disablePlacement,omitempty"`
}

// AggregatePlacement is how a supercluster places new volumes.
type AggregatePlacement struct {
	// Strategy ranks the members that can take a new volume. Defaults to
	// MostAvailableCapacity.
	// +kubebuilder:default=MostAvailableCapacity
	// +optional
	Strategy AggregatePlacementStrategy `json:"strategy,omitempty"`
}

// AggregateCapacity is capacity summed over a supercluster's members, in bytes.
type AggregateCapacity struct {
	// TotalBytes is the physical capacity.
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`

	// UsedBytes is the physical capacity in use.
	// +optional
	UsedBytes *int64 `json:"usedBytes,omitempty"`

	// ProvisionedBytes is the capacity promised to volumes.
	// +optional
	ProvisionedBytes *int64 `json:"provisionedBytes,omitempty"`
}

// AggregateMemberStatus is one member as the supercluster last read it.
type AggregateMemberStatus struct {
	// Name is the member StorageCluster.
	Name string `json:"name"`

	// UUID is the member's backend cluster UUID, empty until it is created.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// Phase is the member's own phase. Pending when the member does not resolve.
	// +optional
	Phase StorageClusterPhase `json:"phase,omitempty"`

	// PlacementEligible says whether new volumes may be placed in this member:
	// listed without disablePlacement, serving, and holding every pool of the
	// supercluster.
	// +optional
	PlacementEligible bool `json:"placementEligible,omitempty"`

	// Capacity is the member's own capacity reading.
	// +optional
	Capacity *AggregateCapacity `json:"capacity,omitempty"`
}
```

The fields added to `StorageClusterSpec`:

```go
	// Aggregate makes this cluster a supercluster: one cluster to Kubernetes,
	// made of the listed member clusters, each with its own nodes and data. A
	// new volume is placed in one member and stays there unless it is moved.
	// Members are StorageClusters in this namespace on the same control plane,
	// and none may be listed by another supercluster. Entries may be added.
	// An entry may be removed only once its member holds no volumes. A cluster
	// cannot gain or lose this field after creation.
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	Aggregate []AggregateMember `json:"aggregate,omitempty"`

	// Placement is how a supercluster places new volumes among its members.
	// Valid only with aggregate.
	// +optional
	Placement *AggregatePlacement `json:"placement,omitempty"`
```

The CEL rules added to `StorageClusterSpec`:

```go
// +kubebuilder:validation:XValidation:rule="has(self.aggregate) || (has(self.maxSubsystemCount) && has(self.vcpuCount))",message="maxSubsystemCount and vcpuCount are required unless the cluster is a supercluster"
// +kubebuilder:validation:XValidation:rule="has(self.aggregate) == has(oldSelf.aggregate)",message="a cluster cannot become a supercluster, or stop being one, after creation"
// +kubebuilder:validation:XValidation:rule="has(self.aggregate) || !has(self.placement)",message="placement is valid only on a supercluster"
```

The fields added to `StorageClusterStatus`:

```go
	// Role is what this cluster is within an aggregate.
	// +optional
	Role StorageClusterRole `json:"role,omitempty"`

	// SuperclusterRef names the supercluster that lists this cluster, on a
	// member only.
	// +optional
	SuperclusterRef string `json:"superclusterRef,omitempty"`

	// Members is each member as last read, on a supercluster only.
	// +optional
	// +listType=map
	// +listMapKey=name
	Members []AggregateMemberStatus `json:"members,omitempty"`

	// MemberCount is the number of members listed, on a supercluster only.
	// +optional
	MemberCount *int32 `json:"memberCount,omitempty"`

	// Capacity is the members' capacity summed, on a supercluster only.
	// +optional
	Capacity *AggregateCapacity `json:"capacity,omitempty"`
```

The root type, with the `Role` and `Members` printcolumns added after `Phase`
and its other markers unchanged:

```go
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="a StorageCluster name is at most 63 characters, because it is written into label values on StorageClasses, StorageDevices, and worker Nodes"
// +kubebuilder:storageversion
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=stc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=".status.role"
// +kubebuilder:printcolumn:name="Members",type=integer,JSONPath=".status.memberCount",priority=1
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// +kubebuilder:printcolumn:name="EC",type=string,JSONPath=".status.erasureCodingScheme"
// +kubebuilder:printcolumn:name="FTT",type=integer,JSONPath=".status.maxFaultTolerance",priority=1
// +kubebuilder:printcolumn:name="UUID",type=string,JSONPath=".status.uuid"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageCluster is one simplyblock backend cluster, or a supercluster that
// aggregates several of them. A backend cluster owns the storage nodes beneath
// it, the pools carved out of it, and the Kubernetes workload its nodes run as.
// A supercluster owns pools that exist in every member and has no nodes.
type StorageCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageClusterSpec   `json:"spec,omitempty"`
	Status StorageClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageClusterList contains a list of StorageCluster.
type StorageClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageCluster `json:"items"`
}
```

---

## Appendix B: `storagepool_types.go`

The addition to `StoragePoolStatus`. The rest of the kind is
[`design-storagepool.md`](crd-redesign/design-storagepool.md) Appendix A.

```go
// PoolMemberStatus is a pool's backend counterpart in one member of a
// supercluster.
type PoolMemberStatus struct {
	// Cluster is the member StorageCluster.
	Cluster string `json:"cluster"`

	// UUID is the backend pool UUID in that member, empty until created.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// Phase is this member's pool's phase.
	// +optional
	Phase StoragePoolPhase `json:"phase,omitempty"`
}
```

```go
	// Members is the pool in each member, when the pool's cluster is a
	// supercluster. UUID stays empty for such a pool, because it has one
	// backend pool per member and none of its own.
	// +optional
	// +listType=map
	// +listMapKey=cluster
	Members []PoolMemberStatus `json:"members,omitempty"`
```

---

## Appendix C: `persistentvolumeops_types.go`

The additions to `PersistentVolumeOps`. The rest of the kind is
[`design-persistentvolumeops.md`](crd-redesign/design-persistentvolumeops.md)
Appendix A.

```go
// +kubebuilder:validation:Enum=Migrate;Relocate
type PersistentVolumeOpsAction string

const (
	// PersistentVolumeOpsActionMigrate moves the volume's backing logical
	// volume to a different storage node.
	PersistentVolumeOpsActionMigrate PersistentVolumeOpsAction = "Migrate"

	// PersistentVolumeOpsActionRelocate moves the volume to a different member
	// of its supercluster, together with every volume sharing its subsystem.
	PersistentVolumeOpsActionRelocate PersistentVolumeOpsAction = "Relocate"
)

// +kubebuilder:validation:Enum=Validating;Migrating;Verifying;Replicating;AttachingPaths;Committing
type PersistentVolumeOpsStep string

const (
	PersistentVolumeOpsStepReplicating    PersistentVolumeOpsStep = "Replicating"
	PersistentVolumeOpsStepAttachingPaths PersistentVolumeOpsStep = "AttachingPaths"
	PersistentVolumeOpsStepCommitting     PersistentVolumeOpsStep = "Committing"
)

// RelocateVolumeSpec is where a Relocate moves the volume.
type RelocateVolumeSpec struct {
	// TargetClusterRef names the member StorageCluster to move the volume to.
	// It must be a different member of the same supercluster as the volume's
	// current cluster, and must accept new volumes.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +k8s:immutable
	TargetClusterRef string `json:"targetClusterRef"`
}

// RelocateStatus is the progress of a Relocate.
type RelocateStatus struct {
	// SourceClusterUUID is the member the volume started in.
	// +optional
	SourceClusterUUID string `json:"sourceClusterUUID,omitempty"`

	// TargetClusterUUID is the member the volume is moving to.
	// +optional
	TargetClusterUUID string `json:"targetClusterUUID,omitempty"`

	// Volumes are every volume moving together, the requested one and those
	// sharing its subsystem, by backend UUID.
	// +optional
	// +listType=set
	Volumes []string `json:"volumes,omitempty"`

	// Committed is set before the cutover is requested, so that a restart
	// never requests it twice.
	// +optional
	Committed bool `json:"committed,omitempty"`

	// TaskID is the control plane's task carrying the cutover.
	// +optional
	TaskID string `json:"taskID,omitempty"`
}
```

The fields added to `PersistentVolumeOpsSpec` and `PersistentVolumeOpsStatus`:

```go
	// Relocate is the target of a Relocate action, and is required with it.
	// +optional
	Relocate *RelocateVolumeSpec `json:"relocate,omitempty"`
```

```go
	// Relocate is the progress of a Relocate action.
	// +optional
	Relocate *RelocateStatus `json:"relocate,omitempty"`
```

---

## Appendix D: `storageclusterops_types.go`

The additions to `StorageClusterOps`. The rest of the kind is
[`design-storagecluster.md`](crd-redesign/design-storagecluster.md) Appendix B.

```go
// +kubebuilder:validation:Enum=Activate;Expand;Shutdown;Start;Restart;RollingRestart;CancelTask;Evacuate
type StorageClusterOpsAction string

const (
	// StorageClusterOpsActionEvacuate moves every volume out of a member of a
	// supercluster into the other members, then removes it from the aggregate.
	StorageClusterOpsActionEvacuate StorageClusterOpsAction = "Evacuate"
)

// EvacuateSpec bounds an evacuation.
type EvacuateSpec struct {
	// MaxConcurrentRelocations is how many volume relocations run at once.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	// +kubebuilder:default=1
	// +optional
	MaxConcurrentRelocations *int32 `json:"maxConcurrentRelocations,omitempty"`
}
```

---

## Appendix E: `operatorops_types.go`

The addition to the discovery run's spec. The rest of the kind is
[`design-clusterdeploymentconfig.md`](crd-redesign/design-clusterdeploymentconfig.md)
Appendix B.

```go
	// FailureDomainLabel is the Node label whose value becomes each worker's
	// failure domain in the draft, for fleets that label racks or power feeds.
	// Defaults to topology.kubernetes.io/zone. A worker without the label gets
	// no failure domain, and the draft reports it.
	// +kubebuilder:validation:MaxLength=317
	// +kubebuilder:default="topology.kubernetes.io/zone"
	// +optional
	FailureDomainLabel string `json:"failureDomainLabel,omitempty"`
```
