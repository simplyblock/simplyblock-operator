# Test Plan: Non-Disruptive Test Failover

Related design: [`designs/design-test-failover.md`](../designs/design-test-failover.md)
Harness: [`operator/internal/controller`](../../internal/controller)

Scope: the operator, the CSI driver, and the Kubernetes surface of this
repository. Control-plane (`sbcli`), SPDK, and OCM behavior is a dependency,
faked at the boundary. See the `test-scenarios` skill.

Scenario IDs are permanent: `U-` unit (no cluster, pure functions, fake
`client.Client`, mock HTTP), `I-` integration (full reconcile loop against
`envtest`, a mock backend, and a mock OCM), `E-` end-to-end (live clusters, real
data path), `M-` manual (needs failure injection or orchestration not yet
automated). Types are `Positive`, `Negative`, `Boundary`, `Regression`. The
`Test` column names the implementing function, or `—` when the scenario is not
yet covered. Every `—` also appears in §8.

This design is `Draft` and nothing is implemented, so every `Test` cell is `—`.
The plan is the specification the implementation is written against, and §8
carries the whole matrix as the gap list until the work lands.

---

## 1. Unit Tests

Pure helpers and controller methods in `testfailover_controller.go`, covered with
a fake `client.Client` and a mock control-plane HTTP server. Numbering runs
continuously across the groups.

### Source resolution (design §4.1, §5.1)

File: `internal/controller/testfailover_unit_test.go`

| #    | Scenario                                                                                             | Type     | Test |
|------|------------------------------------------------------------------------------------------------------|----------|------|
| U-01 | `scope: Volume`: a `ManagedClusterView` projection of the source PVC and PV yields the volume handle | Positive | —    |
| U-02 | `scope: Group`: `sourceRef` resolves to the group's member volumes, one recovered volume per member  | Positive | —    |
| U-03 | The view projects nothing for `sourceRef` → clean `Failed`, no panic                                 | Negative | —    |
| U-04 | `scope: Group` where the group has zero live members → `Failed`, nothing to recover                  | Boundary | —    |

### Recovery-point resolution (design §5.2)

File: `internal/controller/testfailover_unit_test.go`

| #    | Scenario                                                                                                          | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------|----------|------|
| U-05 | `bubbleCluster` is the source's own cluster, `recoveryPoint` empty → fresh source snapshot, `snapshotTaken: true` | Positive | —    |
| U-06 | `bubbleCluster` is the source's own cluster, `recoveryPoint` set → pinned snapshot reused, `snapshotTaken: false` | Positive | —    |
| U-07 | `bubbleCluster` is a DR target → latest replicated snapshot on that backend, `snapshotTaken: false`               | Positive | —    |
| U-08 | `recoveryPoint` names a snapshot of a different source than `sourceRef` → `Failed`, mismatch reported             | Negative | —    |

### Non-disruptiveness fingerprint (design §7.4)

File: `internal/controller/testfailover_fingerprint_unit_test.go`

| #    | Scenario                                                                                                   | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------|----------|------|
| U-09 | Source volume and its replication lag unchanged from `ResolvingSource` to `Ready` → `invariantsHeld: true` | Positive | —    |
| U-10 | Source PVC rebound to a different volume between captures → drift detected                                 | Negative | —    |
| U-11 | A `ReplicationSlot` or VGR state change between captures → drift detected                                  | Negative | —    |

### State machine and idempotency (design §6, §8)

File: `internal/controller/testfailover_statemachine_unit_test.go`

| #    | Scenario                                                                             | Type     | Test |
|------|--------------------------------------------------------------------------------------|----------|------|
| U-12 | Each step advances to the next on its success condition                              | Positive | —    |
| U-13 | A step whose deadline has passed moves the object to `Failed`                        | Boundary | —    |
| U-14 | Deadline exactly at now is not yet expired, and now plus ε is (strict `>`)           | Boundary | —    |
| U-15 | Terminal `Ready` re-reconcile is a no-op                                             | Positive | —    |
| U-16 | Terminal `Failed` re-reconcile is a no-op                                            | Positive | —    |
| U-17 | The snapshot and clone idempotency key is `(test-id, source)`, stable across a retry | Positive | —    |

### Defaults and immutability (design §4.1, §9)

File: `internal/controller/testfailover_unit_test.go`

| #    | Scenario                                                            | Type     | Test |
|------|---------------------------------------------------------------------|----------|------|
| U-18 | `bubbleNamespace` defaults to `bubble` when unset                   | Boundary | —    |
| U-19 | `ttlSeconds` unset → no auto-teardown scheduled                     | Boundary | —    |
| U-20 | `ttlSeconds` set → teardown scheduled at creation time plus the TTL | Positive | —    |

---

## 2. Integration Tests

Run the full controller reconcile loop against a real Kubernetes API via
`envtest`, a mock control-plane HTTP server that records call counts, and a mock
OCM (`ManagedClusterView` and `ManifestWork` with status feedback).

### Source read and in-place drill (design §5.1–§5.4, §6)

| #    | Scenario                                                                                                                                                                                                            | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | Create → `ResolvingSource` creates a `ManagedClusterView` on `sourceCluster`, reads the PVC handle, then takes the snapshot (P0-1), clones it (P0-2), places PV and PVC, reaches `Ready` with a `BubbleReady` event | Positive | —    |
| I-02 | `recoveryPoint` set → no snapshot is taken, the pinned snapshot is cloned (mock shows zero snapshot calls)                                                                                                          | Positive | —    |
| I-03 | `sourceCluster` is not a registered `ManagedCluster` → `Failed` at `ResolvingSource`                                                                                                                                | Negative | —    |
| I-04 | The `ManagedClusterView` projects nothing for `sourceRef` → `Failed` at `ResolvingSource`, ref in `status.message`                                                                                                  | Negative | —    |
| I-05 | Clone returns 5xx → retried, no state advance, mock shows repeated calls                                                                                                                                            | Negative | —    |
| I-06 | Group drill: one group snapshot → one PVC per member, all from that point                                                                                                                                           | Positive | —    |
| I-07 | Control plane unreachable (connection refused) → requeue with backoff, no partial state committed                                                                                                                   | Negative | —    |

### DR-target drill via OCM (design §5.5, §7.6)

| #    | Scenario                                                                                                                                                                                                            | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-08 | `bubbleCluster` set → the recovery point resolves to the target's latest replicated snapshot (P0-4), the clone is built on the target backend, and the PV and PVC are delivered as a `ManifestWork` to that cluster | Positive | —    |
| I-09 | `ManifestWork` status feedback reports the PVC `Bound` → the drill reaches `Ready`                                                                                                                                  | Positive | —    |
| I-10 | `bubbleCluster` is not a registered `ManagedCluster` → `Failed` at `Placing`                                                                                                                                        | Negative | —    |
| I-11 | Phase 2 drill where replication has landed nothing on the target yet (P0-4 404) → `Failed` at `ResolvingPoint`                                                                                                      | Negative | —    |
| I-12 | `ManifestWork` never reports Bound before its deadline → `Failed` at `Placing`, clone recorded for reclaim                                                                                                          | Negative | —    |

### Restart safety (design §6, §7.4)

| #    | Scenario                                                                                                     | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------|----------|------|
| I-13 | Restart at `ResolvingSource` → resumes, does not create a second `ManagedClusterView`                        | Negative | —    |
| I-14 | Restart at `Cloning` after the clone was issued → resumes, clone call count is 1                             | Negative | —    |
| I-15 | Restart at `Placing` after the `ManifestWork` was created → resumes, does not create a second `ManifestWork` | Negative | —    |
| I-16 | Restart at `Releasing` during teardown → resumes the reclaim, does not double-reclaim                        | Negative | —    |

### Non-disruptiveness guard (design §7.4)

| #    | Scenario                                                                                                                        | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-17 | The source and its replication lag are unchanged before and after a `Ready` drill, and `invariantsHeld: true`                   | Positive | —    |
| I-18 | Injected source or relationship change between start and `Ready` → `Failed`, `InvariantViolated` event, `invariantsHeld: false` | Negative | —    |
| I-19 | The whole drill issues zero mutating calls against the source volume or its relationship                                        | Positive | —    |

### Teardown and finalizer (design §5.7, §6)

| #    | Scenario                                                                                                                                                                       | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-20 | Delete a `Ready` drill → `TearingDown` deletes the `ManifestWork` and `ManagedClusterView`, reclaims the clone (P0-3), deletes the drill-taken snapshot, removes the finalizer | Positive | —    |
| I-21 | A DR-target drill that only resolved a replicated snapshot → teardown reclaims the clone but does NOT delete that snapshot                                                     | Positive | —    |
| I-22 | Delete a `Failed` drill → teardown still runs, finalizer removed on the failure path                                                                                           | Negative | —    |
| I-23 | Reclaim returns non-success → holds `TearingDown`, finalizer retained, `ReclaimPending` event                                                                                  | Negative | —    |
| I-24 | Reclaim of an already-gone clone or snapshot returns success (404-as-success), teardown completes                                                                              | Negative | —    |
| I-25 | After teardown, no object carrying the drill's `test-id` label remains                                                                                                         | Positive | —    |

### Admission and concurrency (design §4.1, §7.3)

| #    | Scenario                                                                                                                                                                          | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-26 | Patch any immutable spec field (`scope`, `sourceCluster`, `sourceNamespace`, `sourceRef`, `bubbleCluster`, `recoveryPoint`, `bubbleNamespace`) after creation → admission rejects | Negative | —    |
| I-27 | A second drill on the same `(scope, sourceCluster, sourceRef, bubbleCluster)` while the first is active → refused                                                                 | Negative | —    |
| I-28 | Two drills on different sources run independently, neither blocks the other                                                                                                       | Positive | —    |
| I-29 | RBAC sufficiency: the controller creates a `ManagedClusterView` and a `ManifestWork` without a forbidden verb, and needs no PV/PVC or snapshot-API permission                     | Positive | —    |

---

## 3. E2E Tests

Run against a live two-cluster DR setup (a source cluster and a DR target).
Data-path rows assert recovered-data correctness (a marker written to the
source), not merely that a PVC bound.

### In-place drill (design §5.1–§5.4)

| #    | Scenario                                                                                                                                                | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | `bubbleCluster` = the source's cluster: a fresh source snapshot is cloned into `bubble` there, a pod boots on the PVC, and the recovered marker matches | Positive | —    |
| E-02 | Across the drill, the source's data and I/O are untouched                                                                                               | Positive | —    |

### DR-target drill (design §5.5)

| #    | Scenario                                                                                                                                                                                                    | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-03 | `bubbleCluster` = the DR target: the replicated snapshot there is cloned, the bubble PVC is placed on the target via `ManifestWork`, a pod boots on the target, and the recovered marker matches the source | Positive | —    |
| E-04 | Across a DR-target drill, the source and the running source-to-target replication lag are unchanged                                                                                                         | Positive | —    |
| E-05 | Group drill onto the target: every member PVC recovers from one replicated generation and each carries the marker (crash-consistent set)                                                                    | Positive | —    |
| E-06 | Teardown of a DR-target drill deletes the OCM objects and reclaims the clone, and the target shows no leaked volume, and the replicated snapshot it resolved is left intact                                 | Positive | —    |
| E-07 | Operator restart mid-drill → a single view, snapshot, clone, and `ManifestWork`, the drill still reaches `Ready`                                                                                            | Negative | —    |

---

## 4. Ship-to-Non-Target — Phase 3 (Planned)

Testable only once P0-6 (on-demand shipping to a backend with no copy) exists
(design §5.6). Type and Test are decided when the phase is scoped.

| #       | Scenario                                                                                                                              |
|---------|---------------------------------------------------------------------------------------------------------------------------------------|
| U-P3-01 | Recovery-point resolution routes through the `Shipping` step only when the bubble backend holds no copy                               |
| I-P3-01 | `bubbleCluster` has no replica → `Shipping` calls P0-6, polls the returned handle, then clones on that backend and places the PVC     |
| I-P3-02 | Ship handle poll exceeds its deadline → `Failed` at `Shipping`                                                                        |
| E-P3-01 | Drill onto a third cluster with no replica: the point is shipped, cloned, and a pod boots on the recovered PVC with the marker intact |

---

## 5. Manual Scenarios and Test Concepts

### M-01 — Non-disruptiveness under sustained production I/O

**Design reference:** design §7.4, §2 (Goals)

**What to verify:** a drill run while the source application is actively writing,
and while source-to-target replication is running, disturbs neither. No write is
lost and the replication lag does not regress.

**Test concept:**
1. Run fio in verify mode against the source workload, with replication to the target active.
2. While it runs, create a `TestFailover` with `bubbleCluster` = the target and let it reach `Ready`.
3. Assert `status.report.invariantsHeld` is true, the source is bound to the same volume, and the replication lag is within normal variance.
4. Tear the drill down and confirm fio still verifies with no errors.

### M-02 — Pinned recovery point deleted before the clone

**Design reference:** design §5.2, §10 (Failure Modes)

**What to verify:** a drill that pins an existing snapshot degrades cleanly if
that snapshot is deleted before the clone runs.

**Test concept:**
1. Create a `TestFailover` with `recoveryPoint` naming an existing snapshot.
2. Delete that snapshot before the clone call.
3. Assert the drill reports `Failed` at `Cloning` with a not-found reason, and that no clone was created.

### M-03 — Leftover proof with `LIST_VOLUMES` disabled

**Design reference:** design §5.7, Open Question 3

**What to verify:** teardown leaves no leaked backend clone or drill-taken
snapshot on the bubble backend, even though the CSI driver does not advertise
`LIST_VOLUMES`, so the Kubernetes-side label enumeration cannot be cross-checked
through CSI.

**Open question:** whether the label enumeration on Kubernetes objects is
sufficient, or a backend enumeration is required (design Open Question 3).

**Test concept:**
1. Run and tear down a DR-target drill.
2. Enumerate backend volumes and snapshots directly on the target's storage cluster (out of band) and assert none carry the drill's `test-id`.

---

## 6. Axis Coverage

| Axis                         | Values covered                                                                       | IDs                                | Not covered                                            |
|------------------------------|--------------------------------------------------------------------------------------|------------------------------------|--------------------------------------------------------|
| Where the bubble runs        | source's own cluster, DR target                                                      | I-01, E-01, I-08, E-03             | non-target cluster (Phase 3: I-P3-01, E-P3-01)         |
| Source location              | read on a managed cluster via ManagedClusterView                                     | U-01, I-01                         | source on the hub's own self-managed cluster           |
| Scope                        | Volume, Group                                                                        | U-01, U-02, I-06, E-05             | —                                                      |
| Recovery point               | fresh snapshot, pinned snapshot, replicated                                          | U-05, U-06, U-07                   | shipped (Phase 3)                                      |
| Cross-cluster transport      | ManagedClusterView read, ManifestWork write                                          | I-01, I-08                         | —                                                      |
| Lifecycle / restart          | mid-step restart (each step), delete mid-drill, TTL teardown                         | I-13, I-15, I-20, U-20             | control-plane restart mid-call                         |
| Control-plane / OCM response | 404, 5xx, connection refused, ManifestWork timeout, idempotent retry, 404-as-success | I-04, I-05, I-07, I-12, I-14, I-24 | partial-write then crash                               |
| Concurrency                  | same source, different sources                                                       | I-27, I-28                         | spec mutated mid-drill (blocked by immutability, I-26) |
| Data correctness             | recovered marker, source untouched, crash-consistent group                           | E-01, E-04, E-05                   | recovered-data checksum under load (M-01)              |

---

## 7. Coverage Summary

| Class             | Scenarios | Covered | Not covered                        |
|-------------------|-----------|---------|------------------------------------|
| Unit              | 20        | 0       | U-01 … U-20                        |
| Integration       | 29        | 0       | I-01 … I-29                        |
| E2E               | 7         | 0       | E-01 … E-07                        |
| Manual            | 3         | 0       | M-01 … M-03                        |
| Phase 3 (planned) | 4         | 0       | U-P3-01, I-P3-01, I-P3-02, E-P3-01 |

Nothing is covered: the design is `Draft` and the CRD and controller are unbuilt.
Phases 1 and 2 have no unbuilt backend dependency, so their scenarios become
implementable as soon as the controller exists.

---

## 8. What Is Not Yet Covered

| #                                  | Gap                                          | Reason                                                                                                                                        |
|------------------------------------|----------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------|
| U-01 … U-20                        | All unit scenarios                           | The `TestFailover` type and controller do not exist yet                                                                                       |
| I-01 … I-29                        | All integration scenarios                    | The controller, its `envtest` suite, and the mock OCM are unwritten                                                                           |
| E-01 … E-07                        | All E2E scenarios                            | Needs a live two-cluster DR setup and the shipped feature                                                                                     |
| M-01 … M-03                        | All manual scenarios                         | Need the shipped feature plus failure injection (snapshot delete, backend enumeration)                                                        |
| U-P3-01, I-P3-01, I-P3-02, E-P3-01 | Ship-to-non-target                           | Blocked on P0-6 (on-demand shipping to a backend with no copy), which does not exist                                                          |
| —                                  | Source on the hub's own self-managed cluster | An edge of the topology axis. The primary path reads the source on a managed cluster, and the self-managed case is not exercised separately   |
| —                                  | Partial-write then crash on the backend      | The backend verbs are the idempotency boundary, and asserting a mid-write crash needs backend fault injection this repository's harness lacks |
| —                                  | Recovered-data checksum under sustained load | Covered as a manual concept (M-01), and not automatable without a live-cluster fio harness                                                    |
