# Design Document: SSE Push Notifications — Control-Plane Event Streaming into the Operator

**Status:** Final
**Author:** Christoph Engelbert
**Date:** 2026-08-03

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Control-Plane SSE Contract](#3-control-plane-sse-contract)
4. [Architecture Overview](#4-architecture-overview)
5. [Component Design](#5-component-design)
6. [Consistency and Correctness](#6-consistency-and-correctness)
7. [Reliability and Connection Lifecycle](#7-reliability-and-connection-lifecycle)
8. [Leader Election](#8-leader-election)
9. [ID → CR Mapping](#9-id--cr-mapping)
10. [CSI](#10-csi)
11. [Implementation Phases](#11-implementation-phases)
12. [References](#12-references)

---

## 1. Background

The operator is polling-based. Every controller learns about simplyblock
control-plane state by calling the HTTP API from inside `Reconcile` and
re-triggering itself with `ctrl.Result{RequeueAfter: ...}` — 200+ such requeue
sites, on intervals from 5s to 120s. `ControlPlaneReconciler.Reconcile`
(`operator/internal/controller/controlplane_controller.go`) is the canonical
example: it polls `/api/v2/_meta/ready` and returns `RequeueAfter: 30s` on both
the success and failure paths. Control-plane readiness that fans out to other
controllers does so CR→CR (the `StorageNodeSet` controller watches the
`ControlPlane` CR that `ControlPlaneReconciler` updates from its poll) — still
poll-sourced at the root.

This model has two costs:

| Cost | Cause |
|---|---|
| **Latency** | A change in the control plane is invisible until the next requeue tick — worst case ~2 minutes. |
| **API load** | Every controller re-reads the control plane every few seconds regardless of whether anything changed, on every replica. |

The control-plane v2 API provides **Server-Sent-Events push notifications**: any
list or detail endpoint opened with `?watch=true` returns a live
`text/event-stream` instead of a JSON body. This design consumes that stream to
make reconciliation push-driven: the control plane tells the operator what
changed, and the operator reacts immediately and reads the change locally instead
of polling.

The trigger mechanism is controller-runtime's **`source.Channel`** — a `Source`
backed by a Go channel of `event.GenericEvent`, attached via
`WatchesRawSource(...)`. Kubernetes `corev1.Event` objects are not used as a
reconcile trigger: they are an observability primitive (surfaced by
`kubectl describe`, already produced via the operator's `EventRecorder`),
unindexed, best-effort, and TTL'd. In controller-runtime v0.24 (this repo's
version) `source.Channel` is a function returning a `SyncingSource`, attached with
`.WatchesRawSource(...)`.

---

## 2. Goals and Non-Goals

### Goals

- Push-driven reconciliation for every resource the control plane streams:
  react to changes in sub-second time instead of on a 5–120s poll tick.
- Eliminate redundant reads. The stream carries the full object (identical to the
  REST DTO), so the operator holds control-plane state in memory and reconcilers
  read it locally — a **control-plane informer**. Reconcilers do not re-fetch what
  the event already delivered.
- Preserve level-triggered correctness: the stream is a live cache plus a trigger,
  never an edge-triggered command log. A dropped stream degrades to the polling
  backstop, never to silent failure.
- Incremental, per-resource adoption behind a polling backstop.

### Non-Goals

- The CSI driver. CSI is gRPC-invoked by kubelet and reconciles nothing; it stays
  request/response. See [§10](#10-csi).
- Changes to the control-plane API surface. The operator is a pure consumer.
- Replacing the operator's own CRD watches, which remain informer-backed.

---

## 3. Control-Plane SSE Contract

This is the wire contract the operator parses. The control-plane `sse` branch
tests are the authoritative specification.

### 3.1 Transport and framing

- FastAPI + `sse-starlette`; each stream is an async generator wrapped in
  `EventSourceResponse` (`simplyblock_web/api/v2/_sse.py`).
- `Content-Type: text/event-stream`, plus `Cache-Control: no-store` and
  `X-Accel-Buffering: no` (defeats proxy buffering).
- SSE framing uses `event:` named types, `data:` payloads, and `retry: 3000`.
  No `id:` field is emitted.

### 3.2 Endpoint coverage

`?watch=true` is honored on all seven v2 resource types, in both list and detail
form: clusters, storage-pools, storage-nodes, devices, snapshots, volumes, tasks.
It is a shared mechanism, wired per route via an `if watch: return sse_response(...)`
branch. `watch` is a FastAPI query param (`WatchParam`).

### 3.3 Event types and payloads

| `event:` | Meaning | `data:` |
|---|---|---|
| `snapshot` | Sent once, first, on every (re)connect | Full current state — a JSON **array** of DTOs (list) or a single DTO (detail) |
| `created` | Entity entered the filtered set | Full DTO |
| `updated` | Entity changed | Full DTO (detail streams always use `updated`, never `created`) |
| `deleted` | Entity left the filtered set | Final DTO with `status: "deleted"` (soft-delete) or `{}` (physical removal) |
| `error` | Backend failure | `{"detail": "backend unavailable"}`, then the stream closes |

The DTO is produced by the same `from_model` builder as the REST route; a streamed
object is byte-identical to what a REST `GET` returns. Operation is carried by the
`event:` name, not by a field inside `data`. No-op updates whose DTO is
byte-identical to the prior one are suppressed server-side.

### 3.4 Origin of events

Events originate from a FoundationDB change-watch. Every write/remove of a watched
model atomically bumps a hierarchical version index in the same FDB transaction
(`simplyblock_core/models/base_model.py`). A shared `ScopeWatch` thread sets one
FDB `watch()` per scope, diffs versions on fire to find exactly which entities
changed, point-reads those, and fans them to subscribers
(`simplyblock_core/watch.py`, `watches.py`). A 30s reconcile re-scan is the
server-side safety net (`WATCH_RECONCILE_SEC`); it also bounds the lag for changes
written by components that do not maintain the index.

### 3.5 Scoping

Watches are scoped to path params via each model's `watch_scope()`:

| Resource | Scope | Streams required by the operator |
|---|---|---|
| Volume (`LVol`) | `(pool_uuid,)` | One per pool |
| Snapshot | `(pool_uuid,)` | One per pool |
| Pool | `(cluster_id,)` | One per cluster |
| Storage node | `(cluster_id,)` | One per cluster |
| Task (`JobSchedule`) | `(cluster_id,)` | One per cluster |
| Cluster | `()` (root) | One total |
| Device | — (watches parent `StorageNode`) | Exploded from the node stream |

Membership matches the REST list endpoint exactly (the watch reuses the same
`select` getter as the filter). Deleting a parent (e.g. a pool) closes its child
streams server-side; a reconnect then returns 404.

### 3.6 Reliability properties

| Property | Value |
|---|---|
| Resume / replay | None. No `id:`, `Last-Event-ID` ignored, no buffer. Reconnect delivers a fresh snapshot. |
| Heartbeat | `: ping <ts>` comment every 15s (`PING_SEC`). |
| Forced close | Hard 1-hour stream lifetime cap (`WATCH_MAX_LIFETIME_SEC`) — clean close, reconnect expected. |
| Delivery | Coalescing — a slow subscriber receives latest state, not a backlog; intermediate transitions may be skipped. |
| Backpressure / errors | FDB errors retry `(1,2,4)s`, then `event: error` + close. DB down at connect returns 503. |
| Concurrency cost | One shared FDB watch + thread per scope, reused across all subscribers of that scope. |
| Auth | Same v2 bearer (admin SA JWT or cluster secret), validated only at connect; the 1h cap bounds a revoked token. |

Two properties shape the operator design: snapshot-on-connect (the reconnect
resync is provided by the server) and coalescing delivery (the stream delivers
current truth, not an ordered edit log — inherently level-triggered).

---

## 4. Architecture Overview

```
                     simplyblock control plane (v2 API)
                                   │
         GET /clusters/{c}/storage-pools/{p}/volumes/?watch=true
         GET /clusters/{c}/storage-nodes/?watch=true   ... (per scope)
                                   │  text/event-stream
                                   ▼
┌───────────────────────────────────────────────────────────────────────────┐
│ Operator process (leader only)                                            │
│                                                                           │
│  ┌───────────────────────────┐     opens/closes streams per scope         │
│  │ Subscription Manager      │◄──── driven by operator's own CR watches   │
│  │ (manager.Runnable,        │       (Pool CRs → which pools to watch …)  │
│  │  NeedLeaderElection=true) │                                            │
│  └───────────┬───────────────┘                                            │
│              │ one goroutine per active stream                            │
│              ▼                                                            │
│  ┌──────────────────────────┐   snapshot seeds / events mutate            │
│  │ Control-Plane Informer   │──────────────┐                              │
│  │  • Store (keyed by id)   │              │ (1) update store             │
│  │  • Lister (Get/List)     │              │ (2) enqueue GenericEvent     │
│  └───────────┬──────────────┘              │                              │
│              │ local, in-memory reads      ▼                              │
│              │                    ┌──────────────────┐                    │
│              │                    │  source.Channel  │────┐               │
│              ▼                    └────────┬─────────┘    │               │
│  ┌─────────────────────────┐       WatchesRawSource       │               │
│  │ Reconcilers             │◄───────────────────────── enqueues           │
│  │  read Lister, not API   │   reconcile.Request                          │
│  └─────────────────────────┘                                              │
│                                                                           │
│  (slow RequeueAfter backstop remains on every controller)                 │
└───────────────────────────────────────────────────────────────────────────┘
```

The design is a re-implementation of client-go's `SharedIndexInformer` +
`Lister` pattern, sourced from SSE instead of the Kubernetes API server. Three
cooperating pieces:

1. **Subscription Manager** — a leader-only `manager.Runnable` that determines
   which scopes to stream and opens/closes stream goroutines as the operator's own
   CRs come and go.
2. **Control-Plane Informer** — per resource, a thread-safe store keyed by
   control-plane id, seeded by the `snapshot` frame and mutated by
   `created`/`updated`/`deleted`. It exposes a `Lister` for reconcilers and, for
   every applied event, pushes a `GenericEvent` onto a channel.
3. **`source.Channel` wiring** — each participating controller attaches the
   informer's channel via `WatchesRawSource`, mapping each event to the
   `reconcile.Request`(s) for the affected CR(s). Reconcilers read the `Lister`
   instead of calling the control-plane API.

The informer exposes the atlas-lib generated v2 DTO types directly; the SSE `data`
deserializes into those types, which reconcilers consume unchanged. New
control-plane access lands on the typed v2 `atlas-lib` client
(`atlas-lib/controlplane`), not `internal/webapi`.

---

## 5. Component Design

### 5.1 Subscription Manager (`manager.Runnable`)

Watches are scoped, so the operator holds one stream per scope instance — one
volume stream per pool, one node stream per cluster, and so on. The manager is
dynamic, driven by the operator's own CRs:

- On start (as leader), it enumerates the scopes to watch from existing CRs
  (`StorageCluster`, `Pool`, …) and opens a stream goroutine for each.
- It watches those CRs; a new scope (e.g. a new `Pool`) opens a stream, and a
  removed scope cancels the stream's context. The server also closes child streams
  when a parent is deleted; the manager tolerates both orders.
- It registers via `mgr.Add(...)` as a `manager.Runnable` and
  `manager.LeaderElectionRunnable` with `NeedLeaderElection() → true`
  ([§8](#8-leader-election)).

```go
type SubscriptionManager struct {
    client    client.Client            // operator CR client, to discover scopes
    cp        *controlplane.Client     // atlas-lib typed v2 client (SSE-capable)
    informers map[Resource]*Informer   // one informer per resource type
    streams   map[ScopeKey]context.CancelFunc
}

func (m *SubscriptionManager) NeedLeaderElection() bool { return true }

func (m *SubscriptionManager) Start(ctx context.Context) error {
    // reconcile desired scope set from CRs; open/close stream goroutines;
    // block until ctx is cancelled (manager shutdown / lost leadership).
}
```

Each stream goroutine runs the connect → parse → apply loop of [§5.3](#53-stream-goroutine)
with the reconnect/backoff of [§7](#7-reliability-and-connection-lifecycle).

### 5.2 Control-Plane Informer (Store + Lister)

Per resource type, a thread-safe store keyed by control-plane id, holding the
atlas-lib generated v2 DTO.

```go
type Informer[T Identifiable] struct {
    mu    sync.RWMutex
    items map[ScopeKey]map[string]T   // scope → id → object
    ch    chan event.GenericEvent     // fed into source.Channel
}

// Lister is what reconcilers use instead of the HTTP client.
type Lister[T any] interface {
    Get(scope ScopeKey, id string) (T, bool) // local, no network
    List(scope ScopeKey) []T                  // local, no network
}
```

Two writers touch the store: the stream goroutine (snapshot/events) and the
mutation path ([§6.2](#62-read-your-writes-after-mutations)).

### 5.3 Stream goroutine

For each active scope:

```
connect(scope) with bearer auth + ?watch=true
loop over SSE frames:
  event: snapshot  → replaceScope(scope, parseArray(data))   // + delete-detect
  event: created   → store.upsert(scope, parse(data)); enqueue(id)
  event: updated   → store.upsert(scope, parse(data)); enqueue(id)
  event: deleted   → store.remove(scope, idFrom(data));  enqueue(id)   // data may be {}
  event: error     → break → reconnect
  : ping           → refresh liveness deadline (no action)
on EOF / 1h close / read-deadline exceeded → reconnect with backoff
```

The store is updated before the `GenericEvent` is enqueued
([§6.1](#61-store-then-enqueue-ordering)).

### 5.4 Reconciler consumption

A participating controller changes in two places:

1. `SetupWithManager` gains
   `.WatchesRawSource(source.Channel(informer.Channel(), handler.EnqueueRequestsFromMapFunc(r.mapCPEventToRequests)))`.
2. `Reconcile` reads the `Lister` for control-plane state instead of calling
   `atlas-lib` over HTTP. The reconcile is otherwise unchanged — idempotent and
   level-triggered.

```go
func (r *VolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&v1alpha1.Volume{}).
        WatchesRawSource(
            source.Channel(
                r.CPVolumes.Channel(),
                handler.EnqueueRequestsFromMapFunc(r.mapCPEventToRequests),
            ),
        ).
        Complete(r)
}
```

---

## 6. Consistency and Correctness

The in-memory cache trades network reads for a coherence problem. Three rules
govern it.

### 6.1 Store-then-enqueue ordering

The stream goroutine updates the store before pushing the `GenericEvent`.
Enqueuing first allows a reconcile to run and read the store before the new value
is applied, seeing stale data — the classic informer race. client-go guarantees
store-then-handler ordering; the operator replicates it.

### 6.2 Read-your-writes after mutations

When a reconciler mutates the control plane, the store does not reflect the change
until the corresponding SSE `updated` arrives (routed DB → FDB watch → stream —
bounded, not synchronous). Two mechanisms cover this:

- The store is seeded from the mutation response. The v2 write endpoints return the
  new object; it is applied to the store immediately, as controllers use the
  returned object after a k8s `Update`.
- The `Lister` is not a linearizable read. A briefly-stale read causes at most one
  extra idempotent reconcile when the real event lands, not incorrectness, because
  reconcile is level-triggered.

### 6.3 Reconnect relist with delete-detection

On reconnect the `snapshot` is the full set for the scope, but a `deleted` that
occurred during the disconnect gap was never delivered. On every snapshot the
informer diffs against the store: any id present in the store for that scope but
absent from the snapshot has been deleted — the removal is synthesized and
enqueued. This is what client-go does on informer relist; without it, deletions
during a dropped connection leak in the cache.

### 6.4 No edge-triggered logic

Delivery is coalescing and there is no replay, so no handler observes every
transition or observes any event exactly once. Handlers make the CR's world match
current truth, idempotently. All side effects live in reconcilers, never in the
stream goroutine.

### 6.5 DTO completeness

The streamed object carries every field the reconciler reads. Where a resource's
list DTO is thinner than its detail DTO and a reconciler needs the richer fields,
the informer watches the detail stream for that resource. The scope is
per-resource: a reconciler that also reads an un-watched control-plane resource
fetches that part over HTTP.

---

## 7. Reliability and Connection Lifecycle

SSE is best-effort; the stream loop assumes the connection dies routinely and
recovers cleanly.

- **Reconnect with backoff** on every disconnect: EOF, the 1-hour forced close,
  `event: error`, a connect-time 503, or a liveness-deadline miss. The server's
  `retry: 3000` hint is the base; jittered exponential backoff caps it.
- **Liveness via ping.** A `: ping` comment arrives every ~15s. A read deadline
  above that interval (e.g. 45s) is maintained; a miss indicates a half-open
  socket and triggers reconnect. TCP alone is not relied upon — proxies and load
  balancers half-open silently.
- **The 1-hour forced close is routine**, handled as an ordinary reconnect. Each
  reconnect re-seeds from the snapshot ([§6.3](#63-reconnect-relist-with-delete-detection)).
- **The polling backstop remains.** The server's 30s FDB reconcile does not protect
  against the operator's own stream goroutine wedging. Each participating
  controller keeps its `RequeueAfter`, relaxed from 5–120s to a few minutes. SSE is
  the latency optimization; polling is the correctness floor.

Resulting latency: sub-second on the happy path, ~30s worst case for changes
written by non-index-maintaining writers, and the relaxed poll interval as the
absolute floor if the stream is down.

---

## 8. Leader Election

The Subscription Manager runs on the leader only. If every replica opens streams,
every replica's informer enqueues, multiplying reconciles.

- It registers as a `manager.LeaderElectionRunnable` with
  `NeedLeaderElection() → true` (in contrast to the cert provisioner, which returns
  `false` to run on every replica).
- Leader election must be enabled in any multi-replica deployment. The manager
  refuses to open streams when leader election is disabled and replicas > 1.
  (`--leader-elect` defaults to `false` in `operator/cmd/main.go`; the Helm chart
  enables it for multi-replica installs.)

Non-leader replicas hold no streams and no informer state. On failover the new
leader opens fresh streams and re-seeds from snapshots; no handoff is required.

---

## 9. ID → CR Mapping

The SSE payload identifies a control-plane resource (cluster/pool/volume/node id);
a `reconcile.Request` needs a CR namespace/name. The
`EnqueueRequestsFromMapFunc` handler bridges this:

- An index maps control-plane id → owning CR(s). For volumes it accounts for the
  `clusterID:poolID:volumeID` volume-handle convention
  (`lvol.VolumeHandle.Split()` in atlas-lib).
- An event whose id maps to no CR is dropped — the operator does not manage that
  object.
- One control-plane change may fan out to several CRs; the map function returns all
  affected `reconcile.Request`s.

---

## 10. CSI

The CSI driver (`csi-driver/`) is out of scope. It is gRPC, invoked synchronously
by kubelet, uses controller-runtime for nothing, and has no event loop into which
an SSE-derived event could be delivered. Its only coupling to the operator is
through the control plane and CRs. CSI remains request/response; CSI-adjacent
concerns (resize, volume state) flow through the operator's CRs or through kubelet
re-invoking CSI.

---

## 11. Implementation Phases

The change is adopted per-resource behind the polling backstop.

1. **Foundations.** The atlas-lib SSE client method (open a `?watch=true` stream,
   parse frames), the generic `Informer`/`Store`/`Lister`, and the
   `SubscriptionManager` runnable. No controller migrated.
2. **Volume (per-pool scope).** Volumes exercise both the per-scope connection
   fan-out and the cache. The volume informer wires into the volume reconciler via
   `source.Channel`, the reconcile reads the `Lister`, and its `RequeueAfter` is
   relaxed.
3. **Remaining resources.** Pools, storage-nodes, tasks, snapshots, and clusters,
   one at a time, each behind its backstop, verifying DTO completeness
   ([§6.5](#65-dto-completeness)) per resource.
4. **Leader-election hardening** ([§8](#8-leader-election)) precedes enabling SSE
   in any multi-replica deployment.

---

## 12. References

### Operator (consumer side)

- Manager / leader-election / runnable wiring — `operator/cmd/main.go`
- Canonical poll-and-requeue controller — `operator/internal/controller/controlplane_controller.go`
- Watch / map-func example — `operator/internal/controller/simplyblockstoragenodeset_controller.go`
- Existing HTTP client — `operator/internal/webapi/client.go`, `operator/internal/webapi/request.go`
- Typed v2 client (target) — `atlas-lib/controlplane/client.go`, `atlas-lib/internal/cpapi/cpapi.gen.go`
- controller-runtime v0.24.1 — `source.Channel` (function form) + `WatchesRawSource`

### Control plane (`sbcli`, branch `sse`)

- SSE framing / `WatchParam` / ping / lifetime / retry — `simplyblock_web/api/v2/_sse.py`
- Watch mechanism (FDB scope watch, version-index diff, 30s reconcile) — `simplyblock_core/watch.py`, `simplyblock_core/watches.py`
- Write-time index bump — `simplyblock_core/models/base_model.py`
- Per-model `watch_scope()` — `lvol_model.py`, `pool.py`, `snapshot.py`, `storage_node.py`, `job_schedule.py`, `cluster.py`
- Auth — `simplyblock_web/api/v2/_auth.py`
- Behavior specification (tests) — `tests/integration/web/test_watch_sse_e2e.py`, `tests/unit/web/api/v2/test_watch_stream.py`, `tests/unit/test_watch_core.py`