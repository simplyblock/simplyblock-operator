# Test Plan: Non-Disruptive Test Failover

Related design: [`designs/design-test-failover.md`](../designs/design-test-failover.md)
Harness: [`operator/internal/controller`](../../internal/controller)

Scope: the operator, the CSI driver, and the Kubernetes surface of this
repository. Control-plane (`sbcli`) and SPDK behavior is a dependency, faked at
the boundary. See the `test-scenarios` skill.

Scenario IDs are permanent: `U-` unit (no cluster, pure functions, fake
`client.Client`, mock HTTP), `I-` integration (full reconcile loop against
`envtest` and a mock backend), `E-` end-to-end (live cluster, real data path),
`M-` manual (needs failure injection or orchestration not yet automated). Types
are `Positive`, `Negative`, `Boundary`, `Regression`. The `Test` column names
the implementing function, or `—` when the scenario is not yet covered. Every
`—` also appears in §8.

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

| #    | Scenario                                                                                         | Type     | Test |
|------|--------------------------------------------------------------------------------------------------|----------|------|
| U-01 | `scope: Volume` resolves `ref` to one source PVC and its backend volume                          | Positive | —    |
| U-02 | `scope: Group` resolves `ref` to the group's current member set, one recovered volume per member | Positive | —    |
| U-03 | `ref` names no PVC or group → clean `Failed` with a reason, no panic                             | Negative | —    |
| U-04 | `scope: Group` where the group has zero live members → `Failed`, nothing to recover              | Boundary | —    |

### Recovery-point choice (design §5.1)

File: `internal/controller/testfailover_unit_test.go`

| #    | Scenario                                                                                        | Type     | Test |
|------|-------------------------------------------------------------------------------------------------|----------|------|
| U-05 | `recoveryPoint` empty → the drill takes a fresh snapshot, `snapshotTaken: true`                 | Positive | —    |
| U-06 | `recoveryPoint` names an existing snapshot → it is reused, `snapshotTaken: false`               | Positive | —    |
| U-07 | `recoveryPoint` names a snapshot of a different volume than `ref` → `Failed`, mismatch reported | Negative | —    |

### Mode selection (design §5.1, §5.4)

File: `internal/controller/testfailover_unit_test.go`

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| U-08 | `targetClusterID` empty → same-cluster mode, no `Shipping` step in the graph  | Positive | —    |
| U-09 | `targetClusterID` set → cross-cluster mode, `Shipping` step present (Phase 2) | Positive | —    |

### Non-disruptiveness fingerprint (design §7.4)

File: `internal/controller/testfailover_fingerprint_unit_test.go`

| #    | Scenario                                                                                     | Type     | Test |
|------|----------------------------------------------------------------------------------------------|----------|------|
| U-10 | Source PVC bound to the same PV and volume before and at `Ready` → `invariantsHeld: true`    | Positive | —    |
| U-11 | Source PVC rebound to a different volume between captures → drift detected                   | Negative | —    |
| U-12 | Where replication exists, a `ReplicationSlot` state change between captures → drift detected | Negative | —    |

### State machine and idempotency (design §6, §8)

File: `internal/controller/testfailover_statemachine_unit_test.go`

| #    | Scenario                                                                             | Type     | Test |
|------|--------------------------------------------------------------------------------------|----------|------|
| U-13 | Each step advances to the next on its success condition                              | Positive | —    |
| U-14 | A step whose deadline has passed moves the object to `Failed`                        | Boundary | —    |
| U-15 | Deadline exactly at now is not yet expired, and now plus ε is (strict `>`)           | Boundary | —    |
| U-16 | Terminal `Ready` re-reconcile is a no-op                                             | Positive | —    |
| U-17 | Terminal `Failed` re-reconcile is a no-op                                            | Positive | —    |
| U-18 | The snapshot and clone idempotency key is `(test-id, source)`, stable across a retry | Positive | —    |

### Defaults and immutability (design §4.1, §9)

File: `internal/controller/testfailover_unit_test.go`

| #    | Scenario                                                            | Type     | Test |
|------|---------------------------------------------------------------------|----------|------|
| U-19 | `bubbleNamespace` defaults to `bubble` when unset                   | Boundary | —    |
| U-20 | `ttlSeconds` unset → no auto-teardown scheduled                     | Boundary | —    |
| U-21 | `ttlSeconds` set → teardown scheduled at creation time plus the TTL | Positive | —    |

---

## 2. Integration Tests

Run the full controller reconcile loop against a real Kubernetes API via
`envtest` and a mock control-plane HTTP server that records call counts.

### Same-cluster drill lifecycle (design §5.1–§5.3, §6)

| #    | Scenario                                                                                                                                                                    | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | Create → `Provisioning` takes the snapshot (P0-1), clones it (P0-2), adopts a static PV and binds a PVC, reaches `Ready`, with `report` populated and a `BubbleReady` event | Positive | —    |
| I-02 | `recoveryPoint` set → no snapshot is taken, the pinned snapshot is cloned (mock shows zero snapshot calls)                                                                  | Positive | —    |
| I-03 | Source volume not found (P0-1 404) → `Failed` at `Snapshotting`, ref in `status.message`                                                                                    | Negative | —    |
| I-04 | Clone returns 5xx → retried, no state advance, mock shows repeated calls                                                                                                    | Negative | —    |
| I-05 | Group drill: one group snapshot → one bound PVC per member, all from that snapshot                                                                                          | Positive | —    |
| I-06 | PVC name collision in the bubble namespace → `Failed` at `Binding`, the name in `status.message`                                                                            | Negative | —    |
| I-07 | Control plane unreachable (connection refused) → requeue with backoff, no partial state committed                                                                           | Negative | —    |

### Restart safety (design §6, §7.4)

| #    | Scenario                                                                                                                                | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-08 | Operator restart at `Snapshotting` after the snapshot was taken but before its id was recorded → resumes, mock snapshot call count is 1 | Negative | —    |
| I-09 | Operator restart at `Cloning` after the clone was issued → resumes, mock clone call count is 1                                          | Negative | —    |
| I-10 | Operator restart at `Binding` → resumes, does not re-create an already-bound PV                                                         | Negative | —    |
| I-11 | Operator restart at `Releasing` during teardown → resumes the reclaim, does not double-reclaim                                          | Negative | —    |

### Non-disruptiveness guard (design §7.4)

| #    | Scenario                                                                                                                                                               | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-12 | The source PVC and its backend volume are unchanged before and after a `Ready` drill, and `invariantsHeld: true`                                                       | Positive | —    |
| I-13 | Injected source change between start and `Ready` → `Failed`, `InvariantViolated` event, `invariantsHeld: false`                                                        | Negative | —    |
| I-14 | The whole drill issues zero mutating calls against the source volume (assert the mock only ever saw a snapshot-read-then-clone, never a write or delete on the source) | Positive | —    |

### Teardown and finalizer (design §5.3, §6)

| #    | Scenario                                                                                                                                     | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-15 | Delete a `Ready` drill → `TearingDown` deletes PVCs and PVs, reclaims clones (P0-3), deletes the drill-taken snapshot, removes the finalizer | Positive | —    |
| I-16 | A drill that reused a pinned `recoveryPoint` → teardown reclaims the clone but does NOT delete the pinned snapshot                           | Positive | —    |
| I-17 | Delete a `Failed` drill → teardown still runs, finalizer removed on the failure path                                                         | Negative | —    |
| I-18 | Reclaim returns non-success → holds `TearingDown`, finalizer retained, `ReclaimPending` event                                                | Negative | —    |
| I-19 | Reclaim of an already-gone clone or snapshot returns success (404-as-success), teardown completes                                            | Negative | —    |
| I-20 | After teardown, no object carrying the drill's `test-id` label remains                                                                       | Positive | —    |

### Admission and concurrency (design §4.1, §7.3)

| #    | Scenario                                                                                                                             | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-21 | Patch `scope`, `ref`, `recoveryPoint`, `targetClusterID`, or `bubbleNamespace` after creation → admission rejects (immutability CEL) | Negative | —    |
| I-22 | A second drill on the same `(scope, ref, targetClusterID)` while the first is active → refused                                       | Negative | —    |
| I-23 | Two drills on different `ref`s run independently, neither blocks the other                                                           | Positive | —    |
| I-24 | RBAC sufficiency: the controller creates a `PersistentVolume` and a PVC without hitting a forbidden verb                             | Positive | —    |

---

## 3. E2E Tests

Run against a live cluster with a real source volume. Data-path rows assert
recovered-data correctness (a marker written to the source), not merely that a
PVC bound.

### Same-cluster drill (design §5.1–§5.3)

| #    | Scenario                                                                                                                                                 | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | A snapshot of the source is cloned into `bubble`, a pod boots on the PVC, and the recovered marker matches the source                                    | Positive | —    |
| E-02 | Across the drill, the source volume's data and I/O are untouched (a marker written after the drill starts is present on the source, absent on the clone) | Positive | —    |
| E-03 | Teardown reclaims the clone and deletes the drill-taken snapshot, and the backend shows no leaked volume or snapshot for the drill's `test-id`           | Positive | —    |
| E-04 | Group drill: every member PVC recovers from one group snapshot and each carries the marker (crash-consistent set)                                        | Positive | —    |
| E-05 | Operator restart mid-drill on a live cluster → a single snapshot and clone, the drill still reaches `Ready`                                              | Negative | —    |

---

## 4. Cross-Cluster — Phase 2 (Planned)

Testable only once P0-4 (on-demand shipping to a named target backend) exists
(design §5.4). Type and Test are decided when the phase is scoped.

| #       | Scenario                                                                                                                             |
|---------|--------------------------------------------------------------------------------------------------------------------------------------|
| U-P2-01 | Mode selection routes through the `Shipping` step only when the target backend differs from the source's backend                     |
| I-P2-01 | `targetClusterID` set → the recovery point is the target's latest replicated snapshot, cloned on the target, and a PVC binds there   |
| I-P2-02 | Separate backend → `Shipping` calls P0-4, polls the returned handle, then clones on the target                                       |
| I-P2-03 | Ship handle poll exceeds its deadline → `Failed` at `Shipping`                                                                       |
| E-P2-01 | Cross-cluster drill to a separate backend: the point is shipped, cloned, and a pod boots on the recovered PVC with the marker intact |

---

## 5. Manual Scenarios and Test Concepts

### M-01 — Non-disruptiveness under sustained production I/O

**Design reference:** design §7.4, §2 (Goals)

**What to verify:** a drill run while the source application is actively writing
does not disturb it. No write is lost and the running volume is unaffected.

**Test concept:**
1. Run fio in verify mode against the source workload.
2. While it runs, create a `TestFailover` against that volume and let it reach `Ready`.
3. Assert `status.report.invariantsHeld` is true and the source PVC is bound to the same backend volume as before.
4. Tear the drill down and confirm fio still verifies with no errors.

### M-02 — Pinned recovery point deleted before the clone

**Design reference:** design §5.1, §10 (Failure Modes)

**What to verify:** a drill that pins an existing snapshot degrades cleanly if
that snapshot is deleted before the clone runs.

**Test concept:**
1. Create a `TestFailover` with `recoveryPoint` naming an existing snapshot.
2. Delete that snapshot before the clone call.
3. Assert the drill reports `Failed` at `Cloning` with a not-found reason, and that no clone was created.

### M-03 — Leftover proof with `LIST_VOLUMES` disabled

**Design reference:** design §5.3, Open Question 2

**What to verify:** teardown leaves no leaked backend clone or drill-taken
snapshot even though the CSI driver does not advertise `LIST_VOLUMES`, so the
Kubernetes-side label enumeration cannot be cross-checked through CSI.

**Open question:** whether the label enumeration on Kubernetes objects is
sufficient, or a backend enumeration is required (design Open Question 2).

**Test concept:**
1. Run and tear down a drill.
2. Enumerate backend volumes and snapshots directly on the storage cluster (out of band) and assert none carry the drill's `test-id`.

---

## 6. Axis Coverage

| Axis                   | Values covered                                                 | IDs                          | Not covered                                            |
|------------------------|----------------------------------------------------------------|------------------------------|--------------------------------------------------------|
| Where the test runs    | source's own cluster                                           | I-01, E-01                   | separate cluster (Phase 2: I-P2-01, E-P2-01)           |
| Scope                  | Volume, Group                                                  | U-01, U-02, I-05, E-04       | —                                                      |
| Recovery point         | fresh snapshot, pinned snapshot                                | U-05, U-06, I-02             | —                                                      |
| Lifecycle / restart    | mid-step restart, delete mid-drill, TTL teardown               | I-08, I-15, U-21             | control-plane restart mid-call                         |
| Control-plane response | 404, 5xx, connection refused, idempotent retry, 404-as-success | I-03, I-04, I-07, I-08, I-19 | partial-write then crash                               |
| Concurrency            | same source, different sources                                 | I-22, I-23                   | spec mutated mid-drill (blocked by immutability, I-21) |
| Data correctness       | recovered marker, source untouched, crash-consistent group     | E-01, E-02, E-04             | recovered-data checksum under load (M-01)              |

---

## 7. Coverage Summary

| Class             | Scenarios | Covered | Not covered                         |
|-------------------|-----------|---------|-------------------------------------|
| Unit              | 21        | 0       | U-01 … U-21                         |
| Integration       | 24        | 0       | I-01 … I-24                         |
| E2E               | 5         | 0       | E-01 … E-05                         |
| Manual            | 3         | 0       | M-01 … M-03                         |
| Phase 2 (planned) | 5         | 0       | U-P2-01, I-P2-01 … I-P2-03, E-P2-01 |

Nothing is covered: the design is `Draft` and the CRD and controller are unbuilt.
Every scenario is a gap until the work plan's items land. Phase 1 has no unbuilt
backend dependency, so its scenarios become implementable as soon as the
controller exists.

---

## 8. What Is Not Yet Covered

| #                                   | Gap                                          | Reason                                                                                                                                        |
|-------------------------------------|----------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------|
| U-01 … U-21                         | All unit scenarios                           | The `TestFailover` type and controller do not exist yet                                                                                       |
| I-01 … I-24                         | All integration scenarios                    | The controller and its `envtest` suite are unwritten                                                                                          |
| E-01 … E-05                         | All E2E scenarios                            | Needs a live cluster and the shipped feature                                                                                                  |
| M-01 … M-03                         | All manual scenarios                         | Need the shipped feature plus failure injection (snapshot delete, backend enumeration)                                                        |
| U-P2-01, I-P2-01 … I-P2-03, E-P2-01 | Cross-cluster                                | Blocked on P0-4 (on-demand shipping to a named target backend), which does not exist                                                          |
| —                                   | Partial-write then crash on the backend      | The backend verbs are the idempotency boundary, and asserting a mid-write crash needs backend fault injection this repository's harness lacks |
| —                                   | Recovered-data checksum under sustained load | Covered as a manual concept (M-01), and not automatable without a live-cluster fio harness                                                    |
