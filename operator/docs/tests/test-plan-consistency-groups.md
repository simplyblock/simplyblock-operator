# Test Plan: Consistency Groups

Related design: [`designs/design-consistency-groups.md`](../designs/design-consistency-groups.md)
Harness: [`csi-driver/internal/csi/node`](../../../csi-driver/internal/csi/node) and [`test/`](../../../test)

Scope is the CSI driver and the Kubernetes surface this repository builds. The control plane (`sbcli`) and SPDK are dependencies, faked at the boundary: a row asserts this driver's response to a backend answer, never the backend's own group logic. The membership epoch math (`included_in_seq`), the group-scoped snapshot listing, and `bdev_lvol_snapshot_group` atomicity are the control plane's and SPDK's to prove, and their coverage lives with the `sbcli` regression suite. Rows that need them assert this repository's behavior at the boundary.

Scenario IDs are permanent and are never reused or renumbered. `U-` is unit (no cluster, pure functions, a fake `client.Client`, a mock HTTP backend), `I-` is integration (full reconcile or sidecar loop against `envtest` and a mock backend), `E-` is end-to-end (a live cluster and the real data path), and `M-` is manual (needs failure injection or orchestration not automated yet). Types are `Positive`, `Negative`, `Boundary`, and `Regression`. A `—` in the `Test` column means nothing implements the scenario yet, and every such row reappears in §7 with its reason.

The plan is the target coverage, and the `Test` columns fill in as the work lands. The unit rows are implemented except U-03 and U-08. The integration, E2E, and manual rows are still `—`.

---

## 1. Unit Tests

Pure functions and single calls against a fake client, with the control plane replaced by a mock HTTP server. No Kubernetes API server is involved. Numbering runs continuously across the groups.

### Provisioner Label Handling (§4.1)

File: `csi-driver/internal/csi/controller/provision_cg_test.go`

| #    | Scenario                                                                                      | Type     | Test                                            |
|------|-----------------------------------------------------------------------------------------------|----------|-------------------------------------------------|
| U-01 | PVC carries the consistency-group label: `consistency_group` is set on the volume-create body | Positive | `TestCreateVolume_SendsConsistencyGroupLabel`   |
| U-02 | PVC has no label: `consistency_group` is absent, create is unchanged                          | Negative | `TestCreateVolume_NoLabelOmitsConsistencyGroup` |
| U-03 | Label present but empty value: rejected as an invalid group name, create fails cleanly        | Boundary | —                                               |

### CSI GroupController (§9)

File: `csi-driver/internal/csi/controller/groupsnapshot_test.go`

| #    | Scenario                                                                                                                                                  | Type     | Test                                                     |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------|----------|----------------------------------------------------------|
| U-04 | Handle set equals the group membership: the take-generation call is made, `group_snapshot_id` is `{gid}:{seq}`                                            | Positive | `TestCreateVolumeGroupSnapshot_HandlesEqualMembership`   |
| U-05 | Handle set differs from the membership (extra handle): `FAILED_PRECONDITION`, no backend take call                                                        | Negative | `TestCreateVolumeGroupSnapshot_ExtraHandleIsRefused`     |
| U-06 | Handle set differs from the membership (missing handle): `FAILED_PRECONDITION`, no backend take call                                                      | Negative | `TestCreateVolumeGroupSnapshot_MissingHandleIsRefused`   |
| U-07 | Handles resolve to two different groups: `FAILED_PRECONDITION`                                                                                            | Negative | `TestCreateVolumeGroupSnapshot_TwoGroupsRefused`         |
| U-08 | `CreateVolumeGroupSnapshot` retried with the same external name: returns the existing generation, no second take                                          | Boundary | —                                                        |
| U-09 | `DeleteVolumeGroupSnapshot` for a group that no longer exists: returns success                                                                            | Boundary | `TestDeleteVolumeGroupSnapshot_MissingIsSuccess`         |
| U-10 | `GetVolumeGroupSnapshot` maps to the group-scoped generation read and returns per-member handles                                                          | Positive | `TestGetVolumeGroupSnapshot_ReadsGeneration`             |
| U-11 | Advertised capabilities include `GROUP_CONTROLLER_SERVICE` and `CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT`                                                  | Positive | `TestGroupControllerGetCapabilities`                     |
| U-20 | Backend take-generation returns a member-unhealthy precondition error: the `VolumeGroupSnapshot` is surfaced not-ready with the message, no partial state | Negative | `TestCreateVolumeGroupSnapshot_BackendTakeErrorSurfaces` |

### VolumeGroupSnapshot Admission Webhook (§9.4)

File: `operator/internal/webhook/volumegroupsnapshot_validator_test.go`

| #    | Scenario                                                                                                                                    | Type     | Test                               |
|------|---------------------------------------------------------------------------------------------------------------------------------------------|----------|------------------------------------|
| U-12 | Label check: selector resolves to PVCs that all carry the same consistency-group label, and the set equals the backend membership: admitted | Positive | `TestVolumeGroupSnapshotValidator` |
| U-13 | Label check: selector resolves to PVCs across two different group labels: rejected (fail-closed), message names the offending PVCs          | Negative | `TestVolumeGroupSnapshotValidator` |
| U-14 | Label check: selector matches a PVC with no consistency-group label: rejected (fail-closed)                                                 | Negative | `TestVolumeGroupSnapshotValidator` |
| U-15 | Label check: selector matches nothing: rejected                                                                                             | Boundary | `TestVolumeGroupSnapshotValidator` |
| U-16 | Membership check: selected set is missing a current member: rejected at admission                                                           | Negative | `TestVolumeGroupSnapshotValidator` |
| U-17 | Membership check: selected set includes a PVC labeled after creation (not a backend member): rejected at admission                          | Negative | `TestVolumeGroupSnapshotValidator` |
| U-18 | Membership check: backend unreachable: admitted (fail-open), GroupController backstops                                                      | Boundary | `TestVolumeGroupSnapshotValidator` |
| U-19 | Membership check: a selected PVC is not yet bound so its lvol is unknown: admitted (fail-open)                                              | Boundary | `TestVolumeGroupSnapshotValidator` |

### VolumeMigration Admission Webhook (§9.5)

File: `operator/internal/webhook/volumemigration_validator_test.go`

| #    | Scenario                                                                                                                         | Type     | Test                           |
|------|----------------------------------------------------------------------------------------------------------------------------------|----------|--------------------------------|
| U-21 | Target PV backs a consistency-group member: the create is rejected (fail-closed), the message names the volume and its group     | Negative | `TestVolumeMigrationValidator` |
| U-22 | Target PV backs a non-member volume: the create is admitted, migration proceeds                                                  | Positive | `TestVolumeMigrationValidator` |
| U-23 | Target PV backs a sibling in the same subsystem as a group member: the create is rejected, because the subsystem migrates as one | Negative | `TestVolumeMigrationValidator` |
| U-24 | Backend unreachable so membership cannot be determined: the create is admitted (fail-open), the backend refusal backstops        | Boundary | `TestVolumeMigrationValidator` |

### VolumeGroupSnapshotOps: Restore (design §7.4)

Files: `operator/internal/controller/volumegroupsnapshotops_controller_unit_test.go` and `operator/internal/webhook/volumegroupsnapshotops_validator_test.go`

| #    | Scenario                                                                                                                                               | Type     | Test                                                |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------|----------|-----------------------------------------------------|
| U-25 | Ready target with N member snapshots: one claim per member is created, each `dataSource` its member snapshot, named `<prefix>-<source PVC>`            | Positive | `TestGroupRestore_CreatesOneClaimPerMember`         |
| U-26 | `namePrefix` empty: restored claims are prefixed with the operation's own name                                                                         | Boundary | `TestGroupRestore_HonorsNamePrefix`                 |
| U-27 | `restore.consistencyGroup` set: every restored claim carries the membership label with that value                                                      | Positive | `TestGroupRestore_ConsistencyGroupLabel`            |
| U-28 | `restore.consistencyGroup` empty: restored claims carry no membership label                                                                            | Negative | `TestGroupRestore_ConsistencyGroupLabel`            |
| U-29 | Incomplete generation with `enablePartialRestore` off: the operation fails naming the missing members, and no claim is created                         | Negative | `TestGroupRestore_IncompleteGenerationFails`        |
| U-30 | Incomplete generation with `enablePartialRestore` on: the present members are restored, and `membersExpected` against `membersBound` reports the gap   | Boundary | `TestGroupRestore_PartialRestore`                   |
| U-31 | A derived claim name collides with an existing claim: the operation fails naming the claim, and claims already created are left in place               | Negative | `TestGroupRestore_ClaimCollisionFails`              |
| U-32 | Target exists but is not `ReadyToUse`: the operation holds in `Pending` with a `RestoreBlocked` event, creates no claim, and proceeds once it is ready | Boundary | `TestGroupRestore_WaitsForTargetReady`              |
| U-33 | Admission: a `volumeGroupSnapshotRef` that resolves is admitted, and one naming no `VolumeGroupSnapshot` in the namespace is rejected at create        | Negative | `TestVolumeGroupSnapshotOpsValidator_RefResolution` |

---

## 2. Integration Tests

The CSI GroupController and the snapshot-controller against a mock backend HTTP server and a real Kubernetes API via `envtest`, with the group feature gate enabled.

### VolumeGroupSnapshot Lifecycle (§5.3, §6.4)

File: `csi-driver/internal/csi/controller/groupsnapshot_test.go`

| #    | Scenario                                                                                                                                                                               | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | Create a `VolumeGroupSnapshot` over a three-member group: one generation is taken, three `VolumeSnapshot` objects are materialized, each backref'd by `status.volumeGroupSnapshotName` | Positive | —    |
| I-02 | After the take, `VolumeGroupSnapshotContent` `status.volumeGroupSnapshotHandle` is `{gid}:{seq}` (empty before) and `status.volumeSnapshotHandlePairList` has a pair per member        | Positive | —    |
| I-03 | A selector that resolves to a set differing from the membership: the `VolumeGroupSnapshot` reports `FAILED_PRECONDITION`, no generation is taken                                       | Negative | —    |
| I-04 | Delete the `VolumeGroupSnapshot`: the generation and its member snapshots are deleted, the group is not                                                                                | Positive | —    |
| I-05 | Delete a `VolumeGroupSnapshot` whose group was already deleted with its last member: delete returns success                                                                            | Boundary | —    |
| I-06 | Backend 5xx during the take: the `VolumeGroupSnapshot` reports not-ready, recovers when the backend returns                                                                            | Negative | —    |
| I-07 | The admission webhook (registered in envtest) rejects a `VolumeGroupSnapshot` whose selector spans two groups at create, and admits a single-group one                                 | Negative | —    |
| I-08 | The admission webhook (registered in envtest) rejects a `VolumeMigration` whose target PV backs a consistency-group member at create, and admits one for a non-member volume           | Negative | —    |

### VolumeGroupSnapshotOps Lifecycle (design §7.4)

File: `operator/internal/controller/volumegroupsnapshotops_controller_test.go`

| #    | Scenario                                                                                                                                                                                           | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-09 | Full reconcile over a ready target: every claim is created and binds, the phase reaches `Succeeded`, `membersBound` equals `membersExpected`, and `observedGeneration` matches the spec generation | Positive | —    |
| I-10 | Mutating `volumeGroupSnapshotRef`, `action`, or the restore parameters after create is rejected by the API server (CEL immutability)                                                               | Negative | —    |
| I-11 | Operator restart mid-restore: `status.step` restores the machine's position and no duplicate claim is created                                                                                      | Boundary | —    |
| I-12 | Deleting a `Succeeded` operation leaves the restored claims in place                                                                                                                               | Boundary | —    |

---

## 3. E2E Tests

Against a live simplyblock cluster with real fio workloads. The cross-volume correctness rows assert data coherence with hashes, not merely that I/O continued. The live rows are currently exercised by the external `regression_test/19` and `regression_test/20` scripts, which live outside this repository, so the `Test` column stays `—` until they move in.

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

| #    | Scenario                                                                                                                                                                                      | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-08 | Detach a member after generation N: generation N still lists it and restores complete, generation N+1 excludes it                                                                             | Positive | —    |
| E-09 | `snapshot list --consistency-group` shows the group and generation for every member snapshot, no name parsing needed                                                                          | Positive | —    |
| E-10 | The group-scoped listing reports expected-versus-present member counts, and flags an incomplete generation                                                                                    | Boundary | —    |
| E-11 | Delete the last member: the group and its remaining generations are removed, and a backend event records the widening                                                                         | Positive | —    |
| E-12 | A member is offline at snapshot time: the group snapshot is refused naming the member, no generation is taken, no I/O is frozen on the healthy members, and a retry succeeds once it recovers | Negative | —    |

### Single-Operation Restore (design §7.4)

| #    | Scenario                                                                                                                                                                 | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-13 | One applied `VolumeGroupSnapshotOps` restores a generation: every claim binds and the restored set is hash-verified crash-consistent (the one-apply counterpart of E-04) | Positive | —    |
| E-14 | A restore with `consistencyGroup` set: the clones form a new group pinned on one node (the one-apply counterpart of E-06)                                                | Positive | —    |

---

## 4. E2E — Phase 2 gating

The Phase 2 rows (I-01 … I-06, E-04 … E-11 through the `VolumeGroupSnapshot` path) are testable only once P0-4 enables the `CSIVolumeGroupSnapshot` feature gate and the CSI GroupController ships. Until then, E-01 … E-03 and the membership rows are exercisable through the backend group and `sbctl`. The Phase 3 rows (U-25 … U-33, I-09 … I-12, E-13, E-14) additionally depend on the `VolumeGroupSnapshotOps` kind, its webhook, and its controller, since the restore consumes Phase 2's materialized member snapshots.

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

### M-02 — A consistency-group member cannot be migrated

**Design reference:** §8.4, §9.5, Open Question 2

**What to verify:** migration of a group member is refused at two layers. The operator's `VolumeMigration` admission webhook (§9.5) declines the create at `kubectl apply`, so the operator never starts the move, and the backend refuses as the last line of defense for any path the webhook does not cover. Either way a group's placement stays fixed and its snapshots can always be frozen on one store.

**Test concept:**
1. Provision a three-member group.
2. Apply a `VolumeMigration` targeting one member's PV. Assert the admission webhook rejects the create, naming the volume and its group, and that no `VolumeMigration` object is created.
3. Reach the backend migration path directly (bypassing the webhook) and assert it refuses with a clear error, and the member stays on the pinned store.

---

## 6. Axis Coverage

| Axis                       | Values covered                                                                  | IDs                                                     | Not covered                          |
|----------------------------|---------------------------------------------------------------------------------|---------------------------------------------------------|--------------------------------------|
| Cluster topology           | 1 node, multi-node with a pinned group                                          | E-01, E-02                                              | asymmetric node sizes                |
| Group size                 | 1 member, 3+ members, the 20-member cap boundary (sbcli unit)                   | E-01, E-04                                              | very large groups (subsystem slots)  |
| Membership change          | join at create, one-way detach, death with last member                          | E-01, E-03, E-08, E-11                                  | re-establish via a labeled clone     |
| Selector versus membership | equal, extra handle, missing handle, two groups                                 | U-04 … U-07, U-12 … U-19, U-21 … U-24, I-03, I-07, I-08 | —                                    |
| Snapshot lifecycle         | take, get, delete, retry, delete-after-group-gone                               | U-04 … U-10, I-04, I-05                                 | —                                    |
| Representation             | per-snapshot group fields, group-scoped listing, incomplete generation          | E-09, E-10                                              | listing under very many generations  |
| Data correctness           | consistent clone, negative control, delete-preserves                            | E-04, E-05, M-01                                        | migration mid-snapshot (M-02 manual) |
| Restore path               | per-member `dataSource`, one-apply Ops, partial generation, new-group formation | E-04 … E-07, U-25 … U-33, I-09 … I-12, E-13, E-14       | restore into another namespace       |

---

## 7. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 33        | 31      | U-03, U-08  |
| Integration | 12        | 0       | I-01 … I-12 |
| E2E         | 14        | 0       | E-01 … E-14 |
| Manual      | 2         | 0       | M-01, M-02  |

Every scenario is uncovered because the feature is Draft. The counts are the target, and each `Test` column fills in as the work lands.

---

## 8. What Is Not Yet Covered

| #                                    | Gap                                                                                                          | Reason                                                                                                         |
|--------------------------------------|--------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------|
| U-01 … U-24                          | Provisioner label handling, the CSI GroupController, and the admission webhook                               | `TestCreateVolume_SendsConsistencyGroupLabel`                                                                  |
| I-01 … I-08                          | The `VolumeGroupSnapshot` lifecycle and admission webhook under `envtest`                                    | Depends on the CSI GroupController and the `CSIVolumeGroupSnapshot` feature gate (P0-4)                        |
| E-01 … E-12                          | Membership, placement, cross-volume consistency, clone, representation, and health-precheck live             | Depends on the standalone backend group (P0-2, P0-3), which is not shipped                                     |
| U-25 … U-33, I-09 … I-12, E-13, E-14 | The `VolumeGroupSnapshotOps` restore: claim derivation, admission, lifecycle, and the live one-apply restore | `TestGroupRestore_CreatesOneClaimPerMember`                                                                    |
| —                                    | Restore into another namespace                                                                               | The Ops kind is namespaced and restores into its own namespace, and a cross-namespace restore is not designed  |
| M-01                                 | Deleting a member preserves its group snapshots                                                              | The one data-loss path (§8.2); needs the standalone delete path and the group-scoped listing to assert against |
| M-02                                 | A member migrated off the pinned store                                                                       | Needs migration orchestration and the group-snapshot failure path (Open Question 2)                            |
| —                                    | Asymmetric node sizes, very large groups, listing under many generations                                     | Beyond the first coverage pass, recorded so the gap is explicit rather than assumed covered                    |



