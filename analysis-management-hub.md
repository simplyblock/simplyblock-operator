# Fleet Management on Open Cluster Management

**Status:** Design sketch  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-09-11  
**Design:** none yet. This is the input to `design-management-hub.md`.

A single simplyblock control plane runs outside Kubernetes and manages several Kubernetes clusters, each of which runs
the operator. This document specifies that arrangement on Open Cluster Management: the kinds the hub owns, the kinds the
managed cluster keeps, what crosses between them, and what the hub serves to a management console. The link protocol,
the enrollment handshake, and the controllers belong to the design document that follows it.

---

## Table of Contents

1. [The Shape](#1-the-shape)
2. [Separate Kinds on Each Side](#2-separate-kinds-on-each-side)
3. [The Standalone Constraint](#3-the-standalone-constraint)
4. [What the Control Plane Already Knows](#4-what-the-control-plane-already-knows)
5. [The Two Bands](#5-the-two-bands)
6. [What Crosses, and What Does Not](#6-what-crosses-and-what-does-not)
7. [Band B: The Hub-Side Kinds](#7-band-b-the-hub-side-kinds)
8. [The Hub as a Gateway for `storage.simplyblock.io`](#8-the-hub-as-a-gateway-for-storagesimplyblockio)
9. [The Three Transports](#9-the-three-transports)
10. [The Add-Ons](#10-the-add-ons)
11. [Open Questions](#11-open-questions)

Appendix:

- [Appendix A: `fleet.simplyblock.io` types](#appendix-a-fleetsimplyblockio-types)

---

## 1. The Shape

There is exactly one control plane, and it is either local to a Kubernetes cluster or remote. A remote one fronts
several Kubernetes clusters, each running its own operator, which is the deployment
[`design-controlplane.md`](operator/docs/designs/crd-redesign/design-controlplane.md) §5.2 anticipates when it says an
external control plane may be shared and the operator must assume it is.

A hub runs beside that control plane, and each managed cluster runs an add-on. The hub holds the desired shape of every
managed cluster and ships it down, and the managed cluster reconciles it exactly as a standalone deployment does.

The hub holds no copy of a managed object. Desired state lives in hub-owned kinds distinct from the managed kinds, and
current state is served from the control plane the hub sits beside, joined with what the add-on reports. §2 states the
reasons.

---

## 2. Separate Kinds on Each Side

The hub and the managed cluster share no kind. A hub-side object carries intent and a managed-side object is what the
operator reconciles, and no object exists on both sides.

The alternative is a digital twin, where the hub owns each object's spec, the add-on replays it into a local copy no
user may write, the operator writes the local status, and the add-on mirrors that back. Six properties of this API rule
it out.

- **`metadata.generation` is issued by the local API server**, so a mirrored status carries a generation the hub never
  saw. [`design-crd-model.md`](operator/docs/designs/crd-redesign/design-crd-model.md) §7.9 makes `observedGeneration`
  mandatory because it is the only field that says a status is current, and across a twin it reports a
  definite-looking wrong answer. One field also cannot say whether the generation it reports is the hub's or the
  member's, so each side reads the other's write as an unobserved change and answers it, without end.
- **Defaulting and pruning mean the spec written is never the spec read back**, so an agent that writes, reads, diffs,
  and resynchronizes flaps forever.
- **UIDs do not travel.** `spec.creatorRef` carries one and
  [`design-persistentvolumeops.md`](operator/docs/designs/crd-redesign/design-persistentvolumeops.md) §11.1 calls it the
  load-bearing part, and owner references carry them too. The ownership spine cannot be reconstructed on the far side.
- **A refused write has nowhere to be reported.** Three designs put `failurePolicy: Fail` webhooks in front of creates,
  and a refused create writes no object, so there is no status to mirror.
- **`Ops` kinds are garbage-collected**, so an agent that reads local absence as work to do re-creates them, and a
  re-created `StorageNodeOps` with the `Remove` action drains a node a second time.
- **Cluster-scoped kinds have no hub-side layout**, and object names collide across members.

Each of these is a property of the twin, and each stops applying once the two sides share no kind.

[RamenDR](https://github.com/RamenDR/ramen), the disaster-recovery orchestrator behind OpenShift Data Foundation, is
built the same way. Its hub holds `DRPolicy`, `DRCluster`, and `DRPlacementControl`, and its managed clusters hold
`VolumeReplicationGroup`, `DRClusterConfig`, and `MaintenanceMode`. The two sets share no kind. A
`VolumeReplicationGroup` is composed by a hub controller and shipped as an opaque payload, and no hub-side copy of it
exists. One binary runs in one of two modes, `DRHubType` or `DRClusterType`, chosen in `cmd/main.go`, and each mode
registers a different set of controllers.

---

## 3. The Standalone Constraint

A standalone deployment against a local control plane keeps working exactly as it does today. The constraint is stated
as a test, so that a fleet concern cannot reach the product:

> A standalone installation installs no hub kind, runs no add-on, and its `CustomResourceDefinition` set, its RBAC, and
> its webhook configurations are byte-identical to what it installs today.

Two consequences follow. Hub kinds live in their own API group, installed only on the hub. And the agent is an add-on
rather than a mode of the operator, so the operator binary and its RBAC are unchanged. RamenDR selects its mode in one
binary, and this arrangement does not.

The operator therefore behaves identically in both deployments. It reconciles objects somebody wrote, and it draws no
distinction between a person and a work agent as the writer.

---

## 4. What the Control Plane Already Knows

The one control plane is the fleet's state store. Every design in this group reads its backend state from that control
plane's stream rather than from Kubernetes
([`design-crd-model.md`](operator/docs/designs/crd-redesign/design-crd-model.md) §7.7), and a hub sitting beside it
reads the same stream.

| Fact                                                   | Where the hub reads it                                                                                                                                       |
|--------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Cluster, node, pool, and volume state                  | The control plane, directly                                                                                                                                  |
| Device inventory and health                            | The control plane. `StorageDevice` is a pure projection of it ([`design-storagedevice.md`](operator/docs/designs/crd-redesign/design-storagedevice.md) §5.1) |
| Capacity and occupancy measurements                    | The control plane's own metrics                                                                                                                              |
| Which worker a node runs on, and whether its pod is up | The add-on. Kubernetes knows this and the control plane does not                                                                                             |
| What a worker has before a cluster exists on it        | The add-on, as the discovery inventory                                                                                                                       |
| Driver version and rollout state                       | The add-on                                                                                                                                                   |
| Claim-to-volume binding                                | The add-on, which is why a measurement kind is named after the claim                                                                                         |

The add-on therefore carries the Kubernetes-shaped half of the picture, which is small and specific, and it carries
intent downward. Most of what a console displays about storage is on the hub's side of the wire before the add-on
reports anything.

A disconnect degrades both halves. The control-plane half stays current only while the member's storage nodes still
reach the control plane, and an add-on that cannot reach the hub and nodes that cannot reach the control plane are
usually the same network event. The hub can always report when each half was last current, which is what separates a
partial answer from a wrong one.

---

## 5. The Two Bands

| Band | Group                               | Installed on                         | Contents                                                                      |
|------|-------------------------------------|--------------------------------------|-------------------------------------------------------------------------------|
| A    | `storage.simplyblock.io`, unchanged | Every standalone and managed cluster | Every kind in the target model, exactly as designed                           |
| B    | `fleet.simplyblock.io`, new         | The hub only                         | Intent kinds naming a member and composing a Band A object                    |
| —    | Open Cluster Management             | Hub and managed                      | `ManagedCluster`, `ManifestWork`, `ManagedClusterAddOn`, `ManagedClusterView` |

Band B stacks on Band A. Nothing is removed from Band A and nothing in it references Band B, so §3's test passes by
construction. In a managed cluster the Band A objects are the same objects, applied by the work agent instead of by a
person.

References run one way. A hub kind names a member, and no managed kind names a hub, which is what lets a managed
cluster lose its hub and keep reconciling.

---

## 6. What Crosses, and What Does Not

| Kind                      | Standalone              | Managed cluster                      | Crosses as                                                                  |
|---------------------------|-------------------------|--------------------------------------|-----------------------------------------------------------------------------|
| `ControlPlane`            | `source.managed`        | `source.external`, same kind         | Nothing                                                                     |
| `ControlPlaneOps`         | Applies                 | No action applies                    | Nothing                                                                     |
| `SimplyblockDriver`       | Written by a person     | `ManifestWork` payload               | Down, from `DriverDeployment`                                               |
| `ClusterDeploymentConfig` | Written or discovered   | `ManifestWork` payload               | Down, from `ClusterDeployment`                                              |
| `OperatorOps` discovery   | Written by a person     | Runs locally, triggered by a payload | Down as a trigger via `FleetOperation`, up as inventory                     |
| `StorageCluster`          | Written by a person     | Expanded from the config, as today   | Nothing. State comes from the control plane                                 |
| `StorageNode`             | Expanded by its cluster | Expanded by its cluster              | Nothing. The hub reads the control plane                                    |
| `StorageDevice`           | Projected               | Projected                            | Nothing. The hub reads the control plane                                    |
| `StoragePool`             | Written by a person     | `ManifestWork` payload               | Down, from a `StoragePool` payload. The backend half is the control plane's |
| `StorageClass`            | Authored by a person    | `ManifestWork` payload               | Down, from `StorageClassDeployment`                                         |
| Every `Ops` kind          | Written by a person     | Payload, or raised locally           | Down via `FleetOperation`                                                   |
| `PersistentVolumeOps`     | Written or fanned out   | Almost always fanned out locally     | Nothing. Cluster-scoped, high cardinality, local origin                     |
| `XyzMetrics`              | Served                  | Served                               | Nothing. Both read the control plane                                        |

Six kinds never cross in either direction.

A local object the control plane does not know is read straight from the member. A `StorageClass` is one, so a console
listing the classes that draw on a pool asks for them through a `ManagedClusterView` rather than finding them in the
projection. The same holds for any local object a Band A kind produced rather than mirrored.

---

## 7. Band B: The Hub-Side Kinds

`fleet.simplyblock.io/v1alpha1`, installed on the hub and nowhere else. Every convention below is
[`design-crd-model.md`](operator/docs/designs/crd-redesign/design-crd-model.md) §3 and §7 applied unchanged, so that the
two groups are one API to learn. The prefix for every label and annotation these kinds define is
`fleet.simplyblock.io`, one prefix per group, and metrics keep the `simplyblock_<entity>_<item>_<agg>` form.

The types are Appendix A, whole and as they are to be written. Each section below quotes the field its argument turns on
and no more, which is the shape
[`design-storagenode.md`](operator/docs/designs/crd-redesign/design-storagenode.md) §3 follows.

| Kind                     | Category | Short | Holds                                                                   |
|--------------------------|----------|-------|-------------------------------------------------------------------------|
| `StorageFleet`           | Entity   | `sf`  | The one control plane, and the fleet's defaults. Singleton              |
| `FleetMember`            | Entity   | `fm`  | One enrolled Kubernetes cluster, its link, its inventory, and a roll-up |
| `ClusterDeployment`      | Entity   | `cd`  | The composed `ClusterDeploymentConfig` for one member, and its approval |
| `DriverDeployment`       | Entity   | `dd`  | The `SimplyblockDriver` payload for one member                          |
| `StorageClassDeployment` | Entity   | `scd` | One `StorageClass` drawing on a pool in one member                      |
| `FleetOperation`         | Action   | `fop` | One Band A `Ops` object, shipped into one member                        |

Five of the six are entities, because building out a cluster is desired state: a document, a driver version, the classes
that consume the capacity, and a member to put them in. Issuing an operation is the one imperative act, and
`FleetOperation` carries it.

Ownership is a two-level tree. `FleetMember` owns every object that names it, so detaching a member collects its
deployments, its classes, and its operations. `StorageFleet` owns nothing, because a member outlives an edit to the
fleet's defaults and a singleton at the root of the tree makes one deletion a fleet-wide cascade.

### 7.1 Three Namespaces

Namespace means four different things across the boundary, and the distinction between any two of them is a privilege
boundary.

| Namespace                          | Side    | Holds                                                                         | Is the boundary for                    |
|------------------------------------|---------|-------------------------------------------------------------------------------|----------------------------------------|
| A tenant namespace                 | Hub     | Every Band B object: the member, its deployments, its classes, its operations | Who may operate on this tenant's fleet |
| The member's OCM namespace         | Hub     | `ManifestWork`, `ManagedClusterView`, and the add-on's registration           | Open Cluster Management's own layout   |
| The operator's namespace           | Managed | The operator workload, its webhooks, and its own configuration                | Who may operate the installation       |
| Any namespace a deployment chooses | Managed | Every Band A object a payload creates or a cluster expands                    | What a team may consume in the member  |

The first two are both on the hub and stay separate. Open Cluster Management requires a `ManifestWork` to live in its
`ManagedCluster`'s namespace, which is OCM's addressing rather than a tenancy decision, and a tenant needs no rights
there: write access to a member's OCM namespace is write access to any payload into that member, which is every
permission the fleet holds over it. The Band B object is therefore a tenant's to write, the `ManifestWork` is the
controller's, and the controller is the only thing that crosses between them.

The last two are both in the member, and the operator's own namespace is not where its objects live. The operator
workload runs in `simplyblock-system` and watches cluster-wide: `WATCH_NAMESPACE` is set by the chart and read nowhere,
and `operator/internal/upgrade/scope.go` states that a `StorageCluster` in any namespace belongs to the installation. A
`StorageCluster`, its nodes, and its pools therefore live wherever a deployment puts them, which is a choice a
standalone deployment already makes and which the hub makes too.

A payload states its namespace rather than inheriting one. There is no default to fall back on, so the document a
`ClusterDeployment` carries names the namespace its objects are created in, and `status.result.namespace` records what
was used. It bears no relation to the hub-side namespace the intent was written in.

The same fact fixes what the admission guard keys on. The set of namespaces a deployment may use is not knowable in
advance, so §10's guard keys on the work agent's field ownership, which travels with the object wherever it is written.

The `ManifestWork` objects these controllers create live in the member's OCM namespace, which is a different namespace
from the Band B object's, so an owner reference is unavailable for the reason
[`design-crd-model.md`](operator/docs/designs/crd-redesign/design-crd-model.md) §5 gives for the `StorageClass`. They
carry `fleet.simplyblock.io/managed-by`, and a finalizer performs the cascade, which is the group's existing pattern.

### 7.2 StorageFleet and FleetMember

`StorageFleet` is a singleton named `simplyblock`, matching `ControlPlane`'s convention: there is exactly one control
plane, and no kind carries a reference by which a controller could select among several. Its spec is where that control
plane is, and it is immutable as a block, because re-pointing a live fleet at another control plane produces a different
fleet.

`FleetMember` is one enrolled Kubernetes cluster, and its spec holds two fields:

```go
// ClusterRef names the Open Cluster Management ManagedCluster this member is.
// That kind is cluster-scoped and its names are unique, so a bare name is
// unambiguous.
// +kubebuilder:validation:Required
// +k8s:immutable
ClusterRef string `json:"clusterRef"`

// DetachPolicy is what happens to what the fleet applied when this member is
// deleted. Retain leaves the member a working standalone deployment.
// +kubebuilder:validation:Enum=Retain;Delete
// +kubebuilder:default=Retain
// +optional
DetachPolicy FleetDetachPolicy `json:"detachPolicy,omitempty"`
```

Everything else about a member is observed rather than declared. The status splits into three blocks that come from
different places and go stale independently: `link` is the add-on's own report, `inventory` is a summary of the
discovery reports, and `storage` is the roll-up read from the control plane. Each carries the time it was last current,
for the reason §4 gives.

Detaching is expressed as a deletion. Deleting a `FleetMember` is the instruction to unenroll, and a finalizer performs
it: patch every `ManifestWork` for the member to `propagationPolicy: Orphan` when the policy is `Retain`, delete them,
then withdraw the add-on. Orphaning before withdrawing is what leaves a working standalone deployment behind, and it is
why `deleteOption.propagationPolicy` is load-bearing.

`status.inventory` is a summary rather than the reports. A discovery run produces one `ConfigMap` per worker, each
admitted up to a mebibyte (`operator/internal/nodeprobe/configmap.go`), so a fifty-worker member is fifty objects and
tens of megabytes. A fleet-wide list needs the counts, the device classes, and the raw capacity. The full reports stay
in the member and are read through a `ManagedClusterView` when a deployment is composed.

### 7.3 ClusterDeployment and DriverDeployment

Both carry a Band A spec as their payload, and both are entities rather than operations, because a deployment is applied
rather than operated, which is the reading `ClusterDeploymentConfig` already has.

```go
// Config is the document that will be applied in the member, composed from that
// member's inventory. It is a typed Band A spec rather than an opaque blob
// because it is the thing a reviewer reads.
// +kubebuilder:validation:Required
Config storagev1alpha2.ClusterDeploymentConfigSpec `json:"config"`

// Approved is the instruction to build, and setting it is the deployment rather
// than a review stage in front of one.
// +optional
Approved bool `json:"approved,omitempty"`
```

Two CEL rules carry what markers cannot:

```go
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.config == oldSelf.config",message="config is immutable once approved"
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.approved",message="approval cannot be withdrawn"
```

The document is editable while it is a draft and frozen once approved, which is the one place this kind departs from
`ClusterDeploymentConfig`'s immutability. A draft composed at the hub is edited at the hub, and the Band A object is
created only once the hub-side document is settled, so what ships down is always an already-approved document and the
member never holds a draft nobody approved.

`spec.approvedBy` records the identity that approved. A webhook sets it from the request's user info, because the work
agent replaying the edit into the member is otherwise the only approver the audit trail carries, and
[`design-clusterdeploymentconfig.md`](operator/docs/designs/crd-redesign/design-clusterdeploymentconfig.md) §5 made
approval a spec field for its audit surface.

`DriverDeployment` is the same shape around `SimplyblockDriverSpec`, without the approval, because a driver version is
an edit rather than a deployment gate. It is one object per member, so a fleet-wide rollout is a set of them, and §11
carries the kind that would generate the set.

### 7.4 StorageClassDeployment

A class is how a claim asks a pool for capacity, and
[`design-storagepool.md`](operator/docs/designs/crd-redesign/design-storagepool.md) §5 settles the two facts that decide
the hub's shape for it.

A class is authored rather than generated, and a pool may have zero or more. One pool can back a class with compression
on, another with it off, one formatted `ext4`, one formatted `xfs`, a permissive ceiling for a batch tenant, and a tight
one for a latency-sensitive one. Configuring a class for a pool that already exists is therefore the ordinary case, and
it changes nothing about the pool.

The assignment is three labels on the class. `storage.simplyblock.io/namespace`, `.../cluster`, and `.../pool` are what
say which pool a class draws from, in either direction, and `StoragePool.status.storageClassNames` is the pool's side of
the same selector. Neither side owns the other.

One class is expanded locally and the rest come from the hub. A cluster creation writes one class for its default pool,
carrying `storage.simplyblock.io/managed-by: storagecluster`, and that happens in a managed cluster exactly as it does
in a standalone one, because it is Band A behavior the hub does not participate in. Every class beyond that first one is
authored, and on a managed cluster the hub authors it.

A `StorageClass` is therefore an independent object, shipped as its own payload.

```go
// Pool is the pool this class draws on, in the member. The three fields become
// the three assignment labels on the class, so an author states the pool and
// never writes a label by hand.
// +kubebuilder:validation:Required
// +k8s:immutable
Pool StorageClassPoolRef `json:"pool"`

// Template is what the class carries beyond its assignment. Parameters are
// written in the superseding QoS spelling only, which is what the operator's own
// generated class does.
// +kubebuilder:validation:Required
Template StorageClassTemplate `json:"template"`
```

A namespaced hub kind is what makes a cluster-scoped object delegable. `StorageClass` is cluster-scoped, and
`resourceNames` covers `get`, `update`, and `delete` but never `create` or `list`, so RBAC cannot express "may author
classes for this pool" against the class itself. A single cluster offers no namespaced object to hang that permission
on. The hub offers one: a pool's owner holds verbs on `storageclassdeployments` in their own namespace and needs no
rights on `StorageClass` anywhere, and the work agent writes the cluster-scoped object in the member under its own
identity. The delegation boundary sits at the hub, and the cardinality stays whatever the pool needs.

Deleting the Band B object deletes the class, without colliding with the operator. A `ManifestWork` with the default
`propagationPolicy: Foreground` removes what it applied, and §6 of the pool design lets the operator delete only a class
carrying `storage.simplyblock.io/managed-by: storagecluster`, which is the one class it generates for a default pool. A
hub-shipped class carries the fleet's marker, so each side deletes what it created and refuses on the other's.

`spec.className` is immutable, because it is the object's name in the member and a rename produces a different class.
Everything under `template` is editable, so re-tuning a ceiling is an edit rather than a delete and a re-create.

### 7.5 FleetOperation

One kind ships any Band A `Ops` object into one member.

```go
// Operation is the operation to run in the member. Exactly one member is set,
// and each is the spec of the Band A kind it names.
// +kubebuilder:validation:XValidation:rule="[has(self.operatorOps),has(self.storageClusterOps),has(self.storageNodeOps),has(self.storageDeviceOps),has(self.storagePoolOps),has(self.storageBackupOps)].filter(x, x).size() == 1",message="set exactly one operation"
// +kubebuilder:validation:Required
// +k8s:immutable
Operation FleetOperationTarget `json:"operation"`
```

The single kind is a deliberate break with the `<Entity>Ops` convention. That convention holds that a kind ending in
`Ops` is one-shot and names one target, and this kind is both. What it does not name is a Band B entity: its target is
an object in a member, and the parameters belong to the Band A kind whose spec it carries. One hub kind per managed
`Ops` kind reintroduces the twin for the one category where re-creating an object is dangerous.

The discriminated block follows the `ControlPlane.spec.source` pattern: one member per variant, exactly one set,
enforced in CEL. The block is typed rather than an opaque `RawExtension`, because this is the payload that performs a
side effect and it is the one that most needs validation.

Status comes back in two pieces, which is how a managed operation is observed from the hub:

```go
// Remote is what the member's own object reports, filled from the status
// feedback rules on the ManifestWork. Scalars only, which is what a feedback
// rule carries and what a list view needs.
// +optional
Remote *RemoteOpsStatus `json:"remote,omitempty"`
```

`RemoteOpsStatus` holds the phase, the step's state, the message, and the observed generation. Anything beyond that is a
`ManagedClusterView` against the object the payload created, on demand, which returns a whole and current object rather
than a copy of one.

The block enumerates the target set rather than what compiles today. Three of its members, `StorageDeviceOpsSpec`,
`StoragePoolOpsSpec`, and `StorageBackupOpsSpec`, are specified in their designs and declared nowhere in `operator/api`
yet, and two more are still at `v1alpha1`. That is the state of the tree rather than a constraint on this design: Band
A's redesign and Band B are one body of work, and the members exist by the time either ships.

---

## 8. The Hub as a Gateway for `storage.simplyblock.io`

A console reads the same kinds on the hub that it reads in a single cluster: one set of types, one client, one
authorization model. §5 keeps Band A off the hub as stored objects, and a kind is served by whatever is registered for
its group, which a `CustomResourceDefinition` is one of two ways to be.

The operator already runs an aggregated API server. `operator/internal/metricsapi/` is the implementation, and
`helm-charts/charts/simplyblock-operator/templates/metrics-apiserver.yaml` ships the `APIService` and the two delegation
bindings. [`design-crd-model.md`](operator/docs/designs/crd-redesign/design-crd-model.md) §7.13 states the trade: a kind
served that way is computed when a client asks for it and is never persisted, which is what `metrics.k8s.io` does for
`PodMetrics`. Its only consumer today is one measurement kind, which is the thinnest use of the mechanism rather than
its purpose. A serving path, a certificate rotator, an `APIService`, and the authorization delegation are all built, and
none of them knows what group it is serving.

The hub therefore registers `storage.simplyblock.io` as an `APIService` rather than as `CustomResourceDefinition`
objects, and serves it by calling the control plane it sits beside. `kubectl -n cluster-a get storageclusters` answers
on the hub, and the console's read path is the code it already has.

Two properties carry that. A client cannot tell the difference, because the group, the versions, the kinds, and the
verbs are identical over the wire whichever way they are registered. And authorization is unchanged, because the
delegation bindings the chart already ships are what make an ordinary `RoleBinding` on an aggregated group work.

A cluster serves a group one way or the other, never both, and each cluster chooses independently. A standalone or
managed cluster is CRD-backed and the hub is aggregated, so §3's test is untouched: the hub is neither.

### 8.1 Namespacing

The projection serves a member's objects in a namespace on the hub, and which one follows from the tenancy model rather
than from Open Cluster Management. Serving them in the tenant namespace of §7.1 lets one `RoleBinding` cover a tenant's
intent and the state it produced, at the cost of a disambiguator when one tenant holds several members. Serving them in
the member's own namespace inverts both. Either is a hub-side namespace and not the member's operator namespace.

Under either choice, names that would collide across members do not, and cluster-scoped Band A kinds become namespaced
on the hub with no translation the console has to know about.

### 8.2 Versions, and What the Projection Omits

Registration is per group-version. An `APIService` is named `<version>.<group>` and carries the two as separate fields,
which `operator/config/apiservice/apiservice.yaml` shows for `v1alpha1.metrics.simplyblock.io`. A gateway for
`storage.simplyblock.io/v1alpha2` is one object, and serving `v1alpha1` beside it is a second.

The gateway serves one version. A managed cluster cannot predate the arrangement, so it is a new installation, it starts
at `v1alpha2`, and the gateway registers `v1alpha2.storage.simplyblock.io` and nothing else. No conversion is needed,
because no second version is in play.

The cost is deferred rather than absent. An aggregated server gets no conversion webhook the way a
`CustomResourceDefinition` does, so it is responsible for every version it claims. The first hub that serves two
versions of the group at once owns a conversion the operator has already written for its own upgrade path.

A status in this group is two things, and only one is the control plane's.
[`design-storagedevice.md`](operator/docs/designs/crd-redesign/design-storagedevice.md) §4.2 draws the line in one kind
and it holds across the group: `status.phase` is the operator's own view, and `status.deviceStatus` is the control
plane's string kept in its own spelling. Everything on the operator's side is absent from a control-plane projection,
which means `status.phase`, `status.conditions`, `status.message`, `status.observedGeneration`, `status.activeOpsRef`,
`status.step`, and every `Ops` object.

The projection therefore serves an inventory console. It answers what the fleet has and how full it is. What an operator
is doing about a node right now comes from §9.

---

## 9. The Three Transports

Open Cluster Management defines three, and a console uses a different one per view.

| Transport             | Direction      | Carries                                                                                                                      |
|-----------------------|----------------|------------------------------------------------------------------------------------------------------------------------------|
| `ManifestWork`        | Hub to managed | A complete resource as an opaque payload, applied locally by the work agent, with its own conditions from pending to applied |
| Status feedback rules | Managed to hub | Named scalars selected by JSON path out of the applied resource, on a poll or a watch                                        |
| `ManagedClusterView`  | Managed to hub | One named object, fetched on demand, returned as raw JSON                                                                    |

Alongside the projection of §8 they make four tiers:

| Tier | Mechanism            | Serves                                               | Costs                                        |
|------|----------------------|------------------------------------------------------|----------------------------------------------|
| 1    | The projection       | Fleet-wide lists and search, from the control plane  | Nothing per member. It never leaves the hub  |
| 2    | Status feedback      | An operation's phase, step, and message in a list    | Scalars only, so a phase and not a condition |
| 3    | `ManagedClusterView` | One real object, whole, from the member's API server | A round trip, and the link has to be up      |
| 4    | `cluster-proxy`      | The member's API server directly, including events   | The same, plus an add-on and a tunnel        |

Tier 2 is what makes a fleet list of operations useful. A phase, a step state, and an observed generation are single
values, which is what a feedback rule carries, so a list of what is running across the fleet needs no per-object round
trip.

Tier 4 is optional and answers what the other three cannot. The `cluster-proxy` add-on runs a proxy server on the hub
and an agent in each member, and the agent dials the tunnel outbound, so a hub component reaches a member's
`kube-apiserver` by naming the member. Through it a console reads the actual objects and the actual `Events`, which live
in no object at all and which
[`design-crd-model.md`](operator/docs/designs/crd-redesign/design-crd-model.md) §3.3 identifies as the only thing
separating a queued operation from a new one. It uses the same outbound direction the work agent already does, so it
adds no egress path and no credential.

Tiers 3 and 4 answer about one member at a time. Which members have a node in `Degraded` is a fan-out over N clusters
with N failure modes, which is what tier 1 answers.

---

## 10. The Add-Ons

Three, of which the first two are required.

| Add-on                  | Does                                                                                                        |
|-------------------------|-------------------------------------------------------------------------------------------------------------|
| The inventory publisher | Publishes the merged discovery inventory after a discovery operation, and the Kubernetes-shaped facts of §4 |
| The admission guard     | Refuses a spec write to an agent-applied or agent-expanded object from any identity but the work agent      |
| `cluster-proxy`         | Optional. Tier 4 of §9, where single-object access is not enough                                            |

The guard ships with the add-on, which is what keeps §3's test true: a standalone cluster carries no guard because it
carries no add-on, and the operator's webhook configuration is unchanged. It keys on the work agent's field ownership
rather than on a list of kinds, so it needs no per-kind configuration.

Field-level exceptions are declared in the payload. `ManifestWork`'s `updateStrategy.type: ServerSideApply` takes a
`fieldManager` and an `ignoreFields` list, whose `condition: OnSpokePresent` stops reconciling a path once the resource
exists in the member. That is how the three `StorageNode` fields a migration writes stay the operator's while the rest
of the spec stays the hub's, and it is declarative and travels with the payload.

Two other `ManifestWork` settings are load-bearing. `updateStrategy.type: CreateOnly` ensures creation and never
re-applies, which is what makes a one-shot `Ops` payload safe. And `deleteOption.propagationPolicy` carries the detach
semantics of §7.2.

---

## 11. Open Questions

- **Both halves of the picture can be stale at once.** §4 states the reason: an add-on that cannot reach the hub and
  nodes that cannot reach the control plane are usually the same event. Every block of `FleetMember.status` carries when
  it was last current, and a console that renders them as one panel loses the distinction.
- **The gateway owes a conversion implementation the first time it serves two versions of the group** (§8.2). Not now,
  because a member is a new installation and starts at `v1alpha2`.
- **`status.step.deadline` is an absolute `metav1.Time`.** Clock skew makes a step's remaining time wrong at the hub. A
  start instant and a duration fix it, and the blast radius is `atlas-lib/statemachine`,
  [`design-crd-model.md`](operator/docs/designs/crd-redesign/design-crd-model.md) §3.1, and every `Ops` kind's
  `status.step`.
- **A discovery report that cannot be parsed is skipped with an event**, and its worker is left out of the draft.
  `FleetMember.status.inventory.unreadableReports` is where that becomes visible at the hub, as a count rather than a
  reason.
- **A fleet-wide driver rollout is a set of `DriverDeployment` objects.** A kind generating the set from a selection has
  a partial-success outcome to represent, which the `Ops` shape does not model, so it is an entity with a per-member
  status list and it needs its own working out.
- **Tenancy is mostly RBAC, and the quantities are an allocation envelope beside it.** Band B kinds are namespaced and
  the projection serves a member in its own namespace, so a tenant is a set of namespaces and ordinary `RoleBinding`
  objects separate them, including on the aggregated group. A quantity is what RBAC cannot express, and a separate
  draft, `simplyblock-multicluster-rbac-design.md`, is where that lives: an admin-owned, cluster-scoped allocation
  naming the nodes and devices a namespace may claim and the cores and hugepages it may take, enforced by a
  `ValidatingAdmissionPolicy` with the allocation as its `paramKind`, and a `ResourceQuota` per consumer namespace for
  capacity. That draft also settles whether a tenant owns whole Kubernetes clusters or pools inside a shared one, which
  is the structural choice underneath all of it. `resourceNames` does not apply to `list` or `watch`, so a pool admin in
  a shared namespace either lists every pool or is refused, with no filtered middle.
- **Neither of that draft's two arguments against the kinds here holds.** Its treatment of `StorageClass` assumes one
  class per pool, and §7.4 states why the count is whatever the pool needs and why a namespaced hub kind delegates the
  cluster-scoped object. Its rule that an action is a resource rather than a field would split `FleetOperation` into one
  kind per operation family, and §7.5 settles that the other way.

---

## Appendix A: `fleet.simplyblock.io` types

The six kinds of §7, whole. Package `fleet/v1alpha1`, importing `storagev1alpha2`
for the Band A specs §7 explains the coupling to, and `statemachine` for the step snapshot of
[`design-crd-model.md`](operator/docs/designs/crd-redesign/design-crd-model.md) §3.1.

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
	StorageFleetPhaseAvailable   StorageFleetPhase = "Available"
	StorageFleetPhaseDegraded    StorageFleetPhase = "Degraded"
	StorageFleetPhaseUnavailable StorageFleetPhase = "Unavailable"
)
```

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
	FleetDetachPolicyRetain FleetDetachPolicy = "Retain"
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
	FleetMemberPhasePending     FleetMemberPhase = "Pending"
	FleetMemberPhaseEnrolling   FleetMemberPhase = "Enrolling"
	FleetMemberPhaseReady       FleetMemberPhase = "Ready"
	FleetMemberPhaseDegraded    FleetMemberPhase = "Degraded"
	FleetMemberPhaseUnreachable FleetMemberPhase = "Unreachable"
	FleetMemberPhaseDetaching   FleetMemberPhase = "Detaching"
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

	// Nodes and NodesOnline are the member's storage nodes and how many answer.
	// +optional
	Nodes int32 `json:"nodes,omitempty"`
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

// ClusterDeploymentSpec is the document and the approval. The document is
// editable while it is a draft and frozen once approved, which is the one place
// this kind departs from ClusterDeploymentConfig's immutability.
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.config == oldSelf.config",message="config is immutable once approved"
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.approved",message="approval cannot be withdrawn"
type ClusterDeploymentSpec struct {
	// MemberRef names the FleetMember this cluster is built in.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	MemberRef string `json:"memberRef"`

	// Config is the document that will be applied in the member, composed from
	// that member's inventory. It is a typed Band A spec rather than an opaque
	// blob because it is the thing a reviewer reads.
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

	// ConfigFingerprint is a hash of spec.config, and Delivery.AppliedFingerprint
	// is the one that reached the member. The pair is what makes an edit's
	// arrival answerable without a generation the hub did not issue.
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
	ClusterDeploymentPhaseDraft      ClusterDeploymentPhase = "Draft"
	ClusterDeploymentPhaseDelivering ClusterDeploymentPhase = "Delivering"
	ClusterDeploymentPhaseDeploying  ClusterDeploymentPhase = "Deploying"
	ClusterDeploymentPhaseReady      ClusterDeploymentPhase = "Ready"
	ClusterDeploymentPhaseDegraded   ClusterDeploymentPhase = "Degraded"
	ClusterDeploymentPhaseFailed     ClusterDeploymentPhase = "Failed"
)

// ClusterDeploymentStep is one step of the delivery machine.
// +kubebuilder:validation:Enum=Composing;Delivering;Applying;Expanding;Verifying
type ClusterDeploymentStep string

const (
	ClusterDeploymentStepComposing  ClusterDeploymentStep = "Composing"
	ClusterDeploymentStepDelivering ClusterDeploymentStep = "Delivering"
	ClusterDeploymentStepApplying   ClusterDeploymentStep = "Applying"
	ClusterDeploymentStepExpanding  ClusterDeploymentStep = "Expanding"
	ClusterDeploymentStepVerifying  ClusterDeploymentStep = "Verifying"
)

// ClusterDeploymentResult identifies what the member built, so that a console
// reaches it through the projection without re-deriving the name.
type ClusterDeploymentResult struct {
	// StorageClusterName is the object's name in the member.
	// +optional
	StorageClusterName string `json:"storageClusterName,omitempty"`

	// StorageClusterUUID is the control plane's identifier, which is what the
	// roll-up and the projection address the same object by.
	// +optional
	StorageClusterUUID string `json:"storageClusterUUID,omitempty"`

	// Namespace is where the objects were created in the member.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// NodesExpected and NodesReady are what the document asked for and what
	// answered.
	// +optional
	NodesExpected int32 `json:"nodesExpected,omitempty"`
	// +optional
	NodesReady int32 `json:"nodesReady,omitempty"`
}

// DeliveryStatus is what the ManifestWork carrying a payload reports, and the
// only place a refused write can be seen from the hub. It is shared by every
// kind in this group that delivers one.
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

// DriverDeploymentSpec is the driver payload for one member. There is no
// approval field: a driver version is an edit rather than a deployment gate.
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
	DriverDeploymentPhaseDelivering DriverDeploymentPhase = "Delivering"
	DriverDeploymentPhaseInstalling DriverDeploymentPhase = "Installing"
	DriverDeploymentPhaseReady      DriverDeploymentPhase = "Ready"
	DriverDeploymentPhaseDegraded   DriverDeploymentPhase = "Degraded"
	DriverDeploymentPhaseFailed     DriverDeploymentPhase = "Failed"
)
```

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
	StorageClassDeploymentPhaseDelivering  StorageClassDeploymentPhase = "Delivering"
	StorageClassDeploymentPhaseReady       StorageClassDeploymentPhase = "Ready"
	StorageClassDeploymentPhasePoolMissing StorageClassDeploymentPhase = "PoolMissing"
	StorageClassDeploymentPhaseDegraded    StorageClassDeploymentPhase = "Degraded"
	StorageClassDeploymentPhaseFailed      StorageClassDeploymentPhase = "Failed"
)
```

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

// FleetOperation ships one Band A Ops object into one member and reports what it
// does there. One kind rather than one per managed Ops kind, because the hub's
// question is which member and which operation, and the parameters belong to the
// kind whose spec it carries.
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

// FleetOperationTarget carries the spec of exactly one Band A Ops kind. The
// discriminated block is the shape ControlPlane.spec.source uses, and it is
// typed rather than opaque because this is the payload that performs a side
// effect.
// +kubebuilder:validation:XValidation:rule="[has(self.operatorOps),has(self.storageClusterOps),has(self.storageNodeOps),has(self.storageDeviceOps),has(self.storagePoolOps),has(self.storageBackupOps)].filter(x, x).size() == 1",message="set exactly one operation"
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

	// StorageDeviceOps runs a device-level operation.
	// +optional
	StorageDeviceOps *storagev1alpha2.StorageDeviceOpsSpec `json:"storageDeviceOps,omitempty"`

	// StoragePoolOps runs a pool-level operation.
	// +optional
	StoragePoolOps *storagev1alpha2.StoragePoolOpsSpec `json:"storagePoolOps,omitempty"`

	// StorageBackupOps runs a backup or restore.
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
	FleetOperationPhasePending    FleetOperationPhase = "Pending"
	FleetOperationPhaseDelivering FleetOperationPhase = "Delivering"
	FleetOperationPhaseRunning    FleetOperationPhase = "Running"
	FleetOperationPhaseSucceeded  FleetOperationPhase = "Succeeded"
	FleetOperationPhaseFailed     FleetOperationPhase = "Failed"
	FleetOperationPhaseAborted    FleetOperationPhase = "Aborted"
)

// FleetOperationTargetRef locates the object the payload created.
type FleetOperationTargetRef struct {
	// Kind is the Band A kind that was created.
	// +optional
	Kind string `json:"kind,omitempty"`

	// Namespace and Name locate it in the member.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
}

// RemoteOpsStatus is what a feedback rule can carry off a Band A Ops object.
// Every field is a scalar, because a JSON path feedback rule selects one value.
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
