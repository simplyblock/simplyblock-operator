# Design Document: The StorageNode and Its Operations

**Status:** Implemented  
**Authors:** Christoph Engelbert (noctarius), Israel Geoffrey (`StorageNodeOps`)  
**Date:** 2026-08-28 (last updated 2026-10-05)  
**Supersedes:** `design-storagenodeset-storagenode.md` and `design-node-removal-draining.md`, both removed in the same change  
**Test Plan:** [`tests/test-plan-storagenode.md`](../../tests/test-plan-storagenode.md)

This document specifies the model both kinds now carry. Both moved to
`storage.simplyblock.io/v1alpha2` with a `v1alpha1` spoke and a conversion between
them, and the controllers were rewritten against this specification in
`operator/internal/controllers/node/`. §15 remains the record of what changed
against the registered API.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [StorageNode: API](#3-storagenode-api)
4. [StorageNode: Controller](#4-storagenode-controller)
5. [The Storage-Node Workload](#5-the-storage-node-workload)
6. [StorageNodeOps: API](#6-storagenodeops-api)
7. [StorageNodeOps: Controller](#7-storagenodeops-controller)
8. [Remove: Draining a Node Before It Leaves](#8-remove-draining-a-node-before-it-leaves)
9. [Migrate: Relocating a Node onto Another Worker](#9-migrate-relocating-a-node-onto-another-worker)
10. [Host Maintenance: Surviving a Kubernetes Node Drain](#10-host-maintenance-surviving-a-kubernetes-node-drain)
11. [Mutual Exclusion](#11-mutual-exclusion)
12. [Backend API Requirements](#12-backend-api-requirements)
13. [Observability](#13-observability)
14. [Testing Strategy](#14-testing-strategy)
15. [Migration from the Registered API](#15-migration-from-the-registered-api)
16. [Open Questions](#16-open-questions)

Appendices:

- [Appendix A: `storagenode_types.go`](#appendix-a-storagenode_typesgo)
- [Appendix B: `storagenodeops_types.go`](#appendix-b-storagenodeops_typesgo)
- [Appendix C: the `StorageCluster` addition](#appendix-c-the-storagecluster-addition)

---

## Overview

`StorageNode` is one simplyblock backend storage node expressed as a Kubernetes
resource, and `StorageNodeOps` is a single operation performed against one of
them. They are specified together because they are one decision split across two
kinds, which is the convention stated in
[`design-crd-model.md`](design-crd-model.md) §3.

A node is where the abstract layout a `StorageCluster` declares becomes a process
on a physical machine holding physical devices, and that is what makes this level
the one with the most ways to go wrong. Everything below the cluster has a blast
radius measured in data: a node that is shut down while a peer is already offline
can exceed the cluster's fault tolerance, a node removed before its volumes have
moved takes them with it, and a node relocated onto a host whose storage-node API
is not yet resolvable comes back offline with its devices stuck. Each of those is
a step in this document, and each is a step because it is a place a process can
die between a decision and its consequence.

The document also carries the workload (§5). Retiring `StorageNodeSet`
([`design-crd-model.md`](design-crd-model.md) §9.2) leaves the DaemonSet, the
Services, the certificates, and the per-node configuration without an owner, and
they belong beside the node they exist to run.

---

## 1. Background

A `StorageNode` is one backend storage node, meaning one SPDK process bound to
one NUMA socket of one Kubernetes worker. A worker with two sockets may host two of
them, and the cluster's erasure coding is spread across all of them. The kind
exists so that an operation can name one node rather than a fleet, which is the
difference between restarting one SPDK process and restarting every node in the
cluster.

Three things about the registered shape are the reason this document is a rework
rather than a re-sync.

**The parent is the wrong resource.** `StorageNode.spec.storageNodeSetRef` is
required, and `StorageNodeSet` is being retired. The set was three concerns in
one object: a fleet template, a per-node configuration map, and the owner of the
Kubernetes workload that runs the nodes. The first two move into
`ClusterDeploymentConfig` ([`design-crd-model.md`](design-crd-model.md) §6), the
third moves to `StorageCluster`, and until the third has a specification the
retirement cannot proceed, because deleting a `StorageNodeSet` today is what
tears the workload down.

**Four operations improvise their own state machine.** `StorageNodeOps` serves
six actions through a hand-rolled `switch` over `status.subPhase`, whose enum is
the union of two disjoint workflows, so nothing but convention stops a `Remove`
from reporting `Promoting`. A seventh operation, the Kubernetes node drain
coordinator, is not a `StorageNodeOps` at all: it is a separate controller
driving an eight-phase workflow through `StorageNodeSet.status.drainCoordination`,
a field the retirement deletes.

**The node's own reconcile is multi-step and does not say so.** Provisioning a
node waits for the worker's storage-node API to answer, waits for a slot under
the parallel-add limit, posts once, and then polls the control plane until a UUID
appears. That is four steps carried by two nullable status fields and a `List`
over sibling objects, and a process that dies mid-sequence has to infer where it
was.

---

## 2. Goals and Non-Goals

### Goals

- Specify `StorageNode`'s spec and status against a `StorageCluster` parent, and
  state which fields are genuinely per-node and which were only ever per-node by
  accident (§3).
- Specify the entity controller's four paths, meaning provisioning, adoption,
  steady-state synchronization, and deletion, including the step machine that
  makes the node-add call single-shot (§4).
- Specify the Kubernetes workload a storage node runs as, its ownership after the
  `StorageNodeSet` retirement, and the labels and per-node configuration it
  reads (§5).
- Specify `StorageNodeOps`, its seven actions, and one declared state graph per
  action (§6, §7).
- Specify the three multi-step actions in full: the drain that precedes a
  removal (§8), the relocation of a node onto another worker (§9), and the
  coordination of a Kubernetes node drain (§10).
- Specify the lock that keeps node operations serialized, and its relationship to
  the cluster lock one level up (§11).
- Record where the registered kinds do not meet the conventions of
  [`design-crd-model.md`](design-crd-model.md), as findings rather than as
  intentions (§15).

### Non-Goals

- **Not the cluster.** `StorageCluster` and `StorageClusterOps` are specified in
  [`design-storagecluster.md`](design-storagecluster.md). §5 adds one spec group
  to that kind, which is recorded in §15 as an addition that document has to
  absorb.
- **Not the device.** `StorageDevice` and `StorageDeviceOps` are
  [`design-storagedevice.md`](design-storagedevice.md). This document keeps the
  `status.resources.devices` summary beside them (§3.3), which that document
  argues for rather than replaces.
- **Not the volume migration.** The kind the drain fans out one per subsystem is
  [`design-persistentvolumeops.md`](design-persistentvolumeops.md), and the
  migration algorithm it runs is
  [`design-auto-rebalancing.md`](../design-auto-rebalancing.md). This document
  specifies what creates those objects and what it does with their outcomes, not
  what they do.
- **Not the deployment config.** What `ClusterDeploymentConfig.nodeSets[]` looks
  like belongs to the bootstrap layer's own design. This document specifies what
  a `StorageNode` carries once that document has been expanded into one, which is
  the input that shapes it.
- **Not the API group's conventions.** The entity and action split, the naming
  rule, the ownership spine, the annotation key prefix, and the push model belong
  to [`design-crd-model.md`](design-crd-model.md) and are cited rather than
  restated.

---

## 3. StorageNode: API

Declared in `operator/api/v1alpha2/storagenode_types.go`, short name `sn`.
**The type is Appendix A**, whole and as it is to be written. What follows quotes
the field an argument turns on and no more, so that one copy of each type exists
and it is the one an implementation is written against.

### 3.1 Spec

The spec divides into three groups: what the node is, where it runs, and how it
is configured.

**Identity, immutable from creation.** A `StorageNode` is one backend node, and
which one it is cannot change.

```go
// ClusterRef names the StorageCluster this node belongs to. The cluster also
// owns this object by controller reference, so deleting the cluster deletes its
// nodes.
// +kubebuilder:validation:Required
// +k8s:immutable
ClusterRef string `json:"clusterRef"`

// Slot is which storage-node slot on this worker the object occupies, counted
// from zero. It is the identity the operator keys on, and it outlives the node
// filling it: only the UUID behind a slot changes when a node is replaced or
// relocated.
// +kubebuilder:validation:Minimum=0
// +optional
// +k8s:immutable
Slot *int32 `json:"slot,omitempty"`
```

**`spec.slot` is the identity, and `socketId` and `nodeIndex` are how a person
reads it.** A worker runs `len(socketsToUse) * nodesPerSocket` storage nodes, and
the slot is the position among them. The other two decompose it into the socket a
node is bound to and its position among the nodes sharing that socket, which is
what a `kubectl get sn` column is worth showing, and nothing but those columns
reads either. The three are not peers: the slot is what the topology label key,
the CR-to-slot matching, and the adoption lookup all use (§4.3, §5.2), so it is
frozen to everybody while the other two are the operator's to rewrite. A
relocation moves a node onto a host whose free socket is not necessarily the
source's, and a pair that still described the old host would be a column telling
a person something that is not so.

**The slot is named for where a user already meets it.** The topology label the
CSI driver reads is
`storage.simplyblock.io/storage-node-uuid.<clusterUUID>.<slot>` (§5.2), so the
field and the label say the same word. Naming it for the arithmetic that produces
it would describe something that stops being true once `nodesPerSocket` exceeds
one and two slots share a socket, and naming it for the RPC-port ordering would
describe how a slot is matched to a backend node rather than what it identifies.

**`spec.clusterRef` replaces `spec.storageNodeSetRef`, and the object name is
derived rather than generated.** A `StorageNode` is named from its cluster, its
worker, and its slot through the shared formula
([`design-api-upgrade.md`](design-api-upgrade.md) §19.6), so the three facts that
identify a node at creation are the three the name is built from and a name is
arrived at the same way twice by anything that has to predict one.

**The formula is bounded at a label's 63 bytes and not at the 253 an object name
may be**, because the name travels: the `StorageDevice` mirror writes it into
`storage.simplyblock.io/node` on every device the node carries
([`design-api-upgrade.md`](design-api-upgrade.md) §19.1). The arithmetic is not
academic. A regional cluster name and a worker a cloud named after its fully
qualified domain name are most of the budget between them, so a name that
overflows is what ordinary inputs produce rather than what a contrived one does.
Past that point the formula keeps what fits and spends the rest on a digest taken
over all three parts, so the worker stays in the name's text while it fits and in
its digest afterward, and two nodes differing only by worker never collide either
way.

**The name is stable across a migration because Kubernetes never renames an
object**, not because the worker is absent from it. A migration re-points
`spec.workerNode` on the node that exists (§9), so what changes is the field and
not the object's identity. What that costs is stated rather than avoided: a node
built on one worker and migrated to another keeps a name describing where it was
built. `spec.workerNode` is where the current host is read, which is the field the
webhook restricts to one writer (§3.2), and the name is an identifier rather than
a report.

**Re-expansion is idempotent without consulting the name.** The expansion lists
the cluster's existing nodes and matches them on the worker and slot their specs
record ([`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md)
§4.2), so a crash part-way through creates the rest and duplicates nothing even
where a name was derived under a wider limit than the one in force now.

**Placement, mutable by the operator alone.**

```go
// WorkerNode is the Kubernetes worker hostname this node runs on. It is not
// marked immutable, because a migration re-points it (§9), but the StorageNode
// validating webhook rejects any change made by an identity outside the
// operator's namespace.
// +kubebuilder:validation:Required
WorkerNode string `json:"workerNode"`
```

`workerNode` is one of three fields with exactly one legitimate writer, and the
webhook is what expresses that (§3.2). `+k8s:immutable` would block the operator
along with everyone else, and leaving it unguarded would let a user move a node by
editing a string, which relocates nothing and leaves the CR describing a host the
backend node is not on.

**Configuration, and the node is self-describing.**

```go
// Config is this node's complete effective configuration, copied from the node
// set entry in ClusterDeploymentConfig that produced it. It is a copy rather
// than a projection: the deployment config is ephemeral, so nothing reads it
// after the node exists.
// +kubebuilder:validation:Required
Config StorageNodeConfig `json:"config"`
```

**A `StorageNode` describes itself, because the document that produced it does
not survive.** `ClusterDeploymentConfig` is a deployment description an
administrator reviews and applies
([`design-crd-model.md`](design-crd-model.md) §6), and it can be edited or
deleted the moment the expansion is done. So the whole of a node's configuration
is copied into the node at creation, and three properties follow that a
projection would not have:

- **Deleting the deployment config changes nothing.** No reconcile reads it, and
  a node whose originating document is gone is not degraded in any way.
- **Editing it does not reach existing nodes.** A changed device filter or fault
  group applies to nodes created afterward. A node already carrying data was
  placed against the values it was created with, and retroactively rewriting them
  would describe a layout that is not on the disks.
- **There is no per-reconcile sync to get wrong.** The registered model rewrites
  `StorageNode.spec.overrides` from `StorageNodeSet.spec.nodeConfigs` on every
  pass, which makes the set the single source of truth and the node a cache of
  it. Copying once inverts that: the node is the source of truth about itself.

`Config` is `Required` rather than a pointer for the same reason. A node with no
configuration is not a node in an earlier state of being written, it is a node
that cannot be provisioned, and admission is the right place to say so.

**Most of it is immutable, and the exceptions are named rather than assumed**
(§3.2). What a node's devices are, how its journal is laid out, and which fault
group it belongs to were all decided when its data was placed, and a spec field a
user can edit is a layout claim a user can invalidate.

**Four fields leave `StorageNodeConfig` because they were never per-node.**
`ubuntuHost`, `skipKubeletConfiguration`, `enableCpuTopology`, and
`reservedSystemCPU` are declared as per-node overrides today and reach nothing:
they are merged in `effectiveNodeConfig` and read by no caller, because their only
consumers are environment variables in a DaemonSet pod template, and a DaemonSet
is one object for every node it schedules. A per-node value has nowhere to go. All
four move to `StorageCluster.spec.storageNodes` (§5.1), which is where a
cluster-uniform value belongs and where setting one has an effect.

**`reservedSystemCPU` comes back, because one of them is not
cluster-uniform.** It names core ids rather than a quantity, so `0,1` on a
sixteen-core worker and `0,1` on a ninety-six-core worker hold back different
fractions of the machine, and a fleet whose groups differ in core count has no
one list that is right for all of them. What changed is that a per-node value
now has somewhere to go: the per-node ConfigMap carries one shell-sourceable
entry per worker, the pod sources its own, and the agent exports that one
variable out of it, so the node's list wins over the pod's environment while the
cluster's value stays the fleet's default. The other three are uniform and stay
where they went.

#### A device is named by a PCI address or by a device path

`config.deviceNames` is the explicit list of devices a node owns, and an entry is
either a PCI address (`0000:5e:00.0`) or a device path (`/dev/sdb`). Those are
the two classes simplyblock accepts as backend storage, and one list carries both
because a node's devices are one set however each of them was reached. A bare
device name is read as a path under `/dev`, which is what the registered field
accepted and what keeps every manifest that sets it valid (§15.1).

**Which of the two a node names is the cluster's decision, and mixing them is
rejected.** A cluster is built out of one class of backend storage, named by
`StorageCluster.spec.deviceClass`
([`design-storagecluster.md`](design-storagecluster.md) §3.1), because an
erasure-coding stripe placed across both classes is written and rebuilt at the
slower one's rate. So every entry of a node's list is of its cluster's class, a
list holding both is rejected at admission, and so is a list of the class the
cluster is not (§3.4). One field still carries both spellings, because a node's
devices are one set however each of them was reached and the cluster is what says
which spelling that set is written in.

**A generated deployment writes the persistent path, not the kernel one.** Both
are device paths and the field takes either, but they answer different questions.
`/dev/sdb` names whichever disk the kernel found second this boot: on the QEMU
workers this was developed against, the disk the kernel calls `sdb` is the one the
hypervisor calls `drive-scsi0` and `sda` is `drive-scsi2`, so a host that comes
back with its controllers probed in another order hands each kernel name to
another disk. `/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi0` and
`/dev/disk/by-partuuid/28427de0-…` are built from what the device itself reports,
and follow it. Both names exist and both resolve, so a list written in the first
spelling selects a different disk after such a reboot and nothing says so — which
is why discovery writes the second
([`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §8.1),
and why the node configuration carries the name whole rather than shortening it
to the kernel name the backend used to be given. A hand-written list may still
use either spelling, and the node resolves whichever it is handed.

**The PCI filters belong to an NVMe cluster and to no other.**
`config.pcieAllowList`, `config.pcieDenyList`, and `config.pcieModel` match on
something a logical block device does not have, so on a `LogicalBlock` cluster
they are rejected rather than ignored (§3.4). What selects devices there is the
explicit list, which is what a deployment config writes in either case: the
include and exclude lists that narrowed the candidates are a discovery input and
never reach the node
([`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §8.1).

**`config.pcieAllowList` is a filter and stays separate.** Both fields now take a
PCI address, and they are not two spellings of one thing: `deviceNames` says use
exactly these and overrides every filter, while `pcieAllowList` narrows what the
filters then consider. The distinction matters because the allow list is the one
device field the operator writes, merging `spec.migrate.newSsdPcie` into it during
a migration (§3.2, §9), and a field the operator edits cannot also be the field
holding a user's explicit list.

#### Sizing is per node, and one number of the three stays the cluster's

`vcpuCount` and `minHugePagesSize` size a node's SPDK core layout and its
huge-page floor, and `maxSubsystemCount` bounds how many NVMe-oF subsystems a
node serves. All three lived on the `StorageCluster` because the control plane
assumes them uniform across the fleet
([`design-storagecluster.md`](design-storagecluster.md) §3.1), and a node that
disagrees with its peers gets a layout the cluster cannot place erasure-coding
chunks across evenly.

**That assumption holds in steady state and is deliberately broken during a
hardware upgrade.** Replacing a fleet with larger machines is done one node at a
time, and a node moved to a host with more cores is re-sized when it moves. For
the duration of the roll the cluster's nodes genuinely differ, and a model that
cannot express that forces the whole fleet to be re-sized at once or not at all.

**Each of the three is stated once, on the cluster, and nothing below it declares
one.** A deployment config puts all three in its cluster block and gives a node set
no sizing of its own
([`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §3.1),
so what a node holds is a stamp of what it was built with rather than a value
somebody chose for it. That is what makes the field writable by the operator and
by nobody else: a fleet whose nodes differ is a fleet mid-roll, never a fleet
somebody described that way.

So the node carries its own copy of the two that describe the host it runs on:

```go
// StorageNodeSizing is what this node's SPDK core layout and huge-page floor
// were sized from. It is stamped from StorageCluster.spec at creation and is
// equal across the fleet in steady state; a rolling hardware upgrade is what
// makes two nodes differ, and only for as long as the roll takes.
type StorageNodeSizing struct {
	// +kubebuilder:validation:Minimum=4
	// +kubebuilder:validation:Required
	VCPUCount *int32 `json:"vcpuCount"`
	// ...
}
```

**`maxSubsystemCount` is not among them, because nothing about a host decides
it.** A core count and a huge-page floor follow the machine, which is why a
larger host is re-sized onto and why the roll produces a fleet that differs for a
while. The subsystem cap follows the cluster: it bounds how many volumes any node
can serve and is therefore an input to where volumes can be placed at all, so
every node in a cluster has the same one or the cluster's placement has stopped
treating its nodes as interchangeable. A per-node copy could only ever repeat the
cluster's value, and the one thing it could do beyond that is disagree with it.

So the node does not carry it. `StorageCluster.spec.maxSubsystemCount` is read
when the node's configuration is generated (§5.3), which is also what makes a
change to it reach the nodes that already exist rather than only the next one
created ([`design-storagecluster.md`](design-storagecluster.md) §3.2).

**The sizing block is immutable to users and writable by the operator**, which is
the same enforcement `workerNode` carries and for the same reason: unmanaged
divergence is exactly what breaks chunk placement, and managed divergence is a
supported operation. A user editing one node's `vcpuCount` is rejected. What
drives the operator's own re-size is §16, Q8.

Changing the cluster's value no longer reaches existing nodes, which is a
behavioral change from
[`design-storagecluster.md`](design-storagecluster.md) §3.2 and is recorded
there.

### 3.2 Immutability

A `StorageNode` is mostly not editable, which follows from §3.1: it is a record of
what a node was built as, and almost every field in it is a claim about a layout
that is already on disk or on the wire. Three enforcements carry that, and which
one a field takes is decided by whether it has no legitimate writer or exactly
one.

**Immutable to everyone, by marker.** These have no legitimate writer after
creation.

| Field                                                                                    | Optionality | Why                                                                                                     |
|------------------------------------------------------------------------------------------|-------------|---------------------------------------------------------------------------------------------------------|
| `clusterRef`                                                                             | `Required`  | Which cluster a node belongs to is its identity                                                         |
| `nodeSet`, `slot`                                                                        | `+optional` | The slot a node fills, and the document it was declared under (§3.1)                                    |
| `config.deviceNames`, `config.pcieDenyList`, `config.pcieModel`, `config.driveSizeRange` | `+optional` | They select which physical devices the node owns                                                        |
| `config.journalManager`                                                                  | `+optional` | Journal count and per-device share are on-disk layout, fixed when the devices were partitioned          |
| `config.failureDomain`                                                                   | `+optional` | Chunk placement was computed from it. Fillable later, since §4.2 holds provisioning until it is present |
| `config.expand`                                                                          | `+optional` | A creation-time instruction to the control plane, describing how the node joined rather than what it is |

A `Required` field is immutable from creation and an `+optional` one is immutable
once set, which is what makes `failureDomain` fillable and then frozen. The two
strengths and the rules controller-gen generates for each are stated in
[`design-crd-model.md`](design-crd-model.md) §7.6.

**A failure domain is a label, and `rack-b` is what one looks like.** The value
names the physical grouping a node shares with its peers, so it is the rack, the
zone, or the power feed somebody would say out loud, and a node set's name is
usually exactly that. An index carried the same grouping and none of the meaning:
two nodes in `1` are in the same domain and nothing in the API says what `1` is,
so the mapping from the number to the rack lived in whoever wrote the manifest.
The label is bounded by the shape a Kubernetes label value has, because the value
is seeded from one (`topology.kubernetes.io/zone`) where the cluster carries
topology at all
([`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §8.2)
and is printed in a column of its own (§15.1).

**Immutable to users, writable by the operator, by webhook.** Each of these has
exactly one legitimate writer, so a marker would lock the operator out along with
everyone else. `spec.config` is guarded as a block rather than member by member:
the rule is about what the block is, and a list of the fields it happens to have
is a thing to keep up to date. The two rules stack where both apply, so a member
carrying a marker is frozen to the operator as well.

| Field                          | Written by                                                                                                                                                                         |
|--------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `workerNode`                   | A migration re-pointing the node onto another host (§9)                                                                                                                            |
| `config.pcieAllowList`         | A migration merging `spec.migrate.newSsdPcie` into it (§9)                                                                                                                         |
| `config.sizing`                | A re-size during a rolling hardware upgrade (§3.1)                                                                                                                                 |
| every other member of `config` | Nothing today. The block is the record of what the node was built as, and it is guarded whole so that a member added later is the operator's without anybody remembering to say so |
| `socketId`, `nodeIndex`        | A relocation: where a node sits is a fact about the host it runs on (§9)                                                                                                           |

The `StorageNode` validating admission webhook admits a change to any of them
from a service account in the operator's namespace and rejects it from every other
identity. It fails closed, which is safe because it runs in the operator pod: its
availability tracks the operator's own, and an operator that is down is not
reconciling anything the rejection could deadlock.

**Mutable.** `config.spdkImage`, `config.spdkImagePullPolicy`,
`config.spdkProxyImage`, `config.spdkProxyImagePullPolicy`, and
`config.spdkSystemMemory`. The two images are
per-node so that an image rollout can be phased, which is the whole reason they
are not on the cluster, and the memory figure is what a node whose device count
grew legitimately needs to raise. All of them take effect on the node's next
configuration generation, which is a restart-shaped change rather than a
declarative one.

The two pull policies are the fields of the set the operator records and does not
yet spend. The control plane starts the SPDK pod, and its `spdk_process_start`
takes no pull policy: the pod template it renders writes `Always` for both
containers. They are on this kind because a deployment document states them, and
they reach the pod once the control plane accepts one. They are two fields rather
than one because a node may pin the proxy and follow the SPDK image.

### 3.3 Status

`status.uuid` is the backend node UUID and the field the controller branches on:
empty means the node has not been provisioned or adopted, and non-empty means
steady state (§4.1).

`status.phase` is the operator's own view of the node, and `status.step` is the
provisioning machine's position within it, both specified in §4.2.
`status.status` is the backend-reported lifecycle string, unchanged in its values
(`online`, `suspended`, `offline`, `in_creation`, `in_restart`, `in_shutdown`,
`unreachable`, `timeout`). The two are deliberately separate, in the same way the
cluster's are ([`design-storagecluster.md`](design-storagecluster.md) §3.3): one
says how far the operator has got, and the other says what the control plane
reports.

`status.health`, `status.hostname`, and `status.uptime` are backend observations.
`status.resources` groups the reported CPU count, memory, volume count, device
summary, and storage occupancy, and `status.ports` groups the management address
and the NVMe-oF, logical-volume, and RPC ports.

`status.resources.capacity` carries the two numbers a device's capacity carries
([`design-storagedevice.md`](design-storagedevice.md) §4.2), because a node's is
the sum of its devices' and a reader comparing them should not have to reconcile
two shapes, plus the time the reading was taken. It is a measurement, and it is in
status because a node passes the three tests
[`design-crd-model.md`](design-crd-model.md) §7.13 sets: there is one object per
node, the fullness of a node is what placement and capacity planning are about, and
the reading is written with hysteresis. The controller records a sample when it is
the first one, when the used size has moved by at least one percent of the node's
own total, or when the total itself changed, which is what a device joining or
leaving looks like. Everything else carries the previous value forward, so the
patch is empty and the reconciler does not retrigger itself for as long as the node
is serving I/O. Each volume's own occupancy is served from
`metrics.simplyblock.io` against the claim that names it, rather than listed here
([`design-crd-model.md`](design-crd-model.md) §7.13).

`status.resources.devices` is a block of two counts rather than a string, absent
until the control plane has reported. It is a summary rather than an inventory:
which devices a node has, how big each is, and what state each is in belong to
`StorageDevice` ([`design-storagedevice.md`](design-storagedevice.md)), and
nothing in this document depends on the summary's shape, so that kind lands
without changing anything specified here.

**Two counts beat the `"online/total"` string they replace**, for the reason a
count usually beats a rendering of one. A number is comparable, so
`status.resources.devices.online < .total` is a CEL rule, an alert, and a print
column, where the string form makes each of those a parse. It also cannot be
assembled in the wrong order, which the string form is: the operator renders it
`fmt.Sprintf("%d/%d", res.DevicesCount, res.OnlineDevicesCount)` at both call
sites, so a node with three of four devices online reports `4/3` against a
documented format of `online/total` (§15.1).

`status.activeOpsRef` is the operation lock (§11). `status.latencyMetrics` holds
the fio-measured NVMe-oF baseline the volume rebalancer reads, written by the
latency controller and specified in
[`design-auto-rebalancing.md`](../design-auto-rebalancing.md).
`status.failureDomain` is the failure-domain label the control plane actually
assigned, which is not necessarily the one `spec.config.failureDomain` requested.
The control plane indexes a failure domain by integer, so the add sends the index
the cluster's `status.failureDomains` maps the label to
([`design-storagecluster.md`](design-storagecluster.md) §3.3). A label the
mapping does not hold is sent as its number when it is one, and is otherwise left
for the control plane to assign.

`status.observedGeneration` is the generation the rest of `status` was computed
from, so a stale status can be told from a current one.

Three fields that exist today are gone. `status.postedAt` and the `Triggered`
bookkeeping it supported are replaced by the persisted step (§4.2, §7.2), and the
duplicated per-node entries the retired `StorageNodeSet.status.nodes[]` carried
have no successor, because the `StorageNode` objects were always the better copy
of them.

### 3.4 What Admission Validates

The `StorageNodeValidator` of §3.2 has a second responsibility beside the
operator-only fields: it resolves `spec.clusterRef` and rejects a node whose cluster
does not exist.

**The reference is immutable from creation (§3.2), which is what makes admission the
right place.** A node naming a cluster that is not there can never be corrected: the
field cannot be edited, so the only remedy is to delete the object and write it again,
which is exactly what the rejection asks for. Admitting it instead produces a node that
holds forever with a message nobody can act on except by deleting it, which is a slower
way to deliver the same answer.

**A cluster that exists and has no `status.uuid` yet is admitted, and provisioning
waits.** The distinction is between a mistake and an ordering: a manifest that declares
a cluster and its nodes together is the ordinary way to bring up a deployment, and the
cluster's UUID is what `Posting` needs (§4.2) rather than what the create needs. So the
node is admitted, holds before `Posting` with `ClusterNotReady` (§13.1), and proceeds
when the cluster has a UUID. Rejecting that at admission would make a single-apply
deployment fail on the order its files happened to be in.

**The check matters for hand-written nodes rather than for the operator's own.** A
`StorageNode` the cluster expanded from `spec.storageNodes` (§5.1) names the cluster
that created it and always resolves. What the webhook catches is a node somebody wrote
directly, with a cluster name misspelled or one in another namespace, which is the
case where the cluster's own expansion is not there to get it right.

**A resolvable `clusterRef` is what makes the rest of the node's spec worth checking,
because it is what the node will be deployed against.** A hand-written `StorageNode` is
a manually configured storage node: the operator will build a workload for it, write its
ConfigMap entry, and add it to the cluster the reference names. So the same webhook
compares the node's sizing against the cluster it is joining and rejects a mismatch.

| Field                                                      | Rejected when                                                             |
|------------------------------------------------------------|---------------------------------------------------------------------------|
| `config.sizing.vcpuCount`                                  | It differs from the cluster's stamp value                                 |
| `config.sizing.minHugePagesSize`                           | It differs from the cluster's stamp value, an unset value included        |
| `config.deviceNames`                                       | An entry is of a class other than the cluster's `spec.deviceClass`        |
| `config.pcieAllowList`, `config.pcieDenyList`, `pcieModel` | Any of them is set and the cluster's `spec.deviceClass` is `LogicalBlock` |

**The device rows are the cluster's layout reaching down one level.** An entry is
of the NVMe class when it is a PCI address and of the block class when it is a
path, and a bare name is a path (§3.1), so the class of every entry is decidable
from the string. A mixed list is therefore rejected on a cluster of either class,
and a uniform list of the wrong class is rejected too. The PCI filters go with it:
they match on an address a logical block device does not have, so on a
`LogicalBlock` cluster they select nothing and rejecting them says that, where
ignoring them would leave somebody reading a filter that never ran.

**The sizing rows are two and not others, because the control plane assumes them
uniform.** §3.1 states why: a node whose core layout and huge pages disagree with its
peers gets a layout the cluster cannot place erasure-coding chunks across evenly.
There is no row for `maxSubsystemCount`: the node has no copy of it to disagree
with, which is the point of leaving it on the cluster (§3.1).

**An unset value is a divergence, because nothing inherits at render time.** The
node's own `config.sizing` is what §5.3 writes into the per-node ConfigMap, and it
writes `MAX_HUGE_PAGES_SIZE` from that field without consulting the cluster, so a
node omitting it runs on the computed minimum rather than on the floor the cluster
states. `minHugePagesSize` is optional on the type, which is what makes this worth
saying: optional means a cluster need not state a floor at all, not that a node may
decline the one its cluster states. Both rows therefore read the same way, and the
webhook compares the node's value to the cluster's whenever the cluster has one.

**A cluster that states no floor checks nothing.** The comparison is skipped when
`StorageCluster.spec.minHugePagesSize` is empty, because there is no stamp value to
diverge from and every node is then on the computed minimum together, which is
uniform by construction.

**The reference is the cluster's stamp value, not the siblings'.** During a rolling
hardware upgrade the fleet is deliberately heterogeneous (§3.1), so sibling nodes
disagree with each other by design and comparing against them would reject a correctly
sized new node or admit an incorrectly sized one depending on which sibling was asked.
The cluster carries one value, it is the value a new node is stamped with, and it is
therefore the only well-defined thing to compare against.

**The operator's own writes are exempt, which is what keeps the rolling upgrade
expressible.** §3.2 already admits a change to `config.sizing` from a service account in
the operator's namespace and rejects it from every other identity, and the same identity
test applies here: a node the operator creates or re-sizes is admitted, and a divergence
a person writes is not. Managed divergence is a supported operation and unmanaged
divergence is the defect.

The failure policy is the one §3.2 states, for the same reason: the webhook runs in the
operator pod, so an operator that is down is not reconciling anything the rejection
could deadlock.

### 3.5 Examples

A node as the operator writes it when expanding a deployment config, before
anything has been provisioned:

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: StorageNode
metadata:
  name: production-7f3a9c
  namespace: simplyblock
  labels:
    storage.simplyblock.io/cluster: production
    storage.simplyblock.io/worker: worker-3
spec:
  clusterRef: production
  nodeSet: rack-a
  workerNode: worker-3
  socketId: "0"
  nodeIndex: 0
  slot: 0
  config:
    failureDomain: rack-a
    pcieAllowList:
      - "0000:5e:00.0"
      - "0000:5f:00.0"
```

The same node once it is running, on a two-socket worker where it is the second
of the two:

```yaml
spec:
  clusterRef: production
  nodeSet: rack-a
  workerNode: worker-3
  socketId: "1"
  nodeIndex: 0
  slot: 1
status:
  phase: Online
  uuid: 7b90d3e4-52a1-4f0c-8d76-3ca9017be255
  status: online
  health: true
  hostname: worker-3
  uptime: 4d2h11m
  failureDomain: rack-a
  resources:
    cpu: 8
    memory: 100G
    volumes: 12
    devices:
      online: 4
      total: 4
    capacity:
      totalBytes: 15363023929344
      usedBytes: 7681511964672
      sampledAt: "2026-09-08T09:14:02Z"
  ports:
    management: 10.0.3.14
    nvmeof: 4420
    lvol: 9090
    rpc: 8081
  observedGeneration: 2
```

A node that is provisioning, held because the parallel-add limit is reached:

```yaml
status:
  phase: Provisioning
  step:
    state: AwaitingSlot
    deadline: "2026-08-28T11:40:00Z"
  message: waiting for a node-add slot, 1 of 1 in flight
  observedGeneration: 1
```

`status.uuid` is absent, `status.status` is absent, and nothing has been sent to
the control plane. The deadline is what distinguishes a node waiting its turn from
one that will wait forever.

---

## 4. StorageNode: Controller

`StorageNodeReconciler`, in
`operator/internal/controllers/node/storagenode_controller.go`. It owns the node's own
lifecycle. Every imperative operation belongs to `StorageNodeOps` (§7), and the
workload the node runs in belongs to the cluster (§5).

### 4.1 Reconcile

```
┌──────────────────────────────────────────────────────────────┐
│                  Kubernetes Control Plane                    │
│   ┌──────────────────────────────────────────────────────┐   │
│   │                StorageNodeReconciler                 │   │
│   │  1. Get the CR, resolve its StorageCluster           │   │
│   │  2. Deletion: raise a remove ops, then the finalizer │   │
│   │  3. Ensure the finalizer                             │   │
│   │  4. status.uuid != ""  → syncStatus                  │   │
│   │  5. status.uuid == ""  → the provisioning machine    │   │
│   └──────────────────────────────────────────────────────┘   │
│  StorageNode CR   spec.*   status.uuid   status.phase        │
└──────────────────────────────────────────────────────────────┘
              │ HTTP (webapi client, service-account bearer token)
┌─────────────▼────────────────────────────────────────────────┐
│                  Simplyblock Control Plane                   │
│  POST   /api/v2/clusters/{cluster}/storage-nodes             │
│  GET    /api/v2/clusters/{cluster}/storage-nodes/?watch=true │
│  DELETE /api/v2/clusters/{cluster}/storage-nodes/{node}      │
└──────────────────────────────────────────────────────────────┘
```

### 4.2 Provisioning, and the step that makes the node-add single-shot

Adding a backend node is not idempotent, and the call adds every socket of the
worker at once rather than one node. Two `StorageNode` objects for the same
worker that both observe an empty `status.uuid` would therefore add the worker
twice. The claim is made in Kubernetes before the control plane is touched, as a
transition of the node's own machine:

```
  no status.uuid
    │
    ▼
  CheckingHost    ← the worker's storage-node API answers?
    │  reachable
    ▼
  Adoption?       ← an upgrade Secret, or an existing backend node at this IP?
    │  no                                  │ yes → Adopting (§4.3)
    ▼
  CheckingConfig  ← failure domain present when the cluster requires one
    │  valid
    ▼
  AwaitingSlot    ← under nodeProvisioningBudget, and no FDB worker in flight
    │  a slot is free
    ▼
  Posting         ← Status().Patch with MergeFromWithOptimisticLock, then
    │               POST /storage-nodes        │ worker away → AwaitingWorker
    ▼                                          │
  Resolving       ← match this slot against    │ worker away → AwaitingWorker
    │               the cluster's node list    │
    │  UUID found                              ▼
    ▼                                   AwaitingWorker  ← back? → CheckingHost
  phase: Online
```

**The mutex is the optimistic-lock patch on the transition into `Posting`, not
the value it writes.** `Status().Patch` with `MergeFromWithOptimisticLock`
succeeds for exactly one reconciler at a given `resourceVersion` and returns 409
to the rest. That is the same primitive the cluster's creation path uses
([`design-storagecluster.md`](design-storagecluster.md) §4.2), and it replaces
both `status.postedAt` and the `List` over sibling objects that stand in for it
today.

The sibling problem is what the `Posting` step also solves. One `POST` adds every
socket of the worker, so the second socket's object must not post again. It reads
its sibling's step rather than its `postedAt`: a sibling at `Posting` or beyond
means the worker has been claimed, and the second object enters `Resolving`
directly.

**A worker that goes away mid-add is waited for, not counted against.** The
storage pool's MachineConfig is applied by rebooting the machine, so the first
node of a fresh cluster is cordoned, drained, rebooted, and uncordoned in the
middle of being added. Every step of this path waits on something the worker
does, so none of them can progress meanwhile, and a step that kept its deadline
through the reboot would fail the node for the reboot. `AwaitingWorker` is that
wait written down: `Posting` and `Resolving` enter it when the worker stops being
`Ready` or is cordoned, it carries its own hour-long budget, and it leaves for
`CheckingHost` when the machine is back.

**It returns to the first step rather than to the step it left**, because what a
reboot interrupts is not resumable in the middle: a `Posting` resumed after the
fact would ask for a second node, and the add it was waiting on may well have
landed while the machine was away. `CheckingHost` is where a backend node that
already exists is found, and its `Adopting` edge takes that node over instead of
adding it twice.

**It holds the claim while it waits.** A node in `AwaitingWorker` still counts as
having claimed its worker, so `nodeProvisioningBudget` stays closed and no second
worker is handed a configuration change on top of a reboot already running — the
same reason the cap exists at all. That is also why only the two steps that have
claimed the worker divert into it: a claimed sibling on the same worker is read as
an add that already happened and a reason to skip `Posting`, and a node that never
posted must not be read that way. The steps before the claim wait where they are,
and `AwaitingSlot` declines to take a slot at all while its worker is away, which
is what stops an add being posted against a machine that cannot answer it.

**Adoption is checked at every gate before the add, the queue included.** A
backend node the worker already has is what `CheckingHost` and `CheckingConfig`
divert on, and an object passes each of them once. One that reached the queue
before its backend node existed — a previous operator's add, a `POST` whose
response was lost after the control plane committed, a rebuilt object — has no
add to ask for, and checking again where it waits is what recognizes that. It is
checked before the worker is, for the reason `CheckingHost` checks it first: an
adopted node is already running, and the machine being reachable is not this
operator's precondition to establish.

**`AwaitingSlot` is where two independent serialization rules live.**
`nodeProvisioningBudget` caps how many workers may be in flight at once, counted by
distinct worker rather than by object so that a two-socket host consumes one slot.
Workers hosting a FoundationDB pod are always sequential regardless of that cap,
because a node add reboots the host and two simultaneous FDB reboots reduce the
control plane's own fault tolerance. The step re-evaluates both on every pass and
holds rather than failing.

**A slot is taken, and the holders are the Provisioning phase's metadata on the
cluster.** `StorageCluster.status.provisioningSlots` is the list of workers whose
add is outstanding, each entry naming the worker, the `StorageNode` that took it,
and when. Taking one is an optimistic-locked patch of that single list, which is
what makes the cap hold: exactly one node wins a given `resourceVersion` and every
other is refused and counts again. A cap derived instead from the siblings' steps
holds only while every node reads the same set, and reconciles are served from an
informer cache — a cache filled moments ago, after a restart or a lease change,
gives each node a different set and every one of them is alone in its own.

The list is on the cluster rather than a field per node for the same reason. Six
node objects carry six `resourceVersion`s, so a claim written to each is six
separate agreements and no mutual exclusion between them.

A holder releases its own entry and no other, on each of the three ends of an add:
the UUID arriving, the step outliving its deadline, and the object being deleted.
A slot whose holder is gone, has a UUID already, or has failed is reaped by the
next node to ask for one, so a cap cannot be left closed by an object that will
never reconcile again.

The ordering of the waiting workers stays, as a tie-break rather than as the cap.
It settles who tries first among nodes that can see each other, so a node told to
wait is not overtaken by one told to wait beside it, and it is computed net of the
workers already holding a slot.

**`CheckingConfig` is a gate rather than a validation.** A cluster with
`enableFailureDomains` set requires every node to declare a fault group, and a
node that does not is held with a `FailureDomainMissing` event rather than
rejected. The value can arrive later, and holding is what makes filling it in
sufficient.

#### The phase and the step

```go
// StorageNodePhase is where the operator has got to with this node. The first two
// values are the operator's own provisioning path; the rest are its reading of the
// lifecycle status.status carries in the control plane's own spelling.
// +kubebuilder:validation:Enum=Pending;Provisioning;Online;Removing;Removed;Offline;Degraded;Failed
type StorageNodePhase string

// StorageNodeStep is one step of the provisioning path. There is one graph
// rather than a MultiConfig, because an entity has no spec.action to key one on.
// +kubebuilder:validation:Enum=CheckingHost;CheckingConfig;AwaitingSlot;Posting;Resolving;Adopting;AwaitingWorker
type StorageNodeStep string
```

The lifecycle half of the phase reads the control plane's status. `online` and
`active` are `Online`, or `Degraded` with devices missing. `suspended`,
`offline`, and `in_shutdown` are `Offline`, a shutdown in progress being read as
where it is going. `in_creation` and `in_restart` are `Provisioning`. The four
statuses of a removal in progress (`pending_removal`, `migrating_devices`,
`migrating_lvols`, `in_removal`) are `Removing`, and `removed` is the terminal
`Removed`. Anything else, `unreachable` and `timeout` among it, is `Failed`.

`Adopting` is reached from two states rather than one, matching the cluster's
creation machine: an upgrade Secret diverts before the config check, and a
backend node found at the worker's address diverts at the same point (§4.3).

Each step carries a deadline, which is what turns a node whose host never answers
into a step that expires rather than a reconcile that repeats forever. `Resolving`
is the step whose deadline matters most, because a node add that the control plane
accepted and then failed to complete leaves the object polling for a UUID that
never arrives, and that is the failure mode `status.status: timeout` exists for
today with no bound behind it.

### 4.3 Adoption

Adoption is how a backend node the operator did not add becomes a managed one, and
it is the path every pre-existing cluster takes.

A backend node is matched to a `StorageNode` by the worker's internal IP and its
`spec.slot`. The control plane's nodes for one worker are sorted by RPC
port ascending, and position in that list is the socket ordinal, so socket 0 takes
the lowest port. This is a positional match rather than an identity one, and it
holds because the ports are assigned in socket order at node-add time.

Two triggers reach the step. A Secret named `simplyblock-{cluster}-upgrade`
declares that this deployment is being adopted wholesale, which is the migration
route off a Helm deployment. Without one, the step is entered only when a lookup
finds a backend node already at the worker's address, which covers a `POST` whose
response was lost after the control plane committed.

Per-node adoption is the narrow path. The wide one is discovery, which inspects a
whole deployment and writes it out as a `ClusterDeploymentConfig` an administrator
reviews and applies ([`design-crd-model.md`](design-crd-model.md) §6), and the
node-level path exists for what discovery misses rather than as the primary
route.

### 4.4 Steady-state synchronization

With a UUID present, the controller reads the node from the control-plane store
rather than from the HTTP API. An `updated` event on the storage-node stream
enqueues a reconcile, which writes `status` from the streamed object
([`design-crd-model.md`](design-crd-model.md) §7.7). The stream is scoped per
cluster, so one subscription serves every node of one `StorageCluster`. A
reconcile that finds `status`, `health`, and the reported resources unchanged
returns without patching.

One read stays direct. The storage-node API reachability probe of §4.2 is a
Kubernetes-side check against a pod, not a control-plane object, so it has no
stream to arrive on. A `RequeueAfter` survives for that and as a slow backstop for
a stream nothing has yet noticed is dead.

### 4.5 Deletion

The finalizer is `storage.simplyblock.io/storagenode-finalizer`, and deletion is
the one path where the entity controller creates an operation.

A node with no `status.uuid` has no backend node behind it, so its finalizer is
removed immediately. A node that is `online`, `suspended`, or `active` has data on
it, and deleting the object must not delete the node underneath without moving
that data first. The controller therefore raises a `StorageNodeOps` with
`action: Remove`, owned by the `StorageNode` through a controller reference, and
holds the finalizer until `status.activeOpsRef` is clear.

**The entity owns the operation it raised for itself.** That is the one direction
ownership runs between the two categories
([`design-crd-model.md`](design-crd-model.md) §3): an operation never owns its
target, and an operation an entity created for itself is a subordinate of it. A
`StorageNodeOps` a user created is not owned by anything and outlives the node as
its audit record.

Raising the operation is idempotent by name, `<node>-remove`, so a reconcile that
runs again while the drain is in flight finds it rather than creating a second.

---

## 5. The Storage-Node Workload

A backend storage node is an SPDK process on a worker, and something has to put it
there. That something is a DaemonSet, a headless Service, an EndpointSlice per
pod, a serving certificate, a ServiceAccount with its cluster role, and a
ConfigMap the init container reads its configuration out of. `StorageNodeSet` owns
all of them today by controller reference, which is why deleting one tears the
storage plane down, and which is why the kind cannot be retired until the
ownership moves.

### 5.1 What the cluster owns

The workload objects become children of `StorageCluster`, established by
controller reference at the point each is created. That places them in the
ownership spine ([`design-crd-model.md`](design-crd-model.md) §5) and keeps the
property that made them owned in the first place: deleting the thing they exist
for tears them down.

Their configuration becomes one spec group on the cluster.

```go
// Image is the storage-node container image. Defaults to the ControlPlane
// singleton's spec.image when unset, so a deployment states the version once.
// +optional
Image string `json:"image,omitempty"`

// NodeProvisioningBudget limits how many workers may be in the node-add process
// at once. Workers hosting a FoundationDB pod are always sequential regardless of
// this value.
// +kubebuilder:validation:Minimum=1
// +kubebuilder:default=1
// +optional
NodeProvisioningBudget *int32 `json:"nodeProvisioningBudget,omitempty"`

// EnableKubeletConfiguration lets the storage node apply the kubelet
// configuration changes it needs. Off by default, which is the behavior
// skipKubeletConfiguration expressed by being set.
// +optional
EnableKubeletConfiguration *bool `json:"enableKubeletConfiguration,omitempty"`
```

**The two network-interface fields are immutable once set.** `mgmtInterface` and
`dataInterfaces` reach the control plane as the node-add's `interface_name` and
`data_nics`, which it spends on the node it is adding and never revisits. An edit
afterward reconfigures no node that already joined, so the cluster would name one
set of NICs while every node built from it ran on another. Growing the list falls
under the same rule for the same reason: the nodes already added do not pick the
new interface up. Once-set rather than from-creation, so a cluster created before
its network was decided can still be given them.

The whole group is Appendix C, which states it against the file it lands in
rather than either of this document's own. It sits at
`StorageCluster.spec.storageNodes`, which is an addition to a kind this document
does not own.
[`design-storagecluster.md`](design-storagecluster.md) §3.1 has to absorb it, and
§15 records that as a consequence of the retirement rather than as a separate
decision.

**One node set per cluster is what this collapses to.** `StorageNodeSet` allowed
several sets per cluster, each with its own DaemonSet, selected by a per-set node
label. Moving the workload to the cluster makes it one DaemonSet per cluster, and
`spec.nodeSet` on the node survives as the name of the group a node was declared
in rather than as a selector.

**One workload per cluster is enough, because growth is nodes rather than sets.** The
case a second set was for is a cluster that grew, and a cluster grows by acquiring
`StorageNode` objects, two ways. One is a `StorageNode` written by hand, which §3.4
admits once its `clusterRef` resolves and its sizing matches the cluster's. The other is
running discovery again against the new hardware and applying the config it produces
with `spec.clusterRef` naming the existing cluster, which
[`design-clusterdeploymentconfig.md`](design-clusterdeploymentconfig.md) §6 already
specifies as a growth document: the same cluster, and only the nodes that are new.
Neither path needs a second DaemonSet, because neither adds a second *kind* of node.
Each adds nodes to the one that exists.

**What differs between hardware generations is per-node already.**
`config.spdkImage`, `config.spdkProxyImage`, and `config.spdkSystemMemory` are node
fields precisely so that a rollout can be phased (§3.2), and `config.sizing` is a node
field so that a re-size can walk the fleet one machine at a time (§3.1). A second
workload would be a second place to express what these already express, and it would
express it per group rather than per node, which is coarser than the thing it is
competing with.

### 5.2 The storage-plane labels

Two labels on the Kubernetes `Node` object make the workload land, and one of
them is load-bearing far outside this document.

| Label                                                   | Value                         | Read by                                                                      |
|---------------------------------------------------------|-------------------------------|------------------------------------------------------------------------------|
| `io.simplyblock.storagenodeset`                         | The node set name             | The DaemonSet's node selector, and the numa-plugin and fluent-bit affinities |
| `simplyblock.io/storage-node-uuid.<clusterUUID>.<slot>` | The backend storage node UUID | The CSI node plugin and controller plugin                                    |

**The second label's key must never change for a worker's lifetime, and its value
may change freely.** Kubernetes' external-provisioner caches the *set* of topology
keys in the `CSINode` object when the node plugin registers, refreshes it only
when that pod restarts, and then hard-errors `CreateVolume` when a live `Node`'s
topology keys do not match the cached set. The key is therefore scoped by cluster
UUID and slot ordinal, both of which are fixed for the slot, while the UUID that
identifies which backend node currently occupies it is the value, which is always
read fresh. A node replaced or relocated changes the value alone.

Cluster-scoping the key, rather than only the value, is what lets one worker host
nodes of two simplyblock clusters without one cluster's slot colliding with the
other's.

Rebuilding the label set from a failed `List` is the failure mode that matters:
an empty result read as "no slots are desired" would delete every
`storage-node-uuid` label from every worker and break CSI topology across the
cluster. The reconcile aborts on a `List` error rather than proceeding with a
partial view.

The keys move to the `storage.simplyblock.io` prefix with everything else
([`design-crd-model.md`](design-crd-model.md) §7.3 and §9.4), except the CSI
topology key the node plugin publishes, which belongs to the upstream topology
convention and stays under `topology.simplyblock.io`.

### 5.3 Per-node configuration

The DaemonSet's init container reads its configuration from one ConfigMap holding
one entry per worker, keyed by hostname, each a shell-sourceable env file. One
ConfigMap rather than one per node is what lets a single DaemonSet serve nodes
that differ.

```yaml
data:
  worker-3: |
    MAX_SUBSYS_COUNT=20
    MAX_HUGE_PAGES_SIZE=100G
    VCPU_COUNT=8
    PCI_ALLOWED='0000:5e:00.0,0000:5f:00.0'
    PCI_BLOCKED=''
    NVME_DEVICES=''
    DEVICE_MODEL=''
    SIZE_RANGE=''
    JM_PERCENT=3
    HA_JM_COUNT=3
```

`MAX_SUBSYS_COUNT` comes from `StorageCluster.spec.maxSubsystemCount` and is
identical in every entry, because the cap is the cluster's and no node carries a
copy of it (§3.1). `MAX_HUGE_PAGES_SIZE` and `VCPU_COUNT` come from the node's own
`spec.config.sizing`, which is equal across the fleet in steady state and
deliberately unequal for the duration of a rolling hardware upgrade. The rest come
from the node's `spec.config`.

**The ConfigMap is written before the DaemonSet on every pass.** A pod that starts
against a missing or empty entry reaches the node configuration script with
`--max-subsys-count=0` and fails there, which is a long way from the cause. For the
same reason, a cluster missing its required sizing fields is refused with an error
naming them rather than written out as blanks.

**A change to the cluster's `maxSubsystemCount` therefore reaches every node.** The
next pass rewrites every entry, and each node picks the new value up when it next
restarts, which is what makes the cap a cluster-level setting rather than a stamp
([`design-storagecluster.md`](design-storagecluster.md) §3.2).

The node objects and the cluster they belong to are the source, so the ConfigMap is
derived rather than authoritative and can be rebuilt from them at any time.

### 5.4 The address a node is reached at

Each storage-node pod publishes a per-pod DNS name in the headless Service's
EndpointSlice:

```
<worker-hostname-label>.simplyblock-storage-node-api.<namespace>.svc.cluster.local:5000
```

That name is what the control plane is given as `node_address` when a node is
added or restarted, so it is the precondition for both. A restart issued against a
name that does not yet resolve fails name resolution inside the control plane, and
the control plane's response to that is to reset the node to offline, which is why
the migration's `Preparing` step blocks on the EndpointSlice rather than on pod
readiness alone (§9).

The EndpointSlice is read through an uncached reader on that path. A stale informer
cache can miss a freshly published endpoint, and the consequence is a migration
that waits on DNS forever while the name has in fact resolved for minutes.

Serving certificates for the same Service are provisioned by cert-manager or by
OpenShift's service-ca, and the Secret's `resourceVersion` is stamped onto the
DaemonSet's pod template so a certificate rotation rolls the pods. The Secret's
name does not change when it rotates, so nothing else would notice.

---

## 6. StorageNodeOps: API

Declared in `operator/api/v1alpha2/storagenodeops_types.go`, short name `snops`.
The type is Appendix B.

### 6.1 Spec

```go
// StorageNodeOpsAction is the operation a StorageNodeOps performs.
// +kubebuilder:validation:Enum=Shutdown;Restart;Suspend;Resume;Remove;Migrate;HostMaintenance
type StorageNodeOpsAction string

// Abort asks a running operation to stop at its next step and unwind. It is the
// only mutable field on this spec, because it is the only thing about an
// operation that can legitimately be decided after it started.
// +optional
Abort bool `json:"abort,omitempty"`
```

**Per-action parameters are grouped under a block named for their action**, which
is the shape `StorageClusterOps.spec.rollingRestart` already has
([`design-storagecluster.md`](design-storagecluster.md) §5.1). Today
`targetWorkerNode` and `newSsdPcie` sit at the top level, where nothing but a doc
comment says they belong to one action, and `spec.drain` is named for a step of
the `Remove` workflow rather than for the action itself.

`nodeRef` and `action` are `Required` as well as `+k8s:immutable`, so they are
immutable from creation rather than once set. An operation is the record of one
thing done to one target, and re-pointing either afterward would make the record
describe something that never happened.

`force` and `reattachVolume` are action modifiers rather than capability toggles,
which is the class [`design-crd-model.md`](design-crd-model.md) §7.5 leaves
outside the `enableXyz` and `disableXyz` rule.

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: StorageNodeOps
metadata:
  name: move-node-off-worker-3
  namespace: simplyblock
spec:
  nodeRef: production-7f3a9c
  action: Migrate
  reattachVolume: true
  migrate:
    targetWorkerNode: worker-5
    newSsdPcie:
      - "0000:88:00.0"
```

### 6.2 Status

```go
// StorageNodeOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StorageNodeOpsPhase string
```

`Pending` means the operation holds no lock and has issued nothing. `Running`
means it holds its node's lock and its first side effect may have been issued
(§7.2). `Succeeded` and `Failed` are the terminal outcomes, and `Aborted` is the
third: a drain that has been running for an hour and is called off by an
administrator did not go wrong, and reporting it as `Failed` would put it in the
same bucket as one the control plane rejected.

`status.phase` and `status.step.state` each carry a `+kubebuilder:printcolumn`, so
`kubectl get snops` answers where an operation is without a `describe`.

```go
// VolumesTotal is the number of PV-managed volumes the drain has to move,
// written once at the end of Validating and not modified afterward.
VolumesTotal int32 `json:"volumesTotal"`

// VolumesMigrated is how many of them have completed.
VolumesMigrated int32 `json:"volumesMigrated"`
```

Neither field takes `omitempty`: zero is a meaningful value for both, and a field
that disappears at zero makes "nothing to move" and "not yet counted" the same
wire value.

#### Examples

A drain part-way through moving a node's volumes:

```yaml
apiVersion: storage.simplyblock.io/v1alpha2
kind: StorageNodeOps
metadata:
  name: production-7f3a9c-remove
  namespace: simplyblock
spec:
  nodeRef: production-7f3a9c
  action: Remove
status:
  phase: Running
  step:
    state: MigratingVolumes
    deadline: "2026-08-28T14:12:00Z"
  drain:
    volumesTotal: 12
    volumesMigrated: 7
  message: 7 of 12 volumes migrated
  startedAt: "2026-08-28T11:44:02Z"
  observedGeneration: 1
```

The same drain, blocked before anything was touched:

```yaml
status:
  phase: Running
  step:
    state: Validating
    deadline: "2026-08-28T12:44:00Z"
  drain:
    volumesTotal: 0
    volumesMigrated: 0
  message: "blocked: 2 pinned volumes, remove the storage.simplyblock.io/selected-storage-node annotation"
```

The node is untouched and still `online`. Blocking before the suspend rather than
after is the point of putting validation first (§8.2).

A migration that was called off:

```yaml
spec:
  nodeRef: production-7f3a9c
  action: Migrate
  abort: true
  migrate:
    targetWorkerNode: worker-5
status:
  phase: Aborted
  step:
    state: Preparing
  message: aborted before the relocation was issued
  startedAt: "2026-08-28T09:02:11Z"
  completedAt: "2026-08-28T09:04:50Z"
```

`step.state` is `Preparing`, which is what makes the abort safe to report as an
abort rather than a failure: nothing had reached the control plane yet (§7.2).

### 6.3 The step machine

[`design-crd-model.md`](design-crd-model.md) §3.1 requires every `Ops` kind to
carry a `status.step` holding the serialized snapshot of a declared
`atlas-lib/statemachine` graph. `StorageNodeOps` serves seven actions, so it takes
the `MultiConfig` form: one graph per action over one step type.

```go
// StorageNodeOpsStep is the union of every action's steps; which steps belong to
// which action is declared by the graph rather than by this type.
// +kubebuilder:validation:Enum=Requesting;Departing;Awaiting;Validating;MigratingDevices;MigratingVolumes;Verifying;Removing;AwaitingRemoval;Preparing;Relocating;AwaitingNode;Promoting;Holding;ShuttingDown;Releasing;AwaitingHost;Restarting;Cleanup
type StorageNodeOpsStep string
```

**Every action value is PascalCase**, which is the rule for an enum this API group
defines rather than a choice this kind makes: it matches `status.phase` and
`status.step.state` beside it, and it matches every enum core Kubernetes owns
([`design-crd-model.md`](design-crd-model.md) §7.8). §15.2 is what the rename
costs.

**The stored form is `statemachine.KubeSnapshot`, shared with every other `Ops`
kind rather than declared here** ([`design-crd-model.md`](design-crd-model.md)
§3.1). The enum above types the step in Go and constrains the `MultiConfig`, and the
same values appear once more as a CEL rule on `status.step`, which is what an
`Enum` marker would do if a marker could reach a field of a type another module
declares. Those two lists and the graphs are kept level by a test over
`statemachine.DeclaredMultiStates`, not by review.

**A stored step the graph does not declare fails the operation.**
`MultiConfig.FromSnapshot` returns `ErrUnknownState` naming the value and the
states that were declared, and the controller records that as `Failed` rather
than requeuing: an unrecognized step is a downgrade, a hand-edited resource, or a
rename that shipped without a conversion, and none of those resolve by trying
again. A step belonging to a different action is the case the CEL rule cannot
catch, because the rule is the union over the kind and the graph is per action.

The seven graphs:

```
Shutdown, Suspend, Resume
    Requesting ──► Awaiting

Restart (§7.3)
    Requesting ──► Departing ──► Awaiting

Remove (§8)
    Validating ──► ShuttingDown ──► MigratingDevices ──► MigratingVolumes
               ──► Verifying ──► Removing ──► AwaitingRemoval

Migrate (§9)
    Preparing ──► Relocating ──► AwaitingNode ──► Promoting

HostMaintenance (§10)
    Holding ──► ShuttingDown ──► Releasing ──► AwaitingHost ──► Restarting ──► Cleanup
```

**The union stops being a union.** One `status.step` field serves all seven
actions, so nothing in the type prevents a `Remove` from reporting `Promoting`.
The per-action graph makes that an `IllegalTransitionError` at the point of the
write rather than an accepted status update.

**Every graph is validated, not only the one in hand.** `MultiConfig` checks all
seven whenever a machine is built for any of them, so a bad edge in
`HostMaintenance`, which only runs during an OS upgrade and is the most expensive
action to exercise, is caught by any test that builds a machine at all.

**Two steps are shared between actions, and both genuinely mean the same thing.**
`AwaitingNode` waits for the node to report `online` in both `Migrate` and, were
it needed, anywhere else. `MigratingVolumes` is named for what it does rather than
`Migrating`, which in the registered enum means "move the volumes off" in the
`Remove` workflow and "issue the relocation restart" in the `Migrate` one. One
name for two unrelated operations in one enum is the kind of thing the graph
cannot catch, because both are legal in their own action.

An abort is a transition to a terminal outcome from wherever the machine is, and
the graph is what decides whether it is expressible. A `Migrate` that has already
issued `/promote` cannot be unwound, so `Promoting` declares no edge to an aborted
state and the abort is refused with an `IllegalTransitionError` the controller
turns into a message rather than a failure (§7.2).

---

## 7. StorageNodeOps: Controller

`StorageNodeOpsReconciler`, in `controllers/node/storagenodeops_controller.go`. It drives every
imperative operation against a node, and it is the only writer of the node's
operation lock.

### 7.1 Reconcile

```
  CR observed
    │
    ▼
  Deleting?          ← release the lock, remove the finalizer, stop
    │  no
    ▼
  Finalizer present? ← add it and return, so the lock can always be released
    │  yes
    ▼
  Terminal phase?    ← release the lock again best-effort, stop
    │  no
    ▼
  Admitted already?  ← Running, or the lock names it → skip the gate
    │  no
    ▼
  Cluster available? ← not active, rebalancing, or no object → stay Pending, emit, requeue
    │  yes, or the action is exempt
    ▼
  Get the node       ← not found → Failed
    │  found
    ▼
  Lock free?         ← held by another ops → stay Pending, requeue after 15s
    │  free or ours
    ▼
  Acquire the lock   ← optimistic-lock patch; 409 → requeue immediately
    │
    ▼
  Pending → Running  ← stamp startedAt
    │
    ▼
  Restore the machine for spec.action and advance it
```

The terminal-phase branch releases the lock a second time on purpose. A crash
between persisting `Succeeded` and clearing `activeOpsRef` would otherwise leave
the node locked by a finished operation forever, and the release is idempotent
(§11).

**The cluster gate is not the same as the lock.** A node operation that moves
data runs inside a cluster, and one whose cluster is mid-rebalance or not active
will either be rejected by the control plane or succeed into an inconsistent
layout. The operation holds rather than fails, emits `ClusterNotReady`, and is
admitted when the cluster is ready. It applies to the two actions that move data,
`Migrate` and `HostMaintenance`, and to no others.

**The gate is an admission check, asked once and never of an admitted
operation.** A held operation stays `Pending` and holds nothing: it has not taken
the node's lock, so an operation queued on the same node is not queued behind a
wait. Admission is the lock rather than the phase, because the lock is patched
onto the node before the phase is written and a crash between the two restores
an operation that holds its lock at `Pending`. Once admitted, an operation is
never gated again, because an admitted operation
changes the cluster's own reading: a relocation restarts the node and a
maintenance window shuts it down, and either makes the cluster degraded until
the node is back. A gate asked on every pass held each of them on the
consequence of its own action, and it stood in front of the deadline check, so a
held operation could never expire. The steps of a running operation are guarded
by their own preconditions and by their deadlines, and the deadline is the first
thing a pass reads after the abort flag.

**The five other actions are exempt, because each is how the cluster's reading
changes.** A removal is how an unready cluster becomes ready: holding it until
the cluster is active closes a loop with no way out, since the node cannot be
removed until the cluster is active and the cluster cannot become active while
the node it is stuck on is still in it, which is what a node whose add never
finished does to the cluster it was being added to. A `Shutdown` or a `Suspend`
is what makes a cluster degraded, and a `Restart` or a `Resume` is what makes it
active again, so a gate on that reading holds the first pair on what they
produced and makes the second pair unavailable exactly when they are needed. One
`Shutdown` against a healthy four-node cluster demonstrated the loop on
2026-09-28: the node it took down degraded the cluster, the degraded cluster
held the shutdown in `Awaiting` with the node's lock for hours, and three
`Restart` operations queued behind it. The exemptions follow from the gate's own
purpose rather than working against it: none of the five moves data, and each is
what lets the control plane reach a state it will accept further work in.

Watching only `StorageNodeOps` would leave a queued operation waiting up to its
requeue interval after the lock frees. `nodeToOpsRequests` maps a `StorageNode`
event back to every operation targeting it, so a release wakes the queue
immediately.

### 7.2 The persisted position is the write-ahead record

A side effect is preceded by a write, so that a process dying between the two
restarts into a state saying the call may already have landed. Two levels carry
that, and neither needs a flag beside it.

`status.phase` carries the outer one. `Pending` means the operation holds no lock
and has issued nothing. `Running` means it holds the lock and its first side effect
may have been issued. The transition is persisted before the machine is advanced.

`status.step` carries the per-step one. A step is persisted before the side effect
that step performs, so a restart finds the step recorded rather than deciding
afresh whether to make the call.

**A recorded step whose side effect never fired is indistinguishable from one
whose did, and that is safe rather than merely tolerated.** Every step's
completion condition is a predicate over current state rather than an observation
of a transition ([`design-crd-model.md`](design-crd-model.md) §7.7), and every
call a step makes is skipped when its target is already at or past the state that
call would produce. A node already `suspended` receives no second suspend, and a
node already `offline` receives no second shutdown.

`status.triggered` therefore disappears. It exists today because the steps do not
check the state they are trying to reach before calling, so the flag is what
prevents the second `POST`. Checking the state instead removes the flag and also
removes the failure it cannot cover: a crash between the `POST` and the flag's
write leaves the flag false and the call made, which is the case the flag was
supposed to prevent.

**An abort is a transition, so its safety is a property of the graph.** The
controller asks the machine to move to `Aborted` and reads what comes back. A
refusal names the step the operation was in, which tells the controller that
nothing reached the control plane from there, and an accepted transition runs the
step's unwind. A `Remove` is abortable only in `Validating`, which has changed
nothing, so its abort has no unwind (§8.3).

### 7.3 The four single-step actions

`Shutdown`, `Restart`, `Suspend`, and `Resume` are one call and one wait.
Three of them share a two-step graph:

```
    Requesting ──► Awaiting
      ← POST the action        ← wait for the completion condition below
```

`Restart` waits twice, because its completion state is the state it started in.
The control plane accepts the call and performs it on a thread, so the node
keeps reporting `online` for a moment after the request, and a wait for `online`
alone completes before the restart began:

```
    Requesting ──► Departing ──► Awaiting
      ← POST the restart   ← status != online   ← status == online
```

`Departing` is the observation that the restart has begun, and it is satisfied
at once by a node that was offline when the restart was requested. A stream that
coalesces can deliver `online` before and `online` after with nothing in
between, and the step then waits out its budget and fails an operation whose
restart happened. That is the trade §16 Q4 records, and it is the right side of
it: a false failure is visible and recoverable, where the false success it
replaces released the node's lock while the node was restarting.

| Action     | Backend call                          | Completion condition                        |
|------------|---------------------------------------|---------------------------------------------|
| `Shutdown` | `POST /storage-nodes/{node}/shutdown` | `status == offline`                         |
| `Restart`  | `POST /storage-nodes/{node}/restart`  | `status != online`, then `status == online` |
| `Suspend`  | `POST /storage-nodes/{node}/suspend`  | `status == suspended`                       |
| `Resume`   | `POST /storage-nodes/{node}/resume`   | `status == online`                          |

`Restart` passes `reattachVolume` and `force` through when they are set. The
completion condition is evaluated against the streamed storage-node object (§4.4).

Each call is skipped when the node is already at or past the state the call
produces, and "past" includes the state of the call being performed: a node
reporting `in_shutdown` has a shutdown landed on it, and one reporting
`in_restart` has a restart. The second matters most, because a forced restart
into a node already restarting is two restarts of one node, and the step is
re-entered after any crash between the call and the step record (§7.2).

**An unforced restart of a node that is not offline is refused by the operator,
terminally and with the reason.** The control plane restarts only an offline node
unless the restart is forced, and it says so nowhere a caller can see: the
request is accepted with 202 and the restart is dropped on the thread that would
have performed it. An operation that issued that call would then read an online
node as a finished restart. Refusing first is what makes the operation say what
it needs, which is the node shut down first or `spec.force` set. `Migrate` is
different, and forces by default, because a relocation always restarts a node
that is online (§9).

The endpoint column keeps the control plane's own lowercase paths, and the
completion column the control plane's own status strings. A URL segment and a
backend status are the control plane's vocabulary rather than this group's, and
only a value this API defines is PascalCase.

---

## 8. Remove: Draining a Node Before It Leaves

Removing a storage node destroys it, and every logical volume whose data lives on
it has to be somewhere else first. The drain is the part of the operation that
makes that true, and the removal is the last step rather than the operation.

### 8.1 Volume classification

Validation lists the backend volumes on the node and puts each in one of four
buckets.

| Bucket         | Criterion                                                        | What happens                                          |
|----------------|------------------------------------------------------------------|-------------------------------------------------------|
| **PV-managed** | The volume matches a `PersistentVolume` in the cluster           | A `PersistentVolumeOps` moves it to a peer node       |
| **Pinned**     | Its claim carries `storage.simplyblock.io/selected-storage-node` | Blocks, because moving it would violate the pin       |
| **System**     | Its name matches `spec.remove.systemVolumeFilterRegex`           | Skipped, and deleted during verification              |
| **Unmanaged**  | No `PersistentVolume` matches it                                 | Blocks, because nothing in Kubernetes accounts for it |

**A pin is an operator-only concept.** The control plane knows nothing about the
annotation, so the check is made against the claim directly. Resolving the block
means removing the annotation, after which the next reconcile proceeds without any
further input. The operator does not remove it on the user's behalf: a pin is a
placement decision somebody made deliberately, and silently undoing it to
complete a drain would move a volume the pin exists to keep in place.

**System volumes are the rebalancer's own benchmark volumes**, created to measure
per-node latency, and they must neither block a drain nor be migrated. They are
identified by name, which is a convention rather than a guarantee, and §16 Q1 is
whether they should carry a label instead.

**An unmanaged volume is a genuine unknown**, and blocking is the only safe answer:
migrating it moves data nothing in Kubernetes is tracking, and deleting it destroys
data nothing in Kubernetes is tracking.

### 8.2 The steps

```
    Validating ──► ShuttingDown ──► MigratingDevices ──► MigratingVolumes
               ──► Verifying ──► Removing ──► AwaitingRemoval
```

The operator shuts the node down first, and the control plane then carries out
the removal in three calls, which the operator drives in order.
`prepare-removal` admits the node and rebuilds its devices onto the peers. The
operator moves the volumes, and `verify-drained` closes that half. The node
DELETE takes the node apart.

| Step               | Side effect on entry                                                                                                             | Complete when                                                                |
|--------------------|----------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------|
| `Validating`       | None                                                                                                                             | No pinned and no unmanaged volumes remain                                    |
| `ShuttingDown`     | `POST /storage-nodes/{node}/shutdown`, sent only to a node that is `online` or `suspended`                                       | The call returns 202, or the node is not running                             |
| `MigratingDevices` | `POST /storage-nodes/{node}/prepare-removal` once the node is `offline`, and again on every pass from `migrating_devices` on     | `GET /storage-nodes/{node}/prepare-removal` reports `done`                   |
| `MigratingVolumes` | One `PersistentVolumeOps` per NVMe-oF subsystem of the PV-managed volumes, to peers chosen round-robin                           | Every migration is `Succeeded`                                               |
| `Verifying`        | Deletes any remaining system volumes, then `POST /storage-nodes/{node}/verify-drained`                                           | The node reports no volumes, and `verify-drained` reports it drained         |
| `Removing`         | `DELETE /storage-nodes/{node}?force_remove=false`, skipped once the node is `in_removal` or later, and while it is `in_shutdown` | The call returns 200, 204, or 404, or the node reports `in_removal` or later |
| `AwaitingRemoval`  | None                                                                                                                             | The node reports `removed`, or 404                                           |

**Validation runs before the shutdown, and that ordering is the design.** From
the shutdown on there is no way back, so a drain that cannot complete is held
while the node is still fully operational, and somebody decides what to do about
the pinned claim.

**`Validating` holds rather than fails.** It has a deadline like every other step,
but a blocked drain is a correct outcome waiting on a human, so the deadline is
long and expiry is reported rather than treated as an error (§16, Q2).

**The shutdown is the node's own, and comes first.** `prepare-removal` would shut
a running node down itself, but inside its request: it blocked for longer than
the client waits, and on a slow SPDK kill reported the shutdown failed although
SPDK was gone, which left the node `pending_removal` with no rebuild. The node's
`shutdown` answers at once and runs in the background, and with the node down
`prepare-removal` has no shutdown of its own to run. Only a node still running
is shut down: one already `offline` is not shut down again, one `in_shutdown` is
under a shutdown already, and one in a removal status is past the step.

**The shutdown is graceful, and a 409 is waited on.** The control plane answers a
graceful shutdown whose precondition is not met with a 409 naming it: a peer
restarting or shutting down, a migration or restart task, a live restart claim.
Each clears by itself, and the node is still serving, so the step holds with
`RemovalDeferred` and the reason, and the claim's lease sends the shutdown again
at most once a minute. The shutdown is not forced past those conditions, because
they are what keeps two nodes from being down at once, and the removal's admission
has not yet judged what the cluster can afford to lose. Any other 4xx is a refusal
with the node still serving, so the operation fails. A timeout or a 5xx is
retried, and the next pass reads the node.

**Admission follows the shutdown.** `MigratingDevices` waits for the node to be
`offline`, and a node still running a whole shutdown budget after its shutdown
was accepted fails the operation with nothing changed. `prepare-removal` on the
`offline` node is the control plane's admission (fault-tolerance headroom,
failure-domain balance, replica relocation). A refusal fails the operation and
leaves the node offline, because nothing in a removal brings a node back, and
the failure says so: a `Restart` operation returns it to service.

**A refusal that passes by itself is waited on, in both calls.** `prepare-removal`
and the DELETE run the same admission, and some of its refusals describe a moment
rather than the cluster: a cluster still rebalancing, an active task on the node,
or a peer that is down. With the node already shut down, failing on one of them
would leave it offline for a condition that clears minutes later. The step holds
instead with `RemovalDeferred` and the control plane's reason, the claim's lease
sends the call again at most once a minute, and the step's deadline bounds the
wait. A refusal is judged by the reason it names. A DELETE refused without one is
judged by the cluster stream: a cluster that is not `active`, or is rebalancing, is
waited on, and a settled one is final. A cluster the stream has not reported is
not taken for a busy one.

**`MigratingDevices` is the control plane's rebuild.** From `migrating_devices`
on, every pass sends `prepare-removal` again, which the control plane treats as a no-op while the
rebuild runs and as a restart of it after a control-plane restart, and reads the
rebuild's progress. A rebuild the control plane gave up on fails the operation.

**A node stuck in `pending_removal` gets `prepare-removal` again.** The control
plane marks the node `pending_removal` before it shuts the node down, and leaves
it there when the shutdown fails, with no rebuild started. Nothing is sent while
the shutdown may still be running. The step sends `prepare-removal` again when
the control plane reports the step failed (a minute after the previous attempt),
or when the node's status has not moved for longer than the 15-minute shutdown
budget, because the control plane can fail the shutdown without saying so. The
attempts are counted in `status.removal.prepareAttempts` before each is sent, and
after three the operation fails with the control plane's message.

**The fan-out is one move per NVMe-oF subsystem.** The control plane migrates a
subsystem as a whole, so the PV-managed volumes published under one subsystem leave
the node at the one cutover, and the drain raises one `PersistentVolumeOps` for all
of them. The move is named by the subsystem's lexicographically first volume, which
keeps its name stable across passes. A subsystem is covered by a move named after
any of its volumes, including one that has already left the node: the control plane
moves the members' records to the target one at a time during a cutover, so a
sibling can still be reported on the node while its subsystem's move runs. Progress
in `status.drain` is counted in volumes, from the member count each move records. A
volume the control plane reports under no subsystem is a move of its own.

**Migration targets are chosen round-robin over the online peers that hold none
of the replicas of any volume in the subsystem, and that the subsystem has not
already failed on (§8.4).** The control plane lists a volume's replica nodes, and
a node already holding one is never a target: the control plane refuses the
move, and while the volume's primary is shut down for its removal that replica is
what serves the volume. Round-robin over the rest spreads the drained node's
volumes rather than concentrating them on whichever peer sorts first. A drain
with no online peer to move to, a volume whose replicas cover every online peer,
or a subsystem that has already failed on every peer left, is a stall, not a
failure, and emits `NoMigrationTarget`, naming the targets tried. The condition is
resolved by another node coming back or being added, and failing the operation
would only mean starting it again afterward. The step's deadline bounds the wait.

**`Verifying` closes on the control plane's word.** The census walks the pools
for volumes and does not see snapshots, and the node DELETE refuses a node that
still holds either, so the step finishes only when `verify-drained` reports the
node drained, and holds naming what is left otherwise.

**`Verifying` deletes system volumes rather than migrating them.** They are
per-node benchmark artifacts, so moving one to a peer would produce a benchmark
volume measuring the wrong node. A delete the control plane rejects for a reason
other than "already gone" fails the operation, because a volume that cannot be
deleted and cannot be migrated is a volume the removal would destroy.

**`Removing` treats 404 as success.** A node the control plane no longer knows
about is a node that has been removed, and a retry after a lost response is the
common way to arrive there.

**Only an answer is a refusal.** A 4xx to the DELETE fails the operation, unless
it is a refusal that passes by itself (above). A timeout or a 5xx is retried, and the retry reads the node first. `in_removal`,
`removed`, and `removed_failed` say the teardown has begun, and get no second
DELETE. The preparation statuses do not: `prepare-removal` leaves the node
`migrating_lvols` before the DELETE is ever sent, so a node in one of them gets
the DELETE, which the control plane answers with the running removal when one
is already in flight. `in_shutdown` is waited on.

**`AwaitingRemoval` is the control plane's teardown.** The DELETE rewires the
node's replicas and removes its devices before the node reports `removed`. The
step completes on `removed` and fails on `removed_failed`, which is the control
plane giving up on the removal.

**`MigratingDevices` and `AwaitingRemoval` have budgets without progress.** Each
pass records the node's status and the status of each of its `StorageDevice`s in
`status.removal`, and any change moves the step's deadline a whole budget out
from that moment, through the state machine's `Extend`. `migrating_devices` is
one node status for a rebuild that can run for hours, and what changes during it
is the devices, one at a time as each one's data lands on the peers. Either step
is therefore failed only once a whole budget, seven hours, passes with nothing
changing.

**Each call is made under a claim on the step.** A pass that read the operation
from a cache one write behind would otherwise send the shutdown,
`prepare-removal`, or the DELETE a second time. The claim is a one-minute lease on
the stored step, so `prepare-removal`, sent again on every pass of
`MigratingDevices`, goes out at most once a minute. Every write of the step from
the machine keeps the claim the stored step carries, because the machine's
snapshot has none, and dropping it would let the next pass send the call again.

### 8.3 There is no way back

Nothing in a removal resumes the node. `Validating` changes nothing, and from
the shutdown on the node is being taken out of the cluster: it is shut down, its
devices are rebuilt onto the peers, and every later status only moves forward. A removal that fails from there leaves the node to
the control plane, which reports it `removed_failed` when it gives up and
accepts the removal being driven again.

`Validating` is therefore the only abortable step, and the only one whose
operation can be deleted while it runs.

| Condition                         | Step                           | Result                                              |
|-----------------------------------|--------------------------------|-----------------------------------------------------|
| Pinned or unmanaged volumes       | `Validating`                   | Hold, emit, requeue. The node is untouched          |
| The shutdown cannot run yet (409) | `ShuttingDown`                 | Hold, emit, ask again once a minute                 |
| The shutdown was refused          | `ShuttingDown`                 | `Failed`, the node still serving                    |
| The shutdown got no answer        | `ShuttingDown`                 | Retry, reading the node first                       |
| The shutdown never landed         | `MigratingDevices`             | `Failed` after 15 minutes, the node still serving   |
| `prepare-removal` was refused     | `MigratingDevices`             | `Failed`, the node left offline                     |
| A refusal that passes by itself   | `MigratingDevices`, `Removing` | Hold, emit, ask again once a minute                 |
| The shutdown failed or stalled    | `MigratingDevices`             | `prepare-removal` again, three times, then `Failed` |
| The device rebuild gave up        | `MigratingDevices`             | `Failed`                                            |
| No online peer to migrate to      | `MigratingVolumes`             | Hold, emit, requeue                                 |
| A `PersistentVolumeOps` failed    | `MigratingVolumes`             | Keep it and retry against a peer not ruled out      |
| Every eligible peer ruled out     | `MigratingVolumes`             | Hold, emit, requeue, naming the targets tried       |
| A `PersistentVolumeOps` aborted   | `MigratingVolumes`             | Delete it and re-issue it, ruling nothing out       |
| Non-system volumes remain         | `Verifying`                    | Hold, emit, requeue                                 |
| `verify-drained` sees something   | `Verifying`                    | Hold, emit, requeue, naming what is left            |
| A system volume cannot be deleted | `Verifying`                    | `Failed`                                            |
| The removal call was rejected     | `Removing`                     | `Failed`                                            |
| The removal call got no answer    | `Removing`                     | Retry, reading the node first                       |
| The control plane gave up         | `AwaitingRemoval`              | `Failed`                                            |
| A step's deadline expired         | Any                            | `Failed`                                            |
| `spec.abort` set                  | `Validating`                   | `Aborted` directly, since nothing has been done     |
| `spec.abort` set                  | Any later step                 | Refused; the operation runs on                      |

### 8.4 PersistentVolumeOps lifecycle

The operation raises one `PersistentVolumeOps` per subsystem (§8.2), and tracks the fan-out
by `spec.creatorRef` and the `storage.simplyblock.io/managed-by` label rather than
by an owner reference, which the kind's cluster scope forbids
([`design-crd-model.md`](design-crd-model.md) §3). The label is what a watch maps
back to this operation and what a `List` selects on, so the controller is woken by
each completion rather than polling for it, and the drain's finalizer aborts the
objects whose `creatorRef` names it before deleting them, so a deleted operation
does not leave migrations running behind it. Aborting first is also what makes the
cascade's own deletes admissible: a `PersistentVolumeOps` in `Verifying` refuses a
delete ([`design-persistentvolumeops.md`](design-persistentvolumeops.md) §4.3), and
waiting for each member to report a terminal phase is what the cascade does anyway.
[`design-persistentvolumeops.md`](design-persistentvolumeops.md) §11.1 specifies
the reference, the label, and the cascade.

| State       | What happens to the object                                                                                                          |
|-------------|-------------------------------------------------------------------------------------------------------------------------------------|
| `Succeeded` | Deleted. `status.drain.volumesMigrated` is the progress record, not the object's presence                                           |
| `Failed`    | Kept until the step ends, as the record of where the subsystem failed. A replacement is created against a peer it does not rule out |
| `Aborted`   | Deleted and re-issued. An abort is a decision rather than a verdict on the target                                                   |
| In flight   | `spec.abort` is set, and the object is deleted once it reports a terminal phase                                                     |

**A failed move is kept, because it is the drain's memory of the failure.** It
records the target it was headed for, in the `storage.simplyblock.io/target-node`
label, and whether the control plane had accepted its migration. A failure rules its
target out for that subsystem at once when the target took part: the migration was
accepted, or the control plane's refusal names the target. A failure that never
involved the target, such as a migration the cluster did not accept in time, is
counted against it and rules it out on the third. The replacement is named with the
attempt number, so a pass that runs again finds the object it made, and a restart
reads the same memory back from the objects. Each failure is one object and is
counted once. The failed moves are deleted on the pass that finds the node holding
nothing movable, which is the pass that finishes the step, and by the deletion
cascade above. Their number is bounded by the peers each subsystem can fail on.

Deleting completed objects immediately is what keeps a hundred-volume drain from
leaving a hundred objects behind. The counters in `status.drain` are the source of
truth for progress, which is why they are written before the object is deleted
rather than derived from a `List`.

A `PersistentVolumeOps` that is missing when the step expects one, which happens
when an object is deleted out of band, is recreated rather than treated as
complete.

---

## 9. Migrate: Relocating a Node onto Another Worker

A migration moves a storage node to a different worker host without draining it.
The node keeps its backend UUID, its partitions, and its logical-volume
assignments, and what changes is the machine the SPDK process runs on. It is
therefore not a removal followed by an add, and no `PersistentVolumeOps` is created.

The mechanism is a control-plane restart pointed at a different `node_address`,
which is the same primitive a host maintenance uses (§10) aimed somewhere else.

```
    Preparing ──► Relocating ──► AwaitingNode ──► Promoting
```

| Step           | Side effect on entry                                                              | Complete when                          |
|----------------|-----------------------------------------------------------------------------------|----------------------------------------|
| `Preparing`    | Clone the node's config onto the target, label the target into the storage plane  | The target's pod is `Ready` and in DNS |
| `Relocating`   | `POST /storage-nodes/{node}/restart` with the target's `node_address` and `force` | The node has left `online`             |
| `AwaitingNode` | None                                                                              | The node is `online` again             |
| `Promoting`    | `POST /storage-nodes/{node}/promote`, then re-point the Kubernetes topology       | The topology re-point has been applied |

**`Preparing` blocks on DNS, not on readiness.** The control plane resolves
`node_address` itself, and a name that does not resolve makes the restart fail
inside the control plane, whose response is to reset the node to offline. Pod
readiness happens before the EndpointSlice is published, so waiting on readiness
alone leaves a window in which the restart is issued against a name that does not
yet exist. The check reads the EndpointSlice through an uncached reader for the
reason in §5.4.

**The relocation restart is forced by default.** A migration relocates a node that
is still online, and the control plane rejects a non-forced restart of a node that
is not already offline. `spec.force` is honored when it is set explicitly, which is
the one place a default of true is the right one.

**`Relocating` and `AwaitingNode` are two steps because one would race.** The
restart is asynchronous, so a node that is still reporting `online` immediately
after the call may be reporting the state from before it. `Relocating` completes
when the node has left `online`, which is the observation that the restart has
actually started, and `AwaitingNode` completes when it is back. Collapsing them
means `/promote` can be issued while the restart's own node writes are still in
flight, and the relocated devices are left stuck in `new`.

This is the one place in the document where a step's completion condition is a
negative predicate, and it is worth naming as a weakness. A stream that coalesces
can deliver `online` before and `online` after without ever delivering what is in
between ([`design-crd-model.md`](design-crd-model.md) §7.7), so the departure from
`online` can be missed. What makes it tolerable is that the promote is separately
guarded: `Promoting` re-reads the node and its devices before issuing, so a
promote that would race is refused there. §16, Q4 is whether the control plane
should expose a restart generation that would make the observation positive
instead.

**`Promoting` is the step with no way back.** The promote activates the target
host's devices, fails and migrates the origin host's, starts a rebalance, and
re-homes the logical volumes. The graph declares no edge from `Promoting` to
`Aborted` for that reason, and an abort arriving there is refused by the machine
rather than by a hand-written check (§6.3).

**The topology re-point is the Kubernetes half and it happens after the promote.**
The node's `spec.workerNode` moves to the target, its worker label follows, the
node's `spec.config` gains any `newSsdPcie` addresses merged into its allow list
so they survive a rebuild, and the source worker's storage-plane labels are removed
if no other node remains on it. Doing this before the promote would leave the
Kubernetes view describing a relocation the control plane had not performed.

---

## 10. Host Maintenance: Surviving a Kubernetes Node Drain

A Kubernetes worker being drained for an OS upgrade takes its storage-node pod
with it. Left alone, the SPDK process is killed underneath a running backend node,
which the control plane sees as a node that vanished. The `HostMaintenance`
action is how the node is taken down deliberately, allowed out of the way, and
brought back.

**The operator raises this operation, and a user does not.** The trigger is the
worker being cordoned, which the `StorageNode` controller sees by watching
Kubernetes `Node` objects. It raises a `HostMaintenance` operation owned by the
`StorageNode`, in the same way it raises a `Remove` on deletion (§4.5). A user
creating one by hand is accepted and behaves identically, which is what makes the
flow testable without cordoning anything.

Modeling it as a `StorageNodeOps` rather than as its own controller is what gives
it the discipline the other actions have: it takes the node's lock, so nothing
else touches a node whose host is rebooting, its position is a persisted step
rather than a phase string in a fleet object's status, and it is an audit record
of a maintenance window afterward.

```
    Holding ──► ShuttingDown ──► Releasing ──► AwaitingHost ──► Restarting ──► Cleanup
```

| Step           | Side effect on entry                                                                          | Complete when                                     |
|----------------|-----------------------------------------------------------------------------------------------|---------------------------------------------------|
| `Holding`      | None                                                                                          | Fewer than the cluster's limit are in maintenance |
| `ShuttingDown` | Label the evictable pods, create a blocking PDB, `POST /storage-nodes/{node}/shutdown`        | The node is `offline`                             |
| `Releasing`    | Delete the PDB                                                                                | The SPDK pod is gone                              |
| `AwaitingHost` | None                                                                                          | The worker's storage-node API answers again       |
| `Restarting`   | `POST /storage-nodes/{node}/restart`, skipped if the node is already `in_restart` or `online` | The node is `online`                              |
| `Cleanup`      | Delete the PDB, remove the drain label                                                        | Terminal                                          |

The shutdown call is skipped when the node is already `in_shutdown` as well as
`in_restart`, for the same reason: re-posting it is refused with a 409, and a
refusal the step reads as an error spends the window's budget backing off against
its own progress.

**`Holding` is the concurrency gate, and it is a cluster-wide count.** How many
workers may be in maintenance at once is
`StorageCluster.status.maxConcurrentWorkerRestarts`, which is
`min(spec.maxConcurrentWorkerRestarts, status.maxFaultTolerance)` computed by the
cluster ([`design-storagecluster.md`](design-storagecluster.md) §3.3). Counting
operations rather than nodes is what makes the gate correct across a multi-socket
worker: two nodes on one host go into maintenance together, and the pair is one
worker's worth of unavailability.

**The PodDisruptionBudget is the throttle, and it runs backward from the usual
one.** A per-node PDB with no disruption allowed is created *before* the shutdown,
so `kubectl drain` blocks on it while the backend node is being taken down
gracefully. Removing it in `Releasing` is what lets the drain proceed. The
budget's job is therefore to hold the eviction until the storage node is safely
offline, rather than to keep a replica count up.

**What the budget selects is the set of pods a drain can actually evict**, and
the node agent's is not one of them. The agent runs in a DaemonSet, and
`kubectl drain --ignore-daemonsets` skips DaemonSet pods entirely, so a budget
over one holds nothing: the drain never asks about it. The three the budget does
cover are the SPDK pod (`role=simplyblock-storage-node`), the management API, and
the FoundationDB processes. The first is the point of the window. The other two
are there because the shutdown needs the control plane it is talking to, and
losing a webAPI replica or an FDB process mid-shutdown leaves the window with
nothing to ask.

**`Releasing` removes the budget rather than relaxing it to one disruption**, and
that follows from covering several pods. A budget at `maxUnavailable: 1` permits
the first eviction and then reports zero allowed disruptions until the evicted
pod is healthy again — and the SPDK pod is owned by nothing, so nothing
reschedules it. The drain the step just released would block on the budget for
the rest of the window.

**`Releasing` waits for the SPDK pod rather than the agent's**, for the same
reason the budget does not cover the agent's, and one more: a DaemonSet pod on a
rebooting host stays `Running` in the API, because DaemonSet pods tolerate
`unreachable` with no `tolerationSeconds`. Its absence is a state no drain, no
reboot, and no window can bring about. The SPDK pod's absence is exactly what "the
SPDK process is off this host" means, and the graceful shutdown ends in the
control plane deleting it.

The operator protects itself the same way when the worker being drained is its
own: a temporary self-budget prevents the manager pod from being evicted while it
is still setting the storage node's budget up on that host. A stale self-budget
left by a crashed manager is cleaned up on the next pass, since it would otherwise
make the node undrainable.

**The markers are the worker's, and a multi-socket worker shares them.** The
budget and the label are named per worker, and the concurrency gate admits one
window per socket at the same time on purpose — the pair is one worker's worth of
unavailability. They therefore hold the same budget and label the same pods, and
the socket whose backend node goes offline first must not drop the guard the
other's SPDK process is still standing behind, or the drain evicts a live one.
A window releases or clears the worker's markers only once no sibling window on
that worker is still before `Releasing`, and the last of them to end is what takes
them down. A sibling whose node cannot be read counts as still guarding: a drain
that waits is cheaper than an SPDK process that is evicted on a guess.

**Every terminal outcome takes the markers down.** `Cleanup` is on the success
path only — the graph is a chain with no edge from a failing step to it — so a
window that fails on a deadline or is aborted runs the same teardown from its
terminal transition. A budget at `maxUnavailable: 0` outliving the window that
raised it makes the worker undrainable by anything, forever, with nothing left
saying why. The teardown is best-effort, because retrying it forever would leave
an operation that can never end, and a `MaintenanceMarkersLeft` event is what
says a worker needs a hand.

**An uncordon is answered.** A window still at `Holding`, or raised and not yet
admitted at all, has done nothing to the node, so the cordon being undone calls it
off through `spec.abort`, which is what that step being abortable is for. An abort
skips the cluster gate: the gate keeps work off a cluster that cannot take it, and
an operation being called off is not going to do any, so holding it there would
leave a window alive on a worker nobody is draining any more. From `ShuttingDown`
onward the node is down and something has to bring it back, so the window runs
on, and the uncordon it is waiting for is the one `AwaitingHost` reads. A window
that has finished is **deleted** on the uncordon: the operation's name is derived
from the node's, so a terminal record left in place is what `ensureOps` finds on
the next cordon, and the worker would get one window for the lifetime of the
object with every drain after the first unheld. The worker being schedulable
again is the boundary that record belongs on.

**`AwaitingHost` is the step whose length nobody controls.** An OS upgrade and a
reboot take as long as they take, and the node's lock is held throughout. That is
correct, and it means the deadline on this step has to be generous enough for a
firmware update. An expiry is reported and the operation fails, which leaves the
node offline and requires a `Restart` operation to recover, so the deadline is a
detection mechanism rather than a recovery one.

**OpenShift MachineConfigPool pausing is not part of this.** A pool's
`maxUnavailable` is set high and the budget is the throttle instead, which keeps
one mechanism responsible for concurrency rather than two that have to agree.

---

## 11. Mutual Exclusion

`status.activeOpsRef` on the `StorageNode` names the operation currently allowed
to touch it, which is the lock every entity in this group carries under that name
([`design-crd-model.md`](design-crd-model.md) §3.2). That document states the
mechanism: an optimistic-lock acquisition, an idempotent release that checks
ownership, and a release on every terminal path. What is specific to this kind is
below.

**One operation per node is not a constraint this kind has to work around.**
Every action either takes the node out of service or changes what host it runs on,
and two of those at once have no defined outcome, so the limit is the point rather
than the cost. A second operation is admitted, sits at `Pending`, and runs when
the lock frees, and because `Pending` is where an operation starts anyway there is
no queue to build and nothing to reject.

The release path this kind adds is the finalizer
`storage.simplyblock.io/storagenodeops-finalizer`, which is what keeps
`kubectl delete` on a running operation from leaving the node locked by an object
that no longer exists.

**A `DELETE` is refused from the steps that declare no abort edge**, which is the
group rule reading this kind's graph
([`design-crd-model.md`](design-crd-model.md) §3.1). `Promoting` is the clearest
case: the promote has activated the target host's devices, failed the origin's, and
re-homed the logical volumes, so there is nothing to unwind and the operation is
what finishes the relocation (§9). The record is what carries the topology
re-point that is still owed, so admission keeps it. A `Remove` past `Validating`
is refused the same way: from `prepare-removal` on the control plane is taking
the node out of the cluster, and the record is what watches the removal finish
(§8.3).

**The node lock and the cluster lock are independent, and neither implies the
other.** A `StorageClusterOps` rolling restart walks every node of the cluster and
holds `StorageCluster.status.activeOpsRef` for the whole walk
([`design-storagecluster.md`](design-storagecluster.md) §8), while a
`StorageNodeOps` holds one node's. Nothing stops a node operation from starting
during a cluster-wide walk, and the cluster gate of §7.1 is what actually prevents
it: a walk puts the cluster into a state the gate holds on. Making the cluster lock
block node operations directly is §16, Q5.

---

## 12. Backend API Requirements

| Method   | Endpoint                                                             | Notes                                                                                            |
|----------|----------------------------------------------------------------------|--------------------------------------------------------------------------------------------------|
| `POST`   | `/api/v2/clusters/{cluster}/storage-nodes`                           | Adds every socket of one worker at once, and is not idempotent, which is why §4.2 claims first   |
| `GET`    | `/api/v2/clusters/{cluster}/storage-nodes/?watch=true`               | The node stream every status write and completion check reads (§4.4). Scoped per cluster         |
| `DELETE` | `/api/v2/clusters/{cluster}/storage-nodes/{node}?force_remove=false` | The drain's last step. 404 is success, since a node already gone is a node removed               |
| `POST`   | `/api/v2/clusters/{cluster}/storage-nodes/{node}/suspend`            | Used by `Suspend`. Must tolerate a repeat, because a step recorded without its call re-issues it |
| `POST`   | `/api/v2/clusters/{cluster}/storage-nodes/{node}/resume`             | Used by `Resume`                                                                                 |
| `POST`   | `/api/v2/clusters/{cluster}/storage-nodes/{node}/prepare-removal`    | The removal's first step on a node already down: admits it and rebuilds its devices (§8.2)       |
| `GET`    | `/api/v2/clusters/{cluster}/storage-nodes/{node}/prepare-removal`    | The device rebuild's progress, read by `MigratingDevices`                                        |
| `POST`   | `/api/v2/clusters/{cluster}/storage-nodes/{node}/verify-drained`     | Whether the node still holds a volume or a snapshot, which closes `Verifying` (§8.2)             |
| `POST`   | `/api/v2/clusters/{cluster}/storage-nodes/{node}/shutdown`           | Used by `Shutdown`, `HostMaintenance`, and `Remove`                                              |
| `POST`   | `/api/v2/clusters/{cluster}/storage-nodes/{node}/restart`            | Takes `node_address`, `force`, `reattach_volume`, and `new_ssd_pcie`. Used by three actions      |
| `POST`   | `/api/v2/clusters/{cluster}/storage-nodes/{node}/promote`            | The migration's last control-plane call, and the one that cannot be undone (§9)                  |
| `GET`    | `/api/v2/clusters/{cluster}/storage-pools/{pool}/volumes/`           | Lists a node's volumes for the drain's classification and verification (§8)                      |
| `DELETE` | `/api/v2/clusters/{cluster}/storage-pools/{pool}/volumes/{vol}/`     | Deletes system volumes during verification. 404 is success                                       |

**The `?watch=true` row is a Server-Sent-Events subscription rather than a request
that returns**, and it arrives with the control plane's SSE work rather than with
this design ([`design-crd-model.md`](design-crd-model.md) §7.7). Until that lands it
is the one external dependency this design cannot satisfy on its own.

**`status.resources.capacity` comes from a second endpoint, and not from this
table.** Neither the node list nor the node stream carries how full a node is. The
numbers exist only in the metrics the control plane exports, where
`snode_size_total`, `snode_size_used`, and `snode_date` carry them per node under
an `snode` label, and the operator reads them through Prometheus with
`atlas-lib/prometheus`. A deployment with no reachable Prometheus publishes the
node with its capacity absent rather than zero, because a node whose occupancy is
momentarily unknown is worth publishing and its identity and health are not in
doubt (§3.3).

**One capability the control plane does not provide.** There is no way to tell
whether a restart has begun other than observing that the node has left `online`,
which is the negative predicate §9 has to rely on. A restart generation, or any
monotonic counter the node reports, would let `Relocating` complete on a positive
observation and would make the step correct against a coalescing stream rather than
tolerable against one.

---

## 13. Observability

The controllers emit fourteen event reasons between them and export no metric at
all. The events table below is therefore a consolidation of a surface that
exists, and the metrics table is new infrastructure: nothing in the operator
measures how long a drain takes, how long a node is locked, or how often a
maintenance window holds for a peer.

### 13.1 Kubernetes events

Events need a target object, and this pairing has two candidates that are both
right for different things. An event about the node's own lifecycle goes on the
`StorageNode`, which is what an administrator looking at a worker has open. An
event about an operation goes on the `StorageNodeOps`, which outlives the
operation as its audit record. An operation's events are mirrored onto its target
node as well, because the node is where someone investigating a stuck cluster
starts and the operation's name is not something they know yet.

| Event                                                           | Type      | Reason                 | On               |
|-----------------------------------------------------------------|-----------|------------------------|------------------|
| Provisioning is held because the cluster has no UUID yet        | `Normal`  | `ClusterNotReady`      | `StorageNode`    |
| Provisioning is held because no fault group is declared         | `Warning` | `FailureDomainMissing` | `StorageNode`    |
| Provisioning is held because the worker's API does not answer   | `Warning` | `HostUnreachable`      | `StorageNode`    |
| Provisioning is held because no node-add slot is free           | `Normal`  | `AwaitingSlot`         | `StorageNode`    |
| The worker is not Ready or is cordoned, so the step is held     | `Warning` | `WorkerAway`           | `StorageNode`    |
| The worker came back and provisioning starts again              | `Normal`  | `WorkerReturned`       | `StorageNode`    |
| The node was adopted rather than added                          | `Normal`  | `NodeAdopted`          | `StorageNode`    |
| The node came online                                            | `Normal`  | `NodeOnline`           | `StorageNode`    |
| The node's pod reported a scheduling failure                    | `Warning` | `PodSchedulingFailed`  | `StorageNode`    |
| The operation is waiting for another to release the node's lock | `Normal`  | `OperationQueued`      | `StorageNodeOps` |
| The operation is holding because the cluster is not ready       | `Warning` | `ClusterNotReady`      | `StorageNodeOps` |
| The operation acquired the lock and started                     | `Normal`  | `OperationStarted`     | `StorageNodeOps` |
| The operation finished successfully                             | `Normal`  | `OperationSucceeded`   | `StorageNodeOps` |
| The operation failed                                            | `Warning` | `OperationFailed`      | `StorageNodeOps` |
| The operation was canceled                                      | `Normal`  | `OperationAborted`     | `StorageNodeOps` |
| `spec.abort` arrived at a step that cannot be stopped           | `Warning` | `AbortRefused`         | `StorageNodeOps` |
| A step's deadline expired                                       | `Warning` | `StepDeadlineExceeded` | `StorageNodeOps` |
| A drain is blocked by pinned volumes                            | `Warning` | `DrainBlocked`         | `StorageNodeOps` |
| A drain is blocked by unmanaged volumes                         | `Warning` | `DrainBlocked`         | `StorageNodeOps` |
| A drain has no online peer to migrate to                        | `Warning` | `NoMigrationTarget`    | `StorageNodeOps` |
| A volume migration failed and is being retried                  | `Warning` | `MigrationRetried`     | `StorageNodeOps` |
| A removal call or its shutdown was refused for now              | `Warning` | `RemovalDeferred`      | `StorageNodeOps` |
| Every volume has been migrated off the node                     | `Normal`  | `DrainCompleted`       | `StorageNodeOps` |
| The maintenance window is holding for another worker            | `Normal`  | `MaintenanceQueued`    | `StorageNodeOps` |

**`OperationSucceeded` is one reason for all seven actions rather than one each.**
The action is already in `spec.action` and on a print column, so encoding it in
the reason name tells a reader nothing and gives anyone alerting on completion
seven reasons to match instead of one. That replaces `MigrateCompleted`,
`NodeRemoved`, and the other per-action success reasons the controllers emit now.

**`DrainBlocked` is one reason for two conditions**, with the volume names and the
resolution in the message. A pinned volume and an unmanaged one are the same
situation from an alerting perspective, which is a drain that will not proceed
until somebody acts, and the difference is what the message says to do.

**The blocked and holding reasons are the load-bearing ones.** A drain waiting on
a pinned claim, a maintenance window waiting for its turn, and a provisioning node
waiting for a slot are all correct behavior that looks exactly like a stalled
controller, and the event is the only thing that distinguishes them.

### 13.2 Prometheus metrics

| Metric                                                           | Labels                        | Description                                                                                      |
|------------------------------------------------------------------|-------------------------------|--------------------------------------------------------------------------------------------------|
| `simplyblock_storagenode_operation_duration_seconds`             | `cluster`, `action`, `result` | Histogram of operation durations from lock acquisition to a terminal phase                       |
| `simplyblock_storagenode_operations_total`                       | `cluster`, `action`, `result` | Operations reaching a terminal phase, by `succeeded`, `failed`, and `aborted`                    |
| `simplyblock_storagenode_operation_step_duration_seconds`        | `cluster`, `action`, `step`   | Histogram of per-step durations, which is where a slow operation is actually slow                |
| `simplyblock_storagenode_operation_step_deadline_exceeded_total` | `cluster`, `action`, `step`   | Steps that ran out of time, including those that expired while the operator was down             |
| `simplyblock_storagenode_operation_lock_wait_seconds`            | `cluster`, `action`           | Histogram of time spent `Pending` behind another operation's lock                                |
| `simplyblock_storagenode_operation_active_state`                 | `cluster`, `node`             | Gauge, 1 while a node's `activeOpsRef` is set, so a lock held by a finished operation is visible |
| `simplyblock_storagenode_drain_blocked_volumes_count`            | `cluster`, `reason`           | Gauge of volumes blocking a drain, by `pinned` and `unmanaged`                                   |
| `simplyblock_storagenode_drain_volumes_migrated_total`           | `cluster`                     | Volumes moved off a node by a drain, so a drain's rate is graphable against its total            |
| `simplyblock_storagenode_maintenance_hold_seconds`               | `cluster`                     | Histogram of time a maintenance window held for its concurrency slot                             |
| `simplyblock_storagenode_phase_state`                            | `cluster`, `node`, `phase`    | Gauge, 1 for the node's current phase (§4.2), so a node stuck in `Provisioning` is alertable     |
| `simplyblock_storagenode_provisioning_duration_seconds`          | `cluster`                     | Histogram of time from object creation to `Ready`, which is what a node add costs                |

Every metric carries `cluster`, matching the convention the rebalancer's metrics
already follow, so one dashboard covers a multi-cluster deployment.

**Three of these answer questions the design otherwise cannot.**
`simplyblock_storagenode_operation_active_state` is the alert for a leaked lock: the release paths in
§11 are idempotent and run on three separate paths precisely because a lock held
by a terminal operation blocks a node forever, and a gauge is how that gets
noticed rather than reported. `drain_blocked_volumes` turns the most common
support question about a stalled drain into a dashboard panel.
`step_deadline_exceeded_total` distinguishes an operation still working from one
that stopped, which is the distinction `status.message` cannot express.

`simplyblock_storagenode_provisioning_duration_seconds` is the one to watch when a cluster
is being expanded, because `nodeProvisioningBudget` and the FoundationDB
serialization of §4.2 mean the time to add ten workers is not ten times the time to
add one, and nothing today says what it actually is.

---

## 14. Testing Strategy

Scenarios live in
[`tests/test-plan-storagenode.md`](../../tests/test-plan-storagenode.md) and only
there.

Unit tests with a fake client and a mock backend carry most of the weight, and
that is where the concurrency properties are provable: the `Posting` claim's 409
path, the sibling-socket check, the parallel-add and FoundationDB serialization
predicates, the operation lock's acquire and release paths, and the volume
classification of §8.1 are all pure control flow over a fake client and mock HTTP.

The step machines move risk rather than adding it. A transition table is data, so
every illegal transition is a cheap unit test, and the refusal to abort a
`Promoting` migration is one assertion rather than a code path.

The risk that unit tests do not reach concentrates in three places. The first is
the write-ahead discipline, whose entire purpose is to survive a process dying
between a patch and an HTTP call, which needs the operator actually killed at that
point. The second is the migration's DNS precondition and its negative completion
predicate (§9), which only misbehave against a real EndpointSlice and a real
asynchronous restart. The third is host maintenance (§10), which needs a worker
that is genuinely cordoned, drained, and rebooted, because the interaction being
tested is between the operator, the eviction, and the kubelet coming back.

---

## 15. Migration from the Registered API

Both kinds are registered and in use in a shape that predates the conventions this
design follows, and a third kind is retired by it. This section is the whole of the
delta, so that no other section has to carry it.

### 15.1 StorageNode

| Registered                                              | This design                                             | Cost                                                                                                                                                                                                                   |
|---------------------------------------------------------|---------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `spec.storageNodeSetRef`, required                      | `spec.clusterRef` plus `spec.nodeSet` (§3.1)            | Spec rename and a reparent. Blocked on §15.3                                                                                                                                                                           |
| `spec.overrides`, `StorageNodeOverrides`                | `spec.config`, `StorageNodeConfig` (§3.1)               | Spec rename. The struct stops being an override of anything                                                                                                                                                            |
| `spec.socketIndex`                                      | `spec.slot` (§3.1)                                      | Spec rename. The operator is its only writer, so no user-authored object sets it                                                                                                                                       |
| `spec.overrides` rewritten from the set every reconcile | Copied once at creation (§3.1)                          | Behavioral. The node stops being a cache of a document that can be deleted                                                                                                                                             |
| Cluster-scoped sizing, no per-node copy                 | `spec.config.sizing`, the two host-shaped values (§3.1) | Additive on the node, and what makes a rolling hardware upgrade expressible. `maxSubsystemCount` stays the cluster's, because nothing about a host decides it                                                          |
| Everything under `spec.overrides` mutable               | Most of `spec.config` immutable (§3.2)                  | Tightening. A user editing a device filter on a running node is now rejected                                                                                                                                           |
| `deviceNames`, NVMe namespace names                     | A PCI address or a device path (§3.1)                   | Widening. Every value the registered field took is still taken, and a logical block device becomes expressible                                                                                                         |
| `failureDomain`, an integer index                       | A label such as `rack-b` (§3.1)                         | Spec type change on both the spec and the status field. A stored index is not a valid label, so every node that declares a domain is rewritten, and the value stops being a number whose meaning lived outside the API |
| Four dead per-node fields in that struct                | Moved to the cluster (§5.1)                             | Spec removal. None of them reached a consumer, so nothing loses behavior. `skipKubeletConfiguration` is one of them, and the row below is what its successor is called                                                 |
| `skipKubeletConfiguration`                              | Removed here, re-landed on the cluster (§5.1)           | Spec removal. The successor is `StorageCluster.spec.storageNodes.enableKubeletConfiguration`, which is a different kind, so the registered field stashes on the way up rather than converting                          |
| No `status.phase`                                       | `StorageNodePhase` (§4.2)                               | Additive                                                                                                                                                                                                               |
| No step field, provisioning improvising one             | `status.step` (§4.2)                                    | Status only. The optimistic-lock claim moves to the `Posting` transition                                                                                                                                               |
| `status.postedAt` as the duplicate-POST guard           | Removed (§3.3)                                          | Status removal. The persisted step is the record                                                                                                                                                                       |
| `status.resources.devices`, a string                    | Two counts (§3.3)                                       | Status only, and it corrects a rendering that reports `total/online` against a documented `online/total`                                                                                                               |
| No `observedGeneration`                                 | Present (§3.3)                                          | Additive                                                                                                                                                                                                               |
| No `clusterRef` validation                              | `StorageNodeValidator` resolves it (§3.4)               | New. An immutable reference to a cluster that does not exist is refused rather than held forever                                                                                                                       |
| Nothing checks a hand-written node's sizing             | The same webhook compares it to the cluster's (§3.4)    | New. A manually configured node that disagrees with the fleet is refused instead of breaking chunk placement                                                                                                           |
| Owned by `StorageNodeSet`                               | Owned by `StorageCluster` (§3.1)                        | An owner reference moves, which changes what a cluster delete cascades to                                                                                                                                              |
| Polling every backend read                              | The storage-node stream (§4.4)                          | Depends on `design-sse-push-notifications.md`, on the `sse` branch                                                                                                                                                     |
| Two event reasons                                       | The reasons in §13.1                                    | Additive                                                                                                                                                                                                               |
| No metric                                               | The metrics in §13.2                                    | New infrastructure                                                                                                                                                                                                     |

### 15.2 StorageNodeOps

| Registered                                            | This design                                              | Cost                                                                                                                                                                                                                                            |
|-------------------------------------------------------|----------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `spec.storageNodeRef`                                 | `spec.nodeRef` (§6.1)                                    | Spec rename                                                                                                                                                                                                                                     |
| `spec.action` as a plain `string`                     | `StorageNodeOpsAction` (§6.1)                            | Type only, the wire values change with the row below                                                                                                                                                                                            |
| Six lowercase action values                           | PascalCase (§6.3)                                        | Spec rename of every value. `design-crd-model.md` §9.7 owns the deprecation window                                                                                                                                                              |
| `spec.targetWorkerNode`, `spec.newSsdPcie` at the top | `spec.migrate` (§6.1)                                    | Spec regrouping                                                                                                                                                                                                                                 |
| `spec.drain`                                          | `spec.remove` (§6.1)                                     | Spec rename, matching the action it parameterizes                                                                                                                                                                                               |
| Six actions                                           | Seven, adding `HostMaintenance` (§10)                    | Additive, and it retires a controller (§15.3)                                                                                                                                                                                                   |
| No abort                                              | `spec.abort` and the `Aborted` phase (§6.2)              | Additive. Cancellation today means deleting the object                                                                                                                                                                                          |
| `status.subPhase`, a union of two workflows           | `status.step`, one graph per action (§6.3)               | Status only. The old string reads into `step.state` with no deadline                                                                                                                                                                            |
| `Migrating` meaning two different things              | `MigratingVolumes` and `Relocating` (§6.3)               | Status only, and it removes an enum value that is ambiguous by construction                                                                                                                                                                     |
| `status.triggered`                                    | Removed (§7.2)                                           | Status removal. The persisted step is the record, and it covers a case the flag cannot                                                                                                                                                          |
| `status.volumesMigrated`, `status.volumesPending`     | `status.drain` (§6.2)                                    | Status regrouping. `volumesTotal` replaces a pending count that has to be kept in step                                                                                                                                                          |
| No `observedGeneration`                               | Present (§6.2)                                           | Additive                                                                                                                                                                                                                                        |
| No state machine behind any action                    | Seven declared graphs (§6.3)                             | The largest piece of work here. Every side effect moves into a step                                                                                                                                                                             |
| No deadline on any step                               | `status.step.deadline` (§6.3)                            | Additive, and what makes a stalled operation detectable                                                                                                                                                                                         |
| A cluster gate only for `Remove`                      | At admission, for `Migrate` and `HostMaintenance` (§7.1) | Behavioral. A relocation during a rebalance is currently accepted. `Remove` keeps its exemption because it is how an unready cluster becomes ready, and the four single-step actions share it because each is how the cluster's reading changes |

### 15.3 Retiring StorageNodeSet

The kind goes away and three things it carried have to land somewhere first.

| What it carried                               | Where it goes                                             | Blocked on                                |
|-----------------------------------------------|-----------------------------------------------------------|-------------------------------------------|
| The fleet template and `spec.nodeConfigs`     | `ClusterDeploymentConfig.nodeSets[]`                      | That kind's own design                    |
| The Kubernetes workload and its ownership     | `StorageCluster`, configured by `spec.storageNodes` (§5)  | `design-storagecluster.md` absorbing §5.1 |
| `status.drainCoordination` and its controller | `StorageNodeOps` with `action: HostMaintenance` (§10)     | Nothing. It is specified here             |
| `status.nodes[]`, `status.pendingNodeAdds`    | Nothing. The `StorageNode` objects were always the record | Nothing                                   |
| `status.latencyMetrics`                       | `StorageNode.status.latencyMetrics`, which already exists | Nothing                                   |

**§5.1 is an addition to a kind this document does not own.**
[`design-storagecluster.md`](design-storagecluster.md) §3.1 gains a
`spec.storageNodes` group, and its §4 gains the workload objects as children of the
cluster. That re-sync is a consequence of the retirement rather than a separate
decision, and the retirement cannot land before it.

**One capability is deliberately dropped.** Several `StorageNodeSet` objects per
cluster, each with its own DaemonSet selected by a per-set node label, becomes one
workload per cluster (§5.1). Nothing this repository deploys uses the second set, and
what it would have been for, a cluster that grew or one spanning two hardware
generations, is covered by adding nodes and by the per-node image, memory, and sizing
fields §5.1 names.

Every spec row above is breaking, because a renamed spec field is silently ignored
on an object that still sets the old name. Every status row is not, because the
operator is the only writer. The four per-node fields that move to the cluster are
the rows to read twice. Their successor is on a different kind, so no conversion
reaches them, and each stashes under
`storage.simplyblock.io/v1alpha1-spec.overrides.<field>` on the way up so that a
node converted back down carries what it carried
([`design-property-renames.md`](design-property-renames.md) §3.3).

The rows above are audited by
`.claude/skills/api-design/scripts/check-crds.py --kind StorageNode` and
`--kind StorageNodeOps` where a checker covers them. The step machine, the
deadline, the `Aborted` phase, and the per-action parameter blocks are conventions
of [`design-crd-model.md`](design-crd-model.md) §3.1 that no checker covers.

---

## 16. Open Questions

**Q1: How system volumes are identified.** The default
`^sb-fio-baseline-.*` matches the rebalancer's benchmark volumes by name, which is
a naming convention rather than a guarantee, and a volume a user happens to name
that way is silently deleted during a drain's verification (§8.2). A label applied
at creation time would be authoritative, at the cost of a control-plane field the
volume API does not have.

**Q2: What a blocked step's deadline should do.** `Validating` blocked on a pinned
claim is correct behavior waiting on a human, and expiring it fails an operation
that has done nothing wrong (§8.2). The alternatives are a long deadline that
reports rather than fails, no deadline on steps that perform no side effect, or a
separate `Blocked` phase outside the machine. This document takes the first and
does not settle it. `MigratingVolumes` raises the same question from the other
direction: a node holding a hundred large volumes drains for hours, so its
deadline has to be generous enough not to fail a working drain and tight enough to
catch a stalled one, and no number is known to satisfy both.

**Q4: A positive signal that a restart has begun.** §9's `Relocating` completes on
the node having left `online`, which a coalescing stream can fail to deliver. A
restart generation or any monotonic counter on the node object would make the
observation positive. This is a control-plane request, and §12 records it.

**Q5: Whether the cluster lock should block node operations.** A rolling restart
holds the cluster's lock while walking every node
([`design-storagecluster.md`](design-storagecluster.md) §8), and nothing prevents
a `StorageNodeOps` from acquiring a node's lock during the walk. The cluster gate
of §7.1 blocks a relocation or a maintenance window in practice, because a walk
puts the cluster into a state the gate holds on, which is a consequence rather
than a rule, and it blocks none of the five exempt actions. Making the cluster
lock explicit would mean a node operation checking two locks, and a rolling
restart having to acquire each node's lock as it reaches it.

**Q6: Retention of completed operations.** Nothing deletes a terminal
`StorageNodeOps`, so the audit record grows without bound, and a cluster that
cordons its workers monthly accumulates one per node per month.

**Q7: Whether a drain should check fault-tolerance headroom.** A drain reduces the
cluster's redundancy for as long as it runs, and the cluster gate of §7.1 checks
the cluster's status and its rebalancing flag rather than how much headroom is
left. A three-node cluster with one node already offline is at its limit, and a
`Remove` on a second node is currently accepted. Whether the check belongs on this
operation, on the cluster gate, or on the control plane that would have to expose
the headroom is undecided, and it is the question `design-node-removal-draining.md`
carried before this document absorbed it.

**Q8: What drives a per-node re-size.** `spec.config.sizing` is writable by the
operator and by nobody else (§3.1), and nothing here says what asks the operator to
write it. A rolling hardware upgrade is the case it exists for, so the candidates
are a `StorageClusterOps` action that walks the fleet rewriting each node's sizing
as it restarts it, which puts the roll where the other fleet-wide walks live, and a
`StorageNodeOps` action that re-sizes one node, which is the narrower blast radius
and needs something else to sequence it. Until one is specified the field is set at
creation and never rewritten, which is the behavior the cluster-scoped model
already has.

**Q9: Whether an unmanaged volume should ever be forced.** §8.1 blocks a drain on
a volume no `PersistentVolume` accounts for, which is the safe answer and leaves
the operator to resolve it by hand. A force parameter that deleted them would
unblock the case where the volumes are known debris, at the cost of a spec field
whose only effect is to destroy data nothing is tracking. Nothing decides which
side of that trade this kind should take.

---

## Appendix A: `storagenode_types.go`

The type as it is to be written. Everything the sections above show in Go is an
excerpt of this appendix, quoted where an argument turns on one field, and this
is the only place any type appears whole. The doc comments here are the ones the
shipped file carries, so the reasoning that belongs to the design stays in the
body and does not become a comment nobody can act on.

`.claude/skills/api-design/scripts/check-crds.py --design` audits this appendix
against the same conventions it audits the shipped types against.

```go
// StorageNodePhase is where the operator has got to with this node. The first two
// values are the operator's own provisioning path; the rest are its reading of the
// lifecycle status.status carries in the control plane's own spelling.
// +kubebuilder:validation:Enum=Pending;Provisioning;Online;Removing;Removed;Offline;Degraded;Failed
type StorageNodePhase string

const (
	// Pending: the object exists and no slot has been claimed for it yet.
	StorageNodePhasePending StorageNodePhase = "Pending"

	// Provisioning: the provisioning machine of §4.2 is running.
	StorageNodePhaseProvisioning StorageNodePhase = "Provisioning"

	// Online: the control plane reports the node online and carrying its share.
	StorageNodePhaseOnline StorageNodePhase = "Online"

	// Removing: the control plane is removing the node, from the removal being
	// accepted until its devices and volumes have moved off (§8).
	StorageNodePhaseRemoving StorageNodePhase = "Removing"

	// Removed: the control plane has removed the node. It is the last phase a
	// node reaches.
	StorageNodePhaseRemoved StorageNodePhase = "Removed"

	// Offline: out of service and reachable, which is where Shutdown, Suspend,
	// and a host maintenance window leave it (§10).
	StorageNodePhaseOffline StorageNodePhase = "Offline"

	// Degraded: serving with less than its devices, which is the node-level half
	// of what design-storagedevice.md reports per device.
	StorageNodePhaseDegraded StorageNodePhase = "Degraded"

	// Failed: unreachable, timed out, or provisioning that will not complete.
	StorageNodePhaseFailed StorageNodePhase = "Failed"
)

// StorageNodeStep is one step of the provisioning path. There is one graph
// rather than a MultiConfig, because an entity has no spec.action to key one on.
// +kubebuilder:validation:Enum=CheckingHost;CheckingConfig;AwaitingSlot;Posting;Resolving;Adopting;AwaitingWorker
type StorageNodeStep string

const (
	StorageNodeStepCheckingHost   StorageNodeStep = "CheckingHost"
	StorageNodeStepCheckingConfig StorageNodeStep = "CheckingConfig"
	StorageNodeStepAwaitingSlot   StorageNodeStep = "AwaitingSlot"
	StorageNodeStepPosting        StorageNodeStep = "Posting"
	StorageNodeStepResolving      StorageNodeStep = "Resolving"
	StorageNodeStepAdopting       StorageNodeStep = "Adopting"
)


// JournalManagerSpec tunes the journal managers on one storage node.
type JournalManagerSpec struct {
	// Count is the number of journal managers to configure.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Count *int32 `json:"count,omitempty"`

	// PercentPerDevice is the share of each device given to the journal.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	PercentPerDevice *int32 `json:"percentPerDevice,omitempty"`
}

// StorageNodeSizing is what this node's SPDK core layout and huge-page floor were
// sized from. It is stamped from StorageCluster.spec when the node is created and
// is equal across the fleet in steady state; a rolling hardware upgrade is what
// makes two nodes differ, and only for as long as the roll takes. The StorageNode
// validating webhook admits a change from the operator and rejects it from
// everyone else, because unmanaged divergence is what stops the control plane
// placing erasure-coding chunks evenly.
//
// The cluster's maxSubsystemCount is not copied in here. It bounds how many
// volumes a node can serve rather than describing the host the node runs on, so
// it is the same for every node of a cluster and is read from the cluster when
// the node's configuration is generated (§3.1).
type StorageNodeSizing struct {
	// VCPUCount is the number of vCPUs allocated to SPDK on this node, as an
	// explicit core count rather than a percentage. The floor matches the
	// cluster's: a node must carry one core beyond this budget for the system,
	// and the control plane's core layout assigns no NVMe-oF poller core at all
	// for a 2-vCPU budget.
	// +kubebuilder:validation:Minimum=4
	// +kubebuilder:validation:Required
	VCPUCount *int32 `json:"vcpuCount"`

	// MinHugePagesSize is the smallest huge-page allocation this node makes, as a
	// size string ("100G", "1T"; a bare number is gigabytes). It is a floor
	// rather than a limit: the effective allocation is the larger of this value
	// and the minimum the node's device and subsystem count requires.
	// +optional
	MinHugePagesSize string `json:"minHugePagesSize,omitempty"`
}

// StorageNodeConfig is a storage node's complete configuration, copied from the
// ClusterDeploymentConfig entry that produced the node. It is a copy rather than
// a projection, because that document is ephemeral and nothing reads it once the
// node exists.
//
// Most of it is immutable: by marker where a field has no legitimate writer, and
// by the validating webhook where it has exactly one.
type StorageNodeConfig struct {
	// Sizing is what this node's huge pages and core layout were sized from.
	// Writable by the operator alone.
	// +kubebuilder:validation:Required
	Sizing StorageNodeSizing `json:"sizing"`

	// SpdkImage overrides the SPDK image the control plane starts for this node,
	// which is what makes a phased image rollout expressible per node.
	// +optional
	SpdkImage string `json:"spdkImage,omitempty"`

	// SpdkImagePullPolicy controls when that image is pulled, and defaults to
	// Always because the images this product ships are moving tags.
	//
	// The control plane starts the SPDK pod, not the operator, and its
	// spdk_process_start takes no pull policy: the pod template it renders writes
	// Always itself. So a node states the policy here and the node-add call does
	// not yet carry it, which is a gap the control plane closes rather than this
	// kind. Stating anything but Always is therefore recorded and not yet obeyed.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=Always
	// +optional
	SpdkImagePullPolicy corev1.PullPolicy `json:"spdkImagePullPolicy,omitempty"`

	// SpdkProxyImage overrides the SPDK proxy image for this node.
	// +optional
	SpdkProxyImage string `json:"spdkProxyImage,omitempty"`

	// SpdkProxyImagePullPolicy controls when that image is pulled. It is stated
	// apart from SpdkImagePullPolicy because a node may pin the proxy and follow
	// the SPDK image, and it carries the same not-yet-spent caveat: the template
	// the control plane renders writes Always for both containers.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=Always
	// +optional
	SpdkProxyImagePullPolicy corev1.PullPolicy `json:"spdkProxyImagePullPolicy,omitempty"`

	// SpdkSystemMemory is the memory the control plane starts this node's SPDK
	// with ("4G", "512M"). It carries no immutability marker, because a node
	// whose device count grew legitimately needs it raised, and the webhook that
	// guards spec.config is what decides who may raise it.
	// +kubebuilder:validation:Pattern=`^[0-9]+(G|GI|GB|GiB|M|MI|MB|MiB|g|gi|gb|gib|m|mi|mb|mib)?$`
	// +optional
	SpdkSystemMemory string `json:"spdkSystemMemory,omitempty"`

	// ReservedSystemCPU is the CPU set held back from SPDK for the system, as a
	// core list such as 0,1 or 0-3.
	//
	// It is per node and not per cluster because it names core ids: 0,1 on a
	// sixteen-core worker and 0,1 on a ninety-six-core worker are different
	// fractions of the machine, and a fleet whose groups differ in core count
	// has no one list that is right for all of them.
	// StorageCluster.spec.storageNodes.reservedSystemCPU is the fleet's value,
	// which the pod carries as an environment variable, and this overrides it
	// for the node that states it.
	//
	// On OpenShift the value reaches the kubelet through a KubeletConfig for
	// the machine config pool rather than through the node alone, so nodes
	// sharing a pool that disagree are writing over one another's pool
	// configuration. It carries no immutability marker, because the CPUs a
	// machine holds back are a tuning decision rather than a layout one, and
	// the webhook that guards spec.config is what decides who may retune
	// them.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`
	// +optional
	ReservedSystemCPU string `json:"reservedSystemCPU,omitempty"`

	// JournalManager tunes the journal manager count and per-device capacity
	// share for this node. Immutable: both are on-disk layout, fixed when the
	// devices were partitioned.
	// +optional
	// +k8s:immutable
	JournalManager *JournalManagerSpec `json:"journalManager,omitempty"`

	// DeviceNames names the devices to use. An entry is a PCI address
	// ("0000:5e:00.0") or a device path ("/dev/sdb"), which are the two classes
	// simplyblock accepts as backend storage, and a bare name ("nvme0n1") is read
	// as a path under /dev. One list carries both spellings, and every entry is of
	// the class its cluster declares in StorageCluster.spec.deviceClass: a list
	// mixing the two, or naming the class the cluster is not, is rejected by the
	// StorageNode validating webhook (§3.4). Set explicitly, it overrides every
	// filter below. Immutable: it selects which physical devices the node owns.
	// +kubebuilder:validation:items:Pattern=`^([0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]|/dev/[a-zA-Z0-9._/-]+|[a-zA-Z0-9._-]+)$`
	// +optional
	// +k8s:immutable
	DeviceNames []string `json:"deviceNames,omitempty"`

	// PcieAllowList selects devices by PCI address. It is the one device field a
	// migration writes, merging spec.migrate.newSsdPcie into it so devices added
	// on the target host survive a later rebuild, so it is guarded by the
	// StorageNode validating webhook rather than by a marker. This and the two
	// PCI filters below belong to an NVMe cluster: the webhook rejects them on a
	// cluster whose deviceClass is LogicalBlock, because a logical block device
	// has no PCI address to match (§3.4).
	// +optional
	PcieAllowList []string `json:"pcieAllowList,omitempty"`

	// PcieDenyList excludes devices by PCI address.
	// +optional
	// +k8s:immutable
	PcieDenyList []string `json:"pcieDenyList,omitempty"`

	// PcieModel filters devices by PCI model string.
	// +optional
	// +k8s:immutable
	PcieModel string `json:"pcieModel,omitempty"`

	// DriveSizeRange filters devices by size.
	// +optional
	// +k8s:immutable
	DriveSizeRange string `json:"driveSizeRange,omitempty"`

	// FailureDomain is the label of the fault group this node belongs to
	// ("rack-b"), naming the physical grouping it shares with its peers rather
	// than indexing it. Required when the cluster has enableFailureDomains set,
	// and provisioning is held with a FailureDomainMissing event until it is
	// present. Immutable once set, which is what makes it fillable later and then
	// frozen: chunk placement was computed from it.
	//
	// The value takes the shape of a Kubernetes label value, because that is what
	// it is seeded from where a cluster carries topology labels at all.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]([-_.a-zA-Z0-9]*[a-zA-Z0-9])?$`
	// +optional
	// +k8s:immutable
	FailureDomain string `json:"failureDomain,omitempty"`

	// Expand marks this node as an addition to an already-active cluster, which
	// the control plane reads as a request to rebalance onto it rather than to
	// treat it as part of an initial layout. Immutable once set: it describes how
	// the node joined rather than what it is.
	// +optional
	// +k8s:immutable
	Expand *bool `json:"expand,omitempty"`
}

// StorageNodeSpec is the desired state of one backend storage node, meaning one
// SPDK process bound to one NUMA socket of one Kubernetes worker.
type StorageNodeSpec struct {
	// ClusterRef names the StorageCluster this node belongs to. The cluster also
	// owns this object by controller reference, so deleting the cluster deletes
	// its nodes.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// NodeSet is the name of the group in ClusterDeploymentConfig.nodeSets[] this
	// node was declared under. It is a label rather than a reference: nothing is
	// fetched by it, and it exists so that a node can be traced back to the
	// document that produced it.
	// +optional
	// +k8s:immutable
	NodeSet string `json:"nodeSet,omitempty"`

	// WorkerNode is the Kubernetes worker hostname this node runs on. It is not
	// marked immutable, because a migration re-points it, but the StorageNode
	// validating webhook rejects any change made by an identity outside the
	// operator's namespace.
	// +kubebuilder:validation:Required
	WorkerNode string `json:"workerNode"`

	// SocketID is the NUMA socket this node is bound to, as declared in the node
	// set's socket list ("0", "1"). With NodeIndex it decomposes Slot into the
	// pair a person reads; nothing but a print column consumes either.
	//
	// It is not marked immutable, because where a node sits is a fact about the
	// host it runs on and a relocation moves it: the target worker's free socket
	// is not necessarily the source's. The StorageNode validating webhook
	// rejects a change made by an identity outside the operator's namespace,
	// which is the same treatment WorkerNode takes and for the same reason.
	// +optional
	SocketID string `json:"socketId,omitempty"`

	// NodeIndex is the position among the nodes sharing this socket, in
	// 0..nodesPerSocket-1. See SocketID, whose guard it shares.
	// +kubebuilder:validation:Minimum=0
	// +optional
	NodeIndex *int32 `json:"nodeIndex,omitempty"`

	// Slot is which storage-node slot on this worker the object occupies,
	// counted from zero. A worker runs one node per socket per nodesPerSocket,
	// and the slot is the position among them. It is the identity the operator
	// keys on: the topology label the CSI driver reads is
	// storage.simplyblock.io/storage-node-uuid.<clusterUUID>.<slot>, and the slot
	// outlives the node filling it, because only the UUID behind it changes when
	// a node is replaced or relocated.
	// +kubebuilder:validation:Minimum=0
	// +optional
	// +k8s:immutable
	Slot *int32 `json:"slot,omitempty"`

	// Config is this node's complete configuration, copied from the
	// ClusterDeploymentConfig entry that produced it. It is a copy rather than a
	// projection, because that document is ephemeral: nothing reads it once the
	// node exists, deleting it changes nothing, and editing it reaches only nodes
	// created afterward.
	// +kubebuilder:validation:Required
	Config StorageNodeConfig `json:"config"`
}

// StorageNodeDevices counts the NVMe devices on a node and how many of them are
// online. It is a summary rather than an inventory: per-device capacity, health,
// and conditions belong to StorageDevice.
//
// Neither field takes omitempty. Zero online devices is the condition worth
// seeing, and a field that disappears at zero would report it as nothing at all.
// A node the control plane has not reported on is the absent parent instead.
type StorageNodeDevices struct {
	// Online is how many of the node's devices the control plane reports as
	// usable.
	// +kubebuilder:validation:Minimum=0
	Online int32 `json:"online"`

	// Total is how many devices the node has.
	// +kubebuilder:validation:Minimum=0
	Total int32 `json:"total"`
}

// StorageNodeResources groups the compute and storage figures the control plane
// reports for a node.
type StorageNodeResources struct {
	// CPU is the number of SPDK cores allocated to this node.
	// +optional
	CPU *int32 `json:"cpu,omitempty"`

	// Memory is the SPDK memory allocation the control plane reports.
	// +optional
	Memory string `json:"memory,omitempty"`

	// Volumes is the current number of logical volumes on this node.
	// +optional
	Volumes *int32 `json:"volumes,omitempty"`

	// Devices summarizes the node's NVMe devices. Absent until the control plane
	// has reported, which is what tells a node that has not reported from one
	// that genuinely has no devices.
	// +optional
	Devices *StorageNodeDevices `json:"devices,omitempty"`

	// Capacity is how much of the node's storage is in use, summed over its
	// devices. It is a measurement rather than a declaration, so it is absent
	// until something has measured it, and it lags reality by the interval at
	// which the control plane's metrics are scraped.
	// +optional
	Capacity *StorageNodeCapacity `json:"capacity,omitempty"`
}

// StorageNodeCapacity is a node's storage occupancy, as the control plane last
// measured it.
//
// It carries the same two numbers as a device's capacity, because a node's is the
// sum of its devices' and a reader comparing the two should not have to reconcile
// different shapes. It is written only when the reading has moved materially: a
// sample that changed by a few blocks is not worth an etcd write, and writing
// every sample would make the reconciler retrigger itself on its own status
// update.
type StorageNodeCapacity struct {
	// TotalBytes is the storage the node's devices provide.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`

	// UsedBytes is what they currently hold.
	// +kubebuilder:validation:Minimum=0
	// +optional
	UsedBytes *int64 `json:"usedBytes,omitempty"`

	// SampledAt is when the control plane took the reading. It is not when the
	// object was written, and it may be considerably older if metrics collection
	// has stopped.
	// +optional
	SampledAt *metav1.Time `json:"sampledAt,omitempty"`
}

// StorageNodePorts groups the addresses and ports a node listens on.
type StorageNodePorts struct {
	// Management is the management IP address of the node.
	// +optional
	Management string `json:"management,omitempty"`

	// NvmeOf is the NVMe-oF fabric port.
	// +optional
	NvmeOf *int32 `json:"nvmeof,omitempty"`

	// Lvol is the logical-volume subsystem port.
	// +optional
	Lvol *int32 `json:"lvol,omitempty"`

	// Rpc is the RPC and management API port.
	// +optional
	Rpc *int32 `json:"rpc,omitempty"`
}

// StorageNodeStatus is the observed state of one storage node.
type StorageNodeStatus struct {
	// Phase is the operator's own view of this node, and the field its
	// provisioning branches on.
	// +optional
	Phase StorageNodePhase `json:"phase,omitempty"`

	// Step is the position of the provisioning machine, as the shared
	// statemachine.KubeSnapshot (design-crd-model.md §3.1). The rule is what an
	// Enum marker would do if a marker could reach a field of a shared type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['CheckingHost','CheckingConfig','AwaitingSlot','Posting','Resolving','Adopting']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// UUID is the backend node UUID. Empty means the node has neither been
	// provisioned nor adopted, and non-empty means steady state.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// Status is the lifecycle the control plane reports: online, suspended,
	// offline, in_creation, in_restart, in_shutdown, unreachable, or timeout.
	// The values are the control plane's, which is why they are neither
	// PascalCase nor constrained by an Enum here.
	// +optional
	Status string `json:"status,omitempty"`

	// Health is the health flag the control plane reports.
	// +optional
	Health bool `json:"health,omitempty"`

	// Hostname is the node hostname as the control plane reports it.
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// Uptime is the node uptime as the control plane reports it.
	// +optional
	Uptime string `json:"uptime,omitempty"`

	// Resources groups the reported compute and storage figures.
	// +optional
	Resources *StorageNodeResources `json:"resources,omitempty"`

	// Ports groups the reported addresses and ports.
	// +optional
	Ports *StorageNodePorts `json:"ports,omitempty"`

	// FailureDomain is the failure-domain label the control plane actually
	// assigned, which is not necessarily the one spec.config.failureDomain
	// requested.
	// +optional
	FailureDomain string `json:"failureDomain,omitempty"`

	// ActiveOpsRef names the StorageNodeOps currently allowed to touch this node.
	// Empty when none is running.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// LatencyMetrics holds the fio-measured NVMe-oF baseline the volume
	// rebalancer reads.
	// +optional
	LatencyMetrics *NodeLatencyMetrics `json:"latencyMetrics,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the node moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sn
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Worker",type=string,JSONPath=".spec.workerNode"
// +kubebuilder:printcolumn:name="Socket",type=string,JSONPath=".spec.socketId"
// +kubebuilder:printcolumn:name="Slot",type=integer,JSONPath=".spec.slot"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// +kubebuilder:printcolumn:name="Health",type=boolean,JSONPath=".status.health"
// +kubebuilder:printcolumn:name="UUID",type=string,JSONPath=".status.uuid",priority=1
// +kubebuilder:printcolumn:name="FD",type=string,JSONPath=".status.failureDomain",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageNode is one backend storage node: one SPDK process bound to one NUMA
// socket of one Kubernetes worker. One object exists per (workerNode,
// slot) pair, owned by the StorageCluster it belongs to.
type StorageNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageNodeSpec   `json:"spec,omitempty"`
	Status StorageNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageNodeList contains a list of StorageNode.
type StorageNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageNode `json:"items"`
}
```

`NodeLatencyMetrics` is unchanged and is declared where the volume rebalancer
uses it, in [`design-auto-rebalancing.md`](../design-auto-rebalancing.md). It moves
out of `storagenodeset_types.go` with the retirement (§15.3) and is not restated
here.

---

## Appendix B: `storagenodeops_types.go`

```go
// StorageNodeOpsAction is the operation a StorageNodeOps performs. Values are
// PascalCase, which is the casing every enum this API group defines carries
// (design-crd-model.md §7.8).
// +kubebuilder:validation:Enum=Shutdown;Restart;Suspend;Resume;Remove;Migrate;HostMaintenance
type StorageNodeOpsAction string

const (
	StorageNodeOpsActionShutdown        StorageNodeOpsAction = "Shutdown"
	StorageNodeOpsActionRestart         StorageNodeOpsAction = "Restart"
	StorageNodeOpsActionSuspend         StorageNodeOpsAction = "Suspend"
	StorageNodeOpsActionResume          StorageNodeOpsAction = "Resume"
	StorageNodeOpsActionRemove          StorageNodeOpsAction = "Remove"
	StorageNodeOpsActionMigrate         StorageNodeOpsAction = "Migrate"
	StorageNodeOpsActionHostMaintenance StorageNodeOpsAction = "HostMaintenance"
)

// StorageNodeOpsPhase is the operation's own progress. Aborted is terminal and
// distinct from Failed, because a canceled operation did not go wrong.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StorageNodeOpsPhase string

const (
	StorageNodeOpsPhasePending   StorageNodeOpsPhase = "Pending"
	StorageNodeOpsPhaseRunning   StorageNodeOpsPhase = "Running"
	StorageNodeOpsPhaseSucceeded StorageNodeOpsPhase = "Succeeded"
	StorageNodeOpsPhaseFailed    StorageNodeOpsPhase = "Failed"
	StorageNodeOpsPhaseAborted   StorageNodeOpsPhase = "Aborted"
)

// StorageNodeOpsStep is one step of a running node operation. The enum is the
// union of every action's steps; which steps belong to which action is declared
// by the graph rather than by this type.
// +kubebuilder:validation:Enum=Requesting;Departing;Awaiting;Validating;MigratingDevices;MigratingVolumes;Verifying;Removing;AwaitingRemoval;Preparing;Relocating;AwaitingNode;Promoting;Holding;ShuttingDown;Releasing;AwaitingHost;Restarting;Cleanup
type StorageNodeOpsStep string

const (
	// Shutdown, Restart, Suspend, and Resume.
	StorageNodeOpsStepRequesting StorageNodeOpsStep = "Requesting"
	StorageNodeOpsStepAwaiting   StorageNodeOpsStep = "Awaiting"

	// Remove.
	StorageNodeOpsStepValidating       StorageNodeOpsStep = "Validating"
	StorageNodeOpsStepMigratingDevices StorageNodeOpsStep = "MigratingDevices"
	StorageNodeOpsStepMigratingVolumes StorageNodeOpsStep = "MigratingVolumes"
	StorageNodeOpsStepVerifying        StorageNodeOpsStep = "Verifying"
	StorageNodeOpsStepRemoving         StorageNodeOpsStep = "Removing"
	StorageNodeOpsStepAwaitingRemoval  StorageNodeOpsStep = "AwaitingRemoval"

	// Migrate.
	StorageNodeOpsStepPreparing    StorageNodeOpsStep = "Preparing"
	StorageNodeOpsStepRelocating   StorageNodeOpsStep = "Relocating"
	StorageNodeOpsStepAwaitingNode StorageNodeOpsStep = "AwaitingNode"
	StorageNodeOpsStepPromoting    StorageNodeOpsStep = "Promoting"

	// HostMaintenance.
	StorageNodeOpsStepHolding      StorageNodeOpsStep = "Holding"
	StorageNodeOpsStepShuttingDown StorageNodeOpsStep = "ShuttingDown"
	StorageNodeOpsStepReleasing    StorageNodeOpsStep = "Releasing"
	StorageNodeOpsStepAwaitingHost StorageNodeOpsStep = "AwaitingHost"
	StorageNodeOpsStepRestarting   StorageNodeOpsStep = "Restarting"
	StorageNodeOpsStepCleanup      StorageNodeOpsStep = "Cleanup"
)


// MigrateSpec parameterizes the Migrate action and is ignored by the others.
type MigrateSpec struct {
	// TargetWorkerNode is the Kubernetes worker the node is relocated onto.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	TargetWorkerNode string `json:"targetWorkerNode"`

	// NewSsdPcie lists additional NVMe PCI addresses to bind on the target host,
	// passed through to the control-plane restart as new_ssd_pcie and merged into
	// the node's effective allow list so they survive a later rebuild.
	// +optional
	NewSsdPcie []string `json:"newSsdPcie,omitempty"`
}

// RemoveSpec parameterizes the Remove action and is ignored by the others.
type RemoveSpec struct {
	// SystemVolumeFilterRegex matches backend volume names that are system
	// volumes: excluded from the drain's migration and deleted during
	// verification rather than blocking it.
	// +kubebuilder:default=`^sb-fio-baseline-.*`
	// +optional
	SystemVolumeFilterRegex *string `json:"systemVolumeFilterRegex,omitempty"`
}

// StorageNodeOpsSpec is one operation to perform against one StorageNode.
type StorageNodeOpsSpec struct {
	// NodeRef names the StorageNode this operation acts on. The operation never
	// owns its target, because deleting the record of an operation must not
	// delete the node it operated on.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	NodeRef string `json:"nodeRef"`

	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action StorageNodeOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind. It is
	// the only mutable field on this spec, because it is the only thing about an
	// operation that can legitimately be decided after it started. Whether an
	// abort is expressible from the current step is declared by that action's
	// graph rather than checked here.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Force passes the control plane's force flag where the action supports it.
	// Migrate defaults it to true, because the control plane rejects a non-forced
	// restart of a node that is not already offline.
	// +optional
	Force *bool `json:"force,omitempty"`

	// ReattachVolume asks the control plane to reattach this node's volumes as
	// part of a restart. Applies to Restart, Migrate, and HostMaintenance.
	// +optional
	ReattachVolume *bool `json:"reattachVolume,omitempty"`

	// Migrate parameterizes action Migrate and is ignored by the others.
	// +optional
	Migrate *MigrateSpec `json:"migrate,omitempty"`

	// Remove parameterizes action Remove and is ignored by the others.
	// +optional
	Remove *RemoveSpec `json:"remove,omitempty"`
}

// DrainStatus is the drain's progress over the volumes on the node being
// removed. Neither field takes omitempty: zero is meaningful for both, and a
// field that disappears at zero makes "nothing to move" and "not yet counted"
// the same wire value.
type DrainStatus struct {
	// VolumesTotal is the number of PV-managed volumes the drain has to move,
	// written once at the end of Validating and not modified afterward.
	// +kubebuilder:validation:Minimum=0
	VolumesTotal int32 `json:"volumesTotal"`

	// VolumesMigrated is how many of them have completed.
	// +kubebuilder:validation:Minimum=0
	VolumesMigrated int32 `json:"volumesMigrated"`
}

// RemovalStatus is the control plane's progress through a node removal it has
// accepted, as the operation last read it while waiting in AwaitingRemoval. Any
// change in it counts as progress and moves the step's deadline a whole budget
// out.
type RemovalStatus struct {
	// NodeStatus is the node's status as the control plane last reported it.
	// +optional
	NodeStatus string `json:"nodeStatus,omitempty"`

	// Devices is the control-plane status each of the node's StorageDevices
	// last reported, keyed by the StorageDevice's name.
	// +optional
	Devices map[string]string `json:"devices,omitempty"`

	// LastProgressTime is when NodeStatus or any of Devices last changed.
	// +optional
	LastProgressTime *metav1.Time `json:"lastProgressTime,omitempty"`

	// PrepareAttempts is how many times the removal's first step was sent
	// again for a node that stayed pending_removal.
	// +kubebuilder:validation:Minimum=0
	// +optional
	PrepareAttempts int32 `json:"prepareAttempts,omitempty"`

	// LastPrepareTime is when the removal's first step was last sent again.
	// +optional
	LastPrepareTime *metav1.Time `json:"lastPrepareTime,omitempty"`
}

// StorageNodeOpsStatus is the observed state of one node operation.
type StorageNodeOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase StorageNodeOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine, as the shared
	// statemachine.KubeSnapshot (design-crd-model.md §3.1). The rule is what an
	// Enum marker would do if a marker could reach a field of a shared type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Requesting','Departing','Awaiting','Validating','MigratingDevices','MigratingVolumes','Verifying','Removing','AwaitingRemoval','Preparing','Relocating','AwaitingNode','Promoting','Holding','ShuttingDown','Releasing','AwaitingHost','Restarting','Cleanup']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// Drain is the drain's progress over the node's volumes, set only for action
	// Remove.
	// +optional
	Drain *DrainStatus `json:"drain,omitempty"`

	// Removal is the control plane's progress through the removal, written by
	// a Remove while it waits in AwaitingRemoval.
	// +optional
	Removal *RemovalStatus `json:"removal,omitempty"`

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
// +kubebuilder:resource:scope=Namespaced,shortName=snops
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.nodeRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageNodeOps is a single operation performed against one StorageNode. It
// runs to a terminal phase and stays afterward as the audit record of what was
// done, to which node, with which parameters, and how it ended.
type StorageNodeOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageNodeOpsSpec   `json:"spec,omitempty"`
	Status StorageNodeOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageNodeOpsList contains a list of StorageNodeOps.
type StorageNodeOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageNodeOps `json:"items"`
}
```

---

## Appendix C: the `StorageCluster` addition

This block belongs to `storagecluster_types.go` rather than to either file above,
and reaches the API as `StorageClusterSpec.StorageNodes`. It is stated here
because §5 specifies it and
[`design-storagecluster.md`](design-storagecluster.md) §3.1 has to absorb it
before the `StorageNodeSet` retirement can land (§15.3).

```go
// OpenShiftSpec is what a deployment onto OpenShift states beyond what every
// distribution states.
//
// Its presence is the statement. A cluster carrying the block runs on OpenShift
// and one without it does not, which is why there is no boolean beside it: a
// block naming a machine-config pool on a cluster that also said it was not
// OpenShift was expressible before and meant nothing.
type OpenShiftSpec struct {
	// MachineConfigPool names a machine-config role the storage nodes' own pool
	// inherits from, beyond the worker role it always inherits.
	//
	// It is not the pool the nodes end up in, which the description it carried
	// before said and which cost a reader the reboot they were trying to avoid.
	// Adding a node creates a pool of its own, storage-<cluster>, and moves the
	// node into it; a node belongs to exactly one custom pool, so whatever
	// machine configuration its previous pool carried is lost unless that
	// pool's role is named here for the new one to select as well. The default
	// is the role every pool already selects, which is what makes it a no-op
	// for a fleet whose workers are ordinary workers.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:default=worker
	// +optional
	MachineConfigPool string `json:"machineConfigPool,omitempty"`
}

// StorageNodesSpec is the Kubernetes workload every storage node in the cluster
// runs as. Every field here is cluster-uniform by construction, because a
// DaemonSet is one object for every node it schedules and its pod template
// cannot differ per node. What can differ is in StorageNode.spec.config.
type StorageNodesSpec struct {
	// Image is the storage-node container image. Defaults to the ControlPlane
	// singleton's spec.image when unset, so a deployment states the version once.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullPolicy controls when that image is pulled. It defaults to Always
	// for the reason SimplyblockDriver's does: the images this product ships are
	// moving tags, so a node brought up after a release that kept IfNotPresent
	// would run whatever its kubelet already held. The workload builder has
	// always meant this and falls back to Always for an unset policy, a fallback
	// the schema default means it never reaches.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=Always
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// MgmtInterface is the management network interface storage nodes bind.
	// +optional
	// +k8s:immutable
	MgmtInterface string `json:"mgmtInterface,omitempty"`

	// DataInterfaces are the data-plane network interfaces, frozen once set for
	// the reason MgmtInterface is: both reach the control plane as the node-add's
	// interface_name and data_nics, which it spends on the node it is adding and
	// never revisits.
	// +optional
	// +k8s:immutable
	DataInterfaces []string `json:"dataInterfaces,omitempty"`

	// SocketsToUse restricts deployment to selected NUMA sockets. Empty means
	// socket 0 alone.
	// +optional
	SocketsToUse []string `json:"socketsToUse,omitempty"`

	// NodesPerSocket is how many storage nodes run per NUMA socket.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	// +k8s:immutable
	NodesPerSocket *int32 `json:"nodesPerSocket,omitempty"`

	// NodeProvisioningBudget limits how many workers may be in the node-add
	// process at once. Workers hosting a FoundationDB pod are always sequential
	// regardless of this value.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	NodeProvisioningBudget *int32 `json:"nodeProvisioningBudget,omitempty"`

	// EnableJournalDevice dedicates the smallest NVMe device on each node to the
	// journal manager, instead of carving a journal partition out of every
	// device.
	// +optional
	// +k8s:immutable
	EnableJournalDevice *bool `json:"enableJournalDevice,omitempty"`

	// EnableFormat4K formats NVMe devices to a 4K block size where the device
	// supports it.
	// +optional
	// +k8s:immutable
	EnableFormat4K *bool `json:"enableFormat4K,omitempty"`

	// EnableCpuTopology turns on topology-aware CPU assignment.
	// +optional
	EnableCpuTopology *bool `json:"enableCpuTopology,omitempty"`

	// ReservedSystemCPU is the CPU set held back from SPDK for system workloads.
	// +optional
	ReservedSystemCPU string `json:"reservedSystemCPU,omitempty"`

	// EnableKubeletConfiguration lets the storage node apply the kubelet
	// configuration changes it needs. Off by default, which is the behavior
	// skipKubeletConfiguration expressed by being set.
	// +optional
	EnableKubeletConfiguration *bool `json:"enableKubeletConfiguration,omitempty"`

	// UbuntuHost states that the worker's host OS is Ubuntu, which changes how
	// the node configures huge pages and the kernel modules it loads.
	// +optional
	UbuntuHost *bool `json:"ubuntuHost,omitempty"`

	// OpenShift is what this deployment states because it runs on OpenShift,
	// and its presence is that statement. See OpenShiftSpec.
	// +optional
	OpenShift *OpenShiftSpec `json:"openshift,omitempty"`

	// Tolerations are applied to the storage-node pods.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// ContainerResources sets requests and limits for the storage-node
	// container. Unset enforces no limits.
	// +optional
	ContainerResources corev1.ResourceRequirements `json:"containerResources,omitempty"`

	// InitContainerResources does the same for the init container.
	// +optional
	InitContainerResources corev1.ResourceRequirements `json:"initContainerResources,omitempty"`
}
```
