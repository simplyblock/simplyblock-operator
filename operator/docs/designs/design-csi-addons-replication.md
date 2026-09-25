# Design Document: csi-addons Volume Replication

**Status:** Phase 3 Implemented (Phase 4, group replication: Planned)  
**Author:** Israel Geoffrey (geoffrey1330)  
**Date:** 2026-09-16 (last updated 2026-09-25)  
**Test Plan:** [`tests/test-plan-csi-addons-replication.md`](../tests/test-plan-csi-addons-replication.md)

---

## Phasing Overview

| Phase       | Status      | Scope                                                                                                                                                                                                                                                                                                                              | Sections     |
|-------------|-------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------|
| **Phase 1** | Implemented | The csi-addons machinery and the steady-state contract: CRDs, controller-manager, sidecar, the Replication and csi-addons Identity gRPC services with `EnableVolumeReplication`, `DisableVolumeReplication`, and `GetVolumeReplicationInfo`, backed by a typed backend status endpoint                                             | §4, §5.1, §6 |
| **Phase 2** | Implemented | The lifecycle verbs: `PromoteVolume` (planned and forced), `DemoteVolume`, and `ResyncVolume`. Validation end to end against a Ramen `VolumeReplicationGroup` in async mode is still outstanding (§12, E-06/E-07)                                                                                                                  | §5.2, §9     |
| **Phase 3** | Implemented | §11 (the Prometheus metrics). §7.2's peerClasses preflight is out of this design's scope entirely (it's Ramen's own `DRPolicy` mechanism) and is deferred to a future Ramen-integration design                                                                                                                                     | §7.1, §11    |
| **Phase 4** | Planned     | §14 (`VolumeGroupReplication`): the group-level csi-addons surface, driven by the stock controller-manager through a new driver VolumeGroup service and a group handle the existing Replication verbs act on, backed by new group-replication endpoints (P0-6/P0-7). This is the gap analysis's own Phase 2 group-replication item | §14          |

Phase 1 is independently useful: a `VolumeReplication` object per PVC whose status truthfully reports the relationship, which no surface provides today. Phase 2 makes the object drivable, which is what Ramen actually needs. Phase 3 makes the whole thing operable at fleet scale. Phase 4 lifts the same surface from one volume to a consistency group, replicated as one unit.

The phase numbers above are this document's own, not the DR storage foundation gap analysis's (§1): its Phase 0 (shipping the csi-addons contract itself) is this design's Phase 1, and its Phase 1 (promote, demote, and resync end to end through Ramen) is this design's Phase 2.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                                                                                                                                                                      | Kind                    | Blocks  | Status                                                                                                                                                                                |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| P0-1 | A typed, steady-state per-volume replication status read: `GET .../volumes/{id}/replication/status` serving what `lvol_controller.get_replication_info` computes today (state, lag, outstanding bytes, failure counters), available for the volume's whole replicated life                                        | Control plane (`sbcli`) | Phase 1 | Shipped: `GET .../volumes/{v}/replication/status` → `ReplicationStatusDTO` (`simplyblock_web/api/v2/cluster/storage_pool/volume/replication.py:58-69`)                                |
| P0-2 | Idempotent attach and detach: attaching a volume to the policy it already follows returns success, and detaching a non-attached volume returns success                                                                                                                                                            | Control plane (`sbcli`) | Phase 1 | Shipped: `replication_policy_controller.attach_policy`/`detach_policy` (`simplyblock_core/controllers/replication_policy_controller.py:208-262`)                                      |
| P0-3 | A standalone demote verb: `POST .../volumes/{id}/replication/demote` that converges the peer while still serving (repeated snapshot-and-ship until the remaining delta is small), then quiesces, ships the final delta, confirms it landed on the peer, and fences the data path                                  | Control plane (`sbcli`) | Phase 2 | Shipped: `POST .../volumes/{v}/replication/demote` → `lvol_controller.demote_lvol`                                                                                                    |
| P0-4 | An `rpo_target_seconds` field on `ReplicationPolicy`, so RPO compliance is computable against a declared target rather than the derived lag budget                                                                                                                                                                | Control plane (`sbcli`) | Phase 3 | Shipped: `ReplicationPolicy.rpo_target_seconds` (`simplyblock_core/models/replication.py:89`), wired through the API (`PolicyParams.rpo_target_seconds`) and CLI (`--rpo-target-sec`) |
| P0-5 | csi-addons upstream: the `VolumeReplication` and `VolumeReplicationClass` CRDs (`replication.storage.openshift.io/v1alpha1`), the kubernetes-csi-addons controller-manager image, and the csi-addons sidecar image                                                                                                | Ecosystem               | Phase 1 | Vendored in the chart at v0.15.0 behind `csiaddons.create` (all twelve upstream CRDs, since the stock manager starts a controller per kind); sidecar wiring is Phase 1                |
| P0-6 | Group-level replication in the control plane: a consistency group replicated as one unit, with attach/detach to a group replication policy, group failover, group demote, group failback, and a typed group status, using group snapshots (`bdev_lvol_snapshot_group`) as the recovery generations (§14.4, §14.5) | Control plane (`sbcli`) | Phase 4 | Not shipped. New group-replication engine and the `/consistency-groups/{id}/replication/*` endpoints                                                                                  |
| P0-7 | A group replication policy: cadence and target for a whole consistency group, the group twin of `ReplicationPolicy` (Open Question 7)                                                                                                                                                                             | Control plane (`sbcli`) | Phase 4 | Not shipped                                                                                                                                                                           |

Everything the per-volume adapter (Phases 1-3) needs already exists: the attach and detach calls, failover, the failback and commit pair, the relationship read, and the backlog arithmetic inside `get_replication_info`. That adapter is thin precisely because the engine is complete. Group replication (Phase 4) is the exception: P0-6/P0-7 are genuinely new backend work, because the engine replicates per volume today and a consistency group has no group-level replication of its own (§14.4).

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Architecture Overview](#3-architecture-overview)
4. [The csi-addons Machinery](#4-the-csi-addons-machinery)
5. [The Replication Service](#5-the-replication-service)
6. [Steady-State Status and Conditions](#6-steady-state-status-and-conditions)
7. [VolumeReplicationClass and peerClasses](#7-volumereplicationclass-and-peerclasses)
8. [Coexistence with the Legacy Replication Kinds](#8-coexistence-with-the-legacy-replication-kinds)
9. [Backend API Requirements](#9-backend-api-requirements)
10. [Failure Modes and Fallback](#10-failure-modes-and-fallback)
11. [Observability](#11-observability)
12. [Testing Strategy](#12-testing-strategy)
13. [Migration Strategy](#13-migration-strategy)
14. [VolumeGroupReplication](#14-volumegroupreplication)
15. [Open Questions](#15-open-questions)

---

## Overview

simplyblock's replication engine is complete and works: interval snapshots ship per volume under a `ReplicationPolicy`, failover clones the last replicated generation on the target, and the failback-plus-commit pair performs a fenced, lossless cutover back. What it is not is drivable by anything outside simplyblock. A Ramen `VolumeReplicationGroup` speaks exactly one per-volume replication dialect, the csi-addons `VolumeReplication` object, and simplyblock implements none of it.

This design exposes the existing engine behind that dialect. The CSI controller plugin gains the csi-addons Replication and Identity gRPC services, a csi-addons sidecar and controller-manager reconcile `VolumeReplication` objects into those RPCs, and each RPC is a thin adapter onto a backend call that already exists (or is named in Phase 0). No replication mechanism is rebuilt, and the engine keeps shipping snapshots exactly as it does today.

| Concern                        | Mechanism                                                               | Decided when                        |
|--------------------------------|-------------------------------------------------------------------------|-------------------------------------|
| Per-volume replication intent  | `VolumeReplication` (`spec.replicationState: primary\|secondary`)       | Reconciled continuously             |
| Cadence, retention, and target | `VolumeReplicationClass` parameters naming a `ReplicationPolicy`        | At class authoring                  |
| Promote, demote, and resync    | The Replication gRPC verbs, adapted onto failover, demote, and failback | On each reconcile until `Completed` |
| Relationship health            | `VolumeReplication.status.conditions` from the typed status read        | On every reconcile                  |
| RPO figures                    | Prometheus metrics from the control plane (§11)                         | Continuously                        |

A reader who stops here has the model: the engine is unchanged, the csi-addons surface is the adapter, and the volume handle (`{clusterID}:{poolID}:{volumeID}`) is the one identity that lets either cluster's driver address the same backend relationship.

---

## 1. Background

**The engine.** A volume replicates when `do_replicate` is set and a `ReplicationPolicy` names its cadence and target. The snapshot monitor takes interval snapshots, the shipping runner transfers each one to a landing volume on the target cluster and chains it there, and retention prunes behind the shipped frontier. Failover (`POST .../replication/failover`) clones the last fully replicated snapshot into a writable volume on the target with the same NVMe identity. Failback is a reversed replication seeded by `data_uuid` matching, and commit (`POST .../replication/commit`) runs the fenced final-step cutover through a task runner. All of this is driven today by the operator's `ReplicationPair`, `ReplicationPolicy`, `ReplicationSlot`, and `ReplicationOps` kinds, which build their HTTP calls inline against the same endpoints.

**The contract this design targets.** Ramen's dr-cluster operator reconciles one `VolumeReplication` per protected PVC. It flips `spec.replicationState` between `primary` and `secondary` and waits for the driver's conditions (`Completed`, `Degraded`, `Resyncing`) to report the operation done and the relationship healthy. It reads `status.lastSyncTime` for RPO. It never calls a vendor API. The interfaces are the csi-addons specification's Replication gRPC, served by the driver, and the kubernetes-csi-addons controller-manager, which turns `VolumeReplication` objects into those RPCs through a per-driver sidecar.

**The direction is already committed.** The CRD redesign excludes the four replication kinds from its model because "that subsystem is being redesigned against the CSI Addons specification, whose `VolumeReplication` and `VolumeGroupReplication` kinds already carry the per-volume and per-group replication contract that a backup tool or a DR orchestrator understands" (`crd-redesign/design-crd-model.md`, Non-Goals). This document is that redesign's replication chapter: the per-volume half in §5, and the per-group half (`VolumeGroupReplication`) in §14. The DR storage foundation gap analysis (Phase 0 and Appendix A of that document) is its requirements source.

**Three facts about today's surface shape the design.**

1. **The steady-state status has no home.** The typed relationship read (`GET .../replication/relationships/{lvol}` and its per-volume twin) serves an `LVolReplication` record that is only created at cutover or failover, so it returns 404 for a volume's entire healthy replicated life. The real steady-state verdict lives in `lvol_controller.get_replication_info`, an untyped dict reachable only as `rep_info` on the volume DTO. The operator's slot controller documents the consequence in its own comments: `status.lastReplicatedAt` is stamped once at attach and never refreshed while replication is healthy.
2. **There is no demote and no resync verb.** The backend surface is attach, detach, failover, failback, commit, and cutover-proceed. Demote exists only as an internal composition (ANA suspend plus fencing) inside the failover and cutover paths. Resync exists only as the failback direction reversal.
3. **The secondary has no volume object.** During steady-state replication the target cluster holds replicated snapshots and transient landing volumes, never a secondary lvol. A writable volume materializes on the target only at promotion. The `VolumeReplication` on the DR cluster therefore describes a relationship addressed by handle, not a local volume, and the driver's multi-cluster `secret.json` resolution is what makes that address work from either side.

---

## 2. Goals and Non-Goals

### Goals

- A `VolumeReplication` object per PVC, reconciled by the stock kubernetes-csi-addons controller-manager against this driver, drives simplyblock replication: enable, disable, promote (planned and forced), demote, and resync.
- The driver's conditions follow the csi-addons contract: `Completed` reports the last requested state change finished, `Degraded` reports relationship health, and `Resyncing` reports a divergence catch-up in flight. Steady-state healthy async is `Completed=True, Degraded=False, Resyncing=False`, and lag alone trips none of them.
- `status.lastSyncTime` is truthful for the volume's whole replicated life, sourced from a typed backend status read rather than the cutover-time relationship record.
- Every verb is idempotent, because Ramen re-drives every reconcile.
- The adapter reuses one shared control-plane client (`atlas-lib/controlplane`), ending the pattern where each consumer hand-rolls the same replication HTTP calls.
- A `StorageClass` and `VolumeSnapshotClass` naming convention across paired clusters that Ramen's peerClasses can express (§7.2 documents what Ramen's own contract requires; verifying it is a future Ramen-integration design's concern, not this one's).
- The RPO and backlog figures Ramen cannot carry (`bytesBehind`, throughput, RPO compliance) are exported as Prometheus metrics from the control plane.

### Non-Goals

- **Global `VolumeGroupReplication`.** The base group surface (one VRG, one vendor's PVCs, one `VolumeGroupReplication` on top of a consistency group) is specified in §14. RamenDR's newer multi-VRG "Global VGR" consensus, spanning a replication group across several applications, is out of scope (§14.8, Open Question 4). The group snapshot surface (`design-consistency-groups.md`) is untouched.
- **VolSync and the S3 backup path.** The recurrent-immutable-snapshots DR type is a separate phase of the gap analysis and does not pass through this adapter.
- **Synchronous replication.** The engine is asynchronous snapshot shipping, and nothing here changes that.
- **Ramen hub components.** DRPolicy, DRPC, and hub orchestration are consumers of this contract, not part of it.
- **Retiring the legacy replication kinds.** `ReplicationPair`, `ReplicationPolicy`, `ReplicationSlot`, and `ReplicationOps` keep working unchanged. §8 defines coexistence and §13 the consolidation direction. Removal is its own change once the adapter is proven.
- **A simplyblock DR-status CRD.** The observability surface here is metrics. A rollup CRD is future work in the gap analysis's Appendix B.

---

## 3. Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                          Kubernetes (each cluster)                           │
│                                                                              │
│  VolumeReplicationClass          VolumeReplication (one per protected PVC)   │
│  (names a ReplicationPolicy)     spec.replicationState: primary|secondary    │
│            │                                  │                              │
│            ▼                                  ▼                              │
│  ┌──────────────────────────────────────────────────────────────────┐        │
│  │  kubernetes-csi-addons controller-manager (chart-deployed):      │        │
│  │  reconciles VolumeReplication → Replication gRPC on the driver   │        │
│  └──────────────────────────────┬───────────────────────────────────┘        │
│                                 │ via CSIAddonsNode registration             │
│  ┌──────────────────────────────▼───────────────────────────────────┐        │
│  │  CSI controller StatefulSet (SimplyblockDriver reconciler):      │        │
│  │  … csi-provisioner, csi-snapshotter, …, csi-addons sidecar,      │        │
│  │  and the controller plugin, all on one socket. The plugin        │        │
│  │  serves CSI Identity/Controller/GroupController, plus            │        │
│  │  csi-addons Identity and Replication (this design)               │        │
│  └──────────────────────────────┬───────────────────────────────────┘        │
│                                                                              │
│  operator: PVCAnnotationWatcher skips csi-addons-managed volumes (§8);       │
│            ReplicationPair/Policy author the backend target and policy       │
└─────────────────────────────────┬────────────────────────────────────────────┘
                                  │ HTTP, resolved per volume handle
┌─────────────────────────────────▼────────────────────────────────────────────┐
│                        simplyblock control plane                             │
│  PUT    .../volumes/{v}                 {replication_policy_id} (attach,     │
│                                         detach)                              │
│  GET    .../volumes/{v}/replication/status        (P0-1, steady state)       │
│  POST   .../volumes/{v}/replication/failover      (promote: planned gate     │
│                                                   or forced)                 │
│  POST   .../volumes/{v}/replication/demote        (P0-3: converge,           │
│                                                   quiesce, flush, fence)     │
│  POST   .../volumes/{v}/replication/failback      (resync)                   │
│  GET    .../replication/relationships/{lvol}      (cutover records)          │
│  exports simplyblock_replication_* metrics: lag, backlog, RPO (§11)          │
└──────────────────────────────────────────────────────────────────────────────┘
```

**One relationship, addressed from either cluster.** The `VolumeReplication.spec.dataSource` resolves to a PV whose handle is `{clusterID}:{poolID}:{volumeID}`. The driver resolves the backend from the handle through `clusters.Client`, exactly as every other RPC does, so the DR cluster's driver can drive the same backend relationship as the source cluster's without either side holding special state. This is the same stateless addressing the node plugin already uses to redirect a staged volume after failover.

**The adapter holds no state.** csi-addons RPCs are stateless and idempotent by contract. Every answer the driver gives is derived on the spot from the backend status read and the relationship record. There is no driver-side cache, no persisted step, and no state machine. `PromoteVolumeResponse` and `DemoteVolumeResponse` carry no fields at all in `csi-addons/spec` v0.2.0 -- there is no response field to report partial progress in. A verb whose backend work outlives the call (a demote converging a busy peer) instead returns a retryable `ABORTED` error, and the vendored controller-manager's own reconcile requeue is the retry loop; only once the backend reports the target state reached does the call return success, which the controller then reflects as the CR's `Completed` condition.

**The operator's part is small and off the data path.** The kinds that author the backend state (`ReplicationPair` for the target, `ReplicationPolicy` for cadence and retention) keep working unchanged, and the classes name what they author. On top of them the operator teaches the `PVCAnnotationWatcher` the one-owner rule (§8) so the legacy annotation path and a `VolumeReplication` never fight over one volume. Everything imperative it used to own (`ReplicationOps`, the commit cutover, the cutover-proceed handshake) is off this contract and confined to the legacy path.

---

## 4. The csi-addons Machinery

### 4.1 What gets deployed

- **CRDs** (chart): `volumereplications.replication.storage.openshift.io`, `volumereplicationclasses.replication.storage.openshift.io`, and the kubernetes-csi-addons operational CRDs (`csiaddonsnodes.csiaddons.openshift.io`). Vendored at a pinned upstream version, the same way the `VolumeGroupSnapshot` CRDs are.
- **Controller-manager** (chart): the stock kubernetes-csi-addons manager. It discovers driver endpoints through `CSIAddonsNode` objects and reconciles `VolumeReplication` by calling the driver's Replication gRPC.
- **Sidecar** (operator, `SimplyblockDriver` reconciler): the csi-addons sidecar in the controller StatefulSet. It connects to the plugin's socket, probes the csi-addons Identity service for capabilities, publishes a `CSIAddonsNode`, and proxies the manager's gRPC to the plugin. Wiring follows the existing sidecar pattern: a seventh field on `spec.sidecarImages`, a pinned default in `sidecars.go`, an appended `controllerSidecar` entry in `workloads.go` (appended, because the per-container tweaks there are positional), and a new RBAC component whose group alias is `replication.storage.openshift.io` plus `csiaddons.openshift.io`.

### 4.2 What the plugin serves

The plugin registers two additional gRPC services on the existing socket, beside the CSI services, following the GroupController precedent in `csicommon.NonBlockingGRPCServer`:

- **csi-addons Identity:** `GetIdentity`, `GetCapabilities` (advertising `VOLUME_REPLICATION`), and `Probe`. This is a distinct service from CSI Identity, so it is a new small server type, not an extension of the existing one.
- **Replication:** the six verbs of §5, implemented on the controller `Server` through an embedded `replication.UnimplementedControllerServer` from `github.com/csi-addons/spec` (pinned at v0.2.0), mirroring how the GroupController embeds its unimplemented base. The same verbs accept a group handle for group replication (§14.4).
- **csi-addons VolumeGroup (GroupController)** (Phase 4, Planned): `CreateVolumeGroup`, `ModifyVolumeGroupMembership`, and `DeleteVolumeGroup`, mapping a set of volume handles to the backend consistency group and back (§14.3). csi-addons Identity advertises the capability so the stock controller-manager dials it.

`NonBlockingGRPCServer.Start` today takes exactly the three CSI servers. It gains a registration hook so the driver package can register additional services without `csicommon` importing csi-addons.

### 4.3 The shared client

The generated control-plane client already declares every replication endpoint, but it is `internal/` to atlas-lib. This design adds `atlas-lib/controlplane/replication.go`, a typed wrapper over those operations (attach, detach, status, failover, demote, failback, relationship, and commit for the legacy `ReplicationOps` path), following the shape `migrations.go` set for long-running backend operations. The driver adapter consumes it, and the operator's replication reconcilers move onto it opportunistically (§13), ending the three-copies-of-the-same-HTTP-call pattern the gap analysis lists as an inconsistency.

---

## 5. The Replication Service

`volume_id` on every request is the CSI volume handle. The driver parses it with the existing helper and resolves the cluster client from `secret.json`. Every verb is idempotent: repeating a completed operation returns success, and repeating an in-flight one returns the in-flight answer. Backend errors map to gRPC codes through the existing per-RPC classifier, extended with replication constructors.

### 5.1 Phase 1 verbs

| Verb                       | Backend mapping                                                  | Semantics                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
|----------------------------|------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `EnableVolumeReplication`  | `PUT .../volumes/{v}` body `{"replication_policy_id": <policy>}` | The policy comes from the `VolumeReplicationClass` parameters (§7). Attach is synchronous on the backend; already-attached to the same policy is success (P0-2). Attaching to a *different* policy is **not** refused: sbcli's `attach_policy` silently re-attaches onto the new policy rather than returning `FAILED_PRECONDITION`, and no endpoint yet exposes the policy id a volume is currently attached to for the driver to compare against before attaching (§15, Open Question 3). |
| `DisableVolumeReplication` | `PUT .../volumes/{v}` body `{"replication_policy_id": null}`     | Detach. A 409 (cutover in flight) maps to `ABORTED`, retryable. Not-attached is success (P0-2).                                                                                                                                                                                                                                                                                                                                                                                             |
| `GetVolumeReplicationInfo` | `GET .../volumes/{v}/replication/status` (P0-1)                  | Returns `lastSyncTime` (newest fully replicated snapshot's creation time). `csi-addons/spec` v0.2.0's `GetVolumeReplicationInfoResponse` carries no `lastSyncDuration` or `lastSyncBytes` field, so the status read's cycle-duration and shipped-size data has no response field to land in until a newer spec version adds one.                                                                                                                                                            |

### 5.2 Phase 2 verbs

| Verb                               | Backend mapping                                                                                                    | Semantics                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
|------------------------------------|--------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `PromoteVolume` with `force=true`  | `POST .../volumes/{v}/replication/failover` (already shipped, unchanged)                                           | Unplanned promote: clone the last fully replicated generation on the target, retire the (assumed dead) source. Idempotent by the backend's own NQN probe. Ignores demote state entirely -- its whole premise is that the peer may never have been reachable to demote.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `PromoteVolume` with `force=false` | `POST .../volumes/{v}/replication/failover?planned=true` (P0-3 adds the `planned` parameter to the existing route) | The same promote as the forced form, refused unless the peer already holds every acknowledged write, which is exactly the state a completed demote leaves behind: the source fenced and the final flush confirmed. Demote still converging surfaces as `ABORTED` (retryable); no demote ever requested surfaces as `FAILED_PRECONDITION` -- the split matters because the vendored csi-addons controller auto-escalates ANY `FAILED_PRECONDITION` from a force=false promote to force=true inline, with no wait-and-retry grace period of its own (§15, resolves the risk this design's original `FAILED_PRECONDITION`-only wording would have created).                                                                                                                                                                                                         |
| `DemoteVolume`                     | `POST .../volumes/{v}/replication/demote` (P0-3, new route)                                                        | Fence the source (ANA inaccessible) FIRST, then trigger and confirm one final internal snapshot. This is the lossless half of a planned swap: after demote, the peer's planned promote loses nothing. Synchronous and re-drivable, not queued: returns `ABORTED` while the snapshot is still converging, success only once confirmed. Deliberately does not reuse `replication/commit`'s live-cutover engine (shrink rounds, the synchronous hub transfer) -- that machinery exists to bound a freeze window against a *live* writer, and by the time Kubernetes calls `DemoteVolume` the workload has already unmounted, so there is no moving target to protect against, only the ordinary fence-before-snapshot discipline. It also never touches a target volume; that is `PromoteVolume`'s job, on a separate, later call, possibly on a different cluster. |
| `ResyncVolume`                     | `POST .../volumes/{v}/replication/failback`                                                                        | Reverse the shipping direction, seeded by `data_uuid` matching so a recovered source resyncs by delta rather than full copy. Response's `ready` field is false while lag exceeds the budget and true once caught up. Resync reconciles the diverged old primary from the current primary; it never merges, and never calls `commit` -- that would be a cutover, not a resync.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |

**Promote is one operation; planned and unplanned differ only in what precedes it.** Both forms clone the last fully replicated generation on the target and serve it under the preserved NVMe identity, exactly as failover does today. The planned form is lossless not because it runs different machinery but because a completed demote guarantees the last replicated generation contains every acknowledged write, and the planned gate refuses the promote until that holds. The forced form skips the gate and accepts the RPO loss, because its premise is that the source is gone. The engine's commit cutover (`replication/commit`, the `FN_REPLICATION_FINAL` runner with its shrink rounds and cutover-proceed handshake) is deliberately NOT part of this contract and remains behind only the legacy `ReplicationOps` migration path (§8, §13) -- its convergence job does NOT move into the demote verb as this design originally assumed; see the `DemoteVolume` row above for why that engine is the wrong shape for a post-unmount demote.

**The planned form's no-demote branch splits on the source's own health, not on `role`.** The vendored controller-manager has no "already primary" awareness of its own: `markVolumeAsPrimary` calls Promote unconditionally, every time a `VolumeReplication` first declares primary intent (`internal/controller/replication.storage/volumereplication_controller.go`), including day-one protection of a volume that has always lived here and has never failed over. `get_replication_info`'s `role` field cannot distinguish that case from a volume genuinely awaiting a planned re-promotion after a completed demote -- `demote_lvol` writes only `LVol.replication_demote_state`, a field `role`'s computation never reads, so a fenced, demoted volume still reports `role: source`, identically to one that was never touched at all. A role-based short-circuit before the backend call was tried and reverted for exactly this reason: it silently skipped the real `failover?planned=true` call a completed demote is waiting on, leaving the volume fenced while csi-addons reported `Completed=True`. The fix instead reads the source's *own storage node* status (`lvol_controller.replication_source_online`, mirroring the target-node health check `replicate_lvol_on_target_cluster` already makes for the destination side): when `planned=true` and no demote was ever requested, a genuinely online source has nothing to fail over and the call succeeds as the no-op it is, with no clone; a source that is not online falls through to `FAILED_PRECONDITION` exactly as before, letting the vendored controller's force-escalation run for a real disaster. This narrows, but does not close, the race a staleness-tolerant health field always carries: a source that died within the last health-check interval still reads online and is treated as a no-op for one reconcile, correcting itself once the node's status catches up and the controller retries. Test 6 of `regression_test/21/test_csi_addons_replication.sh` needs updating to match -- its `$FRESH_PVC_NAME` scenario now reaches Primary via this no-op path, not via force-escalation, since nothing in that scenario's setup makes the source anything but healthy.

---

## 6. Steady-State Status and Conditions

### 6.1 The typed status read (P0-1)

`GET .../volumes/{v}/replication/status` serves, as a typed DTO, what `get_replication_info` computes today:

```
role:                source | secondary | failed_over | none
state:               in_sync | replicating | lagging | degraded | error | not_replicating
last_replicated_at:  timestamp of the newest fully replicated snapshot
lag_seconds:         now - last_replicated_at
lag_budget_seconds:  derived budget (or the policy's rpo_target_seconds, P0-4)
outstanding_count:   snapshots queued but not yet shipped
outstanding_bytes:   sum of used_size over the outstanding snapshots
failing_count:       shipping tasks currently suspended on errors
max_retry_reached:   whether any task exhausted its retries
last_cycle_bytes:    used_size of the last shipped snapshot
last_cycle_seconds:  duration of the last shipping cycle
resyncing:           whether a failback catch-up is in flight
```

This endpoint exists for the volume's whole replicated life. It does not replace the relationship read: `GET .../replication/relationships/{lvol}` keeps its cutover-record semantics (including surviving source deletion, which the node redirect depends on), and the status read is the steady-state complement. The operator's slot controller can also poll it to keep `status.lastReplicatedAt` honest, independent of this design's adapter.

### 6.2 Condition mapping

Following the csi-addons contract: conditions describe relationship and operation health, never instantaneous RPO. Steady-state healthy async is `Completed=True, Degraded=False, Resyncing=False`, and lag alone trips none of them.

| Condition   | True when                                                              | simplyblock source                                                                                                                                  |
|-------------|------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------|
| `Completed` | The last requested state change (enable, promote, demote) has finished | Enable: attach returned. Promote (either form): the relationship reports the target active. Demote: the P0-3 verb confirmed the final flush landed. |
| `Degraded`  | The relationship is unhealthy or not progressing                       | `state` is `degraded` (suspended shipping tasks) or `error` (retries exhausted), or `lag_seconds` exceeds `lag_budget_seconds`.                     |
| `Resyncing` | A divergence catch-up is reconciling the secondary                     | The status read's `resyncing` flag: a failback direction reversal or a `FN_REPLICATION_FINAL` task in flight.                                       |

`status.state` mirrors `spec.replicationState` once `Completed=True`, from the status read's `role`.

---

## 7. VolumeReplicationClass and peerClasses

### 7.1 The class

A `VolumeReplicationClass` binds a `VolumeReplication` to a backend policy:

```yaml
apiVersion: replication.storage.openshift.io/v1alpha1
kind: VolumeReplicationClass
metadata:
  name: simplyblock-async-5m
spec:
  provisioner: csi.simplyblock.io
  parameters:
    replicationPolicy: dr-policy-5m
    schedulingInterval: 5m
```

`replicationPolicy` names the backend `ReplicationPolicy` (resolved per cluster by name), which owns cadence, retention, mode, and the replication target. `schedulingInterval` restates the policy's interval for Ramen's `DRPolicy` matching. No secrets parameter is needed: the driver's credentials come from `secret.json`, as for every other RPC. The chart ships no default class. Classes are the user's to author, matching the `VolumeGroupSnapshotClass` decision.

One class per (policy, cadence) is the authoring model: a `VolumeReplicationClass` names exactly one policy, and a volume needing a different cadence follows a different policy under a different class. Per-volume interval overrides are not provided.

### 7.2 peerClasses: Ramen's own mechanism, out of this design's scope

`peerClasses` is Ramen's `DRPolicy` computation, not this design's: Ramen's hub-side controller pairs each managed cluster's `StorageClass`/`VolumeReplicationClass` objects itself, through the OCM hub-spoke visibility it already has, and a `DRPolicy` that cannot find a valid pairing already reports that failure on its own. Building any verification of that pairing into this operator -- a preflight, an admission check, or otherwise -- belongs to the future design that actually wires Ramen/OCM into this operator, not here: this design's own scope stops at §7.1, authoring one `VolumeReplicationClass` per policy, which is sufficient for the Phase 1/2 adapter to work whether or not Ramen, OCM, or peerClasses are ever in the picture.

What Ramen's contract requires of a pairing, for whoever writes that future design:

**Contract (Ramen requires this):**

- **The `StorageClass` name exists on both clusters.** A failover restores the protected PVC with its original `spec.storageClassName`, a by-name reference, so an equivalent class of that exact name must exist on the peer. The peerClasses computation also joins classes across the two clusters by `StorageClass` name.
- **The Ramen identity labels.** Each cluster's `StorageClass` carries `ramendr.openshift.io/storageid` (differing per cluster, since the backends differ), and the two `VolumeReplicationClass` objects representing one relationship carry an equal `ramendr.openshift.io/replicationid`. The `VolumeReplicationClass` is selected per cluster by `spec.replicationClassSelector` labels plus `provisioner` and a `schedulingInterval` equal to the `DRPolicy`'s, never by name.

**Convention (this design's own authoring choice, independent of Ramen):**

- **Same `StorageClass` parameters on both clusters** apart from `cluster_id` (necessarily) and pool when pools differ. The replication target's pool mapping already handles the pool difference at shipping time.
- **Same `VolumeSnapshotClass` and `VolumeReplicationClass` names on both clusters**, each side's `replicationPolicy` naming that cluster's policy toward its peer. Ramen does not require the names to match, but one name per relationship is what keeps a fleet legible.

---

## 8. Coexistence with the Legacy Replication Kinds

Two control paths now reach the same backend relationship: the annotation-driven slot family, and `VolumeReplication`. They must not fight.

- **One owner per volume.** A volume is managed either by the annotation path (a `ReplicationSlot` exists for its PVC) or by a `VolumeReplication`, never both. The adapter's `EnableVolumeReplication` refuses (`FAILED_PRECONDITION`) whenever a slot manages the volume, even when the slot's policy and the class's agree: ownership is the conflict, not the policy value, because two owners turn every ownership flap into a detach and re-attach, and a re-attach is a full re-sync on the backend. The `PVCAnnotationWatcher` is taught to skip PVCs that have a `VolumeReplication` (one informer lookup), so the annotation cannot re-attach behind the adapter's back. Moving a volume between the two paths is deliberate: remove the annotation (the slot detaches), then create the `VolumeReplication`.
- **`ReplicationOps` stays imperative and internal.** Bulk failover of a whole policy or target remains a `ReplicationOps` concern until `VolumeGroupReplication` lands. Nothing in this design calls it, and it does not touch csi-addons-managed volumes because the mutual-exclusion rule above keeps the sets disjoint.
- **The slot's status problem is fixed as a side effect.** The typed status read (P0-1) gives the slot controller a truthful `lastReplicatedAt` source, whether or not the adapter is in use.

The consolidation direction (§13) is that the annotation path becomes a compatibility layer and the four kinds retire once Ramen-driven replication is proven, exactly as the CRD redesign anticipates.

---

## 9. Backend API Requirements

Every endpoint is scoped as today: volume-scoped under `/api/v2/clusters/{c}/storage-pools/{p}/volumes/{v}`, cluster-scoped under `/api/v2/clusters/{c}/replication`.

| Method | Endpoint                                                | Notes                                                                                                                                                                     |
|--------|---------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `PUT`  | `.../volumes/{v}` (`replication_policy_id`)             | Existing attach and detach. P0-2 makes both idempotent: same-policy attach and non-attached detach return success.                                                        |
| `GET`  | `.../volumes/{v}/replication/status`                    | **New (P0-1).** The typed steady-state status of §6.1. Never 404s for a volume that exists; `state: not_replicating, role: none` is a valid answer.                       |
| `POST` | `.../volumes/{v}/replication/failover` (+ planned gate) | Existing; the one promote, both forms. Gains a planned form that is refused unless the peer holds every acknowledged write (a completed demote). Idempotent by NQN probe. |
| `POST` | `.../volumes/{v}/replication/demote`                    | **New (P0-3).** Quiesce, final ship, confirm on peer, fence. Idempotent: demoting a demoted volume returns success.                                                       |
| `POST` | `.../volumes/{v}/replication/failback`                  | Existing. Resync (direction reversal, delta-seeded).                                                                                                                      |
| `POST` | `.../volumes/{v}/replication/commit`                    | Existing, unchanged, and NOT part of this contract: it stays behind the legacy `ReplicationOps` migration path only (§13).                                                |
| `GET`  | `.../replication/relationships/{lvol}`                  | Existing, unchanged. Cutover records only; the node redirect depends on its survive-deletion semantics.                                                                   |
| `POST` | `.../volumes/{v}/replication/cutover-proceed`           | Existing, unchanged, legacy path only: the adapter never reaches it, because the commit cutover is off this contract.                                                     |

The unused backend verbs the operator never calls (`start`, `stop`, `trigger`, `tasks`) are unaffected, and `start` and `stop` remain the policy-less legacy path.

---

## 10. Failure Modes and Fallback

| Failure                                                                  | Detection                                               | Behavior                                                                                                                                                                 |
|--------------------------------------------------------------------------|---------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Enable against a volume attached to a different policy                   | Adapter compares the status read's policy               | `FAILED_PRECONDITION`; the message names both policies. Never silently re-attaches, because that is a full re-sync.                                                      |
| Disable during a cutover                                                 | Backend 409                                             | `ABORTED`, retryable. Ramen re-drives; the detach succeeds after the cutover settles.                                                                                    |
| Planned promote without a completed demote (source live or peer lagging) | The planned gate on the failover endpoint refuses       | `FAILED_PRECONDITION` naming the un-flushed tail. The caller demotes (or resyncs) first, and `Resyncing` reports progress.                                               |
| Forced promote when the source is alive                                  | Backend fences the source data path as part of failover | Split-brain is prevented structurally: the source is retired (ANA inaccessible, subsystem removed) before the target serves.                                             |
| Backend unreachable                                                      | HTTP error from the shared client                       | The RPC returns `UNAVAILABLE`; the controller-manager retries. Conditions keep their last-observed values, so a blip does not flap `Degraded`.                           |
| Demote cannot complete its flush (peer slow or unreachable)              | The demote's confirm step times out                     | `Completed` stays `False`, and the volume is not left fenced without a decision: whether the backend rolls back to serving primary or holds quiesced is Open Question 1. |
| Status read missing (pre-P0-1 backend)                                   | 404 from the status endpoint                            | The csi-addons capability is not advertised, so the controller-manager never drives this driver. The adapter ships dark until the backend is current.                    |
| Both an annotation slot and a VolumeReplication claim a volume           | Adapter check plus watcher skip (§8)                    | The first owner wins; the second surfaces `FAILED_PRECONDITION` (adapter) or a skip event (watcher). Never two writers to one relationship.                              |

---

## 11. Observability

**Baseline.** The replication engine's telemetry today is log lines (the `XFER-TIMING` phases) and the `rep_info` dict. Nothing is exported as metrics, and the `VolumeReplication` surface adds only `lastSyncTime`. The figures an operator actually watches live here.

### Kubernetes Events

The kubernetes-csi-addons controller-manager owns events on `VolumeReplication` (promote, demote, and resync outcomes), and this design adds none there. This design defines no events of its own on `ReplicationPair` either: the peerClasses preflight that would have emitted them is Ramen's own concern, out of scope here (§7.2).

### Prometheus Metrics (Implemented)

Exported by the control plane's existing v2 `Collector`-pattern exporter (`simplyblock_web/api/v2/metrics.py`), rebuilt from FDB on every scrape like every other series in that file. Labeled `lvol`/`lvol_name`/`pvc_name`/`pool`/`pool_name` (not the bare `volume` this section originally specified: the exporter's existing lvol-scoped metrics already use `lvol`/`lvol_name`, and joining the new series against them needs a shared label name) plus `policy`/`policy_name`/`peer_cluster`:

| Metric                                      | Description                                                                     |
|---------------------------------------------|---------------------------------------------------------------------------------|
| `simplyblock_replication_lag_seconds`       | Now minus the newest fully replicated snapshot's creation time.                 |
| `simplyblock_replication_backlog_bytes`     | `outstanding_bytes`: the queued-but-unshipped snapshot sizes.                   |
| `simplyblock_replication_last_sync_seconds` | Duration of the last shipping cycle.                                            |
| `simplyblock_replication_last_sync_bytes`   | Size of the last shipped snapshot.                                              |
| `simplyblock_replication_rpo_violation`     | 1 while `lag_seconds` exceeds the policy's `rpo_target_seconds` (P0-4), else 0. |
| `simplyblock_replication_degraded`          | 1 while the status read's state is `degraded` or `error`.                       |

`simplyblock_replication_rpo_violation` is the alert: it is the declared objective against the measured lag, which no timestamp alone can express. `simplyblock_replication_backlog_bytes` is the second load-bearing figure, because it is the input to any honest RTO estimate and the number that distinguishes "slow cycle" from "falling behind." One caveat is recorded rather than hidden: `outstanding_bytes` measures queued snapshot sizes, not dirty bytes written since the last snapshot, so intra-interval writes are invisible to it. A true dirty-delta figure needs storage-plane support and is future work.

Values are computed by `lvol_controller.get_replication_info_bulk`, a bulk-friendly sibling of `get_replication_info` (P0-1) that reads a whole cluster's job tasks, policies, and targets once rather than once per replicating volume -- `get_replication_info` itself makes several effectively global FDB scans internally (repeated per call), which is fine for a single-volume status read but not for a per-scrape loop across a fleet. `simplyblock_replication_last_sync_seconds`/`_bytes` are omitted per volume when no cycle has completed yet, and `rpo_violation` is omitted entirely for a volume whose policy declares no `rpo_target_seconds`, matching the exporter's existing "omit rather than fabricate" convention (`_health_family`) -- a 0 there would misread as "in compliance" absent a declared target.

---

## 12. Testing Strategy

Full scenario matrix and coverage status: [`tests/test-plan-csi-addons-replication.md`](../tests/test-plan-csi-addons-replication.md)

- **Unit (driver):** each verb against a mock control plane: the idempotency table (repeat enable, repeat disable, repeat promote), the refusal paths (different-policy enable, lagging planned promote, disable during cutover), the condition derivation from every status-read state, and handle parsing failures.
- **Unit (operator):** the `PVCAnnotationWatcher` skip when a `VolumeReplication` exists.
- **Unit (driver, §14):** the VolumeGroup service verbs against a mock control plane (`CreateVolumeGroup` resolves the label-formed group idempotently; `ModifyVolumeGroupMembership` refused off-placement; `DeleteVolumeGroup` leaves members), and the group-handle branch of each Replication verb routing to the group endpoints.
- **Unit (backend, §14.5):** the group-replication engine in `sbcli` (group failover clones the last group generation for every member atomically; group demote quiesces all then ships one final group snapshot; group failback reverses direction), tests-first.
- **Integration:** the csi-addons sidecar and controller-manager against the driver with a mock backend under envtest or kind: a `VolumeReplication` flipped `primary` to `secondary` and back walks the verbs in order and lands the conditions.
- **E2E (two live clusters):** the Ramen-shaped lifecycle without Ramen: enable on the source, write data, and verify `lastSyncTime` advances; forced promote on the DR side, verifying the clone serves with the source fenced; and resync back with a planned swap (demote then promote), verifying zero loss with a hashed writer. Then the same driven by an actual Ramen VRG in async mode, which is Phase 2's acceptance gate.

The risk concentrates in the demote verb's flush-confirmation (the lossless-swap guarantee), in idempotency under Ramen's aggressive re-drive, and in the coexistence rules of §8. Those must not be cut.

---

## 13. Migration Strategy

Three replication control surfaces exist today: the operator's kinds, the stale CSI-shipped CRDs (`replications.simplyblock.com`, invalid as written, plus the orphaned `SnapshotReplication`), and the backend-driven node redirect. The target is one: csi-addons, with the node redirect unchanged beneath it.

1. **Phase 1 and 2 (this design):** the adapter ships alongside the legacy kinds. The mutual-exclusion rule (§8) keeps the two paths disjoint per volume. The shared `atlas-lib/controlplane/replication.go` client lands, and the driver uses it from day one.
2. **Opportunistic:** the operator's replication reconcilers move their inline HTTP calls onto the shared client, and the slot controller sources `lastReplicatedAt` from the typed status read. No behavior change, one client.
3. **After Ramen validation:** the annotation path is declared a compatibility layer, and new volumes are protected through `VolumeReplication`. The stale CSI-shipped CRDs are deleted (they were never installable), and `SnapshotReplication` is retired from the charts.
4. **The redesign's replication chapter:** whether `ReplicationPair` and `ReplicationPolicy` survive as the backend-policy authoring surface (classes need policies to name) or are re-cut is decided there, not here. This design only requires that a policy exists per cluster pair, however it is authored.
5. **Removing the offloaded group reconciler.** An earlier revision implemented `VolumeGroupReplication` as an operator-owned, `external: true` path: `VolumeGroupReplicationReconciler` (`operator/internal/controller/volumegroupreplication_controller.go`) and `VolumeGroupReplicationValidator` (`operator/internal/webhook/volumegroupreplication_validator.go`), which fanned a group out to per-member `VolumeReplication` objects. §14 supersedes that with the stock, `external: false` driver path, so both files, their unit tests, the `main.go` wiring, and the webhook registration are removed. Group protection then rides the driver's VolumeGroup service and the group-replication endpoints (P0-6/P0-7), with no operator code on the path.

---

## 14. VolumeGroupReplication

`VolumeGroupReplication` protects a consistency group as one replicated unit. It is the group-level sibling of the `VolumeReplication` surface (§5), in the same csi-addons `replication.storage.openshift.io` API group, and it is driven entirely by the stock kubernetes-csi-addons machinery: the generic controller-manager forms a backend volume group through the driver's csi-addons **VolumeGroup** service, then replicates that group as a single unit through the Replication service (§5), addressing it by one group handle. No operator reconciler and no vendor-specific controller take part. This is the gap analysis's Phase 2 group-replication item, and it is **Planned** (§13 records the earlier attempt this supersedes).

Ramen's `VolumeReplicationGroup` creates one `VolumeGroupReplication` when a protected application's PVCs share a `StorageClass` carrying `ramendr.openshift.io/groupreplicationid` (the group-level sibling of the per-volume `replicationid` of §7.1). The end-to-end Ramen validation is `design-ramen-integration.md` §6, whose test plan carries its E2E scenario as M-05.

### 14.1 Why the driver, not the operator

csi-addons's `VolumeGroupReplication` controller (v0.15.0) branches on `spec.external`:

- **`external: false` (this design's path).** The stock controller creates a `VolumeGroupReplicationContent`, calls the driver's VolumeGroup service to group the member volumes and obtain a group handle, then creates **one** `VolumeReplication` addressed by that group handle, which the ordinary `VolumeReplication` controller drives through the driver's Replication service (§5). The whole group is one replicated object, exactly as a single volume is.
- **`external: true`.** The stock controller skips the object entirely, handing it to a vendor controller.

This design takes the `external: false` path: the group is a first-class backend object the driver creates, and the standard machinery replicates it. The alternative, an operator `VolumeGroupReplicationReconciler` owning `external: true` objects and fanning them out to per-member `VolumeReplication` objects, is not used. It re-implements in the operator what the csi-addons controller already does, and it drives the group through per-member calls rather than one group operation, losing the crash-consistent, all-at-one-point failover a consistency group exists to give. An earlier revision built that reconciler and its admission webhook; §13 removes them.

### 14.2 Ramen configuration

Ramen sets `spec.external` from whether the member PVCs' `StorageClass` carries `ramendr.openshift.io/offloaded`. For this driver-driven design the `StorageClass` carries `groupreplicationid` but **not** `offloaded`, so Ramen creates the object with `external: false` and a real `volumeReplicationClassName`, and the stock controller reconciles it.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: simplyblock-group-sc
  labels:
    ramendr.openshift.io/storageid: <per-cluster>       # differs per cluster
    ramendr.openshift.io/replicationid: <shared>        # names the per-volume relationship
    ramendr.openshift.io/groupreplicationid: <shared>   # triggers group replication; no `offloaded` label
provisioner: csi.simplyblock.io
parameters: { cluster_id: <uuid>, pool_name: <pool> }   # plus the usual StorageClass parameters
---
apiVersion: replication.storage.openshift.io/v1alpha1
kind: VolumeGroupReplicationClass
metadata:
  name: simplyblock-group-async-5m
  labels:
    ramendr.openshift.io/groupreplicationid: <shared>   # must equal the StorageClass's
    ramendr.openshift.io/storageid: <per-cluster>       # must equal the StorageClass's
spec:
  provisioner: csi.simplyblock.io
  parameters:
    schedulingInterval: "5m"                            # must equal the DRPolicy's
```

Member PVCs carry `storage.simplyblock.io/consistency-group` (backend grouping and group snapshots, `design-consistency-groups.md`) and the `pvcSelector` label the VRG matches. Ramen additionally reads its own `ramendr.openshift.io/consistency-group` label to build the group's selector, so a Ramen-protected member carries both consistency-group labels with the same value.

### 14.3 The VolumeGroup service (driver)

The plugin serves the csi-addons **VolumeGroup** (GroupController) service beside csi-addons Identity and Replication (§4.2), advertising the capability through csi-addons Identity so the stock controller-manager dials it. Its verbs map onto the backend consistency group that already exists as a first-class object (`design-consistency-groups.md`):

| Verb                          | Backend mapping                                                                                            | Semantics                                                                                                                                                                              |
|-------------------------------|------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `CreateVolumeGroup`           | Resolve (or ensure) the consistency group over the given volume handles; return its id as the group handle | Idempotent over the group the `storage.simplyblock.io/consistency-group` label already formed at provisioning: the members are co-placed, so the call returns the existing group's id. |
| `ModifyVolumeGroupMembership` | Add or remove members                                                                                      | Refused when placement forbids it (a volume not co-placed on the group's node/LVS); dynamic membership is `design-consistency-groups.md`'s own Phase 4.                                |
| `DeleteVolumeGroup`           | Dissolve the group                                                                                         | The member volumes survive; only the grouping is removed. Idempotent.                                                                                                                  |

The label remains the single source of truth for membership (`design-consistency-groups.md`): `CreateVolumeGroup` does not introduce a competing grouping, it addresses the same backend group the label formed. One backend consistency group is therefore reachable three ways: by label at provisioning, by label for `VolumeGroupSnapshot`, and by handle for group replication.

### 14.4 Group-wide replication (driver and backend)

Once grouped, csi-addons addresses the whole group by its handle through the **same** Replication verbs of §5. Each verb operates on the group as one unit and maps to a new backend group-replication endpoint (§9, and P0-6/P0-7):

| Verb (on the group handle)      | Backend group operation                                                                                                   |
|---------------------------------|---------------------------------------------------------------------------------------------------------------------------|
| `EnableVolumeReplication`       | Attach the consistency group to a group replication policy (cadence and target for the whole group).                      |
| `PromoteVolume` (force/planned) | Group failover: clone the last fully replicated group-snapshot generation for **every** member on the target, atomically. |
| `DemoteVolume`                  | Group demote: quiesce every member, take and ship one final group snapshot, confirm it landed, then fence every member.   |
| `ResyncVolume`                  | Group failback: reverse the shipping direction for the whole group.                                                       |
| `GetVolumeReplicationInfo`      | Group `lastSyncTime`: the creation time of the newest fully replicated group-snapshot generation.                         |
| `DisableVolumeReplication`      | Detach the group from its group replication policy.                                                                       |

The recovery generation is a **group snapshot** (`bdev_lvol_snapshot_group`, `design-consistency-groups.md` P0-1): one frozen snapshot of every member at a single point, shipped atomically, so a group failover always lands every member at one crash-consistent point. Group promote, demote, and failback are all-or-nothing across members (Open Question 6). The driver detects a group handle (versus a per-volume handle) and routes to these endpoints; a per-volume handle still takes the §5 path unchanged.

### 14.5 Backend API

The group-replication endpoints, cluster-scoped like the per-volume ones under `/api/v2/clusters/{c}`, are new backend work (P0-6, P0-7):

| Method | Endpoint                                                | Notes                                                                                                 |
|--------|---------------------------------------------------------|-------------------------------------------------------------------------------------------------------|
| `PUT`  | `.../consistency-groups/{id}` (`replication_policy_id`) | Attach and detach the group to a group replication policy. Idempotent (P0-2's group twin).            |
| `POST` | `.../consistency-groups/{id}/replication/failover`      | Group promote, planned and forced. Clones the last group generation for every member atomically.      |
| `POST` | `.../consistency-groups/{id}/replication/demote`        | Group demote: quiesce all, ship one final group snapshot, confirm, fence all.                         |
| `POST` | `.../consistency-groups/{id}/replication/failback`      | Group resync (direction reversal).                                                                    |
| `GET`  | `.../consistency-groups/{id}/replication/status`        | Typed group status: role, group `lastSyncTime`, lag, per-member health rollup.                        |
| `GET`  | `.../consistency-groups/` (by name), `/{id}/members`    | Existing reads (`design-consistency-groups.md`); the VolumeGroup service resolves the handle by them. |

### 14.6 Observability

The stock kubernetes-csi-addons controller-manager owns Kubernetes events on `VolumeGroupReplication` (the group promote, demote, and resync outcomes), the same way it owns them for `VolumeReplication`; this design adds none there. The per-member replication metrics of §11 cover each group member; a group rollup (`simplyblock_replication_group_lag_seconds`, keyed by `consistency_group`) is derivable from the group status read of §14.5 and is exported by the same v2 exporter, its worst-member lag being the figure a group RPO alert watches.

### 14.7 Scope

The base case is one VRG, one storage vendor's PVCs, one `VolumeGroupReplication`. RamenDR's newer multi-VRG "Global VGR" consensus, spanning a replication group across several applications' VRGs, is out of scope (Open Question 4). `VolumeGroupReplicationContent` is created and owned by the stock controller-manager on the `external: false` path (it carries the group handle `CreateVolumeGroup` returns); this design writes nothing to it directly.

---

## 15. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   | Owner                   |
|-----|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|
| 1   | **Demote semantics for the application.** The P0-3 demote fences the volume (ANA inaccessible) after the final flush, and with convergence folded into the verb it is now the only place a planned swap can stall. This is also the one verb the planned promote's lossless guarantee entirely depends on (§5.2): a planned promote is refused unless a completed demote already fenced the source and confirmed the final delta landed, so an unresolved failure mode here is an unresolved gap in the whole "zero loss" claim. Ramen relocation unmounts the workload first, so the fence is ordinarily unopposed, but that is Ramen's choreography, not a guarantee the driver can rely on: a stuck termination, a stale mount that never released, or a demote invoked outside Ramen's normal flow can all leave writes still arriving when quiesce fires. Confirm the verb's behavior when writes are still in flight at quiesce (block versus fail), whether the converge phase has its own budget separate from the quiesced flush, and whether a timeout in either phase must abort back to serving primary or leave the volume fenced with no automatic recovery. | Backend team            |
| 2   | **Per-volume policy granularity.** A `VolumeReplicationClass` names one policy, and today one policy implies one target and cadence for all its volumes. Confirm one class per (policy, cadence) is an acceptable authoring model for Ramen's `replicationClassSelector`, or whether per-volume interval overrides are needed.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             | Operator / Backend team |
| 3   | ~~**Avoiding the clone on day-one protection.**~~ **Resolved:** `POST .../replication/failover?planned=true`'s no-demote branch now checks `lvol_controller.replication_source_online` (the source's own storage-node status) before falling through to `FAILED_PRECONDITION` -- an online source is a no-op (§5.2), so a healthy volume's first-ever `PromoteVolume` no longer materializes a clone. The remaining residual: a source that dies within the last health-check interval still briefly reads online, so one reconcile can treat a genuine disaster as a no-op before the node's status catches up and the controller retries -- bounded by the health-check detection window, not open-ended.                                                                                                                                                                                                                                                                                                                                                                                                                                                                | Backend team            |
| 4   | **Is Global VGR needed (§14.7).** §14 covers the base case: one VRG, one vendor's PVCs, one group. Confirm whether any planned simplyblock deployment spans a replication group across more than one application's VRG before treating RamenDR's multi-VRG "Global VGR" consensus as work this design should also specify.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | Operator team           |
| 5   | **`VolumeGroupReplicationContent` on the `external: false` path (§14.7).** The stock controller-manager creates and owns the `Content`, populating it with the group handle `CreateVolumeGroup` returns. Confirm the driver need only return a stable handle and never reads or writes the `Content` itself, across the csi-addons versions in scope.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | Operator team           |
| 6   | **Atomicity of group failover, demote, and failback (§14.4).** The design states all-or-nothing across members. Confirm the backend can guarantee it, and define the behavior when one member cannot complete: abort the whole group, or serve a partial group and report it.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              | Backend team            |
| 7   | **The group replication policy (§14.4, P0-7).** `design-consistency-groups.md` removed the replication policy from the consistency group; group replication needs a cadence and target back. Confirm a group replication policy attached to the CG (the group twin of `ReplicationPolicy`) is the model, versus per-member policies coordinated at the group level.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | Backend team            |
| 8   | **Grouping already-provisioned, non-co-placed volumes (§14.3).** `CreateVolumeGroup` is idempotent over the label-formed, co-placed group. Confirm whether `ModifyVolumeGroupMembership` must support adding a volume that is not already co-placed (a data move), or whether that stays refused as `design-consistency-groups.md`'s Phase 4 dynamic-membership work.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | Backend team            |
