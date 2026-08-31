# Design Document: The StorageCluster and Its Operations

**Status:** Draft  
**Authors:** Christoph Engelbert (noctarius), Israel Geoffrey (`StorageClusterOps`)  
**Date:** 2026-08-28  
**Supersedes:** `design-storageclusterops.md`, removed in the same change  
**Test Plan:** [`tests/test-plan-storagecluster.md`](../../tests/test-plan-storagecluster.md)

This document specifies the target model. Both kinds and both controllers exist in
a shape that predates the conventions of
[`design-crd-model.md`](design-crd-model.md), and §12 is the single record of what
the rework changes against them.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [StorageCluster: API](#3-storagecluster-api)
4. [StorageCluster: Controller](#4-storagecluster-controller)
5. [StorageClusterOps: API](#5-storageclusterops-api)
6. [StorageClusterOps: Controller](#6-storageclusterops-controller)
7. [Rolling Restart](#7-rolling-restart)
8. [Mutual Exclusion](#8-mutual-exclusion)
9. [Backend API Requirements](#9-backend-api-requirements)
10. [Observability](#10-observability)
11. [Testing Strategy](#11-testing-strategy)
12. [Migration from the Registered API](#12-migration-from-the-registered-api)
13. [Open Questions](#13-open-questions)

Appendices:

- [Appendix A: `storagecluster_types.go`](#appendix-a-storagecluster_typesgo)
- [Appendix B: `storageclusterops_types.go`](#appendix-b-storageclusterops_typesgo)

---

## Overview

`StorageCluster` is one simplyblock backend cluster expressed as a Kubernetes
resource, and `StorageClusterOps` is a single operation performed against one of
them. They are specified together because they are one decision split across two
kinds: the entity declares a layout that mostly cannot change under a live
cluster, and the operation is how the things that can change are asked for. Which
half a given field belongs on is the question this document answers most often.

The pairing is the convention stated in
[`design-crd-model.md`](design-crd-model.md) §3. That document is the map of the
whole API group and the categories a kind belongs to. This one is the
specification of one level of its spine.

---

## 1. Background

A cluster-level operation touches every node beneath it. Activating a cluster,
expanding it, shutting it down, and rolling its nodes one at a time are all
multi-step, all destructive if repeated, and all long enough to outlive the process
that started them. None of that is expressible as desired state, which is why the
operation is a resource of its own and why most of this document is about how one
survives an operator restart without repeating a side effect.

The entity carries the other half of the risk. `StorageClusterSpec` has twenty
fields governing erasure coding, the fabric, port ranges, huge pages, core counts,
capacity thresholds, backup targets, key storage, and two separate migration
policies. Eight describe on-disk or on-wire layout that a live cluster cannot
tolerate changing, and the rest are mutable in the sense that the API server
accepts the edit, which is not the same as the cluster tolerating it.

---

## 2. Goals and Non-Goals

### Goals

- Specify `StorageCluster`'s spec and status, and state for every field whether a
  live cluster tolerates a change to it, by which mechanism, and with what
  semantics (§3).
- Specify the entity controller's four paths, meaning creation, adoption,
  steady-state synchronization, and deletion, including the optimistic-lock patch
  that prevents two reconcilers from creating two backend clusters (§4).
- Specify `StorageClusterOps`, its six actions, and the write-ahead discipline
  that makes each of them safe to retry (§5, §6).
- Specify the per-node rolling restart state machine, which is the only action
  that persists progress across an operator restart (§7).
- Specify the lock that keeps cluster-wide operations serialized, and the paths
  that release it (§8).
- Record where the two kinds do not meet the conventions of
  [`design-crd-model.md`](design-crd-model.md), as findings rather than as
  intentions (§12).

### Non-Goals

- **Not the storage node or the pool.** `StorageNode`, `StorageNodeOps`, and the
  Kubernetes workload the storage nodes run as are specified in
  [`design-storagenode.md`](design-storagenode.md), whose §5.1 adds a
  `spec.storageNodes` group to this kind. `StoragePool` has its own document to
  be written.
- **Not the rebalancing algorithm.** `spec.volumeAutoPlacement` configures the
  auto-rebalancer, whose scoring, candidate selection, and migration policy are
  specified in [`design-auto-rebalancing.md`](../design-auto-rebalancing.md). This
  document covers the fields and their defaults, not what reads them.
- **Not the API group's conventions.** The entity and action split, the naming
  rule, the ownership spine, and the annotation key prefix belong to
  [`design-crd-model.md`](design-crd-model.md) and are cited rather than
  restated.
- **Not the ownership edges the target model adds.** That a `StorageCluster`
  should own its pools and its tasks by controller reference, and what deleting
  one ought to do to a pool with bound volumes, is
  [`design-crd-model.md`](design-crd-model.md) §9.3 and its open question.

---

## 3. StorageCluster: API

Declared in `operator/api/v1alpha1/storagecluster_types.go`, short name `stc`.
**The type is Appendix A**, whole and as it is to be written. What follows quotes
the field an argument turns on and no more, so that one copy of each type exists
and it is the one an implementation is written against.

**The short name is `stc` because `sc` is `StorageClass`'s.** Two kinds may declare
the same short name and the RESTMapper resolves it by discovery order, so
`kubectl get sc` reaches `storageclasses.storage.k8s.io` and never this kind. The
operator writes one `StorageClass` per pool
([`design-storagepool.md`](design-storagepool.md) §5), which puts both kinds in
every cluster this runs in. `scops` is unaffected and stays as it is
([`design-crd-model.md`](design-crd-model.md) §7.11).

### 3.1 Spec

The spec divides into five groups by what a change to each costs.

**On-disk and on-wire layout, which a live cluster cannot tolerate changing.**
`stripe` carries the erasure-coding data and parity chunk counts, `fabricType`
the storage fabric, and `nvmfBasePort`, `rpcBasePort`, and `snodeApiPort` the
port ranges every node binds. `kms` names the key management service holding the
cluster's volume encryption keys, and is a block rather than a field, which is
the last part of this section. `enableFailureDomains` opts the cluster into
failure-domain mode, where every node must declare a fault group so the control
plane can spread chunks across independent ones. `enableNodeAffinity` selects
affinity-based placement for storage components. All eight are enforced immutable
once set, in two spellings that mean the same thing (§3.2).

**Uniform SPDK sizing, required and cluster-scoped on purpose.**

```go
// MaxSubsystemCount is the maximum number of NVMe-oF subsystems per storage
// node. Applies to every storage node in the cluster. Required: it sizes huge
// pages, and a node that receives no value fails config generation outright
// rather than falling back to a default.
// +kubebuilder:validation:Required
// +kubebuilder:validation:Minimum=10
// +kubebuilder:validation:Maximum=75
MaxSubsystemCount *int32 `json:"maxSubsystemCount"`

// VCPUCount is the number of vCPUs allocated to SPDK on each storage node.
// This is an explicit core count, not a percentage. Required: the core layout
// it produces must match across the cluster, so it is stated rather than left
// to a per-node heuristic.
// +kubebuilder:validation:Required
// +kubebuilder:validation:Minimum=6
VCPUCount *int32 `json:"vcpuCount"`
```

These live on the cluster because the control plane assumes them uniform. Huge
pages are sized from `maxSubsystemCount` together with the isolated core count,
and a node that disagrees with its peers ends up with a huge-page and core layout
the cluster cannot place erasure-coding chunks across evenly. Both are `Required`
rather than defaulted, because a node receiving no value fails config generation
instead of quietly running a different shape from its peers.

**What the cluster holds is the value a new node is stamped with, not a value
every node reads.** Each `StorageNode` carries its own effective sizing in
`spec.config.sizing`, copied from here when it is created
([`design-storagenode.md`](design-storagenode.md) §3.1). The two are equal across
the fleet in steady state, and a rolling hardware upgrade is what makes them
differ: replacing a fleet with larger machines is done one node at a time, and a
node moved to a host with more cores is re-sized when it moves, so for the
duration of the roll the cluster's nodes genuinely differ. A model that could not
express that would force the whole fleet to be re-sized at once or not at all.

Uniformity is therefore enforced rather than assumed. A node's sizing is immutable
to users and writable only by the operator, so unmanaged divergence, which is what
actually breaks chunk placement, is rejected at admission, while managed
divergence is an operation. What drives the operator's own re-size is
[`design-storagenode.md`](design-storagenode.md) §16, Q8.

**`minHugePagesSize` is the third member of the group, and it is a floor.** The
effective allocation is the larger of this value and the minimum the node's device
and subsystem count requires. Raising it reserves more huge pages than simplyblock
needs, lowering it below the computed minimum changes nothing, and omitting it
uses the computed minimum. A bare number is read as gigabytes.

```go
// MinHugePagesSize is the smallest huge-page allocation each storage node makes,
// as a size string ("100G", "1T"; a bare number is gigabytes). It is a floor and
// not a limit: the effective allocation is the larger of this value and the
// minimum the node's device and subsystem count requires, so simplyblock takes
// more when it needs more. Omitted, the computed minimum is used.
// +optional
MinHugePagesSize string `json:"minHugePagesSize,omitempty"`
```

The value is forwarded to the control plane as `hugepages_mem` and written into
each node's configuration as `MAX_HUGE_PAGES_SIZE`. Nothing compares an allocation
against it, which is what makes it a floor rather than a limit (§12).

**Capacity thresholds.** `warningThreshold` and `criticalThreshold` each carry an
absolute `capacity` and a `provisionedCapacity` value, passed to the backend at
creation as four separate parameters.

**Operational policy, freely mutable.** `maxConcurrentWorkerRestarts` caps how
many Kubernetes worker nodes the operator may drain and restart at once, with a
minimum of 1 and a default of 1 when unset. The effective value is
`min(spec.maxConcurrentWorkerRestarts, status.maxFaultTolerance)`, computed by
`effectiveConcurrentRestarts` and published to `status` so tooling reads one
authoritative number rather than recomputing it.

**Two separate migration policies, deliberately not one.**
`volumeMigrationSettings` controls manual migration and the post-migration data
realignment. `volumeAutoPlacement` configures automatic latency-driven
rebalancing. They are separate because realignment applies to every volume move,
whether it came from the auto-rebalancer, a manual migration, or a drain, so it
cannot sit under the rebalancing policy that is only one of its three sources.

**`spec.backup` is where the cluster's backups live, and setting it is what makes
them visible.** It names an S3 endpoint, a bucket, an optional prefix, and the Secret
holding the credentials, and it is the one location the control plane writes copies to
and the operator reads them from. Setting it at creation and setting it a year later are
the same operation, which is why it is one of the few mutable fields here: a cluster
that has never had a store gains one by acquiring the field, and every backup already in
that bucket becomes a `StorageBackup`
([`design-storagebackup.md`](design-storagebackup.md) §5.1).

**Repointing it changes which backups have objects, and destroys nothing.** The object
set is derived from the location on every walk rather than accumulated across walks, so
a store swapped for another leaves the first store's backups where they are and stops
representing them. Unsetting the field is the same statement with an empty answer.

**Each policy's on-off switch is a field of the spec, and there are two of them.**
`spec.enableDataRealignment` and `spec.enableVolumeAutoPlacement` sit beside the blocks
they govern rather than inside them, because a toggle named for its subject repeats
itself when the subject is also its parent:
`spec.volumeAutoPlacement.enableVolumeAutoPlacement` says the same word twice. Only the
switches move up. Each block keeps its tuning fields, so the grouping §3.1 is built on
survives and the thing a reader turns on is one field at the top rather than one buried
in each block.

**There is no switch for volume migration, because migration cannot be turned off.**
The registered `volumeMigrationSettings.enabled` implies a cluster that refuses to move
a volume, and that cluster cannot drain a node, cannot rebalance, and cannot replace a
device: migration is how those are performed rather than a feature layered on top of
them. So the field is removed rather than renamed, and what remains under
`volumeMigrationSettings` is how migration behaves, not whether it happens.

`volumeMigrationSettings.dataRealignment` carries the interaction that most
affects a busy cluster. A realignment blocks all volume migrations for as long as
the control plane needs, measured at tens of minutes on a loaded cluster, so
`interval` is a floor on the spacing between realignment *requests* and not a
ceiling on how long one takes. `minMoves` defaults to 1, which makes migration
and realignment alternate, and is the field to raise in order to batch. The
`storage.simplyblock.io/trigger-realignment` annotation bypasses both.

#### Key management is a block, not a field

`spec.kms` selects where the cluster stores volume encryption keys, with one member
per provider.

```go
// KMS selects where the cluster stores volume encryption keys.
// +optional
KMS *KMSSpec `json:"kms,omitempty"`
```

**The provider is not the concern, key management is.** A field named for
HashiCorp forecloses the shape a second provider needs. Adding one to the flat
form means a second top-level field beside the first, and then nothing in the API
says the two are alternatives, that setting both is meaningless, or which one a
cluster is actually using. Under `kms` they are siblings, so the mutual exclusion
becomes one CEL rule on the block when the second provider arrives, and a reader
sees a choice rather than two unrelated settings. The `Settings` suffix goes with
the move, because `spec.kms.vault.baseURL` says what it is without it.

One provider needs no exclusion rule, so `kms` carries none. What it does carry is
the immutability that `hashicorpVaultSettings` carries now, moved up
to the block: `!has(oldSelf.kms) || self.kms == oldSelf.kms` (§3.2). Keeping the
rule on the block rather than on `vault` is deliberate, since switching providers
on a live cluster is at least as unsupportable as changing one provider's endpoint.

The backend is unaffected. `hashicorp_vault_settings.base_url` is the control
plane's name for the same value, and the operator's request builder maps to it
from wherever the field sits, so this is a Kubernetes-side regrouping only.

### 3.2 Immutability

Eight spec fields are enforced immutable. Every one of them is optional, and the
enforcement is immutable once set: the field may be filled in later, and from that
point it can be neither changed nor removed.

| Spelling                                         | Fields                                                                       |
|--------------------------------------------------|------------------------------------------------------------------------------|
| `+k8s:immutable` on the field                    | `enableNodeAffinity`, `enableFailureDomains`                                 |
| Type-level `+kubebuilder:validation:XValidation` | `fabricType`, `kms`, `stripe`, `nvmfBasePort`, `rpcBasePort`, `snodeApiPort` |

`+k8s:immutable` generates two rules. controller-gen v0.21.0 emits a field-level
`self == oldSelf`, and for a field outside `required` a parent-level
`!has(oldSelf.X) || has(self.X)`. The field rule rejects a change of value. The
parent rule rejects removal, covering the case the field rule cannot: a field-level
transition rule is evaluated only where the field is present, so a cleared field
would otherwise be re-settable to any value. A first assignment matches neither
rule, which is the once-set semantics.

On a `Required` field the parent rule is omitted and the field rule applies from
creation, since the field is never absent. `StorageClusterOps.spec.clusterRef` and
`spec.action` are that case (§5.1).

The type-level CEL form expresses the same intent at greater length and guards
only the value, so the six fields carrying it take `+k8s:immutable` on the next
change to each.

Everything outside that table is mutable, including the sizing group. Changing
`vcpuCount` or `maxSubsystemCount` on a live cluster is accepted by the API
server, and it changes what the next node created is stamped with rather than
re-sizing the nodes that exist (§3.1). Re-sizing those is an operation, because a
node's layout is generated from its own copy and changing it under a running node
is restart-shaped rather than declarative.

### 3.3 Status

`status.uuid` is the backend cluster UUID and the field the controller branches
on: empty means the cluster has not been created, non-empty means steady state
(§4.1). `status.clusterName` is the resolved backend name, `status.nqn` the
cluster subsystem qualified name, and `status.erasureCodingScheme` the active
layout rendered as `"<ndcs>x<npcs>"`. `status.status` is the backend-reported
lifecycle status, and `status.configured` records that initial setup completed.

`status.maxFaultTolerance` is the backend-reported number of nodes that may be
simultaneously offline without violating redundancy, and it is the ceiling in the
`effectiveConcurrentRestarts` computation published as
`status.maxConcurrentWorkerRestarts`.

`status.activeOpsRef` is the operation lock (§8). `status.rebalancingMetrics` is
written by the auto-rebalancer each evaluation cycle and is specified in
[`design-auto-rebalancing.md`](../design-auto-rebalancing.md).

Two counters track realignment debt. `status.volumeMoveGeneration` is incremented
by every migration reaching `Completed` and by nothing else, so it only grows.
`status.realignedGeneration` is the generation the last successfully requested
realignment covers, and a realignment is outstanding while the former exceeds the
latter. The recorded value is the one read *before* the request was sent, which
is what the realignment can actually account for: a migration completing while
the request is in flight raises `volumeMoveGeneration` past it and correctly
leaves another realignment outstanding, rather than being swallowed by the run
already in progress.

`status.phase` is the cluster's lifecycle phase and `status.step` the creation
machine's position, both specified with the creation lock in §4.2.

`status.tasks` is what the backend is currently busy with, specified in §3.4.

**Four registered status fields are removed rather than carried forward.**
`mgmtNodes`, `storageNodes`, `lastUpdated`, and `created` are declared on the
registered kind, carry `FIXME` comments naming a possible API dependency, and are never
written. A field that is declared and never written reports a definite-looking zero
instead of nothing, which is worse than its absence
([`design-crd-model.md`](design-crd-model.md) §7.9), and removing one is cheap only
while the group is at `v1alpha1`.

### 3.4 The Tasks the Backend Is Running

The control plane runs asynchronous work of its own: migrations, rebalances, and
health jobs, none of which `kubectl` shows. What an administrator wants at that moment is
the answer to "what is this cluster doing," which is a property of the cluster and
belongs in its status.

**`status.tasks` holds the running and pending tasks, and never more than twenty.**

```go
// Tasks are the control plane's own asynchronous jobs, as of the last stream
// frame: running and pending only, newest first, and capped. It is a window on
// the backend rather than a record: a task that finishes leaves the list, and
// what remains of it is the event that says it did (§10.1).
// +kubebuilder:validation:MaxItems=20
// +optional
Tasks []ClusterTask `json:"tasks,omitempty"`
```

**Twenty is a window onto a cluster's work.** A busy cluster runs more than twenty
tasks, and the field shows what fits in a status somebody reads: ordered newest first,
so the twenty most recent running or pending tasks are visible and the rest stay in the
control plane. The
cap is what keeps an object bounded whose subject is not, which is the constraint any
status list has to answer to
([`design-crd-model.md`](design-crd-model.md) §3.1).

**Only running and pending tasks appear.** A completed or canceled task is not
current state, so it leaves the list, and the object stops describing it. That is what
makes the cap safe: the list's length tracks concurrency rather than history, and a
cluster that has run ten thousand tasks has as short a list as one that has run ten.

**What happens to a task is an event, which is where the history lives.** A task
reaching a terminal outcome emits on the `StorageCluster`: `TaskCompleted` or
`TaskCanceled` (§10.1). Events expire and the list is capped, so neither is an audit
log, and neither claims to be. The control plane holds the record, `kubectl describe`
shows the recent past, and `status.tasks` shows the present.

**A task is addressed by its control-plane ID, not by its position.** `ClusterTask.id`
is what `StorageClusterOps` with `action: CancelTask` names (§6.3), so an operation
against a task survives the list being reordered by a later frame. That is the property
a list in status has to provide before anything can act on one of its entries.

### 3.5 Examples

The smallest valid `StorageCluster` is the two required sizing fields and nothing
else. Everything the control plane can default, it defaults.

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageCluster
metadata:
  name: production
  namespace: simplyblock
spec:
  maxSubsystemCount: 20
  vcpuCount: 8
```

A cluster that sets the layout, the tenancy thresholds, key storage, and both
migration policies:

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageCluster
metadata:
  name: production
  namespace: simplyblock
spec:
  # Layout. Immutable once set (§3.2).
  stripe:
    dataChunks: 2
    parityChunks: 1
  fabricType: tcp
  clientDataIfname: eth1
  nvmfBasePort: 4420
  rpcBasePort: 8080
  snodeApiPort: 50001
  enableFailureDomains: true
  enableNodeAffinity: true
  kms:
    vault:
      baseURL: https://vault.example.com:8200

  # Uniform SPDK sizing. Required, and a floor rather than a cap (§3.1).
  maxSubsystemCount: 20
  vcpuCount: 8
  minHugePagesSize: 100G

  # Tenancy thresholds, in percent.
  warningThreshold:
    capacity: 75
    provisionedCapacity: 150
  criticalThreshold:
    capacity: 90
    provisionedCapacity: 200

  maxConcurrentWorkerRestarts: 2

  backup:
    endpoint: https://s3.example.com
    bucket: simplyblock-backups
    prefix: production/
    credentialsSecretRef:
      name: backup-credentials

  enableDataRealignment: true
  enableVolumeAutoPlacement: true

  volumeMigrationSettings:
    rebalancerImage: quay.io/simplyblock-io/rebalancer:26.2.2
    dataRealignment:
      interval: 10m
      minMoves: 4

  volumeAutoPlacement:
    evaluationInterval: 60s
    imbalanceThreshold: 80
    minHotColdDifferencePct: 20
    maxVolumeMigrationsPerCycle: 10
    metricsBackend: prometheus
    prometheusURL: http://prometheus.monitoring:9090
    enableLatencyBenchmark: true
    latencyBenchmarkInterval: 5m
```

Both switches are shown set, which is not their default: each is `enable`-formed, so
each is off unless a spec asks for it (§3.1).

A status on a running cluster with no operation in flight:

```yaml
status:
  phase: Online
  uuid: 8f3c1e70-9a2b-4d51-b1c7-2f6e0d9a4c88
  clusterName: production
  nqn: nqn.2023-02.io.simplyblock:8f3c1e70
  status: active
  erasureCodingScheme: 2x1
  configured: true
  maxFaultTolerance: 1
  maxConcurrentWorkerRestarts: 1
  rebalancing: false
  volumeMoveGeneration: 412
  realignedGeneration: 412
  lastDataRealignmentAt: "2026-08-28T09:14:02Z"
  observedGeneration: 7
```

`status.step` is absent because the creation machine is terminal, and
`status.activeOpsRef` is absent because no operation holds the lock.
`maxConcurrentWorkerRestarts` reads 1 rather than the 2 the spec asks for, because
the effective value is clamped to `maxFaultTolerance` (§3.1).

---

## 4. StorageCluster: Controller

`StorageClusterReconciler`, in
`operator/internal/controllers/cluster/storagecluster_controller.go`. Since
`StorageClusterOps` took over the imperative work, this reconciler is
steady-state only.

### 4.1 Reconcile

```
┌──────────────────────────────────────────────────────────────┐
│                  Kubernetes Control Plane                    │
│   ┌──────────────────────────────────────────────────────┐   │
│   │              StorageClusterReconciler                │   │
│   │  1. Get the CR from the API server, not the cache    │   │
│   │  2. Deletion: backend DELETE, then finalizer         │   │
│   │  3. Ensure the finalizer                             │   │
│   │  4. status.uuid != ""  → syncStatus                  │   │
│   │  5. status.uuid == ""  → reconcileCreate             │   │
│   └──────────────────────────────────────────────────────┘   │
│  StorageCluster CR   spec.*   status.uuid   status.phase     │
└──────────────────────────────────────────────────────────────┘
              │ HTTP (webapi client, service-account bearer token)
┌─────────────▼────────────────────────────────────────────────┐
│                  Simplyblock Control Plane                   │
│  GET    /api/v2/_meta/ready                                  │
│  POST   /api/v2/clusters/                                    │
│  GET    /api/v2/clusters/{cluster}                           │
│  DELETE /api/v2/clusters/{cluster}                           │
└──────────────────────────────────────────────────────────────┘
```

The CR is fetched with a direct read rather than from the informer cache. A
cached read can still return `status.uuid == ""` immediately after
`Status().Patch` has persisted a UUID, and acting on that stale value is a second
`POST` and a second backend cluster.

### 4.2 Creation, and the lock that makes it single-shot

Creating a backend cluster is not idempotent, and two reconcilers that both
observe a cluster with no UUID would both create one. The claim is therefore made
in Kubernetes before the backend is touched, as the machine's first transition:

```
  no status.uuid
    │
    ▼
  Claiming              ← Status().Patch with MergeFromWithOptimisticLock
    │  409 Conflict → another reconciler owns the claim, requeue
    │  patched
    ▼
  CheckingControlPlane  ← GET /_meta/ready, emitting FDBNotReady on failure
    │  ready
    ▼
  Adoption?             ← Secret "simplyblock-{name}-upgrade" present?
    │  no                                    │ yes → Adopting (§4.3)
    ▼
  ResolvingConfig       ← backup credentials Secret, Vault URL, huge-page size
    │  valid
    ▼
  Creating              ← POST /clusters/; on failure look the cluster up by
    │  created            name and divert to Adopting
    ▼
  Persisting            ← CSI credentials Secret, then status.uuid and the rest
    │
    ▼
  phase: Online
```

**The mutex is the optimistic-lock patch, not the value it writes.**
`Status().Patch` with `MergeFromWithOptimisticLock` succeeds for exactly one
reconciler at a given `resourceVersion` and returns 409 to the rest, so persisting
the transition into `Claiming` is what makes creation single-shot. Any persisted
field would serve as the token. Making it the step means the field also says
something.

A failed `POST` is treated as a possible race rather than as a failure. The
controller looks the cluster up by name and adopts it if it is there, which covers
two reconciles that both passed the claim on different `resourceVersion`s, and a
response lost after the backend committed.

#### The phase and the step

```go
// StorageClusterPhase is where the operator has got to with this cluster.
// +kubebuilder:validation:Enum=Pending;Creating;Online;Degraded;Unavailable;Suspended
type StorageClusterPhase string

// StorageClusterStep is one step of the creation path. There is one graph rather
// than a MultiConfig, because an entity has no spec.action to key one on.
// +kubebuilder:validation:Enum=Claiming;CheckingControlPlane;ResolvingConfig;Creating;Adopting;Persisting
type StorageClusterStep string
```

`Adopting` is reached from two states rather than one: the upgrade Secret diverts
before any `POST`, and a `POST` that failed against an existing cluster diverts
after (§4.3). Both converge on `Persisting`. Declaring both edges makes that a
graph a reader can check rather than two `adoptExistingCluster` calls in unrelated
branches.

Each step carries a bound through `status.step.deadline`, so a control plane that
never becomes ready is a step that expires rather than a reconcile that repeats
forever, and a cluster stuck in `CheckingControlPlane` is distinguishable from one
still coming up.

### 4.3 Adoption

Adoption is how a backend cluster the operator did not create becomes a managed
one. Three paths reach it: an explicit upgrade Secret, a `POST` that failed
against a cluster which already exists, and a name lookup that finds one.

The upgrade path is the migration route off a Helm deployment. An operator finds
a Secret named `simplyblock-{clusterName}-upgrade` carrying `uuid` and `secret`,
fetches that cluster, and populates status from the response without posting
anything.

Adoption writes a Secret named `simplyblock-cluster-{name}` holding the UUID and
the cluster secret, owned by the CR through a controller reference.

### 4.4 Steady-state synchronization

With a UUID present, the controller reads the cluster from the control-plane store
rather than from the HTTP API. An `updated` event on the cluster stream enqueues a
reconcile, which writes `status` from the streamed object
([`design-crd-model.md`](design-crd-model.md) §7.7). When `status`, `nqn`, and
`rebalancing` all match what is already recorded the reconcile returns without
patching, so a cluster whose state has not moved produces no writes.

The cluster stream is scoped to the API root rather than per cluster, so one
subscription serves every `StorageCluster` in every namespace. The rolling
restart's per-node reads come from the storage-node stream, which is scoped per
cluster (§7).

Two reads stay direct, because the control plane streams neither. The readiness
probe of §4.2 is a one-shot `GET /_meta/ready` per cluster creation, and the
DaemonSet pod readiness of §7.2 is a Kubernetes object rather than a control-plane
one. A `RequeueAfter` survives for those and as a slow backstop for a stream the
subscription manager has not yet noticed is dead. It is never how a change is
discovered.

The same reconcile re-upserts the CSI credentials Secret, reading the per-cluster
Secret and writing the aggregate entry the CSI driver consumes, which is how an
entry deleted or corrupted out of band is restored without intervention.

### 4.5 Deletion

The finalizer is `storage.simplyblock.io/storagecluster-finalizer`, which is the
group's `<lowercased kind>-finalizer` spelling and a rename of the one that ships
(§12). On deletion the
controller issues `DELETE /clusters/{cluster}`, and on any non-2xx response requeues
after 20 seconds and retries rather than removing the finalizer, so a backend
that is unreachable blocks the object instead of orphaning the cluster behind it.
The CSI credentials entry is removed next, and the finalizer last.

A CR with no `status.uuid` has nothing to delete, and its finalizer is removed
immediately.

---

## 5. StorageClusterOps: API

Declared in `operator/api/v1alpha1/storageclusterops_types.go`, short name
`scops`. The type is Appendix B.

### 5.1 Spec

```go
// StorageClusterOpsAction is the operation a StorageClusterOps performs.
// +kubebuilder:validation:Enum=Activate;Expand;Shutdown;Start;Restart;RollingRestart;CancelTask
type StorageClusterOpsAction string
```

`clusterRef` and `action` are `Required` as well as `+k8s:immutable`, so they are
immutable from creation rather than once set (§3.2). That is the correct strength:
an operation is the record of one thing done to one target, and re-pointing either
after the fact would make the record describe something that never happened. The
target is named rather than owned, because deleting the record of an operation must
never delete the cluster it operated on
([`design-crd-model.md`](design-crd-model.md) §3).

`refreshSNodeAPI` keeps a verb rather than taking an `enable` prefix: it
parameterizes one operation rather than turning a cluster capability on, which is
the class [`design-crd-model.md`](design-crd-model.md) §7.5 leaves outside the
`enableXyz`/`disableXyz` rule.

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: roll-the-fleet
  namespace: simplyblock
spec:
  clusterRef: production
  action: RollingRestart
  rollingRestart:
    refreshSNodeAPI: true
```

### 5.2 Status

```go
// StorageClusterOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StorageClusterOpsPhase string
```

`Pending` means the operation holds no lock and has issued nothing. `Running` means
it holds the lock and its first side effect may have been issued (§6.2).
`Succeeded` and `Failed` are the terminal outcomes, and `Aborted` is the third:
a rolling restart holding on a degraded cluster is the one operation an
administrator has a reason to call off, and a called-off operation did not go
wrong.

`status.phase` and `status.step.state` each carry a `+kubebuilder:printcolumn`, so
`kubectl get scops` answers where an operation is without a `describe`.

#### Examples

An operation that is one call and one wait, in flight:

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: activate-production
  namespace: simplyblock
spec:
  clusterRef: production
  action: Activate
status:
  phase: Running
  step:
    state: Awaiting
    deadline: "2026-08-28T09:25:00Z"
  message: waiting for the cluster to report active
  startedAt: "2026-08-28T09:20:11Z"
  observedGeneration: 1
```

A rolling restart part-way through its walk. This is the status worth reading
twice, because the position is split across two fields on purpose: `step` is where
the machine has got to within one node, and `rollingRestart` is which node that is
(§7).

```yaml
apiVersion: storage.simplyblock.io/v1alpha1
kind: StorageClusterOps
metadata:
  name: roll-the-fleet
  namespace: simplyblock
spec:
  clusterRef: production
  action: RollingRestart
  rollingRestart:
    refreshSNodeAPI: true
status:
  phase: Running
  step:
    state: RestartingNode
    deadline: "2026-08-28T09:41:30Z"
  rollingRestart:
    nodes:
      - 1a4f7c22-0e6b-4a19-9c33-b8d2e5f10477
      - 7b90d3e4-52a1-4f0c-8d76-3ca9017be255
      - c2e58f16-9d47-4b83-a015-6f4b8e2d3901
    nodeIndex: 1
  message: "Node 2/3 (7b90d3e4-52a1-4f0c-8d76-3ca9017be255): RestartingNode"
  startedAt: "2026-08-28T09:31:44Z"
  observedGeneration: 1
```

The same operation holding because a peer is down, which is the state §7.2 exists
to make legible:

```yaml
status:
  phase: Running
  step:
    state: CheckingPeers
    deadline: "2026-08-28T10:05:00Z"
  rollingRestart:
    nodes: [1a4f7c22-…, 7b90d3e4-…, c2e58f16-…]
    nodeIndex: 2
  message: "Node 3/3 (c2e58f16-9d47-4b83-a015-6f4b8e2d3901): waiting for peer nodes"
```

`nodeIndex` is 2 with three nodes, so two are done and the walk is holding before
touching the third. Nothing has been sent to that node: `CheckingPeers` performs no
side effect, and its deadline is what turns an indefinite hold into a step the
controller can report on (§7.2).

A terminal operation, which stays as the audit record:

```yaml
status:
  phase: Succeeded
  step:
    state: Rebalancing
  rollingRestart:
    nodes: [1a4f7c22-…, 7b90d3e4-…, c2e58f16-…]
    nodeIndex: 3
  message: all 3 nodes restarted
  startedAt: "2026-08-28T09:31:44Z"
  completedAt: "2026-08-28T09:58:02Z"
  observedGeneration: 1
```

`nodeIndex` equals `len(nodes)`, which is what completion means (§7.1). `step.state`
holds the last node's terminal step and carries no deadline, because `Rebalancing`
completed rather than expiring.

### 5.3 The step machine

[`design-crd-model.md`](design-crd-model.md) §3.1 requires every `Ops` kind to
carry a `status.step` holding the serialized snapshot of a declared
`atlas-lib/statemachine` graph, rather than a position the reconciler decides
inline. `StorageClusterOps` serves seven actions, so it takes the `MultiConfig` form:
one graph per action over one step type.

```go
// StorageClusterOpsStep is the union of every action's steps; which steps belong
// to which action is declared by the graph rather than by this type.
// +kubebuilder:validation:Enum=Requesting;Awaiting;ShuttingDown;Starting;CheckingPeers;ShuttingDownNode;RefreshingPod;AwaitingPod;RestartingNode;Rebalancing
type StorageClusterOpsStep string
```

**Every value is PascalCase**, which is the rule for an enum this API group defines
rather than a choice this kind makes: it matches `status.phase` beside it, and it
matches every enum core Kubernetes owns
([`design-crd-model.md`](design-crd-model.md) §7.8). §12 is what the rename
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

Each action's graph is specified with that action: §6.3 for the four that are one
call and one wait, §6.4 for `Restart`, and §7 for `RollingRestart`. Two properties
hold across all of them.

**The union stops being a union.** One `status.step` field serves all six actions,
so nothing in the type prevents an `Activate` from reporting `Rebalancing`. The
per-action graph makes that an `IllegalTransitionError` at the point of the write.

**Every graph is validated, not only the one in hand.** `MultiConfig` checks all
six whenever a machine is built for any of them, so a bad edge in
`RollingRestart`, which is the action least often exercised and the most expensive
to exercise, is caught by any test that builds a machine at all.

---

## 6. StorageClusterOps: Controller

`StorageClusterOpsReconciler`, in `controllers/cluster/storageclusterops_controller.go` and
`controllers/cluster/rollingrestart.go`.

### 6.1 Reconcile

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
  Terminal phase?    ← release the lock again best-effort, remove the finalizer
    │  no
    ▼
  Get the cluster    ← not found → Failed
    │  found
    ▼
  Lock free?         ← held by another ops → stay Pending, requeue after 10s
    │  free or ours
    ▼
  Acquire the lock   ← optimistic-lock patch; 409 → requeue immediately
    │
    ▼
  Pending → Running  ← stamp startedAt
    │
    ▼
  Dispatch on spec.action
```

The terminal-phase branch releases the lock a second time on purpose. A crash
between persisting `Succeeded` and clearing `activeOpsRef` would otherwise leave
the cluster locked by a finished operation forever, and the release is idempotent
(§8).

Watching only `StorageClusterOps` would leave a queued operation waiting up to
its 10-second requeue after the lock frees. `clusterToOpsRequests` maps a
`StorageCluster` event back to every operation targeting it, so a release wakes
the queue immediately.

### 6.2 The persisted position is the write-ahead record

A side effect is preceded by a write, so that a process dying between the two
restarts into a state saying the call may already have landed. Two levels carry
that, and neither needs a flag beside it.

`status.phase` carries the outer one. `Pending` means the operation holds no lock
and has issued nothing. `Running` means it holds the lock and its first side effect
may have been issued. The transition is persisted before dispatch, so a restart
never re-enters an action believing nothing has happened.

`status.step` carries the per-step one. A step is persisted before the side effect
that step performs, so a restart finds the step recorded rather than deciding
afresh whether to make the call.

**A recorded step whose side effect never fired is indistinguishable from one whose
did, and that is safe rather than merely tolerated.** Every step's completion
condition is a predicate over current state rather than an observation of a
transition (`design-crd-model.md` §7.7), and every call a step makes is skipped
when its target is
already at or past the state that call would produce, so a node already `offline`
receives no second shutdown. The question a `triggered` flag answers is therefore
one this controller never asks: it reads the state and either acts or advances.

No `status.triggered` and no per-step equivalent appear anywhere in §5, because
neither would carry information the position does not.

### 6.3 Actions

Five of the seven are one call and one wait, and share a two-step graph:

```
Activate, Expand, Shutdown, Start, CancelTask
    Requesting ──► Awaiting
      ← POST the action        ← wait for the completion condition below
```

The completion condition is evaluated against streamed state in the target model,
against the streamed cluster object (§4.4).

| Action           | Backend call                                          | Completion condition                  |
|------------------|-------------------------------------------------------|---------------------------------------|
| `Activate`       | `POST /api/v2/clusters/{cluster}/activate`            | `status == active`                    |
| `Expand`         | `POST /api/v2/clusters/{cluster}/expand`              | `status == active`                    |
| `Shutdown`       | `POST /api/v2/clusters/{cluster}/shutdown`            | `status != active`                    |
| `Start`          | `POST /api/v2/clusters/{cluster}/start`               | `status == active`                    |
| `CancelTask`     | `POST /api/v2/clusters/{cluster}/tasks/{task}/cancel` | The task leaves `status.tasks` (§3.4) |
| `Restart`        | `Shutdown`, then `Start` (§6.4)                       | `status == active`                    |
| `RollingRestart` | A per-node state machine (§7)                         | Every node processed                  |

**`CancelTask` is the one action with nothing to call yet.** The v2 API lists tasks
and reads one by ID, and offers no cancel for them (§9), so the action is specified
and blocked rather than specified and buildable. Its completion condition needs the
task stream either way, since a cancel the control plane accepted is one it finishes
in its own time.

The endpoint column keeps the control plane's own lowercase paths. A URL segment
is the control plane's vocabulary rather than this group's, which is the same
distinction that keeps `status == active` lowercase: only a value this API defines
is PascalCase.

**`CancelTask` is the only action whose target is inside the cluster rather than the
cluster itself.** `spec.cancelTask.taskID` names one entry of `status.tasks` by its
control-plane ID, which is why §3.4 makes the ID the address: an index would name a
different task after the next stream frame reordered the list. The operation is
otherwise an ordinary one, taking the cluster's lock like every other, because a
cancellation and a shutdown arriving together are two things the cluster should not be
doing at once.

**Its completion condition is the task leaving the list.** A cancel the control plane
accepted is a cancel it has started, and it finishes the stopping in its own time, so `Awaiting` waits for the stream to report the task gone from the running and
pending set, and `TaskCanceled` is emitted then (§10.1). That is the same rule §7.7 of
[`design-crd-model.md`](design-crd-model.md) states for every step in the group, and
the same reason it is stated.

**Canceling a task that is already gone succeeds.** The task finished on its own
between the operation being written and its `Requesting` step, which is the outcome the
operation asked for reached by another route. A 404 from the cancel endpoint is
therefore success, and the distinction between "canceled" and "finished first" is in
the events rather than in the operation's phase.

### 6.4 Restart is a client-side sequence

The control plane has no cluster restart endpoint, so `Restart` sequences a
shutdown and a start. That makes it the one action with two side effects, and the
`ShuttingDown` and `Starting` steps of §5.3 are where the sequence lives:

```
  ShuttingDown   ← POST /shutdown, then wait for status != active
    │
    ▼
  Starting       ← POST /start, then wait for status == active
    │
    ▼
  (terminal; the phase moves to Succeeded)
```

A server-side restart endpoint would collapse this graph to the two-step shape the
other four actions use (§9), which is a reason to ask for one rather than a reason
to wait.

---

## 7. Rolling Restart

The other five actions are one call and one wait. This one walks every storage node
in the cluster sequentially, so it is the only action whose progress has to survive
an operator restart.

**The graph covers one node, and the walk is the field beside it.**

```
RollingRestart, one machine lifetime per node
    CheckingPeers ──► ShuttingDownNode ──┬──► RefreshingPod ──► AwaitingPod ──┐
                                         │                                    │
                                         │                                    ▼
                                         └──────────────────────► RestartingNode ──► Rebalancing
```

`Rebalancing` is terminal, so `IsTerminal()` answers "this node is done" and the
machine never carries a cycle. Starting the next node is `Machine.Reset()`, which
returns to `CheckingPeers`, clears the deadline, validates no edge, and runs no
hook. That is the right primitive: entering `CheckingPeers` for node five is not a
transition out of node four's `Rebalancing`, and declaring it as one would make the
graph cyclic and `IsTerminal()` useless. Because `Reset` runs no hook, whatever
entering `CheckingPeers` implies is the controller's to do on the next pass, which
it does anyway because the peer check is that step's work.

Clearing the deadline is what gives each node its own budget: a twenty-node walk is
bounded per step per node rather than by one deadline covering all of them.

`status.step` is therefore not the whole position. It says where the machine has got
to within the node being restarted, and `status.rollingRestart` says which node that
is. Neither is complete without the other, which is why the node list is not machine
state:

```go
// Nodes is the ordered list of storage node UUIDs this action covers, written
// once when the walk starts and not modified afterward.
Nodes []string `json:"nodes,omitempty"`

// NodeIndex is the position in Nodes of the node being restarted. No omitempty:
// zero is a valid index, and a field that disappears at zero makes "the first
// node" and "unset" the same wire value.
NodeIndex int32 `json:"nodeIndex"`
```

An immutable list with an index is what makes advancing one increment rather than a
removal and an append that have to agree, keeps the order the walk was planned in
to the end instead of draining it away, and gives §7.3 its progress report without
counting anything.

### 7.1 Initialization

The action holds its cluster's lock and is `Running` before anything is listed,
which is the write-ahead for the whole walk (§6.2). It then lists the cluster's
storage nodes and writes `nodes` in the order it will restart them, `nodeIndex` at
zero, and `status.step` at `CheckingPeers`.

`nodes` is written once. A reconcile that finds it populated does not re-list, so a
node added to the cluster mid-walk is not restarted and a node removed mid-walk is
skipped when the walk reaches it. Both follow from a rolling restart being over the
fleet it was started against, and neither is a failure.

### 7.2 The steps

`RefreshingPod` and `AwaitingPod` are entered only when
`spec.rollingRestart.refreshSNodeAPI` is set. Without it, `ShuttingDownNode` goes
straight to `RestartingNode`.

| Step               | Side effect on entry                                                                           | Complete when                               |
|--------------------|------------------------------------------------------------------------------------------------|---------------------------------------------|
| `CheckingPeers`    | None                                                                                           | Every peer node is `online`                 |
| `ShuttingDownNode` | `POST /storage-nodes/{node}/shutdown`, skipped if the node is already at or past `in_shutdown` | The node is `offline` or beyond             |
| `RefreshingPod`    | Delete this node's storage-node DaemonSet pod, forcing an image pull                           | The pod is gone                             |
| `AwaitingPod`      | None                                                                                           | The replacement pod is `Ready`              |
| `RestartingNode`   | `POST /storage-nodes/{node}/restart`, skipped if the node is already `in_restart` or `online`  | The node is `online`                        |
| `Rebalancing`      | None                                                                                           | The cluster reports `is_re_balancing` false |

`Rebalancing` completing advances the walk: `nodeIndex` increments and the machine
resets. Success is `nodeIndex` reaching `len(nodes)`.

**A step completes on a state, not on a transition.** The "complete when" column is
a predicate over current state, deliberately: a step that waited to observe a
particular state would never complete against a coalescing stream, which delivers
current truth rather than an edit log. A node moving from `in_shutdown` to `offline`
to `in_restart` to `online` between two deliveries arrives as `online` once, and
`ShuttingDownNode` has to accept that. Polling has the same weakness and hides it
behind a short interval, so a predicate is the correct shape whether the state
arrives by stream or by poll (`design-crd-model.md` §7.7).

**The pre-shutdown peer check is the safety property of the whole action.** Taking a
node down while another is already offline can exceed the cluster's fault tolerance
and lose data, so `CheckingPeers` gates every shutdown on all peers being online and
the walk holds there rather than proceeding. Holding is reported in `status.message`
as `waiting for peer nodes`. The step's deadline is what distinguishes a walk holding
because the cluster is degraded from one holding because of a bug.

### 7.3 Progress

`status.message` carries `Node 2/5 (uuid): restarting`, read off `nodeIndex` and
`len(nodes)`, so `kubectl describe scops` shows the position in the walk without
reading the cluster CR.

---

## 8. Mutual Exclusion

`status.activeOpsRef` on the `StorageCluster` names the operation currently
allowed to act on it, which is the lock every entity in this group carries under
that name ([`design-crd-model.md`](design-crd-model.md) §3.2). That document
states the mechanism: an optimistic-lock acquisition, an idempotent release that
checks ownership, and a release on every terminal path. What is specific to this
kind is below.

**One operation per cluster is what the blast radius requires.** A cluster-wide
operation touches every node beneath it, so two at once would interleave restarts
across a fleet with no defined ordering. A second operation is admitted, sits at
`Pending`, and runs when the lock frees, which costs nothing because `Pending` is
where an operation starts anyway.

The release is `releaseClusterLock`, reached from the terminal transition through
`succeedOps` or `failOps`, from the terminal-phase branch of a later reconcile,
and from the finalizer `storage.simplyblock.io/storageclusterops-finalizer`. The
finalizer is the one that matters most, because `kubectl delete` on a running
operation would otherwise leave the cluster locked by an object that no longer
exists.

**A rolling restart holds the lock for the whole walk**, which on a large fleet is
the longest any operation holds one. Nothing bounds how long a queued operation
waits behind it, and `simplyblock_storagecluster_operation_lock_wait_seconds` (§10.2) is what
makes that wait a measurement rather than an anecdote.

---

## 9. Backend API Requirements

| Method   | Endpoint                                                   | Notes                                                                                                                                                 |
|----------|------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------|
| `GET`    | `/api/v2/_meta/ready`                                      | Readiness gate before creation. Non-2xx emits `FDBNotReady` and requeues                                                                              |
| `POST`   | `/api/v2/clusters/`                                        | Not idempotent, which is why §4.2 claims the slot in Kubernetes first                                                                                 |
| `GET`    | `/api/v2/clusters/?watch=true`                             | The cluster stream every status write and completion check reads (§4.4). Root-scoped, and an SSE subscription rather than a `GET` that returns        |
| `GET`    | `/api/v2/clusters/{cluster}/storage-nodes/?watch=true`     | The node stream the rolling restart reads (§7). One per cluster, and the same subscription [`design-storagenode.md`](design-storagenode.md) §12 opens |
| `GET`    | `/api/v2/clusters/{cluster}/tasks/?watch=true`             | The task stream `status.tasks` mirrors (§3.4), and what `CancelTask` waits on for the task to leave the list (§6.3). One per cluster                  |
| `DELETE` | `/api/v2/clusters/{cluster}`                               | Retried until it succeeds, and the finalizer is not removed before it does                                                                            |
| `POST`   | `/api/v2/clusters/{cluster}/activate`                      | Must tolerate a repeat, because a step recorded without its call having fired re-issues it (§6.2)                                                     |
| `POST`   | `/api/v2/clusters/{cluster}/expand`                        | Takes no parameters (§13, Q1)                                                                                                                         |
| `POST`   | `/api/v2/clusters/{cluster}/shutdown`                      | Used by `Shutdown` and as the first half of `Restart`                                                                                                 |
| `POST`   | `/api/v2/clusters/{cluster}/tasks/{task}/cancel`           | Used by `CancelTask`. A 404 is success, since a task already gone is a task not running (§6.3). **Not provided today**                                |
| `POST`   | `/api/v2/clusters/{cluster}/start`                         | Used by `Start` and as the second half of `Restart`                                                                                                   |
| `POST`   | `/api/v2/clusters/{cluster}/storage-nodes/{node}/shutdown` | Per-node, in the rolling restart walk                                                                                                                 |
| `POST`   | `/api/v2/clusters/{cluster}/storage-nodes/{node}/restart`  | Per-node, in the rolling restart walk                                                                                                                 |

**The three `?watch=true` rows are Server-Sent-Events subscriptions rather than
requests that return**, and they arrive with the control plane's SSE work rather
than with this design. `design-sse-push-notifications.md`, on the `sse` branch,
owns the wire contract and the subscription manager, and
[`design-crd-model.md`](design-crd-model.md) §7.7 is the rule that makes every
controller here read streamed state rather than poll for it. They are the external
dependency this design cannot satisfy on its own, and §12 records them against the
polling they replace.

**Two calls the control plane does not provide at all.** There is no cluster
restart, which is why `Restart` is a client-side sequence (§6.4). A server-side
restart would collapse the `Restart` graph to the two-step shape the other four
actions use (§6.4).

The second is the task cancel. The v2 API lists tasks and reads one by ID, and the
only cancel it offers is the volume migration's `DELETE`, so `CancelTask` (§6.3) has
nothing to call yet. It is the action's whole first step, which makes this the
prerequisite the action cannot ship without, unlike the restart, which only
simplifies a graph that already works.

---

## 10. Observability

The two controllers emit events already and export no metric at all. The events
table below is therefore additions to a surface that exists, and the metrics table
is new infrastructure: the only metrics in this area belong to the auto-rebalancer,
in `operator/internal/controllers/volume/rebalancer_metrics.go` and
`operator/internal/autoplacement/metrics.go`, and they describe rebalancing rather
than the cluster or its operations. An operation that walks twenty nodes and holds
the cluster lock for an hour has to be measurable as more than a changing
`status.message`.

### 10.1 Kubernetes events

Events need a target object, and both kinds are their own. An event about the
cluster's own reconcile goes on the `StorageCluster`, which is what an
administrator has open. An event about an operation goes on the
`StorageClusterOps`, which outlives the operation as its audit record and is what
`kubectl describe scops` shows.

| Event                                                    | Type      | Reason                   | On                  |
|----------------------------------------------------------|-----------|--------------------------|---------------------|
| The control plane is not ready to accept a creation      | `Warning` | `FDBNotReady`            | `StorageCluster`    |
| The backup credentials Secret cannot be resolved         | `Warning` | `BackupCredentialsError` | `StorageCluster`    |
| A user-supplied field failed validation                  | `Warning` | `InvalidConfig`          | `StorageCluster`    |
| The creation call was rejected by the control plane      | `Warning` | `ClusterCreationFailed`  | `StorageCluster`    |
| An existing backend cluster was adopted rather than made | `Normal`  | `ClusterAdopted`         | `StorageCluster`    |
| The operation is waiting for another to release the lock | `Normal`  | `OperationQueued`        | `StorageClusterOps` |
| The operation acquired the lock and started              | `Normal`  | `OperationStarted`       | `StorageClusterOps` |
| The operation finished successfully                      | `Normal`  | `OperationSucceeded`     | `StorageClusterOps` |
| The operation failed                                     | `Warning` | `OperationFailed`        | `StorageClusterOps` |
| The operation was canceled                               | `Normal`  | `OperationAborted`       | `StorageClusterOps` |
| A step's deadline expired                                | `Warning` | `StepDeadlineExceeded`   | `StorageClusterOps` |
| The walk is holding because a peer node is not online    | `Warning` | `PeerNodeNotOnline`      | `StorageClusterOps` |
| A backend task finished                                  | `Normal`  | `TaskCompleted`          | `StorageCluster`    |
| A backend task was canceled                              | `Normal`  | `TaskCanceled`           | `StorageCluster`    |
| The walk advanced to the next node                       | `Normal`  | `NodeRestarted`          | `StorageClusterOps` |

`ClusterCreationFailed` carries the HTTP status and the full response body, so the
cause is visible in `kubectl describe` without reading controller logs.

**`OperationSucceeded` is one reason for all six actions rather than one each.**
The action is already in `spec.action` and on a print column, so encoding it in the
reason name tells a reader nothing and gives anyone alerting on completion six
reasons to match instead of one.

**`PeerNodeNotOnline` is the load-bearing one.** Holding for a peer is correct
behavior, and without an event it is indistinguishable from a stalled controller.

### 10.2 Prometheus metrics

| Metric                                                              | Labels                        | Description                                                                                    |
|---------------------------------------------------------------------|-------------------------------|------------------------------------------------------------------------------------------------|
| `simplyblock_storagecluster_operation_duration_seconds`             | `cluster`, `action`, `result` | Histogram of operation durations from lock acquisition to a terminal phase                     |
| `simplyblock_storagecluster_operations_total`                       | `cluster`, `action`, `result` | Operations reaching a terminal phase, by `succeeded`, `failed`, and `aborted`                  |
| `simplyblock_storagecluster_operation_step_duration_seconds`        | `cluster`, `action`, `step`   | Histogram of per-step durations, which is where a slow operation is actually slow              |
| `simplyblock_storagecluster_operation_step_deadline_exceeded_total` | `cluster`, `action`, `step`   | Steps that ran out of time, including those that expired while the operator was down           |
| `simplyblock_storagecluster_operation_lock_wait_seconds`            | `cluster`, `action`           | Histogram of time spent `Pending` behind another operation's lock                              |
| `simplyblock_storagecluster_operation_active_state`                 | `cluster`                     | Gauge, 1 while `status.activeOpsRef` is set, so a lock held by a finished operation is visible |
| `simplyblock_storagecluster_rolling_restart_peer_hold_seconds`      | `cluster`                     | Histogram of time the rolling restart held for a peer node to come back online                 |
| `simplyblock_storagecluster_rolling_restart_node_index_count`       | `cluster`                     | Gauge of `nodeIndex`, against the `nodes` length, so walk progress is graphable                |
| `simplyblock_storagecluster_phase_state`                            | `cluster`, `phase`            | Gauge, 1 for the cluster's current phase (§4.2), so a cluster stuck in `Creating` is alertable |

Every metric carries `cluster`, matching the rebalancer's existing convention, so
one dashboard covers a multi-cluster deployment.

**Three of these exist to answer questions the design otherwise cannot.**
`simplyblock_storagecluster_operation_active_state` is the alert for a leaked lock: the release paths
in §8 are idempotent and run on three separate paths precisely because a lock held
by a terminal operation would block the cluster forever, and a gauge is how that
state is noticed rather than reported. `peer_hold_seconds` makes the §7.2 hold a
measurement rather than an anecdote, which is what decides whether it should
eventually carry a timeout. `step_deadline_exceeded_total` is the counter that
distinguishes an operation still working from one that stopped, which is the
distinction `status.message` cannot express.

---

## 11. Testing Strategy

Scenarios live in
[`tests/test-plan-storagecluster.md`](../../tests/test-plan-storagecluster.md) and
only there.

Unit tests with a fake client and a mock backend carry most of the weight, and
that is where the concurrency properties are provable: the creation lock's 409
path, the operation lock's acquire and release paths, and the idempotence of
`releaseClusterLock` are all pure control flow over a fake client.

The step machine moves risk rather than adding it. A transition table is data, so
every illegal transition is a cheap unit test. What a graph test does not reach is
the crash between a step's write and its side effect, which stays a crash-injection
scenario.

The risk that unit tests do not reach concentrates in two places. The first is
the write-ahead discipline, whose entire purpose is to survive a process dying
between a patch and an HTTP call, which needs the operator actually killed at
that point rather than a fake returning early. The second is the rolling restart
walk, which is long, per-node, and gated on peer health, so proving that it holds
on a degraded cluster and resumes when the cluster recovers needs a real cluster
with a node taken down.

---

## 12. Migration from the Registered API

Both kinds are registered and in use in a shape that predates the conventions this
design follows. This section is the whole of the delta, so that no other section
has to carry it.

| Registered                                                                          | This design                                                    | Cost                                                                                                                                                                                                                                                                                                                                                                      |
|-------------------------------------------------------------------------------------|----------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `spec.maxHugePagesSize`                                                             | `spec.minHugePagesSize` (§3.1)                                 | Spec rename. See the note below on how far it reaches                                                                                                                                                                                                                                                                                                                     |
| `spec.hashicorpVaultSettings.baseURL`                                               | `spec.kms.vault.baseURL` (§3.1)                                | Spec regrouping, Kubernetes-side only                                                                                                                                                                                                                                                                                                                                     |
| Sizing read live by every node                                                      | Stamped onto a node at creation (§3.1)                         | Behavioral. A cluster-wide sizing edit stops reaching existing nodes, which is what makes a rolling hardware upgrade expressible                                                                                                                                                                                                                                          |
| `StripeSpec`, `WarningThresholdSpec`, `CriticalThresholdSpec` Go field names        | `Stripe`, `WarningThreshold`, `CriticalThreshold` (Appendix A) | Go only. The JSON tags already read `stripe`, `warningThreshold`, and `criticalThreshold`, so nothing changes on the wire                                                                                                                                                                                                                                                 |
| No `spec.storageNodes`                                                              | The workload group (Appendix A)                                | Additive, and required by the `StorageNodeSet` retirement. `design-storagenode.md` §5 specifies it                                                                                                                                                                                                                                                                        |
| Six misnamed boolean toggles                                                        | `enableXyz` or `disableXyz` (`design-crd-model.md` §7.5)       | Spec renames, owned by `design-crd-model.md` §9.6, and §3.1 for the two this kind names                                                                                                                                                                                                                                                                                   |
| `volumeMigrationSettings.dataRealignment.enabled`                                   | `spec.enableDataRealignment` (§3.1)                            | Spec rename and a move up one level, and the `enable` form fixes the default at off                                                                                                                                                                                                                                                                                       |
| `volumeAutoPlacement.enabled`                                                       | `spec.enableVolumeAutoPlacement` (§3.1)                        | The same, and it is the choice `design-crd-model.md` §9.6 deferred to this kind                                                                                                                                                                                                                                                                                           |
| `volumeMigrationSettings.enabled`                                                   | Removed (§3.1)                                                 | Behavioral. Migration cannot be turned off, because a drain, a rebalance, and a device replacement are performed by moving volumes                                                                                                                                                                                                                                        |
| `spec.backup`, typed `BackupSpec`                                                   | The same field, typed `BackupStoreSpec` (Appendix A)           | Type rename. `design-controlplane.md` declares a different `BackupSpec` in the same package, and two cannot coexist                                                                                                                                                                                                                                                       |
| `spec.backup.localEndpoint`                                                         | `endpoint`, plus `bucket`, `prefix`, and `region` (Appendix A) | Field rename and three additions. The registered type had no bucket, so nothing in the store could be located                                                                                                                                                                                                                                                             |
| `spec.backup.withCompression`, `snapshotBackups`, `secondaryTarget`, `localTesting` | Removed (Appendix A)                                           | Spec removals. The store is a location, and how a copy is taken is the control plane's: it keeps accepting these values, so what changes is that the operator stops sending them and the backend's defaults apply                                                                                                                                                         |
| `spec.backup` as a write target only                                                | Also the inventory backups are discovered from (§3.1)          | Behavioral, and it is what retires backup import and export                                                                                                                                                                                                                                                                                                               |
| No `status.tasks`                                                                   | Present, capped at twenty (§3.4)                               | Additive, and what the retired task mirror becomes                                                                                                                                                                                                                                                                                                                        |
| `status.phase` as an untyped creation marker                                        | `StorageClusterPhase`, a lifecycle phase (§4.2)                | Status only. The optimistic-lock claim moves to the step transition                                                                                                                                                                                                                                                                                                       |
| `status.subPhase`, one value                                                        | `status.step`, the creation machine's snapshot (§4.2)          | Status only. The old string reads into `step.state` with no deadline                                                                                                                                                                                                                                                                                                      |
| No `observedGeneration` on either kind                                              | Present on both                                                | Additive                                                                                                                                                                                                                                                                                                                                                                  |
| No `shortName` on `StorageCluster`                                                  | `stc` (§3)                                                     | Additive. Not `sc`, which is `StorageClass`'s in core                                                                                                                                                                                                                                                                                                                     |
| `storage.simplyblock.io/cluster-finalizer`                                          | `storage.simplyblock.io/storagecluster-finalizer` (§4.5)       | Key rename, and the one change here that wedges rather than degrades. It is the only finalizer of the seven that ships whose name is not its kind's, so it is worth correcting, but an operator that reads only the new key leaves every object created by an older one in `Terminating` forever. Both spellings are read for a release, and the old one is removed after |
| Four declared, unused `status` fields                                               | Removed (§3.3)                                                 | Status removal, cheap while the group is at `v1alpha1`                                                                                                                                                                                                                                                                                                                    |
| `spec.action` as a plain `string`                                                   | `StorageClusterOpsAction` (§5.1)                               | Type only, the wire values change with the row below                                                                                                                                                                                                                                                                                                                      |
| Six lowercase, kebab-case action values                                             | PascalCase (§5.3)                                              | Spec rename of every value. `design-crd-model.md` §9.7 owns the deprecation window                                                                                                                                                                                                                                                                                        |
| `action: node-rolling-restart`                                                      | `action: RollingRestart` (§5.3)                                | Spec rename of an enum value                                                                                                                                                                                                                                                                                                                                              |
| `spec.nodeRollingRestart`                                                           | `spec.rollingRestart` (§5.3)                                   | Spec rename, with `NodeRollingRestartSpec` to `RollingRestartSpec`                                                                                                                                                                                                                                                                                                        |
| `status.nodeRollingRestartStatus`                                                   | `status.rollingRestart` (§7)                                   | Status rename. `nodes` and `nodeIndex` replace `pendingNodes` and `processedNodes`                                                                                                                                                                                                                                                                                        |
| `status.triggered`, `phaseTriggered`                                                | Removed (§6.2)                                                 | Status removal. The persisted position is the record                                                                                                                                                                                                                                                                                                                      |
| No step field, `Restart` and the walk improvising one                               | `status.step` on both (§5.3)                                   | The two improvised carriers, `status.message` and `nodePhase`, stop being control flow                                                                                                                                                                                                                                                                                    |
| No state machine behind any action                                                  | One declared graph per action (§5.3)                           | The largest piece of work here. Every side effect moves into a step                                                                                                                                                                                                                                                                                                       |
| No `Aborted` phase                                                                  | Present (§5.2)                                                 | Additive                                                                                                                                                                                                                                                                                                                                                                  |
| No deadline on any step                                                             | `status.step.deadline` (§5.3)                                  | Additive, and what makes a stalled operation detectable                                                                                                                                                                                                                                                                                                                   |
| Polling every backend read                                                          | The control-plane streams (§4.4)                               | Depends on `design-sse-push-notifications.md`, on the `sse` branch                                                                                                                                                                                                                                                                                                        |
| One success event per action, no others                                             | The reasons in §10.1                                           | Additive, and five reasons replace five                                                                                                                                                                                                                                                                                                                                   |
| No metric on either kind                                                            | The nine metrics of §10.2                                      | New infrastructure                                                                                                                                                                                                                                                                                                                                                        |

Every spec row is breaking, because a renamed spec field is silently ignored on an
object that still sets the old name. Every status row is not, because the operator
is the only writer.

**The `minHugePagesSize` rename does not reach as far as it looks.** The CRD field
is the operator's to rename behind a deprecation window, but the two names it feeds
are not: `hugepages_mem` belongs to the control-plane API, and
`MAX_HUGE_PAGES_SIZE` is the variable each storage node's configuration is read
with, so changing that one is a coordinated change with the node image. The rename
therefore lands with the field emitting `MAX_HUGE_PAGES_SIZE` unchanged, and the
mismatch becomes a documented boundary rather than a bug. Four call sites move with
the field: `controllers/cluster/storagecluster_controller.go`,
`controllers/node/pernodeconfig.go`, three unit tests, and
`helm-charts/.../operator_customresources.yaml`. The `kms` regrouping is
Kubernetes-side only for the same reason in reverse: the control plane keeps
`hashicorp_vault_settings.base_url` and the request builder maps to it from
wherever the field sits.

**The phase row is the one a reader should not skim.** The registered
`status.phase` is not a lifecycle report: it takes the value `"creation"` while a
creation is claimed and is empty at every other time, including on a fully running
cluster. Anyone who assumes it means what `phase` means on every other kind in the
group reads an empty string on a healthy cluster and concludes something is wrong.

The rows above are audited by
`.claude/skills/api-design/scripts/check-crds.py --kind StorageCluster` and
`--kind StorageClusterOps` where a checker covers them. The step machine, the
deadline, and the `Aborted` phase are conventions of
[`design-crd-model.md`](design-crd-model.md) §3.1 that no checker covers.

---

## 13. Open Questions

**Q1: What `Expand` is, before what parameters it takes.** The action posts to
`/expand` with no body. The narrow question is whether expansion needs a target node
list or a capacity argument, and therefore whether `StorageClusterOpsSpec` needs a typed
`expand` block. It sits inside a wider one that has to be answered first: whether
expansion is an operation at all.

**The constraint any answer has to satisfy is batching.** Adding capacity is several
`StorageNode` objects created, each provisioning independently, and only then a cluster
that spreads itself over them. Somebody adding three nodes wants one expansion after the
third, not one after each: an expansion per node redistributes twice for nothing and
takes the cluster's lock three times. So whatever expansion is, it cannot be a
consequence the operator draws on its own from a node reaching `Ready`.

**That is an argument for an explicit operation, and the alternatives are worth stating
against it.** Expansion could be implicit, triggered when a node joins, which is the
reading batching rules out. It could be desired state, a node count on the cluster whose
increase the operator acts on, which reads well until the question is what a *decrease*
means and whether re-applying the same count expands again, since a one-shot
redistribution is not a thing a spec field describes. Or it could be what it is here:
a one-shot operation somebody issues once the nodes they wanted are all present, which
is the shape [`design-crd-model.md`](design-crd-model.md) §3 gives an `Ops` kind and the
reason this design has expansion as an action rather than a field.

**What is genuinely unresolved is the pairing with rebalancing.** An expansion that
spreads the cluster over new nodes and a rebalance that evens out what is already placed
are two operations somebody performs together, and nothing here says whether `Expand`
implies the second, takes a parameter asking for it, or leaves it to the auto-rebalancer
to notice. The last is what happens today by default, which means the answer is
currently "whenever the rebalancer next runs" rather than a decision this design made.

**Q2: Whether this kind adopts the shared retention setting.** Nothing deletes a
terminal `StorageClusterOps`, so the audit record grows without bound.
[`design-persistentvolumeops.md`](design-persistentvolumeops.md) §11.2 specifies the
mechanism, which is `StorageCluster.spec.opsRetention`, a duration applied to terminal
objects only, and states it as one setting covering the operations of every kind. What is open is whether this kind
takes it, which is a decision about adding one field here and one predicate to this
controller rather than about designing anything.

---

## Appendix A: `storagecluster_types.go`

The type as it is to be written. Everything the sections above show in Go is an
excerpt of this appendix, quoted where an argument turns on one field, and this
is the only place any type appears whole. The doc comments here are the ones the
shipped file carries, so the reasoning that belongs to the design stays in the
body and does not become a comment nobody can act on.

`.claude/skills/api-design/scripts/check-crds.py --design` audits this appendix
against the same conventions it audits the shipped types against.

```go
// StorageClusterPhase is where the operator has got to with this cluster. The
// first two values are the operator's own creation path; the rest are its reading
// of the lifecycle status.status carries in the control plane's own spelling.
// +kubebuilder:validation:Enum=Pending;Creating;Online;Degraded;Unavailable;Suspended
type StorageClusterPhase string

const (
	// Pending: the object exists and nothing has been claimed for it yet.
	StorageClusterPhasePending StorageClusterPhase = "Pending"

	// Creating: the creation machine of §4.2 is running.
	StorageClusterPhaseCreating StorageClusterPhase = "Creating"

	// Online: the control plane reports the cluster active and serving.
	StorageClusterPhaseOnline StorageClusterPhase = "Online"

	// Degraded: serving, with less than the redundancy it was built for.
	StorageClusterPhaseDegraded StorageClusterPhase = "Degraded"

	// Unavailable: not serving, and not because anybody asked.
	StorageClusterPhaseUnavailable StorageClusterPhase = "Unavailable"

	// Suspended: shut down deliberately, which is where Shutdown leaves it (§6.3).
	StorageClusterPhaseSuspended StorageClusterPhase = "Suspended"
)

// StorageClusterStep is one step of the creation path. There is one graph rather
// than a MultiConfig, because an entity has no spec.action to key one on.
// +kubebuilder:validation:Enum=Claiming;CheckingControlPlane;ResolvingConfig;Creating;Adopting;Persisting
type StorageClusterStep string

const (
	StorageClusterStepClaiming             StorageClusterStep = "Claiming"
	StorageClusterStepCheckingControlPlane StorageClusterStep = "CheckingControlPlane"
	StorageClusterStepResolvingConfig      StorageClusterStep = "ResolvingConfig"
	StorageClusterStepCreating             StorageClusterStep = "Creating"
	StorageClusterStepAdopting             StorageClusterStep = "Adopting"
	StorageClusterStepPersisting           StorageClusterStep = "Persisting"
)


// StripeSpec is the erasure-coding layout: how many data chunks a stripe carries
// and how many parity chunks protect them.
type StripeSpec struct {
	// DataChunks is the number of data chunks per stripe (ndcs).
	// +kubebuilder:validation:Minimum=1
	// +optional
	DataChunks *int32 `json:"dataChunks,omitempty"`

	// ParityChunks is the number of parity chunks per stripe (npcs), and
	// therefore how many chunk losses a stripe survives.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ParityChunks *int32 `json:"parityChunks,omitempty"`
}

// CapacityThresholdSpec is one capacity alarm level, as an absolute figure for
// used capacity and one for provisioned capacity.
type CapacityThresholdSpec struct {
	// Capacity is the used-capacity threshold.
	// +optional
	Capacity *int64 `json:"capacity,omitempty"`

	// ProvisionedCapacity is the provisioned-capacity threshold.
	// +optional
	ProvisionedCapacity *int64 `json:"provisionedCapacity,omitempty"`
}

// VaultKMS configures the HashiCorp Vault key store.
type VaultKMS struct {
	// BaseURL is the Vault endpoint, for example https://vault.example.com:8200.
	// Rejected unless it resolves to an external address.
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?(/.*)?$`
	// +kubebuilder:validation:Required
	BaseURL string `json:"baseURL"`
}

// KMSSpec selects where the cluster stores volume encryption keys. It is a block
// with one member per provider so that the providers are siblings, which is what
// makes choosing between them expressible.
type KMSSpec struct {
	// Vault stores keys in HashiCorp Vault.
	// +optional
	Vault *VaultKMS `json:"vault,omitempty"`
}

// BackupStoreSpec is the S3 location a cluster's backups live in, and the
// credentials to reach it. It is a location and nothing else: how backups are taken
// and what they contain are the control plane's, and this block only says where they
// go. It is both the target copies are written to and the inventory the operator
// walks to produce StorageBackup objects (design-storagebackup.md §5.1), because
// those are the same bucket.
//
// The whole block is mutable. A cluster can be created without a store and given
// one later, and what changes when it changes is which backups have objects, since
// the object set is derived from the location rather than accumulated.
type BackupStoreSpec struct {
	// Endpoint is the S3 endpoint, for example https://s3.example.com.
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?(/.*)?$`
	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`

	// Bucket is the bucket backups are written to and read from.
	// +kubebuilder:validation:Required
	Bucket string `json:"bucket"`

	// Prefix narrows the store to one key prefix, so that several clusters can
	// share a bucket without each walking the others' backups.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Region is the bucket's region, for endpoints that do not imply one.
	// +optional
	Region string `json:"region,omitempty"`

	// CredentialsSecretRef names the Secret holding the access key and the secret
	// key. It is a reference rather than the values, because a spec is readable by
	// anybody who can read the object.
	// +kubebuilder:validation:Required
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

// StorageClusterSpec is the desired state of one simplyblock backend cluster.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.kms) || self.kms == oldSelf.kms",message="kms is immutable once set"
type StorageClusterSpec struct {
	// MaxSubsystemCount is the maximum number of NVMe-oF subsystems per storage
	// node. It is what a node is stamped with at creation, into
	// StorageNode.spec.config.sizing, rather than a value every node reads.
	// Required: it sizes huge pages, and a node that receives no value fails
	// config generation outright rather than falling back to a default.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=75
	MaxSubsystemCount *int32 `json:"maxSubsystemCount"`

	// VCPUCount is the number of vCPUs allocated to SPDK on each storage node, as
	// an explicit core count rather than a percentage, and is stamped onto a node
	// at creation like MaxSubsystemCount. Required: the core layout it produces
	// must match across the cluster in steady state, so it is stated rather than
	// left to a per-node heuristic.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=6
	VCPUCount *int32 `json:"vcpuCount"`

	// MinHugePagesSize is the smallest huge-page allocation each storage node
	// makes, as a size string ("100G", "1T"; a bare number is gigabytes). It is a
	// floor and not a limit: the effective allocation is the larger of this value
	// and the minimum the node's device and subsystem count requires. Omitted,
	// the computed minimum is used.
	// +optional
	MinHugePagesSize string `json:"minHugePagesSize,omitempty"`

	// Stripe is the erasure-coding layout every volume in the cluster is written
	// with. It describes on-disk layout, so it cannot change under a live
	// cluster.
	// +optional
	// +k8s:immutable
	Stripe *StripeSpec `json:"stripe,omitempty"`

	// FabricType is the storage fabric the cluster serves volumes over. It
	// describes on-wire layout, so it cannot change under a live cluster.
	// +optional
	// +k8s:immutable
	FabricType string `json:"fabricType,omitempty"`

	// ClientDataIfname is the network interface clients reach the data plane on.
	// +optional
	ClientDataIfname string `json:"clientDataIfname,omitempty"`

	// NvmfBasePort is the base of the NVMe-oF port range every node binds.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	// +optional
	// +k8s:immutable
	NvmfBasePort *int32 `json:"nvmfBasePort,omitempty"`

	// RpcBasePort is the base of the RPC port range every node binds.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	// +optional
	// +k8s:immutable
	RpcBasePort *int32 `json:"rpcBasePort,omitempty"`

	// SnodeApiPort is the port each node's storage-node API listens on.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	// +optional
	// +k8s:immutable
	SnodeApiPort *int32 `json:"snodeApiPort,omitempty"`

	// EnableFailureDomains opts the cluster into failure-domain mode, where every
	// node must declare a fault group so the control plane can spread
	// erasure-coding chunks across independent ones.
	// +optional
	// +k8s:immutable
	EnableFailureDomains *bool `json:"enableFailureDomains,omitempty"`

	// EnableNodeAffinity selects affinity-based placement for storage components.
	// +optional
	// +k8s:immutable
	EnableNodeAffinity *bool `json:"enableNodeAffinity,omitempty"`

	// KMS selects where the cluster stores volume encryption keys. Switching
	// providers on a live cluster is at least as unsupportable as changing one
	// provider's endpoint, which is why the whole block is immutable rather than
	// its members.
	// +optional
	KMS *KMSSpec `json:"kms,omitempty"`

	// WarningThreshold is the capacity level at which the cluster warns.
	// +optional
	WarningThreshold *CapacityThresholdSpec `json:"warningThreshold,omitempty"`

	// CriticalThreshold is the capacity level at which the cluster alarms.
	// +optional
	CriticalThreshold *CapacityThresholdSpec `json:"criticalThreshold,omitempty"`

	// MaxConcurrentWorkerRestarts caps how many Kubernetes workers the operator
	// may drain and restart at once. The effective value is the smaller of this
	// and status.maxFaultTolerance, published as
	// status.maxConcurrentWorkerRestarts so tooling reads one authoritative
	// number rather than recomputing it.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	MaxConcurrentWorkerRestarts *int32 `json:"maxConcurrentWorkerRestarts,omitempty"`

	// Backup is the S3 location this cluster's backups live in, and it is both the
	// target copies are written to and the inventory the operator walks to produce
	// StorageBackup objects (design-storagebackup.md §5.1). Mutable, and the type
	// is above.
	// +optional
	Backup *BackupStoreSpec `json:"backup,omitempty"`

	// StorageNodes is the Kubernetes workload every storage node in this cluster
	// runs as, and the objects the cluster owns to run it.
	// +optional
	StorageNodes *StorageNodesSpec `json:"storageNodes,omitempty"`

	// EnableDataRealignment turns on the post-migration data realignment. It is a
	// field of the spec rather than of the block it governs, because
	// volumeMigrationSettings.dataRealignment.enableDataRealignment says the same
	// word twice (§3.1). There is no EnableVolumeMigration beside it: migration
	// cannot be turned off, since a drain, a rebalance, and a device replacement
	// are all performed by moving volumes.
	// +optional
	EnableDataRealignment *bool `json:"enableDataRealignment,omitempty"`

	// EnableVolumeAutoPlacement turns on automatic, latency-driven rebalancing.
	// +optional
	EnableVolumeAutoPlacement *bool `json:"enableVolumeAutoPlacement,omitempty"`

	// VolumeMigrationSettings controls how volume migration and the
	// post-migration realignment behave, not whether they happen. It is separate
	// from volumeAutoPlacement because realignment applies to every volume move,
	// whatever asked for it.
	// +optional
	VolumeMigrationSettings *VolumeMigrationSettings `json:"volumeMigrationSettings,omitempty"`

	// VolumeAutoPlacement configures automatic, latency-driven rebalancing.
	// +optional
	VolumeAutoPlacement *VolumeAutoPlacementSettings `json:"volumeAutoPlacement,omitempty"`
}

// ClusterTask is one asynchronous job the control plane is running, as of the last
// stream frame. It is a window rather than a record: a task that reaches a terminal
// outcome leaves status.tasks, and what remains of it is an event (§3.4).
type ClusterTask struct {
	// ID is the control plane's identifier, and it is how a CancelTask operation
	// names the task (§6.3). A position in the list is not an identity, because
	// the next frame may order it differently.
	// +kubebuilder:validation:Required
	ID string `json:"id"`

	// Type is what kind of job it is, in the control plane's own spelling for the
	// reason design-crd-model.md §7.8 gives: the value is the backend's rather
	// than this group's.
	// +optional
	Type string `json:"type,omitempty"`

	// Status is the control plane's own status string, and it is why this entry
	// carries no phase: the operator adds nothing to what the backend reports.
	// +optional
	Status string `json:"status,omitempty"`

	// Progress is how far along the task is, where the control plane reports it.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	Progress *int32 `json:"progress,omitempty"`

	// CreatedAt is when the control plane started the task, which is what the
	// list is ordered by, newest first.
	// +optional
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`
}

// StorageClusterStatus is the observed state of one backend cluster.
type StorageClusterStatus struct {
	// Phase is the operator's own view of this cluster, and the field its
	// creation path branches on.
	// +optional
	Phase StorageClusterPhase `json:"phase,omitempty"`

	// Step is the position of the creation machine, as the shared
	// statemachine.KubeSnapshot (design-crd-model.md §3.1). The rule is what an
	// Enum marker would do if a marker could reach a field of a shared type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Claiming','CheckingControlPlane','ResolvingConfig','Creating','Adopting','Persisting']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// UUID is the backend cluster UUID. Empty means the cluster has not been
	// created or adopted, and non-empty means steady state.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// ClusterName is the resolved backend name.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`

	// NQN is the cluster subsystem qualified name.
	// +optional
	NQN string `json:"nqn,omitempty"`

	// ErasureCodingScheme is the active layout, rendered as "<ndcs>x<npcs>".
	// +optional
	ErasureCodingScheme string `json:"erasureCodingScheme,omitempty"`

	// Status is the lifecycle the control plane reports, and its values are the
	// control plane's, which is why they are neither PascalCase nor constrained
	// by an Enum here.
	// +optional
	Status string `json:"status,omitempty"`

	// Configured records that initial setup completed.
	// +optional
	Configured bool `json:"configured,omitempty"`

	// Rebalancing is the control plane's report that a rebalance is in progress,
	// which is one of the two conditions that hold a node operation.
	// +optional
	Rebalancing *bool `json:"rebalancing,omitempty"`

	// MaxFaultTolerance is how many nodes may be simultaneously offline without
	// violating redundancy, as the control plane reports it.
	// +optional
	MaxFaultTolerance *int32 `json:"maxFaultTolerance,omitempty"`

	// MaxConcurrentWorkerRestarts is the effective limit, the smaller of
	// spec.maxConcurrentWorkerRestarts and maxFaultTolerance.
	// +optional
	MaxConcurrentWorkerRestarts *int32 `json:"maxConcurrentWorkerRestarts,omitempty"`

	// VolumeMoveGeneration is incremented by every migration reaching Completed
	// and by nothing else, so it only grows.
	// +optional
	VolumeMoveGeneration *int64 `json:"volumeMoveGeneration,omitempty"`

	// RealignedGeneration is the generation the last successfully requested
	// realignment covers. A realignment is outstanding while volumeMoveGeneration
	// exceeds it. The value recorded is the one read before the request was sent,
	// which is what that realignment can actually account for.
	// +optional
	RealignedGeneration *int64 `json:"realignedGeneration,omitempty"`

	// LastDataRealignmentAt is when a realignment was last requested, and it is
	// what the configured interval spaces requests against.
	// +optional
	LastDataRealignmentAt *metav1.Time `json:"lastDataRealignmentAt,omitempty"`

	// Tasks are the control plane's running and pending jobs, newest first and
	// capped at twenty (§3.4). Completed and canceled tasks are not here: they
	// leave the list and become events, so the length tracks concurrency rather
	// than history.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	Tasks []ClusterTask `json:"tasks,omitempty"`

	// ActiveOpsRef names the StorageClusterOps currently allowed to operate on
	// this cluster. Empty when none is running.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// RebalancingMetrics is written by the auto-rebalancer each evaluation cycle.
	// +optional
	RebalancingMetrics *RebalancingMetrics `json:"rebalancingMetrics,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the cluster moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=stc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// +kubebuilder:printcolumn:name="EC",type=string,JSONPath=".status.erasureCodingScheme"
// +kubebuilder:printcolumn:name="FTT",type=integer,JSONPath=".status.maxFaultTolerance",priority=1
// +kubebuilder:printcolumn:name="UUID",type=string,JSONPath=".status.uuid",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageCluster is one simplyblock backend cluster. It owns the storage nodes
// beneath it, the pools carved out of it, and the Kubernetes workload its nodes
// run as.
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

Three types the spec references are declared where the design that owns them
specifies them, and are not restated here. `VolumeMigrationSettings`,
`VolumeAutoPlacementSettings`,
and `RebalancingMetrics` belong to
[`design-auto-rebalancing.md`](../design-auto-rebalancing.md). The three bare `enabled`
fields those types carry today do not survive: two become the spec fields above and the
third is removed (§3.1).
`StorageNodesSpec` belongs to
[`design-storagenode.md`](design-storagenode.md), Appendix C.

---

## Appendix B: `storageclusterops_types.go`

```go
// StorageClusterOpsAction is the operation a StorageClusterOps performs. Values
// are PascalCase, which is the casing every enum this API group defines carries
// (design-crd-model.md §7.8).
// +kubebuilder:validation:Enum=Activate;Expand;Shutdown;Start;Restart;RollingRestart;CancelTask
type StorageClusterOpsAction string

const (
	StorageClusterOpsActionActivate       StorageClusterOpsAction = "Activate"
	StorageClusterOpsActionExpand         StorageClusterOpsAction = "Expand"
	StorageClusterOpsActionShutdown       StorageClusterOpsAction = "Shutdown"
	StorageClusterOpsActionStart          StorageClusterOpsAction = "Start"
	StorageClusterOpsActionRestart        StorageClusterOpsAction = "Restart"
	StorageClusterOpsActionRollingRestart StorageClusterOpsAction = "RollingRestart"

	// CancelTask is the one action whose target is inside the cluster: it names a
	// task of status.tasks by its control-plane ID (§6.3).
	StorageClusterOpsActionCancelTask StorageClusterOpsAction = "CancelTask"
)

// StorageClusterOpsPhase is the operation's own progress. Aborted is terminal
// and distinct from Failed, because a canceled operation did not go wrong.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StorageClusterOpsPhase string

const (
	StorageClusterOpsPhasePending   StorageClusterOpsPhase = "Pending"
	StorageClusterOpsPhaseRunning   StorageClusterOpsPhase = "Running"
	StorageClusterOpsPhaseSucceeded StorageClusterOpsPhase = "Succeeded"
	StorageClusterOpsPhaseFailed    StorageClusterOpsPhase = "Failed"
	StorageClusterOpsPhaseAborted   StorageClusterOpsPhase = "Aborted"
)

// StorageClusterOpsStep is one step of a running cluster operation. The enum is
// the union of every action's steps; which steps belong to which action is
// declared by the graph rather than by this type.
// +kubebuilder:validation:Enum=Requesting;Awaiting;ShuttingDown;Starting;CheckingPeers;ShuttingDownNode;RefreshingPod;AwaitingPod;RestartingNode;Rebalancing
type StorageClusterOpsStep string

const (
	// Activate, Expand, Shutdown, and Start.
	StorageClusterOpsStepRequesting StorageClusterOpsStep = "Requesting"
	StorageClusterOpsStepAwaiting   StorageClusterOpsStep = "Awaiting"

	// Restart.
	StorageClusterOpsStepShuttingDown StorageClusterOpsStep = "ShuttingDown"
	StorageClusterOpsStepStarting     StorageClusterOpsStep = "Starting"

	// RollingRestart, one machine lifetime per node.
	StorageClusterOpsStepCheckingPeers    StorageClusterOpsStep = "CheckingPeers"
	StorageClusterOpsStepShuttingDownNode StorageClusterOpsStep = "ShuttingDownNode"
	StorageClusterOpsStepRefreshingPod    StorageClusterOpsStep = "RefreshingPod"
	StorageClusterOpsStepAwaitingPod      StorageClusterOpsStep = "AwaitingPod"
	StorageClusterOpsStepRestartingNode   StorageClusterOpsStep = "RestartingNode"
	StorageClusterOpsStepRebalancing      StorageClusterOpsStep = "Rebalancing"
)


// RollingRestartSpec parameterizes the RollingRestart action.
type RollingRestartSpec struct {
	// RefreshSNodeAPI restarts each node's storage-node DaemonSet pod between its
	// shutdown and its restart, so the latest image is running when the node
	// returns.
	// +optional
	RefreshSNodeAPI bool `json:"refreshSNodeAPI,omitempty"`
}

// CancelTaskSpec parameterizes the CancelTask action.
type CancelTaskSpec struct {
	// TaskID names the entry of StorageCluster.status.tasks to cancel, by the
	// control plane's identifier for it rather than by its position in the list
	// (§3.4).
	// +kubebuilder:validation:Required
	// +k8s:immutable
	TaskID string `json:"taskID"`
}

// StorageClusterOpsSpec is one operation to perform against one StorageCluster.
type StorageClusterOpsSpec struct {
	// ClusterRef names the StorageCluster this operation acts on. The operation
	// never owns its target, because deleting the record of an operation must not
	// delete the cluster it operated on.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action StorageClusterOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind.
	// Whether an abort is expressible from the current step is declared by that
	// action's graph rather than checked here.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// CancelTask parameterizes action CancelTask and is ignored by the others.
	// +optional
	CancelTask *CancelTaskSpec `json:"cancelTask,omitempty"`

	// RollingRestart parameterizes action RollingRestart and is ignored by the
	// others.
	// +optional
	RollingRestart *RollingRestartSpec `json:"rollingRestart,omitempty"`
}

// RollingRestartStatus is the walk's position over the cluster's nodes. Where
// the machine has got to within the node currently being restarted is
// status.step.
type RollingRestartStatus struct {
	// Nodes is the ordered list of storage node UUIDs this action covers, written
	// once when the walk starts and not modified afterward, so a node added
	// mid-walk is not restarted and one removed mid-walk is skipped.
	// +optional
	Nodes []string `json:"nodes,omitempty"`

	// NodeIndex is the position in Nodes of the node being restarted. Advancing
	// the walk increments it, and the walk is complete when it reaches
	// len(Nodes).
	//
	// No omitempty: zero is a valid index, and a field that disappears at zero
	// makes "the first node" and "unset" the same wire value.
	// +kubebuilder:validation:Minimum=0
	NodeIndex int32 `json:"nodeIndex"`
}

// StorageClusterOpsStatus is the observed state of one cluster operation.
type StorageClusterOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase StorageClusterOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine, as the shared
	// statemachine.KubeSnapshot (design-crd-model.md §3.1). The rule is what an
	// Enum marker would do if a marker could reach a field of a shared type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Requesting','Awaiting','ShuttingDown','Starting','CheckingPeers','ShuttingDownNode','RefreshingPod','AwaitingPod','RestartingNode','Rebalancing']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// RollingRestart is the walk's position, set only for action RollingRestart.
	// +optional
	RollingRestart *RollingRestartStatus `json:"rollingRestart,omitempty"`

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
// +kubebuilder:resource:scope=Namespaced,shortName=scops
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageClusterOps is a single operation performed against one StorageCluster.
// It runs to a terminal phase and stays afterward as the audit record of what
// was done, to which cluster, with which parameters, and how it ended.
type StorageClusterOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageClusterOpsSpec   `json:"spec,omitempty"`
	Status StorageClusterOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageClusterOpsList contains a list of StorageClusterOps.
type StorageClusterOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageClusterOps `json:"items"`
}
```
