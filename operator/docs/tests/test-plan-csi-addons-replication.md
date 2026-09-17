# Test Plan: csi-addons Volume Replication

Related design: [`designs/design-csi-addons-replication.md`](../designs/design-csi-addons-replication.md)

Scope is the CSI driver's Replication service, the operator's preflight and coexistence rules, and the deployment of the csi-addons machinery. The replication engine itself (snapshot shipping, failover cloning, the cutover task runner) is the control plane's to prove and is exercised here only through the adapter's boundary. The kubernetes-csi-addons controller-manager is stock upstream and is not re-tested; what is tested is this driver's conformance to the contract it drives.

Scenario IDs are permanent and are never reused or renumbered. `U-` is unit (no cluster: mock control plane, fake `client.Client`), `I-` is integration (the sidecar and controller-manager against the driver with a mock backend), `E-` is end-to-end (two live simplyblock clusters), and `M-` is manual. Types are `Positive`, `Negative`, `Boundary`, and `Regression`. A `—` in the `Test` column means nothing implements the scenario yet, and every such row reappears in §6 with its reason.

The plan is the target coverage for a Draft design: every row is `—` until the work lands.

---

## 1. Unit Tests

The Replication service against a mock control plane, and the operator pieces against fake clients. No Kubernetes API server. Numbering runs continuously across the groups.

### Replication Verbs: Enable, Disable, Info (design §5.1)

File: `csi-driver/internal/csi/controller/replication_test.go` (planned)

| #    | Scenario                                                                                                      | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------|----------|------|
| U-01 | Enable on an unattached volume: the attach call carries the class's policy, and the RPC succeeds              | Positive | —    |
| U-02 | Enable on a volume already attached to the same policy: success, no second attach call (idempotency)          | Boundary | —    |
| U-03 | Enable on a volume attached to a different policy: `FAILED_PRECONDITION` naming both policies, no attach call | Negative | —    |
| U-04 | Disable on an attached volume: the detach call is made, success                                               | Positive | —    |
| U-05 | Disable on a non-attached volume: success without a backend call (idempotency)                                | Boundary | —    |
| U-06 | Disable while a cutover is in flight (backend 409): `ABORTED`, retryable                                      | Negative | —    |
| U-07 | Info returns `lastSyncTime`, `lastSyncDuration`, and `lastSyncBytes` from the status read                     | Positive | —    |
| U-08 | A malformed volume handle: `INVALID_ARGUMENT` before any backend call                                         | Negative | —    |
| U-09 | Backend unreachable: `UNAVAILABLE`, and no condition flap is implied by the error                             | Negative | —    |

### Replication Verbs: Promote, Demote, Resync (design §5.2)

File: `csi-driver/internal/csi/controller/replication_lifecycle_test.go` (planned)

| #    | Scenario                                                                                                                                                          | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-10 | Forced promote: the failover endpoint is called, and `Completed` follows the relationship reaching `failed_over`                                                  | Positive | —    |
| U-11 | Forced promote repeated after completion: success without a second failover (backend idempotency honored)                                                         | Boundary | —    |
| U-12 | Planned promote after a completed demote: the failover endpoint is called with the planned gate, `Completed=True` once the relationship reports the target active | Positive | —    |
| U-13 | Planned promote without a completed demote (source live or peer lagging): the planned gate refuses, `FAILED_PRECONDITION` naming the un-flushed tail              | Negative | —    |
| U-14 | Demote: the demote endpoint is called, and `Completed` is `True` only after the flush-confirmed response                                                          | Positive | —    |
| U-15 | Demote repeated on a demoted volume: success (idempotency)                                                                                                        | Boundary | —    |
| U-16 | Resync: the failback endpoint is called and `Resyncing=True` while the status read reports the catch-up                                                           | Positive | —    |
| U-17 | Resync completion: `Resyncing` clears when the reverse lag is inside the budget                                                                                   | Boundary | —    |

### Condition Derivation (design §6.2)

File: `csi-driver/internal/csi/controller/replication_conditions_test.go` (planned)

| #    | Scenario                                                                                                        | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------|----------|------|
| U-18 | Status `in_sync` and `replicating`: `Completed=True, Degraded=False, Resyncing=False` (lag alone trips nothing) | Positive | —    |
| U-19 | Status `degraded` (suspended tasks) and `error` (retries exhausted): `Degraded=True` with the failure detail    | Negative | —    |
| U-20 | Lag beyond the budget with healthy shipping: `Degraded=True` on the staleness rule                              | Boundary | —    |
| U-21 | Status read reports `resyncing`: `Resyncing=True`; cleared on the next read without it                          | Positive | —    |
| U-22 | Status `not_replicating`, role `none`: reported as disabled, no `Degraded`                                      | Boundary | —    |

### Operator: Preflight and Coexistence (design §7.2, §8)

Files: `operator/internal/controller/peerclasses_preflight_test.go`, `operator/internal/controller/pvcreplication_controller_test.go` (planned)

| #    | Scenario                                                                                                                                                    | Type     | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-23 | Same-named classes on both clusters with mutually pointing policies: `PeerClassesVerified` on the pair                                                      | Positive | —    |
| U-24 | A replication-enabled class missing on the peer: `PeerClassesMismatch` naming the class and side                                                            | Negative | —    |
| U-25 | Named policies exist but do not point at each other's clusters: `PeerClassesMismatch`                                                                       | Negative | —    |
| U-26 | `PVCAnnotationWatcher` skips a PVC whose volume has a `VolumeReplication`: no slot is created, a skip is recorded                                           | Negative | —    |
| U-27 | Annotation added and later a `VolumeReplication` appears: the existing slot is not deleted by the adapter, and the enable is refused per the one-owner rule | Boundary | —    |

---

## 2. Integration Tests

The csi-addons sidecar and controller-manager against the driver with a mock control plane, on a kind or envtest-backed cluster with the vendored CRDs installed.

### VolumeReplication Lifecycle (design §4, §5)

| #    | Scenario                                                                                                                                          | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | The sidecar probes the driver, advertises `VOLUME_REPLICATION`, and a `CSIAddonsNode` is published                                                | Positive | —    |
| I-02 | Creating a `VolumeReplication` with `replicationState: primary` enables replication and the conditions settle at `Completed=True, Degraded=False` | Positive | —    |
| I-03 | Flipping to `secondary` drives demote, and back to `primary` drives promote, each waiting on `Completed`                                          | Positive | —    |
| I-04 | Deleting the `VolumeReplication` disables replication                                                                                             | Positive | —    |
| I-05 | A `VolumeReplicationClass` naming a nonexistent policy: enable fails, the condition carries the message, and the object retries without flapping  | Negative | —    |
| I-06 | `status.lastSyncTime` advances across reconciles while the mock backend advances its newest replicated snapshot                                   | Positive | —    |
| I-07 | Driver restart mid-operation: the re-driven verb is idempotent and the conditions re-derive without manual repair                                 | Boundary | —    |

---

## 3. E2E Tests

Two live simplyblock clusters with the chart-deployed csi-addons machinery. The Ramen VRG rows are the Phase 2 acceptance gate.

### Adapter Lifecycle (design §5, §6)

| #    | Scenario                                                                                                                                  | Type       | Test |
|------|-------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| E-01 | Enable through a `VolumeReplication`, write data, and `lastSyncTime` advances at the policy cadence                                       | Positive   | —    |
| E-02 | Forced promote on the DR cluster: the clone serves with the source fenced, and data matches the last replicated generation                | Positive   | —    |
| E-03 | Resync back after a forced promote, then a planned swap (demote, then planned promote): a hashed writer proves zero loss across the swap  | Positive   | —    |
| E-04 | Planned promote refused while lagging, succeeds after `Resyncing` clears                                                                  | Negative   | —    |
| E-05 | The typed status read never 404s across the whole lifecycle (enable through swap), and the slot controller's `lastReplicatedAt` tracks it | Regression | —    |

### Ramen-Driven (design §12, Phase 2 gate)

| #    | Scenario                                                                                                                                     | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-06 | A Ramen `VolumeReplicationGroup` in async mode selects the class, creates one `VolumeReplication` per PVC, and `lastGroupSyncTime` populates | Positive | —    |
| E-07 | Ramen failover (`force`) and relocate (demote plus planned promote) both complete against a live workload                                    | Positive | —    |

---

## 4. Manual Scenarios and Test Concepts

### M-01 — Demote with writes in flight

**Design reference:** §5.2, Open Question 1

**What to verify:** the demote verb's quiesce behavior when the workload is still writing at the moment of the fence, since Ramen ordinarily unmounts first but nothing guarantees it.

**Test concept:**
1. Run a continuous writer against a replicated volume.
2. Issue demote without stopping the writer.
3. Verify the final flush lands on the peer, the writer's in-flight I/O fails cleanly (no acknowledged-but-lost write), and a subsequent planned promote on the peer serves every acknowledged write.

### M-02 — Coexistence under concurrent claims

**Design reference:** §8

**What to verify:** the one-owner rule under a race: the annotation and a `VolumeReplication` claiming the same volume in the same reconcile window must converge on one owner with the other visibly refused, never two attach calls.

**Test concept:**
1. Apply the PVC annotation and create a `VolumeReplication` for the same PVC near-simultaneously.
2. Verify exactly one path attached, the other surfaced its refusal (event or condition), and the backend saw a single policy attach.

---

## 5. Axis Coverage

| Axis             | Values covered                                                         | IDs                                   | Not covered                                               |
|------------------|------------------------------------------------------------------------|---------------------------------------|-----------------------------------------------------------|
| Verb lifecycle   | enable, disable, info, forced promote, planned promote, demote, resync | U-01 … U-17, I-02 … I-04, E-01 … E-04 | —                                                         |
| Idempotency      | repeat enable, disable, promote, demote; re-drive after restart        | U-02, U-05, U-11, U-15, I-07          | repeated resync                                           |
| Conditions       | healthy, degraded, error, staleness, resyncing, disabled               | U-18 … U-22, E-05                     | condition behavior across backend upgrade                 |
| Coexistence      | slot skip, one-owner refusal, concurrent claim                         | U-26, U-27, M-02                      | migration of an annotated volume onto a VolumeReplication |
| peerClasses      | verified, missing class, mispaired policies                            | U-23 … U-25                           | drift after verification                                  |
| Orchestrator     | direct kubectl lifecycle, Ramen VRG async                              | I-02 … I-06, E-06, E-07               | Ramen hub failover of multiple apps                       |
| Cluster topology | two clusters, one relationship addressed from both sides               | E-01 … E-07                           | three-cluster (cascaded) topologies                       |

---

## 6. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 27        | 0       | U-01 … U-27 |
| Integration | 7         | 0       | I-01 … I-07 |
| E2E         | 7         | 0       | E-01 … E-07 |
| Manual      | 2         | 0       | M-01, M-02  |

Every scenario is uncovered because the design is Draft. The counts are the target, and each `Test` column fills in as the work lands.

---

## 7. What Is Not Yet Covered

| #           | Gap                                                                                                               | Reason                                                                                                                                      |
|-------------|-------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------|
| U-01 … U-27 | The verb adapter, condition derivation, preflight, and coexistence units                                          | The adapter does not exist; blocked on P0-1 and P0-2 for the Phase 1 verbs and P0-3 for demote                                              |
| I-01 … I-07 | The sidecar and controller-manager loop                                                                           | CRDs and controller-manager vendored in the chart (P0-5, `csiaddons.create`); blocked on the Phase 1 sidecar and driver Replication service |
| E-01 … E-07 | The live lifecycle and the Ramen gate                                                                             | Blocked on Phase 1 and 2 landing, plus a two-cluster test bed with Ramen dr-cluster installed for E-06 and E-07                             |
| —           | Repeated resync, class drift after verification, annotated-volume migration onto the adapter, cascaded topologies | Beyond the first coverage pass, recorded so the gaps are explicit rather than assumed covered                                               |
| M-01, M-02  | Demote under writes; concurrent ownership race                                                                    | Need failure injection and precise timing a live two-cluster run does not automate yet                                                      |
