# Design Document: Ramen Integration

**Status:** Draft (§3 confirms peerClasses needs no operator code; §4 defers `VolumeGroupReplication` to `design-csi-addons-replication.md` §14, driver-and-backend, Planned; E2E validation pending)  
**Author:** Israel Geoffrey (geoffrey1330)  
**Date:** 2026-09-21  
**Test Plan:** [`tests/test-plan-ramen-integration.md`](../tests/test-plan-ramen-integration.md)

---

## Phase 0: External Prerequisites

| #    | Prerequisite                                                                                                       | Kind      | Blocks                                       | Status                                                                                                    |
|------|--------------------------------------------------------------------------------------------------------------------|-----------|----------------------------------------------|-----------------------------------------------------------------------------------------------------------|
| P0-1 | A live OCM hub with at least two registered managed clusters                                                       | Ecosystem | The E2E validation (§6)                      | Unknown                                                                                                   |
| P0-2 | Ramen installed on the hub and on each managed cluster, with a `DRPolicy` naming both clusters                     | Ecosystem | The E2E validation (§6)                      | Unknown                                                                                                   |
| P0-3 | SiteMap, or a hand-authored `DRPlacementControl` standing in for it, driving the `DRPolicy`                        | Ecosystem | The E2E validation (§6)                      | Not shipped. SiteMap is an external document and system, and storage is explicitly outside its own scope. |
| P0-4 | `csi-addons/spec` at a version whose `GetVolumeReplicationInfoResponse` carries `lastSyncBytes`/`lastSyncDuration` | Ecosystem | Full Appendix A.3 `GetVolumeReplicationInfo` | Not shipped: pinned at v0.2.0 today, which has neither field.                                             |

Without P0-1 through P0-3 nothing in §6 can run, because the validation is E2E-only, live-cluster work that no mock or `envtest` substitutes for. §4's `VolumeGroupReplication` work is a driver-and-backend feature specified in `design-csi-addons-replication.md` §14 and does not depend on the OCM hub for its own unit and backend tests, only for the E2E group scenario (M-05). P0-4's absence is narrower: it leaves `GetVolumeReplicationInfo` reporting only `lastSyncTime`, never cycle size or duration, but Ramen's own `PeerReady` gate (§5.1) does not read either field, so P0-4 does not block the validation itself.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [peerClasses: Ramen's Own Mechanism](#3-peerclasses-ramens-own-mechanism)
4. [VolumeGroupReplication](#4-volumegroupreplication)
5. [The Contract, Confirmed](#5-the-contract-confirmed)
6. [E2E Validation Plan](#6-e2e-validation-plan)
7. [Testing Strategy](#7-testing-strategy)
8. [Open Questions](#8-open-questions)

---

## Overview

`design-csi-addons-replication.md` builds the storage-level adapter Ramen's per-volume DR contract requires, and validates it end to end "without Ramen" (its own §12): real backend, real csi-addons machinery, but a hand-driven `VolumeReplication` object rather than a real Ramen reconcile loop. That document's own §7.2 named `peerClasses` verification Ramen's own hub-side mechanism, out of its scope, and deferred `VolumeGroupReplication` as "the next design," strictly per volume itself. `design-consistency-groups.md` deferred `VolumeGroupReplication` too, as future work independent of any replication policy. This document records Ramen's part in the two pieces that needed new design: peerClasses (§3) and `VolumeGroupReplication` (§4), the latter now specified in full in `design-csi-addons-replication.md` §14. §3 confirms, rather than reopens, `design-csi-addons-replication.md` §7.2's original position on peerClasses, after this document's own history of first building a same-cluster preflight for it and then removing that preflight once it became clear a same-cluster check cannot verify a cross-cluster pairing. §5 through §6 confirm the rest of the per-volume contract against what already shipped and specify the E2E validation that closes `design-csi-addons-replication.md` §12's outstanding acceptance gate (E-06, E-07).

---

## 1. Background

The gap analysis's own headline finding (§2) was that simplyblock's DR machinery was "a complete, parallel, out-of-band system that implements none of the interfaces Ramen drives." Appendix A answered that with a concise spec: the six csi-addons `Replication` gRPC verbs, the one info query, and the `VolumeReplication.status` conditions Ramen actually reads (`Completed`, `Degraded`, `Resyncing`), aggregated by Ramen's own VRG into `DataProtected` and by the DRPC into `PeerReady`, the boolean that gates `Relocate` and failback. Appendix B specified the RPO/RTO figures Ramen cannot carry natively, as metrics.

`design-csi-addons-replication.md`'s three phases implement that spec. This document does not repeat what it built. §5 below cites, section by section, where each Appendix A and Appendix B item now lives in the shipped code.

The other half of Appendix A's own premise is that Ramen's hub, not this operator, computes `peerClasses` and drives the VRG, through OCM's hub-spoke visibility into every managed cluster. That hub, and the OCM/SiteMap layer above it, is external to this repository (confirmed this session: no `ManagedCluster`, `DRPolicy`, or `VolumeReplicationGroup` reference exists anywhere in this codebase outside design-doc prose, and no mechanism for this operator to reach a peer cluster's Kubernetes API exists or is needed. `design-management-hub.md`, this repo's own hub design, specifies a real hub component of its own (the `fleet-manager`, reading member clusters through OCM's `ManagedClusterView`), but it is a separate fleet-config-distribution concern, and nothing in it builds or is intended to build Ramen's own pairing). Ramen's hub already reports when it cannot find a valid `StorageClass`/`VolumeReplicationClass` pairing across two clusters, once it looks, and only the hub's own cross-cluster visibility can make that comparison at all: a same-cluster read sees whether this cluster's own label is present, never whether its value agrees with the peer's, which is the only question a pairing check actually needs answered. §3 explains why this operator's own history of trying to close that gap locally settled on not closing it.

The gap analysis's own §6 named a second piece of genuinely new work, in its Phase 2: `VolumeGroupReplication`, "on top of the CG primitive," so that the VRG async group path can protect and fail over a multi-volume app at one point rather than only snapshot it. `design-consistency-groups.md` built the CG primitive that gap analysis cites (the `storage.simplyblock.io/consistency-group` label, `VolumeGroupSnapshot`, the `GroupController`) but explicitly left group replication for later, independent of any policy, exactly so this document could attach it without reshaping the group. §4 records Ramen's part in that attachment; the group surface itself is specified as driver-and-backend work in `design-csi-addons-replication.md` §14 (Planned). Everything else Appendix A and Appendix B specify is confirmed, in §5, against code `design-csi-addons-replication.md` already shipped.

---

## 2. Goals and Non-Goals

### Goals

- Confirm that Ramen's `peerClasses` pairing needs no operator-side code, settling the question this document's own earlier attempt at a same-cluster preflight left open (§3).
- Confirm Ramen drives `VolumeGroupReplication` through the stock csi-addons machinery (the `external: false` path) against the driver-and-backend group surface `design-csi-addons-replication.md` §14 specifies, satisfying Appendix A.4's group-readiness query and the gap analysis's own Phase 2 ask (§4).
- Confirm, against the actual shipped code, that every condition, verb, and status query Appendix A specifies is satisfied, or state precisely which is not and why (§5).
- Confirm, against the actual shipped code, which Appendix B metrics are delivered, which are derivable from what already exists, and which remain future work (§5.4).
- Specify a live-cluster validation that closes `design-csi-addons-replication.md` §12's outstanding acceptance gate: a real Ramen VRG, on a real OCM-registered pair of clusters, driving the adapter through protect, planned relocate, and unplanned failover (§6).

### Non-Goals

- **Building any hub, OCM, or cross-cluster Kubernetes access mechanism.** That is Ramen's and OCM's job, external to this operator, confirmed in §1. Nothing in §6's validation plan asks this operator to reach a peer cluster's API server: every step drives objects on the cluster where the workload currently runs, exactly as `design-csi-addons-replication.md`'s own architecture already assumes.
- **A same-cluster `peerClasses` preflight, in any form.** Tried once, in this document's own history, and removed (§3): a same-cluster read can confirm a label is present, never that its value agrees with the peer's, and a pairing check that cannot verify agreement is not a pairing check. That comparison needs the hub's own visibility into both clusters and is Ramen's job alone, once a `DRPolicy` exists.
- **Global VGR.** RamenDR's newer multi-VRG consensus feature for a replication group spanning several applications is out of scope. §4 covers the base case: one VRG, one storage vendor's PVCs, one `VolumeGroupReplication` (§8 Open Question 4).
- **A new group gRPC verb on the driver's Replication service.** The same `PromoteVolume`/`DemoteVolume`/`ResyncVolume` verbs act on a group handle rather than a single volume (`design-csi-addons-replication.md` §14.4). The new driver surface is the csi-addons VolumeGroup service (`CreateVolumeGroup` and siblings), not a fourth Replication verb.
- **SiteMap.** A separate external document and system. Where §6's topology needs a `DRPlacementControl` and SiteMap is not available to author one, a hand-authored stand-in is explicitly permitted (P0-3).
- **`bytesBehind`'s remaining Appendix B siblings** (throughput, RTO estimation, backup RPO/RTO). §5.4 accounts for each, and none blocks the validation this document specifies.
- **Widening `csi-addons/spec` past v0.2.0.** P0-4 is recorded as a prerequisite, not solved here: it is an upstream dependency version, not something this repository's own code can add a field to.

---

## 3. peerClasses: Ramen's Own Mechanism

Ramen's hub pairs a `StorageClass` and a `VolumeReplicationClass` across two managed clusters into a `peerClasses` entry on its `DRPolicy`, through OCM's hub-spoke visibility into both clusters at once. That visibility is what makes the pairing check meaningful, and it is exactly what a reconciler running inside one managed cluster does not have and cannot substitute for: the only question worth asking about a pairing is whether the two clusters' objects *agree*, and agreement is a cross-cluster comparison, not a same-cluster one.

This document tried the same-cluster version anyway, once. A `ReplicationPairReconciler` preflight was specified and implemented, confirming this cluster's own `VolumeReplicationClass` carried the `ramendr.openshift.io/replicationid` label and a `schedulingInterval`, and emitting `PeerClassesVerified`/`PeerClassesMismatch` accordingly. That check has a real, narrow use (it catches an operator who forgot the label entirely), but it is not peerClasses verification: a cluster whose label carries a value that does not match its peer's passes identically to one whose value is correct, because presence, not agreement, is everything a same-cluster read can check. Reporting `PeerClassesVerified` under that name risked being read as a stronger guarantee than it delivered, so it has been removed. Nothing in this repository performs this check today, which is the position `design-csi-addons-replication.md` §7.2 already took before this document first tried to revisit it.

The exact contract Ramen's pairing requires of the two clusters' objects (the identity labels, the `StorageClass` name-matching, and the authoring convention this repository follows beyond what Ramen strictly requires) is recorded in `design-csi-addons-replication.md` §7.2, unchanged by this document. `VolumeGroupReplication` (§4) is unaffected by any of this: it needs no cross-cluster comparison of its own, only this cluster's own consistency-group membership.

---

## 4. VolumeGroupReplication

The gap analysis's Phase 2 named this the piece consistency groups still owed: "Add `VolumeGroupReplication` (csi-addons) on top of the CG primitive so the VRG async group path can protect and fail over multi-volume apps at one point, not just snapshot them." It is now specified in full in `design-csi-addons-replication.md` §14, as a **driver-and-backend** feature driven by the stock kubernetes-csi-addons machinery (Phase 4, Planned there). This section records only what Ramen contributes to that path and where this document validates it.

**Ramen's part.** Ramen's VRG (`RamenDR/ramen`'s `vrg_volgrouprep.go`, confirmed present at `v0.1.0-rc1`) creates one `VolumeGroupReplication` when the member PVCs' `StorageClass` carries `ramendr.openshift.io/groupreplicationid` (the group-level sibling of the per-volume `replicationid` of `design-csi-addons-replication.md` §7.1), and sets `spec.external` from whether that same `StorageClass` carries `ramendr.openshift.io/offloaded`. simplyblock takes the `external: false` path (`design-csi-addons-replication.md` §14.1): the `StorageClass` carries `groupreplicationid` but **not** `offloaded`, so the stock controller-manager owns the object, groups the members through the driver's csi-addons VolumeGroup service, and replicates the whole group as one unit through the driver's Replication service, addressed by a group handle. No operator code is on the path.

**No new group verb.** Group promote, demote, and resync are the same three verbs `design-csi-addons-replication.md` §5 already ships, applied to the group handle rather than a single volume (§14.4). The recovery generation is a group snapshot, so a group failover lands every member at one crash-consistent point.

**Global VGR out of scope.** RamenDR's newer multi-VRG consensus, spanning a replication group across several applications' VRGs, is not covered here; §8 Open Question 4.

**An earlier operator-owned revision is removed.** A previous version of this section specified a `VolumeGroupReplicationReconciler` and a `VolumeGroupReplicationValidator` in this operator, owning `external: true` objects and fanning them out to per-member `VolumeReplication` objects. `design-csi-addons-replication.md` §13 removes them in favor of the driver-driven path above: it re-implemented in the operator what the stock controller already does, and drove the group through per-member calls rather than one group operation. Nothing in this operator handles `VolumeGroupReplication` now.

---

## 5. The Contract, Confirmed

### 5.1 Conditions (Appendix A.2)

Appendix A.2 specified `Completed`, `Degraded`, `Resyncing`, with steady-state healthy async as all three settled (`Completed=True, Degraded=False, Resyncing=False`) and lag alone tripping none of them. `design-csi-addons-replication.md` §6.2 implements exactly this mapping: `Completed` from the last requested state change finishing, `Degraded` from the status read's `degraded`/`error` state or `lag_seconds` exceeding `lag_budget_seconds`, `Resyncing` from the status read's own `resyncing` flag. This exceeds Appendix A.2's own "interim" fallback (a staleness heuristic on `lastReplicatedAt`): `Degraded` is sourced from the backend's real failing-task signal, not a timestamp guess.

Appendix A.3's `PeerReady` (`Completed=True && Degraded=False && Resyncing=False` on the peer) needs no code of this repository's own. It is Ramen's own DRPC-level aggregation of the three conditions above, computed client-side from what §6.2 already reports.

### 5.2 gRPC verbs (Appendix A.3)

All six verbs Appendix A.3 specifies are implemented and idempotent, per `design-csi-addons-replication.md` §5:

| csi-addons verb            | Appendix A.3 requirement                                                       | Status                                                                                                                                                                                                                                                                                                                                               |
|----------------------------|--------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `EnableVolumeReplication`  | Explicit, idempotent attach. `Completed` once the relationship is established. | Implemented (§5.1, P0-2)                                                                                                                                                                                                                                                                                                                             |
| `DisableVolumeReplication` | Explicit stop/teardown verb (implicit before this design)                      | Implemented (§5.1, P0-2)                                                                                                                                                                                                                                                                                                                             |
| `PromoteVolume` (`force`)  | Planned (peer alive) vs. unplanned (peer gone), wired through `force`          | Implemented (§5.2), including the source-health no-op that keeps a healthy day-one promote from cloning unnecessarily                                                                                                                                                                                                                                |
| `DemoteVolume`             | A new standalone verb: quiesce, final flush, confirm on peer                   | Implemented (§5.2, P0-3), exactly the "new standalone demote" Appendix A called for                                                                                                                                                                                                                                                                  |
| `ResyncVolume`             | A direction-reversing resync with `Resyncing`→`Completed` progress             | Implemented (§5.2), maps onto `failback`                                                                                                                                                                                                                                                                                                             |
| `GetVolumeReplicationInfo` | `lastSyncTime`, plus `lastSyncBytes`/`lastSyncDuration`                        | `lastSyncTime` implemented (§5.1, P0-1). `lastSyncBytes`/`lastSyncDuration` blocked on P0-4: the response type in the pinned `csi-addons/spec` v0.2.0 has no field for either, so the backend's own cycle-size and cycle-duration reads (`get_replication_info`'s `last_cycle_bytes`/`last_cycle_seconds`) have nowhere to land in the RPC response. |

### 5.3 Status queries (Appendix A.4)

1. **Per-slot health/role, never 404ing:** implemented (P0-1, `design-csi-addons-replication.md` §6.1). `state: not_replicating, role: none` is the valid answer for an unreplicated volume, closing exactly the 404 gap Appendix A.4.1 named.
2. **The `PeerReady` boolean:** covered by §5.1 above, Ramen's own aggregation, needing no new code.
3. **Group readiness:** out of scope here (§2 Non-Goals), `design-consistency-groups.md`'s future work.

### 5.4 Observability (Appendix B)

| Appendix B concept       | Metric                                               | Status                                                                                                                                                                                                    |
|--------------------------|------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| RPO, time gap            | `simplyblock_replication_lag_seconds`                | Implemented (`design-csi-addons-replication.md` §11)                                                                                                                                                      |
| RPO, data gap            | `simplyblock_replication_backlog_bytes`              | Implemented. This is Appendix B's "one genuinely new backend measurement," `bytesBehind`. `get_replication_info`'s existing `outstanding_bytes` (queued-but-unshipped snapshot sizes) already reports it. |
| Last cycle size/duration | `simplyblock_replication_last_sync_bytes`/`_seconds` | Implemented                                                                                                                                                                                               |
| RPO compliance           | `simplyblock_replication_rpo_violation`              | Implemented, keyed on `ReplicationPolicy.rpo_target_seconds`                                                                                                                                              |
| Degraded state           | `simplyblock_replication_degraded`                   | Implemented                                                                                                                                                                                               |
| Throughput (smoothed)    | (none)                                               | Not implemented as its own series. Derivable via PromQL (`last_sync_bytes / last_sync_seconds`) from what already exists. Non-blocking.                                                                   |
| RTO (estimated)          | (none)                                               | Not implemented. Appendix B itself frames this as "always an estimate," lowest priority of the set. Non-blocking.                                                                                         |
| Backup RPO/RTO (S3)      | (none)                                               | Out of scope. `design-csi-addons-replication.md` §2's own Non-Goals exclude the VolSync/S3 backup path entirely, a separate phase of the gap analysis.                                                    |

Nothing in this row set blocks §6's validation: every metric `PeerReady` or a planned/unplanned promote depends on is already implemented.

---

## 6. E2E Validation Plan

### 6.1 Topology

Two Kubernetes clusters, each running a `SimplyblockDriver` against its own simplyblock storage cluster, paired exactly as `regression_test/21/test_csi_addons_replication.sh` already sets up (one shared simplyblock control plane, `ReplicationPair`/`ReplicationPolicy` authoring the backend relationship). On top of that pair: an OCM hub (a third cluster, or the hub role colocated on one of the two), both storage clusters registered as `ManagedCluster`s, Ramen's dr-cluster operator installed on each, Ramen's hub operator installed on the hub, and one `DRPolicy` naming both clusters via their `StorageClass`/`VolumeReplicationClass` labels (`design-csi-addons-replication.md` §7.1's `ramendr.openshift.io/storageid`/`replicationid` labels, already implemented). A `DRPlacementControl` drives the workload, authored by SiteMap where available and hand-authored otherwise (P0-3).

### 6.2 Test flow

1. **Protect.** Deploy a workload with a PVC on cluster A, under a `StorageClass`/`VolumeReplicationClass` pair carrying the Ramen labels. Create the `DRPlacementControl`. Confirm Ramen's VRG creates one `VolumeReplication` for the PVC, `EnableVolumeReplication` fires, and `status.lastSyncTime` advances on the policy's ordinary cadence.
2. **Planned relocate.** Trigger Ramen's `Relocate` action. Confirm the sequence design-csi-addons-replication.md §5.2 documents drives correctly through Ramen rather than by hand: the source-side VRG demotes (fence, final flush, `Completed` settles once confirmed), then the target-side VRG promotes with `force=false`, gated on `PeerReady` from step 1's demote. Confirm the workload comes up on cluster B with the data a hashed writer wrote before relocation, and that Ramen's own `PeerReady`/`DataProtected` aggregation reports correctly throughout, not just this design's own conditions in isolation.
3. **Unplanned failover.** Simulate cluster B's loss (or reachability loss, matching this session's `replication_source_online` distinction) and trigger Ramen's `Failover` action against cluster A. Confirm the force-escalation path Ramen's own controller drives (§5.2's "no wait-and-retry grace period" behavior) still lands correctly when Ramen, not a test script, is the one issuing the calls.
4. **Resync.** Recover the failed cluster and confirm Ramen drives `ResyncVolume` to reconcile the diverged copy, without merging or re-triggering a full cutover.

### 6.3 Pass/fail criteria

Every step's pass criterion is an observable Ramen already reports on its own objects (`VolumeReplication.status.conditions`, the VRG's `DataProtected`, the DRPC's `PeerReady`), not a simplyblock-specific read: the point of this validation is that Ramen's own surface reflects reality, not that a side-channel confirms it. Data correctness is checked with a hashed writer across every promote, matching `design-csi-addons-replication.md` §12's existing E2E discipline.

---

## 7. Testing Strategy

Two different classes of coverage, for the two different things this document specifies. §3 adds nothing to test: it confirms that no operator-side code exists for peerClasses, and there is no code left to exercise once the same-cluster preflight was removed.

- **§4's group surface is tested where it lives**, in `design-csi-addons-replication.md` §14: the driver's VolumeGroup service and group-handle Replication routing, and the backend group-replication engine, as unit and backend scenarios in that document's test plan (U-62 … U-69). This document adds no unit test of its own for it, since no operator code implements it any longer.
- **§5 confirms existing code** and adds nothing to test on its own. **§6 is exclusively E2E, live-cluster validation** with no smaller harness to substitute, including a group-protect scenario (M-05) confirming a real VRG's `VolumeGroupReplication` driving the driver's group surface end to end.

Full scenario detail: [`tests/test-plan-ramen-integration.md`](../tests/test-plan-ramen-integration.md). Its M-01 and M-02 close `design-csi-addons-replication.md` test plan's E-06 and E-07, which have carried no implementing test since they were written.

---

## 8. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                                       | Owner                 |
|-----|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------|
| 1   | **Is a real OCM hub with Ramen already available for this validation**, or does P0-1/P0-2 need to be stood up from scratch? The answer decides whether §6 is schedulable now or needs its own infrastructure work first.                                                                                                                                                       | Operator team / Infra |
| 2   | **Does SiteMap exist in a runnable form yet**, or does every run of §6 use a hand-authored `DRPlacementControl` stand-in (P0-3)? If SiteMap is not yet runnable, note that explicitly rather than blocking on it indefinitely.                                                                                                                                                 | Operator team         |
| 3   | **`csi-addons/spec` version floor (P0-4).** Confirm whether a newer pinned version already carries `lastSyncBytes`/`lastSyncDuration` before treating this as a real upstream gap to track.                                                                                                                                                                                    | Operator team         |
| 4   | **Is Global VGR needed.** §4 scopes `VolumeGroupReplication` to the base, single-VRG case. Confirm whether any planned simplyblock deployment spans a replication group across more than one application's VRG before treating RamenDR's multi-VRG consensus feature as work this document should also specify. Tracked as `design-csi-addons-replication.md` Open Question 4. | Operator team         |
| 5   | ~~**`VolumeGroupReplicationContent`'s exact contract.**~~ **Moved.** With group replication now the driver-and-backend, `external: false` path (§4), the `Content` object is created and owned by the stock controller-manager; its contract is tracked in `design-csi-addons-replication.md` Open Question 5, not here.                                                       | Operator team         |
