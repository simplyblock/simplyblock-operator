# Test Plan: Ramen Integration

Related design: [`designs/design-ramen-integration.md`](../designs/design-ramen-integration.md)
Harness: one class. Everything this document specifies needs a live two-cluster simplyblock deployment (`regression_test/21/`), plus an OCM hub with Ramen installed on the hub and both managed clusters (design §6.1). `VolumeGroupReplication` (design §4) is now a driver-and-backend feature specified in `design-csi-addons-replication.md` §14; its unit and backend scenarios live in that document's test plan (U-62 … U-69), not here.

Scope: this document specifies whether a real Ramen `VolumeReplicationGroup`, driven through a real OCM hub, correctly drives the csi-addons surface `design-csi-addons-replication.md` implements, for both a single volume and a consistency group of them, as manual E2E scenarios (`M-`), since no smaller harness substitutes for a real Ramen reconcile loop against a real OCM-registered cluster pair. The manual scenarios close two rows already carried in [`test-plan-csi-addons-replication.md`](test-plan-csi-addons-replication.md): E-06 and E-07, both `—` in that plan's `Test` column since they were written. Design §3 (peerClasses) adds no scenarios of its own: it confirms that no operator-side code exists to test.

---

## 1. Unit Scenarios

### VolumeGroupReplication (design §4)

`VolumeGroupReplication` is no longer an operator reconciler; it is the driver-and-backend, `external: false` path of `design-csi-addons-replication.md` §14. Its unit and backend scenarios are that document's test plan U-62 … U-69, and its live group relocate is that plan's E-08 (which points back to M-05 here). This plan owns no unit scenario for it.

U-01 … U-05 are **retired**: they covered the operator-owned `VolumeGroupReplicationReconciler`/`VolumeGroupReplicationValidator`, which `design-csi-addons-replication.md` §13 removes.

| #        | Scenario                                                                                | Type | Test |
|----------|-----------------------------------------------------------------------------------------|------|------|
| ~~U-01~~ | **Retired** (operator fan-in, all members healthy; reconciler removed)                  | —    | —    |
| ~~U-02~~ | **Retired** (operator fan-in, one member degraded; reconciler removed)                  | —    | —    |
| ~~U-03~~ | **Retired** (operator oldest-`lastSyncTime`; reconciler removed)                        | —    | —    |
| ~~U-04~~ | **Retired** (operator webhook admits a whole group; validator removed)                  | —    | —    |
| ~~U-05~~ | **Retired** (operator webhook rejects a subset/cross-group selector; validator removed) | —    | —    |

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

**Design reference:** design §4 and `design-csi-addons-replication.md` §14 (the driver-and-backend group surface), exercised through the topology and test flow §6 defines for the per-volume case. New scope this document adds; it closes `test-plan-csi-addons-replication.md` E-08. It cannot run until Phase 4 (that plan's U-62 … U-69) is built.

**What to verify:** a VRG whose PVCs share the consistency-group labels and a `StorageClass` carrying `ramendr.openshift.io/groupreplicationid` (and **not** `offloaded`) creates one `VolumeGroupReplication` with `spec.external: false`; the stock kubernetes-csi-addons controller-manager forms the backend group through the driver's `CreateVolumeGroup`, replicates it as one unit addressed by the group handle, and a planned relocate of the whole app moves every member together at one crash-consistent point, none diverging.

**Test concept:**
1. Extend M-01's topology: a workload with three PVCs sharing one `storage.simplyblock.io/consistency-group` value (and Ramen's own `ramendr.openshift.io/consistency-group`), under a group `StorageClass` carrying `groupreplicationid` and a `VolumeGroupReplicationClass` matching it.
2. Confirm exactly one `VolumeGroupReplication` (`external: false`) and its `VolumeGroupReplicationContent` exist, that the backend consistency group holds all three members, and that the group is replicating (the group `lastSyncTime` advances).
3. Trigger Ramen's `Relocate` action, as in M-02.
4. Assert: the whole group promotes on cluster B and demotes on cluster A as one unit, each member's PVC restored and serving on cluster B, every member's pre-relocate data intact, and the group's `lastSyncTime` tracking one group generation throughout. No member is left behind mid-relocate.

---

## 3. Axis Coverage

| Axis                             | Values covered                                                             | IDs                                                    | Not covered                                                                                                                                          |
|----------------------------------|----------------------------------------------------------------------------|--------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------|
| Group replication (unit/backend) | the driver VolumeGroup service and the backend group engine                | (in `test-plan-csi-addons-replication.md` U-62 … U-69) | owned by the csi-addons plan now that group replication is driver-and-backend (design §4), not this plan                                             |
| Orchestrator                     | Ramen VRG async, hub-driven                                                | M-01 … M-05                                            | direct `kubectl` lifecycle (already covered in `test-plan-csi-addons-replication.md`)                                                                |
| Cluster topology                 | two managed clusters, one relationship, hub-mediated                       | M-01 … M-05                                            | three-cluster (cascaded) topologies. SiteMap-authored `DRPlacementControl` specifically (§8 Open Question 2 may leave this a hand-authored stand-in) |
| Failure mode                     | planned relocate, unplanned failover, post-recovery resync, group relocate | M-02, M-03, M-04, M-05                                 | a demote that stalls mid-convergence while Ramen-driven (covered by hand in `test-plan-csi-addons-replication.md` M-01/M-02, not yet by Ramen)       |

---

## 4. Coverage Summary

| Class        | Scenarios | Covered | Not covered                                                                                                 |
|--------------|-----------|---------|-------------------------------------------------------------------------------------------------------------|
| Unit         | 0         | 0       | — (U-01 … U-05 retired; group unit/backend scenarios are `test-plan-csi-addons-replication.md` U-62 … U-69) |
| Manual (E2E) | 5         | 0       | M-01 … M-05                                                                                                 |

---

## 5. What Is Not Yet Covered

| #           | Gap                                                               | Reason                                                                                                                                                                                                                                                                                                                                                                                     |
|-------------|-------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| M-01 … M-05 | The entire Ramen-driven validation                                | Blocked on Phase 0 (design §Phase 0): a live OCM hub with Ramen installed across a registered two-cluster pair, not yet confirmed available (§8 Open Question 1). M-05 additionally needs Phase 4 built (`test-plan-csi-addons-replication.md` U-62 … U-69: the driver VolumeGroup service and the backend group engine) and the `VolumeGroupReplication` CRDs installed on both clusters. |
| —           | Three-cluster / cascaded topologies                               | Out of scope for this document, and not part of the gap analysis's Appendix A either                                                                                                                                                                                                                                                                                                       |
| —           | SiteMap-authored (rather than hand-authored) `DRPlacementControl` | Depends on SiteMap's own availability (§8 Open Question 2)                                                                                                                                                                                                                                                                                                                                 |
| —           | Global VGR (multi-VRG consensus)                                  | Out of scope for design §4.1, tracked as design §8 Open Question 4                                                                                                                                                                                                                                                                                                                         |
