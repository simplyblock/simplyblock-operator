# Design Document: Ramen Integration

**Status:** Draft (contract confirmed, peerClasses preflight specified, implementation and E2E validation pending)  
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

Without P0-1 through P0-3 nothing in §6 can run, because the validation is E2E-only, live-cluster work that no mock or `envtest` substitutes for. §3's preflight and §4's `VolumeGroupReplication` reconciler are both unaffected: neither needs an OCM hub, a Ramen installation, or a `DRPolicy`, only this cluster's own objects. P0-4's absence is narrower: it leaves `GetVolumeReplicationInfo` reporting only `lastSyncTime`, never cycle size or duration, but Ramen's own `PeerReady` gate (§5.1) does not read either field, so P0-4 does not block the validation itself.

---

## Table of Contents

1. [Background](#1-background)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [peerClasses Preflight](#3-peerclasses-preflight)
4. [VolumeGroupReplication](#4-volumegroupreplication)
5. [The Contract, Confirmed](#5-the-contract-confirmed)
6. [E2E Validation Plan](#6-e2e-validation-plan)
7. [Testing Strategy](#7-testing-strategy)
8. [Open Questions](#8-open-questions)

---

## Overview

`design-csi-addons-replication.md` builds the storage-level adapter Ramen's per-volume DR contract requires, and validates it end to end "without Ramen" (its own §12): real backend, real csi-addons machinery, but a hand-driven `VolumeReplication` object rather than a real Ramen reconcile loop. That document deferred two pieces as "the next design": a `peerClasses` preflight (its own §7.2, once it became clear `peerClasses` itself is Ramen's mechanism, not this operator's) and `VolumeGroupReplication` (its own §2 Non-Goals, strictly per volume itself). `design-consistency-groups.md` deferred `VolumeGroupReplication` too, as future work independent of any replication policy. Neither document claims it. This document is where both land: §3 specifies the preflight, §4 specifies group replication on top of the consistency-group primitive, and §5 through §6 confirm the rest of the per-volume contract against what already shipped and specify the E2E validation that closes `design-csi-addons-replication.md` §12's outstanding acceptance gate (E-06, E-07).

---

## 1. Background

The gap analysis's own headline finding (§2) was that simplyblock's DR machinery was "a complete, parallel, out-of-band system that implements none of the interfaces Ramen drives." Appendix A answered that with a concise spec: the six csi-addons `Replication` gRPC verbs, the one info query, and the `VolumeReplication.status` conditions Ramen actually reads (`Completed`, `Degraded`, `Resyncing`), aggregated by Ramen's own VRG into `DataProtected` and by the DRPC into `PeerReady`, the boolean that gates `Relocate` and failback. Appendix B specified the RPO/RTO figures Ramen cannot carry natively, as metrics.

`design-csi-addons-replication.md`'s three phases implement that spec. This document does not repeat what it built. §5 below cites, section by section, where each Appendix A and Appendix B item now lives in the shipped code.

The other half of Appendix A's own premise is that Ramen's hub, not this operator, computes `peerClasses` and drives the VRG, through OCM's hub-spoke visibility into every managed cluster. That hub, and the OCM/SiteMap layer above it, is external to this repository (confirmed this session: no `ManagedCluster`, `DRPolicy`, or `VolumeReplicationGroup` reference exists anywhere in this codebase outside design-doc prose, and no mechanism for this operator to reach a peer cluster's Kubernetes API exists or is needed. `design-management-hub.md`, this repo's own hub design, is a separate fleet-config-distribution concern that cites Ramen's hub/spoke split only as precedent, not as something it builds). Ramen's hub already reports when it cannot find a valid `StorageClass`/`VolumeReplicationClass` pairing across two clusters, once it looks. What it has no way to see is a pairing that is wrong on one cluster alone, before any hub-side comparison happens: a `VolumeReplicationClass` missing the `ramendr.openshift.io/replicationid` label, or carrying no `schedulingInterval`, sits invisibly broken until a `DRPolicy` is authored against it and the hub's own reconcile surfaces a cryptic cross-cluster mismatch instead of the local, fixable cause. §3 closes that local gap.

The gap analysis's own §6 named a second piece of genuinely new work, in its Phase 2: `VolumeGroupReplication`, "on top of the CG primitive," so that the VRG async group path can protect and fail over a multi-volume app at one point rather than only snapshot it. `design-consistency-groups.md` built the CG primitive that gap analysis cites (the `storage.simplyblock.io/consistency-group` label, `VolumeGroupSnapshot`, the `GroupController`) but explicitly left group replication for later, independent of any policy, exactly so this document could attach it without reshaping the group. §4 is that attachment. Together, §3 and §4 are the only new production code this document proposes: everything else Appendix A and Appendix B specify is confirmed, in §5, against code `design-csi-addons-replication.md` already shipped.

---

## 2. Goals and Non-Goals

### Goals

- Specify and implement a same-cluster `peerClasses` preflight: catch a `StorageClass`/`VolumeReplicationClass` pairing that Ramen's contract requires but this cluster's objects do not satisfy, before a `DRPolicy` is ever authored against it (§3).
- Specify and implement `VolumeGroupReplication` on top of the consistency-group primitive: fan a group's `primary`/`secondary`/`resync` intent out to its members' existing per-volume adapter, and fan their status back in, satisfying Appendix A.4's group-readiness status query and the gap analysis's own Phase 2 ask (§4).
- Confirm, against the actual shipped code, that every condition, verb, and status query Appendix A specifies is satisfied, or state precisely which is not and why (§5).
- Confirm, against the actual shipped code, which Appendix B metrics are delivered, which are derivable from what already exists, and which remain future work (§5.4).
- Specify a live-cluster validation that closes `design-csi-addons-replication.md` §12's outstanding acceptance gate: a real Ramen VRG, on a real OCM-registered pair of clusters, driving the adapter through protect, planned relocate, and unplanned failover (§6).

### Non-Goals

- **Building any hub, OCM, or cross-cluster Kubernetes access mechanism.** That is Ramen's and OCM's job, external to this operator, confirmed in §1. Neither §3's preflight nor §4's group reconciler reaches past this cluster's own objects, and nothing in §6's validation plan asks this operator to reach a peer cluster's API server: every step drives objects on the cluster where the workload currently runs, exactly as `design-csi-addons-replication.md`'s own architecture already assumes.
- **Resolving a `peerClasses` mismatch across two clusters.** That comparison needs the hub's own visibility into both clusters and is Ramen's job once a `DRPolicy` exists. §3 catches what is locally wrong before that comparison ever runs, and it does not repeat the comparison itself.
- **Global VGR.** RamenDR's newer multi-VRG consensus feature for a replication group spanning several applications is out of scope. §4 covers the base case: one VRG, one storage vendor's PVCs, one `VolumeGroupReplication` (§4.1, §8 Open Question 5).
- **A new gRPC verb for group replication.** §4.2 is explicit: group promote, demote, and resync fan the same three verbs `design-csi-addons-replication.md` §5 already ships out to every member. Nothing changes on the driver.
- **SiteMap.** A separate external document and system. Where §6's topology needs a `DRPlacementControl` and SiteMap is not available to author one, a hand-authored stand-in is explicitly permitted (P0-3).
- **`bytesBehind`'s remaining Appendix B siblings** (throughput, RTO estimation, backup RPO/RTO). §5.4 accounts for each, and none blocks the validation this document specifies.
- **Widening `csi-addons/spec` past v0.2.0.** P0-4 is recorded as a prerequisite, not solved here: it is an upstream dependency version, not something this repository's own code can add a field to.

---

## 3. peerClasses Preflight

Ramen pairs a `StorageClass` and a `VolumeReplicationClass` across two managed clusters into a `peerClasses` entry on the hub's `DRPolicy`, using labels this operator's own convention already defines: `ramendr.openshift.io/storageid` on the `StorageClass`, `ramendr.openshift.io/replicationid` on the `VolumeReplicationClass`, both cited in `design-csi-addons-replication.md` §7.1. When the pairing is wrong on one cluster alone, the earliest and most legible place to catch it is that cluster, before a `DRPolicy` ever compares it against a peer.

### 3.1 What it checks

The `ReplicationPairReconciler` (`operator/internal/controller/replicationpair_controller.go`), on its existing 60-second reconcile cadence (`replPairSyncInterval`), adds a preflight step reading only this cluster's own objects:

1. **List `StorageClass` objects** (typed `storagev1.StorageClass`, the same type `pvcreplication_controller.go` already reads), filtered to `Provisioner == "csi.simplyblock.io"`.
2. **Enrollment signal.** If none of this cluster's simplyblock `StorageClass` objects carries the `ramendr.openshift.io/storageid` label, there is nothing to preflight, and the check is a no-op: an unlabeled `StorageClass` is not a Ramen-managed one, and flagging it would be noise, not a finding.
3. **List `VolumeReplicationClass` objects.** This is an external CRD (`replication.storage.openshift.io/v1alpha1`, from `csi-addons`'s own `volume-replication-operator`) this repository does not vendor a Go type for, so the list reads `unstructured.UnstructuredList` against `schema.GroupVersionKind{Group: "replication.storage.openshift.io", Version: "v1alpha1", Kind: "VolumeReplicationClassList"}`, the same no-vendored-type pattern `internal/upgrade/discover/kinds.go` already uses for `cert-manager`'s `Certificate`.
4. **Per-class check**, for every listed `VolumeReplicationClass` whose `spec.provisioner` is `csi.simplyblock.io`:
   - `metadata.labels["ramendr.openshift.io/replicationid"]` is present and non-empty.
   - `spec.parameters.schedulingInterval` is present and non-empty.

   Neither check calls the simplyblock backend: whether the named `schedulingInterval` corresponds to a real `ReplicationPolicy` is `design-csi-addons-replication.md` §5's own province (`replicationpolicy_controller.go` already rejects an unresolvable policy at that layer), and re-checking it here would be the sbcli-backed preflight rejected earlier in this design's own history. This step verifies only that Ramen's own two labels and one parameter are present, which is everything a same-cluster read can verify.
5. **Verdict.** If step 2 found an enrolled `StorageClass` and step 4 found no `VolumeReplicationClass` with a matching provisioner, or found one with a missing label or parameter, the preflight fails. Otherwise, it passes.

### 3.2 Where the result goes

One Kubernetes event per reconcile, on the `ReplicationPair` object driving the check, not a new CR and not a status field: the preflight is a diagnostic, and the `ReplicationPair`'s own status already carries the fields Ramen and the backend care about.

| Event reason          | Type    | When                                                                                                                                                                               |
|-----------------------|---------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `PeerClassesVerified` | Normal  | An enrolled `StorageClass` exists and every matching `VolumeReplicationClass` carries both the label and the parameter.                                                            |
| `PeerClassesMismatch` | Warning | An enrolled `StorageClass` exists but no matching `VolumeReplicationClass` does, or one is missing the label or the parameter. The message names the missing field and the object. |

`ReplicationPairReconciler` gains a `Recorder events.EventRecorder` field, following the exact pattern `backuppolicy_controller.go` already uses (`r.Recorder.Eventf(object, nil, eventType, reason, reason, format, args...)`), wired in `operator/cmd/main.go` as `mgr.GetEventRecorder("replicationpair-controller")`.

### 3.3 RBAC

Two markers, both read-only, added to the reconciler's existing block:

```go
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumereplicationclasses,verbs=get;list;watch
```

The second is this repository's second grant on an external CRD group it does not own, alongside `simplyblockstoragenodeset_controller.go`'s existing `cert-manager.io/certificates` marker, the precedent `rbac-hardening` cites for exactly this shape of grant.

### 3.4 What this is not

This is not the cross-cluster preflight the first pass at this design attempted and the second pass's user correction rejected: it makes no call to the simplyblock backend, resolves no `ReplicationPolicy` by name, and reaches no peer cluster's API server. It is the narrowest thing that closes the gap Ramen's own hub cannot see: a `StorageClass`/`VolumeReplicationClass` pairing broken on one cluster, before any `DRPolicy` compares it to a peer.

---

## 4. VolumeGroupReplication

The gap analysis's own Phase 2 named this the piece consistency groups still owed: "Add `VolumeGroupReplication` (csi-addons) on top of the CG primitive so the VRG async group path can protect and fail over multi-volume apps at one point, not just snapshot them." `design-csi-addons-replication.md` §2 deferred it as "the next design," strictly per-volume itself. `design-consistency-groups.md` §2 deferred it too, as "future work," independent of any replication policy so that work could attach later. Neither claims it. This section is that attachment.

### 4.1 What already exists, upstream

`VolumeGroupReplication`, `VolumeGroupReplicationClass`, and `VolumeGroupReplicationContent` are shipped CRDs in the same `replication.storage.openshift.io` group as `VolumeReplication` and `VolumeReplicationClass`, from `kubernetes-csi-addons`. `VolumeGroupReplication.spec` carries the same three-state `replicationState` (`primary`/`secondary`/`resync`) the per-volume kind carries, a `source.selector` naming the member PVCs by label, and an `external` boolean: `false` routes reconciliation through the generic kubernetes-csi-addons controller-manager, and `true` hands it to "an external controller managed by the storage vendor." Ramen's own VRG (`RamenDR/ramen`'s `vrg_volgrouprep.go`) creates and drives `VolumeGroupReplication` objects directly, once a VRG's PVCs share a `VolumeGroupReplicationClass` carrying a `ramendr.openshift.io/groupreplicationid` label, the group-level sibling of the per-volume `replicationid` label §3.1 already checks. Ramen's own documentation names this case "offloaded" replication: a storage backend that replicates at the LUN or logical-volume-store level, outside Kubernetes, through a vendor controller, exactly the shape simplyblock's backend already has.

**Global VGR, RamenDR's newer multi-VRG consensus feature for a replication group spanning several applications' VRGs, is out of scope here.** This section covers the base case Ramen has supported longer: one VRG, one storage vendor's PVCs, one `VolumeGroupReplication`. Whether the base case is sufficient for simplyblock's own use, or Global VGR's cross-VRG consensus is eventually needed too, is §8 Open Question 5.

### 4.2 No new gRPC contract

Promoting, demoting, or resyncing a group is fanning the same three verbs `design-csi-addons-replication.md` §5 already ships (`PromoteVolume`, `DemoteVolume`, `ResyncVolume`) out to every member, not a fourth verb on the driver. The `replicationState` values line up one for one with the per-volume kind's, by design: `kubernetes-csi-addons`'s own generic controller reconciles a *non*-external `VolumeGroupReplication` this same way, fanning it out to member `VolumeReplication` objects it creates itself. Setting `external: true` does not change what the fan-out does. It changes who performs it, so that the vendor controller can skip the generic manager's own per-member `VolumeReplication` bookkeeping and go straight to whatever shape fits the backend. simplyblock's shape is already built: reuse the per-volume adapter through the same `VolumeReplication` objects Ramen already drives for a single volume, one per group member.

### 4.3 The reconciler

A new `VolumeGroupReplicationReconciler`, alongside `ReplicationPairReconciler` and the rest, owns every `VolumeGroupReplication` whose `spec.external` is `true` and whose `spec.volumeGroupReplicationClassName` names a class with `provisioner: csi.simplyblock.io`:

1. **Resolve membership.** Read `spec.source.selector` against this cluster's PVCs. Reuse the exact invariant `design-consistency-groups.md` §9.2 already established for `VolumeGroupSnapshot`: the selected set must equal a `storage.simplyblock.io/consistency-group` value's current membership exactly, not merely a subset or superset of it. A selector that does not resolve to one whole group is a configuration error, not a partial group to serve.
2. **Fan out.** For each member PVC, ensure a per-volume `VolumeReplication` object exists, owned by the `VolumeGroupReplication`, named deterministically from the group and the member, with `spec.replicationState` mirroring the group's. The already-shipped `kubernetes-csi-addons` controller-manager reconciles each of these exactly as it does any Ramen-created per-volume `VolumeReplication` (`design-csi-addons-replication.md` §5): this reconciler creates and updates the member objects, and never calls the driver's Replication gRPC itself.
3. **Fan in.** Aggregate every member's `VolumeReplication.status.conditions` into the group's own status: `Completed` is the conjunction across all members, `Degraded` and `Resyncing` are the disjunction (one degraded or resyncing member makes the group so). `status.lastGroupSyncTime`, the field Appendix A.4's group-readiness status query names, is the oldest of the members' `lastSyncTime`: a group's recovery point is only as fresh as its slowest member.

### 4.4 Admission webhook, extended

`design-consistency-groups.md` §9.4's validating webhook on `VolumeGroupSnapshot` create already enforces "the selector must equal the group's current membership" at `kubectl apply`, with the same fail-closed label check and fail-open backend check. A sibling webhook on `VolumeGroupReplication` create makes the identical two checks against the identical label, so a `VolumeGroupReplication` that could never resolve to one whole group is rejected before the reconciler in §4.3 ever sees it, rather than sitting unreconciled.

### 4.5 RBAC and events

`VolumeGroupReplicationReconciler` needs read access to `persistentvolumeclaims` (already granted elsewhere in this operator) and read-write access to the external `volumegroupreplications.replication.storage.openshift.io` and the per-member `volumereplications.replication.storage.openshift.io` it creates:

```go
// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumegroupreplications,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumegroupreplications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumereplications,verbs=get;list;watch;create;update;patch;delete
```

Events follow §3.2's pattern, on the `VolumeGroupReplication` object: `GroupReplicationVerified`/`GroupMembershipMismatch` at the webhook's own admission-time checks, mirrored as reconcile-time events for the case the webhook admitted open, and a `GroupReplicationDegraded` (Warning) when §4.3's fan-in first observes a member `Degraded` after previously reporting none.

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

0. **Preflight.** Before the `DRPolicy` is authored, confirm §3's preflight passes against both clusters' own `StorageClass`/`VolumeReplicationClass` objects: a `PeerClassesVerified` event on each cluster's `ReplicationPair`, not a `PeerClassesMismatch`.
1. **Protect.** Deploy a workload with a PVC on cluster A, under a `StorageClass`/`VolumeReplicationClass` pair carrying the Ramen labels. Create the `DRPlacementControl`. Confirm Ramen's VRG creates one `VolumeReplication` for the PVC, `EnableVolumeReplication` fires, and `status.lastSyncTime` advances on the policy's ordinary cadence.
2. **Planned relocate.** Trigger Ramen's `Relocate` action. Confirm the sequence design-csi-addons-replication.md §5.2 documents drives correctly through Ramen rather than by hand: the source-side VRG demotes (fence, final flush, `Completed` settles once confirmed), then the target-side VRG promotes with `force=false`, gated on `PeerReady` from step 1's demote. Confirm the workload comes up on cluster B with the data a hashed writer wrote before relocation, and that Ramen's own `PeerReady`/`DataProtected` aggregation reports correctly throughout, not just this design's own conditions in isolation.
3. **Unplanned failover.** Simulate cluster B's loss (or reachability loss, matching this session's `replication_source_online` distinction) and trigger Ramen's `Failover` action against cluster A. Confirm the force-escalation path Ramen's own controller drives (§5.2's "no wait-and-retry grace period" behavior) still lands correctly when Ramen, not a test script, is the one issuing the calls.
4. **Resync.** Recover the failed cluster and confirm Ramen drives `ResyncVolume` to reconcile the diverged copy, without merging or re-triggering a full cutover.

### 6.3 Pass/fail criteria

Every step's pass criterion is an observable Ramen already reports on its own objects (`VolumeReplication.status.conditions`, the VRG's `DataProtected`, the DRPC's `PeerReady`), not a simplyblock-specific read: the point of this validation is that Ramen's own surface reflects reality, not that a side-channel confirms it. Data correctness is checked with a hashed writer across every promote, matching `design-csi-addons-replication.md` §12's existing E2E discipline.

---

## 7. Testing Strategy

Three different classes of coverage, for the three different things this document specifies:

- **§3's preflight is unit-tested**, with the fake client and event recorder `replicationpair_controller_unit_test.go` already uses for the rest of `ReplicationPairReconciler`. An enrolled `StorageClass` with a matching, complete `VolumeReplicationClass` yields `PeerClassesVerified`. One with a missing label, a missing parameter, or no matching class at all yields `PeerClassesMismatch`, and no enrolled `StorageClass` yields no event at all. No `envtest` is needed, since the fake client registers the unstructured `VolumeReplicationClass` GVK the same way it does any typed kind.
- **§4's `VolumeGroupReplicationReconciler` is unit-tested against a fake client the same way**, standing in a `ConsistencyGroup`-labeled set of PVCs and asserting the member `VolumeReplication` fan-out and the status fan-in: a group whose members all report `Completed` yields a `Completed` group, any one member `Degraded` yields a `Degraded` group, and `status.lastGroupSyncTime` is the oldest member `lastSyncTime`, not the newest. The admission webhook (§4.4) is unit-tested the same way `design-consistency-groups.md` §9.4's tests already exercise its `VolumeGroupSnapshot` sibling: a selector matching a whole group admitted, one matching a subset or spanning two groups rejected.
- **§5 confirms existing code** and adds nothing to test on its own. **§6 is exclusively E2E, live-cluster validation** with no smaller harness to substitute, including its step 0 confirmation that §3's preflight passed before the `DRPolicy` was authored, and a group-protect scenario confirming §4's reconciler under a real VRG's `VolumeGroupReplication`.

Full scenario detail: [`tests/test-plan-ramen-integration.md`](../tests/test-plan-ramen-integration.md). Its M-01 and M-02 close `design-csi-addons-replication.md` test plan's E-06 and E-07, which have carried no implementing test since they were written.

---

## 8. Open Questions

| #   | Question                                                                                                                                                                                                                                                                                                                                                 | Owner                 |
|-----|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------|
| 1   | **Is a real OCM hub with Ramen already available for this validation**, or does P0-1/P0-2 need to be stood up from scratch? The answer decides whether §6 is schedulable now or needs its own infrastructure work first.                                                                                                                                 | Operator team / Infra |
| 2   | **Does SiteMap exist in a runnable form yet**, or does every run of §6 use a hand-authored `DRPlacementControl` stand-in (P0-3)? If SiteMap is not yet runnable, note that explicitly rather than blocking on it indefinitely.                                                                                                                           | Operator team         |
| 3   | **`csi-addons/spec` version floor (P0-4).** Confirm whether a newer pinned version already carries `lastSyncBytes`/`lastSyncDuration` before treating this as a real upstream gap to track.                                                                                                                                                              | Operator team         |
| 4   | **Event volume on a large fleet.** `PeerClassesVerified` fires every 60-second reconcile once the preflight passes, on every `ReplicationPair`. Confirm this matches the existing event-rate expectations `rbac-hardening`'s workload review sets, or whether the verified case should log rather than emit an event, firing only once per state change. | Operator team         |
| 5   | **Is Global VGR needed.** §4.1 scopes `VolumeGroupReplication` to the base, single-VRG case. Confirm whether any planned simplyblock deployment spans a replication group across more than one application's VRG before treating RamenDR's multi-VRG consensus feature as work this document should also specify.                                        | Operator team         |
| 6   | **`VolumeGroupReplicationContent`'s exact contract.** §4.3 specifies the reconciler against `VolumeGroupReplication.spec`/`.status` only. Confirm what, if anything, this reconciler must also write to `VolumeGroupReplicationContent` before implementation starts, against the CRD's real schema rather than this document's reading of it.           | Operator team         |
