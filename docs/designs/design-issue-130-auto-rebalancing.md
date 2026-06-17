# Design Document: Automatic Volume Rebalancing (Issue #130)

**Status:** Phase 1 Implemented (including VolumeMigration CRD and autobalancing refactor)  
**Author:** Christoph Engelbert (noctarius)  
**Date:** 2026-06-02 (last updated 2026-06-17)  
**Issue:** https://github.com/simplyblock/simplyblock-operator/issues/130

---

## Phasing Overview

| Phase                             | Status          | Signal                                                                    | Volume priority                                                |
|-----------------------------------|-----------------|---------------------------------------------------------------------------|----------------------------------------------------------------|
| **Phase 1** (this doc, §5.1–§5.3) | **Implemented** | fio p99 latency deviation from per-node baseline                          | `iopsWeight × IOPS + throughputWeight × (Throughput / MB·s⁻¹)` |
| **Phase 2** (this doc, §5.4–§5.5) | Planned         | Prometheus IOPS sliding window, block-size weight table, node-size factor | Full block-size × erasure-scheme × direction weight table      |

The Phase 1 implementation is self-contained and does not require a Prometheus deployment or SPDK metric agreement. Phase 2 extends it with richer I/O weighting once per-node SPDK metrics are available.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [Data Model Changes](#4-data-model-changes)
5. [I/O Load Measurement and Weighting](#5-io-load-measurement-and-weighting)
6. [Volume Migration Algorithm](#6-volume-migration-algorithm)
7. [Cool-Down Mechanism](#7-cool-down-mechanism)
8. [New Controller: VolumeRebalancer](#8-new-controller-volumerebalancer)
9. [New CRD: VolumeMigration](#9-new-crd-volumemigration)
10. [Autobalancing Package](#10-autobalancing-package)
11. [Metrics Provider Interface](#11-metrics-provider-interface)
12. [Backend API Requirements](#12-backend-api-requirements)
13. [Configuration](#13-configuration)
14. [Observability](#14-observability)
15. [Testing Strategy](#15-testing-strategy)
16. [Open Questions](#16-open-questions)

---

## 1. Background

Storage volumes are currently placed using a volume-count–weighted random selection at creation time: nodes with fewer volumes are more likely to receive new ones. This heuristic ignores actual I/O load. A node that hosts fewer, but extremely hot, volumes can become a bottleneck while other nodes remain idle.

The operator has existing primitives for drain-based rebalancing (triggered during node recycle operations) and tracks a `Rebalancing` flag in `StorageClusterStatus`, but it has no capability to proactively detect load imbalance and migrate volumes without an operator-driven event.

---

## 2. Goals and Non-Goals

### Goals

- Continuously monitor per-node latency degradation relative to each node's own baseline.
- Proactively migrate volumes from degraded nodes to healthy nodes when a deviation threshold is exceeded.
- Prioritise volumes contributing the most I/O load (highest combined IOPS + throughput) for migration.
- Respect per-volume pinning: volumes whose PVC carries the `simplyblock.io/pinned-volume` annotation are never migrated.
- Apply a configurable cool-down period after each migration to prevent oscillation.
- Emit warnings when the only candidates are pinned or cooling-down volumes.
- Expose Prometheus metrics for deviation, migration events, and per-node scores.

### Non-Goals

- Volume migration between different storage clusters.
- Changing the volume placement strategy at creation time (a separate concern).
- Replacing the existing drain-based rebalancing path.
- Rebalancing based on capacity utilization (this design focuses on I/O load).

---

## 3. Architecture Overview

```
┌───────────────────────────────────────────────────────────────────────┐
│                        Kubernetes Control Plane                       │
│                                                                       │
│  ┌────────────────────────────────────────────────────────────────┐   │
│  │            VolumeRebalancerReconciler                          │   │
│  │                                                                │   │
│  │  1. Periodic poll (configurable interval)                      │   │
│  │  2. Compute latency deviation per node (Prometheus + CR)       │   │
│  │  3. Delegate selection to autobalancing.Rebalancer             │   │
│  │     ├── StorageNodeSelector  (which nodes are hot/cool)        │   │
│  │     └── LogicalVolumeSelector (which volumes to migrate)       │   │
│  │  4. executeMigrations: POST /migrations/ per candidate         │   │
│  │  5. processPendingMigrations: poll GET /migrations/{id}/       │   │
│  │  6. Record cool-down state in MigrationState                   │   │
│  └──────────────────────────────┬─────────────────────────────────┘   │
│                                 │ reads latency state                 │
│  ┌──────────────────────────────▼─────────────────────────────────┐   │
│  │            StorageNodeLatencyReconciler                        │   │
│  │                                                                │   │
│  │  Manages fio baseline Jobs per backend node UUID               │   │
│  │  Stores BaselineP50NS/P99NS in StorageNode.status              │   │
│  └────────────────────────────────────────────────────────────────┘   │
│                                                                       │
│  ┌────────────────────────────────────────────────────────────────┐   │
│  │            VolumeMigrationReconciler (NEW §9)                  │   │
│  │                                                                │   │
│  │  User-triggered migration via VolumeMigration CR               │   │
│  │  Pending → Validating → Running → Completed/Failed/Aborted     │   │
│  │  Spawns nvme-validation Job; calls CreateMigration +           │   │
│  │  ContinueMigration; polls GetMigration to track completion     │   │
│  └────────────────────────────────────────────────────────────────┘   │
│                                                                       │
│  StorageCluster CR  spec.volumeRebalancing.*  status.rebalancing      │
│  StorageNode CR     status.latencyMetrics[]                           │
│  VolumeMigration CR (NEW §9)  spec.pvName  spec.targetNodeUUID        │
└───────────────────────────────────────────────────────────────────────┘
              │ HTTP (webapi client, service-account bearer token)
┌─────────────▼──────────────────────────────────────────────────────────┐
│              SimplyBlock Backend API                                   │
│  GET  /api/v2/clusters/{id}/storage-nodes/                             │
│  GET  /api/v2/clusters/{id}/storage-pools/{id}/volumes/                │
│  POST /api/v2/clusters/{id}/migrations/          (returns connections) │
│  POST /api/v2/clusters/{id}/migrations/continue  (NEW — two-phase)    │
│  GET  /api/v2/clusters/{id}/migrations/{id}/                           │
│  POST /api/v2/clusters/{id}/migrations/{id}/cancel                     │
└────────────────────────────────────────────────────────────────────────┘
```

**Key change from initial design:** The `VolumeRebalancerReconciler` now delegates the full selection algorithm to the `autobalancing` package (§10), which provides three testable components — `StorageNodeSelector`, `LogicalVolumeSelector`, and `Rebalancer`. The reconciler only orchestrates API calls and status writes.

Both the `StorageNodeLatencyReconciler` and the `VolumeMigrationReconciler` watch owned `batchv1.Job` objects (`.Owns(&batchv1.Job{})`) so they are triggered immediately when a Job terminates rather than waiting for the next poll interval.

**Authentication change:** The `webapi.Client` now authenticates via the operator pod's Kubernetes service-account token (`/var/run/secrets/kubernetes.io/serviceaccount/token`) rather than per-cluster secrets. All `Do` / `DoWithHeaders` calls no longer take a `clusterSecret` parameter.

---

## 4. Data Model Changes

### 4.1 StorageCluster Spec — `VolumeRebalancingSpec` (new, Phase 1)

```go
// VolumeRebalancingSpec controls the automatic volume rebalancing behaviour.
type VolumeRebalancingSpec struct {
    // Enabled activates automatic rebalancing for this cluster.
    // Defaults to true.
    // +optional
    Enabled *bool `json:"enabled,omitempty"`

    // EvaluationInterval is how often the rebalancer evaluates load.
    // Defaults to 60s.
    // +optional
    EvaluationInterval *metav1.Duration `json:"evaluationInterval,omitempty"`

    // ImbalanceThreshold is the minimum p99 latency deviation from baseline
    // (as a percentage) that a node must exhibit before it is considered a
    // rebalancing source. Defaults to 20.
    // Example: 20 means "trigger if currentP99 > 1.20 × baselineP99".
    // +optional
    ImbalanceThreshold *int32 `json:"imbalanceThreshold,omitempty"`

    // DefaultCoolDownSeconds is the default cool-down period (in seconds)
    // applied to a volume after it has been migrated. Defaults to 60.
    // +optional
    DefaultCoolDownSeconds *int32 `json:"defaultCoolDownSeconds,omitempty"`

    // MaxVolumeMigrationsPerCycle defines the maximum number of volumes that may be
    // moved in a single evaluation cycle. The 10% IO-score budget is always the
    // binding constraint; this field provides an additional hard cap on the
    // volume count. Defaults to 10.
    // +optional
    MaxVolumeMigrationsPerCycle *int32 `json:"maxVolumeMigrationsPerCycle,omitempty"`

    // IOPSWeight is the weight applied to per-volume IOPS when computing the
    // volume IO score used to rank migration candidates.
    // Defaults to 1.0.
    // +optional
    IOPSWeight *float64 `json:"iopsWeight,omitempty"`

    // ThroughputWeight is the weight applied to per-volume throughput (in MB/s)
    // when computing the volume IO score.
    // Defaults to 0.1 (so 1 MB/s ≈ 0.1 of a IOPS unit — both terms land on a
    // comparable numerical scale with typical NVMe workloads).
    // +optional
    ThroughputWeight *float64 `json:"throughputWeight,omitempty"`

    // LatencyBenchmarkEnabled enables fio-based NVMe-oF latency measurement.
    // Defaults to false; set to true once a FioBenchmarkImage is configured.
    // +optional
    LatencyBenchmarkEnabled *bool `json:"latencyBenchmarkEnabled,omitempty"`

    // LatencyBenchmarkInterval controls how often the sidecar result is read
    // by the operator. The sidecar itself runs on a fixed 5-minute cycle.
    // Defaults to 5m.
    // +optional
    LatencyBenchmarkInterval *metav1.Duration `json:"latencyBenchmarkInterval,omitempty"`

    // FioBenchmarkImage is the container image injected as the fio-bench-probe
    // sidecar into each SPDK DaemonSet pod. The image must include fio, nvme-cli,
    // jq, and prometheus-node-exporter.
    // +optional
    FioBenchmarkImage *string `json:"fioBenchmarkImage,omitempty"`

    // PrometheusURL is the address of the Prometheus instance that scrapes
    // the fio-bench-probe sidecar metrics. Required in Phase 1 — the rebalancer
    // reads simplyblock_node_fio_write_latency_p99_ns from Prometheus to obtain
    // the current per-node latency.
    // +optional
    PrometheusURL *string `json:"prometheusURL,omitempty"`

    // --- Phase 2 fields (no-ops in Phase 1) ---

    // MetricsBackend selects the data source for IOPS metrics (Phase 2 only).
    // Has no effect in Phase 1.
    // +optional
    MetricsBackend *MetricsBackend `json:"metricsBackend,omitempty"`
}
```

### 4.2 StorageCluster Status — `RebalancingMetrics` (new)

```go
// NodeLoadMetrics holds the latency deviation state for a single storage node.
type NodeLoadMetrics struct {
    NodeUUID            string      `json:"nodeUUID"`
    LatencyDeviationPct float64     `json:"latencyDeviationPct"`
    VolumeCount         int         `json:"volumeCount"`
    LastUpdated         metav1.Time `json:"lastUpdated"`
}

// RebalancingMetrics is written by the VolumeRebalancerReconciler each cycle.
type RebalancingMetrics struct {
    // AvgDeviationPct is the mean latency deviation across all measured nodes.
    AvgDeviationPct  float64           `json:"avgDeviationPct"`
    // MaxDeviationPct is the highest per-node latency deviation (mirrors ImbalancePercent).
    MaxDeviationPct  float64           `json:"maxDeviationPct"`
    HottestNodeUUID  string            `json:"hottestNodeUUID"`
    CoolestNodeUUID  string            `json:"coolestNodeUUID"`
    ImbalancePercent float64           `json:"imbalancePercent"`
    LastEvaluatedAt  *metav1.Time      `json:"lastEvaluatedAt,omitempty"`
    LastMigrationAt  *metav1.Time      `json:"lastMigrationAt,omitempty"`
    NodeMetrics      []NodeLoadMetrics `json:"nodeMetrics,omitempty"`
}
```

Added to `StorageClusterStatus`:

```go
// RebalancingMetrics is updated by the auto-rebalancer each evaluation cycle.
// +optional
RebalancingMetrics *RebalancingMetrics `json:"rebalancingMetrics,omitempty"`
```

### 4.3 StorageNode Status — `NodeLatencyMetrics` (new)

Added to `StorageNodeStatus`:

```go
// LatencyMetrics holds per-backend-node fio-measured latency data for rebalancing decisions.
// +optional
LatencyMetrics []NodeLatencyMetrics `json:"latencyMetrics,omitempty"`
```

New type:

```go
// NodeLatencyMetrics holds fio-measured 4K NVMe-oF write latency for a single backend node.
// The benchmark volume NQN and connection details are derived at runtime from the cluster NQN
// and the node UUID — they are not stored here.
type NodeLatencyMetrics struct {
    NodeUUID           string       `json:"nodeUUID"`
    BaselineP50NS      int64        `json:"baselineP50NS,omitempty"`
    BaselineP99NS      int64        `json:"baselineP99NS,omitempty"`
    BaselineMeasuredAt *metav1.Time `json:"baselineMeasuredAt,omitempty"`
}
```

> **Note:** Current latency (`CurrentP50NS`, `CurrentP99NS`) is not stored in the CR. The `VolumeRebalancerReconciler` reads it directly from Prometheus (`simplyblock_node_fio_write_latency_p99_ns`) on every evaluation cycle. Only the baseline — a one-time, stable measurement — is persisted in the CR.

> **Benchmark volume connection:** The benchmark volume exists automatically on every storage node. Its logical volume ID equals the storage node UUID, so the NVMe-oF NQN is derived as `<StorageCluster.Status.NQN>:lvol:<nodeUUID>`. The TCP address comes from `NodeStatus.MgmtIp` and the port from `NodeStatus.NvmfPort` (fallback: 4420). No API call is needed to discover these values.

### 4.4 Per-Volume Cool-Down State

Cool-down tracking is kept in-memory inside the controller (`map[string]time.Time`, keyed by `clusterUUID/volumeUUID`). Cool-down state intentionally does not survive operator restarts. The worst case is one extra migration cycle before the cool-down window re-establishes — an acceptable trade-off that avoids write amplification on every evaluation cycle.

After a restart the `Migrating` field on `VolumeInfo` (returned by the REST API) fills the gap: `filterEligibleVolumes` excludes any volume where `Migrating == true`, preventing re-migration of an already in-flight volume even when the in-memory cool-down map is empty.

---

## 5. I/O Load Measurement and Weighting

### 5.1 Latency Measurement Architecture (Phase 1 — Implemented)

The rebalancing trigger in Phase 1 is per-node **p99 write latency deviation** from each node's own baseline, measured by `fio` running directly on the storage host. Two measurement modes collaborate:

```
┌──────────────────────────────────────────────────────────────────┐
│                  SPDK DaemonSet Pod (per node)                   │
│                                                                  │
│  ┌─────────────────────┐    ┌────────────────────────────────┐   │
│  │   spdk container    │    │  fio-bench-probe sidecar       │   │
│  │   (storage I/O)     │    │  0.1 vCPU / 25 MiB             │   │
│  │                     │    │  privileged, hostNetwork       │   │
│  │                     │    │                                │   │
│  │                     │    │  1. Read JSON array from CM    │   │
│  │                     │    │  2. Per NUMA node (parallel):  │   │
│  │                     │    │     nvme connect (TCP)         │   │
│  │                     │    │     fio 30s every 5 min        │   │
│  │                     │    │     Write fio_<nodeUUID>.prom  │   │
│  │                     │    │                                │   │
│  └─────────────────────┘    └────────────┬───────────────────┘   │
│                                          │ :9199 (textfile)      │
└──────────────────────────────────────────┼───────────────────────┘
                                           │
                     ┌─────────────────────▼────────────────────┐
                     │  prometheus-node-exporter (:9199)        │
                     │  Exposes simplyblock_node_fio_write_*    │
                     └──────────────────────────────────────────┘
```

**Baseline Job** (`sb-fio-baseline-<nodeUUID>`): A one-shot Kubernetes Job is created per backend node UUID when the node comes online, before production volumes are attached. It connects via NVMe-oF, runs the same 30 s fio benchmark, writes `{"p50_ns":..., "p99_ns":...}` to `/dev/termination-log`, then exits. The `StorageNodeLatencyReconciler` is notified immediately via the owned-Job watch (`.Owns(&batchv1.Job{})`), reads the result from `pod.Status.ContainerStatuses[*].State.Terminated.Message`, stores `BaselineP50NS` / `BaselineP99NS` in the `StorageNode` status, and deletes the Job.

**Runtime sidecar** (`fio-bench-probe`): A persistent sidecar container injected into the SPDK DaemonSet pod by `BuildStorageNodeDaemonSet`. It starts the `prometheus-node-exporter` on port 9199, reads a **JSON array** of per-node NVMe-oF connection configs from a ConfigMap (keyed by `$HOSTNAME`), and launches one independent background loop per NUMA node. Each loop connects to its node's NVMe-oF volume and runs `fio` every ~5 minutes, writing per-node results to `fio_<nodeUUID>.prom` picked up by the textfile collector. On NUMA hosts with multiple backend nodes, all nodes are measured in parallel with independent NVMe connections and independent `.prom` files.

> **Why a sidecar instead of a periodic Job?** The sidecar is colocated with the SPDK process on the same host kernel. `hostNetwork: true` eliminates TCP round-trip latency from the measurement, giving a true reflection of storage stack latency. A periodic Job would incur pod-scheduling overhead and NVMe-oF reconnect latency (~100 ms) on every benchmark run, polluting the results. The marginal resource cost of the sidecar (0.1 vCPU, 25 MiB) is negligible.

**Prometheus as the current-latency data source:** `simplyblock_node_fio_write_latency_p99_ns{cluster, node}` is the **primary source** for current latency in Phase 1. The `VolumeRebalancerReconciler` queries this metric directly from Prometheus on every evaluation cycle using `spec.volumeRebalancing.prometheusURL`. The sidecar writes the metric via the textfile collector on every fio run (~every 5 minutes); Prometheus must be configured to scrape port 9199 on the storage-node pods.

**Baseline is stored in the StorageNode CR, not in Prometheus.** The baseline is a one-time, stable measurement — it does not belong in a time-series store. `StorageNodeLatencyReconciler` writes `BaselineP99NS` into `StorageNode.Status.LatencyMetrics` after the baseline Job completes, and `VolumeRebalancerReconciler` reads it from there. The baseline is never re-measured automatically.

### 5.2 Latency Deviation Score (Phase 1 — Implemented)

For each storage node that has completed at least one baseline and one runtime measurement:

```
latencyDeviationPct(node) = (currentP99NS - baselineP99NS) / baselineP99NS × 100
```

Properties:
- Returns **0** if `currentP99NS ≤ baselineP99NS` (no degradation counted).
- Returns **0** if either value is non-positive (no data → not a rebalancing source).
- No sliding window, no decay function — the latest measurement is the signal. The 5-minute sidecar cadence provides natural smoothing.

### 5.3 Volume IO Score (Phase 1 — Implemented)

Volumes are prioritised for migration by their measured contribution to node I/O load:

```
volumeIOScore(vol) = iopsWeight × IOPS + throughputWeight × (ThroughputBytesPerSec / 1 000 000)
```

where `IOPS` and `ThroughputBytesPerSec` are queried from Prometheus using the metrics exported by the simplyblock control plane. The throughput term is normalised to MB/s so both terms land on a comparable numerical scale with default weights.

**Prometheus metric sources:**

| Metric                               | Label                | Value                      |
|--------------------------------------|----------------------|----------------------------|
| `lvol_read_io_ps{cluster, lvol}`     | `lvol` = volume UUID | read IOPS                  |
| `lvol_write_io_ps{cluster, lvol}`    | `lvol` = volume UUID | write IOPS                 |
| `lvol_read_bytes_ps{cluster, lvol}`  | `lvol` = volume UUID | read throughput (bytes/s)  |
| `lvol_write_bytes_ps{cluster, lvol}` | `lvol` = volume UUID | write throughput (bytes/s) |

`IOPS = lvol_read_io_ps + lvol_write_io_ps`, `ThroughputBytesPerSec = lvol_read_bytes_ps + lvol_write_bytes_ps`.

These metrics are exported by the control plane from its own Prometheus scrape of the SPDK statistics. The `VolumeIOProvider` (`internal/metrics/prometheus/volumes.go`) queries all four metrics in a single reconcile cycle and merges the results by volume UUID.

**Default weights:**

| Parameter          | Default | Meaning                                              |
|--------------------|---------|------------------------------------------------------|
| `iopsWeight`       | 1.0     | 1 IOPS = 1 unit                                      |
| `throughputWeight` | 0.1     | 1 MB/s = 0.1 units (so 10 IOPS ≈ 1 MB/s in priority) |

Both weights are configurable via `spec.volumeRebalancing.iopsWeight` / `throughputWeight`. The formula intentionally avoids needing a histogram of block sizes; a single combined score is sufficient to order migration candidates by load contribution.

**Fallback when Prometheus data is unavailable:** If the Prometheus query fails or a volume has no series yet, `VolumeInfo.IOPS` and `ThroughputBytesPerSec` retain whatever the REST API returned (may be zero). If all scores are 0 on a node, the budget check (`rc.score ≤ 0`) is true for every candidate after the first, resulting in up to `maxVolumeMigrationsPerCycle` migrations. This is the correct "no data" fallback — proceed with migration based on latency signal alone.

---

### 5.4 Block-Size Weight Table (Phase 2 — Planned)

> **Phase 2 only.** The following section describes the planned richer weighting algorithm that will replace or complement the Phase 1 score once per-node SPDK metrics are available via Prometheus.

#### Sliding Window

The rebalancer will maintain a **sliding window** of raw I/O samples per storage node. Each sample records:

```
(timestamp, blocksize_bucket, operation_type, bytes_transferred, erasure_scheme)
```

The window width is `3 × evaluationInterval` (e.g., 3 minutes at the 60 s default). Samples are exponentially discounted with a half-life of `evaluationInterval`:

```
effective_weight = raw_weight × exp(-ln(2) × age_seconds / half_life_seconds)
```

#### Block-Size Weight Table

The raw weight for a single I/O operation depends on **block size**, **erasure coding scheme**, and **direction**:

| Block size | Read | Write 1+1 | Write 2+1 | Write 4+1 | Write 1+2 | Write 2+2 | Write 4+2 |
|------------|------|-----------|-----------|-----------|-----------|-----------|-----------|
| >= 64K     | 1    | 2         | 1.5       | 1.25      | 3         | 2         | 1.5       |
| >= 32K     | 1.5  | 3         | 2.25      | 1.6       | 4.5       | 3         | 2.25      |
| >= 16K     | 2    | 4         | 3         | 2.5       | 6         | 4         | 3         |
| >= 8K      | 3    | 6         | 4.5       | 3.75      | 9         | 6         | 4.5       |
| 4K         | 4    | 8         | 12        | 12        | 12        | 16        | 16        |

Block-size buckets are evaluated from largest to smallest; the first matching bucket is used.

#### Node-Size Normalisation Factor

```
w_node = node_total_storage_bytes / cluster_average_storage_bytes
weighted_score(node) = Σ(effective_weight × io_weight_per_op) / w_node
```

#### Source Node Selection in Phase 2

Phase 2 re-introduces `storageNodeCandidateCount` (default: 3): the top-K nodes by weighted IOPS score are evaluated and the one with the highest **migratable load** (eligible volume subset only) is selected as source. This avoids stalling on nodes whose high score is driven entirely by pinned or cooling-down volumes.

### 5.5 Metrics Provider Interface (Phase 2 — Planned)

> See Section 9 for the full interface definition. In Phase 1 the `NodeMetricsProvider` interface exists in the codebase but is not wired into `VolumeRebalancerReconciler`. The controller reads latency state exclusively from `StorageNode` CRs.

---

## 6. Volume Migration Algorithm

Run once per evaluation cycle for each enabled `StorageCluster`. **Phase 1 implementation.**

### Step 1 — Abort conditions

Skip migration if any of the following are true:
- The cluster is not fully online (any node in `in_restart`, `unreachable`, or `offline`).
- No node exceeds `imbalanceThreshold`% latency deviation (`nodesAboveThreshold` returns empty).
- Any migration is still tracked in `pendingMigrations` for this cluster. New migrations are not started until all in-flight migrations complete, preventing cascading load.

### Step 2 — Select source node

Nodes above the threshold are sorted by `latencyDeviationPct` descending (worst first). The controller iterates this list and selects the first node that has at least one eligible migration candidate. Eligible means:

- Volume `status == "online"`
- Volume `migrating == false` (guards against operator-restart edge case where cool-down map is empty)
- Not in the PVC-annotation pinned set (`simplyblock.io/pinned-volume`)
- Not in the in-memory cool-down map with a non-expired expiry

If no node has any eligible volume, emit `VolumeRebalancingBlocked` Warning and skip.

> **Compared to Phase 2:** Phase 1 does not evaluate `storageNodeCandidateCount` nodes for "migratable load" — it simply takes the worst node that has any eligible volume. Phase 2 will add a migratable-load comparison across the top-K hot nodes.

### Step 3 — Rank migration candidates

Take the eligible volume list from the selected source node. Compute `volumeIOScore` for each volume:

```
score(vol) = iopsWeight × vol.IOPS + throughputWeight × (vol.ThroughputBytesPerSec / 1e6)
```

Sort candidates by score descending — the volume contributing the most measured I/O is migrated first.

> **Compared to Phase 2:** Phase 1 uses a single combined IOPS + throughput score without a per-block-size weight table. Phase 2 will replace this with the sliding-window weighted score derived from SPDK Prometheus metrics.

### Step 4 — Select migration set

Goal: migrate volumes whose combined IO score is ≤ 10% of the source node's total eligible volume IO score.

```
totalVolScore = Σ score(vol) for eligible volumes on source node
budget        = 0.10 × totalVolScore
```

Greedy selection: add volumes to the migration set while `score(vol) ≤ remaining_budget` OR the set is still empty (always migrate at least one). Stop when `maxVolumeMigrationsPerCycle` is reached.

When `totalVolScore == 0` (no IOPS data from API): every candidate has `score = 0`, so `0 ≤ 0` is true for all after the first; up to `maxVolumeMigrationsPerCycle` volumes are migrated.

### Step 5 — Select target node

For each volume to migrate, the target is the node with the lowest `latencyDeviationPct` that is:

1. A different node than the source.
2. `status == "online"` and `health_check == true`.
3. In the same storage cluster.

Nodes with no latency data (not yet measured, `deviation = 0`) are treated as the best possible targets and rank first. If no valid target exists, skip the volume.

> **Compared to Phase 2:** Phase 1 selects by latency deviation. Phase 2 will add a projected-score guard: the target must remain below the source's weighted IOPS score after absorbing the migrated volume's load estimate.

### Step 6 — Emit migrations

Volumes are migrated sequentially within the cycle budget (`cycle_deadline = cycle_start + evaluationInterval`). Each migration:

1. Calls `POST /api/v2/clusters/{id}/migrations/` with `{volume_id, target_node_id}`.
2. On failure: log error, emit `VolumeRebalancingFailed` Warning, try next candidate.
3. On success: record in `coolDownMap` and `pendingMigrations{state: waiting_for_completion}`, emit `VolumeRebalancingStarted` Normal event.

The `processPendingMigrations` function runs at the start of every cycle and polls `GET /api/v2/clusters/{id}/migrations/{id}/` for all tracked entries. When `CompletedAt > 0`, the entry is removed and a `VolumeRebalancingComplete` (or `VolumeRebalancingFailed`) event is emitted. Entries not completing within 30 minutes trigger a `VolumeRebalancingStuck` Warning event.

### Step 7 — Update status

After each cycle, patch `status.rebalancingMetrics` with the current per-node deviation values, the worst/best node UUIDs, and `avgDeviationPct` / `maxDeviationPct`. The Prometheus gauge `simplyblock_rebalancer_max_latency_deviation_pct` is set to `maxDeviationPct`.

### Algorithm Summary (Phase 1 pseudocode)

```
for each enabled StorageCluster:
    latencyByNode = collectLatencyState()          // map[nodeUUID]{baselineP99, currentP99}
    deviations    = {n: deviationPct(n) for n in latencyByNode}
    maxDev        = max(deviations.values())
    threshold     = spec.imbalanceThreshold ?? 20

    if hasOfflineNode(): skip
    hotNodes = [n for n in deviations if deviations[n] > threshold]
               sorted by deviation descending

    if empty(hotNodes): skip

    // Step 2: first hot node with eligible volumes is source
    sourceNode = nil
    for node in hotNodes:
        eligible = volumes_on(node)
            .filter(status == "online")
            .filter(not migrating)
            .filter(not pinned)
            .filter(not in_cooldown)
            .sort_by(volumeIOScore, desc)
        if not empty(eligible):
            sourceNode = node
            candidates = eligible
            break

    if sourceNode == nil:
        emit Warning(VolumeRebalancingBlocked)
        continue

    // Step 4: budget selection
    totalScore = sum(volumeIOScore(v) for v in candidates)
    budget     = 0.10 * totalScore
    toMigrate  = []
    for vol in candidates:
        if empty(toMigrate) or volumeIOScore(vol) <= budget:
            toMigrate.append(vol)
            budget -= volumeIOScore(vol)
        if len(toMigrate) >= maxVolumeMigrationsPerCycle:
            break

    // Step 5: for each volume, pick target with lowest deviation
    for vol in toMigrate:
        if now >= cycle_deadline: defer remainder, emit Normal event; break
        target = min_deviation_node(
            online=true, healthy=true, uuid != sourceNode.uuid
        )
        if target == nil: skip vol
        migrate(vol, target)
        cooldown[vol.uuid] = now + defaultCoolDownSeconds
```

---

## 7. Cool-Down Mechanism

After a volume is successfully migrated, it enters a cool-down period during which it is excluded from future migration consideration.

- **Default:** 60 seconds (configurable via `spec.volumeRebalancing.defaultCoolDownSeconds`).
- **Per-volume override:** The `simplyblock.io/rebalancing-cooldown-seconds: "<value>"` annotation on the PVC overrides the cluster default for that volume. Value `0` means no cool-down.
- **Restart guard:** Even after cool-down map is cleared on restart, volumes with `Migrating == true` in the API response are excluded by `filterEligibleVolumes`.
- Cool-down state is stored in-memory only; see §4.4 for the rationale.

---

## 8. New Controller: VolumeRebalancer

### 8.1 Location

`internal/controller/volumerebalancer_controller.go`

### 8.2 Reconciliation Trigger

The controller does not watch for CR update events in the conventional sense. Evaluations happen at regular wall-clock intervals: at the start of each reconciliation a `cycle_start` timestamp is recorded and `RequeueAfter` is set to `evaluationInterval - elapsed` at the end. The `cycle_deadline = cycle_start + evaluationInterval` bounds total processing time so the next cycle always starts on schedule. If a cycle overruns, `RequeueAfter` is clamped to zero.

A `GenerationChangedPredicate` is also applied so configuration changes trigger an immediate reconcile.

### 8.3 Concurrency

The controller uses `MaxConcurrentReconciles: 1`. Multiple `StorageCluster` CRs across namespaces can be evaluated in separate goroutines.

### 8.4 Interaction with Existing Drain Controller

The existing `NodeDrainController` blocks new drains while `status.rebalancing == true`. The `VolumeRebalancerReconciler`:

1. Sets `status.rebalancing = true` before any migration API call.
2. Clears it to `false` (via deferred call) after all migrations in the cycle have completed.
3. Uses `r.Status().Patch` (not `Update`) to avoid clobbering concurrent status writes from the drain controller.

### 8.5 RBAC

```go
//+kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
```

---

## 9. New CRD: VolumeMigration

`VolumeMigration` (`storage.simplyblock.io/v1alpha1`) exposes manual volume migration as a first-class Kubernetes resource. It is the user-facing counterpart of the automatic rebalancer and reuses the same two-phase backend migration protocol.

### 9.1 Spec

```go
type VolumeMigrationSpec struct {
    // PVName is the PersistentVolume whose backing logical volume should be moved.
    PVName         string `json:"pvName"`
    // TargetNodeUUID is the destination storage node.
    TargetNodeUUID string `json:"targetNodeUUID"`
    // Abort requests cancellation of an in-progress migration.
    Abort          bool   `json:"abort,omitempty"`
}
```

### 9.2 Status and Phases

```
Pending → Validating → Running → Completed
                    ↓              ↓
                 Failed         Failed
         (abort) Aborted     (abort) Aborted
```

| Phase        | Meaning                                                                                              |
|--------------|------------------------------------------------------------------------------------------------------|
| `Pending`    | Accepted; PV and cluster/pool not yet resolved                                                       |
| `Validating` | `CreateMigration` called; NVMe paths being established and ANA-validated by a Job on the target node |
| `Running`    | `ContinueMigration` called; data transfer in progress                                                |
| `Completed`  | Migration finished successfully                                                                      |
| `Failed`     | Unrecoverable error (NVMe validation failed, API error, etc.)                                        |
| `Aborted`    | `spec.abort=true` was set; `CancelMigration` was called                                              |

Key status fields: `migrationID`, `clusterUUID`, `volumeUUID`, `poolUUID`, `sourceNodeUUID`, `connections []MigrationConnection`, `validationJobName`, `snapsTotal`, `snapsMigrated`, `startedAt`, `completedAt`, `errorMessage`.

### 9.3 Reconcile Flow

```
reconcileStart
  1. Resolve PV → CSI volume handle → volumeUUID
  2. Scan StorageClusters for the owning cluster/pool
  3. POST /migrations/  → MigrationDTO { id, connections[] }
  4. Store connections + migrationID in status → phase=Validating

reconcileValidating
  5. If no ValidationJob yet: resolve target node k8s hostname,
     get FioBenchmarkImage from StorageCluster, spawn Job on target node
  6. Job runs: nvme connect for each connection, then
     nvme list --verbose → verify all NQNs present with ANA state "inaccessible"
  7. Job success  → POST /migrations/continue → phase=Running
     Job failure  → POST /migrations/{id}/cancel → phase=Failed

reconcileRunning
  8. Poll GET /migrations/{id}/ (via shared PollMigration helper)
     CompletedAt>0 && no error → phase=Completed
     CompletedAt>0 && error    → phase=Failed

reconcileAbort (valid in Validating or Running)
  9. POST /migrations/{id}/cancel → phase=Aborted
```

The `VolumeMigrationReconciler` watches owned `batchv1.Job` objects so step 7 fires immediately when the validation Job terminates, without a polling delay.

### 9.4 Starting a migration programmatically

Internal callers (e.g. the rebalancer) use `volumemigration.StartMigration(ctx, k8sClient, volumeUUID, targetNodeUUID, name, namespace, ownerRefs)`, which resolves the volume UUID to a PV name and creates the `VolumeMigration` CR.

---

## 10. Autobalancing Package

The selection algorithm originally embedded in `VolumeRebalancerReconciler` was extracted into `internal/volumemigration/autobalancing` for testability and reuse.

### 10.1 Components

| Component               | File                         | Responsibility                                                                                                                                                          |
|-------------------------|------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `StorageNodeSelector`   | `storage_node_selector.go`   | Combines Prometheus current p99 + CR baseline per node; computes deviation per cluster; returns `[]NodeMigrationPair` (source→target, worst-first)                      |
| `LogicalVolumeSelector` | `logical_volume_selector.go` | Collects volumes from API, enriches with Prometheus IOPS/throughput, filters eligibility, selects the ranked migration set (10 % IO-budget cap + `MaxMigrations` limit) |
| `Rebalancer`            | `rebalancer.go`              | Wires the two selectors; produces `[]MigrationCandidate{ClusterUUID, SourceNodeUUID, TargetNodeUUID, Volume}`                                                           |

### 10.2 Key Types

```go
// StorageNodeSelectorInput groups nodes by namespace (for baseline CR lookup).
type StorageNodeSelectorInput struct {
    Namespace    string
    StorageNodes []volumemigration.StorageNode
}

// NodeMigrationPair is one source→target pairing from StorageNodeSelector.
type NodeMigrationPair struct {
    ClusterUUID, SourceNodeUUID, TargetNodeUUID string
}

// MigrationCandidate is the final output: one volume ready to migrate.
type MigrationCandidate struct {
    ClusterUUID, SourceNodeUUID, TargetNodeUUID string
    Volume VolumePlacement
}

// ClusterDeviationStats holds per-cluster aggregate stats after one cycle.
type ClusterDeviationStats struct {
    MaxDeviationPct, AvgDeviationPct float64
    HottestNodeUUID, CoolestNodeUUID string
}
```

### 10.3 Multi-cluster Prometheus query

`StorageNodeSelector` calls `promlatency.GetClustersCurrentP99(ctx, clusterUUIDs)` which issues a single PromQL query with a `cluster=~"uuid1|uuid2|..."` regex matcher, returning `map[clusterUUID]map[nodeUUID]int64`. This replaces the former per-cluster `GetClusterCurrentP99` loop.

### 10.4 Rebalancer entry point

```go
candidates, err := rebalancer.SelectMigrations(ctx, cfg, isCoolingDown, inputs...)
// isCoolingDown func(clusterUUID, volumeUUID string) bool
```

`VolumeRebalancerReconciler` calls this once per cycle, then calls `executeMigrations` on the returned candidates. The `isCoolingDown` closure bridges the reconciler's in-memory `MigrationState` into the Rebalancer.

---

## 11. Metrics Provider Interface

> **Phase 2.** The interface exists and the `PrometheusMetricsProvider` and `UniformMetricsProvider` implementations are present in the codebase, but `VolumeRebalancerReconciler` does not invoke them in Phase 1. They will be wired in Phase 2 for the sliding-window IOPS scoring.

### 9.1 Core Types

```go
type IOOperation string
const (
    IOOperationRead  IOOperation = "read"
    IOOperationWrite IOOperation = "write"
)

type ErasureScheme string
const (
    ErasureScheme1Plus1 ErasureScheme = "1+1"
    ErasureScheme2Plus1 ErasureScheme = "2+1"
    ErasureScheme4Plus1 ErasureScheme = "4+1"
    ErasureScheme1Plus2 ErasureScheme = "1+2"
    ErasureScheme2Plus2 ErasureScheme = "2+2"
    ErasureScheme4Plus2 ErasureScheme = "4+2"
)

type BlockSizeIOMetrics struct {
    BlockSizeBytes int64
    Operation      IOOperation
    ErasureScheme  ErasureScheme
    IOPS           float64
    BytesPerSecond float64
}

type NodeMetrics struct {
    NodeUUID    string
    CollectedAt time.Time
    IO          []BlockSizeIOMetrics
}
```

### 9.2 Interface Definition

```go
type NodeMetricsProvider interface {
    GetNodeMetrics(ctx context.Context, clusterUUID, nodeUUID string) (*NodeMetrics, error)
    GetClusterMetrics(ctx context.Context, clusterUUID string) (map[string]*NodeMetrics, error)
}
```

### 9.3 Planned Implementations

| Implementation              | Package                       | When to use                                                     |
|-----------------------------|-------------------------------|-----------------------------------------------------------------|
| `PrometheusMetricsProvider` | `internal/metrics/prometheus` | Phase 2 default. PromQL against SPDK-exported per-node metrics. |
| `UniformMetricsProvider`    | `internal/metrics/uniform`    | Returns IOPS=1 everywhere. No-op for scoring; used in tests.    |
| `StaticMetricsProvider`     | `internal/metrics/static`     | Unit/integration tests with fixture data.                       |

### 9.4 Prometheus Metric Schema (Phase 2)

```
# Per-node IOPS by blocksize/operation/erasure-scheme
simplyblock_node_iops_total{cluster, node, blocksize, operation, erasure_scheme}

# Phase 1 sidecar latency (already implemented)
simplyblock_node_fio_write_latency_p50_ns{cluster, node}
simplyblock_node_fio_write_latency_p99_ns{cluster, node}
```

Label names must be agreed upon with the SPDK team for Phase 2.

---

## 12. Backend API Requirements

### 12.1 Volume listing — placement and status fields (Phase 1)

The `GET /api/v2/clusters/{id}/storage-pools/{id}/volumes/` endpoint is used exclusively for volume placement and eligibility checks. Per-volume IOPS and throughput are now read from Prometheus (see §5.3) — not from this endpoint.

Required fields:

```json
{
  "id": "<volumeUUID>",
  "storage_node_id": "<nodeUUID>",
  "status": "online",
  "migrating": false,
  "capacity": { "size_used": 1073741824 }
}
```

The `iops` and `throughput_bytes_per_sec` fields are consumed if present (REST API fallback for volumes not yet in Prometheus), but are not required.

### 10.2 Migration API

### Migration is a two-phase API protocol

Volume migration requires two separate API calls separated by an NVMe path
establishment step performed by the operator on the target storage node:

```
Operator                           Backend API                     Target node kernel
   │                                    │                                │
   │── POST /migrations/ ──────────────▶│                                │
   │◀─ MigrationDTO {id, connections} ──│                                │
   │                                    │                                │
   │── spawn Job on target node ────────────────────────────────────────▶│
   │        nvme connect (each LvolConnectResp)                          │
   │◀─ Job completed ────────────────────────────────────────────────────│
   │                                    │                                │
   │── POST /migrations/continue ──────▶│                                │
   │◀─ MigrationDTO (updated status) ───│                                │
   │                                    │                                │
   │── GET /migrations/{id}/ (poll) ───▶│                                │
   │◀─ MigrationDTO {completed_at>0} ───│                                │
```

**`POST /api/v2/clusters/{clusterUUID}/migrations/`**

Creates the internal infrastructure for the migration on both the control plane
and storage plane. Returns new NVMe-oF connection parameters for the target-side
paths that the operator must establish before the data movement can begin.

Request:
```json
{ "volume_id": "<uuid>", "target_node_id": "<uuid>" }
```

Response (`MigrationDTO`):
```json
{
  "id": "<migrationUUID>",
  "lvol_id": "<volumeUUID>",
  "source_node_id": "<uuid>",
  "target_node_id": "<uuid>",
  "phase": "string",
  "status": "string",
  "started_at": 1748000000,
  "completed_at": 0,
  "connections": [
    {
      "nqn": "nqn.2023-02.io.simplyblock:...",
      "ip": "192.168.10.69",
      "port": 4430,
      "transport": "tcp",
      "nr-io-queues": 3,
      "reconnect-delay": 1,
      "ctrl-loss-tmo": 4
    }
  ]
}
```

**NVMe path establishment (operator-side, between the two API calls)**

After receiving the `connections` array the operator spawns a short-lived
Kubernetes Job on the **target storage node** (pinned via
`nodeSelector: kubernetes.io/hostname`). The Job uses the same fio-bench image
(which includes `nvme-cli`) and runs `nvme connect` for each entry:

```
sudo nvme connect -t <transport> -a <ip> -s <port> -n <nqn> \
    --nr-io-queues=<nr-io-queues> \
    --reconnect-delay=<reconnect-delay> \
    --ctrl-loss-tmo=<ctrl-loss-tmo>
```

The Job mounts `/dev` from the host (`hostPath: /dev`) and runs with
`HostNetwork: true`, identical to the baseline Job. On success (exit 0) the
operator calls `ContinueMigration`. On failure the migration is cancelled via
`CancelMigration`.

**`POST /api/v2/clusters/{clusterUUID}/migrations/continue`**

Kicks off the actual data movement after the operator has confirmed that all
NVMe paths returned by `CreateMigration` are reachable on the target node.

Request:
```json
{ "migration_id": "<migrationUUID>" }
```

Response: updated `MigrationDTO`.

**`GET /api/v2/clusters/{clusterUUID}/migrations/{migrationUUID}/`**

Returns current migration status. `completed_at > 0` signals completion. `error_message` non-empty signals failure.

### 10.3 Benchmark volume connection

No API calls are needed to discover the benchmark volume. Each storage node automatically hosts a benchmark volume whose logical volume ID equals the storage node UUID. The operator derives all connection parameters independently:

| Parameter   | Source                                                           |
|-------------|------------------------------------------------------------------|
| NQN         | `fmt.Sprintf("%s:lvol:%s", StorageCluster.Status.NQN, nodeUUID)` |
| TCP address | `NodeStatus.MgmtIp`                                              |
| TCP port    | `NodeStatus.NvmfPort` (fallback: 4420)                           |

The benchmark volume lifecycle is managed entirely by the backend; the operator neither creates nor deletes it.

### 10.4 Remaining Phase 2 API endpoints

The per-node, per-blocksize I/O metric endpoint described in earlier drafts is deferred to Phase 2:

**`GET /api/v2/clusters/{id}/storage-nodes/{id}/metrics`** — per-blocksize IOPS/latency breakdown. Required for the sliding-window weighted score.

---

## 13. Configuration

### 13.1 Cluster-Level Defaults

| Field                         | Default  | Phase | Description                                                                                                  |
|-------------------------------|----------|-------|--------------------------------------------------------------------------------------------------------------|
| `enabled`                     | `true`   | 1     | Activates the rebalancing loop                                                                               |
| `evaluationInterval`          | `60s`    | 1     | How often the rebalancer runs                                                                                |
| `imbalanceThreshold`          | `20` (%) | 1     | Minimum latency deviation % above baseline to trigger migration                                              |
| `defaultCoolDownSeconds`      | `60`     | 1     | Cool-down after migration                                                                                    |
| `maxVolumeMigrationsPerCycle` | `10`     | 1     | Hard cap on volumes migrated per cycle                                                                       |
| `iopsWeight`                  | `1.0`    | 1     | Weight for per-volume IOPS in the volume priority score                                                      |
| `throughputWeight`            | `0.1`    | 1     | Weight for per-volume throughput (MB/s) in the volume priority score                                         |
| `prometheusURL`               | —        | 1     | **Required.** Prometheus endpoint scraped for `simplyblock_node_fio_write_latency_p99_ns`                    |
| `latencyBenchmarkEnabled`     | `false`  | 1     | Enables fio sidecar + baseline Job; requires `fioBenchmarkImage`                                             |
| `latencyBenchmarkInterval`    | `5m`     | 1     | Operator periodic reconcile cadence; baseline Job completion is also handled immediately via owned-Job watch |
| `fioBenchmarkImage`           | —        | 1     | Container image for fio Jobs and sidecar (requires fio, nvme-cli, jq, prometheus-node-exporter)              |
| `metricsBackend`              | —        | 2     | Data source for IOPS metrics (`prometheus` or `controlplane`); no-op in Phase 1                              |

### 11.2 Volume-Level Overrides (Annotations)

| Annotation                                    | Description                                                                    |
|-----------------------------------------------|--------------------------------------------------------------------------------|
| `simplyblock.io/rebalancing-cooldown-seconds` | Per-volume cool-down override (set on the PVC)                                 |
| `simplyblock.io/pinned-volume`                | Excludes the volume from rebalancing; any non-empty value is treated as `true` |

---

## 14. Observability

### 14.1 Prometheus Metrics

| Metric                                              | Type    | Labels                                  | Description                                                                                            |
|-----------------------------------------------------|---------|-----------------------------------------|--------------------------------------------------------------------------------------------------------|
| `simplyblock_rebalancer_evaluation_total`           | Counter | `cluster`, `result`                     | Evaluation cycles by outcome (`skipped`, `migrated`, `blocked`, `error`)                               |
| `simplyblock_rebalancer_migrations_total`           | Counter | `cluster`, `source_node`, `target_node` | Volume migrations initiated                                                                            |
| `simplyblock_rebalancer_max_latency_deviation_pct`  | Gauge   | `cluster`                               | Maximum p99 write latency deviation from per-node baseline, in percent                                 |
| `simplyblock_rebalancer_node_latency_deviation_pct` | Gauge   | `cluster`, `node`                       | Per-node p99 write latency deviation from baseline, in percent (Phase 1); weighted I/O score (Phase 2) |
| `simplyblock_rebalancer_cooldown_volumes`           | Gauge   | `cluster`                               | Volumes currently in cool-down                                                                         |
| `simplyblock_rebalancer_pinned_blocked_total`       | Counter | `cluster`                               | Times rebalancing was blocked by pinned/cooling-down volumes                                           |
| `simplyblock_node_fio_write_latency_p50_ns`         | Gauge   | `cluster`, `node`                       | Sidecar p50 4K write latency (ns)                                                                      |
| `simplyblock_node_fio_write_latency_p99_ns`         | Gauge   | `cluster`, `node`                       | Sidecar p99 4K write latency (ns)                                                                      |

### 12.2 Kubernetes Events

All events are emitted on the `StorageCluster` object.

| Reason                      | Type    | Message                                                                                  |
|-----------------------------|---------|------------------------------------------------------------------------------------------|
| `VolumeRebalancingStarted`  | Normal  | `Initiating migration of volume <uuid> from node <src> to <dst> (latency deviation: N%)` |
| `VolumeRebalancingComplete` | Normal  | `Migration <id> of volume <uuid> completed successfully`                                 |
| `VolumeRebalancingFailed`   | Warning | `Migration of volume <uuid> to node <dst> failed: <error>`                               |
| `VolumeRebalancingStuck`    | Warning | `Migration <id> of volume <uuid> has not completed after 30 minutes`                     |
| `VolumeRebalancingBlocked`  | Warning | `All N latency-degraded nodes have no eligible migration candidates`                     |
| `VolumeRebalancingDeferred` | Normal  | `Cycle deadline reached; N migration candidate(s) deferred to next cycle`                |

---

## 15. Testing Strategy

### 15.1 Unit Tests (Phase 1 — Implemented)

All pure functions in `volumerebalancer_scoring.go` and the controller helper methods are covered without external dependencies.

#### Latency Deviation (§5.2)

| #    | Scenario                                                                                        | Type                |
|------|-------------------------------------------------------------------------------------------------|---------------------|
| U-01 | `computeLatencyDeviationPct` — zero, equal, above, below baseline; negative/zero inputs         | Positive + Negative |
| U-02 | Fractional precision: `1333 ns / 1000 ns baseline = 33.3%`                                      | Positive            |
| U-03 | Threshold boundary: `deviation == threshold` is NOT a hot node (strict `>`); `threshold + ε` IS | Boundary            |

#### Volume IO Score (§5.3)

| #    | Scenario                                                                                | Type     |
|------|-----------------------------------------------------------------------------------------|----------|
| U-04 | Weight formula: IOPS-only, throughput-only, combined, custom weights, zeros             | Positive |
| U-05 | Throughput bytes→MB conversion: `100e6 bytes/s × 0.1 = 10 units`                        | Positive |
| U-06 | Ranking: three volumes with different IOPS/throughput combinations are sorted correctly | Positive |
| U-07 | Throughput tie-break: equal IOPS, higher throughput → higher score                      | Positive |

#### Hot Node Selection

| #    | Scenario                                               | Type     |
|------|--------------------------------------------------------|----------|
| U-08 | `nodesAboveThreshold` returns worst-first sorted list  | Positive |
| U-09 | Node at exactly the threshold is excluded (strict `>`) | Boundary |
| U-10 | None above threshold → empty result                    | Negative |
| U-11 | Zero threshold: any positive deviation is hot          | Boundary |
| U-12 | Empty deviations map → empty result                    | Negative |
| U-13 | `topKNodes` k=0, k=1, k > map size, empty map          | Boundary |

#### Deviation Statistics

| #    | Scenario                                                   | Type     |
|------|------------------------------------------------------------|----------|
| U-14 | `deviationStats` basic: correct max, avg, hottest, coolest | Positive |
| U-15 | Empty map: all zero/empty                                  | Negative |
| U-16 | Single node: max = avg = node deviation                    | Boundary |
| U-17 | Two-node cluster: correct pair                             | Positive |
| U-18 | All-zero deviations                                        | Boundary |

#### Target Selection (Step 5)

| #    | Scenario                                                         | Type     |
|------|------------------------------------------------------------------|----------|
| U-19 | Picks node with lowest deviation                                 | Positive |
| U-20 | Offline and unhealthy nodes excluded                             | Negative |
| U-21 | No valid targets → returns empty string                          | Negative |
| U-22 | Unmeasured node (deviation=0) ranks above measured-but-good node | Positive |
| U-23 | Two-node cluster: picks the only target                          | Boundary |
| U-24 | All targets degraded: still picks the least degraded             | Positive |

#### Eligible Volume Filtering (Step 2)

| #    | Scenario                                              | Type     |
|------|-------------------------------------------------------|----------|
| U-25 | All clean: all volumes returned                       | Positive |
| U-26 | Pinned volume excluded                                | Negative |
| U-27 | Non-online status excluded                            | Negative |
| U-28 | `Migrating == true` excluded (operator-restart guard) | Negative |
| U-29 | Active cool-down excluded                             | Negative |
| U-30 | Expired cool-down: volume eligible again              | Positive |
| U-31 | Mixed batch: only the single eligible volume returned | Positive |

---

### 15.1b Unit Tests — Autobalancing Package (Implemented)

Tests live in `internal/volumemigration/autobalancing/autobalancing_test.go`.

| #    | Function                          | Scenario                                                                                                                                                                                        |
|------|-----------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| A-01 | `distinctClusterUUIDs`            | Deduplication across multiple inputs; empty UUID skipped; nil input                                                                                                                             |
| A-02 | `deviationStats`                  | Single/multi cluster grouping; hottest/coolest/avg/max correctness; node absent from latency map ignored                                                                                        |
| A-03 | `FilterEligibleVolumes`           | All five exclusion reasons (pinned, cooling-down, not-online, migrating, all excluded); nil IsCoolingDown does not panic                                                                        |
| A-04 | `SelectVolumesForMigration`       | Always includes first candidate; 10 % budget cap excludes mid-range; MaxMigrations hard cap; zero-score fallback includes all; skip node with no eligible volumes; descending IO-score ordering |
| A-05 | `SelectStorageNodes` (pure logic) | Hot/cool pairing via deviationStats; single-node cluster produces no pair                                                                                                                       |
| A-06 | `isCoolingDown` closure           | Correctly captures clusterUUID per cluster (loop variable capture guard)                                                                                                                        |

### 15.1c Unit Tests — Phase 2 (Planned)

When the sliding-window IOPS scoring is implemented the following test cases will be added:

| #       | Scenario                                                                              |
|---------|---------------------------------------------------------------------------------------|
| U-P2-01 | Correct weight for every blocksize/erasure-scheme/direction in the weight table       |
| U-P2-02 | Unknown erasure scheme returns error                                                  |
| U-P2-03 | Exponential decay at 1×, 2×, 3× half-life produces 0.5, 0.25, 0.125                   |
| U-P2-04 | Samples older than 3× half-life are excluded                                          |
| U-P2-05 | Node-size normalisation: 2× capacity node gets half the effective score for equal I/O |
| U-P2-06 | Single node: imbalance = 0, no migration                                              |
| U-P2-07 | All nodes identical scores: imbalance = 0                                             |
| U-P2-08 | `storageNodeCandidateCount` clamped to actual node count                              |
| U-P2-09 | Highest-score node with all volumes pinned: next candidate selected                   |

---

### 13.2 Integration Tests

Run the full controller reconciliation loop against a mock backend HTTP server and a real Kubernetes API via `envtest`.

| #    | Scenario                                                                                            | Type     |
|------|-----------------------------------------------------------------------------------------------------|----------|
| I-01 | Latency deviation above threshold → migration initiated, `status.rebalancing` transitions correctly | Positive |
| I-02 | Latency deviation below threshold → no migration, metrics updated                                   | Negative |
| I-03 | Migration API returns 500 → error event emitted, next candidate attempted                           | Negative |
| I-04 | Migration completes → cool-down entry created, `VolumeRebalancingComplete` event emitted            | Positive |
| I-05 | `status.rebalancing = true` during active cycle blocks concurrent node drain                        | Positive |
| I-06 | PVC with `simplyblock.io/pinned-volume` → volume never passed to migration API                      | Positive |
| I-07 | All hot volumes pinned → `VolumeRebalancingBlocked` warning                                         | Negative |
| I-08 | Volume with `Migrating == true` excluded after operator restart (no cool-down state)                | Negative |
| I-09 | `spec.volumeRebalancing.enabled = false` → loop stops within one cycle                              | Positive |
| I-10 | Pending migration blocks new migration cycle until completion                                       | Positive |
| I-11 | `processedPendingMigrations` removes entry and emits event when `CompletedAt > 0`                   | Positive |
| I-12 | Migration not completed after 30 min → `VolumeRebalancingStuck` warning                             | Negative |

### 13.3 End-to-End Tests

Run against a real SimplyBlock cluster with real fio latency measurements.

| #    | Scenario                                                                           | Type     |
|------|------------------------------------------------------------------------------------|----------|
| E-01 | Inject I/O load on one node; verify migration within 2 evaluation cycles           | Positive |
| E-02 | Balanced cluster; no migration over 5 cycles                                       | Negative |
| E-03 | Overloaded node with all pinned volumes → `VolumeRebalancingBlocked`, no migration | Negative |
| E-04 | Remove pin annotation → volume migrated in next cycle                              | Positive |
| E-05 | Volume not re-migrated within cool-down window                                     | Negative |
| E-06 | Cool-down expires → volume eligible again                                          | Positive |
| E-07 | Continuous I/O during migration → no I/O errors                                    | Positive |
| E-08 | Operator restart mid-migration → migrating volume excluded on restart              | Negative |
| E-09 | 2-node cluster load imbalance → migration proceeds correctly                       | Positive |
| E-10 | Two storage clusters: rebalancing in cluster A does not affect cluster B           | Positive |

---

## 16. Open Questions

| # | Question                                                                                                                                                                                                                                                                                                                                                                                | Owner             |
|---|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------|
| 1 | ~~**IOPS/throughput per volume from REST API**~~ **Resolved.** Per-volume IOPS and throughput are read directly from Prometheus. REST API values are the fallback.                                                                                                                                                                                                                      | Resolved          |
| 2 | **Phase 2 SPDK Prometheus metric schema:** Exact metric names and label keys for per-node, per-blocksize IOPS series must be agreed with the SPDK team.                                                                                                                                                                                                                                 | SPDK/Backend team |
| 3 | ~~**Helm/OLM RBAC sync**~~ **Resolved.** The Helm chart now includes hand-written RBAC for the new controllers and the VolumeMigration CRD.                                                                                                                                                                                                                                             | Resolved          |
| 4 | **`simplyblock.io/pinned-volume` — backend hard-pin:** Does the backend also enforce a hard-pin? Currently only the PVC annotation is checked.                                                                                                                                                                                                                                          | Backend team      |
| 5 | **Sidecar ConfigMap injection latency:** Race condition possible if a pod is scheduled before `StorageNodeLatencyReconciler` has written the ConfigMap entry.                                                                                                                                                                                                                           | —                 |
| 6 | **Per-volume replica placement for scoring (Phase 2):** Secondary/tertiary LVS placement data not available from current API.                                                                                                                                                                                                                                                           | SPDK/Backend team |
| 7 | **ANA state field name in nvme-cli JSON:** The validation Job checks `sub.get('ANA_State', '')` against `"inaccessible"`. The exact JSON field name produced by `nvme list --verbose --output-format=json` must be confirmed against the nvme-cli version shipped in the fio-bench image.                                                                                               | —                 |
| 8 | **VolumeMigration and the rebalancer:** The `VolumeMigrationReconciler` and the `VolumeRebalancerReconciler` both call `CreateMigration` independently. If both attempt to migrate the same volume simultaneously the backend will reject one. A coordination mechanism (e.g. checking the `Migrating` field before creating a `VolumeMigration` CR in the rebalancer) should be added. | —                 |
