# Design Document: Superclusters

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-10-18  
**Target Release:** simplyblock 26.4, between beta2 and beta3  
**Test Plan:** [`tests/test-plan-superclusters.md`](../tests/test-plan-superclusters.md)

---

## Phasing Overview

| Phase                   | Status  | Scope                                                                                                                              | Blocked by                                                                                                           |
|-------------------------|---------|------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------|
| **Phase 1** (§4–§6)     | Planned | Superclusters and members on `StorageCluster`, per-cluster workload names, the shared certificate, and expansion from a deployment | Superclusters and a second standalone cluster in a namespace: P0-4. Workers shared between members: P0-3 and §16 Q11 |
| **Phase 2** (§7)        | Planned | Pools fanned out to members, the supercluster StorageClass, and member selection in the CSI driver                                 | IOPS and throughput ceilings: P0-5                                                                                   |
| **Phase 3** (§8.1–§8.3) | Planned | `PersistentVolumeOps` action `Relocate`: one volume moved to another member                                                        | —                                                                                                                    |
| **Phase 4** (§8.4)      | Planned | `StorageClusterOps` action `RemoveMember`: a member's volumes moved into the others, then the member deleted                       | Phase 3                                                                                                              |
| **Phase 5** (§7.5)      | Planned | Pool QoS ceilings shared across members by demand rather than split evenly                                                         | Phase 2, P0-5                                                                                                        |

Each phase ships on its own. Phase 1 produces a supercluster that reports one
status and lets an administrator name the members directly, which is already
useful for a fleet whose hardware cannot share one cluster. Phase 2 is what makes
it behave as one cluster to a workload. Phase 3 and Phase 4 add mobility between
members, and Phase 5 makes a pool's QoS ceiling follow its load across members.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                                                                                                                                                                                                                                                                                                       | Kind                    | Blocks                                                                       | Status          |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|------------------------------------------------------------------------------|-----------------|
| P0-1 | A cross-cluster move of an encrypted volume creates the target volume with a crypto layer and carries its data encryption key to the target's path before the source is deleted. Without it, `delete_source` deletes the only key (§9.4)                                                                                                                                                                                                           | Control plane (`sbcli`) | Phase 3 for encrypted volumes                                                | Not established |
| P0-2 | Data-plane reachability between the members' storage networks, since the move attaches the target node's transfer hub over NVMe-oF (§8.2)                                                                                                                                                                                                                                                                                                          | Node OS                 | Phase 3                                                                      | Per site        |
| P0-3 | The storage-node API (node agent) runs on the pod network. It reads the host's interfaces from the host network namespace, and its hostname, cloud instance, and topology from the Node object                                                                                                                                                                                                                                                     | Node agent (`sbcli`)    | Workers shared between members (§5.1)                                        | Rewrite planned |
| P0-4 | `POST /api/v2/clusters/` accepts the workload names (§6.1), the cluster record keeps them, and node hostnames and the SPDK pod's certificate Secret are built from them. A record without them resolves to the constant names                                                                                                                                                                                                                      | Control plane (`sbcli`) | Superclusters, and a second standalone cluster in a namespace (§6.1)         | Not established |
| P0-5 | Pool-wide IOPS and throughput limits enforced by SPDK without corrupting the volumes that share them. Today, the `PoolLimits` CEL rule refuses both (`operator/api/v1alpha2/storagepool_types.go`), because SPDK freed the pool's limits array while volumes still used it: the IOPS limit went unenforced, the throughput limit applied per volume, and SPDK crashed on every storage node (regression `2026-09-30-pool-qos-limits-corrupt-spdk`) | Data plane (SPDK)       | IOPS and throughput ceilings on a pool of a supercluster (§7.1), and Phase 5 | Not established |

Without P0-1 an encrypted volume is refused as a move candidate at admission, and
refused again before replication starts, because a move deletes its key (§9.4).
P0-2 is a property of the site, checked by the move's validation step rather than
assumed. Without P0-3 every agent binds the same host port, so a worker serves one
member (§5.1). Without P0-4 the control plane reaches nodes only through the
constant Service names, so only a cluster recorded with them can be created.
Without P0-5 the `PoolLimits` rule stays, so a pool of a supercluster, like every
pool, carries a capacity limit only.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [API: A Supercluster Is a StorageCluster](#4-api-a-supercluster-is-a-storagecluster)
5. [Expansion from a ClusterDeploymentConfig](#5-expansion-from-a-clusterdeploymentconfig)
6. [Workload Names and the Shared Certificate](#6-workload-names-and-the-shared-certificate)
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
- [Appendix F: `clusterdeploymentconfig_types.go`](#appendix-f-clusterdeploymentconfig_typesgo)
- [Appendix G: Follow-up ideas](#appendix-g-follow-up-ideas)

---

## Overview

A supercluster is an aggregate of ordinary simplyblock clusters, its members,
presented to Kubernetes as one cluster. Every member is a complete cluster with
its own nodes, erasure-coding layout, failure domains, and storage-node workload.
The supercluster adds no data path of its own. What it adds is one name that
pools, StorageClasses, backups, and dashboards refer to, one status that rolls
up the members, and a placement decision at volume creation that picks the member
a new volume lands in.

**A supercluster is a `StorageCluster` carrying `spec.supercluster`, and a member
is a `StorageCluster` carrying `spec.superclusterRef`.** Both are the existing
kind, so every reference that already names a cluster
(`StoragePool.spec.clusterRef`, `StorageBackupPolicy.spec.clusterRef`, a
StorageClass's `cluster_id`) names a supercluster with no new reference type.
Membership is a fact about the member, fixed when the member is created.

**A `ClusterDeploymentConfig` with more than one node set produces a
supercluster.** Each node set becomes one member, named
`<cluster>-<nodeset>-<suffix>` with a random three-letter suffix, and the
document's `spec.cluster.name` becomes the supercluster's name. A simplyblock
cluster holds at most 32 storage nodes, so the discovery generator splits a
larger fleet into node sets of near-equal size: forty workers become two
members of twenty. A further document with `clusterRef` naming the supercluster
adds members, and one naming a member adds nodes to it.

**A volume lives in exactly one member for its whole life, unless it is moved.**
The operator picks the member at `CreateVolume`, answering the CSI controller
over the link the driver already holds to the operator, and the driver writes
the member into the volume handle as it does today, so every later call on the
volume reaches the member directly. Snapshots, clones, and expansion stay inside
that member. Moving a volume to another member is an explicit operation (§8)
built on the control plane's cross-cluster replication in migration mode.

| Concern                    | Owned by                                         | Section |
|----------------------------|--------------------------------------------------|---------|
| Which clusters aggregate   | `StorageCluster.spec.superclusterRef` on members | §4      |
| How members come to exist  | `ClusterDeploymentConfig` node sets              | §5      |
| Which member a volume gets | The operator, asked over `csilink`               | §7      |
| Moving a volume            | `PersistentVolumeOps` `Relocate`                 | §8      |
| Removing a member          | `StorageClusterOps` `RemoveMember`               | §8.4    |

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
constant names (`atlas-lib/kube.StorageNodeSetAPIServiceName`, the SPDK proxy
Service, and their certificate Secrets). A `ClusterDeploymentConfig` may list several node sets with several
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
  pools, StorageClasses, and backup policies can name, with membership declared
  by each member (§4).
- Report one status for the aggregate: a phase rolled up from the members, the
  combined capacity, and the lowest fault tolerance among them (§4.3).
- Expand a deployment document with several node sets into a supercluster and
  its members, with member names derived from the document's cluster name (§5).
- Hold every cluster to 32 storage nodes, and have discovery draft a larger
  fleet as evenly sized members within that limit (§5.2, §5.5).
- Draft members from the Kubernetes topology: a member within one region, each
  zone spread over every member, and volumes placed in their consumer's region
  (§5.7, §7.2).
- Derive every workload name from its cluster, so that any number of clusters
  share a namespace, and give a supercluster's members one set of Services and
  one serving certificate (§6).
- Place each new volume in one member, chosen by a declared strategy and by
  capacity, with `CreateVolume` idempotent across members (§7).
- Move a single volume between members of one supercluster without a reconnect
  (§8), and remove a member by moving its volumes into the others (§8.4).
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
  and `sbctl` lists them as such. The aggregate is orchestration in the operator
  and the CSI driver, and the control plane learns of it only when a requirement
  needs it to. Nothing in this design does.
- **Not aggregation across control planes or across Kubernetes clusters.** Every
  member is a cluster of the same control plane, deployed in the same Kubernetes
  cluster and namespace as its supercluster. Fleets spanning Kubernetes clusters are the management
  hub's ([`design-management-hub.md`](design-management-hub.md)).
- **Not nesting.** A supercluster's members are ordinary clusters. A supercluster
  cannot be a member.

---

## 3. Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────────┐
│                       Kubernetes Control Plane                           │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ SuperclusterReconciler (StorageCluster with spec.supercluster)     │  │
│  │  1. list the members naming it, roll up their status               │  │
│  │  2. poll each member's capacity                                    │  │
│  │  3. own the shared Services and serving certificate (§6.2)         │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ PlacementService (served over csilink)                             │  │
│  │  the member for a new volume, from the reconciler's readings       │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ StorageClusterReconciler (members and standalone clusters)         │  │
│  │  per-cluster workload names, a member's workload in the shared     │  │
│  │  Services (§6)                                                     │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ StoragePoolReconciler                                              │  │
│  │  a pool on a supercluster creates one backend pool per member      │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ PersistentVolumeOps (Relocate) / StorageClusterOps (RemoveMember)  │  │
│  └────────────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────────────┘
       │ HTTP (webapi client)                     ▲ csilink: Place(...)
       │                                          │
       │                      ┌───────────────────┴────────────────────────┐
       │                      │ CSI controller: CreateVolume               │
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

**The supercluster has no backend cluster of its own.** Its `status.uuid` is the
aggregate's identifier, which is the object's `metadata.uid`, and it is what a
StorageClass names in `cluster_id` (§7.1). Every call the operator or the CSI
driver makes to the control plane goes to a member's UUID. About forty-five call
sites resolve a cluster name to `status.uuid` through
`operator/internal/utils/objects.go`, and §15 is the audit that keeps any of them
from sending the aggregate's identifier to the control plane.

**Placement is the operator's.** The CSI controller already holds a link to the
operator (`atlas-lib/link`, `operator/internal/csilink`), over which either end
can serve gRPC. The operator serves `PlacementService` on its end, and the CSI
controller calls it when a class's `cluster_id` names no cluster it knows. The
operator holds the members, their capacity readings, their topology, and the
placements it has made since its last reading, so the decision is taken where
all of that is.

---

## 4. API: A Supercluster Is a StorageCluster

### 4.1 Spec

Three fields are added to `StorageClusterSpec`. The full types are in
[Appendix A](#appendix-a-storagecluster_typesgo).

```go
	// +optional
	Supercluster *SuperclusterSpec `json:"supercluster,omitempty"`

	// +kubebuilder:validation:MaxLength=63
	// +optional
	SuperclusterRef string `json:"superclusterRef,omitempty"`

	// +optional
	DisablePlacement *bool `json:"disablePlacement,omitempty"`
```

**`spec.supercluster` makes a cluster a supercluster**, and holds what belongs to
the aggregate as a whole: today, the placement strategy. Its presence is fixed at
creation.

**`spec.superclusterRef` makes a cluster a member** of the named supercluster in
its namespace, reported as `status.role: PlacementGroup`, because a member is one
of the groups the supercluster's volumes are placed in. It is immutable from
creation: a member is created into its
supercluster and leaves it only by being removed (§8.4).

**`spec.disablePlacement` on a member takes it out of placement.** Its volumes,
pools, and status stay where they are. It is valid only on a member.

**`supercluster.placement.strategy` ranks the eligible members** (§7.2).
`MostAvailableCapacity`, the default, picks the member with the most free
physical capacity. `LeastProvisioned` picks the member with the lowest ratio of
provisioned to physical capacity. `LeastVolumes` picks the member holding the
fewest volumes. Under every strategy, members that rank equal are chosen between
at random.

### 4.2 What a supercluster's spec may carry

A supercluster has no nodes, so most of `StorageClusterSpec` describes nothing.
Exactly these fields are admitted beside `supercluster`: `warningThreshold` and
`criticalThreshold`, which apply to the combined capacity in §4.3 and drive the
supercluster's own capacity events. Every other field is refused with one
message naming the field. The webhook enforces this rule, since CEL cannot
enumerate the keys an object carries. Members carry their own full specs.

**A field holding its schema default counts as unset.** The API server applies
`+kubebuilder:default` values before admission, so every supercluster reaches
the webhook carrying `nvmfBasePort`, `rpcBasePort`, `snodeApiPort`,
`enableChecksumValidation`, `enableAtomicity4K`, `deviceClass`,
`maxConcurrentWorkerRestarts`, and `clusterMode`, none of which its author wrote.
The webhook admits a field whose value equals its default and refuses one that
differs. It reads the defaults from the generated CRD schema rather than from a
list of its own, so a defaulted field added to `StorageClusterSpec` later is
covered without a change to the rule. A default written by hand is admitted as
well, which is harmless, because a supercluster reads none of these fields.

`maxSubsystemCount` and `vcpuCount` are `Required` today. They become optional in
the schema, and a CEL rule requires both whenever `supercluster` is absent.

### 4.3 Status

| Field                      | On             | Meaning                                                                                    |
|----------------------------|----------------|--------------------------------------------------------------------------------------------|
| `status.uuid`              | a supercluster | The aggregate's identifier, its `metadata.uid`, which a StorageClass names in `cluster_id` |
| `status.role`              | every cluster  | `Standalone`, `Supercluster`, or `PlacementGroup`                                          |
| `status.members[]`         | a supercluster | Per member: name, UUID, phase, whether placement may choose it, and its capacity reading   |
| `status.memberCount`       | a supercluster | The number of members                                                                      |
| `status.capacity`          | a supercluster | Sums of the members' total, used, and provisioned bytes                                    |
| `status.maxFaultTolerance` | a supercluster | The lowest `maxFaultTolerance` among the members                                           |

Every field is reconstructable from the members and the control plane, which is
the rule for status ([`design-crd-model.md`](crd-redesign/design-crd-model.md)
§7.9). The supercluster finds its members by listing the clusters in its
namespace whose `spec.superclusterRef` names it, through a field index.

**The phase rolls up.** A supercluster uses the existing `StorageClusterPhase`
values, so a dashboard reading the phase column needs no new vocabulary.

| Members                                                         | Supercluster phase |
|-----------------------------------------------------------------|--------------------|
| No member yet, or any member still `Pending` or `Creating`      | `Provisioning`     |
| Every member `Online`                                           | `Online`           |
| At least one member serving, at least one not `Online`          | `Degraded`         |
| No member serving, at least one `Suspended`, none `Unavailable` | `Suspended`        |
| No member serving, otherwise                                    | `Unavailable`      |

"Serving" is `Online`, `Degraded`, `Rebalancing`, or `Shrinking`. A member that
is rebalancing makes the supercluster `Degraded`, and the member row in
`status.members` keeps the member's own phase.

A new printcolumn `Role` sits after `Phase`, and a priority-one `Members` column
reads `status.memberCount`.

### 4.4 Admission

The StorageCluster webhook (`operator/internal/webhook/storagecluster_validator.go`)
gains these rules, each refusing with a message that names the object:

| Rule                                                                                         | Reason                                                     |
|----------------------------------------------------------------------------------------------|------------------------------------------------------------|
| `superclusterRef` resolves to a supercluster in the same namespace                           | `clusterRef` resolution is namespace-local everywhere else |
| `supercluster` and `superclusterRef` are not both set                                        | A supercluster is never a member                           |
| A member is on the same `ControlPlane` as its supercluster's other members                   | Cross-member moves need one FoundationDB                   |
| `disablePlacement` is set only on a member                                                   | It is a member's property                                  |
| Deleting a supercluster that has members is refused                                          | Every member names it in an immutable field                |
| Deleting a member whose pools report bound volumes is refused, outside `RemoveMember` (§8.4) | Removal moves the volumes first                            |

The rule that refused a second `StorageCluster` in a namespace is removed. Every
cluster records its workload names, and only the first standalone cluster in a
namespace takes the constant ones (§6.1), so clusters of any kind share a
namespace freely.

### 4.5 Example

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: StorageCluster
metadata:
  name: prod
  namespace: simplyblock
spec:
  supercluster:
    placement:
      strategy: MostAvailableCapacity
  warningThreshold:
    capacity: 80
  criticalThreshold:
    capacity: 90
status:
  uuid: 0d6f1b2e-…
  role: Supercluster
  phase: Degraded
  maxFaultTolerance: 1
  memberCount: 3
  capacity:
    totalBytes: 1319413953331200
    usedBytes: 412316860416000
    provisionedBytes: 989560464998400
  members:
    - name: prod-dense-kqv
      uuid: 4f0c…
      phase: Online
      placementEligible: true
    - name: prod-fast-mzt
      uuid: 9b21…
      phase: Degraded
      placementEligible: true
    - name: prod-archive-hwd
      uuid: c7e8…
      phase: Online
      placementEligible: false
  observedGeneration: 3
---
apiVersion: storage.simplyblock.io/v1alpha2
kind: StorageCluster
metadata:
  name: prod-archive-hwd
  namespace: simplyblock
spec:
  superclusterRef: prod
  disablePlacement: true
  maxSubsystemCount: 32
  vcpuCount: 8
  # … the member's own full spec
```

---

## 5. Expansion from a ClusterDeploymentConfig

### 5.1 Node sets become members

The expansion machine
([`design-clusterdeploymentconfig.md`](crd-redesign/design-clusterdeploymentconfig.md)
§4.2) reads a document with one node set exactly as it does today: one cluster,
named `spec.cluster.name`. A document with two or more node sets produces a
supercluster, with one member per node set.

**A member keeps fault tolerance across racks.** Failure domains are a group's
field (`NodeGroup.failureDomain`), not a node set's, so a node set whose groups
name three racks is a member spread over three racks. The discovery generator
deals workers into node sets by failure domain for exactly this reason (§5.5).

**The `spec.cluster` template applies to every member, and a node set may
override it.** Sizing is uniform within a cluster because one DaemonSet serves
every node of it, and it stays uniform within each member. Across members it may
differ, because node sets may hold different hardware (§5.8). The interface and device-class checks that today
span the whole document now span one node set: two node sets may differ in
`mgmtInterface`, `dataInterfaces`, and device class, because they become two
clusters with two DaemonSets. Two groups within one node set that disagree remain
a finding, as `TestGroupsThatDisagreeAboutInterfacesAreReported` asserts today.
The device-class rule is CEL on `ClusterDeploymentConfigSpec` today and spans
every node set, so it narrows to `self.nodeSets.all(s, …)` over each set's
groups, and `DeviceClassOf` in `expansion.go` stamps each member with its own
node set's class.

**A worker may serve more than one member.** A `StorageNode` is identified by its
cluster, its worker, and its slot, so the storage nodes of two members on one
worker are distinct. The rule that a worker appears in one group
(`duplicateWorkers` in `validation.go`) narrows to one node set, as the interface
rule does: a worker listed twice within a node set remains a finding. A worker
listed in several node sets is admitted when those node sets do not overlap on it.
Their `socketsToUse` name different sockets, so each keeps the cores and the huge
pages of its own sockets, and the devices their groups resolve to on the worker
are disjoint. An overlap is a `WorkerOverlap` finding naming the worker and both node sets. A
shared worker is in a failure domain of every member it serves, and its loss takes
its storage nodes from each of them.

Sharing needs two things the members cannot provide themselves. Each member's
node agent on the worker binds its port in its own pod, which needs the agent on
the pod network (P0-3). The members' SPDK pods keep host networking and must be
handed disjoint ports, which is an open decision (§16 Q11). Until both hold, a
worker appears in one group across the whole document, as today.

### 5.2 Size limits

A simplyblock cluster holds at most 32 storage nodes. The limit bounds a
cluster's exposure to concurrent failures: its fault tolerance is set by its
erasure-coding scheme and does not grow with its node count, while the chance
that more nodes fail at once than the scheme tolerates grows with every node. A
fleet larger than one cluster is therefore several clusters, and a supercluster
makes them one to Kubernetes. A worker runs one storage
node per slot, and the slot count comes from the node set's `socketsToUse` and
`nodesPerSocket`, overridden or inherited from the template
(`slotsOf(socketsToUse, nodesPerSocket)` in
`operator/internal/controllers/deployment/expansion.go`), so it is the same for
every worker of one node set. A node set's storage-node count is therefore its
worker count times its slots per worker, and a node set may list at most
`floor(32 / slots)` workers. The control plane does not enforce the limit, and
enforcing it there is outside this design.

| Limit                               | Value         | Refused at approval with |
|-------------------------------------|---------------|--------------------------|
| Storage nodes per node set (member) | 32            | `MemberTooLarge`         |
| Cluster name (`spec.cluster.name`)  | 35 characters | API server (`MaxLength`) |
| Node set name                       | 23 characters | API server (`MaxLength`) |
| Member name                         | DNS label     | `MemberNameInvalid`      |

**The storage-node limit holds for an ordinary cluster too.** A one-node-set
document listing more than 32 storage nodes is refused with `MemberTooLarge`,
because the limit is a cluster's, whether or not the cluster is a member.

### 5.3 Naming

| Object                  | Name                           | Example (`spec.cluster.name: prod`, node set `dense`) |
|-------------------------|--------------------------------|-------------------------------------------------------|
| Supercluster            | `spec.cluster.name`            | `prod`                                                |
| Member `StorageCluster` | `<cluster>-<nodeset>-<suffix>` | `prod-dense-kqv`                                      |
| Backend cluster         | the member's name              | `prod-dense-kqv`                                      |
| `StorageNode`           | as today, from the member      | `prod-dense-kqv-<worker>-<slot>`                      |

**The suffix is three random lowercase letters**, drawn when the member is
created. Members come and go over a supercluster's life, so a member's name
carries no position: a removed member's name is never reused by the next member
from the same node set name, and two growth documents naming their node sets
alike produce two distinct members. The expansion records the drawn name in the
document's status before it creates the member, so a retry after a crash
creates the member it named rather than drawing again (§5.4). A drawn name that
already exists is drawn again, and so is a suffix another member of the same
supercluster carries, because the suffix tells members apart in node hostnames
(§6.2).

**The name limits make every member name fit.** A cluster name of at most 35
characters, a node set name of at most 23, the suffix, and two hyphens make a
member name of at most 63, which is what a `StorageCluster` name may be. Both
limits are `MaxLength` markers, so an oversized name is refused by the API server
([Appendix F](#appendix-f-clusterdeploymentconfig_typesgo)). Validation refuses a
member name that is not a DNS label with `MemberNameInvalid`, naming the node
set. `StorageNode.spec.nodeSet` already records the node set, so every node names
the part of the document it came from.

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
  CreatingCluster   ← create the supercluster; draw and record each member's
                      name; create the members, each naming the supercluster
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

**The supercluster is created before its members**, because a member names it in
an immutable field and admission resolves that name. A member then holds before
it creates its backend cluster or any workload until its supercluster reports
the shared Services and serving certificate ready (§6.2), so no member ever
starts with a workload outside its supercluster's.

**`CreatingCluster` writes ahead.** It records every member's drawn name in
`status.members` of the document before creating any member, and creates by
those names, so a crash between two creates resumes with the same names and
creates no second member.

**`status.clusterRef` names the supercluster**, and `status.nodeRefs` lists the
nodes of all members, so the document's printcolumn names the object an
administrator works with.

**Activation runs per member and finishes with the last.** A member whose nodes
are all online is activated without waiting for the others, because members are
independent clusters and an activation is per cluster.

**The erasure-coding minimum is checked per member.** Each scheme needs a minimum
number of storage nodes (eight for 4+2), and the existing check that compares the
document's node count against it compares each node set's count, since each
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
   over every member. Each worker is dealt to one node set. A worker shared
   between members (§5.1) comes from a document edited by hand.
5. **Group within each node set.** Hardware grouping runs again per node set, so
   group names stay unique within a set as the API requires.
6. **Name the node sets.** A tier that needs one node set keeps today's name
   (`infra`, `discovered`, or `control-plane`). A tier that needs several numbers
   them from one, as `discovered-1`, `discovered-2`, and so on, in dealing order.
   A tier spanning several regions carries the region before the number, as
   `discovered-eu-west-1-1`. A generated name is cut to the 23 characters a node
   set name may have, keeping its number (§5.3).

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

Growth stays a second document naming an existing cluster in `spec.clusterRef`
([`design-clusterdeploymentconfig.md`](crd-redesign/design-clusterdeploymentconfig.md)
§6), and what it names decides what it grows:

| `clusterRef` names   | Expansion does                                                                                                  |
|----------------------|-----------------------------------------------------------------------------------------------------------------|
| A standalone cluster | Adds every node set's nodes to that cluster, within 32 storage nodes                                            |
| A supercluster       | Creates one new member per node set, each named `<supercluster>-<nodeset>-<suffix>` and naming the supercluster |
| A member             | Adds every node set's nodes to that member, within 32 storage nodes                                             |

A document adding members to a supercluster states their sizing in
`spec.cluster`, as a creating document does, and its node sets may override it
(§5.8). The template's `name` is not used, because the supercluster already has
one. A document naming a supercluster without `spec.cluster` is refused with
`MemberSizingUnstated`.

The storage-node limit counts the nodes a member already has: a document adding
eight workers to a member of thirty is refused with `MemberTooLarge`, naming both
numbers.

### 5.7 Kubernetes topology

The generator reads two labels off every worker's `Node` object, both
well-known Kubernetes topology labels that cloud providers and most on-premises
installers set:

| Label                                    | Read as              | Effect on the draft                                                                 |
|------------------------------------------|----------------------|-------------------------------------------------------------------------------------|
| `topology.kubernetes.io/region`          | The member boundary  | A member never spans two regions (§5.5 step 1)                                      |
| `topology.kubernetes.io/zone`            | The failure domain   | Copied into `NodeGroup.failureDomain`, and dealt across members (§5.5 step 4)       |
| `storage.simplyblock.io/placement-group` | The member, when set | Workers with one value form one node set, in place of the split (§5.5 steps 2 to 4) |

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

**A placement-group label assigns workers to members.** The label is optional.
When every worker of a partition carries it, workers with one value form one node
set named by the value, and the generator does not count, size, or deal that
partition. Partitioning still applies (§5.5 step 1), so a value whose workers
span two regions or two hardware classes is a finding naming the value. A value
that is not a node set name (a DNS label of at most 23 characters) is a finding
naming the workers that carry it. A labeled node set over the storage-node limit
is drafted with `MemberTooLarge`, which approval refuses (§5.2). When only some
workers of a partition carry the label, the unlabeled workers are split as usual
and the draft carries the finding `PlacementGroupLabelIncomplete`, naming them.

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

**Approval checks that every member holds every failure domain**, on every
document. When the template sets `enableFailureDomains`, `Validating` collects
the failure domains each node set's groups name and refuses the document with
`MemberMissingFailureDomain` when a node set lacks a domain that another node set
of its region holds, naming the node set and the missing domains. Failure domains
and members then form a grid: a domain's loss takes nodes from every member and
all of no member. The split satisfies the rule by construction (§5.5 step 4). A
document written by hand, or drafted from placement-group labels, is held to it,
and the draft reports the same finding before approval.

### 5.8 Per-node-set overrides

A node set may hold hardware unlike any other node set of the document, which is
what makes a mixed supercluster possible: one member of NVMe drives and another
of logical block devices, or a dense member and a fast one. `NodeSet.cluster`
([Appendix F](#appendix-f-clusterdeploymentconfig_typesgo)) overrides the
template's sizing for that node set's member:

| Field               | Why it varies with hardware                                                              |
|---------------------|------------------------------------------------------------------------------------------|
| `vcpuCount`         | Cores given to SPDK depend on the machine                                                |
| `maxSubsystemCount` | Sizes huge pages, which depend on the machine's memory                                   |
| `minHugePagesSize`  | Depends on the machine's memory                                                          |
| `socketsToUse`      | Socket count depends on the machine, and decides the slots (§5.2)                        |
| `nodesPerSocket`    | Decides the slots (§5.2)                                                                 |
| `stripe`            | A small member of large drives and a large member of small drives want different schemes |

An unset field inherits the template's value. The member is created with the
merged values, the size limit counts the node set's own slots, and the
erasure-coding minimum is checked against the member's own scheme. Overrides are
refused on a document creating one standalone cluster, because its template
already states that cluster's sizing.

**Groups within a node set stay in one hardware category.** Device class is
fixed per node set by the narrowed CEL rule (§5.1), so a node set mixing NVMe
and logical block groups is still refused. The discovery generator drafts one
device class per run, and a mixed supercluster is drafted by merging two runs'
node sets into one document, or written by hand.

---

## 6. Workload Names and the Shared Certificate

### 6.1 Workload names

Two Services carry constant names today, `simplyblock-storage-node-api` and
`simplyblock-spdk-proxy`, and so do their serving-certificate Secrets
(`utils.SecretNameStorageNodeSetAPITLS` and `utils.SecretNameSpdkProxyTLS`).
Node hostnames are built from the storage-node API Service's name, as
`<node-hostname>.<service>.<namespace>.svc`, so the name is part of every node's
identity in the control plane.

The control plane builds the same hostnames from literals. `StorageNode` in
sbcli's `simplyblock_core/models/storage_node.py` connects to
`<label>.simplyblock-storage-node-api.<namespace>.svc.cluster.local` and
`<label>.simplyblock-spdk-proxy.<namespace>.svc.cluster.local` when TLS is on,
with `<label>` the node's hostname less its RPC port, and the SPDK pod and
init job templates (`storage_deploy_spdk.yaml.j2`, `storage_init_job.yaml.j2`)
mount `simplyblock-spdk-proxy-tls` by name.

**A cluster's workload names are recorded in its status.**
`status.workloadNames` holds the storage-node API Service, the SPDK proxy
Service, their certificate Secrets, and the node label suffix
([Appendix A](#appendix-a-storagecluster_typesgo)). Every reconciler and the
control plane read the names from there, and nothing derives them a second time.
The record is written once and never changed, because renaming the Service of a
running cluster changes every node's hostname, which is a node re-registration
rather than a rename.

**A cluster without the record uses the constant names.** A cluster created
before this design has a backend cluster (`status.uuid` set) and no
`status.workloadNames`, and its workload carries the constant names, because no
other names existed. The operator writes the constant names into its record on
the first reconcile after the upgrade, so the default is applied once and then
read like any other record.

**A new cluster's record is written before its workload exists.** The operator
fills `status.workloadNames` before it creates any workload object or the backend
cluster:

| Cluster                                                | Services                                                 | Node label suffix                     |
|--------------------------------------------------------|----------------------------------------------------------|---------------------------------------|
| Standalone, constant names unclaimed in the namespace  | `simplyblock-storage-node-api`, `simplyblock-spdk-proxy` | none                                  |
| Standalone, constant names recorded by another cluster | `sb-<name>-node-api`, `sb-<name>-spdk-proxy`             | none                                  |
| Supercluster                                           | `sb-<name>-node-api`, `sb-<name>-spdk-proxy`             | none, it has no nodes                 |
| Member                                                 | its supercluster's                                       | `-<suffix>`, its three letters (§5.3) |

Each certificate Secret is its Service's name with `-tls`. A derived name that
would exceed 63 characters is cut to 40 characters of `<name>` followed by eight
characters of its hash, so every name stays a valid Service name whatever the
cluster name. The functions that derive them are in `atlas-lib/kube`. Two
standalone clusters created at once may both find the constant names unclaimed.
The Service decides between them: a cluster that finds
`simplyblock-storage-node-api` controller-owned by another cluster before its own
backend cluster exists rewrites its record to derived names.

**The control plane learns the names at cluster creation.** The operator sends
the record in the body of `POST /api/v2/clusters/`, beside the `cr_name` and
`cr_namespace` it sends today, and sbcli keeps it on its cluster record. sbcli
builds node hostnames from the recorded Services and node label suffix, and
passes the recorded SPDK proxy Secret to the node agent when it starts an SPDK
pod. A cluster record without the fields uses the constant names and no suffix,
which is the rule the operator applies, so a cluster created before either side
was upgraded resolves the same names on both. Until sbcli accepts the fields
(P0-4), only a cluster whose record holds the constant names can be created.

### 6.2 A supercluster's members share its Services and certificate

**The Services of a supercluster are the supercluster's.** The supercluster
reconciler creates `sb-<supercluster>-node-api` and `sb-<supercluster>-spdk-proxy`,
headless and without a selector. Each member publishes its own EndpointSlice into
the shared Service, named for the member, listing its node agents' pod addresses
(P0-3). A node's hostname is
`<node-hostname>-<suffix>.sb-<supercluster>-node-api.<namespace>.svc`, where
`<suffix>` is the member's three letters (§5.3), because one worker may carry
storage nodes of several members (§5.1). A label longer than 63 characters is cut
by the rule of §6.1.

**One serving certificate covers every member.** It is the shared Service's
certificate, issued by the provider the operator already uses
(`service.beta.openshift.io/serving-cert-secret-name` on OpenShift, a cert-manager
`Certificate` from `simplyblock-certificate-authority-issuer` otherwise), and every
member's pods mount the one Secret. TLS clients of any member verify against one
name and one certificate, and mTLS peers present and accept one identity across
the supercluster, so a volume moved between members and a host connected to
several members need nothing per member. A member added later joins the existing
certificate with no reissue, because the certificate names the Service and not
the members.

**A member's own workload is everything else**: its DaemonSet, its ConfigMaps,
and its EndpointSlice, each named for the member as a standalone cluster's are
today.

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

**Pool limits are the supercluster's.** A pool's `limits` are the ceiling of the
pool across every member, so a pool limited to 100k IOPS delivers at most 100k
IOPS however many members it spans. The control plane enforces QoS per backend
pool, so the operator writes each member's backend pool a share of the ceiling.
Phase 2 splits the ceiling evenly over the placement-eligible members, and §7.5
specifies the demand-driven shares that replace the even split.

**Only the capacity ceiling is split until P0-5 holds.** `PoolLimits` refuses
`iops` and `throughput` on every pool today, after SPDK failed to enforce them
and crashed. The rule stays until enforcement is verified, and is then lifted for
every pool at once rather than for superclusters first, so the IOPS and
throughput shares of this section and of §7.5 are written only after P0-5.

**Every member's backend pool receives the whole `allowedNodes` list.** The list
names the Kubernetes nodes that consume the pool, not the workers that serve it:
`syncAllowedHosts` in `storagepool_controller.go` derives each node's host NQN
from its UID. A volume of the pool may be placed in any member and consumed from
any allowed node, so each member's backend pool admits the full resolved list,
including nodes that run no storage node at all.

**The generated StorageClass names the supercluster in `cluster_id`.** The pool
controller writes the supercluster's `status.uuid` there, as it writes a
cluster's UUID for an ordinary cluster, so a class names a supercluster and a
cluster the same way, and a hub-side class does too (§9.7). Every member is an
ordinary entry in `simplyblock-csi-secret-v2`, where the cluster controller
already lists every cluster the control plane knows, which is what lets every
call after `CreateVolume` find a member from the handle alone.

### 7.2 Choosing a member at CreateVolume

`resolveClusterSelection` in the CSI controller gains one branch, taken when the
class's `cluster_id` names no cluster in the secret: it calls
`PlacementService.Place` over the link to the operator, with the aggregate's
identifier, the pool name, the volume name, the requested size, the
request's accessibility requirements, the PVC's name and namespace, and the PVC's
`storage.simplyblock.io/consistency-group` label, which the controller already
reads at provisioning. The PVC's name and namespace are the parameters the
external-provisioner adds under `--extra-create-metadata`, which the operator
sets on the provisioner it deploys (`operator/internal/controllers/driver/workloads.go`).
A request without them fails with `FailedPrecondition`, as a pNFS volume does
today (`csi-driver/internal/csi/controller/pnfs.go`). The service is declared in
`atlas-lib/placement/placementrpc/placementv1/placement.proto`, beside the
link's other services, and is the first one the operator serves to a peer.

The operator answers from the supercluster reconciler's readings:

1. Resolve the aggregate. An identifier that names no supercluster fails with
   `NotFound`, which the driver returns as `InvalidArgument` naming the class, as
   an unknown cluster does today.
2. Read the PVC's placement record, then look for the volume name in every
   member's pool (§7.3). A record or a hit is the answer.
3. Follow the volume's consistency group (§9.3). A labeled request is answered
   with the member whose cluster holds the group with live members, read from
   `GET /api/v2/clusters/{cluster_id}/consistency-groups/` in each member, or
   placed by the operator for that group name since the last reading. The
   remaining steps are skipped, except that a member that is not
   placement-eligible (step 4) fails the call with `FailedPrecondition` naming
   it. A group found in two members fails with
   `FailedPrecondition` naming both. A label naming no group is placed by the
   steps below, and that member becomes the group's.
4. Restrict to placement-eligible members: not `disablePlacement`, not
   being removed, serving, and holding the pool.
5. Prefer members in the volume's topology. Each member's region and zones are
   read from its workers' `Node` labels. When the request carries a preferred
   topology, candidates narrow to members in the preferred segment's region,
   then to the requisite segments' regions, and fall back to every eligible
   member only when neither leaves a candidate with room.
6. Take each member's free capacity from the latest reading, less the sizes the
   operator has placed in it since that reading, so a burst of provisions spreads
   instead of landing on the member that was emptiest a minute ago. A reading
   older than two polling intervals is refreshed with a two-second deadline, and
   a member whose reading cannot be refreshed is dropped for this call.
7. Drop members whose free capacity is below the requested size, and members
   above their own `criticalThreshold`.
8. Rank by the strategy and take the first, choosing at random among members
   that rank equal. Under `LeastVolumes`, a member's count is the
   number of PersistentVolumes whose handle names it, from the operator's cache,
   plus the volumes placed in it whose PersistentVolume does not exist yet.
9. No candidate left fails with `ResourceExhausted`, naming the aggregate and the
   reason each member was dropped.

A member chosen in step 3 or step 8 is recorded on the PVC before `Place`
answers (§7.3), and a failed write fails the call with `Unavailable`.

The driver writes the chosen member's UUID into the volume handle, exactly as a
`cluster_id` class produces today. A class carrying `zone_cluster_map` or
`region_cluster_map` keeps its existing branch and never reaches the operator.

**The link being down fails the call with `Unavailable`**, which the
external-provisioner retries.

### 7.3 CreateVolume is idempotent across members

The external-provisioner retries `CreateVolume` with the same name until it gets
an answer, and a retry may rank members differently, because capacity changed in
between. A lookup by name cannot settle the retry alone: a create that timed out
in one member may still be running there and not yet visible, and a retry that
finds nothing would place the volume a second time in another member.

**The placement is recorded on the PVC before the first answer.** `Place` writes
the annotation `storage.simplyblock.io/placement-member`, holding the chosen
member's UUID, to the PVC the request names, and answers only once the patch
succeeds. A retry finds the annotation first and returns that member, whether the
volume is visible there yet or not, so a create whose outcome is unknown is
retried in the member it was sent to and nowhere else. The record lives as long
as the PVC and survives an operator restart. A recorded member that has since
left placement is still returned: the retry succeeds there or keeps failing
there, and is never moved to another member.

**Without a record, the operator looks for the name in every member of the
aggregate**, including members not eligible for placement, through the control
plane's lookup by name in the pool. This covers a volume created before the
record existed. A hit returns that member and is recorded. A lookup that fails in
any member fails the call with `Unavailable` rather than placing, because placing
without knowing is how the duplicate arises.

### 7.4 What a volume knows about its aggregate

Nothing. The handle names the member, and every operation after creation reaches
the member directly. Listing snapshots already walks every cluster in the
secret. This is what lets a member keep working with the supercluster object
deleted, and what keeps the aggregate out of the data path.

### 7.5 QoS shared across members (Phase 5)

The ceiling is the supercluster's from Phase 2 on. What Phase 5 changes is how
it is divided. An even split caps a busy member at a third of a three-member
ceiling while the other two sit idle, so the pool controller reweights the
shares by demand:

1. Every 60 seconds, read each member's I/O rate for the pool from the member's
   `GET /api/v2/clusters/{cluster_id}/iostats`.
2. Give every placement-eligible member a floor of 10% of an even share, so a
   member that is idle now can serve the next volume placed in it.
3. Divide the rest of the ceiling in proportion to each member's rate over the
   last interval.
4. Write the new allocation only when some member's share moved by more than 5%
   of its applied value, so that the shares do not churn the control plane. A
   written allocation is written whole: every member whose share differs is
   written, including the ones that moved by less.

The hysteresis is the allocation's and not a member's, because skipping a small
decrease while writing a large increase overshoots the ceiling: moving from
10k/45k/45k to 11k/44.5k/44.5k IOPS writes the 10% increase, and skipping both
1.1% decreases leaves 101k applied against a ceiling of 100k.

**Decreases are written before increases, and increases wait for every
decrease.** The applied limits sum to the ceiling or less before a round, each
decrease lowers the sum, and the increases bring it to the new allocation, which
sums to the ceiling, so the applied sum never exceeds the ceiling at any point of
the round. A decrease that fails ends the round without sending any increase, and
the next interval computes and writes a fresh allocation. A member's applied
share is reported in the pool's `status.members[]`.

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

The steps follow the planned cutover that `ReplicationSlotReconciler` already
drives (`reconcileCutoverPending` in
`operator/internal/controller/replicationslot_controller.go`): commit, attach the
target's paths on every consuming host while the relationship is
`cutover_pending`, then let the control plane flip ANA states.

```
  action=Relocate received
    │
    ▼
  Validating       ← target eligible, capacity for every volume of the subsystem,
    │                no group membership (§9.3), not encrypted unless P0-1 holds
    │  clean
    ▼
  Replicating      ← POST …/replication/start  mode=migration,
    │                replication_cluster_id=<target>; poll …/replication/status
    │  in sync
    ▼
  Committing       ← POST …/replication/commit  delete_source=true  (write-ahead
    │                status.relocate.committed before the call)
    │  202 or 409, the task recorded in status.relocate.taskID
    ▼
  AttachingPaths   ← relationship cutover_pending; GET …/connect returns the
    │                target's paths beside the source's; one preconnect Job per
    │                consuming node, the Job the replication slot uses
    │  every Job succeeded
    ▼
  Proceeding       ← POST …/replication/cutover-proceed  (write-ahead
    │                status.relocate.proceeded before the call)
    │
    ▼
  Verifying        ← task finished; the relationship names the target as active;
    │                the original handle resolves through it (§8.3); every moved
    │                PV marked relocated-to (§8.3); Jobs deleted
    ▼
  phase: Succeeded
```

| Condition                                           | Step                                      | Result                                                                                                                                                                                                                                                                            |
|-----------------------------------------------------|-------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `abort: true`                                       | Before `Committing`                       | `POST …/replication/stop`, phase `Aborted`                                                                                                                                                                                                                                        |
| `abort: true`                                       | `Committing` or later                     | Ignored with an event: a committed cutover cannot be undone                                                                                                                                                                                                                       |
| Operator restart                                    | Any                                       | Resumes from `status.step`. The commit is resent until a task is recorded, and the proceed while the relationship is `cutover_pending`                                                                                                                                            |
| Start, commit, or proceed returns 5xx or times out  | `Replicating`, `Committing`, `Proceeding` | Retried with backoff, no step advance. A timed-out commit or proceed is retried by the same rule as a restart                                                                                                                                                                     |
| Start returns 4xx                                   | `Replicating`                             | Phase `Failed`, the control plane's message in `status.message`                                                                                                                                                                                                                   |
| A consuming node's Job fails                        | `AttachingPaths`                          | Retried with `PathAttachHeld`, and `cutover-proceed` is not sent. After `REPL_CUTOVER_PROCEED_TIMEOUT_SEC` (120 s) the control plane cuts over alone, and a node without the target's paths reaches it through the relationship on its next stage, with an I/O pause on that node |
| Commit task reports failure                         | `Verifying`                               | Phase `Failed`, with the control plane's relationship state reported                                                                                                                                                                                                              |
| The original handle does not resolve after the move | `Verifying`                               | Phase `Failed` with `HandleUnresolved`                                                                                                                                                                                                                                            |

**`committed` and `proceeded` record intent, and the backend records
acceptance.** Each flag is written before its first call, so a restart knows the
call may have reached the control plane, and `committed` is what refuses an abort
from then on. Neither flag suppresses a call, because a crash between the status
write and the request, or a request that times out, leaves the operator unable to
tell from its own status whether the control plane received it:

- **The commit is resent until a task is recorded.** Every reconcile in
  `Committing` without `status.relocate.taskID` sends the commit again. The
  first commit the control plane accepts returns 202 and its task, and every
  later one returns 409 naming the same task (§11), so a resend never starts a
  second cutover and always ends with the task recorded.
- **The proceed is resent while the relationship is `cutover_pending`.** Every
  reconcile in `Proceeding` reads the relationship's state, sends
  `cutover-proceed` while it is still `cutover_pending`, and advances to
  `Verifying` once it has moved on, whether this proceed released it or
  `REPL_CUTOVER_PROCEED_TIMEOUT_SEC` did. A proceed is never sent to a
  relationship past `cutover_pending`.

`ReplicationSlotReconciler` follows the same rule for the proceed today: it
resends one that failed while the preconnect Job still exists
(`handleExistingPreconnectJob`).

### 8.3 The handle survives the move

A PV's `spec.csi.volumeHandle` is immutable and names the source member. The
control plane keeps the replication relationship record after a commit with
`delete_source`, and the CSI driver already follows it for a failed-over
volume. NodeStage walks it when the source volume is gone
(`redirectToActiveVolume` in `csi-driver/internal/csi/node/stats.go`), the
replication calls resolve through it (`resolveChain` in
`csi-driver/internal/csi/controller/replication.go`), and DeleteVolume removes
the chain's retired members (`deleteRetiredReplicaChain`) but never its active
volume, which Phase 3 changes for a relocated PV (below).

Four controller calls address the handle directly and fail with not-found once
the source is deleted: `ControllerExpandVolume`, `CreateSnapshot`, clone from a
volume, and `ControllerPublishVolume` with its unpublish. Phase 3 routes them
through the same chain resolution, so a relocated volume behaves like a
failed-over one for every call. The resolution moves to `atlas-lib/controlplane`,
because the operator's `Verifying` step asserts the same mapping the driver
follows, and two copies of a chain walk are two answers to where a volume is.

**DeleteVolume deletes the destination only of a PV marked as relocated.**
`deleteRetiredReplicaChain` never deletes the relationship's active volume
through a stale handle, because after a disaster-recovery fail-over a PV in
another Kubernetes cluster carries the same handle and runs on that volume. A
relocated PV's handle is stale in the same way, so without a further signal its
deletion leaks the destination. The relationship cannot supply the signal: its
`mode` is `migration` for a `ReplicationOps` migration between sites as well, so
it does not prove that no second PV names the handle.

`Verifying` writes the annotation `storage.simplyblock.io/relocated-to`, holding
the destination's handle, on every PV the operation moved, through the PV
`patch` the `PersistentVolumeOps` controller already holds. A later relocation
of the same PV overwrites it. DeleteVolume runs while the PV still exists,
because the external-provisioner deletes the PV only after the call succeeds, so
the driver reads the mark from its PV informer
(`csi-driver/internal/kubernetes/manager.go`). When the chain's active volume is
the volume the mark names, the driver deletes it with the retired members. A PV
without the mark, or with a mark naming another volume, keeps the safeguard
unchanged: the failure is a leaked destination, never a deleted volume another
cluster runs on.

**A marked PV resolves from its mark.** Every call that resolves a handle, in the
controller and in the node plugin, starts a marked PV's walk at the handle its
mark names instead of at the PV's own handle, and walks relationships onward from
there as before, so a later fail-over of the destination is still followed. The
node plugin reads the mark through the PV `get` it already holds
(`operator/internal/controllers/driver/rbac.go`). Starting at the mark is what
lets the source member go: once every PV naming it is marked, no call reads the
source's relationship records or its entry in `simplyblock-csi-secret-v2`
(§8.4). DeleteVolume reads the mark before it treats a handle naming an unknown
cluster as already deleted. An unmarked handle resolves from the handle itself,
as today. The mark is local to the Kubernetes cluster holding the PV, so a
disaster-recovery peer's PV carrying the same handle never sees it, which is why
removal refuses a member holding a fail-over relationship (§8.4).

### 8.4 Removing a member (Phase 4)

`StorageClusterOps` gains `RemoveMember`
([Appendix D](#appendix-d-storageclusterops_typesgo)), targeting a member. It is a
long-running operation because the member's volumes move into the rest of the
supercluster before the member goes:

```
  action=RemoveMember received
    │
    ▼
  Validating     ← the target is a member; the other members have room for its
    │              volumes; it holds no encrypted volume (§9.4) and no volume
    │              with a fail-over relationship (§8.3)
    ▼
  Draining       ← the member is out of placement from here on; one
    │              PersistentVolumeOps Relocate per subsystem, creatorRef naming
    │              this operation, at most removeMember.maxConcurrentRelocations
    │              at once, each target chosen by the placement of §7.2
    │  the member's pools report no bound volumes, and every PV whose
    │  handle names the member is marked relocated-to another member
    ▼
  Deleting       ← delete the member StorageCluster, which removes its nodes,
    │              pools, and backend cluster through its own deletion path
    ▼
  phase: Succeeded
```

The member is out of placement for as long as the operation runs, which the
supercluster reads from the member's active operation rather than from its spec.
A child relocation that fails holds the walk with `RemovalHeld` instead of
skipping the volume, so a member is never deleted while it holds a volume.

**A member is deleted only once no handle depends on it.** Deleting the member
deletes its backend cluster, which takes the relationship records with it, and
removes its entry from `simplyblock-csi-secret-v2`. A relocated PV keeps a handle
naming the member, so `Draining` ends only when every PV whose handle names the
member carries a `relocated-to` mark naming another member and the mark resolves
(§8.3). Until then the operation holds with `RemovalHeld`, naming the unmarked
PVs. A volume with a `failover`-mode relationship is refused in `Validating`,
naming the volume, because its disaster-recovery peer's PV in another Kubernetes
cluster resolves from the source member and cannot see the mark.
Aborting during `Draining` stops raising children, lets the running ones finish,
and returns the member to placement. `Deleting` cannot be aborted.

The operation takes the member's `status.activeOpsRef` for its duration, so a
removal and a rolling restart of one member exclude each other as two operations
on a cluster do today. A supercluster whose last member is removed has no
members, reports `Provisioning`, and may be deleted (§4.4).

---

## 9. Feature Treatment

Every feature is in one of four classes. A per-member feature works unchanged,
because the handle names the member. An aggregated feature is extended by this
design to span members. A restricted feature works within one member and is
refused across members. An out-of-scope feature is refused when it names a
supercluster.

| #   | Feature                                         | Treatment    | Behavior in a supercluster                                                                                                                            | Phase |
|-----|-------------------------------------------------|--------------|-------------------------------------------------------------------------------------------------------------------------------------------------------|-------|
| 1   | Volume creation                                 | Aggregated   | Member chosen by §7.2                                                                                                                                 | 2     |
| 2   | `CreateVolume` retry                            | Aggregated   | The member recorded on the PVC, else the name looked up in every member (§7.3)                                                                        | 2     |
| 3   | Volume expansion                                | Per member   | Expands in place. Fails with `ResourceExhausted` when the member is full, and relocation is the remedy                                                | —     |
| 4   | Snapshots                                       | Per member   | Taken in the volume's member. Snapshot names are unique per backend cluster, which is unchanged                                                       | —     |
| 5   | Clone from a volume or restore from a snapshot  | Restricted   | Lands in the source's member regardless of placement (§9.2)                                                                                           | 2     |
| 6   | Volume group snapshot, consistency group        | Restricted   | All volumes of a group share one member and one lvstore. Placement follows the group label (§7.2, §9.3)                                               | 2     |
| 7   | Shared subsystems (`max_namespace_per_subsys`)  | Restricted   | Volumes sharing a subsystem share a member and move together (§8.1)                                                                                   | 3     |
| 8   | Node-to-node migration, rebalancer, realignment | Per member   | Run per member. The supercluster is skipped by the rebalancer's watch                                                                                 | 1     |
| 9   | Pools and QoS                                   | Aggregated   | One backend pool per member. The ceiling is the supercluster's, split evenly, then by demand (§7.1, §7.5)                                             | 2, 5  |
| 10  | DHCHAP and `allowedNodes`                       | Aggregated   | Keys are per backend pool and therefore per member. Every member admits the whole allowed-node list (§7.1)                                            | 2     |
| 11  | Encryption and KMS                              | Restricted   | Members share one KMS configuration through the template. Encrypted volumes are never moved until P0-1 holds, because a move deletes their key (§9.4) | 3     |
| 12  | Backup policies and restore                     | Aggregated   | A policy on a supercluster fans out per member. A restore places by §7.2 (§9.5)                                                                       | 2     |
| 13  | Replication (`ReplicationPair`, CSI-Addons)     | Out of scope | A pair naming a supercluster is refused. Members may be named directly (§16 Q5)                                                                       | —     |
| 14  | `TestFailover`                                  | Per member   | Works from volume handles and is unaffected                                                                                                           | —     |
| 15  | `NFSExport` and pNFS                            | Restricted   | An export's volume is in one member, and the metadata server runs per member. Striping across members is refused                                      | 2     |
| 16  | Dual mode and arbitration                       | Per member   | Arbitration is per cluster and unchanged. A supercluster of Dual members is admitted                                                                  | —     |
| 17  | `StorageClusterOps` (restart, shutdown, expand) | Aggregated   | Restart, shutdown, and start walk the members one at a time. Activate and expand stay per member (§9.6)                                               | 1     |
| 18  | `StorageNodeOps` (drain, remove, restart)       | Per member   | A node is a member's node, unchanged                                                                                                                  | —     |
| 19  | Capacity thresholds and alerts                  | Aggregated   | The supercluster's thresholds apply to the combined capacity, and each member keeps its own                                                           | 1     |
| 20  | Failure domains                                 | Per member   | Indexed per member in the member's `status.failureDomains`                                                                                            | —     |
| 21  | `StorageSiteDeployment`                         | Aggregated   | Mirrors the supercluster's identifier, which the hub's class names in `cluster_id` (§9.7)                                                             | 2     |
| 22  | `StorageClusterMetrics` and Prometheus          | Aggregated   | Member series gain a `supercluster` label (§13)                                                                                                       | 1     |
| 23  | Control-plane upgrade                           | Per member   | One control plane serves every member, and its upgrade is unaffected                                                                                  | —     |

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

### 9.3 Groups need one lvstore

A consistency group's frozen snapshot runs on one logical volume store, so every
volume of the group lives on one lvstore of one node
([`design-consistency-groups.md`](design-consistency-groups.md) §4.2). A
`VolumeGroupSnapshot` and a CSI-Addons volume group resolve to the same backend
group, so the same holds for them. Membership is the PVC label
`storage.simplyblock.io/consistency-group`. The control plane pins the group to
its first volume's node and lvstore and creates every later labeled volume on the
pin, failing the create when the pin has no room (`lvol_controller.py` in sbcli).

**Placement keeps a group in one member.** The group is a record of one backend
cluster, so a labeled volume is placed in the member holding the group (§7.2
step 3), and the member's control plane places it on the pin. A group's first
volume is placed like any other, and its member becomes the group's. The group
is bounded by its lvstore's room, not by the supercluster's: a group whose pin is
full fails the create in its member and is not spilled to another.

**A label added later joins only within the member.** The membership watcher in
the CSI controller resolves the group in the volume's own cluster. A volume in
another member is refused with a Warning event on the PVC naming the member that
holds the group. Relocating the volume into that member first (§8) is the remedy,
after which the existing colocation path moves it onto the pin.

**Relocation refuses a volume that belongs to a group**, because moving one
member of a consistency group off its lvstore breaks the group.

### 9.4 Encryption

Every member carries the template's external KMS configuration, so all members
share one Vault. A moved encrypted volume has to find its data encryption key in
the target member. The control plane does not provide that for replication
today, and a move of an encrypted volume with `delete_source` loses the volume's
data permanently:

1. Replication copies the volume's blocks, which are ciphertext, into a landing
   volume created without a crypto layer (`add_lvol_ha` in
   `services/snapshot_replication.py`, with no `crypto_key`). The target holds
   the data, unreadable.
2. The key lives under `cluster/{cluster_id}/lvol/{lvol_id}`, a path naming the
   source cluster and the source volume.
3. `delete_source` deletes the source volume through `delete_lvol`, which deletes
   that key (`simplyblock_core/controllers/lvol_controller.py`, the
   `crypto_bdev` branch at the end of `delete_lvol`).

After the third step the ciphertext on the target has no key anywhere. P0-1 is
what closes this: the target volume created with a crypto layer, and the key
carried to the target's path before the source is deleted, for which the
control plane's `rekey_data_encryption_keys` and `import_data_encryption_keys`
(`simplyblock_core/kms`) are the building blocks.

**Relocation refuses encrypted volumes at admission, and refuses them again in
`Validating`.** Admission refuses a `Relocate` of a volume whose class enables
encryption with `EncryptedVolumeNotMovable`. `Validating` reads the volume's
`crypto_bdev` from the control plane and fails the operation before
`Replicating` if it is set, because a class is not the only way a volume becomes
encrypted and the cost of a missed refusal is the volume. `RemoveMember` refuses
a member holding an encrypted volume in its own `Validating`, and its children
inherit both checks, so a member holding encrypted volumes is never deleted.

### 9.5 Backups

A `StorageBackupPolicy` naming a supercluster fans out into one backend policy
per member, recorded per member in the policy's status. Backup identifiers are
not scoped by cluster in the control plane, so a backup taken in one member can
be restored into another, and a restore through a supercluster class places the
new volume by §7.2. A backup store is one per cluster, and the template writes
the same store to every member.

### 9.6 Cluster operations

`RollingRestart`, `Restart`, `Shutdown`, and `Start` on a supercluster walk its
members one at a time, in order of member name, as one operation
per member that the supercluster's operation raises and waits on. Each member's
operation takes that member's lock, so a member already running an operation of
its own holds the walk until it finishes. A member's operation that fails holds
the walk with `FanOutHeld`, and the remaining members are not touched, because a
restart that keeps going after a member failed to come back takes the next member
down beside it. Progress is in the operation's `status.fanOut`
([Appendix D](#appendix-d-storageclusterops_typesgo)).

`Activate`, `Expand`, and `CancelTask` are refused on a supercluster, because
each names something one cluster has. `RemoveMember` targets a member (§8.4).

### 9.7 StorageSiteDeployment

A site deployment mirrors one `StorageCluster` and its UUID back to the hub,
because a StorageClass on the hub side names `cluster_id` and `pool_name`. A
supercluster's `status.uuid` is its aggregate identifier, so the mirror carries
it unchanged and the hub's class names the supercluster in `cluster_id`. The
placement for that class is answered by the site's operator, which owns the
supercluster, so the class provisions on the site as a cluster's class does
([`design-management-hub.md`](design-management-hub.md)).

---

## 10. Controller Design

### 10.1 Location and triggers

**`SuperclusterReconciler`** lives in
`operator/internal/controllers/cluster/supercluster_controller.go`. It
reconciles `StorageCluster` objects carrying `spec.supercluster`, and
`StorageClusterReconciler` gains the inverse predicate, so each object has
exactly one reconciler. It watches member `StorageCluster` objects and maps each
to the supercluster its `spec.superclusterRef` names, and it polls capacity
every 60 seconds.

Each reconcile:

1. Ensures the shared Services and serving certificate (§6.2), and reports them
   ready, which is what a new member waits for (§5.4).
2. Lists the members through the field index on `spec.superclusterRef`, and reads
   each member's phase and maximum fault tolerance from its status, and its
   capacity from the control plane.
3. Records each member's region and zones, read from the `topology.kubernetes.io`
   labels of the `Node` objects its storage nodes run on.
4. Decides placement eligibility per member: not `disablePlacement`, no
   `RemoveMember` running on it, phase serving, and holding every pool of the
   supercluster.
5. Hands the readings to `PlacementService`, which answers from them (§7.2).
6. Writes status with an optimistic-lock patch and `observedGeneration`.

**`PlacementService`** lives in `operator/internal/placement`, registered on the
operator's end of every CSI controller link by `operator/internal/csilink`. It
keeps, per member, the sizes it has placed since the member's last reading,
which the next reading clears.

**`StorageClusterReconciler`** writes `status.role` for every ordinary cluster,
records the workload names in `status.workloadNames` by §6.1 before it creates
the workload, and for a member labels its pods into the
supercluster's Services and mounts the supercluster's certificate (§6.2).

**`StoragePoolReconciler`** branches on whether the pool's cluster is a
supercluster and, if so, creates and updates one backend pool per member. A new
member receives every pool before the supercluster calls it placement-eligible.

### 10.2 Concurrency and mutual exclusion

The supercluster's reconcile writes nothing to the control plane. A fan-out
operation (§9.6) takes the supercluster's `status.activeOpsRef` for its duration,
so two walks never interleave, and takes each member's lock only while that
member's operation runs. `RemoveMember` takes the member's lock for its duration.
Placements for one supercluster are serialized in the operator, so two concurrent
`CreateVolume` calls each see the size the other placed.

### 10.3 Interaction with existing controllers

| Controller              | Interaction                                                                         |
|-------------------------|-------------------------------------------------------------------------------------|
| Volume rebalancer       | Predicate excludes superclusters. Members are rebalanced as ordinary clusters       |
| Arbitration (`twonode`) | Predicate excludes superclusters, whose `status.uuid` names no backend cluster      |
| Backup policy           | Fans out per member (§9.5)                                                          |
| Deployment expansion    | Creates the supercluster and its members (§5.4), and new members on growth (§5.6)   |
| `PersistentVolumeOps`   | `Relocate` reads the supercluster from the volume's member's `spec.superclusterRef` |

### 10.4 RBAC

No new verbs. Every resource the new controller reads or writes
(`storageclusters`, `storageclusters/status`, `storagepools`, `nodes` for their
topology labels, `persistentvolumeclaims` for the placement record of §7.3,
`persistentvolumes` for the relocation mark of §8.3, and
the Services, Certificates, and Secrets of §6.2, which the workload controller
already creates) is already granted to the manager. The PVC `patch` verb comes
from `persistentvolumeclaim_controller.go`, and the PV `patch` verb from
`persistentvolumeops_controller.go`.

---

## 11. Backend API Requirements

| Method | Endpoint                                                                                      | Notes                                                                                                                                                                                                                              |
|--------|-----------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| GET    | `/api/v2/clusters/`                                                                           | Exists. Used to check that every member is on the same control plane                                                                                                                                                               |
| GET    | `/api/v2/clusters/{cluster_id}/capacity`                                                      | Exists. Polled by the supercluster controller and read by the CSI driver at `CreateVolume`                                                                                                                                         |
| POST   | `/api/v2/clusters/{cluster_id}/storage-pools/`                                                | Exists. Called once per member per pool. Idempotent by name: a 409 on an existing pool is success                                                                                                                                  |
| POST   | `/api/v2/clusters/{cluster_id}/storage-pools/{pool_id}/volumes/{volume_id}/replication/start` | Exists. Body `replication_cluster_id`, `mode: migration`. A repeat on a running relationship must not start a second                                                                                                               |
| GET    | `…/volumes/{volume_id}/replication/status`                                                    | Exists. Polled in `Replicating`                                                                                                                                                                                                    |
| POST   | `…/volumes/{volume_id}/replication/stop`                                                      | Exists. Abort before commit                                                                                                                                                                                                        |
| POST   | `…/volumes/{volume_id}/replication/commit`                                                    | Exists. Body `delete_source: true`. Returns 202 and a task location. Must refuse a second commit with 409 naming the running task, rather than run it twice                                                                        |
| GET    | `/api/v2/clusters/{cluster_id}/tasks/{task_id}`                                               | Exists. Polled in `Verifying`                                                                                                                                                                                                      |
| GET    | `/api/v2/clusters/{cluster_id}/replication/relationships/{lvol_id}`                           | Exists. Maps a source volume to its active volume after a commit with `delete_source`. Read by the CSI driver's chain resolution and by `Verifying`                                                                                |
| GET    | `…/volumes/{volume_id}/connect`                                                               | Exists. While the relationship is `cutover_pending`, returns the target's paths beside the source's. Read in `AttachingPaths`                                                                                                      |
| POST   | `…/volumes/{volume_id}/replication/cutover-proceed`                                           | Exists. Releases the cutover once every consuming host holds the target's paths. The control plane waits for it up to `REPL_CUTOVER_PROCEED_TIMEOUT_SEC` (120 s). Resent while the relationship is `cutover_pending`               |
| —      | Key carried with an encrypted volume                                                          | **Not established (P0-1).** No key handling is defined for replication, and `delete_source` deletes the source's key (§9.4, §16 Q10)                                                                                               |
| POST   | `/api/v2/clusters/`                                                                           | **Not established (P0-4).** Body gains the workload names of §6.1: the storage-node API and SPDK proxy Services, the SPDK proxy certificate Secret, and the node label suffix. Absent fields mean the constant names and no suffix |

The CSI driver authenticates to each member with that member's entry in
`clusters`, as today. No admin token crosses a cluster boundary.

---

## 12. Failure Modes

| Failure                                                           | Detection                                    | Behavior                                                                                                                                                                               |
|-------------------------------------------------------------------|----------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| A member is unavailable                                           | Member phase                                 | Supercluster `Degraded`. The member is not placement-eligible. Its volumes are unavailable, others unaffected                                                                          |
| Every member is unavailable                                       | Member phases                                | Supercluster `Unavailable`. `CreateVolume` fails with `ResourceExhausted` naming each reason                                                                                           |
| A member's capacity reading cannot be refreshed at `CreateVolume` | Two-second deadline                          | Member dropped for this call, logged. Other members still chosen                                                                                                                       |
| Every eligible member is full                                     | Capacity filter                              | `ResourceExhausted`, with the supercluster's `AggregateCapacityExhausted` event                                                                                                        |
| A pool fails to create in one member                              | Backend error                                | That member stays placement-ineligible, pool status names the member, retried                                                                                                          |
| The operator's link is down at `CreateVolume`                     | `Unavailable` from the link                  | The call fails with `Unavailable` and the external-provisioner retries. No volume is placed                                                                                            |
| A create times out in a member and is still running               | PVC placement record                         | The retry is sent to the recorded member and never placed elsewhere. One backend volume results (§7.3)                                                                                 |
| The supercluster object is deleted                                | Admission                                    | Refused while it has members (§4.4)                                                                                                                                                    |
| The supercluster object is recreated, by a restore or by hand     | New `metadata.uid`                           | A new aggregate identifier. Classes naming the old one fail new provisions with `InvalidArgument` until rewritten. Existing volumes are unaffected, because their handles name members |
| Operator restart mid-expansion                                    | Persisted step                               | Members and supercluster created by name, idempotent (§5.4)                                                                                                                            |
| Operator restart mid-relocation                                   | Persisted step, `taskID`, relationship state | Resumes without a second start or cutover: the commit is resent until a task is recorded (§8.2)                                                                                        |

---

## 13. Observability

Both tables extend surfaces that exist: the StorageCluster controller already
emits events on the cluster object, and the operator's registry already exports
per-cluster metrics labeled `cluster`.

### 13.1 Kubernetes events

Aggregate events land on the supercluster `StorageCluster`, which is the object
an administrator reads and the one that outlives any member. Relocation events
land on the `PersistentVolumeOps`, which is the audit record of the move.

| Event                                                                                | Type    | Reason                       | On                    |
|--------------------------------------------------------------------------------------|---------|------------------------------|-----------------------|
| A member left placement because it is not serving, lacks a pool, or is cordoned      | Normal  | `MemberPlacementChanged`     | Supercluster          |
| A member returned to placement                                                       | Normal  | `MemberPlacementChanged`     | Supercluster          |
| The combined capacity crossed the warning or critical threshold                      | Warning | `AggregateCapacityThreshold` | Supercluster          |
| No member has room for a requested volume                                            | Warning | `AggregateCapacityExhausted` | Supercluster          |
| A member joined the supercluster                                                     | Normal  | `MemberAdded`                | Supercluster          |
| A member was removed after its volumes moved into the others                         | Normal  | `MemberRemoved`              | Supercluster          |
| An abort was ignored because the cutover was already committed                       | Warning | `RelocationAbortIgnored`     | `PersistentVolumeOps` |
| A relocation is holding because a consuming node could not attach the target's paths | Warning | `PathAttachHeld`             | `PersistentVolumeOps` |
| The relocated volume's original handle does not resolve after the cutover            | Warning | `HandleUnresolved`           | `PersistentVolumeOps` |
| A member removal is holding because a child relocation failed                        | Warning | `RemovalHeld`                | `StorageClusterOps`   |
| A fan-out operation is holding because a member's operation failed                   | Warning | `FanOutHeld`                 | `StorageClusterOps`   |

`AggregateCapacityExhausted` is raised by the operator's placement service when
it answers `ResourceExhausted`, rate-limited to one per minute per supercluster.

### 13.2 Prometheus metrics

| Metric                                                | Labels                              | Description                                                            |
|-------------------------------------------------------|-------------------------------------|------------------------------------------------------------------------|
| `simplyblock_supercluster_members`                    | `supercluster`, `state`             | Gauge of members by `eligible`, `cordoned`, `notServing`, `missing`    |
| `simplyblock_supercluster_capacity_bytes`             | `supercluster`, `kind`              | Gauge of combined `total`, `used`, and `provisioned` bytes             |
| `simplyblock_supercluster_placement_decisions_total`  | `supercluster`, `cluster`, `result` | Counter of `CreateVolume` placements by chosen member, or `exhausted`  |
| `simplyblock_supercluster_placement_duration_seconds` | `supercluster`                      | Histogram of member selection time, lookup and capacity reads included |
| `simplyblock_relocation_operations_total`             | `supercluster`, `result`            | Counter of `Relocate` operations by `succeeded`, `failed`, `aborted`   |
| `simplyblock_relocation_duration_seconds`             | `supercluster`                      | Histogram from `Replicating` to `Succeeded`                            |

Existing per-cluster series gain a `supercluster` label on members, empty on
standalone clusters, so a dashboard groups a supercluster's members with one
`sum by (supercluster)`.

**Three of these are load-bearing.** `simplyblock_supercluster_placement_decisions_total`
by `cluster` is how a skewed strategy is seen: a member receiving every new
volume while another sits empty is a strategy or a capacity-reading problem, and
nothing else shows it. `simplyblock_supercluster_placement_duration_seconds` is the alert
for a slow member, because the name lookup in every member sits on the
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
  (§5.7), per-node-set overrides (§5.8), the workload naming rule (§6.1), the
  member ranking with every filter and tie-break (§7.2), the ceiling's shares
  (§7.1, §7.5), and the chain resolution of the four controller calls (§8.3),
  each against fixtures, plus the webhook rules of §4.4 and §6.2 with a fake
  client.
- **Integration:** the expansion of a multi-node-set document into members and a
  supercluster, growth adding a member, pool fan-out, and the placement service,
  against `envtest` and a mock control plane. The `Relocate` step machine with
  every failure row of §8.2, including restart in each step, and the fan-out
  walk of §9.6.
- **E2E:** a three-member supercluster on a live control plane, volumes spread by
  a supercluster class, `CreateVolume` retried after an injected timeout without
  a duplicate, and one relocation under fio in verify mode.

The risk concentrates in §7.3 and §8.2. A duplicate volume from a retry is silent
capacity loss, and a relocation that commits twice or leaves a handle dangling is
data unavailability, so neither group's scenarios may be cut.

---

## 15. Migration and Compatibility

**An existing cluster keeps its workload names.** A cluster without
`status.workloadNames` is recorded with the constant names, and an sbcli cluster
record without them resolves the same (§6.1), so no running cluster's node
hostnames change. A new standalone cluster takes the constant names when no other
cluster in its namespace has them, and the rule refusing a second cluster in a
namespace is removed. `TestASecondClusterInOneNamespaceIsRefused` in
`operator/internal/webhook/storagecluster_validator_test.go` asserts the removed
rule and is rewritten to admit the second cluster.

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
(admission refuses it), fanned out by this design (pools, backup policies,
cluster operations), or refused with a typed `ErrAggregateCluster` from the
resolver. A supercluster's `status.uuid` is an identifier the control plane does
not know, so the resolver change is what makes an unaudited site fail with a
named error rather than a 404 from the control plane.

**Spell-check terms.** `supercluster` and `superclusters` are new terms for the
repository's spell check.

---

## 16. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | Owner                       |
|-----|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------|
| 1   | ~~**A control-plane aggregate**~~ **Resolved.** The aggregate stays out of the control plane. It is orchestration in the operator and the CSI driver, and the control plane learns of it only when a requirement needs it to. None of this design's mechanisms does (§2, Non-Goals).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               | Resolved                    |
| 2   | ~~**Per-member layout overrides**~~ **Resolved.** A node set overrides the template's sizing and erasure-coding scheme for its member, which is what makes a mixed supercluster of NVMe and logical block members possible. Groups within a node set stay in one hardware category (§5.8).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | Resolved                    |
| 3   | ~~**Member names for long node set names**~~ **Resolved.** A cluster name is at most 35 characters and a node set name at most 23, and the member suffix is three random letters, so every member name fits (§5.3).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                | Resolved                    |
| 4   | ~~**Aggregate pool ceilings**~~ **Resolved.** A pool's ceiling is the supercluster's. Phase 2 splits it evenly over the members and Phase 5 divides it by demand (§7.1, §7.5).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     | Resolved                    |
| 5   | **Replication with a supercluster on either side:** a pair whose source is a supercluster means one relationship per member, and a supercluster target needs a member mapping. Which shape, and is it needed in the first release?                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | Operator team, Backend team |
| 6   | ~~**Placement affinity for groups**~~ **Resolved.** A group is the PVC label `storage.simplyblock.io/consistency-group`, which the control plane already pins to one node and lvstore. Placement sends a labeled volume to the member that holds the group, and the member's control plane places it on the pin (§7.2, §9.3).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | Resolved                    |
| 7   | ~~**Fan-out cluster operations**~~ **Resolved.** Restart, shutdown, and start on a supercluster walk its members one at a time in order of member name and hold on a member that fails (§9.6).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     | Resolved                    |
| 8   | ~~**Site deployments**~~ **Resolved.** The hub's StorageClass names the supercluster in `cluster_id`, through the supercluster's `status.uuid` (§9.7).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             | Resolved                    |
| 9   | ~~**Volume names across members**~~ **Resolved.** Not relevant: volumes are created through Kubernetes, whose PV names are unique.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | Resolved                    |
| 10  | **Encryption across a cross-cluster move:** replication lands an encrypted volume's ciphertext in a target volume without a crypto layer, and `delete_source` deletes the only key, so the data is permanently lost (§9.4). The same path is reachable through replication's own `commit` with `delete_source` and through failover of encrypted volumes, outside this design. What carries the key to the target (P0-1), and is the existing replication path guarded against it?                                                                                                                                                                                                                                                                                                                                                 | Backend team                |
| 11  | **Port separation for SPDK pods on a shared worker:** SPDK pods keep host networking, and each cluster hands out NVMe-oF, RPC, and firewall ports by counting up from its own `nvmfBasePort`, `rpcBasePort`, and `snodeApiPort` (`get_next_nvmf_port`, `get_next_lvstore_ports`, and `get_next_fw_port` in sbcli), skipping only the ports that cluster uses. Members sharing a worker (§5.1) therefore need disjoint ports. The candidates are three. The operator assigns each member a port range, which needs an upper bound the control plane does not enforce today. The operator gives each member distinct base ports with a fixed spacing, which needs no control-plane change and holds while no member counts past the spacing. The control plane allocates ports per host across clusters. Which, and with what width? | Operator team, Backend team |

---

## Appendix A: `storagecluster_types.go`

The additions to `StorageCluster`, in the order they land in the file. The
existing fields are specified by
[`design-storagecluster.md`](crd-redesign/design-storagecluster.md) Appendix A,
which this appendix does not restate.

```go
// PlacementStrategy is how the operator ranks the members of a supercluster
// when it places a new volume.
// +kubebuilder:validation:Enum=MostAvailableCapacity;LeastProvisioned;LeastVolumes
type PlacementStrategy string

const (
	// PlacementMostAvailableCapacity places a volume in the member with the most
	// free physical capacity.
	PlacementMostAvailableCapacity PlacementStrategy = "MostAvailableCapacity"

	// PlacementLeastProvisioned places a volume in the member with the lowest
	// ratio of provisioned to physical capacity.
	PlacementLeastProvisioned PlacementStrategy = "LeastProvisioned"

	// PlacementLeastVolumes places a volume in the member holding the fewest
	// volumes.
	PlacementLeastVolumes PlacementStrategy = "LeastVolumes"
)

// StorageClusterRole is what a StorageCluster is within a supercluster.
// +kubebuilder:validation:Enum=Standalone;Supercluster;PlacementGroup
type StorageClusterRole string

const (
	// StorageClusterRoleStandalone is an ordinary cluster that belongs to no
	// supercluster.
	StorageClusterRoleStandalone StorageClusterRole = "Standalone"

	// StorageClusterRoleSupercluster is a cluster made of member clusters, with
	// no nodes of its own.
	StorageClusterRoleSupercluster StorageClusterRole = "Supercluster"

	// StorageClusterRolePlacementGroup is an ordinary cluster that belongs to
	// a supercluster, one of the groups its volumes are placed in.
	StorageClusterRolePlacementGroup StorageClusterRole = "PlacementGroup"
)

// SuperclusterPlacement is how a supercluster places new volumes.
type SuperclusterPlacement struct {
	// Strategy ranks the members that can take a new volume. Defaults to
	// MostAvailableCapacity.
	// +kubebuilder:default=MostAvailableCapacity
	// +optional
	Strategy PlacementStrategy `json:"strategy,omitempty"`
}

// SuperclusterSpec makes a StorageCluster a supercluster: one cluster to
// Kubernetes, made of the member clusters whose superclusterRef names it.
type SuperclusterSpec struct {
	// Placement is how new volumes are placed among the members.
	// +optional
	Placement *SuperclusterPlacement `json:"placement,omitempty"`
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

// WorkloadNames are the names a cluster's storage-node workload is reached by.
// They are recorded once, before the workload or the backend cluster exists,
// and never changed, because a node's hostname is built from them.
type WorkloadNames struct {
	// NodeAPIService is the headless Service of the storage-node API.
	// +kubebuilder:validation:MaxLength=63
	NodeAPIService string `json:"nodeAPIService"`

	// NodeAPITLSSecret is the storage-node API's serving-certificate Secret.
	// +kubebuilder:validation:MaxLength=253
	NodeAPITLSSecret string `json:"nodeAPITLSSecret"`

	// SpdkProxyService is the headless Service of the SPDK proxy.
	// +kubebuilder:validation:MaxLength=63
	SpdkProxyService string `json:"spdkProxyService"`

	// SpdkProxyTLSSecret is the SPDK proxy's serving-certificate Secret.
	// +kubebuilder:validation:MaxLength=253
	SpdkProxyTLSSecret string `json:"spdkProxyTLSSecret"`

	// NodeLabelSuffix is appended to a node's hostname label in both
	// Services. A member carries its own three letters, so that members on
	// one worker have distinct hostnames. Empty for every other cluster.
	// +kubebuilder:validation:MaxLength=4
	// +optional
	NodeLabelSuffix string `json:"nodeLabelSuffix,omitempty"`
}

// SuperclusterMemberStatus is one member as the supercluster last read it.
type SuperclusterMemberStatus struct {
	// Name is the member StorageCluster.
	Name string `json:"name"`

	// UUID is the member's backend cluster UUID, empty until it is created.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// Phase is the member's own phase.
	// +optional
	Phase StorageClusterPhase `json:"phase,omitempty"`

	// PlacementEligible says whether new volumes may be placed in this member:
	// placement not disabled, not being removed, serving, and holding every pool
	// of the supercluster.
	// +optional
	PlacementEligible bool `json:"placementEligible,omitempty"`

	// Capacity is the member's own capacity reading.
	// +optional
	Capacity *AggregateCapacity `json:"capacity,omitempty"`
}
```

The fields added to `StorageClusterSpec`:

```go
	// Supercluster makes this cluster a supercluster: one cluster to
	// Kubernetes, made of the StorageClusters in this namespace whose
	// superclusterRef names it, each with its own nodes and data. A new volume
	// is placed in one member and stays there unless it is moved. A supercluster
	// carries no nodes and no sizing, and may carry only this field and the
	// capacity thresholds. Set at creation and never added or removed.
	// +optional
	Supercluster *SuperclusterSpec `json:"supercluster,omitempty"`

	// SuperclusterRef makes this cluster a member of the named supercluster in
	// this namespace. Set at creation and never changed: a member leaves its
	// supercluster through a RemoveMember operation, which moves its volumes
	// into the other members and deletes it.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	SuperclusterRef string `json:"superclusterRef,omitempty"`

	// DisablePlacement stops new volumes being placed in this member. Volumes
	// already in it stay where they are and keep working. Valid only on a
	// member.
	// +optional
	DisablePlacement *bool `json:"disablePlacement,omitempty"`
```

The CEL rules added to `StorageClusterSpec`:

```go
// +kubebuilder:validation:XValidation:rule="has(self.supercluster) || (has(self.maxSubsystemCount) && has(self.vcpuCount))",message="maxSubsystemCount and vcpuCount are required unless the cluster is a supercluster"
// +kubebuilder:validation:XValidation:rule="has(self.supercluster) == has(oldSelf.supercluster)",message="a cluster cannot become a supercluster, or stop being one, after creation"
// +kubebuilder:validation:XValidation:rule="has(self.superclusterRef) == has(oldSelf.superclusterRef) && (!has(self.superclusterRef) || self.superclusterRef == oldSelf.superclusterRef)",message="superclusterRef is set at creation and never changed"
// +kubebuilder:validation:XValidation:rule="!(has(self.supercluster) && has(self.superclusterRef))",message="a supercluster cannot be a member of another"
// +kubebuilder:validation:XValidation:rule="has(self.superclusterRef) || !has(self.disablePlacement)",message="disablePlacement is valid only on a member"
```

The fields added to `StorageClusterStatus`:

```go
	// UUID, an existing field, is the backend cluster UUID on an ordinary
	// cluster. On a supercluster it is the aggregate's identifier, the object's
	// metadata.uid, which a StorageClass names in cluster_id and the control
	// plane does not know.

	// Role is what this cluster is within a supercluster.
	// +optional
	Role StorageClusterRole `json:"role,omitempty"`

	// WorkloadNames are the names this cluster's workload is reached by,
	// recorded before the workload exists. A cluster without them predates
	// the record and uses the constant names.
	// +optional
	WorkloadNames *WorkloadNames `json:"workloadNames,omitempty"`

	// Members is each member as last read, on a supercluster only.
	// +optional
	// +listType=map
	// +listMapKey=name
	Members []SuperclusterMemberStatus `json:"members,omitempty"`

	// MemberCount is the number of members, on a supercluster only.
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

	// Limits are the share of the pool's ceiling this member's backend pool
	// currently enforces. The shares of all members sum to the pool's limits.
	// +optional
	Limits *PoolLimitsStatus `json:"limits,omitempty"`
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

// +kubebuilder:validation:Enum=Validating;Migrating;Verifying;Replicating;Committing;AttachingPaths;Proceeding
type PersistentVolumeOpsStep string

const (
	PersistentVolumeOpsStepReplicating    PersistentVolumeOpsStep = "Replicating"
	PersistentVolumeOpsStepCommitting     PersistentVolumeOpsStep = "Committing"
	PersistentVolumeOpsStepAttachingPaths PersistentVolumeOpsStep = "AttachingPaths"
	PersistentVolumeOpsStepProceeding     PersistentVolumeOpsStep = "Proceeding"
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

	// Committed is set before the commit is first requested. It refuses an
	// abort from then on, and does not stop the commit being resent until
	// TaskID is recorded.
	// +optional
	Committed bool `json:"committed,omitempty"`

	// Proceeded is set before the cutover is first released. The release is
	// resent while the relationship is cutover_pending.
	// +optional
	Proceeded bool `json:"proceeded,omitempty"`

	// TaskID is the control plane's task carrying the cutover, from the
	// commit's 202 or from the 409 that refuses a second commit.
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
// +kubebuilder:validation:Enum=Activate;Expand;Shutdown;Start;Restart;RollingRestart;CancelTask;RemoveMember
type StorageClusterOpsAction string

const (
	// StorageClusterOpsActionRemoveMember moves every volume out of a member of a
	// supercluster into the other members, then deletes the member.
	StorageClusterOpsActionRemoveMember StorageClusterOpsAction = "RemoveMember"
)

// RemoveMemberSpec bounds a member's removal.
type RemoveMemberSpec struct {
	// MaxConcurrentRelocations is how many volume relocations run at once.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	// +kubebuilder:default=1
	// +optional
	MaxConcurrentRelocations *int32 `json:"maxConcurrentRelocations,omitempty"`
}

// FanOutStatus is the progress of an operation on a supercluster that runs as
// one operation per member, one member at a time.
type FanOutStatus struct {
	// Current names the member whose operation is running.
	// +optional
	Current string `json:"current,omitempty"`

	// CurrentOpsRef names the StorageClusterOps raised on the current member.
	// +optional
	CurrentOpsRef string `json:"currentOpsRef,omitempty"`

	// Completed are the members whose operation succeeded, in walk order.
	// +optional
	// +listType=atomic
	Completed []string `json:"completed,omitempty"`
}
```

The field added to `StorageClusterOpsSpec`, beside `cancelTask` and
`rollingRestart`:

```go
	// RemoveMember parameterizes action RemoveMember and is ignored by the
	// others.
	// +optional
	RemoveMember *RemoveMemberSpec `json:"removeMember,omitempty"`
```

The field added to `StorageClusterOpsStatus`:

```go
	// FanOut is the walk over a supercluster's members, on an operation that
	// targets a supercluster.
	// +optional
	FanOut *FanOutStatus `json:"fanOut,omitempty"`
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

---

## Appendix F: `clusterdeploymentconfig_types.go`

The additions to `ClusterDeploymentConfig`. The rest of the kind is
[`design-clusterdeploymentconfig.md`](crd-redesign/design-clusterdeploymentconfig.md)
Appendix A.

```go
// NodeSetClusterOverrides is the sizing a node set's member takes in place of
// the document's cluster template. An unset field inherits the template.
type NodeSetClusterOverrides struct {
	// VCPUCount is the number of vCPUs allocated to SPDK on each storage node of
	// this member.
	// +kubebuilder:validation:Minimum=4
	// +optional
	VCPUCount *int32 `json:"vcpuCount,omitempty"`

	// MaxSubsystemCount is the maximum number of NVMe-oF subsystems each storage
	// node of this member serves.
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=75
	// +optional
	MaxSubsystemCount *int32 `json:"maxSubsystemCount,omitempty"`

	// MinHugePagesSize is the minimum huge-page memory each storage node of this
	// member reserves.
	// +kubebuilder:validation:MaxLength=32
	// +optional
	MinHugePagesSize string `json:"minHugePagesSize,omitempty"`

	// SocketsToUse are the CPU sockets that run storage nodes on this member's
	// workers.
	// +kubebuilder:validation:items:MaxLength=8
	// +kubebuilder:validation:MaxItems=16
	// +optional
	SocketsToUse []string `json:"socketsToUse,omitempty"`

	// NodesPerSocket is how many storage nodes run on each socket.
	// +kubebuilder:validation:Minimum=1
	// +optional
	NodesPerSocket *int32 `json:"nodesPerSocket,omitempty"`

	// Stripe is this member's erasure-coding scheme.
	// +optional
	Stripe *StripeSpec `json:"stripe,omitempty"`
}
```

The fields changed and added on `NodeSet`:

```go
	// Name is the node set's name, and the middle of the member cluster's name
	// when the node set becomes a member, as <cluster>-<nodeset>-<suffix>. At
	// most 23 characters, so that a member name fits beside a cluster name of 35
	// and a suffix of three.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=23
	Name string `json:"name"`

	// Cluster overrides the document's cluster template for this node set's
	// member, for node sets whose hardware differs from the rest. Valid on a
	// document with more than one node set, and on one adding members to a
	// supercluster.
	// +optional
	Cluster *NodeSetClusterOverrides `json:"cluster,omitempty"`
```

The field changed on `ClusterTemplate`, and the rules changed on
`ClusterDeploymentConfigSpec`:

```go
	// Name is the StorageCluster's name, or the supercluster's when the document
	// has more than one node set. At most 35 characters, so that every member
	// name derived from it fits.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=35
	Name string `json:"name"`
```

```go
// +kubebuilder:validation:XValidation:rule="self.nodeSets.all(s, s.groups.all(g, !has(g.devices) || !has(g.devices.block)) || s.groups.all(g, !has(g.devices) || !has(g.devices.nvme)))",message="every group of a node set must name the same device class: all nvme or all block"
// +kubebuilder:validation:XValidation:rule="size(self.nodeSets) > 1 || has(self.clusterRef) || self.nodeSets.all(s, !has(s.cluster))",message="a node set overrides the cluster template only when the document has more than one node set or adds members to a supercluster"
```

The fields added to `ClusterDeploymentConfigStatus`, and the field whose comment
changes on `ClusterDeploymentConfigSpec`:

```go
// ExpansionMember is a member cluster the expansion drew a name for, recorded
// before the member is created so that a retry creates the same one.
type ExpansionMember struct {
	// NodeSet is the node set the member is made of.
	NodeSet string `json:"nodeSet"`

	// Cluster is the member StorageCluster's name, <cluster>-<nodeset>-<suffix>.
	Cluster string `json:"cluster"`
}
```

```go
	// Members are the member clusters the expansion created or is creating, one
	// per node set, when the document produces or grows a supercluster.
	// +optional
	// +listType=map
	// +listMapKey=nodeSet
	Members []ExpansionMember `json:"members,omitempty"`
```

```go
	// Cluster is the StorageCluster to create, or the sizing of the members to
	// add when ClusterRef names a supercluster. Ignored when ClusterRef names a
	// standalone cluster or a member.
	// +optional
	Cluster *ClusterTemplate `json:"cluster,omitempty"`
```

---

## Appendix G: Follow-up ideas

Directions a supercluster opens, recorded as ideas. None of them is specified,
scheduled, or required by the phases above.

| #    | Idea                                                                                                                                                                                                                                                                                                                                                                                                                    | Builds on                                          | Open before it is designed                                                                                                                                                                                                         |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| G-1  | **Stripes across members:** A striped volume places each stripe leg in a different member, a stripe of three over three members, so one volume's load spreads over three clusters' nodes. The CSI node side already assembles an LVM stripe over several namespace connections (`plans.Striped` in `atlas-lib/volstack/plans`), so the legs need not share a cluster                                                    | §7.2 placement per leg, volstack's `members` layer | The volume is lost to any one member's outage, so its availability is the product of its members'. A snapshot of the volume is consistent only if every leg is frozen together, which needs a group snapshot across clusters (G-8) |
| G-2  | **Mixed superclusters by device class:** Logical-block nodes form one member and NVMe nodes another, which lifts the one-class-per-cluster limit at the supercluster level. §5.8 already permits it                                                                                                                                                                                                                     | §5.8 per-node-set overrides, `DeviceClassOf`       | Placement does not read device class today, so a class must name the members it may use (G-3)                                                                                                                                      |
| G-3  | **Class-aware placement:** A StorageClass restricts its candidates to members of a device class, a member label, or a named set, so a mixed supercluster serves an NVMe class and a logical-block class                                                                                                                                                                                                                 | §7.2 step 4                                        | Whether the restriction is a class parameter or a selector on member labels                                                                                                                                                        |
| G-4  | **Balancing by relocation:** A background controller moves volumes between members when their fill or load diverges past a threshold, beyond what placement at creation achieves                                                                                                                                                                                                                                        | §8 `Relocate`, §7.2 readings                       | Load signal (capacity, IOPS, latency), move budget, and the interaction with the rebalancer inside each member                                                                                                                     |
| G-5  | **Tiering in a mixed supercluster:** Extents of one volume move between an NVMe member and a logical-block member by access temperature. The host holds an LVM volume group over a namespace from each member, `pvmove` moves extents or `lvmcache` promotes hot blocks, and `dmstats` measures temperature per region. Moving a whole volume with `Relocate` applies only to a dormant one, such as an unused VM image | G-2, §8, volstack's LVM layers                     | What measures temperature, `dmstats` on the host or the control plane's I/O statistics. A tiered volume shares G-1's availability and snapshot caveats, because it spans two members                                               |
| G-6  | **Subsystem and namespace headroom:** A supercluster offers the sum of its members' subsystems and namespaces, so the per-cluster limits stop bounding the volume count Kubernetes sees                                                                                                                                                                                                                                 | Placement as is                                    | Placement that reads remaining subsystem and namespace slots as a candidate filter                                                                                                                                                 |
| G-7  | **Independent erasure-coding schemes:** Members carry different schemes, chosen by a class's placement                                                                                                                                                                                                                                                                                                                  | §5.8 `stripe` override, G-3                        | Overlaps with per-volume schemes in the control plane, which may make the member-level choice unnecessary                                                                                                                          |
| G-8  | **Group snapshots across members:** A consistency group whose volumes span members, frozen together                                                                                                                                                                                                                                                                                                                     | §9.3, the consistency-group design                 | A freeze coordinated across control-plane clusters, and its rollback when one member fails mid-freeze                                                                                                                              |
| G-9  | **Hardware refresh by member:** A new member of new hardware is added and the old member removed, which moves its volumes and retires its nodes without a cluster-wide migration                                                                                                                                                                                                                                        | §5.6 growth, §8.4 `RemoveMember`                   | Nothing beyond Phase 4, apart from a documented procedure                                                                                                                                                                          |
| G-10 | **Canary rollouts by member:** A data-plane image or setting is rolled out to one member and held there until it proves healthy, before the fan-out continues                                                                                                                                                                                                                                                           | §9.6 fan-out                                       | The health signal that releases the hold                                                                                                                                                                                           |
| G-11 | **Isolation by member:** Namespaces or tenants are bound to dedicated members, so a noisy workload or a member outage stays within its tenant                                                                                                                                                                                                                                                                           | G-3                                                | Whether the binding lives on the class, the namespace, or the supercluster                                                                                                                                                         |
| G-12 | **Growth proposed by capacity:** Discovery drafts a new member when the supercluster's fill passes its warning threshold and unenrolled workers with disks exist                                                                                                                                                                                                                                                        | §5.5, §5.6                                         | Whether a draft is raised automatically or only reported                                                                                                                                                                           |
