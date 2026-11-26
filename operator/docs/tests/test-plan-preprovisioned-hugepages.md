# Test Plan: Preprovisioned Huge-Page Mode

Related design: [`designs/design-preprovisioned-hugepages.md`](../designs/design-preprovisioned-hugepages.md)

Scope: the operator's CRD surface, controller, and webhooks. The control plane's
`/cluster/sizing/hugepages-estimate` endpoint (design §7) and `calculate_minimum_hp_memory`
itself are `sbcli`'s dependency, faked at the boundary. This plan asserts the operator's
behavior against a stub, never the formula's correctness.

Scenario IDs are permanent: `U-` unit (no cluster: pure functions, fake `client.Client`,
stub `controlplane.Client`), `I-` integration (full reconcile loop against `envtest` + a mock
`sbcli` HTTP server), `E-` end-to-end (live cluster, real data path), `M-` manual (needs
failure injection or orchestration not yet automated). Types are `Positive`, `Negative`,
`Boundary`, `Regression`. The `Test` column names the implementing function, or `—` when the
scenario is not yet covered. Every `—` also appears in §8.

None of this design is implemented yet (design status: Draft), so every row below is `—`.
The matrix states what has to be proven once it is.

---

## 1. Unit Tests

No cluster: pure functions over `discovery.KubeNode`/`corev1.Pod` fakes and a stub
`controlplane.Client` that returns a fixed `EstimateHugePages` result or a fixed error.

### Required Huge Pages (§5 Step 1)

File: `internal/hugepages/precheck_test.go`

| #    | Scenario                                                                                  | Type     | Test |
|------|-------------------------------------------------------------------------------------------|----------|------|
| U-01 | Formula result below the `minHugePagesSizeMiB` floor: the floor wins                      | Positive | —    |
| U-02 | Formula result above the floor: the formula wins                                          | Positive | —    |
| U-03 | No floor set (`minHugePagesSizeMiB == 0`): the bare formula result is used                | Positive | —    |
| U-04 | Rounding: an odd-MiB formula result rounds down to the nearest even MiB, then +512        | Boundary | —    |
| U-05 | `cpClient.EstimateHugePages` returns an error: `Evaluate` returns it, no partial `Result` | Negative | —    |

### Capacity and Contention (§5 Steps 2-3)

File: `internal/hugepages/precheck_test.go`

| #    | Scenario                                                                                                                       | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-06 | Worker with no `hugepages-2Mi` entry in `KubeNode.HugePages`                                                                   | Negative | —    |
| U-07 | Worker's `AllocatableBytes` below the required MiB                                                                             | Negative | —    |
| U-08 | Worker's `AllocatableBytes` exactly equal to the required MiB, no other pods: passes (inclusive, not strict `>`)               | Boundary | —    |
| U-09 | Worker with enough capacity, zero other pods on it: passes                                                                     | Positive | —    |
| U-10 | Worker with enough capacity, one unrelated pod requesting `hugepages-2Mi` that leaves exactly the required amount free: passes | Boundary | —    |
| U-11 | Same, but the unrelated pod's request leaves one MiB less than required: `HugePagesContended`, naming that pod                 | Negative | —    |
| U-12 | A pod requesting `hugepages-2Mi` that belongs to this cluster's own storage nodes is excluded from the contention sum          | Negative | —    |
| U-13 | Multiple candidate workers, one failing and one passing: `Result` reports both independently                                   | Positive | —    |
| U-14 | Zero candidate workers: `Evaluate` returns an empty, non-failing `Result`                                                      | Boundary | —    |

### Platform Capability Gate (§6.2)

File: `internal/platform/hugepages_test.go`

| #    | Function                 | Scenario                  | Test |
|------|--------------------------|---------------------------|------|
| U-15 | `HugePagesModeSupported` | `""` (unset) → `true`     | —    |
| U-16 | `HugePagesModeSupported` | `Preprovisioned` → `true` | —    |
| U-17 | `HugePagesModeSupported` | `Exclusive` → `false`     | —    |
| U-18 | `HugePagesModeSupported` | `Shared` → `false`        | —    |

### `StorageNode` Admission (§6.1, §6.2)

File: `internal/webhook/storagenode_validator_test.go`

| #    | Scenario                                                                                                                   | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-19 | Create with `hugePagesMode: Exclusive` (via the owning cluster): denied, citing §6.2's message                             | Negative | —    |
| U-20 | Create with `hugePagesMode: Shared`: denied                                                                                | Negative | —    |
| U-21 | Create with `hugePagesMode: Preprovisioned`, named worker fails Step 2: denied, citing `InsufficientHugePages`             | Negative | —    |
| U-22 | Create with `hugePagesMode: Preprovisioned`, named worker passes: admitted                                                 | Positive | —    |
| U-23 | `hugePagesMode` unset: admitted without calling `hugepages.Evaluate` at all                                                | Positive | —    |
| U-28 | `cpClient.EstimateHugePages` unreachable or erroring during a `Preprovisioned` create: denied (fail closed, design §13 Q1) | Negative | —    |

### `ClusterDeploymentConfig` / `StorageCluster` Admission (§6.2)

Files: `internal/webhook/clusterdeploymentconfig_validator_test.go`, `internal/webhook/storagecluster_validator_test.go`

| #    | Scenario                                                                                                         | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------|----------|------|
| U-24 | `ClusterDeploymentConfig` create, `spec.cluster.hugePagesMode: Exclusive`: denied                                | Negative | —    |
| U-25 | `ClusterDeploymentConfig` create, a node set override of `hugePagesMode: Shared` with the template unset: denied | Negative | —    |
| U-26 | `StorageCluster` create, `spec.storageNodes.hugePagesMode: Exclusive`: denied                                    | Negative | —    |
| U-27 | `StorageCluster` update flipping `hugePagesMode` from `Preprovisioned` to `Exclusive`: denied                    | Negative | —    |

---

## 2. Integration Tests

Run the full `ClusterDeploymentConfig` reconcile loop against `envtest` and a mock `sbcli`
HTTP server standing in for §7's endpoint.

### Precheck Blocks Expansion (§5, §6.1)

File: `internal/controllers/deployment/clusterdeploymentconfig_controller_test.go`

| #    | Scenario                                                                                                                                                                           | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| I-01 | `Preprovisioned` document whose node set resolves to an undersized worker: `HugePagesReady=False`, no `StorageCluster`/`StorageNode` created, phase stays short of `CreatingNodes` | Negative | —    |
| I-02 | Same document, worker corrected (test re-applies with enough capacity): `HugePagesReady` flips to `True` on the next reconcile, expansion proceeds                                 | Positive | —    |
| I-03 | Document with `hugePagesMode` unset: expansion proceeds without ever calling the mock `sbcli` server                                                                               | Positive | —    |
| I-04 | Mock `sbcli` server returns an error from `/cluster/sizing/hugepages-estimate`: reconcile requeues, `HugePagesReady` is left at its prior value (never flips to `True`)            | Negative | —    |

---

## 3. E2E Tests

Live cluster, one worker whose `hugepages-2Mi` is deliberately undersized or deliberately
held by an unrelated pod.

### Live Precheck Outcome (§5)

| #    | Scenario                                                                                                                                                           | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | Worker with `hugepages-2Mi` capacity below the cluster's sizing: `Preprovisioned` document never produces a `StorageNode`, condition names `InsufficientHugePages` | Negative | —    |
| E-02 | Worker with enough capacity, a deliberately scheduled unrelated pod holding the difference: condition names `HugePagesContended` and that pod                      | Negative | —    |
| E-03 | Worker with enough free capacity: `StorageNode` is created and its SPDK pod schedules on the first attempt                                                         | Positive | —    |

---

## 4. Manual Scenarios and Test Concepts

### M-01 — A pod is scheduled onto the worker between the `ClusterDeploymentConfig` check and the actual node add

**Design reference:** §6.1

**What to verify:** the `StorageNode` admission webhook's own evaluation (§6.1's second call site) catches a worker that passed the document's check but has since been contended, rather than relying on the document's now-stale condition.

**Current behavior:** not implemented. §6.1 states the intent but nothing exercises the race.

**Test concept:**
1. Approve a `Preprovisioned` document against a worker with exactly enough free capacity.
2. Before the expansion creates the `StorageNode`, schedule an unrelated pod on that worker requesting enough `hugepages-2Mi` to leave less than required.
3. Assert the `StorageNode` create this cluster's expansion issues is denied with `HugePagesContended`, even though the document's own `HugePagesReady` condition, set before step 2, still reads `True`.

~~### M-02 — `/cluster/sizing/hugepages-estimate` is unreachable at `StorageNode` admission time~~

Superseded by U-28: design §13 Q1 resolved to fail closed, and the scenario needs no live cluster or failure injection, so it moved into §1 as an ordinary negative unit case rather than staying a manual one.

---

## 5. Axis Coverage

| Axis                 | Values covered                                              | IDs              | Not covered                                                                                   |
|----------------------|-------------------------------------------------------------|------------------|-----------------------------------------------------------------------------------------------|
| Node count per check | 0, 1, many                                                  | U-14, U-09, U-13 | —                                                                                             |
| Contention source    | none, one pod, this cluster's own pod (excluded)            | U-09, U-11, U-12 | multiple contending pods summed together                                                      |
| Mode value           | unset, `Preprovisioned`, `Exclusive`, `Shared`              | U-15–U-18, U-23  | —                                                                                             |
| Check site           | `ClusterDeploymentConfig` controller, `StorageNode` webhook | I-01, U-21       | both sites disagreeing on one worker at the same instant (M-01 covers the sequential version) |
| Cluster topology     | single worker (E-01–E-03)                                   | E-01             | multi-worker document where only one of several fails the check                               |

---

## 6. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 28        | 0       | U-01 … U-28 |
| Integration | 4         | 0       | I-01 … I-04 |
| E2E         | 3         | 0       | E-01 … E-03 |
| Manual      | 1         | 0       | M-01        |

---

## 7. What Is Not Yet Covered

| #           | Gap                                           | Reason                                                                                                                                                                    |
|-------------|-----------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-01 … U-28 | All unit scenarios                            | §5/§6 not implemented yet (design status: Draft)                                                                                                                          |
| I-01 … I-04 | All integration scenarios                     | `ClusterDeploymentConfig` controller does not yet call `hugepages.Evaluate`                                                                                               |
| E-01 … E-03 | All E2E scenarios                             | No implementation to run against a live cluster                                                                                                                           |
| M-01        | The admission-time race                       | Needs the `StorageNode` webhook's evaluation implemented first                                                                                                            |
| ~~M-02~~    | ~~Backend-unreachable behavior at admission~~ | Superseded by U-28 now that design §13 Q1 is resolved                                                                                                                     |
| —           | Multiple contending pods summed together      | Not enumerated as its own scenario. Step 3's sum (§5) is linear in the number of pods, so U-11 is taken to generalize, but no row asserts two pods summed rather than one |
| —           | Multi-worker document, partial failure        | Not enumerated. I-01 covers a single-worker document only                                                                                                                 |
