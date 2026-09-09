# Test Plan: Consistency Groups

Related design: [`designs/design-consistency-groups.md`](../designs/design-consistency-groups.md)
Harness: [`operator/internal/controller`](../../internal/controller) and [`test/`](../../../test)

Scope is the operator, the CSI driver, and the Kubernetes surface this repository builds. The control plane (`sbcli`) and SPDK are dependencies, faked at the boundary: a row asserts this operator's or driver's response to a backend answer, never the backend's own group logic. The backend's group snapshot atomicity and group-wide fail-over resolution are the control plane's to prove, and their coverage lives with the `sbcli` regression suite.

Scenario IDs are permanent and are never reused or renumbered. `U-` is unit (no cluster, pure functions, a fake `client.Client`, a mock HTTP backend), `I-` is integration (full reconcile loop against `envtest` and a mock backend), `E-` is end-to-end (a live cluster and the real data path), and `M-` is manual (needs failure injection or orchestration not automated yet). Types are `Positive`, `Negative`, `Boundary`, and `Regression`. A `—` in the `Test` column means nothing implements the scenario yet, and every such row reappears in §8 with its reason.

The whole design is Draft, so every row is currently `—`. The plan is the target coverage, and the `Test` columns fill in as the work lands.

---

## 1. Unit Tests

The attach lifecycle as single reconcile calls against a fake client, with the control plane replaced by a mock HTTP server. No Kubernetes API server is involved. Numbering runs continuously across the groups.

### Policy Attachment Lifecycle (§6)

File: `operator/internal/controller/replicationpolicy_controller_unit_test.go`

| #    | Scenario                                                                                                                               | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-01 | `spec.consistencyGroupName` unset: reconcile is unchanged from today, no group call is made                                            | Negative | —    |
| U-02 | Group resolves empty: policy enters WaitingForGroup, `status.ready` false, `GroupAttachPending` emitted                                | Negative | —    |
| U-03 | Group exists: attach called, `status.ready` true, `GroupAttached` condition True, `GroupAttached` emitted                              | Positive | —    |
| U-04 | Already attached to this policy: re-reconcile is a no-op, no second attach call                                                        | Boundary | —    |
| U-05 | Group already attached to another policy: backend `409`, `GroupAttached` False reason `GroupAlreadyAttached`, not retried              | Negative | —    |
| U-06 | `spec.consistencyGroupName` cleared: detach called, `GroupDetached` emitted                                                            | Positive | —    |
| U-07 | `spec.consistencyGroupName` changed: old attachment detached, new group entered, both events emitted                                   | Positive | —    |
| U-08 | Policy CR deleted while attached: detach called before the finalizer is removed                                                        | Positive | —    |
| U-09 | Group deleted while attached (backend reports no group): re-enters WaitingForGroup, `GroupAttached` False reason `GroupGone`, no error | Negative | —    |
| U-10 | Backend unreachable during attach: requeue with backoff, no state advance, `GroupAttachFailed` emitted                                 | Negative | —    |
| U-11 | Backend unreachable during detach: requeue, attachment not lost, idempotent retry converges                                            | Negative | —    |
| U-12 | State re-derived after a simulated restart mid-attach: attach confirmed idempotently, not duplicated                                   | Boundary | —    |
| U-13 | Attach call retried after a partial success: mock call count asserts idempotency                                                       | Boundary | —    |
| U-14 | `404` on detach of a not-attached policy treated as success                                                                            | Boundary | —    |

### Provisioner Label Handling (§5.1)

File: `csi-driver/internal/csi/controller/controller_unit_test.go`

| #    | Scenario                                                                                      | Type     | Test |
|------|-----------------------------------------------------------------------------------------------|----------|------|
| U-15 | PVC carries the consistency-group label: `consistency_group` is set on the volume-create body | Positive | —    |
| U-16 | PVC has no label: `consistency_group` is absent, create is unchanged                          | Negative | —    |
| U-17 | Label present but empty value: rejected as an invalid group name, create fails cleanly        | Boundary | —    |

---

## 2. Integration Tests

The full reconcile loop against a mock backend HTTP server and a real Kubernetes API via `envtest`.

### Attachment Conditions and Events (§6, §11)

File: `operator/internal/controller/replicationpolicy_controller_test.go`

| #    | Scenario                                                                                                                                               | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | Create a policy naming a not-yet-existent group, then satisfy the group: the CR walks WaitingForGroup to Attached, and `GroupAttached` lands on the CR | Positive | —    |
| I-02 | Mutate `consistencyGroupName` on an attached policy: the CR walks Detaching then Attaching, and both events land                                       | Positive | —    |
| I-03 | `GroupAttachPending` is emitted on every reconcile while waiting, not only on entry                                                                    | Boundary | —    |
| I-04 | Two policies naming one group: the second reports `GroupAlreadyAttached` and does not flap                                                             | Negative | —    |
| I-05 | Backend 5xx during attach: requeued, no condition regression, recovers when the backend returns                                                        | Negative | —    |

---

## 3. E2E Tests

Against a live simplyblock cluster with real fio workloads. The cross-volume correctness rows assert data coherence with hashes, not merely that I/O continued.

### Group Membership and Placement (§5.1, §5.2)

| #    | Scenario                                                                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | First labeled PVC creates the group and pins the node, and a second labeled PVC lands on the same node and store              | Positive | —    |
| E-02 | A labeled PVC that cannot colocate on the pinned node: the PVC stays Pending, the backend error is surfaced, no unpinned join | Negative | —    |
| E-03 | A label added to a PVC after its volume exists does not join the volume to the group                                          | Negative | —    |

### Cross-Volume Consistency (§5.4)

| #    | Scenario                                                                                                                                                                   | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-04 | Hashed round-robin writer across all members, take generations under load, restore every member from one generation: the group prefix property holds and all hashes verify | Positive | —    |
| E-05 | Negative control: restore members from a mix of two generations, the cross-volume check detects the inconsistency                                                          | Negative | —    |
| E-06 | Group-wide fail-over: every member fails over to the same replicated generation, and the restored set is crash-consistent                                                  | Positive | —    |
| E-07 | Fail-back after target-side writes: data written on the target survives, and the group returns to the source still crash-consistent                                        | Positive | —    |

### Group Death (§5.4)

| #    | Scenario                                                                                                                                | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-08 | Delete the last member of a group: the group, its generations, and any attachment are removed, and a backend event records the widening | Positive | —    |

---

## 4. E2E — Phase 2 (Planned)

Testable only once P0-5 enables the `CSIVolumeGroupSnapshot` feature gate and the CSI GroupController ships. Type and Test are decided when the phase is scoped.

| #       | Scenario                                                                                                                               |
|---------|----------------------------------------------------------------------------------------------------------------------------------------|
| E-P2-01 | Create a `VolumeGroupSnapshot` over the group label: one generation is taken, and per-member `VolumeSnapshot` objects are materialized |
| E-P2-02 | Restore each member from the materialized snapshots of one `VolumeGroupSnapshot`: the group prefix property holds                      |
| E-P2-03 | A selector that resolves to a set differing from current membership is refused with `FAILED_PRECONDITION`                              |
| E-P2-04 | Delete a `VolumeGroupSnapshot` whose group was already deleted with its last member: delete returns success                            |
| E-P2-05 | `VolumeGroupSnapshot` delete removes only its own generation, and the group survives while a policy is attached                        |

---

## 5. Manual Scenarios and Test Concepts

### M-01 — A policy waits forever for a group that never gets a member

**Design reference:** §6, §10, §11

**What to verify:** a `ReplicationPolicy` naming a group that no volume ever creates stays in WaitingForGroup, keeps `status.ready` false, and emits `GroupAttachPending` on every reconcile, so the silent misconfiguration is visible rather than looking like a healthy idle policy.

**Test concept:**
1. Apply a `ReplicationPolicy` with `spec.consistencyGroupName: never-born` and no PVC carrying that label.
2. Watch the CR for several reconcile intervals.
3. Assert `status.ready` is false throughout, a `GroupAttachPending` event is emitted each interval, and `simplyblock_replicationpolicy_group_attach_pending` reads at least one.

### M-02 — Concurrent first volumes converge on one group

**Design reference:** §5.1, §8.1

**What to verify:** two PVCs created at the same time with the same group label produce exactly one backend group, and both volumes land on one pinned node.

**Open question:** whether the race is better exercised at the backend boundary (two concurrent volume-create calls against the real control plane) than through Kubernetes, since the convergence is the backend's ensure-group contract (P0-2).

**Test concept:**
1. Create two labeled PVCs in the same reconcile window.
2. After both bind, query the backend for groups of that name.
3. Assert exactly one group exists and both volumes are members on one node.

---

## 6. Axis Coverage

| Axis                 | Values covered                                          | IDs                      | Not covered                                           |
|----------------------|---------------------------------------------------------|--------------------------|-------------------------------------------------------|
| Cluster topology     | 1 node, multi-node with a pinned group                  | E-01, E-02               | asymmetric node sizes                                 |
| Group size           | 1 member, 3+ members                                    | E-01, E-04               | very large groups (subsystem slot exhaustion)         |
| Namespace scope      | namespaced StorageClass (subsystem sharing), standalone | U-15, E-01               | subsystem slot exhaustion mid-group                   |
| Membership change    | join at create, one-way removal, death with last member | E-01, E-03, E-08         | re-establish via a labeled clone                      |
| Attachment lifecycle | wait, attach, detach, change, conflict, restart         | U-02 … U-14, I-01 … I-05 | —                                                     |
| Cluster count        | single cluster, cross-cluster fail-over and fail-back   | E-06, E-07               | more than two clusters                                |
| Backend faults       | 409 conflict, 5xx, unreachable, 404 on delete           | U-05, U-10, U-14, I-05   | partial multi-member snapshot failure (backend-owned) |

---

## 7. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 17        | 0       | U-01 … U-17 |
| Integration | 5         | 0       | I-01 … I-05 |
| E2E         | 8         | 0       | E-01 … E-08 |
| Manual      | 2         | 0       | M-01, M-02  |

Every scenario is uncovered because the feature is Draft. The counts are the target, and each `Test` column fills in as Phase 1 lands.

---

## 8. What Is Not Yet Covered

| #                 | Gap                                                                                           | Reason                                                                                                        |
|-------------------|-----------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------|
| U-01 … U-17       | The attach lifecycle and the provisioner label handling                                       | Phase 1 not implemented, so the `spec.consistencyGroupName` field and the provisioner change do not exist yet |
| I-01 … I-05       | Attachment conditions and events under `envtest`                                              | Depends on the reconciler writing conditions and events, which this design adds                               |
| E-01 … E-08       | Membership, placement, cross-volume consistency, fail-over, and group death on a live cluster | Depends on the backend group-first REST surface (P0-1, P0-2), which is not shipped                            |
| E-P2-01 … E-P2-05 | The `VolumeGroupSnapshot` path                                                                | Phase 2: the CSI GroupController and the `CSIVolumeGroupSnapshot` feature gate (P0-5) are not enabled         |
| M-02              | Concurrent-first-volume convergence                                                           | The convergence is the backend ensure-group contract, so testing it end-to-end waits on P0-2                  |
| —                 | Asymmetric node sizes, subsystem slot exhaustion mid-group, more than two clusters            | Beyond the first coverage pass, recorded so the gap is explicit rather than assumed covered                   |
