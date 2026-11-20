# Design Document: Fleet Management on Open Cluster Management

**Status:** Draft, with the admission guard of §10 implemented  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-09-13 (last updated 2026-09-13)  
**Test Plan:** [`tests/test-plan-management-hub.md`](../tests/test-plan-management-hub.md)

---

## Phase 0 — External Prerequisites

| Prerequisite                                                                       | Provided by                | Needed for                                                                                | Specified in |
|------------------------------------------------------------------------------------|----------------------------|-------------------------------------------------------------------------------------------|--------------|
| OCM `cluster-manager` on the hub: registration, work, placement, and addon-manager | Open Cluster Management    | Every payload, every enrollment, and the add-on's rendering                               | §3, §9       |
| OCM `klusterlet` in each member                                                    | Open Cluster Management    | Applying payloads and reporting their conditions                                          | §3, §11      |
| Kubernetes 1.30 or later in each member                                            | The member's distribution  | `ValidatingAdmissionPolicy` reached general availability there, and the guard is one      | §10          |
| One control plane, reachable from the hub, shared by every member                  | simplyblock (`sbcli`)      | The fleet's state store, and tier 1 of the read path                                      | §3, §11      |
| The control plane answers which member a storage cluster belongs to                | simplyblock (`sbcli`)      | `FleetMember.status.storage`, and the fleet-wide read path                                | §11, Q1      |
| A ClusterRole in each member aggregating admission permissions into the work agent | Whoever enrolls the member | The guard. Without it every policy in the add-on's payload is refused and nothing arrives | §10.6        |
| `multicloud-operators-foundation` in hub and members                               | Open Cluster Management    | `ManagedClusterView`, which is tier 3 alone                                               | §11, Q2      |

The last two are not settled. Q1 is a question about the control plane's own API, and the arrangement degrades rather than fails without it (§11.2). Q2 is a choice this design leaves open, because §9's agent already answers most of what tier 3 answers.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [The Standalone Constraint](#4-the-standalone-constraint)
5. [Separate Kinds on Each Side](#5-separate-kinds-on-each-side)
6. [What Crosses, and What Does Not](#6-what-crosses-and-what-does-not)
7. [The Fleet Component](#7-the-fleet-component)
8. [The Hub-Side Kinds](#8-the-hub-side-kinds)
9. [The Add-On](#9-the-add-on)
10. [The Admission Guard](#10-the-admission-guard)
11. [Transports and the Read Path](#11-transports-and-the-read-path)
12. [Controller Design](#12-controller-design)
13. [Failure Modes](#13-failure-modes)
14. [Observability](#14-observability)
15. [Testing Strategy](#15-testing-strategy)
16. [Rollout](#16-rollout)
17. [Open Questions](#17-open-questions)

Appendices:

- [Appendix A: `storagefleet_types.go`](#appendix-a-storagefleet_typesgo)
- [Appendix B: `fleetmember_types.go`](#appendix-b-fleetmember_typesgo)
- [Appendix C: `clusterdeployment_types.go`](#appendix-c-clusterdeployment_typesgo)
- [Appendix D: `driverdeployment_types.go`](#appendix-d-driverdeployment_typesgo)
- [Appendix E: `storageclassdeployment_types.go`](#appendix-e-storageclassdeployment_typesgo)
- [Appendix F: `fleetoperation_types.go`](#appendix-f-fleetoperation_typesgo)
- [Appendix G: `delivery_types.go`](#appendix-g-delivery_typesgo)

---

## Overview

One simplyblock control plane can serve more than one Kubernetes cluster, and each of those clusters runs the operator. This design specifies how that arrangement is operated from one place: a hub holds the desired shape of every enrolled cluster, ships it down through Open Cluster Management, and reports back what the member did with it.

The hub holds no copy of a managed object. Desired state lives in hub-owned kinds distinct from the kinds a member reconciles, and current state is read from the control plane the hub sits beside, joined with what an agent in the member reports. A member that loses its hub keeps reconciling, and a member the fleet detaches is left a working standalone deployment.

Nothing here reaches a single-cluster installation. The kinds are a new API group installed on the hub alone, the agent is an add-on rather than a mode of the operator, and the operator's binary, its RBAC, and its webhook configurations are unchanged.

---

## 1. Background

[`design-controlplane.md`](crd-redesign/design-controlplane.md) §5.2 anticipates the deployment this design is about: an external control plane may be shared, and the operator must assume it is. What that leaves unanswered is who drives the several Kubernetes clusters on the other side of a shared control plane, and today the answer is that each is driven by hand.

The pieces a fleet needs mostly exist. Every design in the CRD redesign reads backend state from the control plane's stream rather than from Kubernetes ([`design-crd-model.md`](crd-redesign/design-crd-model.md) §7.7), so the control plane is already the state store a fleet reads. [`design-clusterdeploymentconfig.md`](crd-redesign/design-clusterdeploymentconfig.md) makes a whole deployment one reviewable document, which is the natural payload to ship into a member. What is missing is the enrollment, the delivery, and the kinds that carry intent at fleet scope.

[RamenDR](https://github.com/RamenDR/ramen), the disaster-recovery orchestrator behind OpenShift Data Foundation, solves the same shape. Its hub holds `DRPolicy`, `DRCluster`, and `DRPlacementControl`, its managed clusters hold `VolumeReplicationGroup`, `DRClusterConfig`, and `MaintenanceMode`, and the two sets share no kind. §5 states why that separation is load-bearing rather than stylistic.

---

## 2. Goals and Non-Goals

### Goals

- One hub holds the desired shape of every enrolled Kubernetes cluster, and a change to it reaches the member without anyone touching the member.
- A member is enrolled, deployed into, and detached through objects on the hub, and detaching leaves a working standalone deployment behind.
- A console reads fleet-wide state without a round trip per member for the common questions.
- A standalone installation is unaffected, to the byte, by the existence of this design.
- The operator is unchanged. It reconciles objects somebody wrote, and draws no distinction between a person and a work agent as the writer.

### Non-Goals

- **Serving `storage.simplyblock.io` on the hub.** Registering the group as an aggregated `APIService` is a read path rather than a fleet mechanism, it is the largest single piece of the arrangement, and the control plane's own API answers the same questions. §11.4 records its shape and its cost.
- **A hub-side copy of any member object.** §5.
- **Fleet-wide rollout as a single object.** A driver rollout across members is a set of `DriverDeployment` objects, and the kind that would generate the set from a selection is Q5.
- **Tenancy quantities.** Which namespaces a tenant holds is RBAC, and this design is namespaced so that ordinary bindings separate tenants. An allocation envelope, meaning the nodes, devices, cores, and hugepages a namespace may claim, is a separate design.
- **A member that is also the hub.** OCM permits a hub to enroll itself, and nothing here forbids it, but the arrangement is not designed for it and is not tested.

---

## 3. Architecture Overview

There is exactly one control plane, and it is either local to a Kubernetes cluster or remote. A remote one fronts several Kubernetes clusters, each running its own operator. A hub runs beside that control plane, and each member runs an add-on.

```
                          HUB CLUSTER
 ┌──────────────────────────────────────────────────────────────┐
 │  cluster-manager (OCM)                                       │
 │    registration · work · placement · addon-manager           │
 ├──────────────────────────────────────────────────────────────┤
 │  fleet-manager                                               │
 │    1. Enrolls a member and installs the add-on               │
 │    2. Composes a payload and writes a ManifestWork           │
 │    3. Reads the control plane for the storage roll-up        │
 │    4. Rolls the agent's report into FleetMember.status       │
 │  fleet.simplyblock.io  StorageFleet · FleetMember ·          │
 │    ClusterDeployment · DriverDeployment ·                    │
 │    StorageClassDeployment · FleetOperation                   │
 └───────┬────────────────────────────────────────────┬─────────┘
         │ ManifestWork, status feedback              │ HTTPS
         │ (outbound from the member)                 │
 ┌───────▼────────────────────────────────┐  ┌────────▼─────────┐
 │            MEMBER CLUSTER              │  │  Control plane   │
 │  klusterlet (OCM)                      │  │  (sbcli)         │
 │    applies payloads, reports conditions│  │                  │
 │  fleet-agent                           │  │  GET /clusters   │
 │    reports what only the member knows  │  │  GET /storage-   │
 │  simplyblock-operator      (unchanged) │  │      nodes       │
 │    reconciles what was applied         │  │  GET /pools      │
 │  storage.simplyblock.io  every kind    │  └────────▲─────────┘
 └────────────────────────────────────────┘           │
                    storage nodes ────────────────────┘
```

The hub is the Kubernetes cluster, and what makes it one is OCM's `cluster-manager`. The fleet manager is a controller on it. The control plane is neither: it runs outside Kubernetes, and the hub sits beside it.

References run one way. A hub kind names a member, and no kind in a member names a hub, which is what lets a member lose its hub and keep reconciling.

---

## 4. The Standalone Constraint

A standalone deployment against a local control plane keeps working exactly as it does today. The constraint is stated as a test, so that a fleet concern cannot reach the single-cluster product:

> A standalone installation installs no kind of the fleet group, runs no add-on, and its `CustomResourceDefinition` set, its RBAC, and its webhook configurations are byte-identical to what it installs today.

Two consequences follow. The fleet kinds live in their own API group, installed only on the hub. And the agent is an add-on rather than a mode of the operator, so the operator binary and its RBAC are untouched. RamenDR selects its mode in one binary through `DRHubType` and `DRClusterType`, and this arrangement does not.

The operator therefore behaves identically in both deployments. The one thing a member gains that a standalone cluster does not is the admission guard of §10, and it arrives with the add-on.

---

## 5. Separate Kinds on Each Side

The hub and the member share no kind. A hub-side object carries intent and a member-side object is what the operator reconciles, and no object exists on both sides.

The alternative is a twin, where the hub owns each object's spec, the agent replays it into a local copy no user may write, the operator writes the local status, and the agent mirrors that back. Six properties of this API rule it out, and each stops applying once the two sides share no kind.

- **`metadata.generation` is issued by the local API server**, so a mirrored status carries a generation the hub never saw. [`design-crd-model.md`](crd-redesign/design-crd-model.md) §7.9 makes `observedGeneration` mandatory because it is the only field that says a status is current, and across a twin it reports a definite-looking wrong answer. One field also cannot say whether the generation it reports is the hub's or the member's, so each side reads the other's write as an unobserved change and answers it, without end.
- **Defaulting and pruning mean the spec written is never the spec read back**, so an agent that writes, reads, diffs, and resynchronizes flaps forever.
- **UIDs do not travel.** `spec.creatorRef` carries one and [`design-persistentvolumeops.md`](crd-redesign/design-persistentvolumeops.md) §11.1 calls it the load-bearing part, and owner references carry them too. The ownership spine cannot be reconstructed on the far side.
- **A refused write has nowhere to be reported.** Several designs put `failurePolicy: Fail` webhooks in front of creates, and a refused create writes no object, so there is no status to mirror.
- **`Ops` kinds are garbage-collected**, so an agent that reads local absence as work to do re-creates them, and a re-created `StorageNodeOps` with the `Remove` action drains a node a second time.
- **Cluster-scoped kinds have no hub-side layout**, and object names collide across members.

---

## 6. What Crosses, and What Does Not

| Kind                      | Standalone              | Member                               | Crosses as                                              |
|---------------------------|-------------------------|--------------------------------------|---------------------------------------------------------|
| `ControlPlane`            | `source.managed`        | `source.external`, same kind         | Nothing                                                 |
| `ControlPlaneOps`         | Applies                 | No action applies                    | Nothing                                                 |
| `SimplyblockDriver`       | Written by a person     | `ManifestWork` payload               | Down, from `DriverDeployment`                           |
| `ClusterDeploymentConfig` | Written or discovered   | `ManifestWork` payload               | Down, from `ClusterDeployment`                          |
| `OperatorOps` discovery   | Written by a person     | Runs locally, triggered by a payload | Down as a trigger, up as inventory                      |
| `StorageCluster`          | Written by a person     | Expanded from the config, as today   | Nothing. State comes from the control plane             |
| `StorageNode`             | Expanded by its cluster | Expanded by its cluster              | Nothing. The hub reads the control plane                |
| `StorageDevice`           | Projected               | Projected                            | Nothing. The hub reads the control plane                |
| `StoragePool`             | Written by a person     | `ManifestWork` payload               | Down. The backend half is the control plane's           |
| `StorageClass`            | Authored by a person    | `ManifestWork` payload               | Down, from `StorageClassDeployment`                     |
| Every `Ops` kind          | Written by a person     | Payload, or raised locally           | Down, from `FleetOperation`                             |
| `PersistentVolumeOps`     | Written or fanned out   | Almost always fanned out locally     | Nothing. Cluster-scoped, high cardinality, local origin |
| `XyzMetrics`              | Served                  | Served                               | Nothing. Both read the control plane                    |

Six kinds never cross in either direction. A local object the control plane does not know is read from the member itself, and `StorageClass` is one, so a console listing the classes that draw on a pool asks the member rather than the control plane.

---

## 7. The Fleet Component

The fleet is a component of this repository, beside `atlas-lib`, `operator`, and `csi-driver`. It is its own Go module, `github.com/simplyblock/fleet`, resolving `atlas-lib` and the operator through local `replace` directives, and it builds two binaries:

| Binary          | Runs        | Does                                                             |
|-----------------|-------------|------------------------------------------------------------------|
| `fleet-manager` | On the hub  | Reconciles the kinds of §8 into Open Cluster Management payloads |
| `fleet-agent`   | In a member | Reports what only the member can observe, and nothing else       |

**They share one module because they share a wire contract.** The agent writes a report and the manager reads it, and one module is what keeps a single definition of that report from skewing between them.

**They share one image**, `quay.io/simplyblock-io/simplyblock-fleet`. The operator's image already carries its node probe for the same reason, stated in its `Dockerfile`: the operator names its own image for the Job it creates, so the probe cannot be a version out of step with the operator that created it. Here the manager renders the `AddOnTemplate` that deploys the agent and names its own image in it, so the agent cannot be a version out of step with the manager that shipped it.

**The module is separate from the operator's** because the operator is the binary that ships into every member, and `open-cluster-management.io/api` has no business there. The layout is:

```
fleet/
  api/v1alpha1/              the fleet.simplyblock.io kinds
  cmd/fleet-manager/         the hub controller
  cmd/fleet-agent/           the member agent
  config/crd/bases/          the generated CustomResourceDefinitions
  config/addon/              the ClusterManagementAddOn and its AddOnTemplate
  internal/guard/            the admission guard, and the suites that exercise it
  hack/gen-addon/            renders the AddOnTemplate from internal/guard
```

---

## 8. The Hub-Side Kinds

`fleet.simplyblock.io/v1alpha1`, installed on the hub and nowhere else. Every convention is [`design-crd-model.md`](crd-redesign/design-crd-model.md) §3 and §7 applied unchanged, so that the two groups are one API to learn. The prefix for every label and annotation these kinds define is `fleet.simplyblock.io`, one prefix per group, and metrics keep the `simplyblock_<entity>_<item>_<agg>` form.

| Kind                     | Category | Short | Holds                                                                   |
|--------------------------|----------|-------|-------------------------------------------------------------------------|
| `StorageFleet`           | Entity   | `sf`  | The one control plane, and the fleet's defaults. Singleton              |
| `FleetMember`            | Entity   | `fm`  | One enrolled Kubernetes cluster, its link, its inventory, and a roll-up |
| `ClusterDeployment`      | Entity   | `cd`  | The composed `ClusterDeploymentConfig` for one member, and its approval |
| `DriverDeployment`       | Entity   | `dd`  | The `SimplyblockDriver` payload for one member                          |
| `StorageClassDeployment` | Entity   | `scd` | One `StorageClass` drawing on a pool in one member                      |
| `FleetOperation`         | Action   | `fop` | One `Ops` object of the storage group, shipped into one member          |

Five of the six are entities, because building out a cluster is desired state: a document, a driver version, the classes that consume the capacity, and a member to put them in. Issuing an operation is the one imperative act, and `FleetOperation` carries it.

Ownership is a two-level tree. `FleetMember` owns every object that names it, so detaching a member collects its deployments, its classes, and its operations. `StorageFleet` owns nothing, because a member outlives an edit to the fleet's defaults, and ownership from a singleton at the root of the tree turns one deletion into a fleet-wide cascade.

The types are Appendices A through G, whole, and each section below quotes the fields its argument turns on and no more.

### 8.1 Three Namespaces

Namespace means four different things across the boundary, and the distinction between any two of them is a privilege boundary.

| Namespace                          | Side   | Holds                                                                            | Is the boundary for                    |
|------------------------------------|--------|----------------------------------------------------------------------------------|----------------------------------------|
| A tenant namespace                 | Hub    | Every fleet object: the member, its deployments, its classes, and its operations | Who may operate on this tenant's fleet |
| The member's OCM namespace         | Hub    | `ManifestWork`, the add-on's registration, and the agent's report                | Open Cluster Management's own layout   |
| The operator's namespace           | Member | The operator workload, its webhooks, and its own configuration                   | Who may operate the installation       |
| Any namespace a deployment chooses | Member | Every storage-group object a payload creates or a cluster expands                | What a team may consume in the member  |

The first two are both on the hub and stay separate. OCM requires a `ManifestWork` to live in its `ManagedCluster`'s namespace, which is addressing rather than a tenancy decision, and a tenant needs no rights there: write access to a member's OCM namespace is write access to any payload into that member, which is every permission the fleet holds over it. The fleet object is therefore a tenant's to write, the `ManifestWork` is the controller's, and the controller is the only thing that crosses between them.

The last two are both in the member, and the operator's own namespace is not where its objects live. The operator watches cluster-wide, and `operator/internal/upgrade/scope.go` states that a `StorageCluster` in any namespace belongs to the installation. A payload therefore states its namespace rather than inheriting one, `status.result.namespace` records what was used, and it bears no relation to the hub-side namespace the intent was written in.

The `ManifestWork` objects these controllers create live in the member's OCM namespace, which is a different namespace from the fleet object's, so an owner reference is unavailable for the reason [`design-crd-model.md`](crd-redesign/design-crd-model.md) §5 gives for the `StorageClass`. They carry `fleet.simplyblock.io/managed-by`, and a finalizer performs the cascade, which is the group's existing pattern.

### 8.2 StorageFleet and FleetMember

`StorageFleet` is a singleton named `simplyblock`, matching `ControlPlane`'s convention: there is exactly one control plane, and no kind carries a reference by which a controller could select among several. Its spec is where that control plane is, and it is immutable as a block, because re-pointing a live fleet at another control plane produces a different fleet.

`FleetMember` holds only what cannot be observed:

```go
// ClusterRef names the Open Cluster Management ManagedCluster this member is.
// That kind is cluster-scoped and its names are unique, so a bare name is
// unambiguous.
// +kubebuilder:validation:Required
// +k8s:immutable
ClusterRef string `json:"clusterRef"`

// DetachPolicy is what happens to what the fleet applied when this member is
// deleted. Retain orphans it, leaving a working standalone deployment.
// +kubebuilder:default=Retain
// +optional
DetachPolicy FleetDetachPolicy `json:"detachPolicy,omitempty"`
```

Everything else about a member is reported. The status splits into three blocks that come from different places and go stale independently: `link` is the agent's own report, `inventory` is a summary of the discovery reports, and `storage` is the roll-up read from the control plane. Each carries the time it was last current, because an agent that cannot reach the hub and storage nodes that cannot reach the control plane are usually the same network event, and a console that renders the three as one panel loses the distinction.

Detaching is expressed as a deletion. Deleting a `FleetMember` is the instruction to unenroll, and a finalizer performs it: patch every `ManifestWork` for the member to `propagationPolicy: Orphan` when the policy is `Retain`, delete them, then withdraw the add-on. Orphaning before withdrawing is what leaves a working standalone deployment behind, and it is why `deleteOption.propagationPolicy` is load-bearing.

`status.inventory` is a summary rather than the reports. A discovery run produces one `ConfigMap` per worker, each admitted up to a mebibyte (`operator/internal/nodeprobe/configmap.go`), so a fifty-worker member is fifty objects and tens of megabytes. A fleet-wide list needs the counts, the device classes, and the raw capacity, and §9 puts the summarizing in the member.

### 8.3 ClusterDeployment and DriverDeployment

Both carry a spec of the storage group as their payload, and both are entities rather than operations, because a deployment is applied rather than operated, which is the reading `ClusterDeploymentConfig` already has.

```go
// Config is the document that will be applied in the member, composed from
// that member's inventory. It is a typed storage-group spec rather than an
// opaque blob because it is the thing a reviewer reads.
// +kubebuilder:validation:Required
Config storagev1alpha2.ClusterDeploymentConfigSpec `json:"config"`

// Approved is the instruction to build, and setting it is the deployment
// rather than a review stage in front of one.
// +optional
Approved bool `json:"approved,omitempty"`
```

**A typed payload inherits the validation of the type it embeds.** Every `XValidation` marker on `ClusterDeploymentConfigSpec` is generated into this kind's `CustomResourceDefinition` as well, so the rule that a device selection names NVMe addresses or block devices and not both is checked when the document is authored on the hub. A rule expressed in the member's admission webhook is not inherited, and a write it refuses produces no object and therefore no status to read back (§5), so a rule that can refuse a create belongs in CEL on the type.

Two rules carry what markers cannot:

```go
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.config == oldSelf.config",message="config is immutable once approved"
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.approved",message="approval cannot be withdrawn"
```

The document is editable while it is a draft and frozen once approved, which is the one place this kind departs from `ClusterDeploymentConfig`'s immutability. A draft composed at the hub is edited at the hub, and the storage-group object is created only once the hub-side document is settled, so what ships down is always an already-approved document and the member never holds a draft nobody approved. That last property depends on the composition setting the embedded document's own approval, which is Q3.

`spec.approvedBy` records the identity that approved. A webhook sets it from the request's user info, because the work agent replaying the edit into the member is otherwise the only approver the audit trail carries, and [`design-clusterdeploymentconfig.md`](crd-redesign/design-clusterdeploymentconfig.md) §5 made approval a spec field for its audit surface.

`DriverDeployment` is the same shape around `SimplyblockDriverSpec`, without the approval, because a driver version is an edit rather than a deployment gate. It is one object per member, so a fleet-wide rollout is a set of them, and Q5 carries the kind that would generate the set.

### 8.4 StorageClassDeployment

A class is how a claim asks a pool for capacity, and [`design-storagepool.md`](crd-redesign/design-storagepool.md) §5 settles the two facts that decide the hub's shape for it.

A class is authored rather than generated, and a pool may have zero or more. One pool can back a class with compression on, another with it off, one formatted ext4, one formatted XFS, a permissive ceiling for a batch tenant, and a tight one for a latency-sensitive one. Configuring a class for a pool that already exists is therefore the ordinary case, and it changes nothing about the pool.

The assignment is three labels on the class. `storage.simplyblock.io/namespace`, `.../cluster`, and `.../pool` are what say which pool a class draws from, in either direction, and `StoragePool.status.storageClassNames` is the pool's side of the same selector. Neither side owns the other.

One class is expanded locally and the rest come from the hub. A cluster creation writes one class for its default pool, carrying `storage.simplyblock.io/managed-by: storagecluster`, and that happens in a member exactly as it does in a standalone installation. Every class beyond that first one is authored, and in a member the hub authors it.

**A namespaced hub kind is what makes a cluster-scoped object delegable.** `StorageClass` is cluster-scoped, and `resourceNames` covers `get`, `update`, and `delete` but never `create` or `list`, so RBAC cannot express "may author classes for this pool" against the class itself, and a single cluster offers no namespaced object to hang that permission on. A pool's owner holds verbs on `storageclassdeployments` in their own namespace and needs no rights on `StorageClass` anywhere, and the work agent writes the cluster-scoped object in the member under its own identity.

Deleting the fleet object deletes the class, without colliding with the operator. A `ManifestWork` with the default `propagationPolicy: Foreground` removes what it applied, and §6 of the pool design lets the operator delete only a class carrying `storage.simplyblock.io/managed-by: storagecluster`. A class the hub shipped carries the fleet's marker, so each side deletes what it created and refuses on the other's.

### 8.5 FleetOperation

One kind ships any `Ops` object of the storage group into one member.

```go
// Operation is what to run there. Exactly one member is set.
// +kubebuilder:validation:Required
// +k8s:immutable
Operation FleetOperationTarget `json:"operation"`

// Abort asks the member's own object to stop, by setting spec.abort on it
// through the payload. It is the one mutable field on this spec, and the
// member's graph decides whether the step it is on accepts it.
// +optional
Abort bool `json:"abort,omitempty"`
```

The single kind is a deliberate break with the `<Entity>Ops` convention. That convention holds that a kind ending in `Ops` is one-shot and names one target, and this kind is both. What it does not name is a hub-side entity: its target is an object in a member, and the parameters belong to the storage-group kind whose spec it carries. One hub kind per managed `Ops` kind would reintroduce a hub-side copy of a managed object for the one category where re-creating an object is dangerous.

The discriminated block follows the `ControlPlane.spec.source` pattern: one member per variant, exactly one set, enforced in CEL. It is typed rather than an opaque payload, because this is the payload that performs a side effect and it is the one that most needs validating before it is sent. It enumerates the kinds that are declared rather than the kinds that are intended, so `StorageDeviceOps` is absent until it lands (Q4).

Status comes back in two pieces. `status.phase` is the hub's own progress at getting the operation to run, and `status.remote` is what the member's object reports, filled from the status feedback rules on the `ManifestWork`. `RemoteOpsStatus` holds the phase, the step's state, the message, and the observed generation, because a feedback rule selects one value by JSON path and a fleet-wide list needs scalars. Anything beyond it is a whole-object read against `status.target` (§11.3).

Two `ManifestWork` settings make a one-shot payload safe. `updateStrategy.type: CreateOnly` ensures creation and never re-applies, which is what stops a garbage-collected `Ops` object from being written a second time, and `ttlSecondsAfterFinished` removes the work once the operation is terminal.

---

## 9. The Add-On

One add-on, declared once on the hub and installed per member, carries everything the fleet puts in a member that is not a payload.

It is declared as an `AddOnTemplate` plus a `ClusterManagementAddOn`, both cluster-scoped on the hub, and OCM's own addon-manager renders the template into a `ManifestWork` for each member. That is why the fleet needs no hub-side add-on controller of its own and no second delivery mechanism beside the one the deployments already use. `config/addon/` holds both objects.

The install strategy is `Manual` rather than `Automatic`. A member is enrolled by creating a `FleetMember`, and installing the add-on into every cluster an unrelated team registered on the same hub is not what enrolling means, so the fleet manager creates the `ManagedClusterAddOn` for the members it owns.

The template carries two things:

| Content                                                                                    | Purpose                                     |
|--------------------------------------------------------------------------------------------|---------------------------------------------|
| The `fleet-agent` Deployment, its `ServiceAccount`, and a `KubeClient` registration stanza | Reporting what only the member knows (§9.1) |
| The admission guard's policy, its binding, and its allowlist                               | §10                                         |

### 9.1 What the Agent Reports

Three facts the hub needs are Kubernetes-shaped, live in the member, and are expensive to pull one object at a time. The agent is what reports them.

- **The inventory summary.** A discovery run leaves one report per worker, up to a mebibyte each. Pulling fifty of them to the hub to compute a count, a device-class list, and a raw capacity moves tens of megabytes to produce a few hundred bytes, on a refresh interval, per member. The agent computes the summary where the reports already are.
- **The link block.** The operator's version, the driver's version, and the versions of `storage.simplyblock.io` the member serves are each a separate object or a discovery call. Read one at a time from the hub they are several round trips per member per refresh, and read in the member they are one status write.
- **Schema skew, before it prunes a payload.** `status.link.storageAPIVersions` is what makes a member serving an older version of the group visible before a field is silently dropped from a payload rather than after.

The agent registers with `registration.type: KubeClient`, so the addon-manager injects a hub kubeconfig into its Deployment and binds a role for it on the hub. The binding is `CurrentCluster`, which is the member's own OCM namespace, so the agent writes its report there and never into a tenant namespace. The fleet manager rolls that report into `FleetMember.status`. Keeping the agent out of tenant namespaces is what stops the add-on from holding a permission the fleet does not already hold: write access to a member's OCM namespace is equivalent to full control over that member (§8.1), and a tenant namespace sits outside what the fleet already holds.

The kind the agent writes is Q6.

---

## 10. The Admission Guard

In a member, the storage group is written by the fleet, by the operator, and by nothing else. The guard is what enforces that. `fleet/internal/guard` builds its three objects, and `hack/gen-addon` renders them into the add-on template, so the rule the suites exercise and the rule a member installs are one definition.

**The guard is a `ValidatingAdmissionPolicy`, evaluated inside the API server.** The rule is keyed on the requester's identity, and CEL reads that from `request.userInfo`, so the whole of it fits in a policy.

What rules out a workload serving the same rule is that the guard sits in front of the operator's own writes: the operator writes `StorageNode.spec.overrides` and `StorageCluster.spec.nodeConfigs` itself, so at `failurePolicy: Fail` the member's storage control loop stalls for as long as whatever serves the rule is down, and at `Ignore` the rule is advisory. A policy in the API server is reachable whenever the API server is, and it needs no certificate, no rotation, and no second replica.

**Admission never sees reads.** `GET`, `LIST`, and `WATCH` do not reach an admission plugin, so the guard covers `CREATE`, `UPDATE`, and `DELETE`, and restricting who may read the group is RBAC's job and is not part of this design.

The match is the whole group at every version:

```yaml
resourceRules:
  - apiGroups: ["storage.simplyblock.io"]
    apiVersions: ["*"]
    operations: ["CREATE", "UPDATE", "DELETE"]
    resources: ["*"]
```

`resources: ["*"]` matches every resource in the group and no subresource, which is what leaves `status` out. Only the operator writes status, it carries no intent for the fleet to protect, and it is the hottest write path in the member.

The policy, its binding, and its allowlist travel as one payload, in that order. Applied separately, a policy that lands before its allowlist refuses every write to the group until the allowlist follows, which is the binding's deny-on-missing behavior working exactly as intended against an install that split them.

The audit annotation that records a refusal evaluates to an empty string for an admitted request, which the API server omits, so the annotation carries only what was refused. Empty rather than null: the API server's CEL has no overload for a ternary whose branches are a string and a null, in either order, so a policy written that way is refused at install rather than at evaluation.

### 10.1 The Allowlist Is Data

The identities are a `ConfigMap` the policy reads through `paramKind`, rather than literals in the rule, because they are not knowable when the rule is written. The operator's own service account has two spellings: the Helm chart names it `simplyblock-operator` in the release namespace, and the Kustomize and OLM path applies a `simplyblock-operator-` name prefix in `simplyblock-operator-system`.

Six identities and two groups are allowed, and three of them are not about people at all:

| Identity                                                               | Why it is allowed                                                                                                                                             |
|------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------|
| The operator's service account, in each of its install spellings       | It writes its own specs, including a node's overrides                                                                                                         |
| `open-cluster-management-agent:klusterlet-work-sa`                     | Every payload from the hub lands under this identity, and not under the fleet agent's                                                                         |
| `kube-system:generic-garbage-collector`                                | Most objects in this group are deleted by cascade down the ownership spine                                                                                    |
| `kube-system:namespace-controller`                                     | A deleted namespace removes its objects the same way                                                                                                          |
| The group `system:serviceaccounts:open-cluster-management-agent-addon` | Add-ons are allowed by their namespace rather than one name at a time                                                                                         |
| The group `simplyblock:break-glass`                                    | It has no members until somebody binds it, and it exists so that an incident on a member is resolved by an auditable grant rather than by deleting the policy |

A member whose operator runs somewhere none of those three names receives a composed list instead. `Allowlist.WithOperator` adds one identity and is idempotent, so a reconcile that runs twice produces the same list, and the composed list is delivered in the same payload as the policy.

The binding sets `parameterNotFoundAction: Deny`. A missing allowlist is a broken install rather than a reason to stop checking, and the parameter travels in the same payload as the policy, so the work agent restores a deleted one. A refusal for that reason is the binding failing to configure rather than the rule deciding, so it does not carry the `Forbidden` reason a validation denial carries, and anything matching on the reason rather than on the refusal misses it.

### 10.2 The Ceiling: A Member's Administrator

**No admission mechanism prevents a cluster administrator of the member from removing the guard.** Kubernetes skips admission on `admissionregistration.k8s.io` resources to avoid circular dependencies, so no policy and no webhook intercepts its own deletion, and a rule matching those resources installs cleanly and never fires. That is upstream behavior rather than a gap in this design, and an inert rule claiming otherwise is worse than its absence, so the guard carries none.

What follows from it shapes the rest of §10. Against every identity that is not a cluster administrator, the guard prevents. Against a cluster administrator, the fleet's answer is repair and alarm: the `ManifestWork`'s ServerSideApply restores the policy on its next resync, the add-on's health reports the gap, and `simplyblock_fleet_guard_denials_total` and the removal itself are what a hub notices. Repair closes the window after the fact and does not prevent what happened inside it.

Two things narrow the ceiling rather than remove it. **The allowlist is protected**, by a second policy whose identities are inlined rather than read from a parameter, so adding an identity to the allowlist, and deleting it to write a new one, are both refused. And **on Kubernetes 1.36 and later, manifest-based admission policies** are loaded from the API server's own filesystem at startup and cannot be deleted through the API at all. They need control of the member's control-plane configuration, which a payload cannot deliver, so they are available to a deployment that owns its members' control planes and not to the fleet. Q11 owns that.

### 10.3 The Allowlist Is Protected by a Second Policy

`simplyblock-storage-guard-self` matches the allowlist `ConfigMap` by name on create, update, and delete, and admits only the work agent and the break-glass group. Create matters as much as update: an allowlist that may be deleted and created again is an allowlist anybody rewrites in two steps.

**Its two identities are inlined into the rule, and that is load-bearing.** Were the allowlist's protection part of the parameterized binding, a member whose allowlist went missing would refuse every matched request, the allowlist is one of the matched objects, and nothing could put it back. Independent of the parameter, the work agent can always restore a deleted allowlist, while the storage group stays refused in the meantime, which is the correct direction to fail in.

### 10.4 What the Guard Does Not Cover

**The API upgrade tool writes objects under a human administrator's kubeconfig.** `operator/internal/upgrade/steps/migrate.go`, `steps/ownership.go`, and `record.go` all write, and the tool runs with the caller's credentials rather than a service account. It is the path every upgraded cluster takes, so a deny-by-default rule refuses the upgrade using exactly the mechanism that is meant to refuse a person. Q7 owns the answer.

**The CSI driver is not on the allowlist and does not need to be.** It writes no custom resource of this group at runtime, reading an annotation on the claim and otherwise staying out of the API. The data path is unaffected by the guard.

**A drifted object is reconciled back without the guard.** A `ManifestWork` applied with `updateStrategy.type: ServerSideApply` restores what the hub owns on its next resync, so the guard converts a silent revert into a clear refusal rather than being the only thing holding the member's shape.

**`status` is outside the match, and at least three kinds keep a lock there.** `StoragePool`, `StorageDevice`, and `StorageBackup` each carry `status.activeOpsRef`, which names the `Ops` object currently allowed to act on them. An identity the guard refuses can still write status and take or clear that lock. Excluding subresources was a throughput decision, and Q12 owns whether it survives contact with a lock that lives there.

**Impersonation passes straight through.** The rule keys on the effective identity, so anyone holding `impersonate` on the operator's service account writes as the operator. That boundary is RBAC's and not the guard's.

### 10.6 What the Member Must Grant

Open Cluster Management's work agent applies the add-on's payload under its own identity, and that identity is deliberately bounded. It holds the built-in `admin` role and OCM's own two, none of which reaches `admissionregistration.k8s.io`, so a payload carrying the guard is refused per object:

```
validatingadmissionpolicies.admissionregistration.k8s.io "simplyblock-storage-guard" is forbidden:
User "system:serviceaccount:open-cluster-management-agent:klusterlet-work-sa" cannot get resource
"validatingadmissionpolicies" in API group "admissionregistration.k8s.io" at the cluster scope
```

The allowlist `ConfigMap` applies and the four admission objects do not, so the guard installs its parameter and nothing that reads it.

**The fleet cannot grant this to itself.** OCM's extension point is a ClusterRole labeled `open-cluster-management.io/aggregate-to-work: "true"`, whose rules aggregate into the work agent's permissions, and a payload cannot carry it: Kubernetes refuses a ClusterRole conferring permissions its creator does not hold, so the applier of a payload cannot apply the grant that unblocks it. It is applied in the member, once, by whoever enrolls it, and `fleet/config/member/` holds it.

That is the right shape rather than a workaround. Installing admission policy in a cluster is a privilege a cluster grants, and this object is where a member's administrator reads exactly what the fleet may do to its admission configuration. Removing it stops the fleet replacing the guard, which is the ceiling of §10.2 reached by a different lever.

### 10.5 The Fields the Member Owns

A payload that the hub owns and the operator also writes is the one case the identity rule does not settle, because both writers are allowed. `ManifestWork`'s `updateStrategy.ignoreFields`, with `condition: OnSpokePresent`, stops the hub reconciling a path once the resource exists in the member, and the paths it names are the ones the operator writes into a spec the hub composed.

That set is one table with three consumers: the `ignoreFields` entries on the payload, the audit that no spec field has two owners, and any future per-field refinement of the guard. It is maintained as one list, because two mechanisms disagreeing about which fields the member owns is the failure this design cannot detect at runtime.

---

## 11. Transports and the Read Path

Open Cluster Management defines three transports, and a console uses a different one per view.

| Transport             | Direction     | Carries                                                                                                                      |
|-----------------------|---------------|------------------------------------------------------------------------------------------------------------------------------|
| `ManifestWork`        | Hub to member | A complete resource as an opaque payload, applied locally by the work agent, with its own conditions from pending to applied |
| Status feedback rules | Member to hub | Named scalars selected by JSON path out of the applied resource, on a poll or a watch                                        |
| `ManagedClusterView`  | Member to hub | One named object, fetched on demand, returned as raw JSON                                                                    |

### 11.1 Four Tiers

| Tier | Mechanism                          | Serves                                               | Costs                                        |
|------|------------------------------------|------------------------------------------------------|----------------------------------------------|
| 1    | The control plane, read by the hub | Fleet-wide lists and search                          | Nothing per member. It never leaves the hub  |
| 2    | Status feedback                    | An operation's phase, step, and message in a list    | Scalars only, so a phase and not a condition |
| 3    | `ManagedClusterView`               | One real object, whole, from the member's API server | A round trip, and the link has to be up      |
| 4    | `cluster-proxy`                    | The member's API server directly, including events   | The same, plus an add-on and a tunnel        |

Tier 2 is what makes a fleet-wide list of operations useful. A phase, a step state, and an observed generation are single values, which is what a feedback rule carries, so a list of what is running across the fleet needs no round trip per object.

Tier 4 answers what the other three cannot. The `cluster-proxy` add-on runs a proxy server on the hub and an agent in each member, and the agent dials the tunnel outbound, so a hub component reaches a member's `kube-apiserver` by naming the member. Through it a console reads the actual `Events`, which live in no object at all and which [`design-crd-model.md`](crd-redesign/design-crd-model.md) §3.3 identifies as the only thing separating a queued operation from a new one. It is optional and is not part of this design's first build.

Tiers 3 and 4 answer about one member at a time. Which members have a node in `Degraded` is a fan-out over N clusters with N failure modes, which is what tier 1 answers.

### 11.2 Reading the Control Plane

The one control plane is the fleet's state store, and the hub reads the same stream every operator reads.

| Fact                                                   | Where the hub reads it                                          |
|--------------------------------------------------------|-----------------------------------------------------------------|
| Cluster, node, pool, and volume state                  | The control plane, directly                                     |
| Device inventory and health                            | The control plane. `StorageDevice` is a pure projection of it   |
| Capacity and occupancy measurements                    | The control plane's own metrics                                 |
| Which worker a node runs on, and whether its pod is up | The agent. Kubernetes knows this and the control plane does not |
| What a worker has before a cluster exists on it        | The agent, as the inventory summary                             |
| Driver and operator version, and served API versions   | The agent                                                       |

Joining a control-plane record to the member it belongs to is the one part that depends on something outside this repository (Q1). Where the control plane cannot answer it, the mapping is recoverable from the member's side: a `ClusterDeployment` records the identifier of the cluster it built in `status.result.storageClusterUUID`, and the agent reports the identifiers its member holds.

### 11.3 Feedback Rules and Whole Objects

A `FleetOperation`'s payload carries feedback rules for four JSON paths: `status.phase`, `status.step.state`, `status.message`, and `status.observedGeneration`. The deadline beside the step in the member is deliberately not carried, because it is an absolute instant and is meaningless against the hub's clock.

`status.target` names the object the payload created, so a whole-object read needs no re-derivation of the name. Whether that read is a `ManagedClusterView`, and therefore whether the fleet takes a dependency on `multicloud-operators-foundation`, is Q2.

### 11.4 Serving the Storage Group on the Hub

The storage group is not served on the hub, and a console reads fleet-wide state from the control plane's own API instead. The shape a reader may otherwise reach for is an aggregated `APIService` registering the group and answering out of the control plane, which the operator's metrics server is already an instance of, certificate rotation and authorization delegation included.

The constraint is what that server owes. The metrics server computes one kind on request, and this group is a dozen kinds with `list` and `watch` semantics, resource versions, pagination, and field selectors over a REST client. An aggregated server also gets no conversion webhook the way a `CustomResourceDefinition` does, so the first hub serving two versions of the group at once owns the conversion between them.

---

## 12. Controller Design

### 12.1 Location

Every controller in this section lives in `fleet/internal/controller/` and is registered in `fleet/cmd/fleet-manager/main.go`. None of them runs in a member.

| Controller                         | Reconciles               | Produces                                                                   |
|------------------------------------|--------------------------|----------------------------------------------------------------------------|
| `StorageFleetReconciler`           | `StorageFleet`           | The control plane's reachability, its version, and the member count        |
| `FleetMemberReconciler`            | `FleetMember`            | The add-on's installation, the three status blocks, and the detach cascade |
| `ClusterDeploymentReconciler`      | `ClusterDeployment`      | A `ManifestWork` carrying a `ClusterDeploymentConfig`, and the result      |
| `DriverDeploymentReconciler`       | `DriverDeployment`       | A `ManifestWork` carrying a `SimplyblockDriver`                            |
| `StorageClassDeploymentReconciler` | `StorageClassDeployment` | A `ManifestWork` carrying a `StorageClass`                                 |
| `FleetOperationReconciler`         | `FleetOperation`         | A `CreateOnly` `ManifestWork`, and the remote status                       |

### 12.2 Reconciliation Trigger

Each controller owns its kind and watches the `ManifestWork` objects it created, mapped back through the `fleet.simplyblock.io/managed-by` label, so a change in a member's applied conditions reaches the object that asked for it without a poll. `FleetMemberReconciler` additionally watches `ManagedCluster` for availability and the agent's report object in the member's OCM namespace.

The control plane is read on a ticker rather than on a watch, and `status.storage.observedAt` is what says how old the answer is. [`design-sse-push-notifications.md`](design-sse-push-notifications.md) is where that becomes a stream.

### 12.3 Concurrency and Mutual Exclusion

No controller here blocks. A delivery that has not landed is a phase and a requeue, never a wait inside `Reconcile`. That rule holds across this repository's controllers and matters more at fleet scope: one member that stops answering must not consume a worker that every other member needs.

`ClusterDeployment` drives a persisted step machine through `status.step`, using `atlas-lib/statemachine`, so a delivery that restarts mid-flight resumes where it stopped rather than recomposing.

Two deployments naming one member do not exclude each other. A member holds at most one `ClusterDeployment` per storage cluster, and the collision that matters is two `FleetOperation` objects targeting one storage-group entity, which the member's own `Ops` lock refuses. The hub reports that refusal rather than preempting it, because the member's lock is the one that knows.

### 12.4 RBAC

On the hub the manager needs the fleet group, `ManifestWork`, `ManagedCluster`, `ManagedClusterAddOn`, and the `ConfigMap` or report kind the agent writes. It needs no permission in any member, which is the property that makes the arrangement's blast radius the hub's own credentials rather than N clusters' worth.

In a member the fleet holds exactly what the work agent already holds, which is why §8.1 keeps tenants out of the OCM namespace, and the agent holds read access to the discovery reports and to the two workloads it reports versions for.

---

## 13. Failure Modes

| Condition                                                | Where it shows                                                          | Result                                                                                         |
|----------------------------------------------------------|-------------------------------------------------------------------------|------------------------------------------------------------------------------------------------|
| The member is unreachable                                | `FleetMember.status.phase: Unreachable`, `status.link.lastContact` ages | Payloads stay written. The work agent applies them when it reconnects                          |
| The control plane is unreachable                         | `StorageFleet.status.phase: Unavailable`                                | The storage roll-up ages and says so through `observedAt`. Deliveries are unaffected           |
| Both are unreachable                                     | Both blocks age independently                                           | The distinction is preserved in the two timestamps and lost in any panel that merges them (Q9) |
| A payload is refused by the member's admission           | `status.delivery.conditions`, `Applied: False`                          | No object exists in the member, so there is no remote status. §5                               |
| The member serves an older version of the group          | `status.link.storageAPIVersions`                                        | Visible before a field is pruned out of a payload rather than after                            |
| A discovery report cannot be parsed                      | `status.inventory.unreadableReports`, and an event in the member        | The worker is left out of the composed draft                                                   |
| The allowlist `ConfigMap` is absent                      | Every write to the storage group in that member is refused              | Deliberate (§10.1). The work agent restores it                                                 |
| A `FleetMember` is deleted while a delivery is in flight | `status.phase: Detaching`                                               | The finalizer orphans or deletes per `spec.detachPolicy` before withdrawing the add-on         |

---

## 14. Observability

The fleet component has no observability surface today, because it has no code today. Both tables below are new infrastructure rather than additions.

### Kubernetes Events

Events land on the fleet object the user wrote, never on the `ManifestWork` the controller derived from it. The fleet object is the one a user owns, looks at, and keeps: a `ManifestWork` is an implementation detail in a namespace tenants have no rights in, and it is deleted when the delivery is withdrawn.

| Event                                                                                            | Type    | Reason                    | On                                    |
|--------------------------------------------------------------------------------------------------|---------|---------------------------|---------------------------------------|
| The member's cluster is not accepted by Open Cluster Management yet, so nothing can be delivered | Warning | `MemberNotReady`          | `FleetMember`                         |
| The add-on was installed into the member                                                         | Normal  | `AddOnInstalled`          | `FleetMember`                         |
| The detach is orphaning what the fleet applied, leaving a standalone deployment                  | Normal  | `MemberDetaching`         | `FleetMember`                         |
| The control plane did not answer, so the storage roll-up is older than it looks                  | Warning | `ControlPlaneUnreachable` | `StorageFleet`                        |
| The payload was written and is waiting for the member's work agent                               | Normal  | `DeliveryStarted`         | Any deployment kind, `FleetOperation` |
| The member applied the payload                                                                   | Normal  | `DeliveryApplied`         | Any deployment kind, `FleetOperation` |
| The member refused the payload, and the message is the admission response                        | Warning | `DeliveryRefused`         | Any deployment kind, `FleetOperation` |
| The composition is holding because the member's inventory has not been reported yet              | Normal  | `CompositionHeld`         | `ClusterDeployment`                   |
| The class names a pool the member does not have                                                  | Warning | `PoolMissing`             | `StorageClassDeployment`              |
| The operation reached a terminal state in the member                                             | Normal  | `OperationFinished`       | `FleetOperation`                      |

`CompositionHeld` is the one that earns its place twice over. A composition waiting for an inventory that will arrive and a stalled controller are indistinguishable from the object alone, and every held decision in this repository owes an event for that reason.

`DeliveryApplied` and `DeliveryRefused` are one reason each rather than one per kind, because the kind is already the event's target and repeating it in the reason gives anyone alerting on a refusal four reasons to match instead of one.

### Prometheus Metrics

| Metric                                              | Labels                | Description                                                                             |
|-----------------------------------------------------|-----------------------|-----------------------------------------------------------------------------------------|
| `simplyblock_fleet_members`                         | `phase`               | Members in each phase. The gauge a fleet-wide alert reads                               |
| `simplyblock_fleet_member_last_contact_seconds`     | `member`              | Age of the agent's last report, in seconds                                              |
| `simplyblock_fleet_control_plane_last_read_seconds` | —                     | Age of the storage roll-up, in seconds                                                  |
| `simplyblock_fleet_deliveries_total`                | `kind`, `result`      | Payload deliveries by kind and by applied, refused, or withdrawn                        |
| `simplyblock_fleet_delivery_duration_seconds`       | `kind`                | Time from writing a `ManifestWork` to its `Applied` condition                           |
| `simplyblock_fleet_operations_total`                | `operation`, `result` | Fleet operations by the storage-group kind they carried and their terminal remote phase |
| `simplyblock_fleet_guard_denials_total`             | `member`              | Writes the admission guard refused, scraped from the member by the agent                |

Three are load-bearing. `simplyblock_fleet_member_last_contact_seconds` and `simplyblock_fleet_control_plane_last_read_seconds` are the pair that distinguishes the two halves going stale, which is the failure this design is most likely to present as one symptom (Q9). `simplyblock_fleet_delivery_duration_seconds` is what eventually decides whether a delivery needs a timeout, since a `ManifestWork` that is never applied is indistinguishable from one applied slowly without a distribution to compare against. `simplyblock_fleet_guard_denials_total` is how an allowlist that is missing an identity is noticed, rather than reported by whoever was refused.

---

## 15. Testing Strategy

The full matrix is [`test-plan-management-hub.md`](../tests/test-plan-management-hub.md).

**Unit tests** carry the composition and the guard. Composing a payload from an inventory, deriving the three assignment labels from a pool reference, computing a configuration fingerprint, and mapping a `ManifestWork`'s conditions onto a delivery status are pure functions with a fake client, and they are where most of the logic is. The guard's rule is evaluated against the allowlist it ships with, both taken from the objects the builder produces, so the test is of the payload rather than of a copy of it.

**The API server's type-checker is stricter than the unit environment's**, which declares its inputs dynamically and accepts expressions the API server refuses. The guard therefore also runs against a real API server, and that suite is what proves the policy installs, that its binding reaches it, and that the match covers the group and not its subresources.

**Integration tests** need `envtest` with the fleet CRDs, OCM's CRDs, and a mock control plane. What they prove that unit tests cannot is the delivery loop: an edit produces a `ManifestWork`, a change in that work's conditions reaches the object that asked for it, a detach orphans before it withdraws, and a refused write leaves a status that says so.

**End-to-end tests** need two clusters and a shared control plane, which is the harness this repository does not have. Enrollment, a build-out through a member, and a detach that leaves the member serving are the three that matter, and they are manual scenarios until that harness exists.

The risk concentrates in three places: the allowlist, where a missing identity breaks the member rather than the fleet; the detach, which is the only irreversible operation here; and the double-owner field set of §10.3, which no runtime check catches.

---

## 16. Rollout

The fleet is additive. Nothing in it changes an installed cluster until that cluster is enrolled, and enrollment is a `FleetMember` somebody creates.

1. **The hub.** Install OCM's `cluster-manager`, then the fleet CRDs, the fleet manager, and `config/addon/`. A `StorageFleet` names the control plane.
2. **The member.** Register it with OCM as a `ManagedCluster`. It is already running the operator and already pointing at the shared control plane through `ControlPlane.spec.source.external`.
3. **The member's grant.** Apply `fleet/config/member/`, which aggregates admission permissions into the work agent. Without it the add-on installs its allowlist and none of its policies, and the guard is silently absent (§10.6).
4. **Enrollment.** A `FleetMember` names the `ManagedCluster`. The fleet manager installs the add-on, and the member's status blocks begin to fill.
5. **The guard, in audit first.** The binding ships with `validationActions: ["Deny", "Audit"]`. A first rollout onto a cluster whose writers are not yet known carries `Audit` alone, and the `denied-requester` audit annotation is read until it is empty before the pair is restored. §10.2's upgrade tool is the identity this step exists to find.
6. **Deployments.** A `ClusterDeployment` is composed, reviewed, and approved, and the member builds out.

Detaching reverses step 4 alone. With `spec.detachPolicy: Retain`, what the fleet applied is orphaned rather than deleted, and the member is a standalone installation again.

---

## 17. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                          | Owner                   |
|-----|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|
| Q1  | Does the control plane say which Kubernetes cluster a storage cluster belongs to? `FleetMember.status.storage` and tier 1 of the read path rest on it. §11.2 states the fallback, which is that the member reports the identifiers it holds, and that fallback costs a round trip per member and is worth avoiding                                                                | Control plane (`sbcli`) |
| Q2  | Does the fleet depend on `multicloud-operators-foundation` for `ManagedClusterView`? It is the only source of a whole-object read, and it puts an agent this project does not version into every member. §9's agent already answers most of what tier 3 answers                                                                                                                   | —                       |
| Q3  | `ClusterDeploymentConfigSpec` carries its own `approved` field, so a `ClusterDeployment` has both `spec.approved` and `spec.config.approved`. Either the composition sets the embedded one when it ships the payload, or the outer field is dropped and the embedded one is the only gate. Left unanswered, an approved deployment lands in the member as a draft nobody approved | —                       |
| Q4  | `StorageDeviceOps` is specified in its design and not declared in `operator/api/v1alpha2`, so `FleetOperationTarget` enumerates five operations rather than six. Adding the sixth is an edit to the block and to its CEL rule                                                                                                                                                     | —                       |
| Q5  | A fleet-wide driver rollout is a set of `DriverDeployment` objects. OCM's `Placement` and `PlacementDecision` select the members, and what generates the set from a decision has a partial-success outcome that the `Ops` shape does not model, so it is an entity with a per-member status list                                                                                  | —                       |
| Q6  | What kind does the agent write into the member's OCM namespace? A `ConfigMap` needs no CRD and carries no schema, and a small kind of this group carries both. The choice decides what the agent's hub role grants                                                                                                                                                                | —                       |
| Q7  | The API upgrade tool writes objects under a human administrator's kubeconfig (§10.2), so a deny-by-default guard refuses the upgrade. Either the break-glass group is bound for the duration of an upgrade, or the tool gains an identity of its own                                                                                                                              | —                       |
| Q8  | `status.step.deadline` is an absolute `metav1.Time`. Clock skew makes a step's remaining time wrong at the hub, and a start instant with a duration fixes it. The blast radius is `atlas-lib/statemachine`, [`design-crd-model.md`](crd-redesign/design-crd-model.md) §3.1, and every `Ops` kind's `status.step`, so it is cheapest before those kinds ship                       | —                       |
| Q9  | Both halves of the picture can go stale at once, and for the same reason. Every block of `FleetMember.status` carries when it was last current, and a console that renders them as one panel loses the distinction                                                                                                                                                                | —                       |
| Q11 | Do members run Kubernetes 1.36 or later, and does the deployment control their API server configuration? Manifest-based admission policies are loaded from disk at startup and cannot be deleted through the API, which is the only mechanism that closes §10.2's ceiling. A payload cannot deliver one                                                                           | —                       |
| Q12 | `status` is outside the guard's match, and `status.activeOpsRef` is a mutual-exclusion lock on three kinds. Either the match grows to cover the status subresource, at the cost of admission on the hottest write path in the member, or the lock moves out of status                                                                                                             | —                       |
| Q10 | What is the oldest Kubernetes version a member may run? The guard is a `ValidatingAdmissionPolicy`, which reached general availability in 1.30. Below that the guard is a webhook, and §10's availability argument has to be answered rather than avoided                                                                                                                         | —                       |

---

## Appendix A: `storagefleet_types.go`

The fleet's singleton, and the control plane it is built on.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sf
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".status.version"
// +kubebuilder:printcolumn:name="Members",type=integer,JSONPath=".status.memberCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageFleet is the control plane a fleet is built on, and the fleet's
// defaults. One object per hub, named simplyblock.
type StorageFleet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageFleetSpec   `json:"spec,omitempty"`
	Status StorageFleetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageFleetList is a list of StorageFleet.
type StorageFleetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageFleet `json:"items"`
}

// StorageFleetSpec is where the fleet's control plane is, and what its
// deployments inherit.
type StorageFleetSpec struct {
	// ControlPlane is the control plane every member of this fleet is joined to.
	// It is immutable as a block, because re-pointing a live fleet at another
	// control plane produces a different fleet.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ControlPlane FleetControlPlane `json:"controlPlane"`

	// Defaults are the values a member inherits when it omits them.
	// +optional
	Defaults *FleetDefaults `json:"defaults,omitempty"`
}

// FleetControlPlane addresses the management API. It is the shape
// ControlPlane.spec.source.external takes, because it describes the same
// deployment from the other side.
type FleetControlPlane struct {
	// Endpoint is the management API's base URL.
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?(/.*)?$`
	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`

	// CredentialsSecretRef names a Secret in this namespace holding the token.
	// +kubebuilder:validation:Required
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

// FleetDefaults are fleet-wide values a member inherits.
type FleetDefaults struct {
	// OpsRetention is how long a terminal FleetOperation is kept.
	// +optional
	OpsRetention *metav1.Duration `json:"opsRetention,omitempty"`

	// InventoryRefreshInterval is how often a member is asked to re-probe.
	// +optional
	InventoryRefreshInterval *metav1.Duration `json:"inventoryRefreshInterval,omitempty"`
}

// StorageFleetStatus is what the control plane reports about itself.
type StorageFleetStatus struct {
	// Phase is whether the control plane answers.
	// +optional
	Phase StorageFleetPhase `json:"phase,omitempty"`

	// Endpoint is the resolved base URL, so that a reader asks status not spec.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Version is the management API's reported version.
	// +optional
	Version string `json:"version,omitempty"`

	// MemberCount is how many FleetMember objects this fleet has.
	// +optional
	MemberCount int32 `json:"memberCount,omitempty"`

	// LastChecked is when the readiness probe last ran.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`

	// Message is why the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// StorageFleetPhase is whether the fleet's control plane answers. There is no
// Installing value: the control plane exists before the hub does.
// +kubebuilder:validation:Enum=Available;Degraded;Unavailable
type StorageFleetPhase string

const (
	// StorageFleetPhaseAvailable means the management API answers.
	StorageFleetPhaseAvailable StorageFleetPhase = "Available"
	// StorageFleetPhaseDegraded means it answers, and reports a problem of its own.
	StorageFleetPhaseDegraded StorageFleetPhase = "Degraded"
	// StorageFleetPhaseUnavailable means it does not answer.
	StorageFleetPhaseUnavailable StorageFleetPhase = "Unavailable"
)
```

## Appendix B: `fleetmember_types.go`

One enrolled Kubernetes cluster, and the three status blocks of §8.2.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fm
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Workers",type=integer,JSONPath=".status.inventory.eligibleWorkers"
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=".status.storage.nodesOnline"
// +kubebuilder:printcolumn:name="Contact",type=date,JSONPath=".status.link.lastContact"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// FleetMember is one enrolled Kubernetes cluster. Its spec says which cluster
// and what a detachment does, because everything else about a member is
// observed rather than declared.
type FleetMember struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetMemberSpec   `json:"spec,omitempty"`
	Status FleetMemberStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FleetMemberList is a list of FleetMember.
type FleetMemberList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetMember `json:"items"`
}

// FleetMemberSpec identifies the Kubernetes cluster a member is.
type FleetMemberSpec struct {
	// ClusterRef names the Open Cluster Management ManagedCluster this member is.
	// That kind is cluster-scoped and its names are unique, so a bare name is
	// unambiguous.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// DetachPolicy is what happens to what the fleet applied when this member is
	// deleted. Retain orphans it, leaving a working standalone deployment.
	// +kubebuilder:default=Retain
	// +optional
	DetachPolicy FleetDetachPolicy `json:"detachPolicy,omitempty"`

	// InventoryRefreshInterval is how often the hub asks the member to re-probe
	// its workers. Absent inherits the fleet's default, and zero means never.
	// +optional
	InventoryRefreshInterval *metav1.Duration `json:"inventoryRefreshInterval,omitempty"`
}

// FleetDetachPolicy is what a member's deletion does to what the fleet applied.
// +kubebuilder:validation:Enum=Retain;Delete
type FleetDetachPolicy string

const (
	// FleetDetachPolicyRetain orphans what the fleet applied, so the member is
	// left a working standalone deployment.
	FleetDetachPolicyRetain FleetDetachPolicy = "Retain"
	// FleetDetachPolicyDelete removes what the fleet applied.
	FleetDetachPolicyDelete FleetDetachPolicy = "Delete"
)

// FleetMemberStatus is the member's readiness and three blocks that come from
// different places and go stale independently.
type FleetMemberStatus struct {
	// Phase is the member's readiness to be deployed into.
	// +optional
	Phase FleetMemberPhase `json:"phase,omitempty"`

	// Link is what the add-on reports about itself, and it goes stale with it.
	// +optional
	Link *FleetMemberLink `json:"link,omitempty"`

	// Inventory is a summary and never the reports, which stay in the member.
	// +optional
	Inventory *FleetMemberInventory `json:"inventory,omitempty"`

	// Storage is read from the control plane rather than from the member.
	// +optional
	Storage *FleetMemberStorage `json:"storage,omitempty"`

	// Message is why the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FleetMemberPhase is a member's readiness to be deployed into.
// +kubebuilder:validation:Enum=Pending;Enrolling;Ready;Degraded;Unreachable;Detaching
type FleetMemberPhase string

const (
	// FleetMemberPhasePending means the ManagedCluster is not accepted yet.
	FleetMemberPhasePending FleetMemberPhase = "Pending"
	// FleetMemberPhaseEnrolling means the add-on is being installed.
	FleetMemberPhaseEnrolling FleetMemberPhase = "Enrolling"
	// FleetMemberPhaseReady means the member can be deployed into.
	FleetMemberPhaseReady FleetMemberPhase = "Ready"
	// FleetMemberPhaseDegraded means the member answers, and something in it does not.
	FleetMemberPhaseDegraded FleetMemberPhase = "Degraded"
	// FleetMemberPhaseUnreachable means the member is not answering the hub.
	FleetMemberPhaseUnreachable FleetMemberPhase = "Unreachable"
	// FleetMemberPhaseDetaching means the member's deletion is being performed.
	FleetMemberPhaseDetaching FleetMemberPhase = "Detaching"
)

// FleetMemberLink is what the add-on reports about the member it runs in.
type FleetMemberLink struct {
	// AddOnVersion is the add-on's own version.
	// +optional
	AddOnVersion string `json:"addOnVersion,omitempty"`

	// OperatorVersion is the simplyblock operator the member runs.
	// +optional
	OperatorVersion string `json:"operatorVersion,omitempty"`

	// DriverVersion is the CSI driver the member runs.
	// +optional
	DriverVersion string `json:"driverVersion,omitempty"`

	// StorageAPIVersions are the versions of storage.simplyblock.io the member
	// serves, which is what makes schema skew visible before a payload is pruned
	// by it rather than after.
	// +optional
	StorageAPIVersions []string `json:"storageAPIVersions,omitempty"`

	// LastContact is when the add-on last reported.
	// +optional
	LastContact *metav1.Time `json:"lastContact,omitempty"`
}

// FleetMemberInventory summarizes what a member's workers have. The reports
// themselves stay in the member, because a fifty-worker set is tens of
// megabytes and one member's copy is all a composition needs.
type FleetMemberInventory struct {
	// Workers is how many the member has.
	// +optional
	Workers int32 `json:"workers,omitempty"`

	// EligibleWorkers is how many a cluster could be built on.
	// +optional
	EligibleWorkers int32 `json:"eligibleWorkers,omitempty"`

	// DeviceClasses are the classes present, so a fleet-wide list can answer
	// which members could take a cluster of a class without fetching a report.
	// +optional
	DeviceClasses []string `json:"deviceClasses,omitempty"`

	// RawCapacity is the sum of the candidate devices.
	// +optional
	RawCapacity *resource.Quantity `json:"rawCapacity,omitempty"`

	// Revision identifies the report set this summary was computed from.
	// +optional
	Revision string `json:"revision,omitempty"`

	// ObservedAt is when the summary was computed, which is what stops a stale
	// one reading as current.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// UnreadableReports counts workers whose report could not be parsed, which
	// is otherwise reported only by an event inside the member.
	// +optional
	UnreadableReports int32 `json:"unreadableReports,omitempty"`
}

// FleetMemberStorage is the control plane's count of what the member holds.
type FleetMemberStorage struct {
	// Clusters is how many storage clusters the member holds.
	// +optional
	Clusters int32 `json:"clusters,omitempty"`

	// Nodes is how many storage nodes those clusters have.
	// +optional
	Nodes int32 `json:"nodes,omitempty"`

	// NodesOnline is how many of them answer.
	// +optional
	NodesOnline int32 `json:"nodesOnline,omitempty"`

	// Pools is how many pools the member's clusters carry.
	// +optional
	Pools int32 `json:"pools,omitempty"`

	// DevicesDegraded is how many devices are serving and should not be.
	// +optional
	DevicesDegraded int32 `json:"devicesDegraded,omitempty"`

	// ObservedAt is when the control plane was last read, so that a stale
	// projection is distinguishable from a current one.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}
```

## Appendix C: `clusterdeployment_types.go`

The cluster document for one member, and the record of it landing.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cd
// +kubebuilder:printcolumn:name="Member",type=string,JSONPath=".spec.memberRef"
// +kubebuilder:printcolumn:name="Approved",type=boolean,JSONPath=".spec.approved"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".status.result.storageClusterName"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ClusterDeployment is the intent to build one storage cluster in one member,
// the document that will be applied there, and the record of it landing.
type ClusterDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterDeploymentSpec   `json:"spec,omitempty"`
	Status ClusterDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterDeploymentList is a list of ClusterDeployment.
type ClusterDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterDeployment `json:"items"`
}

// ClusterDeploymentSpec is the document and the approval.
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.config == oldSelf.config",message="config is immutable once approved"
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.approved",message="approval cannot be withdrawn"
type ClusterDeploymentSpec struct {
	// MemberRef names the FleetMember this cluster is built in.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	MemberRef string `json:"memberRef"`

	// Config is the document that will be applied in the member, composed from
	// that member's inventory. It is a typed storage-group spec rather than an
	// opaque blob because it is the thing a reviewer reads.
	// +kubebuilder:validation:Required
	Config storagev1alpha2.ClusterDeploymentConfigSpec `json:"config"`

	// Approved is the instruction to build, and setting it is the deployment
	// rather than a review stage in front of one.
	// +optional
	Approved bool `json:"approved,omitempty"`

	// ApprovedBy records the identity that approved, which the service account
	// replaying the edit into the member would otherwise erase. A webhook sets
	// it from the request's user info.
	// +optional
	ApprovedBy string `json:"approvedBy,omitempty"`
}

// ClusterDeploymentStatus is how far the build-out got, and what it produced.
type ClusterDeploymentStatus struct {
	// Phase is where the build-out is.
	// +optional
	Phase ClusterDeploymentPhase `json:"phase,omitempty"`

	// Step is the delivery machine's position.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Composing','Delivering','Applying','Expanding','Verifying']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// ConfigFingerprint is a hash of spec.config, and the delivery block's
	// AppliedFingerprint is the one that reached the member. The pair is what
	// makes an edit's arrival answerable without a generation the hub did not
	// issue.
	// +optional
	ConfigFingerprint string `json:"configFingerprint,omitempty"`

	// Delivery is what the ManifestWork carrying this document reports.
	// +optional
	Delivery *DeliveryStatus `json:"delivery,omitempty"`

	// Result names what the member built.
	// +optional
	Result *ClusterDeploymentResult `json:"result,omitempty"`

	// Message is why the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when delivery began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the build-out reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterDeploymentPhase is where a build-out is. Draft is a phase and not a
// step, because a document waiting for approval is not a delivery that stalled.
// +kubebuilder:validation:Enum=Draft;Delivering;Deploying;Ready;Degraded;Failed
type ClusterDeploymentPhase string

const (
	// ClusterDeploymentPhaseDraft means the document is not approved yet.
	ClusterDeploymentPhaseDraft ClusterDeploymentPhase = "Draft"
	// ClusterDeploymentPhaseDelivering means the payload is on its way to the member.
	ClusterDeploymentPhaseDelivering ClusterDeploymentPhase = "Delivering"
	// ClusterDeploymentPhaseDeploying means the member is expanding the document.
	ClusterDeploymentPhaseDeploying ClusterDeploymentPhase = "Deploying"
	// ClusterDeploymentPhaseReady means the cluster the document asked for is serving.
	ClusterDeploymentPhaseReady ClusterDeploymentPhase = "Ready"
	// ClusterDeploymentPhaseDegraded means it was built, and something in it is not well.
	ClusterDeploymentPhaseDegraded ClusterDeploymentPhase = "Degraded"
	// ClusterDeploymentPhaseFailed means the build-out will not proceed without a change.
	ClusterDeploymentPhaseFailed ClusterDeploymentPhase = "Failed"
)

// ClusterDeploymentStep is one step of the delivery machine.
// +kubebuilder:validation:Enum=Composing;Delivering;Applying;Expanding;Verifying
type ClusterDeploymentStep string

const (
	// ClusterDeploymentStepComposing builds the payload from the member's inventory.
	ClusterDeploymentStepComposing ClusterDeploymentStep = "Composing"
	// ClusterDeploymentStepDelivering writes the ManifestWork.
	ClusterDeploymentStepDelivering ClusterDeploymentStep = "Delivering"
	// ClusterDeploymentStepApplying waits for the work agent to apply it.
	ClusterDeploymentStepApplying ClusterDeploymentStep = "Applying"
	// ClusterDeploymentStepExpanding waits for the member's operator to expand the document.
	ClusterDeploymentStepExpanding ClusterDeploymentStep = "Expanding"
	// ClusterDeploymentStepVerifying reads back what the member built.
	ClusterDeploymentStepVerifying ClusterDeploymentStep = "Verifying"
)

// ClusterDeploymentResult identifies what the member built, so that a console
// reaches it without re-deriving the name.
type ClusterDeploymentResult struct {
	// StorageClusterName is the object's name in the member.
	// +optional
	StorageClusterName string `json:"storageClusterName,omitempty"`

	// StorageClusterUUID is the control plane's identifier, which is what the
	// roll-up and the projection address the same object by.
	// +optional
	StorageClusterUUID string `json:"storageClusterUUID,omitempty"`

	// Namespace is where the objects were created in the member. A payload
	// states its namespace rather than inheriting one, and this records what was
	// used. It bears no relation to the hub-side namespace the intent was
	// written in.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// NodesExpected is how many storage nodes the document asked for.
	// +optional
	NodesExpected int32 `json:"nodesExpected,omitempty"`

	// NodesReady is how many of them answered.
	// +optional
	NodesReady int32 `json:"nodesReady,omitempty"`
}
```

## Appendix D: `driverdeployment_types.go`

The CSI driver one member should run.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=dd
// +kubebuilder:printcolumn:name="Member",type=string,JSONPath=".spec.memberRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=".spec.driver.image"
// +kubebuilder:printcolumn:name="Running",type=string,JSONPath=".status.runningVersion"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// DriverDeployment is the CSI driver one member should run. It is one object
// per member, so a fleet-wide rollout is a set of them.
type DriverDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DriverDeploymentSpec   `json:"spec,omitempty"`
	Status DriverDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DriverDeploymentList is a list of DriverDeployment.
type DriverDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DriverDeployment `json:"items"`
}

// DriverDeploymentSpec is the driver payload for one member.
type DriverDeploymentSpec struct {
	// MemberRef names the FleetMember this driver is installed in.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	MemberRef string `json:"memberRef"`

	// Driver is the SimplyblockDriver spec that will be applied in the member.
	// +kubebuilder:validation:Required
	Driver storagev1alpha2.SimplyblockDriverSpec `json:"driver"`
}

// DriverDeploymentStatus is what the member's driver reports back.
type DriverDeploymentStatus struct {
	// Phase is where the installation is.
	// +optional
	Phase DriverDeploymentPhase `json:"phase,omitempty"`

	// RunningVersion is what the member's driver reports, fed back from the
	// applied object rather than assumed from the image in the spec.
	// +optional
	RunningVersion string `json:"runningVersion,omitempty"`

	// Delivery is what the ManifestWork carrying this driver reports.
	// +optional
	Delivery *DeliveryStatus `json:"delivery,omitempty"`

	// Message is why the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DriverDeploymentPhase is where a driver installation is.
// +kubebuilder:validation:Enum=Delivering;Installing;Ready;Degraded;Failed
type DriverDeploymentPhase string

const (
	// DriverDeploymentPhaseDelivering means the payload is on its way to the member.
	DriverDeploymentPhaseDelivering DriverDeploymentPhase = "Delivering"
	// DriverDeploymentPhaseInstalling means the member's operator is rolling the driver out.
	DriverDeploymentPhaseInstalling DriverDeploymentPhase = "Installing"
	// DriverDeploymentPhaseReady means the driver the spec asked for is serving.
	DriverDeploymentPhaseReady DriverDeploymentPhase = "Ready"
	// DriverDeploymentPhaseDegraded means it is installed, and part of it is not well.
	DriverDeploymentPhaseDegraded DriverDeploymentPhase = "Degraded"
	// DriverDeploymentPhaseFailed means the installation will not proceed without a change.
	DriverDeploymentPhaseFailed DriverDeploymentPhase = "Failed"
)
```

## Appendix E: `storageclassdeployment_types.go`

One `StorageClass` drawing on one pool in one member.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=scd
// +kubebuilder:printcolumn:name="Member",type=string,JSONPath=".spec.memberRef"
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=".spec.className"
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=".spec.pool.pool"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageClassDeployment is one StorageClass drawing on one pool in one member.
// A pool may have zero or more, because nothing about a pool implies a single
// way to consume it.
type StorageClassDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageClassDeploymentSpec   `json:"spec,omitempty"`
	Status StorageClassDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageClassDeploymentList is a list of StorageClassDeployment.
type StorageClassDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageClassDeployment `json:"items"`
}

// StorageClassDeploymentSpec is the class to write in the member, and the pool
// it draws on.
type StorageClassDeploymentSpec struct {
	// MemberRef names the FleetMember the class is written in.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	MemberRef string `json:"memberRef"`

	// ClassName is the StorageClass name in the member. It is immutable because
	// it is the object's name there, and a rename is a different class.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClassName string `json:"className"`

	// Pool is the pool this class draws on. The three fields become the three
	// assignment labels on the class, so an author states the pool and never
	// writes a label by hand.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Pool StorageClassPoolRef `json:"pool"`

	// Template is what the class carries beyond its assignment.
	// +kubebuilder:validation:Required
	Template StorageClassTemplate `json:"template"`
}

// StorageClassPoolRef locates a pool in the member. A class may draw on a pool
// in any namespace, which is why all three parts are stated rather than
// inherited from wherever the class happens to be written.
type StorageClassPoolRef struct {
	// Namespace is the pool's namespace in the member.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Cluster is the StorageCluster the pool belongs to.
	// +kubebuilder:validation:Required
	Cluster string `json:"cluster"`

	// Pool is the StoragePool's name.
	// +kubebuilder:validation:Required
	Pool string `json:"pool"`
}

// StorageClassTemplate is the class body. Everything here is editable, which is
// what makes re-tuning a ceiling an edit rather than a delete and a re-create.
type StorageClassTemplate struct {
	// Parameters are the driver parameters, written in the superseding QoS
	// spelling only, which is what the operator's own generated class does. The
	// three assignment labels are not parameters and are not written here.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// ReclaimPolicy is what happens to a volume when its claim goes.
	// +kubebuilder:validation:Enum=Delete;Retain
	// +optional
	ReclaimPolicy *corev1.PersistentVolumeReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// VolumeBindingMode is when a volume is provisioned.
	// +kubebuilder:validation:Enum=Immediate;WaitForFirstConsumer
	// +optional
	VolumeBindingMode *storagev1.VolumeBindingMode `json:"volumeBindingMode,omitempty"`

	// DisableVolumeExpansion turns off online expansion. The negative spelling
	// is what makes the zero value the default, and every class this product
	// writes today allows expansion.
	// +optional
	DisableVolumeExpansion bool `json:"disableVolumeExpansion,omitempty"`

	// MountOptions are passed to the mount of a volume of this class.
	// +optional
	MountOptions []string `json:"mountOptions,omitempty"`
}

// StorageClassDeploymentStatus is whether the class reached the member.
type StorageClassDeploymentStatus struct {
	// Phase is where the class is.
	// +optional
	Phase StorageClassDeploymentPhase `json:"phase,omitempty"`

	// Delivery is what the ManifestWork carrying this class reports.
	// +optional
	Delivery *DeliveryStatus `json:"delivery,omitempty"`

	// Message is why the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// StorageClassDeploymentPhase is where a class is. PoolMissing is its own value
// because a class naming a pool that is not there is a mistake somebody fixes,
// and not a delivery that failed.
// +kubebuilder:validation:Enum=Delivering;Ready;PoolMissing;Degraded;Failed
type StorageClassDeploymentPhase string

const (
	// StorageClassDeploymentPhaseDelivering means the payload is on its way to the member.
	StorageClassDeploymentPhaseDelivering StorageClassDeploymentPhase = "Delivering"
	// StorageClassDeploymentPhaseReady means the class is present in the member.
	StorageClassDeploymentPhaseReady StorageClassDeploymentPhase = "Ready"
	// StorageClassDeploymentPhasePoolMissing means the pool the class names is not there.
	StorageClassDeploymentPhasePoolMissing StorageClassDeploymentPhase = "PoolMissing"
	// StorageClassDeploymentPhaseDegraded means the class is present, and something about it is not well.
	StorageClassDeploymentPhaseDegraded StorageClassDeploymentPhase = "Degraded"
	// StorageClassDeploymentPhaseFailed means the delivery will not proceed without a change.
	StorageClassDeploymentPhaseFailed StorageClassDeploymentPhase = "Failed"
)
```

## Appendix F: `fleetoperation_types.go`

One operation of the storage group, shipped into one member.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fop
// +kubebuilder:printcolumn:name="Member",type=string,JSONPath=".spec.memberRef"
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=".status.target.kind"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Remote",type=string,JSONPath=".status.remote.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.remote.step"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// FleetOperation ships one storage-group Ops object into one member and reports
// what it does there. One kind rather than one per managed Ops kind, because the
// hub's question is which member and which operation, and the parameters belong
// to the kind whose spec it carries.
type FleetOperation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetOperationSpec   `json:"spec,omitempty"`
	Status FleetOperationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FleetOperationList is a list of FleetOperation.
type FleetOperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetOperation `json:"items"`
}

// FleetOperationSpec is a request, and every field but Abort is immutable.
type FleetOperationSpec struct {
	// MemberRef names the FleetMember the operation runs in.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	MemberRef string `json:"memberRef"`

	// Operation is what to run there. Exactly one member is set.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Operation FleetOperationTarget `json:"operation"`

	// Abort asks the member's own object to stop, by setting spec.abort on it
	// through the payload. It is the one mutable field on this spec, and the
	// member's graph decides whether the step it is on accepts it.
	// +optional
	Abort bool `json:"abort,omitempty"`
}

// FleetOperationTarget carries the spec of exactly one storage-group Ops kind.
// The discriminated block is the shape ControlPlane.spec.source uses.
// +kubebuilder:validation:XValidation:rule="[has(self.operatorOps),has(self.storageClusterOps),has(self.storageNodeOps),has(self.storagePoolOps),has(self.storageBackupOps)].filter(x, x).size() == 1",message="set exactly one operation"
type FleetOperationTarget struct {
	// OperatorOps runs an operator-level operation, today a discovery run.
	// +optional
	OperatorOps *storagev1alpha2.OperatorOpsSpec `json:"operatorOps,omitempty"`

	// StorageClusterOps runs a cluster-level operation.
	// +optional
	StorageClusterOps *storagev1alpha2.StorageClusterOpsSpec `json:"storageClusterOps,omitempty"`

	// StorageNodeOps runs a node-level operation.
	// +optional
	StorageNodeOps *storagev1alpha2.StorageNodeOpsSpec `json:"storageNodeOps,omitempty"`

	// StoragePoolOps runs a pool-level operation.
	// +optional
	StoragePoolOps *storagev1alpha2.StoragePoolOpsSpec `json:"storagePoolOps,omitempty"`

	// StorageBackupOps runs a backup or a restore.
	// +optional
	StorageBackupOps *storagev1alpha2.StorageBackupOpsSpec `json:"storageBackupOps,omitempty"`
}

// FleetOperationStatus is the hub's view of an operation running in a member.
type FleetOperationStatus struct {
	// Phase is the hub's own progress, which is about delivery rather than about
	// the operation. What the operation is doing is in Remote.
	// +optional
	Phase FleetOperationPhase `json:"phase,omitempty"`

	// Target names the object the payload created in the member, so that a
	// ManagedClusterView for the whole object needs no re-derivation.
	// +optional
	Target *FleetOperationTargetRef `json:"target,omitempty"`

	// Remote is what the member's own object reports, filled from the status
	// feedback rules on the ManifestWork. Scalars only, which is what a feedback
	// rule carries and what a list view needs. Anything beyond it is a
	// ManagedClusterView against Target.
	// +optional
	Remote *RemoteOpsStatus `json:"remote,omitempty"`

	// Delivery is what the ManifestWork carrying the payload reports.
	// +optional
	Delivery *DeliveryStatus `json:"delivery,omitempty"`

	// Message is why the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when the payload was delivered.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation reached a terminal remote phase, and is
	// what retention measures against.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ObservedGeneration is the generation this status was computed from. It
	// advances at most twice, and the second advance is the signal that Abort
	// was observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FleetOperationPhase is the hub's progress at getting the operation to run.
// +kubebuilder:validation:Enum=Pending;Delivering;Running;Succeeded;Failed;Aborted
type FleetOperationPhase string

const (
	// FleetOperationPhasePending means the payload has not been written yet.
	FleetOperationPhasePending FleetOperationPhase = "Pending"
	// FleetOperationPhaseDelivering means the payload is on its way to the member.
	FleetOperationPhaseDelivering FleetOperationPhase = "Delivering"
	// FleetOperationPhaseRunning means the member's own object is working.
	FleetOperationPhaseRunning FleetOperationPhase = "Running"
	// FleetOperationPhaseSucceeded means the member's object reached a terminal success.
	FleetOperationPhaseSucceeded FleetOperationPhase = "Succeeded"
	// FleetOperationPhaseFailed means it reached a terminal failure, or never arrived.
	FleetOperationPhaseFailed FleetOperationPhase = "Failed"
	// FleetOperationPhaseAborted means Abort was observed and the operation stopped.
	FleetOperationPhaseAborted FleetOperationPhase = "Aborted"
)

// FleetOperationTargetRef locates the object the payload created.
type FleetOperationTargetRef struct {
	// Kind is the storage-group kind that was created.
	// +optional
	Kind string `json:"kind,omitempty"`

	// Namespace is where it was created in the member.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name is the object's name in the member.
	// +optional
	Name string `json:"name,omitempty"`
}

// RemoteOpsStatus is what a feedback rule can carry off a storage-group Ops
// object. Every field is a scalar, because a JSON path feedback rule selects one
// value.
type RemoteOpsStatus struct {
	// Phase is the member object's own phase, in its own spelling.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Step is the state name of the member object's step machine. The deadline
	// beside it in the member is an absolute instant and is deliberately not
	// carried, because it is meaningless against the hub's clock.
	// +optional
	Step string `json:"step,omitempty"`

	// Message is the member object's status message.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the member object's, and is comparable only against
	// generations in the member.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ObservedAt is when the feedback last arrived.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}
```

## Appendix G: `delivery_types.go`

The delivery status three kinds of this group share.

```go
// DeliveryStatus is what the ManifestWork carrying a payload reports, and the
// only place a refused write can be seen from the hub.
type DeliveryStatus struct {
	// ManifestWorkName is the object in the member's namespace on the hub.
	// +optional
	ManifestWorkName string `json:"manifestWorkName,omitempty"`

	// AppliedFingerprint is the fingerprint of what actually reached the member.
	// +optional
	AppliedFingerprint string `json:"appliedFingerprint,omitempty"`

	// Conditions mirror the ManifestWork's Applied, Available, and Degraded,
	// each written against this object's own generation, which the hub issued
	// and can therefore compare.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```
