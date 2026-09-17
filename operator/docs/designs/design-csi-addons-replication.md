# Design Document: csi-addons Volume Replication

**Status:** Phase 1 Implemented  
**Author:** Israel Geoffrey (geoffrey1330)  
**Date:** 2026-09-16 (last updated 2026-09-17)  
**Test Plan:** [`tests/test-plan-csi-addons-replication.md`](../tests/test-plan-csi-addons-replication.md)

---

## Phasing Overview

| Phase       | Status      | Scope                                                                                                                                                                                                                                                                                  | Sections     |
|-------------|-------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------|
| **Phase 1** | Implemented | The csi-addons machinery and the steady-state contract: CRDs, controller-manager, sidecar, the Replication and csi-addons Identity gRPC services with `EnableVolumeReplication`, `DisableVolumeReplication`, and `GetVolumeReplicationInfo`, backed by a typed backend status endpoint | §4, §5.1, §6 |
| **Phase 2** | Planned     | The lifecycle verbs: `PromoteVolume` (planned and forced), `DemoteVolume`, and `ResyncVolume`, validated end to end against a Ramen `VolumeReplicationGroup` in async mode                                                                                                             | §5.2, §9     |
| **Phase 3** | Planned     | peerClasses convention and preflight, and the replication observability surface (lag, backlog, RPO compliance)                                                                                                                                                                         | §7, §11      |
| **Phase 4** | Planned     | Test failover: the latest-replicated-snapshot read, the `drtest-*` conventions, and the two drill modes (bubble and test cluster) composed from clone, replication, and the real failover                                                                                              | §14          |

Phase 1 is independently useful: a `VolumeReplication` object per PVC whose status truthfully reports the relationship, which no surface provides today. Phase 2 makes the object drivable, which is what Ramen actually needs. Phase 3 makes the whole thing operable at fleet scale. Phase 4 turns the same primitives into a rehearsal: a failover that can be drilled, in a bubble or against a test cluster, without touching production replication.

The phase numbers above are this document's own, not the DR storage foundation gap analysis's (§1): its Phase 0 (shipping the csi-addons contract itself) is this design's Phase 1, and its Phase 1 (promote, demote, and resync end to end through Ramen) is this design's Phase 2.

---

## Phase 0 — External Prerequisites

| #    | Prerequisite                                                                                                                                                                                                                                                                     | Kind                    | Blocks  | Status                                                                                                                                                                         |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|---------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| P0-1 | A typed, steady-state per-volume replication status read: `GET .../volumes/{id}/replication/status` serving what `lvol_controller.get_replication_info` computes today (state, lag, outstanding bytes, failure counters), available for the volume's whole replicated life       | Control plane (`sbcli`) | Phase 1 | Shipped                                                                                                                                                                        |
| P0-2 | Idempotent attach and detach: attaching a volume to the policy it already follows returns success, and detaching a non-attached volume returns success                                                                                                                           | Control plane (`sbcli`) | Phase 1 | Shipped                                                                                                                                                                        |
| P0-3 | A standalone demote verb: `POST .../volumes/{id}/replication/demote` that converges the peer while still serving (repeated snapshot-and-ship until the remaining delta is small), then quiesces, ships the final delta, confirms it landed on the peer, and fences the data path | Control plane (`sbcli`) | Phase 2 | Not shipped                                                                                                                                                                    |
| P0-4 | An `rpo_target_seconds` field on `ReplicationPolicy`, so RPO compliance is computable against a declared target rather than the derived lag budget                                                                                                                               | Control plane (`sbcli`) | Phase 3 | Shipped                                                                                                                                                                        |
| P0-5 | csi-addons upstream: the `VolumeReplication` and `VolumeReplicationClass` CRDs (`replication.storage.openshift.io/v1alpha1`), the kubernetes-csi-addons controller-manager image, and the csi-addons sidecar image                                                               | Ecosystem               | Phase 1 | Vendored in the chart at v0.15.0 behind `csiaddons.create` (all twelve upstream CRDs, since the stock manager starts a controller per kind); sidecar wiring shipped in Phase 1 |
| P0-6 | A latest-replicated-snapshot read: per volume, and per consistency group as one complete generation, the newest fully replicated snapshot on the secondary addressed as a cloneable object                                                                                       | Control plane (`sbcli`) | Phase 4 | Shipped                                                                                                                                                                        |

Everything else the adapter needs already exists: the attach and detach calls, failover, the failback and commit pair, the relationship read, and the backlog arithmetic inside `get_replication_info`. The adapter is thin precisely because the engine is complete. What is missing is the shape Ramen can drive.

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
14. [Test Failover](#14-test-failover)
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

**The direction is already committed.** The CRD redesign excludes the four replication kinds from its model because "that subsystem is being redesigned against the CSI Addons specification, whose `VolumeReplication` and `VolumeGroupReplication` kinds already carry the per-volume and per-group replication contract that a backup tool or a DR orchestrator understands" (`crd-redesign/design-crd-model.md`, Non-Goals). This document is that redesign's first, per-volume half. The DR storage foundation gap analysis (Phase 0 and Appendix A of that document) is its requirements source.

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
- A `StorageClass` and `VolumeSnapshotClass` naming convention across paired clusters that Ramen's peerClasses can express, with a preflight that verifies it.
- The RPO and backlog figures Ramen cannot carry (`bytesBehind`, throughput, RPO compliance) are exported as Prometheus metrics from the control plane.

### Non-Goals

- **`VolumeGroupReplication`.** Continuous group promote and demote on top of consistency groups is the next design. This one is strictly per volume. The group snapshot surface (`design-consistency-groups.md`) is untouched.
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
│  operator: peerClasses preflight, events on the ReplicationPair (§7.2);      │
│            PVCAnnotationWatcher skips csi-addons-managed volumes (§8);       │
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
│  GET    .../relationships/{lvol}/latest-snapshot  (P0-6, test failover)      │
│  exports simplyblock_replication_* metrics: lag, backlog, RPO (§11)          │
└──────────────────────────────────────────────────────────────────────────────┘
```

**One relationship, addressed from either cluster.** The `VolumeReplication.spec.dataSource` resolves to a PV whose handle is `{clusterID}:{poolID}:{volumeID}`. The driver resolves the backend from the handle through `clusters.Client`, exactly as every other RPC does, so the DR cluster's driver can drive the same backend relationship as the source cluster's without either side holding special state. This is the same stateless addressing the node plugin already uses to redirect a staged volume after failover.

**The adapter holds no state.** csi-addons RPCs are stateless and idempotent by contract. Every answer the driver gives is derived on the spot from the backend status read and the relationship record. There is no driver-side cache, no persisted step, and no state machine. A verb whose backend work outlives the call (a demote converging a busy peer) reports `Completed=False` until the backend reflects the target state, and Ramen's re-drive is the retry loop.

**The operator's part is small and off the data path.** The kinds that author the backend state (`ReplicationPair` for the target, `ReplicationPolicy` for cadence and retention) keep working unchanged, and the classes name what they author. On top of them the operator runs the peerClasses preflight (§7.2), surfacing convention drift as events on the pair, and teaches the `PVCAnnotationWatcher` the one-owner rule (§8) so the legacy annotation path and a `VolumeReplication` never fight over one volume. Everything imperative it used to own (`ReplicationOps`, the commit cutover, the cutover-proceed handshake) is off this contract and confined to the legacy path.

**The drill rides the same surface.** Test failover (§14) adds no machinery to this picture: the bubble mode clones the latest replicated snapshot (the P0-6 read plus the ordinary CSI clone path), and the test-cluster mode composes clone, a `drtest-` policy toward the test cluster, and the real failover. The invariant audit that proves a drill disturbed nothing reads the same P0-1 status endpoint the conditions come from.

---

## 4. The csi-addons Machinery

### 4.1 What gets deployed

- **CRDs** (chart): `volumereplications.replication.storage.openshift.io`, `volumereplicationclasses.replication.storage.openshift.io`, and the kubernetes-csi-addons operational CRDs (`csiaddonsnodes.csiaddons.openshift.io`). Vendored at a pinned upstream version, the same way the `VolumeGroupSnapshot` CRDs are.
- **Controller-manager** (chart): the stock kubernetes-csi-addons manager. It discovers driver endpoints through `CSIAddonsNode` objects and reconciles `VolumeReplication` by calling the driver's Replication gRPC.
- **Sidecar** (operator, `SimplyblockDriver` reconciler): the csi-addons sidecar in the controller StatefulSet. It connects to the plugin's socket, probes the csi-addons Identity service for capabilities, publishes a `CSIAddonsNode`, and proxies the manager's gRPC to the plugin. Wiring follows the existing sidecar pattern: a seventh field on `spec.sidecarImages`, a pinned default in `sidecars.go`, an appended `controllerSidecar` entry in `workloads.go` (appended, because the per-container tweaks there are positional), and a new RBAC component whose group alias is `replication.storage.openshift.io` plus `csiaddons.openshift.io`.

### 4.2 What the plugin serves

The plugin registers two additional gRPC services on the existing socket, beside the CSI services, following the GroupController precedent in `csicommon.NonBlockingGRPCServer`:

- **csi-addons Identity:** `GetIdentity`, `GetCapabilities` (advertising `VOLUME_REPLICATION`), and `Probe`. This is a distinct service from CSI Identity, so it is a new small server type, not an extension of the existing one.
- **Replication:** the six verbs of §5, implemented on the controller `Server` through an embedded `replication.UnimplementedControllerServer` from `github.com/csi-addons/spec` (pinned at v0.2.0), mirroring how the GroupController embeds its unimplemented base.

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

| Verb                               | Backend mapping                                                   | Semantics                                                                                                                                                                                                                                                                                                                                                                        |
|------------------------------------|-------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `PromoteVolume` with `force=true`  | `POST .../volumes/{v}/replication/failover`                       | Unplanned promote: clone the last fully replicated generation on the target, retire the (assumed dead) source. Idempotent by the backend's own NQN probe. `Completed=True` once the relationship reads `failed_over`.                                                                                                                                                            |
| `PromoteVolume` with `force=false` | `POST .../volumes/{v}/replication/failover` with the planned gate | The same promote as the forced form, refused unless the peer already holds every acknowledged write, which is exactly the state a completed demote leaves behind: the source fenced and the final flush confirmed. A planned promote against an undemoted or lagging source surfaces as `FAILED_PRECONDITION`. `Completed=True` once the relationship reports the target active. |
| `DemoteVolume`                     | `POST .../volumes/{v}/replication/demote` (P0-3)                  | Quiesce (ANA inaccessible), ship a final internal snapshot, wait until it carries the replicated marker, fence the data path, and record the role. This is the lossless half of a planned swap: after demote, the peer's planned promote loses nothing. `Completed=True` only when the final flush has landed on the peer.                                                       |
| `ResyncVolume`                     | `POST .../volumes/{v}/replication/failback`                       | Reverse the shipping direction, seeded by `data_uuid` matching so a recovered source resyncs by delta rather than full copy. `Resyncing=True` while the catch-up runs; it clears when the reverse lag is inside the budget. Resync reconciles the diverged old primary from the current primary; it never merges.                                                                |

**Promote is one operation; planned and unplanned differ only in what precedes it.** Both forms clone the last fully replicated generation on the target and serve it under the preserved NVMe identity, exactly as failover does today. The planned form is lossless not because it runs different machinery but because a completed demote guarantees the last replicated generation contains every acknowledged write, and the planned gate refuses the promote until that holds. The forced form skips the gate and accepts the RPO loss, because its premise is that the source is gone. The engine's commit cutover (`replication/commit`, the `FN_REPLICATION_FINAL` runner with its shrink rounds and cutover-proceed handshake) is deliberately NOT part of this contract: its convergence job moves into the demote verb (P0-3), and it remains only behind the legacy `ReplicationOps` migration path (§8, §13).

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

`replicationPolicy` names the backend `ReplicationPolicy` (resolved per cluster by name), which owns cadence, retention, mode, and the replication target. `schedulingInterval` restates the policy's interval for Ramen's `DRPolicy` matching, and the preflight (§7.2) checks the two agree. No secrets parameter is needed: the driver's credentials come from `secret.json`, as for every other RPC. The chart ships no default class. Classes are the user's to author, matching the `VolumeGroupSnapshotClass` decision.

One class per (policy, cadence) is the authoring model: a `VolumeReplicationClass` names exactly one policy, and a volume needing a different cadence follows a different policy under a different class. Per-volume interval overrides are not provided.

### 7.2 peerClasses convention and preflight

Two of the requirements below are Ramen's contract, and the rest is this design's convention; the split matters because only the contract can fail a DRPolicy.

**Contract (Ramen requires this):**

- **The `StorageClass` name exists on both clusters.** A failover restores the protected PVC with its original `spec.storageClassName`, a by-name reference, so an equivalent class of that exact name must exist on the peer. The peerClasses computation also joins classes across the two clusters by `StorageClass` name.
- **The Ramen identity labels.** Each cluster's `StorageClass` carries `ramendr.openshift.io/storageid` (differing per cluster, since the backends differ), and the two `VolumeReplicationClass` objects representing one relationship carry an equal `ramendr.openshift.io/replicationid`. The `VolumeReplicationClass` is selected per cluster by `spec.replicationClassSelector` labels plus `provisioner` and a `schedulingInterval` equal to the `DRPolicy`'s, never by name.

**Convention (this design chooses it for operability):**

- **Same `StorageClass` parameters on both clusters** apart from `cluster_id` (necessarily) and pool when pools differ. The replication target's pool mapping already handles the pool difference at shipping time.
- **Same `VolumeSnapshotClass` and `VolumeReplicationClass` names on both clusters**, each side's `replicationPolicy` naming that cluster's policy toward its peer. Ramen does not require the names to match, but one name per relationship is what keeps a fleet legible.

The preflight is a check, not a controller: a validation that runs on demand (and on `ReplicationPair` reconciliation) confirming that for each replication-enabled `StorageClass` the peer cluster has a same-named class, and that the named backend policies exist and point at each other's clusters. Its findings surface as events on the `ReplicationPair`, which is the object that already models the cluster pairing.

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

| Method | Endpoint                                                | Notes                                                                                                                                                                                                                                         |
|--------|---------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `PUT`  | `.../volumes/{v}` (`replication_policy_id`)             | Existing attach and detach. P0-2 makes both idempotent: same-policy attach and non-attached detach return success.                                                                                                                            |
| `GET`  | `.../volumes/{v}/replication/status`                    | **New (P0-1).** The typed steady-state status of §6.1. Never 404s for a volume that exists; `state: not_replicating, role: none` is a valid answer.                                                                                           |
| `POST` | `.../volumes/{v}/replication/failover` (+ planned gate) | Existing; the one promote, both forms. Gains a planned form that is refused unless the peer holds every acknowledged write (a completed demote). Idempotent by NQN probe.                                                                     |
| `POST` | `.../volumes/{v}/replication/demote`                    | **New (P0-3).** Quiesce, final ship, confirm on peer, fence. Idempotent: demoting a demoted volume returns success.                                                                                                                           |
| `POST` | `.../volumes/{v}/replication/failback`                  | Existing. Resync (direction reversal, delta-seeded).                                                                                                                                                                                          |
| `POST` | `.../volumes/{v}/replication/commit`                    | Existing, unchanged, and NOT part of this contract: it stays behind the legacy `ReplicationOps` migration path only (§13).                                                                                                                    |
| `GET`  | `.../replication/relationships/{lvol}`                  | Existing, unchanged. Cutover records only; the node redirect depends on its survive-deletion semantics.                                                                                                                                       |
| `GET`  | `.../replication/relationships/{lvol}/latest-snapshot`  | **New (P0-6).** The newest fully replicated snapshot for the volume, as a cloneable snapshot handle. A consistency-group form returns one complete generation's member snapshots. Exposes what the failover path already computes internally. |
| `POST` | `.../volumes/{v}/replication/cutover-proceed`           | Existing, unchanged, legacy path only: the adapter never reaches it, because the commit cutover is off this contract.                                                                                                                         |

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

The kubernetes-csi-addons controller-manager owns events on `VolumeReplication` (promote, demote, and resync outcomes), and this design adds none there. The operator emits preflight findings on the `ReplicationPair`:

| Event                 | Type    | Emitted when                                                                                                                |
|-----------------------|---------|-----------------------------------------------------------------------------------------------------------------------------|
| `PeerClassesVerified` | Normal  | The preflight confirmed same-named classes and mutually pointing policies on both clusters                                  |
| `PeerClassesMismatch` | Warning | A replication-enabled class has no same-named peer, or the named policies do not pair; the message names the class and side |

### Prometheus Metrics

Exported by the control plane, labeled `volume`, `policy`, and `peer_cluster`:

| Metric                                      | Labels                       | Description                                                                     |
|---------------------------------------------|------------------------------|---------------------------------------------------------------------------------|
| `simplyblock_replication_lag_seconds`       | volume, policy, peer_cluster | Now minus the newest fully replicated snapshot's creation time.                 |
| `simplyblock_replication_backlog_bytes`     | volume, policy, peer_cluster | `outstanding_bytes`: the queued-but-unshipped snapshot sizes.                   |
| `simplyblock_replication_last_sync_seconds` | volume, policy, peer_cluster | Duration of the last shipping cycle.                                            |
| `simplyblock_replication_last_sync_bytes`   | volume, policy, peer_cluster | Size of the last shipped snapshot.                                              |
| `simplyblock_replication_rpo_violation`     | volume, policy, peer_cluster | 1 while `lag_seconds` exceeds the policy's `rpo_target_seconds` (P0-4), else 0. |
| `simplyblock_replication_degraded`          | volume, policy, peer_cluster | 1 while the status read's state is `degraded` or `error`.                       |

`simplyblock_replication_rpo_violation` is the alert: it is the declared objective against the measured lag, which no timestamp alone can express. `simplyblock_replication_backlog_bytes` is the second load-bearing figure, because it is the input to any honest RTO estimate and the number that distinguishes "slow cycle" from "falling behind." One caveat is recorded rather than hidden: `outstanding_bytes` measures queued snapshot sizes, not dirty bytes written since the last snapshot, so intra-interval writes are invisible to it. A true dirty-delta figure needs storage-plane support and is future work.

---

## 12. Testing Strategy

Full scenario matrix and coverage status: [`tests/test-plan-csi-addons-replication.md`](../tests/test-plan-csi-addons-replication.md)

- **Unit (driver):** each verb against a mock control plane: the idempotency table (repeat enable, repeat disable, repeat promote), the refusal paths (different-policy enable, lagging planned promote, disable during cutover), the condition derivation from every status-read state, and handle parsing failures.
- **Unit (operator):** the peerClasses preflight against fake clients for both clusters, and the `PVCAnnotationWatcher` skip when a `VolumeReplication` exists.
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

---

## 14. Test Failover

A DR drill proves that failover works without disturbing production replication. Ramen cannot drive one: its two actions, `Failover` and `Relocate`, move the workload for real. The drill is therefore driven by a simplyblock-native kind (owned by the SiteMap and testing layer, and specified there, not here), and this section defines the storage contract that kind consumes. There are two modes over one substrate: a bubble on the secondary cluster, and a separate test cluster reached like the real failover.

### 14.1 The substrate: replicated snapshots are cloneable test points

The replicated snapshots on the secondary are first-class snapshot records on the secondary's own control plane, chained and complete, and for a consistency group they carry the `group_id` and `group_seq` provenance of their generation. The failover path already resolves the newest fully replicated snapshot per volume, and the group-wide resolution picks one complete generation across members. P0-6 exposes that resolution as a read (§9), so a drill can address its test point without reimplementing the selection logic.

Every object a drill creates, on either side, carries a `drtest-` name prefix and a test-id label. Leftovers are then enumerable, and a teardown can prove completeness instead of assuming it.

### 14.2 Bubble mode: same cluster, different namespace

The drill namespace lives on the secondary Kubernetes cluster, whose driver already talks to the backend holding the replicated snapshots, so no data moves at all:

1. Resolve the test point through P0-6: per volume the newest replicated snapshot, or for a group one complete `group_seq`, so the bubble starts from a single crash-consistent cut.
2. Surface each snapshot as a pre-provisioned `VolumeSnapshotContent` (the handle is the secondary-side snapshot), bind a `VolumeSnapshot` in the drill namespace, and clone it into a PVC through the ordinary `dataSource` path.
3. Deploy the application against the clones. The clones are thin, independent volumes, and writes to them never touch the replication stream.
4. Tear down by deleting the namespace, then enumerate by the test-id label to prove nothing leaked.

### 14.3 Test-cluster mode: the real failover, aimed at expendable volumes

The second mode reaches a separate storage cluster, and it is deliberately composed from primitives this design already relies on rather than a new shipping capability:

1. Clone the resolved test point into `drtest-` volumes on the secondary. For a group, clone one generation into a new `drtest-` consistency group, so the group failover path is exercised too.
2. Attach the clones to a `drtest-` replication policy whose `ReplicationTarget` is the test cluster. The ordinary engine ships them (a full copy, since the test backend shares no ancestry).
3. On the test cluster, run the real failover against the shipped volumes to materialize writable clones, and deploy the application there.

The property this buys is fidelity: the drill exercises the actual failover machinery, target resolution, clone-from-replicated, identity preservation, and the driver redirect, against a third cluster, while the production relationship is never touched, because the `drtest-` policy is a separate policy with its own target.

### 14.4 The non-disruption proof

A drill that silently perturbed replication would be worse than no drill. Before the first clone and after the teardown, the driving kind captures and compares: every production `VolumeReplication`'s conditions, the `lastSyncTime` cadence, the lag and backlog from the typed status read (P0-1), and the count of production `VolumeReplication` objects. Any drift fails the drill as an invariant violation. The status endpoint built for Ramen's conditions is the same instrument this audit reads, which is why the drill contract belongs in this design.

### 14.5 Costs and bounds

- **A live clone pins its base snapshot.** Retention defers pruning a snapshot with a dependent clone, which is what keeps the drill safe, and also why a drill must carry a maximum lifetime: a long-lived bubble holds the secondary's replicated chain back.
- **Test-cluster mode consumes real resources:** cross-cluster bandwidth for the full copy, and capacity on both the secondary (the `drtest-` clones) and the test cluster. The `drtest-` clones on the secondary exist only as replication sources and are never served, so they are created as internal volumes, the same treatment the shipping engine's own landing volumes already get: invisible to normal listings and exempt from the per-node subsystem cap. Storage capacity is unaffected either way; a drill's cleanup and audit go through the `drtest-` test-id enumeration (§14.1), not the ordinary volume listing.
- **The group rules apply unchanged:** a `drtest-` consistency group observes the member cap and the placement pin like any other, so a drill of a large group is a capacity event on the secondary.

---

## 15. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     | Owner                 |
|-----|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------|
| 1   | **Demote semantics for the application.** The P0-3 demote fences the volume (ANA inaccessible) after the final flush, and with convergence folded into the verb it is now the only place a planned swap can stall. Ramen relocation unmounts the workload first, so the fence is ordinarily unopposed. Confirm the verb's behavior when writes are still in flight at quiesce (block versus fail), whether the converge phase has its own budget separate from the quiesced flush, and whether a timeout in either phase must abort back to serving primary. | Backend team          |
| 2   | **Where the preflight lives.** §7.2 attaches peerClasses validation to the `ReplicationPair` reconciler. If the redesign retires the pair kind, the preflight needs a new home (the `SimplyblockDriver`, or a standalone check job).                                                                                                                                                                                                                                                                                                                         | Operator team         |
| 3   | **Different-policy enable refusal has no data source.** §5.1's `EnableVolumeReplication` was designed to refuse an attach to a different policy with `FAILED_PRECONDITION`. Phase 1 found that sbcli's `attach_policy` silently re-attaches instead of refusing, and no endpoint returns the policy id a volume is currently attached to, for the driver to compare against. Either the backend adds that read, or this design accepts the silent re-sync as the behavior.                                                                                   | Backend team          |
| 4   | **`lastSyncDuration` and `lastSyncBytes` have no response field.** §5.1's `GetVolumeReplicationInfo` was designed to return all three fields; `csi-addons/spec` v0.2.0's `GetVolumeReplicationInfoResponse` carries only `lastSyncTime`. Confirm whether a newer spec version adds the other two, or whether they surface some other way (a `VolumeReplication` annotation, a metric).                                                                                                                                                                       | Ecosystem/driver team |
