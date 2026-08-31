# Design Document: The Deployment Config and the Operator's Own Operations

**Status:** Draft  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-08-29 (last updated 2026-08-30)  
**Target Release:** simplyblock 26.4  
**Test Plan:** [`tests/test-plan-clusterdeploymentconfig.md`](../../tests/test-plan-clusterdeploymentconfig.md)  
**Example:** [`assets/example-cluster-config.yaml`](assets/example-cluster-config.yaml)  
**Example:** [`assets/operator-ops-discover.yaml`](assets/operator-ops-discover.yaml)  
**Diagram:** [`assets/deployment-discovery.jpg`](assets/deployment-discovery.jpg)

Both kinds are new. Nothing in this document exists, which makes §11 a list of
what it replaces rather than a migration. 26.4 is the release the CRD redesign
ships in, so it is the first release in which any of this is true, including the
backend's acceptance of logical block devices as backend storage (§8.1).

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [ClusterDeploymentConfig: API](#3-clusterdeploymentconfig-api)
4. [ClusterDeploymentConfig: Controller](#4-clusterdeploymentconfig-controller)
5. [Approval Is a Field, and a Webhook Enforces It](#5-approval-is-a-field-and-a-webhook-enforces-it)
6. [Expansion Is Create-Only](#6-expansion-is-create-only)
7. [OperatorOps](#7-operatorops)
8. [Discovery](#8-discovery)
9. [Observability](#9-observability)
10. [Testing Strategy](#10-testing-strategy)
11. [What This Replaces](#11-what-this-replaces)
12. [Open Questions](#12-open-questions)

Appendices:

- [Appendix A: `clusterdeploymentconfig_types.go`](#appendix-a-clusterdeploymentconfig_typesgo)
- [Appendix B: `operatorops_types.go`](#appendix-b-operatorops_typesgo)

---

## Overview

`ClusterDeploymentConfig` is a whole simplyblock deployment written down as one
reviewable document: the environment it targets, the cluster to create, and the
node sets with their workers, interfaces, and devices. An administrator reviews
it, approves it, and the operator expands it into a `StorageCluster` and its
`StorageNode` objects.

`OperatorOps` is the operator's own operations kind. It has no target reference
because its target is the operator process
([`design-crd-model.md`](design-crd-model.md) §3), and it carries the discovery
run that produces a deployment config in the first place.

**The document is ephemeral, and that is its defining property.** Everything the
expansion produces is self-describing, so the config can be edited or deleted the
moment it has been expanded ([`design-crd-model.md`](design-crd-model.md) §6,
[`design-storagenode.md`](design-storagenode.md) §3.1). It is a deployment
instruction, not a source of truth, and the difference is what makes it safe to
throw away.

---

## 1. Background

There are two ways a simplyblock deployment comes into existence today, and
neither of them is reviewable.

**A fresh deployment is a set of hand-written custom resources.** The chart
renders no storage topology. It installs the operator, its CRDs, its RBAC, and the
CSI driver, and where it is asked to, the control plane. Beside them it ships
`operator_customresources.yaml`, a commented sample of a `StorageCluster`, a
`StoragePool`, and a `StorageNodeSet` that an administrator copies, edits, and
applies. The worker list and the `nodeConfigs` map are written by hand into three
objects that are applied one at a time, and nothing compares them against the
machines they name until the operator acts on them.

**Nothing tells an administrator what the fleet can actually carry.** Which
workers have devices free, how much memory and how many cores each has left, and
which of them are already hosting storage are all knowable from the cluster, and
all of it is worked out by hand and typed into a `nodeConfigs` map. A wrong PCI
address or a device that turns out to be mounted is found when the operator acts
on it, which is after the document was written and approved.

That is what this document adds. Discovery inspects the workers of a Kubernetes
cluster and writes what they have free as one document. Somebody reads it,
corrects it, approves it, and the operator expands it.

---

## 2. Goals and Non-Goals

### Goals

- Specify a document that describes a whole deployment, so that a fleet's
  hardware is reviewed once as one object rather than as three applied in
  sequence (§3).
- Specify the approval gate, and why it is a spec field rather than a mark in
  metadata (§5).
- Specify what expansion produces, and that it happens once (§4, §6).
- Specify what a config does when it disagrees with what is already running,
  which [`design-crd-model.md`](design-crd-model.md) §6 leaves open (§6).
- Specify `OperatorOps`, the one action it carries, and why upgrading the
  operator is not a second one (§7).
- Specify discovery: what it inspects, what it can and cannot know, and what it
  writes (§8).

### Non-Goals

- **Not the kinds it expands into.** `StorageCluster` is
  [`design-storagecluster.md`](design-storagecluster.md) and `StorageNode` is
  [`design-storagenode.md`](design-storagenode.md). This document specifies what
  the expansion writes into them, not what they then do.
- **Not the control plane.** Whether one exists, and whether the operator
  installs it, is [`design-controlplane.md`](design-controlplane.md). A
  deployment config assumes a `ControlPlane` that reports `Ready` and fails
  informatively when there is not one.
- **Not a GitOps integration.** The document is an ordinary custom resource, so
  it works with whatever applies resources. Nothing here is specific to Argo CD
  or Flux.
- **Not hardware inventory.** Discovery reports the devices a node advertises. It
  does not decide which of them simplyblock should use, which is the reviewer's
  job and the reason for the approval gate.

---

## 3. ClusterDeploymentConfig: API

Declared in `operator/api/v1alpha1/clusterdeploymentconfig_types.go`, short name
`cdc`. The type is Appendix A and a filled-in document is
[`assets/example-cluster-config.yaml`](assets/example-cluster-config.yaml). What
follows quotes the field an argument turns on and no more.

### 3.1 Shape

The document has three parts: what the deployment is, what cluster to make, and
which nodes to make it out of.

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: ClusterDeploymentConfig
metadata:
  name: production
  namespace: simplyblock
spec:
  approved: true
  environment: OpenShift
  edgeCluster: false
  cluster:
    name: production
    maxSubsystemCount: 20
    vcpuCount: 8
    stripe:
      dataChunks: 2
      parityChunks: 1
  nodeSets:
    - name: rack-a
      sizing:
        maxSubsystemCount: 20
        vcpuCount: 8
        minHugePagesSize: 16G
      groups:
        - name: saturn
          workers: [worker-3, worker-4]
          mgmtInterface: eth1
          dataInterfaces: [eth2]
          devices:
            nvme: ["0000:5e:00.0", "0000:5f:00.0"]
        - name: mars
          workers: [worker-5]
          mgmtInterface: eth1
          dataInterfaces: [eth2]
          devices:
            block: [/dev/sdb, /dev/sdc]
```

**Three levels of grouping, and each one earns its place.** A node set carries
sizing, because sizing is what a node set is for
([`design-crd-model.md`](design-crd-model.md) §9.2) and because it is what a
rolling hardware upgrade re-scopes
([`design-storagenode.md`](design-storagenode.md) §3.1). A group carries the
per-node configuration that identical hardware shares, so that ten workers with
the same NVMe layout are written once rather than ten times. A worker is a
hostname.

The middle level is `groups` rather than `members`, because an entry is several
workers sharing one configuration rather than one of anything.

`spec.edgeCluster` names a fact about the deployment rather than switching a
capability on, which is the class
[`design-crd-model.md`](design-crd-model.md) §7.5 leaves outside the `enableXyz`
and `disableXyz` rule, alongside `ubuntuHost` and `openShiftCluster`.

**`spec.environment` is a shorthand, and the expansion is where it is spent.** A
distribution decides whether the kubelet is reconfigured, whether CPU topology is
read, and which host assumptions hold, which
[`design-storagenode.md`](design-storagenode.md) §5.1 carries as
`enableKubeletConfiguration`, `enableCpuTopology`, `ubuntuHost`, and
`openShiftCluster` on each node. Naming `OpenShift` once decides all four, and
`CreatingNodes` stamps them onto every `StorageNode` it creates (§4.2).

That is the same relationship `sizing` has, and it is what keeps the document
ephemeral: the nodes carry the resolved flags, so deleting the config loses
nothing. A group that needs one flag against its distribution's default sets that
flag on the node afterward, because `environment` is a starting point and not a
lock.

**A group's devices are an explicit list, never a filter.** `nvme` names NVMe
devices by PCI address and `block` names logical block devices by path, which are
the two classes simplyblock accepts as backend storage, and between them they
are every device the group's workers hand to simplyblock. Both expand into the
node's `config.deviceNames`, which takes a PCI address and a device path in one
list ([`design-storagenode.md`](design-storagenode.md) §3.1).

**Each member validates the shape it accepts**, so `nvme` takes a PCI address and
nothing else and `block` takes a path under `/dev` and nothing else. The node's
own field is looser, because it still accepts the bare device name a registered
manifest may already carry, and a config is new enough to have no such history.
Splitting the two members is what makes the validation possible at all: one mixed
list could only be checked against the union of both shapes, which accepts a PCI
address written where a path was meant.

**A group may name both classes, and mostly should not.** Nothing rejects a group
whose workers hand over an NVMe device and a SATA disk together, because the node
accepts the mixed list, but the two classes have different performance and
different failure behavior and a group is the unit that is supposed to be
uniform. Draft validation says so in `status.message` (§4.1) and blocks nothing:
it is a deployment that is usually a mistake and occasionally exactly what was
meant, which is the shape of an advisory rather than a rule. There is no allow list to evaluate, no
model to match, and no size range to fall inside, because a document whose
meaning depends on what the hardware turns out to be is not a document a reviewer
can approve. Filters belong to the run that produces the list, and §8.1 is where
they are given.

**A group expands to one `StorageNode` per worker per slot.** The slot count comes
from the cluster's `spec.storageNodes.socketsToUse` and `nodesPerSocket`
([`design-storagenode.md`](design-storagenode.md) §5.1), so a group of two
workers on a two-socket layout produces four nodes, each carrying the group's
configuration and the set's sizing.

### 3.2 Immutability

**Everything is immutable once `spec.approved` is true.** Before approval the
document is a draft and a reviewer edits it freely. After approval it is the
record of what was deployed, and editing it would describe a deployment that
never happened.

That is one CEL rule on the spec rather than a marker per field:

```go
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self == oldSelf",message="an approved deployment config is immutable"
```

**`spec.approved` itself is immutable once true.** Un-approving something already
expanded does not un-expand it, and a field that can be toggled back invites the
belief that it does.

Both rules are restated by the validating webhook of §5, which is what turns a
rejection naming a CEL rule into one naming the field that was edited.

### 3.3 Status

`status.phase` is `Draft`, `Expanding`, `Expanded`, or `Failed`.

`Draft` is an unapproved document, and it is the phase a discovery run's output
starts in. Nothing reconciles a `Draft` beyond validating it, which is what makes
review safe: a document that is wrong is a document that has done nothing.

`status.clusterRef` names the `StorageCluster` the expansion produced or added
to, and `status.nodeRefs` lists the `StorageNode` objects, so that a config that
has been expanded says what it expanded into. Those references are what make the
document safe to delete: they are a record, not a dependency, and nothing
resolves them after expansion.

`status.observedGeneration`, `status.message`, and `status.step` follow the group
conventions ([`design-crd-model.md`](design-crd-model.md) §3.1, §7.9).

---

## 4. ClusterDeploymentConfig: Controller

`ClusterDeploymentConfigReconciler`, in
`operator/internal/controllers/deployment/clusterdeploymentconfig_controller.go`.

### 4.1 Reconcile

```
  CR observed
    │
    ▼
  phase Expanded or Failed? ← terminal, return
    │  no
    ▼
  spec.approved false?      ← Validating: check the document, stay Draft
    │  approved
    ▼
  ControlPlane Ready?       ← hold, emit ControlPlaneNotReady
    │  yes
    ▼
  The expansion machine (§4.2)
```

**A `Draft` is validated on every reconcile and expanded on none.** Validation
writes what it found into `status.message` and nothing else, so a reviewer sees
the problems before approving rather than after. That is the whole value of the
gate: a document that names a worker which does not exist should say so while it
is still a draft, which is also why admission lets such a draft be saved (§5.1).

**This is the only check a device gets before the deployment runs**, since
admission does not look at devices (§5.1). A config discovery wrote cannot be
wrong about them, because the list came from the inspection. A hand-written one
can, and `DeviceNotFound` in `status.message` is what says so while the document
is still editable. Approving without reading it is how a device mistake becomes
an immutable document, and no mechanism below the reviewer prevents that.

### 4.2 The expansion machine

```
  approved, control plane ready
    │
    ▼
  Validating        ← workers exist, devices are plausible, no conflict (§6)
    │  clean
    ▼
  CreatingCluster   ← create or resolve the StorageCluster
    │
    ▼
  AwaitingCluster   ← wait for status.uuid
    │
    ▼
  CreatingNodes     ← one StorageNode per worker per slot
    │
    ▼
  phase: Expanded
```

**`CreatingNodes` resolves the document's shorthands as it writes.** Each node
gets its set's `sizing`, its group's devices as one `config.deviceNames` list
(§3.1), and the four distribution flags `spec.environment` stands for. Nothing on
the node refers back to the config, which is what §4.3 means by owning nothing.

**`CreatingNodes` is the step that must be idempotent, and it is by construction.**
A `StorageNode` is identified by `(clusterRef, workerNode, slot)`
([`design-storagenode.md`](design-storagenode.md) §3.1), so the step lists what
exists for the cluster and creates only the slots that do not. A crash part-way
through creates the rest on the next pass and duplicates nothing.

**The expansion does not wait for the nodes to come up.** It creates the objects
and finishes. Provisioning them is the node controller's, it is bounded by
`maxParallelNodeAdds`, and a document that stayed `Expanding` until a
twenty-node fleet was online would be reporting the fleet's progress rather than
its own.

### 4.3 Deletion

**There is no finalizer, and that is deliberate.** Deleting a
`ClusterDeploymentConfig` deletes a document. It owns nothing, nothing references
it, and nothing reads it after expansion, so there is nothing to clean up and
nothing to protect.

Specifically, it does **not** own the `StorageCluster` it created. An owner
reference would make deleting the document delete the cluster and every volume in
it, which is the opposite of ephemeral.

---

## 5. Approval Is a Field, and a Webhook Enforces It

Approval is `spec.approved`, a field on the document rather than a mark in its
metadata. [`design-crd-model.md`](design-crd-model.md) §3 states the reason in its
own terms: a trigger kept in metadata is untyped and unvalidated, has no status, and
presents no RBAC surface.

A spec field is typed, so `approved: "yes"` is rejected at admission rather than
silently read as unset. It is visible in a diff, so a GitOps review shows the
approval as a change rather than as metadata churn. And it is the thing
`status.phase` reports on, so `Draft` against `Expanding` is a legible pair.

`storage.simplyblock.io/ready-to-deploy` is an output. The operator sets it on an
approved config so that a selector can find one, and reads it from nowhere.

### 5.1 The approving edit is the one that is validated

`ClusterDeploymentConfigValidator`, in
`operator/internal/webhook/clusterdeploymentconfig_validator.go`, is a validating
admission webhook on `create` and `update`. It treats the two kinds of edit
differently, and the asymmetry is the design.

**A draft is admitted on its structure alone.** The OpenAPI schema and the CEL
rules of §3.2 are the whole check. A draft may name a worker that does not exist
and a device no node advertises, because a document that could not be saved until
it was correct is a document nobody can work on, and the controller reports what
it found in `status.message` (§4.1) so that a reviewer fixes it in place.

**The edit that sets `spec.approved` to true is validated against the Kubernetes
API.** Every worker named by every group has to exist as a Node,
`spec.clusterRef` has to resolve to a `StorageCluster` when it is set and to
nothing when it is not (§6), and no other approved config may already own the
cluster this one would create. All three are answerable from objects the operator
already caches, which is what makes them cheap enough to answer inside an
admission request.

**Devices are not on that list, because deciding what a cluster could use is
discovery's job (§8).** The inspection that knows whether a device is mounted,
busy, or partitioned runs per node and against the node, not against the API
server, and an admission handler is the wrong place to wait for it. What the
webhook would gain by asking is a check the draft has already been given.

**Admission is the right place for the checks it does keep, because §3.2 makes
the document immutable.** A config that reaches `Failed` on a misspelled worker
cannot be corrected, only deleted and rewritten, and the approval that produced
it is already in the audit trail. Rejecting the approving apply costs one error
message. Admitting it costs a dead document and a `Failed` record of a deployment
that never happened.

**`Validating` stays the first step of the expansion machine, and it is where a
device is checked.** The webhook answers for the cluster as it stood at the moment
of approval, and a worker can be cordoned in the seconds before the controller
acts, so §4.2 checks again. It is also the step that can afford the per-node
inspection admission cannot, so a device that stopped being available between the
draft and the expansion is caught there.

### 5.2 The rules are enforced in two places

The CEL rules are the floor and they hold whatever else is installed. The webhook
restates them because the two rejections say different things: CEL names the rule
that failed, and the webhook names the field that changed and what to do instead,
which for an edit after approval is to write a second config naming the cluster
in `spec.clusterRef` (§6).

**`failurePolicy: Fail`**, for the reason `StorageNodeValidator` already gives in
this repository. The webhook server runs inside the operator pod, so its
availability tracks the operator's, and when the operator is down nothing expands
anyway. A window in which approvals are admitted unchecked is a window in which
immutable mistakes are created, and that is worse than a window in which no
document can be approved.

**Nothing is exempt, which is where this webhook parts company with its
sibling.** `StorageNodeValidator` admits the operator's own service account and
denies everyone else, because only the operator may re-point a node. Here the
operator needs no such exemption: an `OperatorOps` run writes drafts (§8.3), and
a draft is what the permissive path already admits. An exemption on the strict
path would be worse than useless, because a config the operator could approve and
an administrator could not is a gate with a hole in it.

**Approving is not a review step in front of the deployment. It is the
deployment.** Setting `spec.approved` is the instruction to build the cluster,
and the word describes who does it rather than a governance stage the document
passes through. Kubernetes authorizing the whole resource is therefore correct
rather than a limitation: whoever may write a `ClusterDeploymentConfig` in this
namespace is whoever may deploy simplyblock into it, and those are the same
authority.

That is also why nothing here separates approving from editing. A separate
approval kind, or a `SubjectAccessReview` against a verb no editor holds, would
divide one action into two permissions and leave the first of them able to
deploy a cluster by writing a document that is already approved.

---

## 6. Expansion Is Create-Only

[`design-crd-model.md`](design-crd-model.md) §6 leaves open "what the generated
object is reviewed against, and what happens when it disagrees with what is
already running." This is that answer.

**A config creates or adds. It never reconciles a difference.**

| `spec.cluster.name` resolves to | `spec.clusterRef` | Expansion does                             |
|---------------------------------|-------------------|--------------------------------------------|
| No existing `StorageCluster`    | absent            | Creates the cluster and all its nodes      |
| An existing `StorageCluster`    | absent            | Refuses: `ClusterExists`, phase `Failed`   |
| An existing `StorageCluster`    | set to it         | Adds only the nodes that do not exist yet  |
| No existing `StorageCluster`    | set               | Refuses: `ClusterNotFound`, phase `Failed` |

**A config describes one cluster.** A deployment with two clusters in one
namespace is two documents, and the expansion never produces or extends more than
the one `spec.cluster` or `spec.clusterRef` names.

**Growth is a second document, not an edit of the first.** Adding a rack means
writing a config that names the existing cluster in `spec.clusterRef` and lists
only the new node sets. That keeps every config a record of one deployment
action, keeps them all immutable after approval, and means the audit trail of how
a cluster reached its current size is a series of documents rather than a series
of revisions to one.

**A config never removes anything.** A node set dropped from a later document
does not drain a node, and a device removed from a group does not shrink a node.
Removal is destructive and belongs to `StorageNodeOps`
([`design-storagenode.md`](design-storagenode.md) §8), where it is deliberate,
audited, and drains first. A document that could remove nodes by omission would
make a typo a data-loss event.

**Refusing rather than merging is the conservative half of a decision that has a
real cost.** An administrator whose config names an existing cluster by accident
gets a `Failed` document and a clear reason, which is cheap to fix. The
alternative, merging, would have the operator decide what a difference means, and
the differences that matter here are of the form *this node's device list
changed*, whose only correct handling is not to apply it to a node that already
has data on those devices.

---

## 7. OperatorOps

Declared in `operator/api/v1alpha1/operatorops_types.go`, short name `oops`, and
reconciled by `OperatorOpsReconciler` in
`operator/internal/controllers/deployment/operatorops_controller.go`, beside the
config its discovery action writes. The type is Appendix B.

**`OperatorOps` carries no target reference.** Every other `Ops` kind in the
group names the entity it acts on
([`design-crd-model.md`](design-crd-model.md) §3), and this one acts on the
operator process, which is not a resource in this API group. The absent field is
the signal, and the kind's name is the target.

**With no target there is no `activeOpsRef` to release, and the finalizer stays.**
`storage.simplyblock.io/operatorops-finalizer` is what holds a run in `Terminating`
until it reaches a terminal phase, so a delete arriving mid-`Probing` cannot leave
node inspections running with nothing recording them
([`design-crd-model.md`](design-crd-model.md) §3.1). Two runs at once are possible
for the same reason a lock is not: they read and write nothing the other depends on,
and each names its own output through `spec.discover.configName` (§8.1).

**`OperationQueued` is therefore declared and unreachable**, which is the case
[`design-crd-model.md`](design-crd-model.md) §3.3 makes for keeping the six reasons
closed: this kind emits it if it ever gains something to wait on, and every consumer
already handles it. It is the same shape as `OperationAborted` on a kind whose graph
declares no abort edge.

```go
// +kubebuilder:validation:Enum=Discover
type OperatorOpsAction string
```

| Action     | Steps                                | Produces                               |
|------------|--------------------------------------|----------------------------------------|
| `Discover` | `Inspecting` → `Probing` → `Writing` | A `ClusterDeploymentConfig` in `Draft` |

[`assets/operator-ops-discover.yaml`](assets/operator-ops-discover.yaml) is what the
three cases look like as objects: the smallest run that does anything, a run narrowed to
the workers and devices somebody intends to use, and the same object once it has
answered.

**The action field stays, with one value in it.** Every `Ops` kind in the group
is dispatched on `spec.action`
([`design-crd-model.md`](design-crd-model.md) §3), so a kind that left its single
action implicit would be the one kind whose shape has to change the day it gains
a second.

### 7.1 Upgrading the operator is not one of its actions

Upgrading the operator is the obvious second action. `OperatorOps` does not
carry it.

**The chart's entire scope is the operator's own lifecycle.** It renders the
operator Deployment, the CRDs, the RBAC, and the CSI driver, and it renders no
storage topology at all (§1). Whoever installed simplyblock therefore already
holds the mechanism that upgrades the operator, and on OpenShift, OLM holds a
second one. An `Upgrade` action would be a third implementation of the one thing
the existing two are for, and unlike them it would have no way to roll the CRDs
and the RBAC forward with the image.

**It would also run inside the thing it operates on.** The pod that patches the
Deployment is the pod the rollout replaces, so it never observes the outcome.
Carrying an operation across that gap costs a step persisted before the patch, a
record whatever comes up has to recognize as its own, and a deadline that turns a
version which never converges into a failure. All of it is machinery for a state
`helm upgrade` reaches directly.

So the operator is upgraded by whatever installed it, which is the same answer
for a Helm release, an OLM subscription, and a GitOps controller reconciling the
chart. `Discover` justifies the kind on its own: it is a one-shot operation
against no resource, producing a resource, which is exactly the shape an `Ops`
kind exists for.

---

## 8. Discovery

`Discover` inspects what is there and writes what it found. It changes nothing.

### 8.1 What it inspects

| Source                                                | Yields                                                      |
|-------------------------------------------------------|-------------------------------------------------------------|
| The Kubernetes API's node list                        | Worker hostnames, labels, taints, and allocatable resources |
| Each node's available NVMe devices                    | Candidate devices, by PCI address (§8.2)                    |
| Each node's available block devices, where enabled    | Candidate devices, by path (§8.2)                           |
| Node labels, annotations, and cluster-scoped services | `spec.environment`, from the markers a distribution leaves  |
| `StorageNode` objects in the namespace                | Which workers and devices are already taken                 |

**Discovery reads the cluster it runs in and nothing else.** What a worker has
free is a property of the worker, so the Kubernetes API and the node itself are
where both halves of it are read. There is no control-plane client and no backend
call, which is why this document has no backend API requirements.

**A discovery run reports only what is unclaimed, which is what makes re-running
it useful.** A worker that already carries a `StorageNode`, and a device that node
already names in its `spec.config`, are not candidates, so a run against a deployed
fleet finds the machines and disks nobody has taken yet. What it writes is
therefore a growth document (§6): the same `spec.clusterRef` and only the node
sets that would be new. Re-running discovery after every expansion is how a fleet
is grown without anybody writing a worker list by hand.

**`spec.environment` is detected, not asked for.** A distribution leaves
evidence in several places at once: the labels and annotations its installer puts
on nodes, the services and API groups only it registers, and the node images it
boots. Discovery reads them together and writes the distribution it concluded,
which is what makes the field a finding a reviewer corrects rather than a
question a reviewer answers. §3.1 is what the answer then buys.

It also means two documents written against one Kubernetes cluster agree on it
without anybody coordinating, since both runs read the same evidence.

**Two classes of backend storage, and one of them is opt-in.** NVMe devices are
the class simplyblock has always accepted, and logical block devices are the
class 26.4 adds, so discovery has to be told which of them it is looking for.
`spec.discover.deviceFilter.enableLogicalBlockDevices` reports both classes, and
leaving it unset reports NVMe only. Unset is the conservative default for two
reasons. Upgrading to 26.4 must not change what a discovery run reports, and the
block class is the broader and the less uniform of the two even after the
availability rule of §8.2 has excluded everything in use, so a deployment that
only ever meant to use NVMe should not have to review a longer list in order to
say so.

The two classes are the two members of a group's `devices` (§3.1). NVMe
candidates become `nvme` entries and block candidates become `block` entries, so
what discovery scans decides which half of that block a draft can carry.

**`spec.discover.deviceFilter` narrows the candidates before they are written.**
It carries the four filters `StorageNode` accepts, a PCI allow list, a PCI deny
list, a model, and a size range, and it is the only place in either kind that a
device is described by a rule. The three PCI filters narrow the NVMe class alone,
because a logical block device has no PCI address to match, and `driveSizeRange`
narrows both. On a fleet that is uniform about which slot holds
the boot device, one deny list keeps that device out of every group of every
draft, which is the difference between a reviewer correcting one document and a
reviewer correcting each of twenty groups.

**The filter is an input, and it is not written down.** What the draft carries is
the explicit list the filter produced (§3.1), so a config states which devices a
group uses rather than the rule that selected them, and re-running the filter
later against changed hardware cannot change what a reviewer already approved.

### 8.2 What it cannot know

**Working out what the cluster could use is what this phase is for.** The
question a discovery run answers is which of a fleet's machines and disks are
free to be given to simplyblock, and everything below is how it decides. Nothing
later re-derives it: admission does not look at devices (§5.1), and the expansion
re-checks the answer rather than recomputing it.

**What a node reports is its available devices.** Available is a narrow word,
and it means all three of not mounted, not busy in any other way, and carrying no
partitions. A device failing any of those is in use, or looks enough like it that
an inspection cannot tell, so the inspection excludes it rather than handing a
reviewer the job of noticing.

**Partitions are the one condition an administrator can override.**
`spec.discover.deviceFilter.enablePartitionedDevices` reports partitioned devices
alongside the available ones, for the case where the partition table is stale and
the device is meant to be handed over anyway. Naming such a device in a group is
then how it is forced into use (§3.1). Mounted and busy stay absolute: a device
another subsystem is writing to is one simplyblock would corrupt, and a flag that
turned that into an intention would be a flag for losing data.

**Which of the available devices simplyblock should use.** Available means
nothing is using the device today, which is not the same as this cluster being
the thing that should own it. A scratch disk, a device staged for another system,
and a disk an operator left unmounted mid-repair are indistinguishable to an
inspection. Discovery lists what it found and does not choose, which is the
single largest reason the approval gate exists. A `deviceFilter` narrows the list
further, and narrowing is not choosing: the filter is written by the same
administrator who then reviews the result, so it moves the judgment earlier
rather than removing it.

**How workers should be grouped.** It groups by identical hardware, which is a
guess at intent: two racks with identical machines are one group by that rule and
two by any sensible operational one. A reviewer regroups.

**What the failure domains are.** Rack and power topology is not in the
Kubernetes API. Where nodes carry `topology.kubernetes.io/zone` discovery uses it
as a starting point, and where they do not it leaves `failureDomain` unset, which
holds provisioning with a clear reason when the cluster requires it
([`design-storagenode.md`](design-storagenode.md) §4.2) rather than guessing.

### 8.3 The output is a draft, always

Discovery writes `spec.approved: false` and never anything else, including on a
re-run. A second discovery writes a second document rather than editing the
first, because the first may have been reviewed and edited, and overwriting a
reviewer's corrections with a fresh guess is the worst behavior available.

---

## 9. Observability

Both kinds are new, so both tables are new infrastructure.

### 9.1 Kubernetes events

| Event                                                    | Type      | Reason                   | On                        |
|----------------------------------------------------------|-----------|--------------------------|---------------------------|
| A draft names a worker that does not exist               | `Warning` | `WorkerNotFound`         | `ClusterDeploymentConfig` |
| A draft names a device no node advertises                | `Warning` | `DeviceNotFound`         | `ClusterDeploymentConfig` |
| A draft has a group mixing NVMe and block devices        | `Warning` | `MixedDeviceClasses`     | `ClusterDeploymentConfig` |
| A draft is valid and awaiting approval                   | `Normal`  | `AwaitingApproval`       | `ClusterDeploymentConfig` |
| Expansion is held because the control plane is not ready | `Warning` | `ControlPlaneNotReady`   | `ClusterDeploymentConfig` |
| Expansion refused: the cluster already exists            | `Warning` | `ClusterExists`          | `ClusterDeploymentConfig` |
| Expansion refused: `clusterRef` names no cluster         | `Warning` | `ClusterNotFound`        | `ClusterDeploymentConfig` |
| Expansion created the cluster                            | `Normal`  | `ClusterCreated`         | `ClusterDeploymentConfig` |
| Expansion created the nodes                              | `Normal`  | `NodesCreated`           | `ClusterDeploymentConfig` |
| A step's deadline expired                                | `Warning` | `StepDeadlineExceeded`   | `ClusterDeploymentConfig` |
| Discovery could not read a node's devices                | `Warning` | `DeviceInspectionFailed` | `OperatorOps`             |
| Discovery wrote a config                                 | `Normal`  | `ConfigWritten`          | `OperatorOps`             |
| The run is waiting for another to finish (§7)            | `Normal`  | `OperationQueued`        | `OperatorOps`             |
| The run started                                          | `Normal`  | `OperationStarted`       | `OperatorOps`             |
| The operation finished successfully                      | `Normal`  | `OperationSucceeded`     | `OperatorOps`             |
| The operation failed                                     | `Warning` | `OperationFailed`        | `OperatorOps`             |
| The run was aborted and its unwind finished              | `Normal`  | `OperationAborted`       | `OperatorOps`             |

**No event reports a rejected approval.** An admission rejection fails the
request, so what the administrator gets is the webhook's message on their own
terminal and there is no object to record it against. The signal for that path is
the metric below.

**`MixedDeviceClasses` is the one that does not block anything.** It is a
`Warning` because a group whose workers hand over an NVMe device and a SATA disk
together is usually a mistake, and it is only an event because occasionally it is
not (§3.1). Approval proceeds either way.

**`AwaitingApproval` is the one that changes how the kind is used.** A valid
draft that nobody has approved looks identical to a controller that has not
noticed it, and the event is what distinguishes them. It is emitted once, on the
transition to a validated draft, not on every reconcile.

### 9.2 Prometheus metrics

| Metric                                                           | Labels                          | Description                                                                                                       |
|------------------------------------------------------------------|---------------------------------|-------------------------------------------------------------------------------------------------------------------|
| `simplyblock_clusterdeploymentconfig_phase_state`                | `namespace`, `name`, `phase`    | Gauge, 1 for the current phase, so a config stuck in `Draft` is visible                                           |
| `simplyblock_clusterdeploymentconfig_expansion_duration_seconds` | `namespace`                     | Histogram from approval to `Expanded`                                                                             |
| `simplyblock_clusterdeploymentconfig_nodes_created_total`        | `namespace`                     | Nodes an expansion created, which is the fleet's growth over time                                                 |
| `simplyblock_clusterdeploymentconfig_validation_failures_total`  | `namespace`, `reason`           | Draft validation failures by reason, which is what a reviewer keeps hitting                                       |
| `simplyblock_clusterdeploymentconfig_approval_rejections_total`  | `namespace`, `reason`           | Approving edits the webhook rejected, by reason, which is the gate catching a mistake before it becomes immutable |
| `simplyblock_operator_operations_total`                          | `namespace`, `action`, `result` | Operations reaching a terminal phase                                                                              |
| `simplyblock_operator_operation_duration_seconds`                | `namespace`, `action`           | Histogram of operation durations                                                                                  |
| `simplyblock_operator_discovery_workers_found_count`             | `namespace`                     | Gauge of workers the last discovery run found                                                                     |
| `simplyblock_operator_discovery_devices_found_count`             | `namespace`                     | Gauge of candidate devices it found                                                                               |

**`validation_failures_total` by reason is the one worth building first.** It is
the only signal that says whether the approval gate is working as review or as an
obstacle: a deployment where every draft fails on `DeviceNotFound` is a discovery
bug, not a careful reviewer.

**`approval_rejections_total` is the pair it should be read against.** The two
count the same mistakes at different moments, and a rejection that the draft
validation never reported first is a validation gap, since a reviewer should have
been told before the approval was written.

---

## 10. Testing Strategy

Scenarios live in
[`tests/test-plan-clusterdeploymentconfig.md`](../../tests/test-plan-clusterdeploymentconfig.md)
and only there.

Expansion is a pure function of a document plus the cluster's current state, so
most of it is unit-testable against a fake client: what a group of two workers on
a two-socket layout produces, that re-expansion creates nothing, that a config
naming an existing cluster without `clusterRef` is refused. The webhook of §5 is
a pure function too, of an admission request and a lister, so its decisions are
unit tests and only its wiring needs a cluster.

Immutability after approval and the asymmetry between a draft and an approving
edit both belong in `envtest`, because CEL and admission are enforced by the API
server and a fake client applies neither. That the permissive path really is
permissive matters as much as that the strict path is strict: a webhook that
rejected invalid drafts would break discovery and review together, and nothing
below the API server would notice.

The risk that unit tests do not reach is concentrated in discovery, which reads a
real Kubernetes cluster's nodes and a real control plane's inventory. Whether a
device is mounted, busy, or partitioned is the node's own answer and not a fake
client's (§8.2), and the list that survives those three checks is the input the
approval gate exists to correct.

---

## 11. What This Replaces

Nothing is migrated, because neither kind exists. What changes is which of the
existing paths remain.

| Today                                                                         | After                                                                                              |
|-------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------|
| Three custom resources are copied from the chart's sample and applied by hand | A config is expanded into a cluster and its nodes                                                  |
| `StorageNodeSet.spec.nodeConfigs[worker]`                                     | `spec.nodeSets[].groups[].devices` and the interfaces beside them                                  |
| `StorageNodeSet.spec.workerNodes`                                             | `spec.nodeSets[].groups[].workers`                                                                 |
| Set-wide sizing on `StorageCluster`                                           | `spec.nodeSets[].sizing`, stamped per node ([`design-storagenode.md`](design-storagenode.md) §3.1) |

**This is the last thing the `StorageNodeSet` retirement was waiting for.**
[`design-crd-model.md`](design-crd-model.md) §9.2 names three things the set
carried: the fleet template and `nodeConfigs`, which land in `spec.nodeSets[]`
here; the Kubernetes workload, which
[`design-storagenode.md`](design-storagenode.md) §5 moved to `StorageCluster`;
and `status.drainCoordination`, which the same document turned into a
`StorageNodeOps` action. With this document the retirement has no undesigned
piece left.

---

## 12. Open Questions

**Q1: What each `KubernetesEnvironment` value resolves to.** §3.1 has
`spec.environment` decide `enableKubeletConfiguration`, `enableCpuTopology`,
`ubuntuHost`, and `openShiftCluster` on every node the expansion creates, and
names `OpenShift` as the value a reader can already infer. `Vanilla`, `Rancher`,
`K3s`, and `Talos` have no stated mapping. The table belongs in this document,
because the expansion is what applies it, and writing it needs one answer per
distribution about whether the kubelet is reconfigured and whether CPU topology
is readable, which is a question for whoever has run simplyblock on each of them.

---

## Appendix A: `clusterdeploymentconfig_types.go`

The type as it is to be written. Everything the sections above show in Go is an
excerpt of this appendix, and this is the only place any type appears whole.

```go
// ClusterDeploymentConfigPhase is where the operator has got to with this
// document.
// +kubebuilder:validation:Enum=Draft;Expanding;Expanded;Failed
type ClusterDeploymentConfigPhase string

const (
	// ClusterDeploymentConfigPhaseDraft is an unapproved document. It is
	// validated on every reconcile and expanded on none.
	ClusterDeploymentConfigPhaseDraft ClusterDeploymentConfigPhase = "Draft"
	ClusterDeploymentConfigPhaseExpanding ClusterDeploymentConfigPhase = "Expanding"
	ClusterDeploymentConfigPhaseExpanded  ClusterDeploymentConfigPhase = "Expanded"
	ClusterDeploymentConfigPhaseFailed    ClusterDeploymentConfigPhase = "Failed"
)

// ClusterDeploymentConfigStep is one step of the expansion path.
// +kubebuilder:validation:Enum=Validating;CreatingCluster;AwaitingCluster;CreatingNodes
type ClusterDeploymentConfigStep string

const (
	ClusterDeploymentConfigStepValidating      ClusterDeploymentConfigStep = "Validating"
	ClusterDeploymentConfigStepCreatingCluster ClusterDeploymentConfigStep = "CreatingCluster"
	ClusterDeploymentConfigStepAwaitingCluster ClusterDeploymentConfigStep = "AwaitingCluster"
	ClusterDeploymentConfigStepCreatingNodes   ClusterDeploymentConfigStep = "CreatingNodes"
)

// KubernetesEnvironment is the distribution a deployment targets. The values are
// the distributions' own names, which is the exception design-crd-model.md §7.8
// carries for a word this group did not invent.
// +kubebuilder:validation:Enum=Vanilla;OpenShift;Rancher;K3s;Talos
type KubernetesEnvironment string

// DeviceSelection is the explicit list of storage devices a group's workers hand
// to simplyblock. It carries no filter of any kind: a document whose meaning
// depends on what the hardware turns out to be is not a document a reviewer can
// approve, so filtering happens in the discovery run that produces the list and
// what lands here is the result. It expands to the matching fields of
// StorageNode.spec.config, which carry the same meanings.
//
// +kubebuilder:validation:XValidation:rule="has(self.nvme) || has(self.block)",message="a device selection names NVMe addresses, block devices, or both"
type DeviceSelection struct {
	// Nvme names NVMe devices by PCI address ("0000:5e:00.0").
	// +kubebuilder:validation:items:Pattern=`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`
	// +listType=set
	// +optional
	Nvme []string `json:"nvme,omitempty"`

	// Block names logical block devices by path ("/dev/sdb"). It expands into the
	// same config.deviceNames as Nvme, which takes a PCI address and a device
	// path in one list. A group setting both is accepted and discouraged, since a
	// group is meant to be uniform hardware.
	// +kubebuilder:validation:items:Pattern=`^/dev/[a-zA-Z0-9._/-]+$`
	// +listType=set
	// +optional
	Block []string `json:"block,omitempty"`
}

// NodeGroup is a set of workers that share one configuration, which is what
// makes ten identical machines one entry rather than ten.
type NodeGroup struct {
	// Name identifies the group within its node set, for a reader and for the
	// events a validation failure emits.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Workers are the Kubernetes worker hostnames in this group.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=200
	// +listType=set
	// +kubebuilder:validation:Required
	Workers []string `json:"workers"`

	// MgmtInterface is the management network interface the storage nodes bind.
	// +optional
	MgmtInterface string `json:"mgmtInterface,omitempty"`

	// DataInterfaces are the data-plane network interfaces.
	// +optional
	DataInterfaces []string `json:"dataInterfaces,omitempty"`

	// Devices selects the storage devices every worker in the group uses.
	// +optional
	Devices *DeviceSelection `json:"devices,omitempty"`

	// FailureDomain is the fault group every worker in this group belongs to.
	// Discovery leaves it unset where the Kubernetes API carries no topology,
	// which holds provisioning with a clear reason rather than guessing.
	// +kubebuilder:validation:Minimum=0
	// +optional
	FailureDomain *int32 `json:"failureDomain,omitempty"`

	// SpdkSystemMemory is the memory the control plane starts SPDK with on these
	// nodes.
	// +kubebuilder:validation:Pattern=`^[0-9]+(G|GI|GB|GiB|M|MI|MB|MiB|g|gi|gb|gib|m|mi|mb|mib)?$`
	// +optional
	SpdkSystemMemory string `json:"spdkSystemMemory,omitempty"`

	// JournalManager tunes the journal managers on these nodes.
	// +optional
	JournalManager *JournalManagerSpec `json:"journalManager,omitempty"`
}

// NodeSet is a group of groups that share a sizing. The sizing is what a node
// set is for, and it is what a rolling hardware upgrade re-scopes.
type NodeSet struct {
	// Name is the node set's name. It is copied to StorageNode.spec.nodeSet, so
	// that a node can be traced back to the document that produced it.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Sizing is stamped onto every StorageNode this set produces.
	// +kubebuilder:validation:Required
	Sizing StorageNodeSizing `json:"sizing"`

	// Groups are the sets of workers sharing one configuration.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:Required
	Groups []NodeGroup `json:"groups"`
}

// ClusterTemplate is the StorageCluster the expansion creates, where it creates
// one. It carries the layout fields a cluster cannot change later, so that a
// reviewer sees them before the cluster exists rather than after.
type ClusterTemplate struct {
	// Name is the StorageCluster's name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Stripe is the erasure-coding layout.
	// +optional
	Stripe *StripeSpec `json:"stripe,omitempty"`

	// FabricType is the storage fabric.
	// +optional
	FabricType string `json:"fabricType,omitempty"`

	// EnableFailureDomains opts the cluster into failure-domain mode, in which
	// every node must declare a fault group.
	// +optional
	EnableFailureDomains *bool `json:"enableFailureDomains,omitempty"`
}

// ClusterDeploymentConfigSpec is a whole simplyblock deployment as one
// reviewable document.
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self == oldSelf",message="an approved deployment config is immutable"
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.approved",message="approval cannot be withdrawn"
type ClusterDeploymentConfigSpec struct {
	// Approved is the review gate. A document is expanded only once it is set,
	// and is validated but otherwise inert before that, which is what makes
	// reviewing a wrong document safe.
	// +optional
	Approved bool `json:"approved,omitempty"`

	// Environment is the Kubernetes distribution this deployment targets. It is a
	// shorthand the expansion spends: it sets enableKubeletConfiguration,
	// enableCpuTopology, ubuntuHost, and openShiftCluster on every StorageNode
	// the document produces, after which nothing reads it again.
	// +optional
	Environment KubernetesEnvironment `json:"environment,omitempty"`

	// EdgeCluster states that this is an edge deployment. An edge deployment
	// differs from a datacenter one in topology and scale rather than in kind,
	// so it is the same StorageCluster with fewer, smaller nodes.
	// +optional
	EdgeCluster *bool `json:"edgeCluster,omitempty"`

	// ClusterRef names an existing StorageCluster this document adds nodes to.
	// Absent means the document creates the cluster in Cluster. Setting it to a
	// cluster that does not exist, or leaving it absent when one already does,
	// is refused rather than reconciled (§6).
	// +optional
	ClusterRef string `json:"clusterRef,omitempty"`

	// Cluster is the StorageCluster to create. Ignored when ClusterRef is set.
	// +optional
	Cluster *ClusterTemplate `json:"cluster,omitempty"`

	// NodeSets are the nodes the deployment is made of.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:Required
	NodeSets []NodeSet `json:"nodeSets"`
}

// ClusterDeploymentConfigStatus is the observed state of the document.
type ClusterDeploymentConfigStatus struct {
	// Phase is the operator's own view of the document.
	// +optional
	Phase ClusterDeploymentConfigPhase `json:"phase,omitempty"`

	// Step is the position of the expansion machine within Expanding.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','CreatingCluster','AwaitingCluster','CreatingNodes']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// ClusterRef names the StorageCluster the expansion produced or added to. It
	// is a record rather than a dependency: nothing resolves it after expansion,
	// which is what makes the document safe to delete.
	// +optional
	ClusterRef string `json:"clusterRef,omitempty"`

	// NodeRefs names the StorageNode objects the expansion created, for the same
	// reason.
	// +optional
	// +listType=set
	NodeRefs []string `json:"nodeRefs,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the document moves, and never a log. On a Draft it is what validation
	// found, which is what a reviewer reads before approving.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cdc
// +kubebuilder:printcolumn:name="Approved",type=boolean,JSONPath=".spec.approved"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".status.clusterRef"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ClusterDeploymentConfig is a whole simplyblock deployment written down as one
// reviewable document: the environment, the cluster, and the node sets with
// their workers, interfaces, and devices. An administrator reviews it, approves
// it, and the operator expands it into a StorageCluster and its StorageNodes.
//
// It is ephemeral. Everything the expansion produces is self-describing, so the
// document can be edited or deleted once it has been expanded, and nothing reads
// it afterward.
type ClusterDeploymentConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterDeploymentConfigSpec   `json:"spec,omitempty"`
	Status ClusterDeploymentConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterDeploymentConfigList contains a list of ClusterDeploymentConfig.
type ClusterDeploymentConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterDeploymentConfig `json:"items"`
}
```

---

## Appendix B: `operatorops_types.go`

```go
// OperatorOpsAction is the operation an OperatorOps performs.
// +kubebuilder:validation:Enum=Discover
type OperatorOpsAction string

const (
	OperatorOpsActionDiscover OperatorOpsAction = "Discover"
)

// OperatorOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type OperatorOpsPhase string

const (
	OperatorOpsPhasePending   OperatorOpsPhase = "Pending"
	OperatorOpsPhaseRunning   OperatorOpsPhase = "Running"
	OperatorOpsPhaseSucceeded OperatorOpsPhase = "Succeeded"
	OperatorOpsPhaseFailed    OperatorOpsPhase = "Failed"
	OperatorOpsPhaseAborted   OperatorOpsPhase = "Aborted"
)

// OperatorOpsStep is one step of a running operator operation. Which steps
// belong to which action is declared by that action's graph rather than by this
// type, which is why the enum stays flat as actions are added.
// +kubebuilder:validation:Enum=Inspecting;Probing;Writing
type OperatorOpsStep string

const (
	// Discover.
	OperatorOpsStepInspecting OperatorOpsStep = "Inspecting"
	OperatorOpsStepProbing    OperatorOpsStep = "Probing"
	OperatorOpsStepWriting    OperatorOpsStep = "Writing"
)

// DeviceFilter narrows what a discovery run reports as a candidate device. It is
// an input to discovery and never appears in the document discovery writes: a
// ClusterDeploymentConfig carries the explicit list the filter produced, not the
// rule that produced it.
type DeviceFilter struct {
	// EnableLogicalBlockDevices reports a worker's available logical block
	// devices alongside its available NVMe devices. Unset reports NVMe only, so
	// that upgrading to 26.4 does not change what a discovery run reports and a
	// deployment that only ever meant to use NVMe has no longer list to review.
	// +optional
	EnableLogicalBlockDevices *bool `json:"enableLogicalBlockDevices,omitempty"`

	// EnablePartitionedDevices reports devices carrying a partition table
	// alongside the available ones, for the administrator who knows the table is
	// stale and intends to hand the device over anyway. It is the only one of the
	// three availability conditions that can be waived: a mounted or otherwise
	// busy device is never reported, because simplyblock taking it would corrupt
	// whatever is using it.
	// +optional
	EnablePartitionedDevices *bool `json:"enablePartitionedDevices,omitempty"`

	// PcieAllowList restricts candidates to these PCI addresses. This and the two
	// PCI filters below narrow the NVMe class alone, because a logical block
	// device has no PCI address to match.
	// +listType=set
	// +optional
	PcieAllowList []string `json:"pcieAllowList,omitempty"`

	// PcieDenyList excludes these PCI addresses. On a fleet that is uniform
	// about which slot holds the boot device, this is what keeps that device out
	// of every group of every draft.
	// +listType=set
	// +optional
	PcieDenyList []string `json:"pcieDenyList,omitempty"`

	// PcieModel restricts candidates to devices whose PCI model string matches.
	// +optional
	PcieModel string `json:"pcieModel,omitempty"`

	// DriveSizeRange restricts candidates by size ("100G-2T"). Unlike the PCI
	// filters, it narrows both classes.
	// +optional
	DriveSizeRange string `json:"driveSizeRange,omitempty"`
}

// DiscoverSpec parameterizes the Discover action.
type DiscoverSpec struct {
	// ConfigName is the ClusterDeploymentConfig to write. Absent generates one
	// from the run's timestamp, so that a second discovery never overwrites the
	// first, which may have been reviewed and corrected.
	// +optional
	ConfigName string `json:"configName,omitempty"`

	// NodeSelector restricts which workers are inspected. Empty inspects every
	// schedulable worker.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// DeviceFilter narrows which of an inspected worker's devices reach the
	// draft. Empty reports every device the worker advertises, including the one
	// it boots from, which is what the approval gate then has to catch.
	// +optional
	DeviceFilter *DeviceFilter `json:"deviceFilter,omitempty"`
}

// OperatorOpsSpec is one operation to perform against the operator itself.
//
// It carries no target reference. Every other Ops kind in this group names the
// entity it acts on; this one acts on the operator process, which is not a
// resource in this API group, and the absent field is the signal.
type OperatorOpsSpec struct {
	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action OperatorOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind.
	// Whether an abort is expressible from the current step is declared by that
	// action's graph rather than checked here. Discovery changes nothing, so an
	// aborted run leaves behind at most the config it had already written.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Discover parameterizes action Discover.
	// +optional
	Discover *DiscoverSpec `json:"discover,omitempty"`
}

// OperatorOpsStatus is the observed state of one operator operation.
type OperatorOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase OperatorOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Inspecting','Probing','Writing']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// ConfigRef names the ClusterDeploymentConfig a Discover run wrote.
	// +optional
	ConfigRef string `json:"configRef,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StartedAt is when the operation started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when it reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=oops
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Config",type=string,JSONPath=".status.configRef"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// OperatorOps is a single operation performed against the operator itself,
// which today means a discovery run that writes a ClusterDeploymentConfig. It
// runs to a terminal phase and stays afterward as the audit record.
type OperatorOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OperatorOpsSpec   `json:"spec,omitempty"`
	Status OperatorOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OperatorOpsList contains a list of OperatorOps.
type OperatorOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OperatorOps `json:"items"`
}
```
