# Test Plan: Ramen Integration

Related design: [`designs/design-ramen-integration.md`](../designs/design-ramen-integration.md)
Harness: two classes. `VolumeGroupReplicationReconciler` (design §4) is unit-tested against a fake-client harness, the same shape `replicationpair_controller_unit_test.go` already uses elsewhere in this package. Everything else needs a live two-cluster simplyblock deployment (`regression_test/21/`), plus an OCM hub with Ramen installed on the hub and both managed clusters (design §6.1).

Scope: two things this document specifies. First, whether `VolumeGroupReplicationReconciler` (design §4) correctly fans a group's replication state out to its members and their status back in, and correctly validates group membership at admission, as unit scenarios (`U-01` … `U-05`). Second, whether a real Ramen `VolumeReplicationGroup`, driven through a real OCM hub, correctly drives the csi-addons adapter `design-csi-addons-replication.md` implements, for both a single volume and a consistency-group of them, as manual E2E scenarios (`M-`), since no smaller harness substitutes for a real Ramen reconcile loop against a real OCM-registered cluster pair. The manual scenarios close two rows already carried in [`test-plan-csi-addons-replication.md`](test-plan-csi-addons-replication.md): E-06 and E-07, both `—` in that plan's `Test` column since they were written. Design §3 (peerClasses) adds no scenarios of its own: it confirms that no operator-side code exists to test.

---

## 1. Unit Scenarios

### VolumeGroupReplicationReconciler and its admission webhook (design §4)

Implemented in `volumegroupreplication_controller.go` and `volumegroupreplication_validator.go`, covered by `volumegroupreplication_controller_unit_test.go` and `volumegroupreplication_validator_test.go`.

| #    | Scenario                                                                                                          | Type     | Test                                                              |
|------|-------------------------------------------------------------------------------------------------------------------|----------|-------------------------------------------------------------------|
| U-01 | Every member's `VolumeReplication` reports `Completed=True, Degraded=False`, and the group reports the same       | Positive | `TestVolumeGroupReplication_AllMembersHealthyYieldsGroupHealthy`  |
| U-02 | One member's `VolumeReplication` reports `Degraded=True`, and the group reports `Degraded=True`                   | Negative | `TestVolumeGroupReplication_OneMemberDegradedYieldsGroupDegraded` |
| U-03 | Members report differing `lastSyncTime`, and `status.lastSyncTime` is the oldest, not the newest                  | Boundary | `TestVolumeGroupReplication_LastSyncTimeIsTheOldestMember`        |
| U-04 | `spec.source.selector` resolves to exactly one `ConsistencyGroup`'s current membership, and is admitted           | Positive | `TestVolumeGroupReplicationValidator`                             |
| U-05 | `spec.source.selector` resolves to a subset of a group, or spans two groups, and is rejected, naming the mismatch | Negative | `TestVolumeGroupReplicationValidator`                             |

---

## 2. Manual Scenarios and Test Concepts

### M-01: Ramen protects a workload, and `VolumeReplication` reflects it

**Design reference:** design §6.2 step 1. Closes `test-plan-csi-addons-replication.md` E-06.

**What to verify:** A Ramen `VolumeReplicationGroup` in async mode, given a `DRPlacementControl` protecting a namespace, selects the `VolumeReplicationClass` by its `replicationClassSelector` and `provisioner`, creates exactly one `VolumeReplication` per protected PVC, and that object's `status.lastSyncTime` advances on the policy's ordinary cadence, not just when driven by hand (already proven, `design-csi-addons-replication.md` §12), but when Ramen itself is the caller.

**Open question:** whether SiteMap authors the `DRPlacementControl` or a hand-authored stand-in does (design §8, Open Question 2). Record which was used.

**Test concept:**
1. Stand up the topology in design §6.1: two managed clusters, one shared simplyblock control plane, an OCM hub with Ramen installed, a `DRPolicy` naming both clusters.
2. Deploy a workload with a PVC on cluster A under a labeled `StorageClass`/`VolumeReplicationClass` pair (`ramendr.openshift.io/storageid`/`replicationid`, `design-csi-addons-replication.md` §7.1).
3. Create the `DRPlacementControl` protecting the workload's namespace.
4. Assert: exactly one `VolumeReplication` exists, named and owned per Ramen's own convention. Its `status.state` reaches `Primary`. `status.lastSyncTime` is non-nil and advances across at least two policy intervals. The VRG's `DataProtected` condition is `True`.

### M-02: Ramen planned relocate, demote then promote, driven by the VRG

**Design reference:** design §6.2 step 2. Closes `test-plan-csi-addons-replication.md` E-07 (relocate half).

**What to verify:** Ramen's `Relocate` action demotes the source VRG and promotes the target VRG with `force=false`, gated on the source's `PeerReady`, and the workload comes up on the target cluster with the data a hashed writer wrote before relocation: zero loss, the same guarantee `design-csi-addons-replication.md` §5.2 already proves by hand, now proven through Ramen's own reconcile loop.

**Current behavior:** the demote → planned-promote sequence, the `Aborted`/`FailedPrecondition` split that keeps a converging demote from being force-escalated, and the source-health no-op on an already-settled promote are all implemented and unit/E2E-tested against a hand-driven `VolumeReplication` (`design-csi-addons-replication.md` §5.2, §12). Never yet exercised by Ramen's own controller issuing the calls.

**Test concept:**
1. From M-01's protected state, write and hash a known data set to the workload's volume.
2. Trigger Ramen's `Relocate` action toward cluster B.
3. Assert, in order: cluster A's `VolumeReplication` reaches `Secondary` with `Completed=True` (demote confirmed). Cluster B's `VolumeReplication` reaches `Primary` with `Completed=True` and no force-promotion event fired (the planned path, not an escalation). The workload is schedulable and serving on cluster B. The hash taken before relocation matches the data read after.

### M-03: Ramen unplanned failover, force promote, driven by the VRG

**Design reference:** design §6.2 step 3. Closes `test-plan-csi-addons-replication.md` E-07 (failover half).

**What to verify:** with cluster A unreachable (not merely demoted), Ramen's `Failover` action promotes cluster B with `force=true`, and the force-escalation behavior `design-csi-addons-replication.md` §5.2 documents (the vendored controller's own "no wait-and-retry grace period") still lands correctly when Ramen, not a test script, drives it.

**Test concept:**
1. From a protected, healthy state (M-01), make cluster A's storage genuinely unreachable (network partition or node shutdown, not merely a demote).
2. Trigger Ramen's `Failover` action toward cluster B.
3. Assert: cluster B's `VolumeReplication` reaches `Primary`. The promote succeeded via the forced path (a force-promotion event is expected here, unlike M-02). The workload serves on cluster B.

### M-04: Ramen-driven resync after recovery

**Design reference:** design §6.2 step 4 (new scope this document adds, with no corresponding row in `test-plan-csi-addons-replication.md`).

**What to verify:** once cluster A recovers after M-03's failover, Ramen reconciles the diverged copy via `ResyncVolume` without merging or re-triggering a full cutover, matching `design-csi-addons-replication.md` §5.2's "it never merges" guarantee.

**Test concept:**
1. Restore cluster A's connectivity/node after M-03.
2. Confirm Ramen (not a manual `kubectl patch`) drives the recovered volume's `VolumeReplication` toward `Resyncing=True`, then `Completed=True` once caught up.
3. Assert the resync reconciled by delta, not a full copy (compare shipped bytes against the volume's total size), and that cluster B remains primary throughout.

### M-05: Ramen protects and relocates a multi-volume app through `VolumeGroupReplication`

**Design reference:** design §4, exercised through the topology and test flow §6 defines for the per-volume case. New scope this document adds, with no corresponding row in `test-plan-csi-addons-replication.md`.

**What to verify:** a VRG whose PVCs share a `storage.simplyblock.io/consistency-group` label and a `VolumeGroupReplicationClass` carrying `ramendr.openshift.io/groupreplicationid` creates one `VolumeGroupReplication`, `VolumeGroupReplicationReconciler` (design §4.3) fans it out to one `VolumeReplication` per member and fans member status back into the group, and a planned relocate of the whole app moves every member together with none diverging.

**Test concept:**
1. Extend M-01's topology: a workload with three PVCs sharing one `storage.simplyblock.io/consistency-group` value, protected by one VRG under a `VolumeGroupReplicationClass` naming that group's `ReplicationPolicy`.
2. Confirm exactly one `VolumeGroupReplication` exists and exactly three member `VolumeReplication` objects exist, each owned by it.
3. Trigger Ramen's `Relocate` action, as in M-02.
4. Assert: all three members reach `Secondary` on cluster A and `Primary` on cluster B together, not staggered. The group's own `status.lastSyncTime` reflects the oldest member's throughout (U-03). No member is left behind mid-relocate.

---

## 3. Axis Coverage

| Axis                          | Values covered                                                             | IDs                    | Not covered                                                                                                                                                                           |
|-------------------------------|----------------------------------------------------------------------------|------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Group replication aggregation | all members healthy, one member degraded, differing `lastSyncTime`         | U-01 … U-03            | more than one degraded member simultaneously (not a gap: the aggregation is a plain disjunction, one member already exercises it)                                                     |
| Group membership validation   | selector equals membership, selector is a subset or spans two groups       | U-04, U-05             | membership changing between admission and reconcile (the fail-open backend-unreachable case `design-consistency-groups.md` §9.4's sibling check already covers for the snapshot path) |
| Orchestrator                  | Ramen VRG async, hub-driven                                                | M-01 … M-05            | direct `kubectl` lifecycle (already covered in `test-plan-csi-addons-replication.md`)                                                                                                 |
| Cluster topology              | two managed clusters, one relationship, hub-mediated                       | M-01 … M-05            | three-cluster (cascaded) topologies. SiteMap-authored `DRPlacementControl` specifically (§8 Open Question 2 may leave this a hand-authored stand-in)                                  |
| Failure mode                  | planned relocate, unplanned failover, post-recovery resync, group relocate | M-02, M-03, M-04, M-05 | a demote that stalls mid-convergence while Ramen-driven (covered by hand in `test-plan-csi-addons-replication.md` M-01/M-02, not yet by Ramen)                                        |

---

## 4. Coverage Summary

| Class        | Scenarios | Covered         | Not covered |
|--------------|-----------|-----------------|-------------|
| Unit         | 5         | 5 (U-01 … U-05) | —           |
| Manual (E2E) | 5         | 0               | M-01 … M-05 |

---

## 5. What Is Not Yet Covered

| #           | Gap                                                               | Reason                                                                                                                                                                                                                                                  |
|-------------|-------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| M-01 … M-05 | The entire Ramen-driven validation                                | Blocked on Phase 0 (design §Phase 0): a live OCM hub with Ramen installed across a registered two-cluster pair, not yet confirmed available (§8 Open Question 1). M-05 additionally needs the `VolumeGroupReplication` CRDs installed on both clusters. |
| —           | Three-cluster / cascaded topologies                               | Out of scope for this document, and not part of the gap analysis's Appendix A either                                                                                                                                                                    |
| —           | SiteMap-authored (rather than hand-authored) `DRPlacementControl` | Depends on SiteMap's own availability (§8 Open Question 2)                                                                                                                                                                                              |
| —           | Global VGR (multi-VRG consensus)                                  | Out of scope for design §4.1, tracked as design §8 Open Question 4                                                                                                                                                                                      |
