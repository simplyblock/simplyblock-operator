# Design Document: Synchronous Replication through csi-addons

**Status:** Draft  
**Author:** Geoffrey (geoffrey1330)  
**Date:** 2026-10-06  
**Test Plan:** [`tests/test-plan-sync-replication-csi-addons.md`](../tests/test-plan-sync-replication-csi-addons.md)

---

## Phasing Overview

| Phase                        | Status  | csi-addons surface                   | Backend routes                                                             |
|------------------------------|---------|--------------------------------------|----------------------------------------------------------------------------|
| **Phase 1** (§5, §6, §7, §8) | Planned | `VolumeReplication` (per volume)     | `V/replication/{failover,demote,sync-status,status,failback}`, `V/connect` |
| **Phase 2** (§13)            | Planned | `VolumeGroupReplication` (per group) | `G/replication/{failover,demote,sync-status,status,failback}`              |

Phase 1 makes a single replicated volume driveable by RamenDR against a two-site synchronous cluster: promote, demote, status, and the no-op verbs. Phase 2 adds the consistency-group variants, which the backend already serves as one unit (one gate, one promote task for all members). Phase 2 reuses Phase 1's method dispatch and site resolution unchanged, so it ships independently once the group routes are wired in the driver.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                   | Kind                    | Blocks              | Status                                                                                                |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------------------|-------------------------------------------------------------------------------------------------------|
| P0-1 | The v2 sync-replication REST routes (`V/replication/{failover,demote,sync-status,status,failback}`, `G/...`, `V/connect?site=`) and the `site` query parameter | Control plane (`sbcli`) | Phase 1 and Phase 2 | On the `sync-replication` branch, not yet released                                                    |
| P0-2 | The `TasksRunnerSyncPromote` (`FN_SYNC_PROMOTE`) and `TasksRunnerSyncResync` (`FN_SYNC_RESYNC`) background services                                            | Control plane (`sbcli`) | Promote and resync  | On the branch; the operator must add both to its runner table (§8.4)                                  |
| P0-3 | A cluster created with `sync_replication: true` and every node assigned a `site`                                                                               | Control plane (`sbcli`) | Everything          | Deploy-time only, no conversion of a live cluster; the operator drives both from the CRs (§8.5, §8.6) |
| P0-4 | The upstream `VolumeReplication` / `VolumeReplicationClass` CRDs and the kubernetes-csi-addons manager and sidecar                                             | Ecosystem               | Phase 1             | Vendored by the async design (`design-csi-addons-replication.md` P0-5)                                |
| P0-5 | The upstream `VolumeGroupReplication` / `VolumeGroupReplicationClass` CRDs                                                                                     | Ecosystem               | Phase 2             | Vendored by the async design                                                                          |

Without P0-1 and P0-2 the driver has no sync endpoints to call, so every sync-method `VolumeReplication` fails its reconcile. Without P0-3 the backend answers the sync routes as async (the behavior is chosen by the cluster's `sync_replication` flag), so a class carrying `method: sync` against a non-sync cluster is a configuration error the driver reports (§10). P0-4 and P0-5 are shared with the async design and already in the chart.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [The csi-addons Machinery](#4-the-csi-addons-machinery)
5. [Method Dispatch](#5-method-dispatch)
6. [Sync Replication Verbs](#6-sync-replication-verbs)
7. [Steady-State Status and Conditions](#7-steady-state-status-and-conditions)
8. [Classes, Site Resolution, and Operator Provisioning](#8-classes-site-resolution-and-operator-provisioning)
9. [Coexistence with Async Replication](#9-coexistence-with-async-replication)
10. [Backend API Requirements](#10-backend-api-requirements)
11. [Failure Modes and Fallback](#11-failure-modes-and-fallback)
12. [Observability](#12-observability)
13. [VolumeGroupReplication](#13-volumegroupreplication)
14. [Testing Strategy](#14-testing-strategy)
15. [Open Questions](#15-open-questions)

---

## Overview

A simplyblock cluster can be deployed to span exactly two sites, with every logical volume store replicated synchronously to the other site (the contract is `sbcli/docs/sync-replication.md`). A single Kubernetes cluster spans the two zones that map to those sites, and RamenDR drives relocate and fail-over within that one cluster. RamenDR already drives the async (snapshot-based) replication of this product through csi-addons, via the simplyblock CSI driver's replication controller (`design-csi-addons-replication.md`). This design makes the same csi-addons surface drive the synchronous backend instead, selected per `VolumeReplicationClass` or `VolumeGroupReplicationClass`.

The core idea is one dispatch point. Each csi-addons replication RPC already reads its `VolumeReplicationClass` parameters from `req.GetParameters()` (the async path reads `replicationPolicyID` and `sourceClusterID` that way). This design adds two parameters, `replication.storage.simplyblock.io/method` and `replication.storage.simplyblock.io/site`, read at the same point. When `method` is `sync`, the RPC routes to the backend's sync endpoints with the class's `site`. When `method` is absent or `async`, the RPC takes the existing async path unchanged, so every class in the field keeps working.

The mapping from csi-addons to the sync backend is fixed by the backend contract (`sync-replication.md` §4) and is intentionally small. `DemoteVolume` and `PromoteVolume` carry the switchover. `PromoteVolume.force` becomes `planned = !force`, so an unforced promote is a planned switchover and a forced promote is a disaster fail-over. The status verbs read one cluster-wide sync status. `EnableVolumeReplication`, `DisableVolumeReplication`, and `ResyncVolume` are no-ops, because a sync volume keeps its identity on both sites, has no replication relationship to configure, and catches up automatically after a desync.

A relocate's PVC handling is RamenDR's own and identical to the async flow. On demote the application is detached and the PVC is deleted, and on promote RamenDR recreates it bound back to the same underlying simplyblock volume. That volume is retained throughout and never deleted, so the driver's part is only the backend store-leadership switchover, not the volume's lifecycle.

The orchestration contract RamenDR depends on is the status-code protocol. A `409` is always retryable and is never a reason to force. A `412` is the only answer on which csi-addons escalates an unforced promote to a forced one, and the backend never returns `412` for a gate refusal, so a desynced cluster cannot drive RamenDR into an escalation loop.

---

## 1. Background

The simplyblock CSI driver implements the csi-addons replication service in `csi-driver/internal/csi/controller/replication.go`: `EnableVolumeReplication`, `DisableVolumeReplication`, `PromoteVolume`, `DemoteVolume`, `ResyncVolume`, and `GetVolumeReplicationInfo`, plus the group variants. Today, every one of these targets the async snapshot-replication backend. `EnableVolumeReplication` attaches a `ReplicationPolicy` named by the `replicationPolicyID` class parameter. `PromoteVolume` resolves the replication chain and promotes its active end. `ResyncVolume` reconfigures the reverse pipe. The status verbs read the per-volume async replication status and map it onto the csi-addons conditions (`design-csi-addons-replication.md` §5, §6).

A synchronous two-site cluster has none of these shapes. There is no replication chain and no per-volume relationship, because a volume keeps one identity and is served from whichever site its logical volume store is led from. There is no policy to attach, no cadence, and no reverse-pipe reconfiguration, because both sites are always in sync or catching up. The switchover is a leadership move of the store plus a per-volume open on the target site, gated on the replicas being in sync (or on the other site being provably lost, for a disaster fail-over). The backend already exposes this as a distinct set of v2 routes keyed on the caller's `site` (`sync-replication.md` §3), and it decides sync against async from the cluster's own `sync_replication` flag.

What is missing is the driver layer that routes a csi-addons call to those routes. A `VolumeReplication` whose class points at a sync cluster today would take the async path, resolve a chain that does not exist, and fail. The driver needs to know, per call, that this relationship is synchronous and which site the switchover targets.

---

## 2. Goals and Non-Goals

### Goals

- Drive a synchronous two-site cluster's switchover through the existing csi-addons `VolumeReplication` surface, so RamenDR orchestrates it with no Ramen-side knowledge of simplyblock internals (Phase 1).
- Select the replication method per class, from a `VolumeReplicationClass` or `VolumeGroupReplicationClass` parameter, leaving every async class unchanged (§5).
- Map `PromoteVolume`, `DemoteVolume`, and the status verbs onto the backend sync routes exactly as the backend contract specifies, including `force` to `planned` inversion and the `409` / `412` protocol (§6, §11).
- Make `EnableVolumeReplication`, `DisableVolumeReplication`, and `ResyncVolume` correct no-ops on a sync class (§6).
- Surface the sync status as the csi-addons conditions RamenDR gates on (`Completed`, `Degraded`, `Resyncing`) and as a fresh `lastSyncTime` while in sync (§7).
- Add the group variants, driving a consistency group as one unit (Phase 2, §13).
- Expose the deploy-time sync knobs on the operator CRs: a `StorageCluster` toggle to create a sync cluster, and a per-node `site` at storage-node add (§8.5, §8.6).
- Report a misconfiguration (a sync class against a non-sync cluster, a missing site) as a clear, non-retryable error rather than a silent wrong path (§10).

### Non-Goals

- **The synchronous backend itself.** The two-site model, the gates, the promote and demote mechanics, the disaster fail-over, and the automatic resync are the control plane's, specified in `sync-replication.md` and summarized here only where the driver depends on them.
- **Live fail-over or fail-back.** Applications are stopped while their volumes switch sites, a limit of the backend's scope that the driver inherits.
- **RamenDR DRPolicy and class selection.** How Ramen pairs the two clusters' classes and which `schedulingInterval` marks a synchronous relationship is Ramen's hub-side mechanism, out of scope here as it is for the async design (`design-csi-addons-replication.md` §7.2). This design's scope starts at the class the driver is handed. The Ramen-side mapping is tracked as an Open Question (§15).
- **Migrating an async relationship to sync or the reverse.** A cluster is sync or async at deploy time (P0-3). A volume does not change method in place.
- **The PVC lifecycle during a switchover.** Deleting the PVC on demote and recreating it on promote, bound back to the retained simplyblock volume, is RamenDR's work and identical to the async flow. The underlying volume is never deleted, and the driver only moves the backend's store leadership.
- **Exposing sites or `lost_site` through the CSI API.** The driver takes the target site from the class of the switchover it is handed (§8). The cluster's site topology stays a control-plane concern.

---

## 3. Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────┐
│              Kubernetes (one cluster spanning both zones)              │
│                                                                        │
│   RamenDR ── VolumeReplication / VolumeGroupReplication                │
│      │          (spec.replicationState: primary | secondary)          │
│      ▼                                                                 │
│   kubernetes-csi-addons manager ── sidecar ── CSI replication RPCs     │
│      │                                                                 │
│   ┌──▼───────────────────────────────────────────────────────────┐   │
│   │   Driver replication controller (replication.go)              │   │
│   │   1. read method + site from req.GetParameters()  (§5)        │   │
│   │   2. method=sync  ─► sync REST (with ?site=)       (§6)        │   │
│   │      method=async ─► existing chain-based path (async design)  │   │
│   └───────────────────────────────────────────────────────────────┘   │
│   VolumeReplicationClass parameters:                                   │
│     replication.storage.simplyblock.io/method: sync                    │
│     replication.storage.simplyblock.io/site:   <target site>          │
└──────────────────────────────────────────────────────────────────────┘
              │ HTTP (webapi client, service-account bearer token)
┌─────────────▼────────────────────────────────────────────────────────┐
│                     Simplyblock Backend API (two-site cluster)         │
│  POST .../volumes/{v}/replication/failover?site=S&planned=            │
│  POST .../volumes/{v}/replication/demote?site=S                       │
│  GET  .../volumes/{v}/replication/sync-status?site=S                  │
│  GET  .../volumes/{v}/connect?site=S                                  │
└──────────────────────────────────────────────────────────────────────┘
```

**One control plane, two sites, one Kubernetes cluster.** The two sites of a synchronous cluster share one control plane and one FoundationDB (`sync-replication.md` §1). A single Kubernetes cluster spans both zones, and one driver deployment serves volumes in either zone. Site is therefore not a property of the cluster or the deployment. It is the target of a given switchover, carried by the `VolumeReplicationClass` of the `VolumeReplication` being promoted or demoted (§8). A relocate to site B promotes against the class naming `site-b`, so the driver calls the backend with `site=B`.

**The dispatch is per RPC, not per driver.** Nothing about the driver deployment is sync-specific. The same binary serves async and sync classes, deciding per call from the class parameters. This is what lets a fleet run both.

**The backend is the authority on sync against async for reads.** The status routes behave by the cluster's `sync_replication` flag, so `GET .../replication/status` returns an async-shaped DTO filled from the sync status on a sync cluster (§7). The driver still needs `method` to pick the correct *write* path (promote and demote differ completely between async and sync) and to know a `site` is required.

---

## 4. The csi-addons Machinery

Nothing new is deployed. The CRDs (`volumereplications` and `volumereplicationclasses` in `replication.storage.openshift.io/v1alpha1`, and the group variants), the kubernetes-csi-addons manager, and the sidecar are the ones the async design vendors in the chart (`design-csi-addons-replication.md` §4). This design adds no simplyblock CRD and no new controller. It extends the dispatch inside the existing replication RPC handlers and adds a sync client for the backend routes.

The plugin serves the same `ReplicationServer` capabilities as today. A sync relationship uses the identical RPC sequence RamenDR already issues for an async one (`EnableVolumeReplication` then `PromoteVolume` on the side becoming primary, `DemoteVolume` on the side becoming secondary, `GetVolumeReplicationInfo` on both). What differs is only where each RPC routes, decided in §5.

---

## 5. Method Dispatch

### 5.1 The two class parameters

A `VolumeReplicationClass` (and, in Phase 2, a `VolumeGroupReplicationClass`) carries two new parameters, read by the driver from `req.GetParameters()`:

| Parameter                                   | Values                     | Meaning                                                                                                                               |
|---------------------------------------------|----------------------------|---------------------------------------------------------------------------------------------------------------------------------------|
| `replication.storage.simplyblock.io/method` | `sync`, `async`            | The backend to drive. Absent is treated as `async`, so every existing class keeps its behavior.                                       |
| `replication.storage.simplyblock.io/site`   | a site name of the cluster | The target site of the switchover, passed as `?site=` on every sync write route. Required when `method` is `sync`, ignored otherwise. |

The site name follows the backend's rule (`[A-Za-z0-9][A-Za-z0-9._-]{0,62}`). The driver does not validate it against the cluster topology, because the backend rejects an unknown site with a `400` that the driver surfaces (§10).

### 5.2 Where the dispatch happens

Each replication RPC reads `method` first and branches:

```go
// method selects the replication backend this class drives. Absent or "async"
// keeps the existing snapshot-replication path (design-csi-addons-replication.md);
// "sync" drives the two-site synchronous backend (sync-replication.md).
const (
	methodParam = "replication.storage.simplyblock.io/method"
	siteParam   = "replication.storage.simplyblock.io/site"

	methodAsync = "async"
	methodSync  = "sync"
)
```

A helper resolves the method and, for the sync path, the site, returning the error the RPC surfaces when a sync class omits the site. The async branch is the current code, untouched. The sync branch calls the sync client (§6) with the resolved site.

### 5.3 The RPCs without class parameters

`GetVolumeReplicationInfo` does not carry `parameters` in the csi-addons replication service (only the volume handle and secrets), so it cannot read `method` or `site` the way the other verbs do. This is the one dispatch gap, resolved by the backend rather than the class: the driver calls `GET .../replication/status` for the info read regardless of method, and the backend fills it from the sync status on a sync cluster (§7). The `site` that route needs is the subject of Open Question 1 (§15). Every write verb (`Enable`, `Disable`, `Promote`, `Demote`, `Resync`) does carry `parameters`, so the dispatch for the switchover itself is unambiguous.

---

## 6. Sync Replication Verbs

The mapping is fixed by the backend contract (`sync-replication.md` §4). Prefix `V = .../clusters/{c}/storage-pools/{p}/volumes/{v}`.

| csi-addons RPC             | Sync route                                                 | Behavior                                                                                       |
|----------------------------|------------------------------------------------------------|------------------------------------------------------------------------------------------------|
| `EnableVolumeReplication`  | none (or `PUT V/` with `replication_policy_id`, `204`)     | No-op. A sync volume has no policy to attach. Returns success.                                 |
| `DisableVolumeReplication` | none                                                       | No-op. Returns success.                                                                        |
| `ResyncVolume`             | none (or `POST V/replication/failback`, body `{}`, `204`)  | No-op. The catch-up is automatic. Returns success.                                             |
| `DemoteVolume`             | `POST V/replication/demote?site=<site>`                    | Fence the volume on this site. `204` (also when not served here), `409` on a gate refusal.     |
| `PromoteVolume(force)`     | `POST V/replication/failover?site=<site>&planned=<!force>` | Serve the volume here. Repeat while `409` "in progress"; `200` carries the connection entries. |
| `GetVolumeReplicationInfo` | `GET V/replication/status?site=<site>`                     | Read the cluster-wide status (§7).                                                             |

### 6.1 Promote

`PromoteVolume.force` inverts to `planned`: an unforced promote is `planned=true` (a planned switchover that requires both sites in sync), a forced promote is `planned=false` (a disaster fail-over of a lost site). The backend runs the promote as a background task and the route is a poll: the first call queues the task and answers `409` "in progress" with a `task_id`, and the driver keeps calling until the answer is `200` with the connection entries, or a terminal error. The csi-addons `PromoteVolume` is itself idempotent and retried by the sidecar, so the driver returns the `409` "in progress" as a retryable error (`codes.Unavailable` or `codes.Aborted`) and the next reconcile re-issues the poll.

A `200` carries a `SyncPromoteResultDTO` (`{lvol_id, connection_strings}`), the connection entries of this site's triplet. The driver does not act on them here, because node staging reads them separately through `GET V/connect?site=` at publish time. The `200` is the statement that the volume was opened on this site (§7, role is not accessibility).

### 6.2 Demote

`DemoteVolume` is a single `POST V/replication/demote?site=`. It is a no-op (`204`) when the volume is not served on this site, so a retried demote is safe. A `409` is a planned-gate refusal (a desynced store blocks it) and is retryable. A `500` means an ANA RPC failed with nothing recorded, and is also retryable.

A planned switchover demotes every volume of the stores involved on the source site before the promote on the target can pass its gate. RamenDR issues `DemoteVolume` per `VolumeReplication`, which covers the application's own volumes. A store shared with volumes outside the DR set is the operator's concern, noted in §11.

### 6.3 The no-op verbs

`EnableVolumeReplication`, `DisableVolumeReplication`, and `ResyncVolume` return success without a backend call on a sync class. This matches the async design's tolerance of an `Enable` with no policy on the fail-over target (`design-csi-addons-replication.md` §5.1): the side becoming primary carries no reverse-direction configuration, and on sync there is never any to carry. Returning success keeps RamenDR's `Enable` then `Promote` sequence intact.

---

## 7. Steady-State Status and Conditions

RamenDR gates relocate and fail-over on `GetVolumeReplicationInfo` and on the csi-addons conditions. The backend serves one cluster-wide sync status (`sync-replication.md` §3, the `SyncReplicationStatusDTO`), and `GET V/replication/status` returns it shaped as the existing async `ReplicationStatusDTO` on a sync cluster. `GetVolumeReplicationInfo` reads that route and fills the three fields the RPC carries:

| csi-addons field   | Sync source                        | Healthy value |
|--------------------|------------------------------------|---------------|
| `lastSyncTime`     | the last confirmed in-sync instant | now           |
| `lastSyncDuration` | `lag_seconds`                      | ~0            |
| `lastSyncBytes`    | `bytes_behind`                     | 0             |

`lastSyncTime` is the load-bearing field, and the reason is that synchronous replication has no discrete sync cycle. Every write is acknowledged only once it is journaled on both sites, so a volume the backend confirms in sync is current as of the read. The driver reports `lastSyncTime` as the read time whenever the backend reports in sync, which is what lets RamenDR proceed and populates its `lastGroupSyncTime`. When the backend reports a zone behind or a catch-up running, the driver reports the real last-in-sync instant instead, which is older, so RamenDR holds the relocate rather than switching onto a replica that is behind.

The csi-addons conditions follow the same read. A healthy status is `Completed`, with `Degraded` and `Resyncing` both false, so RamenDR reads the relationship as ready. A running catch-up is `Resyncing`, and a zone that is behind, unknown, or unanswered is `Degraded`. `Completed` is also the result of a successful promote or demote, so the condition surface and the status read agree.

`role` is the site a volume is assigned to, not whether it is currently published. A volume demoted a moment ago still reports `role: primary` on its site while every path is inaccessible (`sync-replication.md`, "Role is not accessibility"). The driver therefore never treats `role` as proof that a volume is served. The `200` from a promote is that proof, and it is the result of the operation, not a continuous probe.

The status is cluster-wide: every volume and group gets the same `state`, `lag`, and `bytes_behind`, and only `role` is per volume. The driver does not sum or combine per-member status for a group (§13).

---

## 8. Classes, Site Resolution, and Operator Provisioning

### 8.1 The class

A sync `VolumeReplicationClass` names the simplyblock provisioner and carries the two parameters of §5.1. One Kubernetes cluster spans both zones, so both sites' classes live in that one cluster, differing only in their `site` value:

```yaml
apiVersion: replication.storage.openshift.io/v1alpha1
kind: VolumeReplicationClass
metadata:
  name: simplyblock-sync-a
  labels:
    ramendr.openshift.io/replicationid: <relationship id>
spec:
  provisioner: csi.simplyblock.io
  parameters:
    replication.storage.simplyblock.io/method: sync
    replication.storage.simplyblock.io/site: site-a
---
apiVersion: replication.storage.openshift.io/v1alpha1
kind: VolumeReplicationClass
metadata:
  name: simplyblock-sync-b
  labels:
    ramendr.openshift.io/replicationid: <relationship id>
spec:
  provisioner: csi.simplyblock.io
  parameters:
    replication.storage.simplyblock.io/method: sync
    replication.storage.simplyblock.io/site: site-b
```

There is no `replicationPolicyID` and no `sourceClusterID`, because a sync relationship has no policy and no chain. A relocate to a site promotes the `VolumeReplication` against that site's class.

### 8.2 Site resolution

The `site` is the target of the switchover, not a fixed property of the cluster or the driver. One Kubernetes cluster spans both zones and one driver deployment serves both, so the driver reads `site` from the `VolumeReplicationClass` of the `VolumeReplication` it is promoting or demoting. A relocate to site B promotes against the `site-b` class, and the driver calls the backend with `?site=B`. A relationship is fully described by its two classes, and no driver reconfiguration is needed to stand one up. The one RPC that cannot read a class, `GetVolumeReplicationInfo` (§5.3), has no `site` to read, which is Open Question 1 (§15).

### 8.3 Ramen class selection

Ramen selects a `VolumeReplicationClass` by `provisioner`, `schedulingInterval`, and `replicationClassSelector` labels, never by name (`design-csi-addons-replication.md` §7.2). A synchronous relationship is a Metro-style `DRPolicy`, and how Ramen marks one (conventionally a `schedulingInterval` of zero), how it drives a relocate within a single stretched cluster, and how it selects the target site's class are Ramen's mechanism. This design requires only that the class the driver is handed for a switchover carries `method: sync` and that switchover's target `site`. The Ramen-side mapping is Open Question 2 (§15).

### 8.4 Operator work: the sync task runners

The backend's promote and resync run as two management-plane services, `TasksRunnerSyncPromote` (`FN_SYNC_PROMOTE`) and `TasksRunnerSyncResync` (`FN_SYNC_RESYNC`) (`sync-replication.md` §2). On Kubernetes the operator owns the control-plane runner table in `operator/internal/controllers/controlplane/managementapi.go`, and must add both, or a promote answers "in progress" forever and no resync ever starts. This is one of the operator-side changes this design requires, alongside the provisioning knobs in §8.5 and §8.6.

### 8.5 Operator work: enabling sync at cluster creation

A sync cluster is created with `sync_replication: true` in the cluster-create body (`sync-replication.md` §2), and this is deploy-time only: a live cluster cannot be converted (P0-3). In the deployment model a cluster is declared by a `ClusterDeploymentConfig`, whose `spec.cluster` template expands into the `StorageCluster` (`operator/api/v1alpha2`). The toggle is added in both, mirroring `enableFailureDomains` and `enableChecksumValidation`, the existing precedents for an immutable cluster-wide mode carried on the document and spent on the cluster.

On the `ClusterDeploymentConfig` cluster template (`ClusterTemplate`):

```go
// EnableSyncReplication opts the cluster into synchronous two-site replication.
// The cluster then spans exactly two sites, and every node group declares a site
// (§8.6). It is on the document because it is immutable on the cluster it lands
// on: the backend accepts sync_replication only at cluster-create, so a cluster
// created without it is one nobody can turn it on for.
// +optional
EnableSyncReplication *bool `json:"enableSyncReplication,omitempty"`
```

And on the `StorageCluster` the template expands into, beside `enableFailureDomains`:

```go
// EnableSyncReplication opts the cluster into synchronous two-site replication,
// where every node declares a site and each logical volume store is replicated
// to the other site. Immutable: set only at cluster-create.
// +optional
// +k8s:immutable
EnableSyncReplication *bool `json:"enableSyncReplication,omitempty"`
```

The `StorageCluster` reconciler sets `sync_replication` in its cluster-create body from this field, as it already sets `enable_failure_domain` from `enableFailureDomains` (the control-plane client body, `cpapi`). Sync replication requires an HA cluster and is refused on a single-node one (`sync-replication.md` §2), so the field is valid only with the cluster's HA configuration, and the backend enforces the two-site and per-site capacity rules at the first activation.

### 8.6 Operator work: the node site at storage-node add

Every node of a sync cluster is added with a `site` in the storage-node-create body, mandatory on a sync cluster and rejected with `400` on any other (`sync-replication.md` §2). A node is declared by a `NodeGroup` in the `ClusterDeploymentConfig`, which expands into `StorageNode.spec.config`. Site sits in the same two places as `failureDomain` and shares its discovery seeding, which is the natural fit: a sync cluster's two sites are its two zones, and discovery already seeds the fault group from `topology.kubernetes.io/zone`.

On the `ClusterDeploymentConfig` node group (`NodeGroup`), beside `failureDomain`:

```go
// Site is the replication site every worker in this group belongs to, one of the
// cluster's two sites. Discovery seeds it from topology.kubernetes.io/zone, as it
// does failureDomain, since a sync cluster's two sites are its two zones. Required
// on a sync cluster (§8.5), rejected otherwise. It expands into
// StorageNode.spec.config.site, whose shape it shares.
// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
// +optional
Site string `json:"site,omitempty"`
```

And on `StorageNode.spec.config`, beside `failureDomain`:

```go
// Site is the replication site this node belongs to, copied from the
// ClusterDeploymentConfig node group that produced the node. Immutable: a host
// sits entirely on one site for the cluster's life.
// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
// +k8s:immutable
// +optional
Site string `json:"site,omitempty"`
```

The `StorageNode` reconciler sets `site` in its node-add body from this field, as it already sets `failure_domain`. The operator requires a site on every node of a sync cluster, mirroring the failure-domain check and enforced by the webhook that guards `spec.config`, and the backend is the final authority: a node added without a site to a sync cluster is refused with a `400` the operator surfaces, rather than placed on an undefined site. Exactly two sites must be populated, checked by the backend at the first activation.

Both additions are CRD changes on `ClusterDeploymentConfig`, `StorageCluster`, and `StorageNode` (`operator/api/v1alpha2`), so they go through `make -C operator manifests generate` and `make helm-sync`, and the api-design gate audits the markers (the immutable toggle, the `enableXyz` name, the site pattern) against the shipped types.

---

## 9. Coexistence with Async Replication

The method dispatch is additive. A class with no `method` parameter, or `method: async`, is the async path with every line of today's behavior. A class with `method: sync` is the sync path. Both can exist in one fleet and even on one driver binary, because the choice is per RPC from the class the volume's `VolumeReplication` names.

The async replication kinds the legacy backend exposes are unaffected, as are the `ReplicationPolicy`-based paths. On a sync cluster the backend still accepts a volume created with an async replication policy (`sync-replication.md` §8), so a class pointing a sync cluster at the async routes is a misconfiguration this driver does not need to prevent: the direct async replication operations are refused by the backend with a `400` that the driver surfaces (§10).

---

## 10. Backend API Requirements

All routes are v2, token-authenticated, served on a cluster with `sync_replication: true`. Prefix `V` as in §6. All exist on the `sync-replication` branch (P0-1).

| Method | Endpoint                                 | Notes                                                                                                                                                                                                                               |
|--------|------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| POST   | `V/replication/failover?site=S&planned=` | Promote. `200` `SyncPromoteResultDTO`; `409` in progress or refused (retry); `412` the other site is not online and the call is not forced; `400` bad or missing site. Idempotent: a `200` for an already-served volume is a no-op. |
| POST   | `V/replication/demote?site=S`            | Demote. `204` (also a no-op when not served here); `409` gate refusal; `500` ANA RPC failed. Idempotent.                                                                                                                            |
| GET    | `V/replication/status?site=S`            | The async-shaped status DTO filled from the sync status. `200`; `400` bad or missing site.                                                                                                                                          |
| GET    | `V/replication/sync-status?site=S`       | The native `SyncReplicationStatusDTO`. `200`; `400` on a non-sync cluster.                                                                                                                                                          |
| GET    | `V/connect?site=S[&host_nqn=]`           | The connection entries of S's triplet, for node staging. `200`; `400`; `404` when entries cannot be built.                                                                                                                          |
| POST   | `V/replication/failback` (body `{}`)     | Resync no-op. `204`. Body required.                                                                                                                                                                                                 |

The group routes (prefix `G`) mirror these and are listed in §13.

**Error envelope.** Sync refusals use FastAPI's `detail` object, `{"detail": {"message": "...", ...}}`, with `problems` (gate refusals), `volumes` (volumes blocking a promote, or group members not found), or `task_id` (the promote task) where each applies (`sync-replication.md` §3). The driver classifies on the status code first (§11) and carries the `message` into the gRPC error.

**Idempotency.** Promote and demote are safe to retry: a promote re-polls its task and a `200` for a served volume is a no-op, a demote of a not-served volume is a `204`. This is required, because the csi-addons sidecar retries every RPC.

---

## 11. Failure Modes and Fallback

| Failure                                     | Detection                                                                   | Behavior                                                                                                                                                       |
|---------------------------------------------|-----------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Promote in progress                         | `409` with `task_id`, message "in progress"                                 | Retryable. Return `codes.Unavailable`; the next reconcile re-polls.                                                                                            |
| Planned gate refused (desynced store)       | `409` with `problems`                                                       | Retryable, never forced. Return `codes.FailedPrecondition` with the problems. RamenDR retries; it does not escalate.                                           |
| Other site not online, promote not forced   | `412`                                                                       | The single escalation trigger. Return `codes.FailedPrecondition`; csi-addons re-issues `PromoteVolume` with `force`, which the driver maps to `planned=false`. |
| Forced promote while the other site is live | `409`                                                                       | Retryable. Force never acts on a live site; the driver returns `codes.FailedPrecondition` and waits.                                                           |
| Sync class, missing `site`                  | driver, before any call                                                     | `codes.InvalidArgument`. A misconfigured class, not retryable usefully.                                                                                        |
| Sync class against a non-sync cluster       | `400` from the backend (`sync-status`), or an async route behaving as async | `codes.FailedPrecondition` with the backend message. The deployment paired a sync class with an async cluster.                                                 |
| ANA RPC failed on demote                    | `500`                                                                       | Retryable. Return `codes.Unavailable`.                                                                                                                         |
| Backend unreachable                         | transport error                                                             | Retryable. Return `codes.Unavailable`.                                                                                                                         |

The protocol RamenDR relies on is that `409` is always retryable and never a reason to force, and `412` is the only answer that escalates to a forced promote. The driver preserves it by never mapping a gate refusal to `codes.FailedPrecondition` in a way csi-addons reads as "escalate": the escalation path is keyed on the backend's `412` alone, surfaced as the status that triggers the forced retry, and a gate refusal is a plain retry.

A store shared between the DR set and volumes outside it cannot be switched until every volume on it is demoted. RamenDR demotes only the volumes it manages, so such a store blocks the promote with a `409` naming the still-served volumes (`volumes`). This degrades to a clear, retryable refusal rather than a silent partial switchover, and the resolution is operational (do not share a store across DR boundaries).

---

## 12. Observability

The CSI driver's replication controller emits no Kubernetes events today and exposes the async replication metrics the async design defines (`design-csi-addons-replication.md` §11). This section adds the sync-specific metrics and the events the dispatch and the promote poll owe. Events land on the `VolumeReplication` (and, for Phase 2, the `VolumeGroupReplication`) object, because it is the object RamenDR creates and a user inspects, and it outlives the promote it reports on.

### Kubernetes Events

| Event                                                                   | Type    | Reason                   | On                                        |
|-------------------------------------------------------------------------|---------|--------------------------|-------------------------------------------|
| The promote is in progress and will be polled again                     | Normal  | `SyncPromotePending`     | VolumeReplication, VolumeGroupReplication |
| The volume is served on this site                                       | Normal  | `SyncPromoteSucceeded`   | VolumeReplication, VolumeGroupReplication |
| The planned gate refused the switchover because a store is not in sync  | Warning | `SyncGateRefused`        | VolumeReplication, VolumeGroupReplication |
| The other site is not online; an unforced promote cannot proceed        | Warning | `SyncPeerSiteOffline`    | VolumeReplication, VolumeGroupReplication |
| A sync class is missing its site parameter, or names a non-sync cluster | Warning | `SyncClassMisconfigured` | VolumeReplication, VolumeGroupReplication |

`SyncGateRefused` and `SyncPeerSiteOffline` are the load-bearing ones: a switchover correctly holding for a desynced store and one holding for a lost peer are indistinguishable without them, and both look like a stalled relocate to an operator watching only the `DRPlacementControl`.

### Prometheus Metrics

| Metric                                      | Labels                      | Description                                                                                     |
|---------------------------------------------|-----------------------------|-------------------------------------------------------------------------------------------------|
| `simplyblock_csi_sync_promote_total`        | `cluster`, `site`, `result` | Promote outcomes by result (`succeeded`, `gate_refused`, `peer_offline`, `escalated`, `error`). |
| `simplyblock_csi_sync_promote_poll_seconds` | `cluster`, `site`           | Histogram of wall-clock time from the first promote call to the `200`.                          |
| `simplyblock_csi_sync_demote_total`         | `cluster`, `site`, `result` | Demote outcomes (`fenced`, `gate_refused`, `error`).                                            |
| `simplyblock_csi_sync_status_degraded`      | `cluster`, `site`           | Gauge, one while the cluster-wide sync status is not healthy.                                   |

`simplyblock_csi_sync_promote_poll_seconds` is the switchover-latency alert, the figure an operator compares against the application's downtime budget during a planned switchover. `simplyblock_csi_sync_status_degraded` held at one is how a desync that blocks every switchover gets noticed before the next DR drill rather than during it.

---

## 13. VolumeGroupReplication

Phase 2 adds the consistency-group variants. The backend drives a group as one unit: one gate for every member, the store rule checked over the union of the members' stores, one promote task for all (`sync-replication.md` §3, group routes). The driver's group RPCs (`EnableVolumeGroupReplication`, `PromoteVolumeGroup`, `DemoteVolumeGroup`, and the group info read) dispatch by the same `method` and `site` read from the `VolumeGroupReplicationClass`, exactly as Phase 1 does per volume.

| Method | Endpoint                                 | Notes                                                                                                                                                         |
|--------|------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------|
| POST   | `G/replication/failover?site=S&planned=` | `200` `{"members": [SyncPromoteResultDTO, ...]}` once every member is served; otherwise `409` / `412` / `400` as the volume route.                            |
| POST   | `G/replication/demote?site=S`            | `204` (an empty group too); `409` gate refusal. Members are fenced in order, each recorded after its own fence, so a retry is a no-op for those already done. |
| GET    | `G/replication/status?site=S`            | `ConsistencyGroupReplicationStatusDTO` filled from the sync status plus `member_count`.                                                                       |
| GET    | `G/replication/sync-status?site=S`       | `SyncReplicationStatusDTO`; `400` on a non-sync cluster.                                                                                                      |
| POST   | `G/replication/failback` (body `{}`)     | `204` no-op.                                                                                                                                                  |

A group member that cannot be resolved to a live volume, or that belongs to another cluster, refuses the whole group request with `409` and the ids in `volumes`, before anything is fenced or queued. The driver surfaces that as a retryable `codes.FailedPrecondition` naming the members. The status is cluster-wide, so the group gets the same `state` and `lag` as any volume, plus `member_count`. The driver does not combine per-member status (§7).

Phase 2 is independent of Phase 1 only in the driver wiring. The dispatch, the site resolution, the status mapping, the `force` inversion, and the `409` / `412` protocol are all shared, so Phase 2 is the group RPC handlers calling the `G` routes with the Phase 1 machinery.

---

## 14. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts: [`tests/test-plan-sync-replication-csi-addons.md`](../tests/test-plan-sync-replication-csi-addons.md)

- **Unit:** the method dispatch (absent, `async`, `sync`), the `force` to `planned` inversion, the `site` resolution and the missing-site error, the status-code to gRPC-code mapping including the `409` against `412` distinction, and the status DTO mapping. These run against a mock backend HTTP server with no cluster, because the dispatch and the classification are pure driver logic.
- **Integration:** the full `VolumeReplication` reconcile against the csi-addons sidecar and a mock backend, proving the `Enable` then `Promote` sequence, the promote poll (`409` in progress to `200`), and the condition surface RamenDR reads. Also the operator provisioning (§8.5, §8.6): that `enableSyncReplication` and the node `site` reach the backend create and add bodies, and that admission enforces their immutability and the site-required rule.
- **E2E:** a real two-site synchronous cluster, a planned switchover and a disaster fail-over driven end to end through RamenDR, with data verified on the target site after each. This is the only class that proves the `412` escalation and the connection entries actually serve the volume, and it needs a two-site cluster the other suites do not.

The risk concentrates in the status-code protocol (§11): a `409` miscoded as the escalation trigger drives RamenDR into a force loop, and a `412` miscoded as a plain retry hangs a disaster fail-over. Those unit scenarios are the ones that must not be cut.

Phase 2's group scenarios become testable only once the group RPC handlers land (§13).

---

## 15. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | Owner                     |
|-----|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------|
| 1   | **Site for `GetVolumeReplicationInfo`.** The RPC carries no class parameters (§5.3), so it cannot read `site`, and a single stretched cluster has no fixed per-cluster site to fall back on either. The data it returns (`lastSyncTime`, lag, bytes) is cluster-wide, and `site` selects only `role`, which this RPC does not surface. **Proposed:** make `site` optional on the status-read routes, so the info read needs no site and stays method-agnostic (see the note below). | Backend team              |
| 2   | **Ramen's sync marker.** Which `DRPolicy` / `schedulingInterval` convention marks a synchronous relationship, and whether the sync `VolumeReplicationClass` must carry a matching interval for Ramen to select it (§8.3). Belongs to the Ramen integration design.                                                                                                                                                                                                                  | RamenDR integration owner |
| 3   | **Promote poll backoff.** The driver returns `409` "in progress" as retryable and relies on the sidecar's reconcile cadence to re-poll. Confirm that cadence is fast enough for a planned switchover's downtime budget, or whether the driver should poll inline within one RPC up to a bound.                                                                                                                                                                                      | Operator team             |
| 4   | **Host NQN on promote.** A completed promote builds its connection entries without a host NQN, so a volume with `allowed_hosts` answers `409` instead of its result (`sync-replication.md` §8). Confirm RamenDR-driven volumes never set `allowed_hosts`, or thread the NQN through the promote.                                                                                                                                                                                    | Backend team              |

**Open Question 1, proposed resolution.** Make `site` optional on `GET V/replication/status`, `GET V/replication/sync-status`, and the two `G/...` status routes. A present but invalid site stays a `400`. An absent site returns the cluster-wide status, with `role` taken from the volume's assigned `sync_active_site` or omitted. The mutating routes (promote, demote) keep `site` required, because the driver always reads it from the class there. This keeps one source of truth for `site` (the class, §8.2) and leaves `GetVolumeReplicationInfo` calling `GET .../replication/status` unchanged and method-agnostic. A driver `--site` Deployment flag used only for this RPC is a stopgap, and is not preferred because it makes `site` a second, driftable fact outside the class.
