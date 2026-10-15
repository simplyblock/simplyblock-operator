# Design Document: Synchronous Replication through csi-addons

**Status:** Draft, partially implemented (see [Implementation Status](#implementation-status))  
**Author:** Geoffrey (geoffrey1330)  
**Date:** 2026-10-14 (last updated 2026-10-15)  
**Test Plan:** [`tests/test-plan-sync-replication-csi-addons.md`](../tests/test-plan-sync-replication-csi-addons.md)  
**Work Plan:** [`tasks/work-plan-sync-replication-csi-addons.md`](../tasks/work-plan-sync-replication-csi-addons.md)

---

## Phasing Overview

| Phase               | Status                        | csi-addons surface                   | Backend routes                                                             |
|---------------------|-------------------------------|--------------------------------------|----------------------------------------------------------------------------|
| **Phase 1** (§5–§9) | Partial (verbs done, §6 open) | `VolumeReplication` (per volume)     | `V/replication/{failover,demote,sync-status,status,failback}`, `V/connect` |
| **Phase 2** (§14)   | Partial (dispatch done)       | `VolumeGroupReplication` (per group) | `G/replication/{failover,demote,sync-status,status,failback}`              |

Phase 1 makes a single sync-replicated volume follow its pod between the two sites of a stretched cluster: the node plugin signals the pod's site, the replication controller promotes there, and staging is gated on the promote. Phase 2 adds the consistency-group variant, which the backend already serves as one unit (one gate, one promote task for all members). Phase 2 reuses Phase 1's method dispatch, site signaling, and gating unchanged, so it ships independently once the group routes are wired in the driver.

## Implementation Status

As of 2026-10-14 parts of this design are implemented on the `sync-replication` branch. The document is written in the present tense throughout; this section is the map of what that present tense already refers to and what is still a plan, so a reader does not take the whole design as either built or unbuilt.

**Implemented and tested:**

- **Method dispatch (§5).** `resolveMethod` and the `method` parameter in `csi-driver/internal/csi/controller/replication_sync.go`. Tests: `TestResolveMethod`. The site, which this helper currently also reads from a class parameter, moves to the `VolumeReplication` annotation (§6.2), listed under "not yet implemented" below.
- **The sync write verbs (§7).** The `PromoteVolume`/`DemoteVolume` sync branches with the `force`-to-`planned` inversion and the `409`/`412` classification (`classifySyncError`), the no-op `Enable`/`Disable`/`Resync`, and the consistency-group variants driven through the same RPCs. The backend calls live in `atlas-lib/controlplane/syncreplication.go` (`SyncPromote`, `SyncDemote`, `SyncPromoteGroup`, `SyncDemoteGroup`). Tests: `TestSyncPromoteDispatch`, `TestSyncPromoteStatusProtocol`, `TestSyncPromoteMissingSite`, `TestSyncDemoteDispatch`, `TestSyncDemoteGateRefusalIsRetryable`, `TestSyncNoOpVerbs`, `TestSyncGroupDispatch`, and the atlas-lib client tests `TestSyncPromote`/`TestSyncDemote`/`TestSyncPromoteGroup`/`TestSyncDemoteGroup`.
- **Operator provisioning (§9.4–§9.6).** The `FN_SYNC_PROMOTE`/`FN_SYNC_RESYNC` runner registration (`TestTheSyncReplicationRunnersAreRegistered`), `EnableSyncReplication` on `ClusterDeploymentConfig`/`StorageCluster` and its `sync_replication` cluster-create body, and `Site` on `NodeGroup`/`StorageNode.spec.config` with the `SiteMissing` provisioning hold (`TestSitesAreCheckedAgainstTheTemplate`).
- **The scheduling-driven node path, baseline parts (§6).** The PVC-to-`VolumeReplication` correlation in `atlas-lib/kube` (`SyncVolumeReplicationForClaim`, `NewSyncResolver`); the node's per-PVC detection with the fail-closed-until-synced, trust-the-cache gate (`detectSyncReplication`); the switchover gate that holds on the promote (`ensureSyncPromoted`); the site read (`readNodeSite`); and the driver wiring that stands up the informers and resolver when the VolumeReplication CRD is present (`enableSyncReplication`). The node does not demote the old site on unstage (§6.5): the new site's planned promote fences it. Unit tests cover each step; a sync volume whose promote has not completed holds at staging, and an ordinary volume is untouched. The class-by-site lookup built here (`VolumeReplicationClassForSite`) is superseded by the single class of §9.1.
- **`GetVolumeReplicationInfo` for sync (§5.3, §8).** It reads `GET V/replication/status`, which the backend fills from the sync status on a sync cluster, and a sync volume (no async relationship) resolves the chain walk to itself rather than erroring (`TestGetVolumeReplicationInfoServesASyncVolume`). The csi-addons response carries only `LastSyncTime`, which it sets.

**Not yet implemented:**

- **The site signal (§6.2).** The first implementation patched `spec.volumeReplicationClass` (`patchVolumeReplicationToSite`), which a live run on 2026-10-15 showed is rejected on every move: that field is immutable, so the baseline stage works (the class is set at creation) but no move can change it. §6.2 is re-specified to carry the site in the mutable `simplyblock.io/sync-site` annotation the node patches, with one site-agnostic class (§9.1). The node RBAC stays `patch` on volumereplications, with no `create` or `delete`. This replaces `patchVolumeReplicationToSite` and the site-from-class read; it is the primary remaining work.
- **The driver reads the site from the annotation (§5.2, §6.3).** `PromoteVolume` and `DemoteVolume` correlate the volume to its `VolumeReplication` and read `simplyblock.io/sync-site`, rather than taking the site from the class parameter.
- **The stale-cache handling of the node-stage determination (§6.6, Open Question 2):** whether a cache-miss does one live API confirm or trusts the resync window. The node currently trusts the cache.
- **Unstage teardown robustness (§12, Open Question 4):** a bounded NVMe disconnect and a teardown that survives a missing stashed context, so a move is not blocked at the attach layer by a hung unstage. A separate csi-driver work item.

This also refines the Phase 0 rows: P0-2 (runner table) is done, and the operator half of P0-3 (the CR knobs) is done. The sync backend routes (P0-1) consumed so far are `failover`, `demote`, and `replication/status`; `sync-status` and `failback` are not called (the gate reuses the existing site-less `V/connect`).

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                             | Kind                    | Blocks              | Status                                                                                                |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------------------|-------------------------------------------------------------------------------------------------------|
| P0-1 | The v2 sync-replication REST routes (`V/replication/{failover,demote,sync-status,status,failback}`, `G/...`, `V/connect`) and the `site` query parameter | Control plane (`sbcli`) | Phase 1 and Phase 2 | On the `sync-replication` branch, not yet released                                                    |
| P0-2 | The `TasksRunnerSyncPromote` (`FN_SYNC_PROMOTE`) and `TasksRunnerSyncResync` (`FN_SYNC_RESYNC`) background services                                      | Control plane (`sbcli`) | Promote and resync  | On the branch; the operator must add both to its runner table (§9.4)                                  |
| P0-3 | A cluster created with `sync_replication: true` and every node assigned a `site`                                                                         | Control plane (`sbcli`) | Everything          | Deploy-time only, no conversion of a live cluster; the operator drives both from the CRs (§9.5, §9.6) |
| P0-4 | The upstream `VolumeReplication` / `VolumeReplicationClass` CRDs and the kubernetes-csi-addons manager and sidecar                                       | Ecosystem               | Phase 1             | Vendored by the async design (`design-csi-addons-replication.md` P0-5)                                |
| P0-5 | The upstream `VolumeGroupReplication` / `VolumeGroupReplicationClass` CRDs                                                                               | Ecosystem               | Phase 2             | Vendored by the async design                                                                          |

Without P0-1 and P0-2 the driver has no sync endpoints to call, so every sync-method switch fails. Without P0-3 the backend answers the sync routes as async (the behavior is chosen by the cluster's `sync_replication` flag), so a class carrying `method: sync` against a non-sync cluster is a configuration error the driver reports (§11). P0-4 and P0-5 are shared with the async design and already in the chart.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [The csi-addons Machinery](#4-the-csi-addons-machinery)
5. [Method Dispatch](#5-method-dispatch)
6. [Scheduling-Driven Switchover](#6-scheduling-driven-switchover)
7. [Sync Replication Verbs](#7-sync-replication-verbs)
8. [Steady-State Status and Conditions](#8-steady-state-status-and-conditions)
9. [Classes, Site Resolution, and Operator Provisioning](#9-classes-site-resolution-and-operator-provisioning)
10. [Coexistence with Async Replication](#10-coexistence-with-async-replication)
11. [Backend API Requirements](#11-backend-api-requirements)
12. [Failure Modes and Fallback](#12-failure-modes-and-fallback)
13. [Observability](#13-observability)
14. [VolumeGroupReplication](#14-volumegroupreplication)
15. [Testing Strategy](#15-testing-strategy)
16. [Open Questions](#16-open-questions)

---

## Overview

A simplyblock cluster can be deployed to span exactly two sites, with every logical volume store replicated synchronously to the other site (the contract is `sbcli/docs/sync-replication.md`). A single Kubernetes cluster spans the two zones that map to those sites. A sync volume keeps one identity on both sites and is served from whichever site its store is currently led from, so a pod using that volume has to run on the led site, or the store's leadership has to move to the pod.

This design makes the leadership follow the pod, driven by scheduling and gated at attach. There is no RamenDR in this topology: one Kubernetes cluster is one failure-isolation boundary for the orchestrator, so there is nothing to relocate *between*, and Ramen's hub-and-managed model does not apply. What drives the switchover instead is the fact the cluster already produces on its own — where the pod is scheduled.

The control model is three parts:

- **The administrator declares the relationship once.** A `VolumeReplication` (or, for a consistency group, a `VolumeGroupReplication`) on the PVC, naming the single sync `VolumeReplicationClass` (§9.1). The administrator never flips `replicationState` by hand and never picks a site; the node records the active site on the object, so the administrator's declaration never has to change.
- **The node plugin signals the pod's site.** When a pod is scheduled and its volume is staged, `NodeStageVolume` runs on the pod's node, reads that node's site, and sets it on the volume's `VolumeReplication` in the `simplyblock.io/sync-site` annotation (§6.2). This is the only place the scheduling fact is known, so it is where the switch begins.
- **The replication controller performs the switch, and staging is gated on it.** The csi-addons `VolumeReplication` reconciler sees the patched object and promotes the volume on the annotated site, which the backend does by fencing the other site and taking leadership here (§7). `NodeStageVolume` does not fetch the connection string or attach the NVMe-oF device until the volume is confirmed promoted on this site (§6.4). A volume is therefore never connected on a site it is not led from, which is the property that makes the moving leadership safe.

All of this is gated on the volume being synchronous: `method: sync` on its class and a cluster created with `sync_replication: true`. A volume that is neither stages exactly as it does today, with no site signaling and no gate (§6.6). The backend routes, the `force`-to-`planned` inversion, and the `409` / `412` status protocol are unchanged from the switchover primitives the control plane already serves (§7, §11, §12); what this revision changes is who invokes them and when.

---

## 1. Background

The simplyblock CSI driver implements the csi-addons replication service in `csi-driver/internal/csi/controller/replication.go`: `EnableVolumeReplication`, `DisableVolumeReplication`, `PromoteVolume`, `DemoteVolume`, `ResyncVolume`, and `GetVolumeReplicationInfo`, plus the group variants. Today, every one of these targets the async snapshot-replication backend. `EnableVolumeReplication` attaches a `ReplicationPolicy` named by the `replicationPolicyID` class parameter. `PromoteVolume` resolves the replication chain and promotes its active end. The status verbs read the per-volume async replication status and map it onto the csi-addons conditions (`design-csi-addons-replication.md` §5, §6). The async surface is driven by RamenDR across two clusters.

A synchronous two-site cluster has none of these shapes. There is no replication chain and no per-volume relationship, because a volume keeps one identity and is served from whichever site its store is led from. There is no policy to attach, no cadence, and no reverse-pipe reconfiguration, because both sites are always in sync or catching up. The switchover is a leadership move of the store plus a per-volume open on the target site, gated on the replicas being in sync, or on the other site being provably lost for a disaster promote. The backend already exposes this as a distinct set of v2 routes keyed on the caller's `site` (`sync-replication.md` §3), and it decides sync against async from the cluster's own `sync_replication` flag.

There is also no second cluster, so there is no orchestrator above the driver. The async design leaned on RamenDR to decide when and where a volume should be primary; a stretched sync cluster has to decide that itself. The decision it can make without an orchestrator is the obvious one: a volume should be led from the site where its pod is running. The node plugin is the component that learns that fact, at the moment the volume is staged for the pod, so this design puts the decision there and lets the existing replication controller carry it out.

---

## 2. Goals and Non-Goals

### Goals

- Make a sync volume's store leadership follow its pod between the two sites of one stretched cluster, with no orchestrator above the driver (§3, §6).
- Trigger the switch from the pod's scheduled site, read by the node plugin at `NodeStageVolume`, by signaling that site onto the volume's `VolumeReplication` (§6.1, §6.2).
- Have the existing csi-addons `VolumeReplication` reconciler perform the promote and demote in response to that signal, reusing the backend route mapping, the `force`-to-`planned` inversion, and the `409` / `412` protocol unchanged (§7, §12).
- Gate staging: never fetch the connection string or attach a volume's device on a site until the volume is confirmed promoted there (§6.4).
- Apply all of this only to a synchronous volume — `method: sync` on its class and a cluster with `sync_replication: true` — and leave every other volume's staging and every async class untouched (§6.6, §10).
- Select the method per class, from a `VolumeReplicationClass` or `VolumeGroupReplicationClass` parameter; the active site rides on the `VolumeReplication` as an annotation, not on the class (§5, §9.1).
- Surface the switch state as the csi-addons conditions the node plugin gates on, and as a fresh `lastSyncTime` while in sync (§8).
- Add the group variant, driving a consistency group as one unit when a pod mounts several of its members (Phase 2, §14).
- Expose the deploy-time sync knobs on the operator CRs: a `StorageCluster` toggle to create a sync cluster, and a per-node `site` at storage-node add (§9.5, §9.6).
- Report a misconfiguration — a sync class against a non-sync cluster, a missing site, a volume with no resolvable class — as a clear, non-retryable staging error rather than a silent wrong path (§11, §12).

### Non-Goals

- **The synchronous backend itself.** The two-site model, the gates, the promote and demote mechanics, the disaster promote, and the automatic resync are the control plane's, specified in `sync-replication.md` and summarized here only where the driver depends on them.
- **RamenDR for sync.** A stretched single cluster has nothing for Ramen to relocate between, so Ramen does not drive sync switchover and is not deployed for it. Ramen remains the driver of the separate, multi-cluster async Regional-DR product (`design-csi-addons-replication.md`), which this design leaves untouched (§10).
- **Zero-downtime handover.** A pod is stopped on the old site and started on the new one by the scheduler; the volume's leadership moves in the gap. The application sees the ordinary reschedule outage, not a live migration of a running process.
- **Choosing where a pod runs.** The scheduler places the pod; this design reacts to that placement. Pinning a workload to a site, spreading it, or keeping it near its led store is the user's affinity configuration, out of scope here.
- **The backend's lost-site fencing.** The controller escalates a planned promote to a forced one on its own when the backend reports a site lost, so a workload Kubernetes reschedules onto the surviving zone comes back with no human in the loop (§7.1, §12). What makes that safe is that the losing site stops serving writes when it loses control-plane quorum, which is the backend's guarantee, not the driver's. This design relies on it and does not implement it; its soundness against a partitioned-but-alive site is Open Question 1 (§16).
- **Migrating an async relationship to sync or the reverse.** A cluster is sync or async at deploy time (P0-3). A volume does not change method in place.
- **Exposing sites or `lost_site` through the CSI provisioning API.** Site enters the node path from the node's own topology label (§6.1), and the switchover's target is recorded on the `VolumeReplication`'s site annotation (§6.2). The cluster's site topology stays a control-plane concern.

---

## 3. Architecture Overview

```
┌────────────────────────────────────────────────────────────────────────────┐
│                 Kubernetes (one cluster spanning both zones)                │
│                                                                              │
│   Pod scheduled on a node in site B                                          │
│      │                                                                       │
│      ▼                                                                       │
│   CSI node plugin ── NodeStageVolume(volume) on that node                    │
│      │   1. volume is sync? (method=sync + cluster sync)      (§6.6)         │
│      │   2. read this node's site label  ─► site B           (§6.1)         │
│      │   3. correlate volume ─► its VolumeReplication         (§6.2)         │
│      │   4. patch VR.spec.volumeReplicationClass = <site-B class>, primary    │
│      │   5. wait until VR reports promoted-on-B               (§6.4)  ◄─┐    │
│      │   6. GET V/connect (led site = B now) ─► attach, mount              │    │
│      ▼                                                                  │    │
│   kubernetes-csi-addons manager ── sidecar ── CSI replication RPCs      │    │
│      │                                                                  │    │
│   ┌──▼───────────────────────────────────────────────────────────┐    │    │
│   │ Driver replication controller (replication.go)                │    │    │
│   │   PromoteVolume ─► POST V/replication/failover?site=B&planned= │────┘    │
│   │   DemoteVolume  ─► POST V/replication/demote?site=A            │ status  │
│   └───────────────────────────────────────────────────────────────┘         │
│   VolumeReplicationClass (one, site-agnostic):                                   │
│     replication.storage.simplyblock.io/method: sync                              │
│   VolumeReplication annotation: simplyblock.io/sync-site: site-a|site-b          │
└────────────────────────────────────────────────────────────────────────────┘
               │ HTTP (webapi client, service-account bearer token)
┌──────────────▼─────────────────────────────────────────────────────────────┐
│                     Simplyblock Backend API (two-site cluster)              │
│  POST .../volumes/{v}/replication/failover?site=S&planned=                  │
│  POST .../volumes/{v}/replication/demote?site=S                             │
│  GET  .../volumes/{v}/replication/status?site=S                            │
│  GET  .../volumes/{v}/connect   (no site; returns the led site)             │
└────────────────────────────────────────────────────────────────────────────┘
```

**One control plane, two sites, one Kubernetes cluster.** The two sites share one control plane and one FoundationDB (`sync-replication.md` §1). A single Kubernetes cluster spans both zones, and one driver deployment serves volumes in either zone. Site is not a property of the cluster or the deployment. A node's site is a property of the node, carried on its topology label (§6.1), and a switchover's target site is that of the pod being staged.

**The switch begins at the node, not at a controller.** The csi-addons `VolumeReplication` reconciler runs cluster-wide and does not know where a pod was scheduled. The node plugin does, because `NodeStageVolume` runs on the pod's node. So the node plugin reads the site and writes it onto the `VolumeReplication`, and the reconciler, which already turns a `VolumeReplication` into backend promote and demote calls, carries it out. The division is deliberate: the node contributes the one fact only it has, and the existing controller keeps owning the switchover mechanics.

**Staging is the gate.** `NodeStageVolume` is the single point where a volume becomes connectable on a node, so it is where the connection is gated on the promote. The node plugin waits for the `VolumeReplication` to report the volume promoted on this node's site before it reads the connection entries (`GET V/connect`, which returns the led site) and attaches the device. An un-promoted or wrong-site volume is never connected, which is what lets leadership move under a rescheduling pod without a window in which a node could write to a demoted copy.

**The dispatch is per volume, not per driver.** Nothing about the driver deployment is sync-specific. The same binary serves async and sync volumes, deciding per staging and per replication RPC from the volume's class and the cluster's flag (§5, §6.6). This is what lets a fleet run both.

---

## 4. The csi-addons Machinery

Nothing new is deployed. The CRDs (`volumereplications` and `volumereplicationclasses` in `replication.storage.openshift.io/v1alpha1`, and the group variants), the kubernetes-csi-addons manager, and the sidecar are the ones the async design vendors in the chart (`design-csi-addons-replication.md` §4). This design adds no simplyblock CRD and no new controller. It extends the dispatch inside the existing replication RPC handlers (§5), adds a sync client for the backend routes (§7), and adds two things to the node plugin: the site read and the `VolumeReplication` patch at `NodeStageVolume`, and the promote gate before attach (§6).

The replication controller serves the same `ReplicationServer` capabilities as today. The RPC sequence is the one the sidecar already issues for a `VolumeReplication` whose `spec.replicationState` is `primary`: `EnableVolumeReplication` then `PromoteVolume`. What is new is who causes a `VolumeReplication` to become `primary` on a given site — the node plugin's patch, in place of a RamenDR `DRPlacementControl` — and the staging gate that waits for the result.

**The node plugin becomes a Kubernetes API client.** Signaling the site means the node plugin reads the `Node`, the volume's `PersistentVolume` and `PersistentVolumeClaim`, and the `VolumeReplication` and `VolumeReplicationClass` objects, and patches the `VolumeReplication`. The node `DaemonSet`'s service account therefore gains `get` on nodes, persistentvolumes, and persistentvolumeclaims, `get` and `list` on volumereplicationclasses, and `get`, `list`, `watch`, and `patch` on volumereplications (and the group variants in Phase 2). The correlation primitives (volume handle to PV to PVC to the referencing `VolumeReplication`) are the Kubernetes object-correlation helpers in `atlas-lib`, not a second copy in the driver.

This is a real widening of the node plugin's privilege, which today is close to none, and it is an accepted part of this design rather than an open question. The access is narrow in kind: every verb but one is read-only, and the one write is `patch` on `VolumeReplication`, with no `create`, no `delete`, and no access to any `Secret`. The bound worth stating, because RBAC cannot tighten it, is that `patch` cannot be scoped to "only the volumereplications of volumes on this node": a `DaemonSet` needs a `ClusterRole`, so every node can patch every `VolumeReplication` in the cluster. A compromised node can therefore disturb the replication of workloads in other namespaces (flip a `replicationState`, change a site annotation), though not read a secret or reach any other resource, so the blast radius is bounded to replication objects. That is the posture this design accepts in exchange for the node-signals-the-VR model (§6.2). If it ever needs tightening, the path is to route the patch through the controller the node calls rather than patching directly, or to gate `VolumeReplication` writes behind an admission webhook that admits a node's patch only for a volume staged on that node; neither is built now. The grant is still minimized to the verbs and resources above and reviewed under the `rbac-hardening` skill.

---

## 5. Method Dispatch

### 5.1 The class parameter and the site annotation

A `VolumeReplicationClass` (and, in Phase 2, a `VolumeGroupReplicationClass`) carries one parameter, read by the driver from `req.GetParameters()`:

| Parameter                                   | Values          | Meaning                                                                                         |
|---------------------------------------------|-----------------|-------------------------------------------------------------------------------------------------|
| `replication.storage.simplyblock.io/method` | `sync`, `async` | The backend to drive. Absent is treated as `async`, so every existing class keeps its behavior. |

A sync cluster has a single sync class (§9.1); the site is not a class parameter. The site a sync write acts on rides on the `VolumeReplication` object, in the `simplyblock.io/sync-site` annotation the node sets as the pod is scheduled (§6.2), and the driver reads it there (§5.2). The site is a mutable, per-volume field because it has to move with the pod, and `spec.volumeReplicationClass` is immutable once set, so a site in the class could never move. The site name follows the backend's rule (`[A-Za-z0-9][A-Za-z0-9._-]{0,62}`); the driver does not validate it against the cluster topology, because the backend rejects an unknown site with a `400` that the driver surfaces (§11).

### 5.2 Where the dispatch happens

Each replication RPC reads `method` first and branches:

```go
// method selects the replication backend this class drives. Absent or "async"
// keeps the existing snapshot-replication path (design-csi-addons-replication.md);
// "sync" drives the two-site synchronous backend (sync-replication.md). The site
// a sync write acts on is read from the VolumeReplication's site annotation, not
// from the class (§6.2).
const (
	methodParam = "replication.storage.simplyblock.io/method"
	siteAnnotation = "simplyblock.io/sync-site"

	methodAsync = "async"
	methodSync  = "sync"
)
```

A helper resolves the method from the class parameters. The async branch is the current code, untouched. For the sync path, the write RPCs (`PromoteVolume`, `DemoteVolume`) read the site from the `VolumeReplication`'s `simplyblock.io/sync-site` annotation: the driver correlates the volume to its `VolumeReplication` (the same correlation the node uses, §6.2) and calls the sync client (§7) with that site. Reading the site from the object rather than from the class is the one new dependency this places on the driver's controller, which already correlates a volume to its `VolumeReplication` for the status read (§5.3). The node plugin reads the method off the class it correlates to, to decide whether a volume is synchronous at all (§6.6).

### 5.3 The RPCs without class parameters

`GetVolumeReplicationInfo` does not carry `parameters` in the csi-addons replication service (only the volume handle and secrets), so it cannot read `method` or `site` the way the other verbs do. This is the one dispatch gap, resolved by the backend rather than the class: the driver calls `GET V/replication/status` for the info read regardless of method, and the backend fills it from the sync status on a sync cluster (§8). The status-read route takes `site` as optional (§8, §11), so this call needs none: it returns the cluster-wide status, with `role` taken from the volume's assigned site or omitted. Every write verb (`Enable`, `Disable`, `Promote`, `Demote`, `Resync`) does carry `parameters`, so the dispatch for the switchover itself is unambiguous.

---

## 6. Scheduling-Driven Switchover

This is the core of the design: how the pod's placement becomes a leadership move, and how staging is gated on it. The switchover primitives it drives are in §7; this section is the control flow around them.

### 6.1 The trigger: the node's site

`NodeStageVolume` runs on the node the pod was scheduled to, with the backend volume handle in `req.GetVolumeId()`. The node's site is read from the node object's topology label. A sync cluster's two sites are its two zones, and discovery seeds each node's site from `topology.kubernetes.io/zone` (§9.6), so the node plugin reads the site from that same zone label on its own `Node`. The node plugin knows which node it runs on from the Kubernetes downward API (`spec.nodeName`), already available to the `DaemonSet`.

A node belongs to exactly one site for its whole life (§9.6, the field is immutable), so the site read is stable and needs no caching invalidation beyond the node plugin's own lifetime.

### 6.2 Signaling the site: the VolumeReplication's site annotation

The site a sync volume is led from is not carried in its `VolumeReplicationClass`. A sync cluster has one site-agnostic sync class (§9.1), and the active site rides on the `VolumeReplication` itself, in a mutable annotation `simplyblock.io/sync-site` the node sets. `spec.volumeReplicationClass` is immutable on the csi-addons CRD (a `self == oldSelf` rule), so a site encoded in the class could never move a volume. Keeping the site in an annotation keeps the signal a single mutable patch that follows the pod.

With the node's site in hand, the node plugin finds the volume's `VolumeReplication` and patches it:

1. **Correlate the volume to its `VolumeReplication`.** From the volume handle, find the `PersistentVolume` whose `csi.volumeHandle` matches, read its `claimRef` to the `PersistentVolumeClaim`, and find the `VolumeReplication` in the PVC's namespace whose `spec.dataSource` names that PVC. For a group member, the `VolumeGroupReplication` whose selector matches the PVC (§14). The correlation uses the `atlas-lib` helpers (§4).
2. **Patch the site annotation.** Set `metadata.annotations["simplyblock.io/sync-site"]` to the node's site and `spec.replicationState` to `primary`. The patch is idempotent: a `VolumeReplication` already carrying this site and `primary` is unchanged, and staging proceeds straight to the gate (§6.4).

The patch is the whole signal, and it touches one mutable annotation and one mutable spec field, so there is no object to delete and recreate and no window in which the PVC has no `VolumeReplication`. The promote to the annotated site is the controller's (§6.3).

This splits ownership of the object cleanly, which is what lets the administrator and the node both manage it. The administrator, or the GitOps that manages them, owns the `VolumeReplication`'s spec: it declares the volume protected and names the one sync class, and that spec never has to change. The node owns the `simplyblock.io/sync-site` annotation, a field the administrator's spec does not set, so a GitOps re-apply of the spec does not revert the node's site and the two do not fight. Under server-side apply the spec and the annotation are distinct field managers; a GitOps tool that strips unmanaged metadata on apply excludes this annotation, the one deployment rule the split needs (§9.1).

The signal needs `patch` on `volumereplications`, with the `get`, `list`, and `watch` the gate's informer uses and the `volumereplicationclasses` read of §6.6 (§4). It needs no `create`, no `delete`, and no access to any `Secret`, the posture §4 describes.

### 6.3 The controller performs the switch

The csi-addons `VolumeReplication` reconciler observes the patched object and issues the RPC sequence for a primary: `EnableVolumeReplication` (a no-op on sync, §7.3) then `PromoteVolume`. `PromoteVolume` reads the target site from the object's `simplyblock.io/sync-site` annotation, correlating the volume to its `VolumeReplication` to do so (§5.2), and calls `POST V/replication/failover?site=<that site>&planned=true`, and the backend performs the leadership move — it fences the other site and takes leadership here as one gated operation (`sync-replication.md` §4), so the node plugin issues no separate demote against the old site, neither for the switch nor on unstage (§6.5).

The node's patch is what drives the reconcile. On the first stage it sets `replicationState: primary` onto a fresh object, and the reconciler promotes naturally. On a later move the `simplyblock.io/sync-site` annotation changes but `replicationState` is already `primary`, and an annotation-only change does not re-invoke `PromoteVolume`: the object is already `primary` with the `Completed` condition set, so the reconciler has nothing to do and the backend's leadership never moves. The node therefore forces the promote to re-run the way RamenDR drives a failover — it transitions `replicationState` `primary → secondary → primary`, a real spec change the reconciler acts on by re-issuing `DemoteVolume` then `PromoteVolume`, the latter the `failover?site=<this site>` that moves leadership. The intermediate demote carries this node's site and is a backend `204` no-op (§7.2); it exists only to make the following `primary` a transition the reconciler sees, not to fence anything — the failover fences the old site. The node sequences the two across its staging retries, setting `secondary`, waiting until the status reports `Secondary` (so the demote is not lost), then setting `primary` (§6.4). This resolves Open Question 3.

A planned promote requires both sites in sync. When the other site is lost — the disaster case, where the pod could only have been scheduled here because the other zone is down — the backend answers `412`, and the controller escalates to a forced promote (`planned=false`) on its own. This is what makes an AZ loss self-healing: Kubernetes reschedules the workload onto the surviving zone, the stage promotes there, and the volume comes back with no operator in the loop. Holding for a human instead would strand the workload in `ContainerCreating` for the length of the outage, which defeats the resilience the two-site cluster exists for. The mechanism is the same `force`-to-`planned` inversion the backend already defines (§7.1), and its safety rests on the backend fencing the lost site (§12, Open Question 1).

### 6.4 Gating the connection string

`NodeStageVolume` does not read the connection entries or attach the NVMe-oF device until the volume is confirmed promoted on this node's site. After the patch (§6.2) it waits on the `VolumeReplication` status:

- **Promoted here:** `status.state: Primary` with the `Completed` condition true and the `VolumeReplication`'s `simplyblock.io/sync-site` annotation naming this node (§8). The node plugin then calls `GET V/connect` and attaches the returned entries, completing staging. `V/connect` takes no site and returns the led site's triplet, which the promote above has just made this node's site, so a node never has to name a site it cannot know it is led from (`sync-replication.md` §3).
- **In progress:** the promote is running (the condition is not yet `Completed`, or the backend promote route is still answering `409` "in progress"). `NodeStageVolume` returns a retryable error (`codes.Unavailable`) so the kubelet re-invokes it. The next invocation finds the patch already applied (idempotent) and re-checks the status, so the promote poll advances across `NodeStageVolume` retries rather than blocking one call for the whole switchover.
- **On a move, the forced re-promote comes first.** When the volume is already `Primary`/`Completed` but annotated for another site, the gate does not release on that status — it would be stale, since the status is site-agnostic and cannot say which site it reflects. The node drives `replicationState` `primary → secondary → primary` (§6.3) and releases only once the status returns to `Primary`/`Completed` after that cycle, which is the promote on *this* site. The `secondary` step is skipped on a first promote, where the status is not yet `Completed` and the natural promote already targets this site.
- **Refused or failed:** a planned-gate refusal (`409` with `problems`) or a lost-peer `412` surfaces as a staging error carrying the backend message (§12). The pod stays `ContainerCreating` with an event explaining the hold, which is the correct visible state for a volume that cannot yet be served here.

The gate is the safety property of the whole design. Because the connection entries are read only after the promote is confirmed, and the backend has fenced the other site as part of that promote, a node never holds a writable path to a site that is not the volume's current leader. A stale pod on the old site cannot be the one connected, because its site was demoted before this one was promoted, and this one is the only one that reaches `GET /connect`.

The node plugin does not poll inline. Each `NodeStageVolume` checks the `VolumeReplication` status once and returns, so a call never holds a node-plugin staging slot while a promote runs or hangs, and a stuck promote on one volume cannot starve the staging of others on the node. The wait is spread across the kubelet's re-invocations, which adds the kubelet's retry backoff to a fast switchover, a cost accepted to keep staging non-blocking.

### 6.5 The old site on unstage

When a pod moves off a site, the kubelet calls `NodeUnstageVolume` on the old node, which tears down the device and frees the single-writer attachment. The node does not demote the old site here: the switch's own fencing does it. The new site's planned promote (`failover?planned=true`, §6.3) fences the old site and takes leadership as one gated backend operation, and the single-writer (`ReadWriteOnce`) access mode orders the move so the volume detaches from the old node before the new node attaches and promotes. An explicit unstage-time demote was tried and removed: it put a backend call on the teardown's critical path — the one path a move depends on completing promptly (§12) — for no correctness benefit, since the next promote fences the old site regardless and a demote of an already-handled volume is a `204` no-op (§7.2). A `NodeUnstageVolume` that cannot complete holds the single-writer attachment and blocks the move at the Kubernetes attach layer, before any of §6.2 runs on the new node (§12).

### 6.6 Precondition: only synchronous volumes

Everything in this section applies only to a volume that is synchronous. The node plugin treats a volume as synchronous when both hold:

- its correlated `VolumeReplication` names a `VolumeReplicationClass` with `method: sync` (§6.2), and
- the cluster is a sync cluster, which the backend is authoritative on through the `sync-status` route (`400` on a non-sync cluster, §11).

A volume with no `VolumeReplication`, or one whose class is async, is staged exactly as today: no site read, no patch, no gate, straight to `GET /connect` and attach. This is what keeps the change additive — an ordinary volume on a sync-capable cluster, and every volume on an async cluster, is untouched. A volume whose class says `sync` on a cluster that is not sync is a deployment error reported at staging (§12), never a silent async attach.

The determination is per PVC and made at stage time, so a sync volume and a plain one may share a `StorageClass`. The node plugin resolves the volume to its PVC (the PVC name and namespace arrive in `volume_context`) and reads whether a sync `VolumeReplication` references it. The read is served from a `VolumeReplication` informer the node plugin watches (the `watch` RBAC of §4), so a stage reads a local cache rather than making a per-stage API call, and it runs only on a sync cluster, so an ordinary volume on an async cluster is never looked up. A volume the cache resolves to a sync VR takes the §6 path; one it resolves to no sync VR stages exactly as today. When the determination cannot be made because the informer has not synced, staging fails closed with a retryable error and the kubelet re-stages, so a volume is never attached as plain on a guess. The one residual this leaves, a synced cache briefly stale against a just-created sync VR, is Open Question 2 (§16).

### 6.7 Crash-safety of the signal

The signal is a single idempotent patch (§6.2), not a sequence, so a crash has nothing half-done to recover. The `VolumeReplication` always exists, so a volume is never briefly unrecognizable as sync (§6.6). A `NodeStageVolume` interrupted after the patch and before the promote completes re-patches the same annotation on its next invocation, a no-op, and re-checks the gate (§6.4); one interrupted before the patch applies it on the next. The node plugin restarting mid-stage resumes from the object's current state rather than from a step counter, because every stage drives toward the same one state: the `VolumeReplication` annotated with this node's site and `primary`.

---

## 7. Sync Replication Verbs

The mapping is fixed by the backend contract (`sync-replication.md` §4) and is the same whether the promote was triggered by a node's patch (§6) or issued directly. Prefix `V = .../clusters/{c}/storage-pools/{p}/volumes/{v}`.

| csi-addons RPC             | Sync route                                                 | Behavior                                                                                       |
|----------------------------|------------------------------------------------------------|------------------------------------------------------------------------------------------------|
| `EnableVolumeReplication`  | none (or `PUT V/` with `replication_policy_id`, `204`)     | No-op. A sync volume has no policy to attach. Returns success.                                 |
| `DisableVolumeReplication` | none                                                       | No-op. Returns success.                                                                        |
| `ResyncVolume`             | none (or `POST V/replication/failback`, body `{}`, `204`)  | No-op. The catch-up is automatic. Returns success.                                             |
| `DemoteVolume`             | `POST V/replication/demote?site=<site>`                    | Fence the volume on this site. `204` (also when not served here), `409` on a gate refusal.     |
| `PromoteVolume(force)`     | `POST V/replication/failover?site=<site>&planned=<!force>` | Serve the volume here. Repeat while `409` "in progress"; `200` carries the connection entries. |
| `GetVolumeReplicationInfo` | `GET V/replication/status` (no site)                       | Read the cluster-wide status (§8); carries back only `lastSyncTime`.                           |

### 7.1 Promote

`PromoteVolume.force` inverts to `planned`: an unforced promote is `planned=true` (a planned switchover that requires both sites in sync), a forced promote is `planned=false` (a disaster promote of a lost site). The backend runs the promote as a background task and the route is a poll: the first call queues the task and answers `409` "in progress" with a `task_id`, and the driver keeps calling until the answer is `200` with the connection entries, or a terminal error. The csi-addons `PromoteVolume` is itself idempotent and retried by the sidecar, so the driver returns `409` "in progress" as a retryable error (`codes.Unavailable` or `codes.Aborted`) and the next reconcile re-issues the poll.

A `200` carries a `SyncPromoteResultDTO` (`{lvol_id, connection_strings}`), the connection entries of this site's triplet. The driver does not act on them here, because node staging reads them separately through `GET V/connect` at the gate (§6.4). The `200` is the statement that the volume was opened on this site, which is also what flips the `VolumeReplication` to `Primary`/`Completed` for the node plugin's gate to release (§8).

A sync volume under this design does not set `allowed_hosts`. A host-restricted volume would make the promote and the `GET /connect` the gate calls build their entries without a host NQN and answer `409` rather than return the connection (`sync-replication.md` §8), which would stall the gate. The provisioning path for a sync volume leaves `allowed_hosts` unset, so the entries are always built and returned, and host admission is not part of the sync switchover.

Because there is no orchestrator above the driver, the escalation from a planned promote to a forced one is a decision inside the cluster, not one RamenDR makes. The controller makes it automatically on the backend's `412`, trusting the backend's lost-site determination because the controller has no better view of the peer than the backend does (§12). The one safety question that decision rests on, whether the losing site can still serve writes while being force-promoted against, is the backend's and is Open Question 1 (§16).

### 7.2 Demote

`DemoteVolume` is a single `POST V/replication/demote?site=`. It is a no-op (`204`) when the volume is not served on this site, so a retried demote, or one that races a promote on the other site, is safe. A `409` is a planned-gate refusal (a desynced store blocks it) and is retryable. A `500` means an ANA RPC failed with nothing recorded, and is also retryable. In this design the single-volume switch issues no demote: the old site is fenced by the new site's planned promote, and the node does not demote on unstage (§6.5). The verb is kept for the csi-addons contract and the group and failback paths.

### 7.3 The no-op verbs

`EnableVolumeReplication`, `DisableVolumeReplication`, and `ResyncVolume` return success without a backend call on a sync class. This matches the async design's tolerance of an `Enable` with no policy on the fail-over target (`design-csi-addons-replication.md` §5.1): the side becoming primary carries no reverse-direction configuration, and on sync there is never any to carry. Returning success keeps the sidecar's `Enable` then `Promote` sequence intact.

---

## 8. Steady-State Status and Conditions

The node plugin's gate (§6.4) and any status reader gate on `GetVolumeReplicationInfo` and on the csi-addons conditions. The backend serves one cluster-wide sync status (`sync-replication.md` §3, the `SyncReplicationStatusDTO`), and `GET V/replication/status` returns it shaped as the existing async `ReplicationStatusDTO` on a sync cluster. `GetVolumeReplicationInfo` reads that route and fills the one field the vendored csi-addons response carries:

| csi-addons field | Sync source                        | Healthy value |
|------------------|------------------------------------|---------------|
| `lastSyncTime`   | the last confirmed in-sync instant | now           |

The `GetVolumeReplicationInfoResponse` of this csi-addons version carries only `lastSyncTime`, so the backend's `lag_seconds` and `bytes_behind`, though present in the status DTO, have nowhere to land in the RPC result and are not reported through it. A later csi-addons version that adds `lastSyncDuration`/`lastSyncBytes` would map them from those fields.

`lastSyncTime` is the load-bearing field, because synchronous replication has no discrete sync cycle. Every write is acknowledged only once it is journaled on both sites, so a volume the backend confirms in sync is current as of the read. The driver reports `lastSyncTime` as the read time whenever the backend reports in sync. When the backend reports a zone behind or a catch-up running, the driver reports the real last-in-sync instant instead, which is older, so a reader holds rather than treating a behind replica as current.

The node plugin's gate reads `status.state: Primary` and the `Completed` condition (§6.4), which a successful promote sets. `GetVolumeReplicationInfo` does not itself set `Completed`, `Degraded`, or `Resyncing`: it carries only `lastSyncTime`, and the conditions are the csi-addons controller's, from the replication operations it drives rather than from this read. The gate therefore waits on the promote result, not on a status probe.

`role` is the site a volume is assigned to, not whether it is currently published. A volume demoted a moment ago still reports `role: primary` on its site while every path is inaccessible (`sync-replication.md`, "Role is not accessibility"). The node plugin therefore never treats `role` as proof that a volume is served and releases its gate only on the `Completed` promote result, not on a role read. The `200` from the promote is that proof, and it is the result of the operation, not a continuous probe.

The status is cluster-wide: every volume and group gets the same `state`, `lag`, and `bytes_behind`, and only `role` is per volume. The driver does not sum or combine per-member status for a group (§14).

---

## 9. Classes, Site Resolution, and Operator Provisioning

### 9.1 The class

A sync cluster has one `VolumeReplicationClass`, naming the simplyblock provisioner and the single `method: sync` parameter of §5.1. It carries no site, so one class serves both zones:

```yaml
apiVersion: replication.storage.openshift.io/v1alpha1
kind: VolumeReplicationClass
metadata:
  name: simplyblock-sync
spec:
  provisioner: csi.simplyblock.io
  parameters:
    replication.storage.simplyblock.io/method: sync
```

There is no `replicationPolicyID` and no `sourceClusterID`, because a sync relationship has no policy and no chain. There is no `ramendr.openshift.io/replicationid` label, because no Ramen selects the class. The administrator creates one `VolumeReplication` per protected PVC (or one `VolumeGroupReplication` per group, §14) naming this class; the active site is the node plugin's to set, on the object's `simplyblock.io/sync-site` annotation (§6.2).

A `VolumeReplication` managed by GitOps carries one deployment rule: the tool must not strip the `simplyblock.io/sync-site` annotation on re-apply, since the node owns it and the administrator's spec does not set it (§6.2). Server-side apply does this by field manager; a tool doing a client-side replace excludes the annotation from what it prunes.

### 9.2 Site resolution

The target site of a switchover is the site of the node staging the volume, read from that node's zone label (§6.1). The node writes it onto the `VolumeReplication`'s `simplyblock.io/sync-site` annotation (§6.2), and the driver's `PromoteVolume` and `DemoteVolume` read it from there to address the backend route (§5.2). This is the inversion from the async design, where a RamenDR relocate chose the class and the class carried the site. Here scheduling chooses the site, the node records it on the object, and the class is the same whichever site is active.

The one RPC that reads neither, `GetVolumeReplicationInfo` (§5.3), needs no site: the status-read route takes `site` as optional and returns the cluster-wide status (§8, §11).

### 9.3 No Ramen class selection

The async design relies on RamenDR to select a `VolumeReplicationClass` by `provisioner`, `schedulingInterval`, and `replicationClassSelector` labels (`design-csi-addons-replication.md` §7.2). Sync in a stretched cluster has no Ramen and no `DRPolicy`, so none of that applies. The single class carries no `schedulingInterval` and no Ramen labels, and there is nothing to select by label, because the active site rides on the `VolumeReplication` annotation the node sets (§6.2) rather than on a class.

### 9.4 Operator work: the sync task runners

The backend's promote and resync run as two management-plane services, `TasksRunnerSyncPromote` (`FN_SYNC_PROMOTE`) and `TasksRunnerSyncResync` (`FN_SYNC_RESYNC`) (`sync-replication.md` §2). On Kubernetes the operator owns the control-plane runner table in `operator/internal/controllers/controlplane/managementapi.go`, and must add both, or a promote answers "in progress" forever (so the node plugin's gate never releases) and no resync ever starts. This is one of the operator-side changes this design requires, alongside the provisioning knobs in §9.5 and §9.6.

### 9.5 Operator work: enabling sync at cluster creation

A sync cluster is created with `sync_replication: true` in the cluster-create body (`sync-replication.md` §2), and this is deploy-time only: a live cluster cannot be converted (P0-3). In the deployment model a cluster is declared by a `ClusterDeploymentConfig`, whose `spec.cluster` template expands into the `StorageCluster` (`operator/api/v1alpha2`). The toggle is added in both, mirroring `enableFailureDomains` and `enableChecksumValidation`, the existing precedents for an immutable cluster-wide mode carried on the document and spent on the cluster.

On the `ClusterDeploymentConfig` cluster template (`ClusterTemplate`):

```go
// EnableSyncReplication opts the cluster into synchronous two-site replication.
// The cluster then spans exactly two sites, and every node group declares a site
// (§9.6). It is on the document because it is immutable on the cluster it lands
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

### 9.6 Operator work: the node site at storage-node add

Every node of a sync cluster is added with a `site` in the storage-node-create body, mandatory on a sync cluster and rejected with `400` on any other (`sync-replication.md` §2). A node is declared by a `NodeGroup` in the `ClusterDeploymentConfig`, which expands into `StorageNode.spec.config`. Site sits in the same two places as `failureDomain` and shares its discovery seeding: a sync cluster's two sites are its two zones, and discovery already seeds the fault group from `topology.kubernetes.io/zone`. That same zone label is what the node plugin reads at staging (§6.1), so the node's site is consistent between the control plane's view and the node plugin's.

On the `ClusterDeploymentConfig` node group (`NodeGroup`), beside `failureDomain`:

```go
// Site is the replication site every worker in this group belongs to, one of the
// cluster's two sites. Discovery seeds it from topology.kubernetes.io/zone, as it
// does failureDomain, since a sync cluster's two sites are its two zones. Required
// on a sync cluster (§9.5), rejected otherwise. It expands into
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

## 10. Coexistence with Async Replication

The method dispatch is additive and so is the node path. A class with no `method` parameter, or `method: async`, is the async path with every line of today's behavior, driven by RamenDR across clusters. A class with `method: sync` is the scheduling-driven path of this design, within one stretched cluster. Both can exist in one fleet and even on one driver binary, because the choice is per volume from the class the volume's `VolumeReplication` names and from the cluster's `sync_replication` flag.

Staging coexists the same way. The node plugin's site read, `VolumeReplication` patch, and promote gate (§6) run only when the volume is synchronous (§6.6). An async-replicated volume, or a volume with no replication at all, stages through the existing path untouched: the node plugin never patches its `VolumeReplication` (RamenDR owns that object's `replicationState`) and never gates its attach.

On a sync cluster the backend still accepts a volume created with an async replication policy (`sync-replication.md` §8), so a class pointing a sync cluster at the async routes is a misconfiguration this driver does not need to prevent: the direct async replication operations are refused by the backend with a `400` that the driver surfaces (§11).

---

## 11. Backend API Requirements

All routes are v2, token-authenticated, served on a cluster with `sync_replication: true`. Prefix `V` as in §7. All exist on the `sync-replication` branch (P0-1).

| Method | Endpoint                                 | Notes                                                                                                                                                                                                                               |
|--------|------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| POST   | `V/replication/failover?site=S&planned=` | Promote. `200` `SyncPromoteResultDTO`; `409` in progress or refused (retry); `412` the other site is not online and the call is not forced; `400` bad or missing site. Idempotent: a `200` for an already-served volume is a no-op. |
| POST   | `V/replication/demote?site=S`            | Demote. `204` (also a no-op when not served here); `409` gate refusal; `500` ANA RPC failed. Idempotent.                                                                                                                            |
| GET    | `V/replication/status[?site=S]`          | The async-shaped status DTO filled from the sync status. `site` optional. `200`; `400` on an invalid site.                                                                                                                          |
| GET    | `V/replication/sync-status[?site=S]`     | The native `SyncReplicationStatusDTO`. `site` optional. `200`; `400` on a non-sync cluster or an invalid site.                                                                                                                      |
| GET    | `V/connect[?host_nqn=]`                  | Node staging. **No `site`**: returns the entries of the site the LVS is led from (`lvs_active_site`), which the preceding promote has set to this node's site. `200`; `404` (plain text) when entries cannot be built.              |
| POST   | `V/replication/failback` (body `{}`)     | Resync no-op. `204`. Body required.                                                                                                                                                                                                 |

The group routes (prefix `G`) mirror these and are listed in §14.

**The gate reads two routes.** The node plugin's gate (§6.4) releases on the `VolumeReplication` status the controller writes from `V/replication/status`, and then attaches from the site-less `V/connect`. `connect` is only ever called after a `Completed` promote, so the led site it returns is this node's site, which is why it needs no site parameter.

**Error envelope.** Sync refusals use FastAPI's `detail` object, `{"detail": {"message": "...", ...}}`, with `problems` (gate refusals), `volumes` (volumes blocking a promote, or group members not found), or `task_id` (the promote task) where each applies (`sync-replication.md` §3). The driver classifies on the status code first (§12) and carries the `message` into the gRPC error and the staging error.

**Idempotency.** Promote and demote are safe to retry: a promote re-polls its task and a `200` for a served volume is a no-op, a demote of a not-served volume is a `204`. This is required, because the csi-addons sidecar retries every RPC and `NodeStageVolume` is re-invoked by the kubelet across the gate (§6.4).

**Site parameter.** The mutating routes (`failover`, `demote`) require `site`, which the controller reads from the `VolumeReplication`'s `simplyblock.io/sync-site` annotation (§5.2, §6.3). The status-read routes take `site` as optional; an absent site returns the cluster-wide status, which is what lets `GetVolumeReplicationInfo` (§5.3) call the status route without one. A present but invalid site is a `400`.

---

## 12. Failure Modes and Fallback

| Failure                                                              | Detection                                          | Behavior                                                                                                                                                                                                                                                               |
|----------------------------------------------------------------------|----------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Promote in progress                                                  | `409` with `task_id`, message "in progress"        | Retryable. The controller returns `codes.Unavailable` and re-polls; `NodeStageVolume` returns `codes.Unavailable` so the kubelet re-stages and re-checks.                                                                                                              |
| Planned gate refused (desynced store)                                | `409` with `problems`                              | Retryable, never forced. The controller returns `codes.FailedPrecondition` with the problems; staging holds with an event. No escalation.                                                                                                                              |
| Other site not online, promote not forced                            | `412`                                              | The single escalation trigger. The controller escalates to a forced promote (`planned=false`) automatically, so an AZ loss self-heals as Kubernetes reschedules the workload (§7.1). Safety rests on the backend fencing the lost site (Open Question 1, §16).         |
| Forced promote while the other site is live                          | `409`                                              | Retryable. Force never acts on a live site; the driver returns `codes.FailedPrecondition` and waits.                                                                                                                                                                   |
| No `VolumeReplication`, or its class is absent or not `method: sync` | node plugin, during correlation (§6.2, §6.6)       | The volume is treated as plain and staged ordinarily (§6.6). A sync volume the administrator never protected, or one whose class is misconfigured, is a configuration gap, not a driver error.                                                                         |
| Sync class, missing `site`                                           | driver, before any backend call                    | `codes.InvalidArgument`. A misconfigured class, not retryable usefully.                                                                                                                                                                                                |
| Sync class against a non-sync cluster                                | `400` from the backend                             | `codes.InvalidArgument` with the backend message. The backend returns `400` for both a bad/missing site and a non-sync cluster, so the driver does not distinguish them; it is never the `412` escalation. The deployment paired a sync class with an async cluster.   |
| Node plugin cannot reach the Kubernetes API                          | node plugin, reading or patching the VR            | Retryable staging error. Without the patch the switch cannot begin, so staging holds rather than attaching an un-promoted volume.                                                                                                                                      |
| Old node's `NodeUnstageVolume` cannot finish                         | kubelet, the move never leaves `ContainerCreating` | The single-writer attachment stays held on the old node, so the volume cannot attach on the new node and none of §6.2 runs. The move is blocked at the Kubernetes attach layer, below this design. The unstage teardown must complete promptly (Open Question 4, §16). |
| ANA RPC failed on demote                                             | `500`                                              | Retryable. Return `codes.Unavailable`.                                                                                                                                                                                                                                 |
| Backend unreachable                                                  | transport error                                    | Retryable. Return `codes.Unavailable`.                                                                                                                                                                                                                                 |

The protocol is that `409` is always retryable and never a reason to force, and `412` is the only answer that escalates to a forced promote. In the async design csi-addons owned the escalation; here, with no orchestrator, the controller owns it and escalates automatically on `412`. This is deliberate: an AZ loss reschedules the workload onto the surviving zone, and a `412` that held for a human would keep the pod in `ContainerCreating` for the whole outage, so the automatic force is what lets the two-site cluster survive a zone loss on its own. The controller still emits the `SyncPeerSiteOffline` event (§13) before it forces, so the escalation is visible rather than silent.

The split-brain risk an automatic force carries is a force against a peer that is partitioned rather than dead, and the defense against it is not the controller's. The backend decides a site is lost before it answers `412`, and the losing site must stop serving writes once it loses control-plane quorum, so a force on the survivor cannot race a still-writable peer. That fencing is the backend's, specified in `sync-replication.md`, and its soundness is Open Question 1 (§16); the controller trusting the backend's `412` is correct only to the degree that guarantee holds.

A store shared between protected volumes and volumes outside the set cannot be switched until every volume on it is demoted. A promote blocks with a `409` naming the still-served volumes (`volumes`), which holds staging with a clear, retryable event rather than a silent partial switchover; the resolution is operational (do not share a store across workloads that schedule to different sites).

---

## 13. Observability

The CSI driver emits no Kubernetes events today and exposes the async replication metrics the async design defines (`design-csi-addons-replication.md` §11). This section adds the sync-specific events and metrics the switch and the staging gate owe. Events land on the `VolumeReplication` (and, for Phase 2, the `VolumeGroupReplication`) object, because it is the object the switch acts on and a user inspects, and it outlives the promote it reports on. Staging-side events (the gate holding, a correlation failure) also surface on the pod through the ordinary `NodeStageVolume` error, so a held pod carries a message without a `VolumeReplication` lookup.

### Kubernetes Events

| Event                                                                    | Type    | Reason                   | On                                        |
|--------------------------------------------------------------------------|---------|--------------------------|-------------------------------------------|
| The node signaled its site onto the VolumeReplication                    | Normal  | `SyncSiteSignaled`       | VolumeReplication, VolumeGroupReplication |
| The promote is in progress and staging is holding on the gate            | Normal  | `SyncPromotePending`     | VolumeReplication, VolumeGroupReplication |
| The volume is served on this site and staging may proceed                | Normal  | `SyncPromoteSucceeded`   | VolumeReplication, VolumeGroupReplication |
| The planned gate refused the switchover because a store is not in sync   | Warning | `SyncGateRefused`        | VolumeReplication, VolumeGroupReplication |
| The other site is not online; an unforced promote cannot proceed         | Warning | `SyncPeerSiteOffline`    | VolumeReplication, VolumeGroupReplication |
| A sync class is missing its site, names a non-sync cluster, or is absent | Warning | `SyncClassMisconfigured` | VolumeReplication, VolumeGroupReplication |

`SyncGateRefused` and `SyncPeerSiteOffline` are the load-bearing ones: a switchover correctly holding for a desynced store and one holding for a lost peer are indistinguishable without them, and both look like a pod stuck in `ContainerCreating` to an operator watching only the workload.

### Prometheus Metrics

| Metric                                    | Labels                      | Description                                                                                     |
|-------------------------------------------|-----------------------------|-------------------------------------------------------------------------------------------------|
| `simplyblock_csi_sync_promote_total`      | `cluster`, `site`, `result` | Promote outcomes by result (`succeeded`, `gate_refused`, `peer_offline`, `escalated`, `error`). |
| `simplyblock_csi_sync_stage_gate_seconds` | `cluster`, `site`           | Histogram of wall-clock time a `NodeStageVolume` spent gated from the first patch to attach.    |
| `simplyblock_csi_sync_demote_total`       | `cluster`, `site`, `result` | Demote outcomes (`fenced`, `gate_refused`, `error`).                                            |
| `simplyblock_csi_sync_status_degraded`    | `cluster`, `site`           | Gauge, one while the cluster-wide sync status is not healthy.                                   |

`simplyblock_csi_sync_stage_gate_seconds` is the switchover-latency signal an operator compares against the application's reschedule-downtime budget, because the gate is the window a moving pod's volume is unavailable. `simplyblock_csi_sync_status_degraded` held at one is how a desync that blocks every switchover gets noticed before a pod needs to move rather than when one does.

---

## 14. VolumeGroupReplication

Phase 2 adds the consistency-group variant. The backend drives a group as one unit: one gate for every member, the store rule checked over the union of the members' stores, one promote task for all (`sync-replication.md` §3, group routes). The driver's group RPCs (`EnableVolumeGroupReplication`, `PromoteVolumeGroup`, `DemoteVolumeGroup`, and the group info read) dispatch by the same `method` and `site` read from the `VolumeGroupReplicationClass`, exactly as Phase 1 does per volume.

The scheduling trigger extends naturally. A pod that mounts several PVCs of one consistency group causes `NodeStageVolume` to run per member on the same node, so each member resolves to the same `VolumeGroupReplication`. The node plugin patches that one `VolumeGroupReplication` to the node's site's `VolumeGroupReplicationClass` (the first member's stage does it; the rest find it already pointed there), and every member's gate waits on the one group promote. Because the group promotes as a unit, a pod never comes up with some members led from one site and some from the other.

| Method | Endpoint                                 | Notes                                                                                                                                                         |
|--------|------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------|
| POST   | `G/replication/failover?site=S&planned=` | `200` `{"members": [SyncPromoteResultDTO, ...]}` once every member is served; otherwise `409` / `412` / `400` as the volume route.                            |
| POST   | `G/replication/demote?site=S`            | `204` (an empty group too); `409` gate refusal. Members are fenced in order, each recorded after its own fence, so a retry is a no-op for those already done. |
| GET    | `G/replication/status[?site=S]`          | `ConsistencyGroupReplicationStatusDTO` filled from the sync status plus `member_count`. `site` optional.                                                      |
| GET    | `G/replication/sync-status[?site=S]`     | `SyncReplicationStatusDTO`. `site` optional. `400` on a non-sync cluster.                                                                                     |
| POST   | `G/replication/failback` (body `{}`)     | `204` no-op.                                                                                                                                                  |

A group member that cannot be resolved to a live volume, or that belongs to another cluster, refuses the whole group request with `409` and the ids in `volumes`, before anything is fenced or queued. The driver surfaces that as a retryable `codes.FailedPrecondition` naming the members, and the staging of every member holds. The status is cluster-wide, so the group gets the same `state` and `lag` as any volume, plus `member_count`. The driver does not combine per-member status (§8).

Phase 2 is independent of Phase 1 only in the driver wiring. The dispatch, the site signaling, the gate, the status mapping, the `force` inversion, and the `409` / `412` protocol are all shared, so Phase 2 is the group RPC handlers and the group correlation calling the `G` routes with the Phase 1 machinery.

---

## 15. Testing Strategy

Full scenario matrix, coverage status, and hand-off test concepts: [`tests/test-plan-sync-replication-csi-addons.md`](../tests/test-plan-sync-replication-csi-addons.md). The matrix moves to the scheduling-driven model with this revision and needs the companion update (the Ramen-driven scenarios retire, the node-stage and gating scenarios are new).

- **Unit:** the method dispatch (absent, `async`, `sync`); the `force`-to-`planned` inversion; the site read from the node label; the volume-to-`VolumeReplication` correlation and the site-annotation patch, including the no-VR error and the site read from the annotation on promote and demote; the gate's three outcomes (promoted, in progress, refused) against a `VolumeReplication` status; and the status-code to gRPC-code mapping including the `409`-against-`412` distinction. These run against fake Kubernetes objects and a mock backend HTTP server with no cluster, because the node logic and the classification are pure.
- **Integration:** the full staging path against the csi-addons sidecar and a mock backend — a `NodeStageVolume` that reads a node's site, patches the `VolumeReplication`, waits out a promote poll (`409` in progress to `200`), and attaches only after `Completed`; a re-staged pod whose volume moves site; and a stage that holds on a `409` gate refusal and on a `412`. Also the operator provisioning (§9.5, §9.6): that `enableSyncReplication` and the node `site` reach the backend create and add bodies, and that admission enforces their immutability and the site-required rule. And the node plugin's new RBAC: that the `DaemonSet` service account can read the node, PV, PVC, and classes and patch the `VolumeReplication`, and no more (§4, `rbac-hardening`).
- **E2E:** a real two-site synchronous cluster. A pod scheduled to site A, then rescheduled to site B (a node drain or an affinity change), with its data verified on B after the move and the leadership confirmed to have followed. A disaster case: site A taken down, a pod that can now only schedule on B, and the promote that brings the volume up there (which proves the `412` path and whichever escalation policy §16 settles). The consistency-group case: one pod mounting several members, moved as a unit. E2E is the only class that proves the gate actually prevents a wrong-site attach and that the connection entries serve the volume, and it needs a two-site cluster the other suites do not.

The risk concentrates in two places: the gate's correctness (a released gate on a not-yet-promoted volume is a wrong-site attach, the one outcome the whole design exists to prevent) and the status-code protocol (a `409` miscoded as the escalation trigger forces a promote that should have waited, a `412` miscoded as a plain retry hangs a disaster promote). Those unit and integration scenarios are the ones that must not be cut.

Phase 2's group scenarios become testable only once the group RPC handlers and the group correlation land (§14).

---

## 16. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              | Owner           |
|-----|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------|
| 1   | **Lost-site fencing under the automatic force.** The controller escalates to a forced promote automatically on `412`, so an AZ loss self-heals as Kubernetes reschedules the workload (§7.1, §12). That is safe only if a partitioned-but-alive losing site cannot keep serving writes while the survivor is force-promoted. Confirm the backend guarantees this: the losing site self-fences on control-plane quorum loss, the backend's lost-site determination does not declare a merely slow peer lost, and the control plane itself survives a single-zone loss (which for two zones implies a witness or arbiter outside both). If any of these does not hold, the automatic force needs a guard the driver does not currently have.                                                                            | Backend team    |
| 2   | **Stale VR cache at NodeStage.** The node plugin decides sync-vs-plain per PVC from a `VolumeReplication` informer cache, failing closed until the cache has synced (§6.6). A synced cache that is briefly stale against a just-created sync `VolumeReplication` would resolve that volume to no VR and stage it as plain, with no gate, a wrong-site attach for the window of the staleness. Decide whether a cache-miss on a sync cluster does one live API confirm before treating the volume as plain (safe, a round-trip on every plain volume's stage) or trusts the cache (cheap, the exposure bounded by the informer's resync interval). The per-PVC stage-time lookup is chosen over a dedicated sync `StorageClass`, so sync and plain volumes may share a class; this is the residual that choice leaves. | Operator team   |
| 3   | **Reconcile trigger for the site annotation. (Resolved.)** A live run on 2026-10-15 confirmed an annotation-only change does *not* re-invoke `PromoteVolume`: the controller made zero `failover` calls on a move and `sync_active_site` never moved, while the gate released on the stale, site-agnostic `Primary`/`Completed` status and the data check passed because both sites are byte-identical. The node now forces the re-promote by transitioning `replicationState` `primary → secondary → primary` (`ensureSyncPromoted`), as RamenDR does, so the reconciler re-issues `DemoteVolume` then `PromoteVolume` (§6.3, §6.4).                                                                                                                                                                                 | Operator team   |
| 4   | **Unstage teardown must complete promptly.** A move depends on the old node's `NodeUnstageVolume` finishing so the single-writer volume detaches and can attach on the new node (§6.5, §12). A `NodeUnstageVolume` that hangs in the NVMe teardown, or fails to identify the volume's namespace once its stashed context is gone, holds the attachment and blocks the move below this design. The teardown's robustness (a bounded, forced NVMe disconnect, and a teardown that survives a missing stashed context) is a separate csi-driver work item, tracked so the follow-pod move is not left waiting on it.                                                                                                                                                                                                     | csi-driver team |
