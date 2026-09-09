# Test Plan: Consistency Groups

Related design: [`designs/design-consistency-groups.md`](../designs/design-consistency-groups.md)
Harness: [`csi-driver/internal/csi/node`](../../../csi-driver/internal/csi/node) and [`test/`](../../../test)

Scope is the CSI driver and the Kubernetes surface this repository builds. The control plane (`sbcli`) and SPDK are dependencies, faked at the boundary: a row asserts this driver's response to a backend answer, never the backend's own group logic. The membership epoch math (`included_in_seq`), the group-scoped snapshot listing, and `bdev_lvol_snapshot_group` atomicity are the control plane's and SPDK's to prove, and their coverage lives with the `sbcli` regression suite. Rows that need them assert this repository's behavior at the boundary.

Scenario IDs are permanent and are never reused or renumbered. `U-` is unit (no cluster, pure functions, a fake `client.Client`, a mock HTTP backend), `I-` is integration (full reconcile or sidecar loop against `envtest` and a mock backend), `E-` is end-to-end (a live cluster and the real data path), and `M-` is manual (needs failure injection or orchestration not automated yet). Types are `Positive`, `Negative`, `Boundary`, and `Regression`. A `—` in the `Test` column means nothing implements the scenario yet, and every such row reappears in §7 with its reason.

The whole design is Draft, so every row is currently `—`. The plan is the target coverage, and the `Test` columns fill in as the work lands.

---

## 1. Unit Tests

Pure functions and single calls against a fake client, with the control plane replaced by a mock HTTP server. No Kubernetes API server is involved. Numbering runs continuously across the groups.

### Provisioner Label Handling (§4.1)

File: `csi-driver/internal/csi/controller/controller_unit_test.go`

| #    | Scenario                                                                                      | Type     | Test |
|------|-----------------------------------------------------------------------------------------------|----------|------|
| U-01 | PVC carries the consistency-group label: `consistency_group` is set on the volume-create body | Positive | —    |
| U-02 | PVC has no label: `consistency_group` is absent, create is unchanged                          | Negative | —    |
| U-03 | Label present but empty value: rejected as an invalid group name, create fails cleanly        | Boundary | —    |

### CSI GroupController (§9)

File: `csi-driver/internal/csi/controller/groupsnapshot_unit_test.go`

| #    | Scenario                                                                                                         | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------|----------|------|
| U-04 | Handle set equals the group membership: the take-generation call is made, `group_snapshot_id` is `{gid}:{seq}`   | Positive | —    |
| U-05 | Handle set differs from the membership (extra handle): `FAILED_PRECONDITION`, no backend take call               | Negative | —    |
| U-06 | Handle set differs from the membership (missing handle): `FAILED_PRECONDITION`, no backend take call             | Negative | —    |
| U-07 | Handles resolve to two different groups: `FAILED_PRECONDITION`                                                   | Negative | —    |
| U-08 | `CreateVolumeGroupSnapshot` retried with the same external name: returns the existing generation, no second take | Boundary | —    |
| U-09 | `DeleteVolumeGroupSnapshot` for a group that no longer exists: returns success                                   | Boundary | —    |
| U-10 | `GetVolumeGroupSnapshot` maps to the group-scoped generation read and returns per-member handles                 | Positive | —    |
| U-11 | Advertised capabilities include `GROUP_CONTROLLER_SERVICE` and `CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT`         | Positive | —    |

---

## 2. Integration Tests

The CSI GroupController and the snapshot-controller against a mock backend HTTP server and a real Kubernetes API via `envtest`, with the group feature gate enabled.

### VolumeGroupSnapshot Lifecycle (§5.3, §6.4)

File: `csi-driver/internal/csi/controller/groupsnapshot_test.go`

| #    | Scenario                                                                                                                                                                               | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | Create a `VolumeGroupSnapshot` over a three-member group: one generation is taken, three `VolumeSnapshot` objects are materialized, each backref'd by `status.volumeGroupSnapshotName` | Positive | —    |
| I-02 | The `VolumeGroupSnapshotContent` carries `groupSnapshotHandle` `{gid}:{seq}` and a handle pair per member                                                                              | Positive | —    |
| I-03 | A selector that resolves to a set differing from the membership: the `VolumeGroupSnapshot` reports `FAILED_PRECONDITION`, no generation is taken                                       | Negative | —    |
| I-04 | Delete the `VolumeGroupSnapshot`: the generation and its member snapshots are deleted, the group is not                                                                                | Positive | —    |
| I-05 | Delete a `VolumeGroupSnapshot` whose group was already deleted with its last member: delete returns success                                                                            | Boundary | —    |
| I-06 | Backend 5xx during the take: the `VolumeGroupSnapshot` reports not-ready, recovers when the backend returns                                                                            | Negative | —    |

---

## 3. E2E Tests

Against a live simplyblock cluster with real fio workloads. The cross-volume correctness rows assert data coherence with hashes, not merely that I/O continued.

### Group Membership and Placement (§4.1, §4.2)

| #    | Scenario                                                                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | First labeled PVC creates the group and pins the node, and a second labeled PVC lands on the same node and store              | Positive | —    |
| E-02 | A labeled PVC that cannot colocate on the pinned node: the PVC stays Pending, the backend error is surfaced, no unpinned join | Negative | —    |
| E-03 | A label added to a PVC after its volume exists does not join the volume to the group                                          | Negative | —    |

### Cross-Volume Consistency (§5, §7)

| #    | Scenario                                                                                                                                                                   | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-04 | Hashed round-robin writer across all members, take a generation under load, clone every member from that generation: the group prefix property holds and all hashes verify | Positive | —    |
| E-05 | Negative control: clone members from a mix of two generations, the cross-volume check detects the inconsistency                                                            | Negative | —    |
| E-06 | Clone a generation into labeled PVCs: the clones form a new group pinned on one node and are mutually consistent                                                           | Positive | —    |
| E-07 | Clone a generation into unlabeled PVCs: the clones are mutually consistent and are not a group                                                                             | Positive | —    |

### Membership Changes and Representation (§6, §8)

| #    | Scenario                                                                                                              | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------|----------|------|
| E-08 | Detach a member after generation N: generation N still lists it and restores complete, generation N+1 excludes it     | Positive | —    |
| E-09 | `snapshot list --consistency-group` shows the group and generation for every member snapshot, no name parsing needed  | Positive | —    |
| E-10 | The group-scoped listing reports expected-versus-present member counts, and flags an incomplete generation            | Boundary | —    |
| E-11 | Delete the last member: the group and its remaining generations are removed, and a backend event records the widening | Positive | —    |

---

## 4. E2E — Phase 2 gating

The Phase 2 rows (I-01 … I-06, E-04 … E-11 through the `VolumeGroupSnapshot` path) are testable only once P0-4 enables the `CSIVolumeGroupSnapshot` feature gate and the CSI GroupController ships. Until then, E-01 … E-03 and the membership rows are exercisable through the backend group and `sbctl`.

---

## 5. Manual Scenarios and Test Concepts

### M-01 — Deleting a member volume must preserve its group snapshots

**Design reference:** §8.2, Open Question 5

**What to verify:** the one data-loss path the design forbids. Deleting a member volume must not delete the group snapshots that prior generations depend on, so a prior generation stays complete and restorable.

**Test concept:**
1. Provision a three-member group, take generation 4.
2. Delete one member volume outright (not a detach).
3. Query the group-scoped listing: generation 4 must still report expected three, present three.
4. Clone generation 4: it must produce three volumes, the deleted member's data among them, hash-verified against what was written before the delete.

### M-02 — A member migrated off the pinned store

**Design reference:** §8.4, Open Question 2

**What to verify:** a group snapshot with a member off the pinned store fails loudly rather than snapshotting the members still on the store and calling it a generation.

**Test concept:**
1. Provision a three-member group, migrate one member to another node.
2. Take a group snapshot.
3. Assert it fails with a clear error and takes no generation, rather than producing a two-member generation.

---

## 6. Axis Coverage

| Axis                       | Values covered                                                         | IDs                     | Not covered                          |
|----------------------------|------------------------------------------------------------------------|-------------------------|--------------------------------------|
| Cluster topology           | 1 node, multi-node with a pinned group                                 | E-01, E-02              | asymmetric node sizes                |
| Group size                 | 1 member, 3+ members                                                   | E-01, E-04              | very large groups (subsystem slots)  |
| Membership change          | join at create, one-way detach, death with last member                 | E-01, E-03, E-08, E-11  | re-establish via a labeled clone     |
| Selector versus membership | equal, extra handle, missing handle, two groups                        | U-04 … U-07, I-03       | —                                    |
| Snapshot lifecycle         | take, get, delete, retry, delete-after-group-gone                      | U-04 … U-10, I-04, I-05 | —                                    |
| Representation             | per-snapshot group fields, group-scoped listing, incomplete generation | E-09, E-10              | listing under very many generations  |
| Data correctness           | consistent clone, negative control, delete-preserves                   | E-04, E-05, M-01        | migration mid-snapshot (M-02 manual) |

---

## 7. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 11        | 0       | U-01 … U-11 |
| Integration | 6         | 0       | I-01 … I-06 |
| E2E         | 11        | 0       | E-01 … E-11 |
| Manual      | 2         | 0       | M-01, M-02  |

Every scenario is uncovered because the feature is Draft. The counts are the target, and each `Test` column fills in as the work lands.

---

## 8. What Is Not Yet Covered

| #           | Gap                                                                             | Reason                                                                                                         |
|-------------|---------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------|
| U-01 … U-11 | Provisioner label handling and the CSI GroupController                          | Phase 1 and Phase 2 not implemented: the provisioner field and the group service do not exist yet              |
| I-01 … I-06 | The `VolumeGroupSnapshot` lifecycle under `envtest`                             | Depends on the CSI GroupController and the `CSIVolumeGroupSnapshot` feature gate (P0-4)                        |
| E-01 … E-11 | Membership, placement, cross-volume consistency, clone, and representation live | Depends on the standalone backend group (P0-2, P0-3), which is not shipped                                     |
| M-01        | Deleting a member preserves its group snapshots                                 | The one data-loss path (§8.2); needs the standalone delete path and the group-scoped listing to assert against |
| M-02        | A member migrated off the pinned store                                          | Needs migration orchestration and the group-snapshot failure path (Open Question 2)                            |
| —           | Asymmetric node sizes, very large groups, listing under many generations        | Beyond the first coverage pass, recorded so the gap is explicit rather than assumed covered                    |
