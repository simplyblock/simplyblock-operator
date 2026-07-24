# Design Document: Volume Placement at Creation Time

**Status:** Proposed

**Author:** Manohar Reddy &nbsp;·&nbsp; **Date:** 2026-07-23

**Related Issues:**

- [#216](https://github.com/simplyblock/simplyblock-operator/issues/216) — Automatically Select Volume Owner at Creation Time
- [#272](https://github.com/simplyblock/simplyblock-operator/issues/272) — Node-Affinity-Aware Placement
- [#308](https://github.com/simplyblock/simplyblock-operator/issues/308) — Auto-Placement of Volumes at Creation (umbrella issue)
- [#130](https://github.com/simplyblock/simplyblock-operator/issues/130) — Automatic Rebalancing (the load signal this design reuses)

---

## Table of Contents

- [Design Document: Volume Placement at Creation Time](#design-document-volume-placement-at-creation-time)
  - [Table of Contents](#table-of-contents)
  - [Overview](#overview)
  - [1. Background](#1-background)
  - [2. Goals and Non-Goals](#2-goals-and-non-goals)
    - [Goals](#goals)
    - [Non-Goals](#non-goals)
  - [3. The Unified Placement Algorithm](#3-the-unified-placement-algorithm)
  - [4. Tier 1: Node/Pod Affinity Placement](#4-tier-1-nodepod-affinity-placement)
    - [What it means](#what-it-means)
    - [How it's resolved: standard CSI topology, not a custom mechanism](#how-its-resolved-standard-csi-topology-not-a-custom-mechanism)
    - [Mechanism](#mechanism)
    - [Multi-instance tie-break](#multi-instance-tie-break)
    - [Works with nodeSelector, nodeAffinity, and podAffinity — not with `spec.nodeName`](#works-with-nodeselector-nodeaffinity-and-podaffinity--not-with-specnodename)
    - [Reusing `EnableNodeAffinity` as Tier 1's gate](#reusing-enablenodeaffinity-as-tier-1s-gate)
    - [Per-PVC `pod-affinity` annotation](#per-pvc-pod-affinity-annotation)
  - [5. Tier 2: Load-Aware Placement](#5-tier-2-load-aware-placement)
    - [5.1 Architecture Overview](#51-architecture-overview)
    - [5.2 Clones and Snapshot Restores Are Unaffected](#52-clones-and-snapshot-restores-are-unaffected)
    - [5.3 Node Selection Algorithm](#53-node-selection-algorithm)
    - [5.4 Webhook Handler](#54-webhook-handler)
  - [6. Load Threshold Gate](#6-load-threshold-gate)
  - [7. Configuration: Enable/Disable and Runtime Reconfigurability](#7-configuration-enabledisable-and-runtime-reconfigurability)
    - [Tier 1's gate](#tier-1s-gate)
    - [Per-PVC opt-out annotation](#per-pvc-opt-out-annotation)
  - [8. Data Model Changes](#8-data-model-changes)
    - [8.1 `operator/internal/webapi/rebalancing.go` — `StorageNodeInfo`](#81-operatorinternalwebapirebalancinggo--storagenodeinfo)
    - [8.2 `StorageCluster` CRD — one field addition (§6's load threshold)](#82-storagecluster-crd--one-field-addition-6s-load-threshold)
    - [8.3 `StorageNodeSet` controller — label extension (Tier 1, §4)](#83-storagenodeset-controller--label-extension-tier-1-4)
    - [8.4 `spdk-csi` — topology + resolution extension (Tier 1, §4)](#84-spdk-csi--topology--resolution-extension-tier-1-4)
    - [8.5 No other CRD changes](#85-no-other-crd-changes)
  - [9. Failure Modes and Fallback](#9-failure-modes-and-fallback)
  - [10. Observability](#10-observability)
    - [Kubernetes Events](#kubernetes-events)
    - [Prometheus Metrics](#prometheus-metrics)
  - [11. Testing Strategy](#11-testing-strategy)
    - [Unit Tests (Tier 2)](#unit-tests-tier-2)
    - [Unit Tests (Tier 1)](#unit-tests-tier-1)
    - [Regression](#regression)
    - [Manual / E2E (Tier 2)](#manual--e2e-tier-2)
    - [Manual / E2E (Tier 1)](#manual--e2e-tier-1)
    - [Further Coverage](#further-coverage)

---

## Overview

**What this is.** A new volume's primary storage node is decided by one
layered algorithm. In order, the first tier with something useful to say wins:

| Tier | What it does |
|---|---|
| **0 — Explicit pin** | `simplyblock.io/host-id` annotation already set (by a user, or by anything else) |
| **1 — Node/Pod affinity** | The consuming Pod's Kubernetes scheduling (`nodeSelector`, node affinity, **or pod affinity**) put it on a worker that also hosts a storage node → use that node |
| **2 — Load-aware** | No locality signal available → pick the least-loaded eligible node |
| **3 — Control-plane default** | None of the above fired → `sbcli`'s existing weighted-random pick |

**The core idea.** Tier 1 automatically co-locates a new volume with whichever
worker node the consuming Pod ends up scheduled to. It works identically
regardless of *how* the scheduler chose that node: a plain `nodeSelector`,
`nodeAffinity`, or **`podAffinity`** all resolve the same way, because Tier 1
only ever looks at the Pod's *final* resolved node, not the mechanism that got
it there. One thing does **not** work: setting `spec.nodeName` directly on the
Pod. That bypasses the Kubernetes scheduler entirely, which is what normally
sets the annotation `WaitForFirstConsumer` provisioning depends on — this
breaks *any* CSI driver under `WaitForFirstConsumer`, not just ours (see §4).

**How the tiers resolve:** Tier 0 and Tier 2 share one signal — the
`simplyblock.io/host-id` PVC annotation — and that annotation, whenever set,
is checked **before** Tier 1's topology lookup and wins if present. Tier 1
only fills in `host_id` when the annotation is still empty. So an explicit
pin is never silently overridden.

**Each automatic tier has its own gate:**

- **Tier 1** is gated by **two conditions, both required**: the cluster's
  **node-affinity** feature (`StorageCluster.Spec.EnableNodeAffinity`,
  `--enable-node-affinity` at cluster creation — the same flag that makes the
  SPDK data plane keep a volume's data local to its primary node), *and* the
  PVC's own `simplyblock.io/pod-affinity: "true"` annotation (§4). The
  cluster flag makes co-location possible at all; the per-PVC annotation is
  what a specific workload uses to actually request it — without it, that
  PVC doesn't get Tier 1 treatment even on a node-affinity-enabled cluster.
- **Tier 2** is gated by the cluster's **auto-rebalancing** feature
  (`StorageCluster.Spec.AutoRebalancing`) — it activates whenever a cluster
  has that configured, independent of node affinity.

A single volume can also opt out of both tiers on its own, regardless of
cluster-wide gating, via `simplyblock.io/disable-smart-placement: "true"`
on its PVC (§7).

---

## 1. Background

Volumes are currently placed by `sbcli`'s `_get_next_3_nodes()`
(`simplyblock_core/controllers/lvol_controller.py`), a weighted-random pick over
online storage nodes keyed **only** on subsystem count per node
(`constants.weights = {"lvol": 100}`). This ignores two things that matter for a
new volume's primary placement:

- **Load** — a node hosting few, but extremely hot, volumes is just as likely to
  receive the next volume as an idle one.
- **Locality** — if the pod that will consume the volume is scheduled (via
  standard Kubernetes
  [node/pod affinity](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/))
  onto a worker node that also runs a storage node, the volume should land
  there too, avoiding an unnecessary network hop on the data path.

Both gaps route through the same mechanism: `add_lvol_ha` accepts an explicit
`host_id_or_name` and skips its own weighted-random pick entirely when one is
supplied, and `spdk-csi` reads a `simplyblock.io/host-id` PVC annotation and
forwards it as `host_id` on `CreateVolume` — the same path the operator's own
benchmark-volume provisioner (`operator/internal/controller/benchmark_provisioner.go`)
already relies on, always setting `HostID` explicitly.

---

## 2. Goals and Non-Goals

### Goals

- Treat placement as a single layered decision — explicit pin, then locality
  (#272), then load (#216), then the existing control-plane default.
- Reuse the rebalancer's existing node-hotness signal (current Prometheus p99
  latency vs. per-node fio baseline, Issue #130) for the load tier.
- Resolve locality using standard Kubernetes CSI topology-aware provisioning.
  This must work no matter which standard Kubernetes mechanism 
  (`nodeSelector`, node affinity, pod affinity) the workload uses to choose its node.
  Tier 1 only ever reads the Pod's *resolved* node, never the scheduling rule itself.
- Inject every tier's decision via the existing, already-supported
  `simplyblock.io/host-id` PVC annotation contract, so the backend requires no
  changes to the creation path itself for any tier.
- An explicit user-supplied `host_id` annotation (Tier 0) always wins over
  every automatic tier below it, including Tier 1 — see §3.
- Tier 1 is gated by the cluster's existing node-affinity feature
  (`StorageCluster.Spec.EnableNodeAffinity`) — the same flag that makes the
  SPDK data plane actually honor locality (§4, §7). No new flag is introduced
  for this.
- Tier 2 is gated by the cluster's existing auto-rebalancing feature
  (`StorageCluster.Spec.AutoRebalancing`), independent of Tier 1/node
  affinity.
- Every configuration knob in this design — the per-tier gates, the load
  threshold — is a `StorageCluster` CR spec field, reconciled live. None of
  it is a command-line flag or environment variable that requires an
  operator restart to change.
- Degrade silently to today's behavior (`sbcli`'s weighted-random pick)
  whenever a tier has nothing to contribute — placement can only ever be
  *better or unchanged*, never worse or blocking.
- Avoid picking a node that is offline, unhealthy, or already at subsystem
  capacity.

### Non-Goals

- Changing `sbcli`'s fallback placement algorithm (`_get_next_3_nodes`) itself
  — it remains the final fallback, unmodified.
- Capacity-based placement (disk space utilization) — out of scope, same as
  the rebalancer (Issue #130 §2 Non-Goals).
- Cross-cluster placement.
- Per-block-size or per-volume-QoS-aware scoring (Phase 2 of Issue #130, not
  needed here).
- Changing rebalancing/migration behavior (Issue #130) or pinning semantics
  (`simplyblock.io/pinned-volume`) — this document is scoped to the
  **creation-time** decision only. In particular, Tier 1 does **not** keep a
  volume co-located if its consumer Pod is later rescheduled elsewhere:
  neither `VolumeMigration` (a manual CR requiring an explicit target node)
  nor `VolumeRebalancer` (driven purely by Prometheus latency deviation,
  with no Pod/Node awareness) reacts to a Pod moving to a different worker.
  Closing that gap would be new work — a controller watching Pod↔PVC↔Node
  relationships and creating `VolumeMigration` CRs to correct drift.
- True NUMA/socket-level affinity. A worker can host more than one storage
  node instance (multi-socket deployments); Tier 1 guarantees host-level
  co-location only — see §4's tie-break discussion.

---

## 3. The Unified Placement Algorithm

Every new volume's primary node is decided by evaluating these tiers in order;
the first one that produces an answer wins. Tier 0 and Tier 2 resolve first
(they share one signal, the `host-id` annotation) and Tier 1 only fills the
gap if that annotation is still empty when `spdk-csi`'s `createVolume` runs:

```
Tier 0 — Explicit pin            simplyblock.io/host-id already set by the user
                                  before placement runs. → use it. Nothing
                                  below (including Tier 1) ever overrides it.

Tier 2 — Load-aware placement    (Resolved earlier than Tier 1, chronologically
                                  — see "Why this spans two components" below.)
                                  No pin yet. Rank eligible nodes by current
                                  latency deviation (Issue #130's signal) and
                                  pick the coolest — but only when the
                                  imbalance is meaningful (§6 Load Threshold
                                  Gate); otherwise leave the annotation unset
                                  and let Tier 1 (or Tier 3) decide.

Tier 1 — Node/Pod affinity       Resolved inside spdk-csi's CreateVolume, once
                                  the consuming Pod's node is known. Only
                                  evaluated if the PVC has
                                  simplyblock.io/pod-affinity: "true" (§4).
                                  If so, and the host-id annotation is STILL
                                  empty at this point (no Tier 0 pin, and
                                  Tier 2 didn't fire or isn't enabled) and the
                                  Pod's resolved node hosts a co-located
                                  storage node → use that node. Otherwise
                                  leave whatever's already there (Tier 0 or
                                  Tier 2's pick) alone.

Tier 3 — Control-plane default   Annotation still empty after all of the above.
  (sbcli)                        sbcli's existing weighted-random pick
                                  (_get_next_3_nodes) runs unmodified.
```

Tier 1 is gated by `StorageCluster.Spec.EnableNodeAffinity` **and** the PVC's
own `simplyblock.io/pod-affinity` annotation — both required (§4, §7); Tier
2 is gated independently by `autoRebalancing`. When Tier 1's gate is off
(either half of it), evaluation falls straight through to Tier 2 or Tier 3;
when Tier 2's gate is off, straight through to Tier 1 or Tier 3. Tier 0 (an
explicit pin) is never gated — that's a property of the annotation itself,
not of any tier's logic.

A single volume can also opt out of both automatic tiers regardless of
cluster-wide gating, via a per-PVC annotation — see §7's per-PVC opt-out.


## 4. Tier 1: Node/Pod Affinity Placement

### What it means

Standard Kubernetes
[node affinity, pod affinity, and pod anti-affinity](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/)
(or a plain `nodeSelector`) let a workload's Pod spec constrain which worker
node the Kubernetes scheduler places it on. This tier makes a **new volume's
primary land on the storage node co-located with wherever the scheduler
actually puts the consuming Pod** — not a random node, and not (necessarily)
the coolest node, but the *local* one, so the data path never has to cross
the network to reach storage.

This only applies to `WaitForFirstConsumer` StorageClasses — that's the
binding mode that delays provisioning until a consumer Pod is scheduled,
which is a precondition for there being a "consumer's worker node" to resolve
against at all.

### How it's resolved: standard CSI topology, not a custom mechanism

This doesn't need a bespoke scheduling integration — Kubernetes CSI has a
generic mechanism for exactly this (topology-aware dynamic provisioning),
which `spdk-csi` uses for other purposes too. `NodeGetInfo`
(`pkg/spdk/nodeserver.go`) builds and reports `AccessibleTopology` for the CSI
node driver, sourced from labels on the k8s `Node` object
(`buildAccessibleTopology`):

- `topology.kubernetes.io/zone` / `.../region` — for multi-cluster
  zone/region-mapped StorageClasses.
- `simplyblock.io/pool.<name>: allowed` — one segment per pool a node is
  allowed to serve, for DHCHAP-restricted pools.
- **`simplyblock.io/storage-node-uuid.<clusterUUID>.<socketOrdinal>: <uuid>`
  — new, added by this feature** (see below).
- A `topology.simplyblock.io/hostname` fallback when none of the above is
  present, so `WaitForFirstConsumer` provisioning always has at least one
  topology key to work with.

The external-provisioner reads these segments off each `CSINode` and — for
`WaitForFirstConsumer` volumes — passes the *scheduled* Pod's node's segments
as `accessibility_requirements` on `CreateVolumeRequest`. `spdk-csi`'s
`createVolume` reads this back out via `coLocatedHostID`
(`pkg/spdk/controllerserver.go`).

### Mechanism

1. **Label workers with their co-located storage node(s).**
   `StorageNodeSetReconciler.labelWorkerNodes`
   (`operator/internal/controller/simplyblockstoragenodeset_controller.go`)
   labels each worker with
   `io.simplyblock.node-type: simplyblock-storage-plane-<clusterName>` — a
   cluster-level "this worker hosts the storage plane" label, not a per-node
   one — and, alongside it, one label per co-located storage-node instance:

   ```
   simplyblock.io/storage-node-uuid.<clusterUUID>.<socketOrdinal> = <storage-node UUID>
   ```

   sourced from each owned `StorageNode` CR's `Status.UUID` and
   `Spec.SocketIndex`. Stale entries (a slot whose storage node was removed or
   never came up) are cleaned up on every reconcile, not just accumulated.

   **The key deliberately excludes the UUID.** Kubernetes' `external-provisioner`
   caches the **set of topology keys** in the `CSINode` object at node-plugin
   registration time, and hard-errors `CreateVolume` the moment a live Node's
   label keys diverge from that cached set — topology *keys* must be a stable
   schema for a Node's lifetime, with only *values* expected to change. Keying
   by `<clusterUUID>.<socketOrdinal>` is stable (it only changes if the
   StorageNodeSet's socket layout is reconfigured, a rare admin action); the
   UUID goes in the **value**, which is read fresh on every request and can
   churn freely (e.g. when a storage node is replaced). This also
   cluster-scopes the key, so a worker hosting storage nodes from two
   different Simplyblock clusters can't have one cluster's socket slot
   collide with another's.

2. **Advertise the label as a topology segment.** `buildAccessibleTopology`
   forwards any Node label with the `simplyblock.io/storage-node-uuid.`
   prefix into CSI topology, symmetric with how it handles zone/region and
   `pool.<name>` labels.

3. **Resolve it in `createVolume`, gated on the PVC's own opt-in.**
   `createVolume` only calls `coLocatedHostID` at all when the PVC carries
   `simplyblock.io/pod-affinity: "true"` (see below) — without it, Tier 1 is
   skipped for that PVC regardless of what the cluster's topology labels say.
   When it does run, `coLocatedHostID` scans `accessibility_requirements`
   (`Preferred` first, then `Requisite` — `Preferred[0]` is the actual
   Pod-scheduled node per the CSI spec's ranking contract) for a segment whose
   key decodes to the current cluster, and fills `host_id` with it — but
   **only when the annotation-derived `host_id` is still empty** (§3's
   precedence correction).

### Multi-instance tie-break

A worker can host more than one storage-node instance (NUMA/multi-socket
deployments, `StorageNodeSetSpec.NodesPerSocket`/`SocketsToUse`). When more
than one segment matches the current cluster, `coLocatedHostID` picks
**uniformly at random** among the matching instances, so volumes routed to
that worker via Tier 1 spread across every storage node actually co-located
there instead of piling onto a single one. This is a host-level guarantee
only — true socket-level (NUMA) affinity isn't resolvable at `CreateVolume`
time, since kubelet's Topology Manager pins a Pod to a specific NUMA socket
only when its container actually starts, which is after the volume already
had to exist. The socket ordinal remains part of the topology key only for its
original purpose (a stable, cluster-scoped key for `CSINode` — see above); it
plays no role in selecting among candidates.

### Works with nodeSelector, nodeAffinity, and podAffinity — not with `spec.nodeName`

Tier 1 only reads the Pod's *final resolved node* out of
`accessibility_requirements` — it has no idea, and doesn't need to know,
which Kubernetes mechanism put the Pod there:

| Scheduling mechanism | Result |
|---|---|
| No constraint (Pod lands anywhere) | Does nothing when the resolved node hosts no storage node (e.g. control-plane node) — `host_id` left unset, `sbcli`'s own default placement applies |
| `nodeSelector: kubernetes.io/hostname: <worker>` | Volume's `host_id` = that worker's co-located storage-node UUID |
| `podAffinity` (`requiredDuringSchedulingIgnoredDuringExecution`, `topologyKey: kubernetes.io/hostname`, matching an anchor Pod pinned elsewhere) | Same result — Tier 1 doesn't care that the Pod's node was resolved indirectly via another Pod's label, only that it *was* resolved to a specific node |
| `spec.nodeName` set directly on the Pod | **Does not work — but not because of Tier 1.** Setting `nodeName` bypasses the Kubernetes scheduler entirely, so the scheduler's `VolumeBinding` plugin never runs and the `volume.kubernetes.io/selected-node` PVC annotation `WaitForFirstConsumer` depends on is never written. The PVC sits at `WaitForFirstConsumer` forever, `Provisioning` is never even attempted. This is a well-known upstream Kubernetes limitation ([kubernetes/kubernetes#89953](https://github.com/kubernetes/kubernetes/issues/89953)) that affects *every* CSI driver under `WaitForFirstConsumer`, not something specific to this feature or to `spdk-csi`. |

### Reusing `EnableNodeAffinity` as Tier 1's gate

`StorageClusterSpec.EnableNodeAffinity` **already exists** on the CRD
(`operator/api/v1alpha1/storagecluster_types.go`) and is **already wired up
end-to-end** — but it means something else entirely. It's forwarded to `sbcli`
as `enable_node_affinity` at cluster-creation time
(`simplyblockstoragecluster_controller.go` →
`utils.ClusterAddParams.EnableNodeAffinity` → `cluster_ops.py` →
`cluster.enable_node_affinity`), and consumed in `distr_controller.py`
(`build_cluster_map`) to set `ppln1`/`local_node_index` on an erasure-coded
distrib's cluster map — an **SPDK data-plane I/O-path locality optimization**
(prefer serving reads/writes from the local node's own devices before crossing
the network), entirely internal to how an *already-placed* volume's
erasure-coded chunks are served.

In `sbcli`'s `add_lvol_ha`
(`simplyblock_core/controllers/lvol_controller.py`), `host_id_or_name`
unconditionally becomes the volume's primary node
(`lvol.node_id = host_node.get_id()`, then `add_lvol_on_node`) — **regardless**
of `enable_node_affinity`. So `host_id` (from a manual pin, or from Tier 1)
places the volume's primary correctly on *any* cluster. What
`enable_node_affinity` additionally buys is the `ppln1` hint that makes the
distributed/erasure-coded placement algorithm *also* prefer keeping that
volume's redundant chunks local to the primary — the deeper half of the
locality benefit this whole feature exists for.

**Tier 1 reuses `EnableNodeAffinity` directly as its gate — no new flag.**
Even though `EnableNodeAffinity`'s original purpose is the SPDK data-plane
hint, it's the right signal for Tier 1 too: Tier 1 only exists to exploit that
same locality mechanism, so a cluster without it enabled has nothing to gain
from Tier 1 either. See §7 for exactly where this check happens (on the
*labeling* side, in the operator, not inside `spdk-csi`).

This is a cluster-wide precondition, not per-volume control — it decides
whether co-location is possible on this cluster *at all*, not which specific
PVCs should actually get it. That's what the next annotation is for.

### Per-PVC `pod-affinity` annotation

`EnableNodeAffinity` alone would make Tier 1 apply to *every* PVC on a
node-affinity-enabled cluster, whether or not that specific workload actually
wants its volume kept local. A PVC opts in explicitly instead:

```
simplyblock.io/pod-affinity: "true"
```

**Set by the workload's author** (or their Helm chart/deployment tooling) on
the PVC manifest itself. Both conditions are required for Tier 1 to actually
fire on a given PVC:

- The cluster has `StorageCluster.Spec.EnableNodeAffinity` set (§7) — gates
  whether the operator's `labelWorkerNodes` emits `storage-node-uuid` labels
  at all, on the *labeling* side.
- The PVC carries `simplyblock.io/pod-affinity: "true"` — gates whether
  `createVolume` actually calls `coLocatedHostID` for *this* PVC, on the
  *resolution* side, inside `spdk-csi` (§4 Mechanism, step 3).

Without the annotation, a PVC on a node-affinity-enabled cluster still falls
through to Tier 2/Tier 3 exactly as if the cluster-wide flag were off — Tier
1 never even attempts the topology lookup for it. This gives per-workload
control over which volumes want locality and which don't, independent of the
cluster-wide setting: some workloads on the same cluster can request
co-location while others participate in ordinary load-aware placement (Tier
2) without being pinned to wherever their Pod happens to land.

---

## 5. Tier 2: Load-Aware Placement

This tier's mutating webhook
(`operator/internal/webhook/simplyblock_volume_placement_injector.go`) is
described here in full, since it's the concrete mechanism the rest of this
document builds around. Unlike Tier 1, **Tier 2 is not gated by
`EnableNodeAffinity`** — picking the least-loaded node is a load-balancing
concern independent of whether the SPDK data plane's locality optimization is
active.

### 5.1 Architecture Overview

```
PVC created (user)
       │
       ▼
┌──────────────────────────────────────────────────────────────────────────┐
│         SimplyblockVolumePlacementInjector (mutating webhook)            │
│                                                                          │
│  1. Skip if simplyblock.io/host-id already set (explicit pin wins)      │
│  2. Resolve StorageClass → cluster_id / pool_name params                │
│  3. Resolve StorageCluster CR by Status.UUID == cluster_id              │
│  4. Skip if AutoRebalancing disabled or PrometheusURL unset             │
│  5. GET storage-nodes (webapi.Client) → filter online/healthy/          │
│     under-capacity                                                       │
│  6. autobalancing.StorageNodeSelector.SelectBestNode(...)               │
│     → lowest current-latency-deviation eligible node                   │
│  7. Patch PVC: simplyblock.io/host-id = <chosen node UUID>              │
└───────────────────────────────┬──────────────────────────────────────────┘
                                 │ admission response (mutating patch)
                                 ▼
                     PVC persisted with host-id annotation
                                 │
                                 ▼
┌──────────────────────────────────────────────────────────────────────────┐
│  external-provisioner → spdk-csi CreateVolume                            │
│  prepareCreateVolumeReq() re-fetches PVC live (fetchPVCAnnotations)      │
│  → reads simplyblock.io/host-id → CreateLVolData.HostID                 │
│  Tier 1 checks CSI topology, but only fills host_id if this read was    │
│  empty — an already-set value here (Tier 0 or Tier 2's pick) wins (§3)  │
└───────────────────────────────┬──────────────────────────────────────────┘
                                 │ HTTP
┌───────────────────────────────▼──────────────────────────────────────────┐
│  Simplyblock Backend: add_lvol_ha(host_id_or_name=<uuid>)                │
│  host_node set → _get_next_3_nodes() is NEVER called                    │
│  _resolve_lvol_subsystem() still enforces max_lvol as a hard backstop   │
└────────────────────────────────────────────────────────────────────────┘
```

**Why a mutating webhook, not a new backend/CSI call:** `spdk-csi`'s
`fetchPVCAnnotations` (`pkg/spdk/controllerserver.go`) performs a **live**
GET of the PVC object at `CreateVolume` time — it does not rely on CSI request
parameters cached earlier in the provisioning pipeline. A webhook that mutates
the PVC at admission time (before the external-provisioner sidecar even
notices the PVC) is therefore guaranteed to be visible by the time `spdk-csi`
reads it. This requires **zero changes to `spdk-csi`** for Tier 2 itself.

This mirrors the existing `SimplyblockRebalancerInjector` pod-mutating webhook
(`operator/internal/webhook/simplyblock_rebalancer_injector.go`), which already
follows the same pattern: resolve the owning `StorageCluster` from context,
check whether the relevant feature is enabled for that cluster, patch if so,
allow unconditionally (`failurePolicy=Ignore`) otherwise.

### 5.2 Clones and Snapshot Restores Are Unaffected

Clones (from another PVC or a VolumeSnapshot) must land on the same host as
their source — this webhook does not special-case that, and it doesn't need
to:

- In `spdk-csi`, `createVolume` (`pkg/spdk/controllerserver.go`) checks
  `req.GetVolumeContentSource()` **before** calling `prepareCreateVolumeReq`
  (the function that reads the `host-id` annotation). When the PVC has a data
  source, `handleVolumeContentSource` handles it via
  `CloneSnapshot`/`CloneVolume` and returns — `prepareCreateVolumeReq` is
  never reached, so a `host-id` annotation stamped by this webhook is never
  read for a clone/restore, and Tier 1's topology check never runs either.
- On the backend, `clone_lvol` (`simplyblock_core/controllers/lvol_controller.py`)
  always places the clone via `lvol.node_id` — the **source** volume's own
  node. There is no `host_id` parameter on the clone path at all.

So same-host clone placement is already guaranteed by the existing
CSI/backend architecture, independent of this webhook and independent of
Tier 1, for the same reason: the clone path never reaches
`prepareCreateVolumeReq`.

### 5.3 Node Selection Algorithm

Reuses the exact signal `autobalancing.StorageNodeSelector` computes for the
rebalancer (Issue #130 §5.2):

```
latencyDeviationPct(node) = (currentP99NS - baselineP99NS) / baselineP99NS × 100
```

Where `currentP99NS` is queried live from Prometheus
(`simplyblock_node_fio_write_latency_p99_ns`) and `baselineP99NS` is the
one-time fio baseline stored on the owning `StorageNodeSet` CR
(`Status.LatencyMetrics`).

**Entry point: `SelectBestNode`.** `pickColdTarget`
(`storage_node_selector.go`) already contains the core "pick the coolest node"
loop, gated by `MinHotColdDifferencePct` (a migration-specific "must be
meaningfully cooler than a given hot source" rule that doesn't apply to
placement). `SelectBestNode` extracts the ranking core into its own method:

```go
// SelectBestNode returns the least-loaded eligible node — the one with the
// lowest current latency deviation — across the given candidate pool. Unlike
// pickColdTarget, there is no source node to compare against and no
// MinHotColdDifferencePct gate: placement always wants the single best
// candidate, however small its lead over the second-best.
func (sns *StorageNodeSelector) SelectBestNode(
    ctx context.Context,
    cfg RebalancingConfig,
    eligible map[string]bool, // nodeUUID -> true
    inputs ...StorageNodeSelectorInput,
) (nodeUUID string, ok bool, err error)
```

Nodes with no latency data yet (no baseline measured, deviation = 0) rank as
the best possible candidates — consistent with how the rebalancer treats
unmeasured nodes as migration targets (Issue #130 §6 Step 5). §6 refines this
ranking further, to only override when the imbalance is meaningful.

**Eligibility filter, applied before ranking:**

| Filter | Source | Rationale |
|---|---|---|
| `status == "online"` | `webapi.StorageNodeInfo.Status` | Never place on an offline node |
| `health_check == true` | `webapi.StorageNodeInfo.Healthy` | Mirrors rebalancer target eligibility (Issue #130 §6 Step 5) |
| `Lvols < LvolsMax` | `webapi.StorageNodeInfo.Lvols` / `.LvolsMax` (§8) | Mirrors `sbcli`'s own `max_lvol` capacity gate (`_resolve_lvol_subsystem`) so we don't hand the backend a node it will immediately reject |

There is deliberately no "is this a secondary/replica-only node" filter.
`sbcli`'s `StorageNode.is_secondary_node` models a dedicated-replica-capacity
node type (one that's never handed a new primary, but can back an unlimited
number of other primaries' HA groups — see `get_secondary_nodes` in
`storage_node_ops.py`), but nothing in the current backend ever sets it to
`True`: `add_node` takes no such parameter, and no CLI or API endpoint (v1 or
v2) exposes it as input — only test fixtures construct it. Every real node is
`is_secondary_node == False` today, so the filter would be dead weight. If a
future `sbcli` release wires up a way to actually provision such nodes, this
filter should be reinstated.

The capacity check is an approximation of `sbcli`'s `count_lvol_subsystems`
(which counts distinct subsystems, not raw lvol count, since namespaced pools
share a subsystem across lvols) — it is slightly conservative but avoids the
common "clearly full node" case. `_resolve_lvol_subsystem`'s exact check
remains the authoritative backstop server-side regardless.

### 5.4 Webhook Handler

File: `operator/internal/webhook/simplyblock_volume_placement_injector.go`,
same shape as `simplyblock_rebalancer_injector.go`.

```go
// +kubebuilder:webhook:path=/mutate-v1-pvc-simplyblock-placement,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=persistentvolumeclaims,verbs=create,versions=v1,name=simplyblock-volume-placement-injector.simplyblock.io,admissionReviewVersions=v1

type SimplyblockVolumePlacementInjector struct {
    Client       client.Client
    APIClient    storageNodeLister   // narrow interface over *webapi.Client
    NodeSelector primaryNodeSelector // narrow interface over *autobalancing.StorageNodeSelector
}
```

**Handle flow:**

1. Decode the PVC. Allow unmodified if:
   - `simplyblock.io/host-id` (or deprecated `simplybk/host-id`) is already
     set — Tier 0, absolute (§3).
   - `pvc.Spec.StorageClassName` is unset, or the referenced `StorageClass`'s
     `Provisioner` isn't the simplyblock CSI driver, or
     `parameters["cluster_id"]` is empty.
2. Resolve the `StorageCluster` CR whose `Status.UUID == cluster_id` — same
   lookup pattern as `SimplyblockRebalancerInjector.resolveConfig`. Allow
   unmodified if not found, or if `Spec.AutoRebalancing` is nil/disabled, or
   `PrometheusURL` is unset — this is Tier 2's gate (§7), nothing more is
   needed.
3. Build `autobalancing.RebalancingConfig` via the existing
   `autobalancing.ResolveRebalancingConfig(spec)`.
4. `APIClient.GetStorageNodes(ctx, clusterUUID)` — same call
   `VolumeRebalancerReconciler` already makes (in-cluster service-account
   auth, no per-cluster secret).
5. Apply the eligibility filter (§5.3).
6. Apply the load threshold gate (§6) — skip if no eligible node clears it.
7. `NodeSelector.SelectBestNode(...)`. Allow unmodified if no eligible node.
8. Patch the PVC: `simplyblock.io/host-id = <chosen node UUID>`.

Any error at steps 2–7 (backend unreachable, Prometheus query failure, no
StorageNodeSet baseline yet) results in `admission.Allowed(...)` with no patch
— Tier 3 (sbcli's existing weighted-random pick) runs unmodified. This mirrors
`failurePolicy=Ignore` on the webhook registration itself: the feature can
never block volume provisioning.

**Registration** (`operator/cmd/main.go`, under the same `webhookReady` gate
as the pod-rebalancer webhook):

```go
mgr.GetWebhookServer().Register("/mutate-v1-pvc-simplyblock-placement",
    &webhook.Admission{Handler: &internalwebhook.SimplyblockVolumePlacementInjector{
        Client:       mgr.GetClient(),
        APIClient:    webapi.NewClient(),
        NodeSelector: autobalancing.NewStorageNodeSelector(mgr.GetClient()),
    }})
```

Both this webhook and the pre-existing pod-rebalancer-injector (#130) are
packaged into the Helm chart's `templates/simplyblock-operator-webhook.yaml`
(a `Service` + `MutatingWebhookConfiguration`, names matching
`internal/utils/constants.go` exactly) plus a `webhook-certs` emptyDir
volume/mount on the operator Deployment.

**RBAC:** no new PVC write RBAC is needed — the mutation happens via the
admission response patch, not a client-side `Update`.

```go
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets,verbs=get;list;watch
```

`storageclasses` and the two `storage.simplyblock.io` reads are already
granted to other controllers (`simplyblockpool_controller.go`,
`volumerebalancer_controller.go`) and land in the same aggregated ClusterRole.

---

## 6. Load Threshold Gate

Tier 2 should only override placement when the imbalance across eligible
nodes is meaningful — otherwise ranking by the smallest latency-deviation
difference risks reacting to noise. The exact threshold formula and its
`StorageCluster` field (name, semantics) are an open question.

---

## 7. Configuration: Enable/Disable and Runtime Reconfigurability

Every knob below is a `StorageCluster` CR spec field. None of it is a
command-line flag or environment variable on the operator Deployment — the
whole point is that an operator can turn any part of this on, off, or retune
it without a restart or rollout. (Contrast with `--latency-percentile`, an
existing *operator-wide* flag that does require a restart to change — that
pattern is explicitly the wrong one for anything in this design.) This falls
out of how the webhook works: `StorageCluster.Spec` is read fresh, with no
caching, on every single PVC admission request.

**Tier 2's configuration:**

```yaml
spec:
  autoRebalancing:
    enabled: true
    latencyBenchmarkEnabled: true
    prometheusURL: "http://prometheus.monitoring:9090"
```

`Spec.AutoRebalancing` is the same top-level field Issue #130 introduced for
the rebalancer. Tier 2 activates whenever a cluster has `enabled: true` here
— there is no separate flag for creation-time placement, distinct from the
rebalancer's own activation flag: both `SimplyblockVolumePlacementInjector`
and `VolumeRebalancerReconciler` gate on this exact same `Spec.AutoRebalancing.Enabled`
field, so a cluster can't opt into one without the other via this flag alone.
The distinct `MigrationEnabled` sub-field only controls whether the
rebalancer's periodic evaluation actually creates `VolumeMigration` CRs once
triggered (dry-run vs. not) — it has no effect on Tier 2.

### Tier 1's gate

No new CRD field is needed — Tier 1 reuses
`StorageCluster.Spec.EnableNodeAffinity` directly (§4).

**Mechanism:** `spdk-csi` has no visibility into `StorageCluster` CRs, so the
gate belongs on the operator side instead. `labelWorkerNodes` resolves the
owning `StorageCluster` CR elsewhere in the same reconcile
(`utils.ResolveClusterCR`) and only emits `simplyblock.io/storage-node-uuid.*`
labels when that CR's `Spec.EnableNodeAffinity` is true. If it's false, no
worker advertises a co-located storage node, Tier 1 has nothing to match, and
volumes fall through to Tier 2/Tier 3 — zero `spdk-csi` changes needed.

**Tier 0** (the manual annotation) has no equivalent hook: it's a raw
annotation `spdk-csi` reads directly, and `spdk-csi` has no CR-read capability
to check `EnableNodeAffinity`. The pragmatic default is to leave Tier 0
always-honored regardless of `EnableNodeAffinity`, since a manual pin already
implies the operator knows what they're doing.

This cluster-wide flag is only half of Tier 1's gate — the other half is the
per-PVC `simplyblock.io/pod-affinity` annotation (§4), which controls
whether an individual PVC actually gets Tier 1 treatment even when this flag
is on.

### Per-PVC opt-out annotation

Everything else in this section is cluster-wide. A single volume can also
opt out of **both** automatic tiers on its own, regardless of how the
cluster is configured, via a PVC annotation:

```
simplyblock.io/disable-smart-placement: "true"
```

This is a different thing from a manual `host-id` pin (Tier 0): a pin says
"put it exactly here"; this annotation says "don't guess for me, and I'm not
naming a node either — just let `sbcli`'s normal default placement run."

**Mechanism, one check per tier, no shared code needed:**

- **Tier 2's webhook** checks this annotation early, alongside its existing
  "`host-id` already set" check — if present, allow the PVC through
  unmodified, same as any other skip case (§9).
- **Tier 1** (`spdk-csi`'s `createVolume`) checks it via the same PVC
  annotation fetch it already uses for `host-id` and the QoS annotations
  (`prepareCreateVolumeReq`) — if present, skip the topology lookup entirely
  and leave `host_id` exactly as `prepareCreateVolumeReq` found it.

---

## 8. Data Model Changes

### 8.1 `operator/internal/webapi/rebalancing.go` — `StorageNodeInfo`

```go
type StorageNodeInfo struct {
    UUID       string `json:"id"`
    Status     string `json:"status"`
    Healthy    bool   `json:"health_check"`
    TotalBytes int64  `json:"total_capacity_bytes"`
    Lvols      int    `json:"lvols"`
    LvolsMax   int    `json:"lvols_max"`
}
```

`Lvols` / `LvolsMax` require **no backend change** — `StorageNodeDTO` in
`simplyblock_web/api/v2/_dtos.py` already serializes `lvols` and `lvols_max`
(`model.lvols`, `model.max_lvol`); the Go struct simply never mapped them
because the rebalancer never needed them.

### 8.2 `StorageCluster` CRD — one field addition (§6's load threshold)

- `Spec.AutoRebalancing.MinImbalancePct *int32` (name/semantics TBD, §6) — the
  only new CRD field in this design.

Tier 1's gate (§7) needs **no CRD change at all**: it reuses the existing
`Spec.EnableNodeAffinity` field; `labelWorkerNodes` checks that field before
emitting its labels.

### 8.3 `StorageNodeSet` controller — label extension (Tier 1, §4)

`labelWorkerNodes`
(`operator/internal/controller/simplyblockstoragenodeset_controller.go`)
labels each worker with `io.simplyblock.node-type` and, gated on
`StorageCluster.Spec.EnableNodeAffinity` (§7), with
`simplyblock.io/storage-node-uuid.<clusterUUID>.<socketOrdinal> = <uuid>` for
every co-located storage-node instance — reconciling additions, value updates
(UUID churn on node replacement), and removals (stale slots) on every pass.

### 8.4 `spdk-csi` — topology + resolution extension (Tier 1, §4)

- `buildAccessibleTopology` (`pkg/spdk/nodeserver.go`) forwards any Node label
  with the `storage-node-uuid.` prefix as a topology segment, symmetric with
  the existing zone/region/`pool.<name>` handling.
- `createVolume` (`pkg/spdk/controllerserver.go`) calls `coLocatedHostID` on
  `accessibility_requirements` **only when the PVC carries
  `simplyblock.io/pod-affinity: "true"`** (§4), and uses the result as
  `host_id` **only when the annotation-derived value is empty** (§3) — i.e. an
  explicit pin or Tier 2's computed pick always takes precedence. Reads the
  new annotation via the same PVC annotation fetch already used for
  `host-id`, the QoS annotations, and the opt-out annotation (§7) — no new
  RBAC needed.

### 8.5 No other CRD changes

Tier 2 reads existing fields only: `StorageCluster.Spec.AutoRebalancing`
(Issue #130 §4.1) and `StorageNodeSet.Status.LatencyMetrics` (Issue #130 §4.3).
Tier 1 reads one CRD field for its gate, `Spec.EnableNodeAffinity` (§7).

---

## 9. Failure Modes and Fallback

| Condition | Behavior |
|---|---|
| `simplyblock.io/host-id` already set on the PVC (Tier 0) | Skip Tier 2 and Tier 1 — explicit pin always wins (§3) |
| `StorageCluster.Spec.EnableNodeAffinity` is false or unset (§7) | Skip Tier 1 — operator never emits `storage-node-uuid` labels for that cluster's workers, so Tier 1 has nothing to match; falls through to whatever's in `host-id` (Tier 0/2) or Tier 3 |
| PVC lacks `simplyblock.io/pod-affinity: "true"` (§4) | Skip Tier 1 for this PVC specifically — `createVolume` never calls `coLocatedHostID`, even if the cluster has `EnableNodeAffinity` on and the Pod's worker hosts a co-located storage node; falls through to Tier 2/Tier 3 |
| StorageClass isn't simplyblock-provisioned, or has no `cluster_id` | Skip Tier 2 |
| `StorageCluster` not found for `cluster_id` | Skip Tier 2 (log) |
| `AutoRebalancing` nil/disabled or `PrometheusURL` unset for the cluster | Skip Tier 2 — cluster hasn't opted into the load signal |
| Backend API (`GetStorageNodes`) unreachable | Skip Tier 2 (log); `failurePolicy=Ignore` also protects at the webhook-server level |
| Prometheus unreachable / query error | Skip Tier 2 (log) |
| No eligible node, or none clears the load threshold (§6) | Skip Tier 2 (log) |
| Pool has `qos_host` set (`pool.has_qos()`) | Not special-cased — `add_lvol_ha` overrides any `host_id` with `pool.qos_host` regardless, so an injected annotation is harmless but ignored. Documented, not fixed. |
| Tier 1 finds no co-located node for this consumer (no matching topology segment, or the Pod landed on a worker with no storage node) | Falls back to whatever's in `host-id` already (Tier 0 or Tier 2's pick), then Tier 3 |
| Consuming Pod sets `spec.nodeName` directly, bypassing the scheduler | `WaitForFirstConsumer` never resolves at all — PVC stuck, `Provisioning` never fires. Not a Tier 1 failure mode specifically; affects any `WaitForFirstConsumer` StorageClass regardless of CSI driver (§4). |

In every skip case the PVC ends up on `sbcli`'s existing weighted-random
placement (Tier 3, `_get_next_3_nodes`), unmodified — this design can only
ever make placement *better or unchanged*, never worse or blocking.

---

## 10. Observability

### Kubernetes Events

| Event | Type | Reason |
|---|---|---|
| Primary node selected for new PVC (any tier) | `Normal` | `PrimaryNodeSelected` |
| Selection skipped — no signal available | (none; logged only, high frequency expected) | — |

### Prometheus Metrics

| Metric | Labels | Description |
|---|---|---|
| `simplyblock_placement_decisions_total` | `cluster_uuid`, `tier` (`affinity`\|`load`\|`default`), `result` (`selected`\|`skipped`) | Count of placement decisions by tier and outcome |
| `simplyblock_placement_selected_node_deviation_pct` | `cluster_uuid`, `node_uuid` | Latency deviation of the node chosen by Tier 2, at selection time |

---

## 11. Testing Strategy

### Unit Tests (Tier 2)

Mirroring `simplyblock_rebalancer_injector_test.go`, with a fake
`client.Client`, fixture `StorageCluster`/`StorageNodeSet` CRs, and a stubbed
`webapi.Client` + Prometheus response:

- Annotation already set → PVC unmodified.
- StorageClass missing / not simplyblock-provisioned → PVC unmodified.
- `AutoRebalancing` disabled or `PrometheusURL` unset → PVC unmodified.
- Multiple eligible nodes with different deviations → lowest-deviation node
  chosen.
- Offline / unhealthy / at-capacity nodes excluded from candidates.
- No eligible node → PVC unmodified (no error surfaced to the CO).
- Backend or Prometheus error → PVC unmodified, error logged, request still
  `Allowed`.

### Unit Tests (Tier 1)

- `operator`: `TestStorageNodeSetLabelingHelpers` — single co-located UUID
  labeled; multiple sockets on one worker each get their own label; a slot's
  UUID value is updated in place when its storage node is replaced (the
  key stays stable — this is the regression test for the bug in §4); a slot
  label is removed once its `StorageNode` CR no longer exists.
- `csi-driver`: `TestCoLocatedHostID` — nil/no-match, `Preferred` takes
  precedence over `Requisite`, a segment scoped to a different cluster is
  skipped, a malformed ordinal value is ignored, and multiple sockets on one
  worker resolve to one of the valid co-located candidates — asserted over
  many trials to also confirm it isn't always the same candidate.
- `csi-driver`: `createVolume`/`prepareCreateVolumeReq` gating on
  `pod-affinity` — a matching topology segment is ignored and `host_id` is
  left as-is when the PVC lacks `simplyblock.io/pod-affinity: "true"`; the
  same segment is resolved normally when the annotation is present.

### Regression

- `go build ./...` and `make test` after extracting `SelectBestNode` out of
  `pickColdTarget`, to confirm the rebalancer's existing migration-target
  selection behavior (still gated by `MinHotColdDifferencePct`) is unchanged.

### Manual / E2E (Tier 2)

On a cluster (3+ nodes) with `spec.autoRebalancing` set (`enabled: true`,
`prometheusURL` pointing at the cluster's Prometheus):

- Create a plain PVC against the pool's StorageClass, with no consuming Pod
  yet. Confirm `simplyblock.io/host-id` is stamped on the PVC at admission,
  before any Pod exists.
- Create a Pod referencing that PVC; confirm it binds and reaches `Running`.
- Inspect the CSI provisioner's `CreateVolume` request/response (or the
  backend's volume record) and confirm the stamped node was used as primary
  (listed first in the returned `connections`).
- Repeat with multiple eligible nodes at different latency deviations, and
  confirm the PVC is always stamped with the coolest one (§5.3).

### Manual / E2E (Tier 1)

On a multi-node cluster (operator + `spdk-csi`) with `EnableNodeAffinity` set
on the `StorageCluster` and at least one worker co-located with a storage
node. Unless noted otherwise, every PVC below also carries
`simplyblock.io/pod-affinity: "true"` — without it, Tier 1 does not
evaluate at all regardless of the scheduling outcome:

- **`pod-affinity` gate:** create two otherwise-identical PVCs, each with a
  Pod pinned via `nodeSelector` to the same storage-plane worker — one PVC
  with `simplyblock.io/pod-affinity: "true"`, one without. Confirm the
  annotated PVC's `host_id` resolves to that worker's co-located storage-node
  UUID, and the unannotated one's does not, despite identical topology.
- **No affinity signal:** schedule an unconstrained Pod so it lands on a
  worker with no co-located storage node (e.g. a control-plane node). Confirm
  `accessibility_requirements` carries no `storage-node-uuid` segment and
  `host_id` is left unset.
- **`nodeSelector`:** pin a Pod to a storage-plane worker via
  `kubernetes.io/hostname`. Confirm `createVolume` resolves `host_id` to that
  worker's co-located storage-node UUID, and that the Pod can read/write
  through the mounted volume.
- **`podAffinity`:** pin an anchor Pod to a worker, then schedule a second Pod
  using `requiredDuringSchedulingIgnoredDuringExecution` podAffinity
  (`topologyKey: kubernetes.io/hostname`) matching the anchor, with no direct
  node reference of its own. Confirm it lands on the same worker and resolves
  to the same co-located storage-node UUID as the `nodeSelector` case —
  demonstrating Tier 1 is scheduler-mechanism-agnostic.
- **`spec.nodeName`:** set `spec.nodeName` directly on a Pod referencing a
  `WaitForFirstConsumer` PVC. Confirm the PVC never leaves
  `WaitForFirstConsumer` and provisioning never starts (§4) — a general
  Kubernetes limitation, not a Tier 1 defect.
- **CSINode key stability:** replace the storage node backing a co-located
  worker (forcing a UUID change) and confirm `CreateVolume` for that worker
  keeps succeeding — the topology label's key (`<clusterUUID>.<socketOrdinal>`)
  must stay stable even as its UUID value churns (§4).
- **Multi-instance tie-break:** on a worker co-located with more than one
  storage-node instance, create several volumes via Tier 1 and confirm
  placement spreads across all co-located instances rather than concentrating
  on one (§4).

### Further Coverage

- Unit + manual test for the `EnableNodeAffinity` precondition gate (§7): a
  `StorageNodeSet` on a cluster with `EnableNodeAffinity: false` should never
  get `storage-node-uuid` labels written, and a Pod pinned to such a worker
  should fall straight through to Tier 2/Tier 3.
