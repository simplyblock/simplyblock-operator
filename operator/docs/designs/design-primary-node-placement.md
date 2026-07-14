# Design Document: Load-Aware Primary Node Placement at Volume Creation

**Status:** Proposed
**Author:** Manohar Reddy
**Date:** 2026-07-14
**Closes:** https://github.com/simplyblock/simplyblock-operator/issues/216 (Automatically Select Volume Owner at Creation Time)
**Related Issues:** https://github.com/simplyblock/simplyblock-operator/issues/308 (Auto-Placement of Volumes at Creation), https://github.com/simplyblock/simplyblock-operator/issues/130 (Automatic Rebalancing — the algorithm this design reuses)

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
   - [3.1 Clones and Snapshot Restores Are Unaffected](#31-clones-and-snapshot-restores-are-unaffected)
4. [Node Selection Algorithm](#4-node-selection-algorithm)
5. [Webhook Handler](#5-webhook-handler)
6. [Data Model Changes](#6-data-model-changes)
7. [Backend API Requirements](#7-backend-api-requirements)
8. [Failure Modes and Fallback](#8-failure-modes-and-fallback)
9. [Configuration](#9-configuration)
10. [Observability](#10-observability)
11. [Testing Strategy](#11-testing-strategy)
12. [Open Questions](#12-open-questions)

---

## 1. Background

Volumes are currently placed by `sbcli`'s `_get_next_3_nodes()`
(`simplyblock_core/controllers/lvol_controller.py`), a weighted-random pick over
online storage nodes keyed **only** on subsystem count per node
(`constants.weights = {"lvol": 100}`). It has no notion of actual I/O load: a node
hosting few, but extremely hot, volumes is just as likely to receive the next
volume as an idle one.

The operator already computes a much better "how loaded is this node right now"
signal for a different purpose — `internal/volumemigration/autobalancing.StorageNodeSelector`,
built for the auto-rebalancer (Issue #130). It combines live Prometheus p99 write
latency with a per-node fio baseline into a per-node deviation score, and already
picks the "coolest" node as a migration target (`pickColdTarget`).

`add_lvol_ha` already accepts an explicit `host_id_or_name` and skips its own
weighted-random pick entirely when one is supplied. `spdk-csi` already reads a
`simplyblock.io/host-id` PVC annotation and forwards it as `host_id` on
`CreateVolume` — this path is proven end-to-end today by the operator's own
benchmark-volume provisioner (`operator/internal/controller/benchmark_provisioner.go`),
which always sets `HostID` explicitly. What is missing is anything that computes
that annotation from real load data, synchronously, for user-created (PVC-driven)
volumes.

This design closes that gap: the operator computes the best primary node using the
same node-hotness signal the rebalancer already uses, and stamps it onto the PVC as
`simplyblock.io/host-id` before the CSI provisioner ever calls `CreateVolume`.

---

## 2. Goals and Non-Goals

### Goals

- Reuse the rebalancer's existing node-hotness signal (current Prometheus p99
  latency vs. per-node fio baseline) to rank storage nodes by load at volume
  creation time.
- Compute the selected primary node **entirely inside the operator** — no new
  logic in `sbcli`, no new logic in `spdk-csi`.
- Inject the decision via the existing, already-supported `simplyblock.io/host-id`
  PVC annotation contract, so `spdk-csi` and the backend require zero changes to
  the creation path itself.
- Never override an explicit user-supplied `host_id` annotation (manual pinning
  always wins).
- Degrade silently to today's behavior (`sbcli`'s weighted-random pick) whenever
  the load signal isn't available for a cluster — this feature is strictly
  additive on top of clusters that already opted into latency benchmarking
  (Issue #130).
- Avoid picking a node that is offline, unhealthy, a secondary node, or already at
  subsystem capacity.

### Non-Goals

- Changing `sbcli`'s fallback placement algorithm (`_get_next_3_nodes`) itself.
- Node-affinity / topology-aware placement (pinning a volume to the consumer
  pod's worker node) — tracked separately (Issue #272).
- Capacity-based placement (disk space utilization) — out of scope, same as the
  rebalancer (Issue #130 §2 Non-Goals).
- Cross-cluster placement.
- Per-block-size or per-volume-QoS-aware scoring (Phase 2 of Issue #130, not
  needed here).

---

## 3. Architecture Overview

```
PVC created (user)
       │
       ▼
┌──────────────────────────────────────────────────────────────────────────┐
│         SimplyblockVolumePlacementInjector (NEW mutating webhook)        │
│                                                                          │
│  1. Skip if simplyblock.io/host-id already set (explicit pin wins)      │
│  2. Resolve StorageClass → cluster_id / pool_name params                │
│  3. Resolve StorageCluster CR by Status.UUID == cluster_id              │
│  4. Skip if AutoRebalancing disabled or PrometheusURL unset             │
│  5. GET storage-nodes (webapi.Client) → filter online/healthy/          │
│     non-secondary/under-capacity                                        │
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
└───────────────────────────────┬──────────────────────────────────────────┘
                                 │ HTTP
┌───────────────────────────────▼──────────────────────────────────────────┐
│  SimplyBlock Backend: add_lvol_ha(host_id_or_name=<uuid>)                │
│  host_node set → _get_next_3_nodes() is NEVER called                    │
│  _resolve_lvol_subsystem() still enforces max_lvol as a hard backstop   │
└────────────────────────────────────────────────────────────────────────┘
```

**Why a mutating webhook, not a new backend/CSI call:** `spdk-csi`'s
`fetchPVCAnnotations` (`pkg/spdk/controllerserver.go:1240`) performs a **live** GET
of the PVC object at `CreateVolume` time — it does not rely on CSI request
parameters cached earlier in the provisioning pipeline. A webhook that mutates the
PVC at admission time (before the external-provisioner sidecar even notices the
PVC) is therefore guaranteed to be visible by the time `spdk-csi` reads it. This
requires **zero changes to `spdk-csi`**.

This mirrors the existing `SimplyblockRebalancerInjector` pod-mutating webhook
(`operator/internal/webhook/simplyblock_rebalancer_injector.go`), which already
follows the same pattern: resolve the owning `StorageCluster` from context, check
whether the relevant feature is enabled for that cluster, patch if so, allow
unconditionally (`failurePolicy=Ignore`) otherwise.

### 3.1 Clones and Snapshot Restores Are Unaffected

Issue #216 explicitly calls out that clones (from another PVC or a
VolumeSnapshot) must land on the same host as their source — this webhook does
not special-case that, and it doesn't need to:

- In `spdk-csi`, `createVolume` (`pkg/spdk/controllerserver.go:736`) checks
  `req.GetVolumeContentSource()` **before** calling `prepareCreateVolumeReq` (the
  function that reads the `host-id` annotation). When the PVC has a data source,
  `handleVolumeContentSource` handles it via `CloneSnapshot`/`CloneVolume` and
  returns — `prepareCreateVolumeReq` is never reached, so a `host-id` annotation
  stamped by this webhook is never read for a clone/restore.
- On the backend, `clone_lvol` (`simplyblock_core/controllers/lvol_controller.py:2572`)
  always places the clone via `lvol.node_id` — the **source** volume's own node.
  There is no `host_id` parameter on the clone path at all.

So same-host clone placement is already guaranteed by the existing
CSI/backend architecture, independent of this webhook. If a future change to
`spdk-csi` ever made `prepareCreateVolumeReq` run for content-sourced PVCs too,
this invariant would need to be re-verified.

---

## 4. Node Selection Algorithm

Reuses the exact signal `autobalancing.StorageNodeSelector` computes for the
rebalancer (Issue #130 §5.2):

```
latencyDeviationPct(node) = (currentP99NS - baselineP99NS) / baselineP99NS × 100
```

Where `currentP99NS` is queried live from Prometheus
(`simplyblock_node_fio_write_latency_p99_ns`) and `baselineP99NS` is the one-time
fio baseline stored on the owning `StorageNodeSet` CR (`Status.LatencyMetrics`).

### New entry point: `SelectBestNode`

`pickColdTarget` (`storage_node_selector.go`) already contains the core "pick the
coolest node" loop, gated by `MinHotColdDifferencePct` (a migration-specific
"must be meaningfully cooler than a given hot source" rule that doesn't apply to
placement). This design extracts the ranking core into a new exported method:

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

Nodes with no latency data yet (no baseline measured, deviation = 0) rank as the
best possible candidates — consistent with how the rebalancer already treats
unmeasured nodes as migration targets (Issue #130 §6 Step 5).

### Eligibility filter (applied before ranking)

| Filter | Source | Rationale |
|---|---|---|
| `status == "online"` | `webapi.StorageNodeInfo.Status` | Never place on an offline node |
| `health_check == true` | `webapi.StorageNodeInfo.Healthy` | Mirrors rebalancer target eligibility (Issue #130 §6 Step 5) |
| not a secondary node | `webapi.StorageNodeInfo.IsSecondary` (new field, §6) | Only primary-capable nodes host a new lvol's primary subsystem |
| `Lvols < LvolsMax` | `webapi.StorageNodeInfo.Lvols` / `.LvolsMax` (new fields, §6) | Mirrors `sbcli`'s own `max_lvol` capacity gate (`_resolve_lvol_subsystem`) so we don't hand the backend a node it will immediately reject |

The capacity check is an approximation of `sbcli`'s `count_lvol_subsystems` (which
counts distinct subsystems, not raw lvol count, since namespaced pools share a
subsystem across lvols) — it is slightly conservative but avoids the common
"clearly full node" case. `_resolve_lvol_subsystem`'s exact check remains the
authoritative backstop server-side regardless.

---

## 5. Webhook Handler

New file: `operator/internal/webhook/simplyblock_volume_placement_injector.go`,
same shape as `simplyblock_rebalancer_injector.go`.

```go
// +kubebuilder:webhook:path=/mutate-v1-pvc-simplyblock-placement,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=persistentvolumeclaims,verbs=create,versions=v1,name=simplyblock-volume-placement-injector.simplyblock.io,admissionReviewVersions=v1

type SimplyblockVolumePlacementInjector struct {
    Client       client.Client
    APIClient    *webapi.Client
    NodeSelector *autobalancing.StorageNodeSelector
}
```

### Handle flow

1. Decode the PVC. Allow unmodified if:
   - `simplyblock.io/host-id` (or deprecated `simplybk/host-id`) is already set.
   - `pvc.Spec.StorageClassName` is unset, or the referenced `StorageClass`'s
     `Provisioner` isn't the simplyblock CSI driver, or `parameters["cluster_id"]`
     is empty.
2. Resolve the `StorageCluster` CR whose `Status.UUID == cluster_id` — same lookup
   pattern as `SimplyblockRebalancerInjector.resolveConfig`. Allow unmodified if
   not found, or if `Spec.VolumeMigrationSettings.AutoRebalancing` is nil/disabled,
   or `PrometheusURL` is unset.
3. Build `autobalancing.RebalancingConfig` via the existing
   `autobalancing.ResolveRebalancingConfig(spec)`.
4. `APIClient.GetStorageNodes(ctx, clusterUUID)` — same call
   `VolumeRebalancerReconciler` already makes (in-cluster service-account auth, no
   per-cluster secret).
5. Apply the eligibility filter (§4).
6. `NodeSelector.SelectBestNode(...)`. Allow unmodified if no eligible node.
7. Patch the PVC: `simplyblock.io/host-id = <chosen node UUID>`.

Any error at steps 2–6 (backend unreachable, Prometheus query failure, no
StorageNodeSet baseline yet) results in `admission.Allowed(...)` with no patch —
`sbcli`'s existing weighted-random pick runs exactly as it does today. This
mirrors `failurePolicy=Ignore` on the webhook registration itself: the feature
can never block volume provisioning.

### Registration

`operator/cmd/main.go`, alongside the existing registration (~line 418), under the
same `webhookReady` gate:

```go
mgr.GetWebhookServer().Register("/mutate-v1-pvc-simplyblock-placement",
    &webhook.Admission{Handler: &internalwebhook.SimplyblockVolumePlacementInjector{
        Client:       mgr.GetClient(),
        APIClient:    webapi.NewClient(),
        NodeSelector: autobalancing.NewStorageNodeSelector(mgr.GetClient()),
    }})
```

`make manifests` regenerates `config/webhook/manifests.yaml` (and the Helm chart /
`dist/install.yaml` copies) from the new marker.

### RBAC

No new PVC write RBAC is needed — the mutation happens via the admission response
patch, not a client-side `Update`. New read RBAC:

```go
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets,verbs=get;list;watch
```

`storageclasses` and the two `storage.simplyblock.io` reads are already granted to
other controllers (`simplyblockpool_controller.go`, `volumerebalancer_controller.go`)
and land in the same aggregated ClusterRole — verify at implementation time
whether a distinct marker is still needed for kubebuilder to pick it up for this
file, but no new *permission* is required.

---

## 6. Data Model Changes

All additions are additive; nothing existing changes shape.

### 6.1 `operator/internal/webapi/rebalancing.go` — `StorageNodeInfo`

```go
type StorageNodeInfo struct {
    UUID        string `json:"id"`
    Status      string `json:"status"`
    Healthy     bool   `json:"health_check"`
    TotalBytes  int64  `json:"total_capacity_bytes"`
    Lvols       int    `json:"lvols"`        // NEW — already returned by the v2 API
    LvolsMax    int    `json:"lvols_max"`    // NEW — already returned by the v2 API
    IsSecondary bool   `json:"is_secondary"` // NEW — requires a backend field addition, §7
}
```

`Lvols` / `LvolsMax` require **no backend change** — `StorageNodeDTO` in
`simplyblock_web/api/v2/_dtos.py` already serializes `lvols` and `lvols_max`
(`model.lvols`, `model.max_lvol`); the Go struct simply never mapped them because
the rebalancer never needed them.

### 6.2 No StorageCluster/StorageNodeSet CRD changes

This design reads existing fields only: `StorageCluster.Spec.VolumeMigrationSettings.AutoRebalancing`
(Issue #130 §4.1) and `StorageNodeSet.Status.LatencyMetrics` (Issue #130 §4.3,
implemented on `StorageNodeSet` in this codebase).

---

## 7. Backend API Requirements

One additive field, the **only** `sbcli` change in this design:

`simplyblock_web/api/v2/_dtos.py` — `StorageNodeDTO` doesn't currently expose
whether a node is a secondary node (only `secondary_node_id`, which is a
*primary's* pointer to its own secondary; a secondary node's own record isn't
distinguishable via the v2 API today). Add a passthrough field mirroring
`model.is_secondary_node`:

```python
class StorageNodeDTO(BaseModel):
    ...
    is_secondary: bool  # NEW

    @staticmethod
    def from_model(model: StorageNode, stat_obj: ...):
        return StorageNodeDTO(
            ...,
            is_secondary=model.is_secondary_node,  # NEW
        )
```

No new logic, no new endpoint — a one-field API exposure of data the model
already tracks (`simplyblock_core/models/storage_node.py:72`).

---

## 8. Failure Modes and Fallback

| Condition | Behavior |
|---|---|
| `simplyblock.io/host-id` already set on the PVC | Skip — explicit pin always wins |
| StorageClass isn't simplyblock-provisioned, or has no `cluster_id` | Skip |
| `StorageCluster` not found for `cluster_id` | Skip (log) |
| `AutoRebalancing` nil/disabled or `PrometheusURL` unset for the cluster | Skip — cluster hasn't opted into the load signal |
| Backend API (`GetStorageNodes`) unreachable | Skip (log); `failurePolicy=Ignore` also protects at the webhook-server level |
| Prometheus unreachable / query error | Skip (log) |
| No eligible node (all offline/unhealthy/secondary/at-capacity) | Skip (log) |
| Pool has `qos_host` set (`pool.has_qos()`) | Not special-cased — `add_lvol_ha` overrides any `host_id` with `pool.qos_host` regardless, so the injected annotation is harmless but ignored. Documented, not fixed, in v1. |

In every skip case the PVC is admitted unmodified and `sbcli`'s existing
weighted-random placement (`_get_next_3_nodes`) runs exactly as it does today —
this feature can only ever make placement *better or unchanged*, never worse or
blocking.

---

## 9. Configuration

No new configuration surface. The feature activates automatically, per cluster,
whenever that cluster already has:

```yaml
spec:
  volumeMigrationSettings:
    autoRebalancing:
      enabled: true
      latencyBenchmarkEnabled: true
      prometheusURL: "http://prometheus.monitoring:9090"
```

(the same fields Issue #130 introduced). Clusters without latency benchmarking
enabled see no behavior change.

---

## 10. Observability

### Kubernetes Events

| Event | Type | Reason |
|---|---|---|
| Primary node selected for new PVC | `Normal` | `PrimaryNodeSelected` |
| Selection skipped — no signal available | (none; logged only, high frequency expected) | — |

### Prometheus Metrics

| Metric | Labels | Description |
|---|---|---|
| `simplyblock_placement_decisions_total` | `cluster_uuid`, `result` (`selected`\|`skipped`) | Count of webhook invocations by outcome |
| `simplyblock_placement_selected_node_deviation_pct` | `cluster_uuid`, `node_uuid` | Latency deviation of the node chosen, at selection time |

---

## 11. Testing Strategy

### Unit Tests

Mirroring `simplyblock_rebalancer_injector_test.go`, with a fake `client.Client`,
fixture `StorageCluster`/`StorageNodeSet` CRs, and a stubbed `webapi.Client` +
Prometheus response:

- Annotation already set → PVC unmodified.
- StorageClass missing / not simplyblock-provisioned → PVC unmodified.
- `AutoRebalancing` disabled or `PrometheusURL` unset → PVC unmodified.
- Multiple eligible nodes with different deviations → lowest-deviation node chosen.
- Offline / unhealthy / secondary / at-capacity nodes excluded from candidates.
- No eligible node → PVC unmodified (no error surfaced to the CO).
- Backend or Prometheus error → PVC unmodified, error logged, request still `Allowed`.

### Regression

- `go build ./...` and `make test` after extracting `SelectBestNode` out of
  `pickColdTarget`, to confirm the rebalancer's existing migration-target
  selection behavior (still gated by `MinHotColdDifferencePct`) is unchanged.

### Manual / E2E

On a test cluster with `latencyBenchmarkEnabled: true` and `prometheusURL` set:
create a PVC against a StorageClass carrying `cluster_id`/`pool_name` parameters;
confirm `kubectl get pvc -o yaml` shows `simplyblock.io/host-id` stamped before
the PVC binds, and that the resulting lvol's `storage_node_id` matches the
coolest node per `rebalancer_node_latency_deviation_pct` at that moment.

---

## 12. Open Questions

**Q1: Should the webhook also weigh current IOPS/throughput, not just latency
deviation?** The rebalancer's volume-level IO score doesn't apply (no history for
a not-yet-created volume), but a node-level aggregate IOPS/throughput signal could
supplement latency deviation the same way `iopsWeight`/`throughputWeight` do for
migration ranking. Deferred — latency deviation alone is a meaningful
improvement over pure subsystem-count weighting and keeps this a small, additive
change.

**Q2: Should capacity (disk space) factor into node selection?** Explicitly out
of scope here (matching Issue #130), but a node that's I/O-cool and about to fill
up is a poor placement choice. Worth a follow-up design.

**Q3: What about pools with `qos_host` set?** Currently the injected annotation is
silently overridden server-side (§8). Should the webhook skip computing/injecting
for such pools entirely, as a minor optimization? Not required for correctness,
low priority.

**Q4: Should there be a per-PVC opt-out annotation** (e.g.
`simplyblock.io/disable-smart-placement: "true"`) for workloads that want the
legacy weighted-random behavior even when the cluster has benchmarking enabled?

**Q5: Multi-cluster StorageClasses.** `resolveClusterSelection` in `spdk-csi`
supports zone/region-mapped multi-cluster StorageClasses
(`paramZoneClusterMap`/`paramRegionClusterMap`), resolved from the CSI
`CreateVolumeRequest`'s topology requirements — information the webhook does not
have access to at PVC-admission time (topology is resolved later, during
scheduling). For such StorageClasses the webhook cannot determine `cluster_id`
up front and must skip. Confirm this is an acceptable, documented limitation
rather than a gap to close.
