# Test Plan: Preprovisioned Huge-Page Mode

Related design: [`designs/design-preprovisioned-hugepages.md`](../designs/design-preprovisioned-hugepages.md)

Scope: Phase 1 (design §5, §6) is the operator's own CRD surface, controller, and webhooks,
asserted against `/cluster/sizing/hugepages-estimate` faked at the boundary; this plan asserts
the operator's behavior against a stub, never the formula's correctness. Phases 2 (design §14)
and 3 (design §15) additionally cover `sbcli`'s node-add Job (`_provision_talos_hugepages`, the
OpenShift `MachineConfig` patch), asserted directly, since that is where the design places the
mechanism; the operator's own suites assert only the platform-aware admission decision around it.

Scenario IDs are permanent: `U-` unit (no cluster: pure functions, fake `client.Client`,
stub `controlplane.Client`), `I-` integration (full reconcile loop against `envtest` + a mock
`sbcli` HTTP server), `E-` end-to-end (live cluster, real data path), `M-` manual (needs
failure injection or orchestration not yet automated). Types are `Positive`, `Negative`,
`Boundary`, `Regression`. The `Test` column names the implementing function, or `—` when the
scenario is not yet covered. Every `—` also appears in this plan's own §8.

Design Phase 1 is implemented; its rows below carry real test names. Design Phases 2 and 3 are
Planned, so every row under §1.4 onward, §2.2, §3.2–§3.3, and §4 is `—`.

---

## 1. Unit Tests

No cluster: pure functions over `discovery.KubeNode`/`corev1.Pod` fakes and a stub
`controlplane.Client` that returns a fixed `EstimateHugePages` result or a fixed error.

### 1.1 Required Huge Pages (design §5 Step 1)

File: `internal/hugepages/precheck_test.go`

| #    | Scenario                                                                                                       | Type     | Test                        |
|------|----------------------------------------------------------------------------------------------------------------|----------|-----------------------------|
| U-01 | Formula result below the `minHugePagesSizeMiB` floor: the floor wins                                           | Positive | `TestEvaluateRequiredMiB`   |
| U-02 | Formula result above the floor: the formula wins                                                               | Positive | `TestEvaluateRequiredMiB`   |
| U-03 | No floor set (`minHugePagesSizeMiB == 0`): the bare formula result is used                                     | Positive | `TestEvaluateRequiredMiB`   |
| U-04 | Rounding: an odd-MiB formula result rounds down to the nearest even MiB, then +512                             | Boundary | `TestEvaluateRequiredMiB`   |
| U-05 | `cpClient.EstimateHugePages` returns an error: `Evaluate` returns it, no partial `Result`                      | Negative | `TestEvaluateEstimateError` |
| U-31 | `MinHugePagesSizeMiB` parses an unset, decimal (`100G`), and binary-decimal (`1T`) `MinHugePagesSize` into MiB | Positive | `TestMinHugePagesSizeMiB`   |

### 1.2 Capacity and Contention (design §5 Steps 2-3)

File: `internal/hugepages/precheck_test.go`

| #    | Scenario                                                                                                                                         | Type     | Test                                |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------|----------|-------------------------------------|
| U-06 | Worker with no `hugepages-2Mi` entry in `KubeNode.HugePages`                                                                                     | Negative | `TestEvaluateCapacityAndContention` |
| U-07 | Worker's `AllocatableBytes` below the required MiB                                                                                               | Negative | `TestEvaluateCapacityAndContention` |
| U-08 | Worker's `AllocatableBytes` exactly equal to the required MiB, no other pods: passes (inclusive, not strict `>`)                                 | Boundary | `TestEvaluateCapacityAndContention` |
| U-09 | Worker with enough capacity, zero other pods on it: passes                                                                                       | Positive | `TestEvaluateCapacityAndContention` |
| U-10 | Worker with enough capacity, one unrelated pod requesting `hugepages-2Mi` that leaves exactly the required amount free: passes                   | Boundary | `TestEvaluateCapacityAndContention` |
| U-11 | Same, but the unrelated pod's request leaves one MiB less than required: `HugePagesContended`, naming that pod                                   | Negative | `TestEvaluateCapacityAndContention` |
| U-12 | A pod requesting `hugepages-2Mi` that belongs to this cluster's own storage nodes (`simplyblock.io/cluster`) is excluded from the contention sum | Negative | `TestEvaluateCapacityAndContention` |
| U-13 | Multiple candidate workers, one failing and one passing: `Result` reports both independently                                                     | Positive | `TestEvaluateCapacityAndContention` |
| U-14 | Zero candidate workers: `Evaluate` returns an empty, non-failing `Result`                                                                        | Boundary | `TestEvaluateCapacityAndContention` |

### 1.3 Platform Capability Gate (design §6.2)

File: `internal/platform/hugepages_test.go`

| #    | Function                 | Scenario                                                                   | Test                         |
|------|--------------------------|----------------------------------------------------------------------------|------------------------------|
| U-15 | `HugePagesModeSupported` | `""` (unset) → `true`                                                      | `TestHugePagesModeSupported` |
| U-16 | `HugePagesModeSupported` | `Preprovisioned` → `true`                                                  | `TestHugePagesModeSupported` |
| U-17 | `HugePagesModeSupported` | `Exclusive`, zero-value `Platform` (neither Talos nor OpenShift) → `false` | `TestHugePagesModeSupported` |
| U-18 | `HugePagesModeSupported` | `Shared`, zero-value `Platform` → `false`                                  | `TestHugePagesModeSupported` |

### 1.4 Platform Capability Gate, Per Platform (design §6.2, Phase 2/3 — Planned)

Added once `HugePagesModeSupported` gains the `Platform` parameter (design §6.2). The existing
U-17/U-18 above become the "off-platform" case of each mode; these are the "on-platform" and
"wrong-platform" cases.

| #    | Function                 | Scenario                                                            | Type     | Test |
|------|--------------------------|---------------------------------------------------------------------|----------|------|
| U-32 | `HugePagesModeSupported` | `Exclusive`, `Platform{Talos: true}` → `true`                       | Positive | —    |
| U-33 | `HugePagesModeSupported` | `Shared`, `Platform{OpenShift: true}` → `true`                      | Positive | —    |
| U-34 | `HugePagesModeSupported` | `Exclusive`, `Platform{OpenShift: true}` (wrong platform) → `false` | Negative | —    |
| U-35 | `HugePagesModeSupported` | `Shared`, `Platform{Talos: true}` (wrong platform) → `false`        | Negative | —    |

### 1.5 `StorageNode` Admission (design §6.1, design §6.2)

File: `internal/webhook/storagenode_validator_test.go`

| #    | Scenario                                                                                                                    | Type     | Test                                                   |
|------|-----------------------------------------------------------------------------------------------------------------------------|----------|--------------------------------------------------------|
| U-19 | Create with `hugePagesMode: Exclusive` (via the owning cluster), off Talos: denied, citing design §6.2's message            | Negative | `TestExclusiveAndSharedModesAreDeniedAtNodeAdmission`  |
| U-20 | Create with `hugePagesMode: Shared`, off OpenShift: denied                                                                  | Negative | `TestExclusiveAndSharedModesAreDeniedAtNodeAdmission`  |
| U-21 | Create with `hugePagesMode: Preprovisioned`, named worker fails Step 2: denied, citing `InsufficientHugePages`              | Negative | `TestPreprovisionedNodeWithAnUndersizedWorkerIsDenied` |
| U-22 | Create with `hugePagesMode: Preprovisioned`, named worker passes: admitted                                                  | Positive | `TestPreprovisionedNodeWithAReadyWorkerIsAdmitted`     |
| U-23 | `hugePagesMode` unset: admitted without calling `hugepages.Evaluate` at all (a `nil` `ControlPlane` would panic if it were) | Positive | `TestHugePagesModeUnsetSkipsThePrecheckEntirely`       |
| U-28 | `cpClient.EstimateHugePages` unreachable or erroring during a `Preprovisioned` create: denied (fail closed, design §13 Q1)  | Negative | `TestControlPlaneErrorDuringPrecheckDeniesTheCreate`   |

### 1.6 `ClusterDeploymentConfig` / `StorageCluster` Admission (design §6.2)

Files: `internal/webhook/clusterdeploymentconfig_validator_test.go`, `internal/webhook/storagecluster_validator_test.go`

| #    | Scenario                                                                                                         | Type     | Test                                                          |
|------|------------------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------------------|
| U-24 | `ClusterDeploymentConfig` create, `spec.cluster.hugePagesMode: Exclusive`: denied even as a draft                | Negative | `TestADraftWithAnUnsupportedHugePagesModeIsDenied`            |
| U-25 | `ClusterDeploymentConfig` create, a node set override of `hugePagesMode: Shared` with the template unset: denied | Negative | `TestANodeSetOverrideWithAnUnsupportedHugePagesModeIsDenied`  |
| U-26 | `StorageCluster` create, `spec.storageNodes.hugePagesMode: Exclusive`: denied                                    | Negative | `TestAClusterWithAnUnsupportedHugePagesModeIsDenied`          |
| U-27 | `StorageCluster` update flipping `hugePagesMode` from `Preprovisioned` to `Exclusive`: denied                    | Negative | `TestAnUpdateFlippingHugePagesModeToAnUnsupportedOneIsDenied` |
| U-29 | `ClusterDeploymentConfig` create, `spec.cluster.hugePagesMode: Preprovisioned`: admitted like any other field    | Positive | `TestADraftWithPreprovisionedHugePagesModeIsAdmitted`         |
| U-30 | `StorageCluster` create, `spec.storageNodes.hugePagesMode: Preprovisioned`: admitted like any other field        | Positive | `TestAClusterWithPreprovisionedHugePagesModeIsAdmitted`       |

### 1.7 Talos Huge-Page Mode Split (design §4.3, design §14 — Planned)

File: `sbcli/tests/unit/test_talos_hugepages.py`

| #    | Scenario                                                                                                                                                                                                 | Type       | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| U-36 | `_talos_hugepages_managed()` returns `True` for `TALOS_HUGEPAGES_MODE=exclusive`                                                                                                                         | Positive   | —    |
| U-37 | `_talos_hugepages_managed()` returns `False` for `TALOS_HUGEPAGES_MODE=preprovisioned` and for unset                                                                                                     | Positive   | —    |
| U-38 | `_talos_hugepages_managed()` returns `False` with a warning for an unrecognized value                                                                                                                    | Negative   | —    |
| U-39 | `_provision_talos_hugepages` reads `_talos_hugepages_managed()`, not `_talos_machine_config_managed()`: `TALOS_HUGEPAGES_MODE=exclusive` with `TALOS_MACHINE_CONFIG=preprovisioned` still grows the pool | Positive   | —    |
| U-40 | `_configure_talos_kubelet_topology` is unaffected by `TALOS_HUGEPAGES_MODE`: it keeps reading `TALOS_MACHINE_CONFIG` alone                                                                               | Regression | —    |

### 1.8 `HugePagesMode`-to-`TalosMachineConfig` Default (design §12 — Planned)

File: `internal/controllers/cluster/expansion_test.go` (or the file the propagation step
`storagesitedeployment_controller.go:268-269`/`expansion.go:338,792` already has tests in)

| #    | Scenario                                                                                                              | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------------------|----------|------|
| U-41 | Talos cluster, `HugePagesMode` unset, `TalosMachineConfig: Managed`: defaults to `Exclusive`                          | Positive | —    |
| U-42 | Talos cluster, `HugePagesMode` unset, `TalosMachineConfig` unset or `Preprovisioned`: defaults to `Preprovisioned`    | Positive | —    |
| U-43 | Talos cluster, `HugePagesMode` explicitly set: the default never overwrites it, whatever `TalosMachineConfig` carries | Negative | —    |
| U-44 | Non-Talos cluster: the default never runs, `HugePagesMode` stays unset                                                | Negative | —    |

### 1.9 OpenShift `MachineConfig` Kernel Argument (design §15.1–design §15.2 — Planned)

File: `sbcli/tests/unit/test_openshift_hugepages.py`

| #    | Scenario                                                                                                                                                         | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-45 | `HugePagesMode: Shared`: the rendered `MachineConfig` carries `spec.kernelArguments` with `hugepagesz=2M` and `hugepages=<HUGEPAGES_TOTAL_PAGES>`                | Positive | —    |
| U-46 | `HugePagesMode` unset or `Preprovisioned`: the rendered `MachineConfig` carries no `kernelArguments`, exactly as today                                           | Negative | —    |
| U-47 | `HugePagesMode: Shared` with CPU topology disabled (`cpu_topology_enabled=False`, `core_isolate=False`): `wants_host_config` is still `True`, the Job still runs | Boundary | —    |
| U-48 | `HugePagesMode: Shared`, CPU topology also enabled: one `MachineConfig` carries both the kubelet-topology Ignition content and the kernel argument, one reboot   | Positive | —    |

---

## 2. Integration Tests

### 2.1 Precheck Blocks Expansion (design §5, design §6.1 — Phase 1, Implemented)

Run the `ClusterDeploymentConfig` reconciler's own step methods (`hugePagesReady`,
`performStep`) directly against a fake `client.Client` and a stub `controlplane.Client`, per
the suite's own header comment — not, as originally planned, against `envtest` and a mock HTTP
server. The method-level harness asserts the same contract (a stub satisfying
`hugepages.EstimateHugePagesClient`, no real server) at lower cost, since the reconciler never
calls the mock server through anything `envtest` adds.

File: `internal/controllers/deployment/hugepages_gate_test.go`

| #    | Scenario                                                                                                                                                                                      | Type     | Test                                                                                                          |
|------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|---------------------------------------------------------------------------------------------------------------|
| I-01 | `Preprovisioned` document whose node set resolves to an undersized worker: `HugePagesReady=False`, `stepCreatingCluster` does not finish, phase stays short of `CreatingNodes`                | Negative | `TestHugePagesGateHoldsOnAnUndersizedWorker`, `TestPerformStepDoesNotCreateTheClusterWhileHugePagesGateHolds` |
| I-02 | Same document, worker corrected in place (status updated with enough capacity): `HugePagesReady` flips to `True` on the next call, expansion proceeds                                         | Positive | `TestHugePagesGateFlipsToReadyOnceTheWorkerIsCorrected`                                                       |
| I-03 | Document with `hugePagesMode` unset: expansion proceeds without ever calling the stub `controlplane.Client`, and writes no condition                                                          | Positive | `TestHugePagesGateSkipsEntirelyWhenModeIsUnset`                                                               |
| I-04 | Stub `controlplane.Client` returns an error: `hugePagesReady` returns it, `HugePagesReady` is left at its prior value (never flips to `True`)                                                 | Negative | `TestHugePagesGateLeavesTheConditionOnAControlPlaneError`                                                     |
| I-05 | A growth document (`spec.clusterRef` naming an existing `StorageCluster`, no `spec.cluster` of its own) reads `HugePagesMode` off that `StorageCluster`, not off a template it does not carry | Positive | `TestHugePagesGateReadsModeOffTheExistingClusterForAGrowthDocument`                                           |

### 2.2 Platform-Aware Admission, End to End (design §6.2, Phase 2/3 — Planned)

Against `envtest`, once the `Platform` parameter lands (§1.4): a `ClusterDeploymentConfig` or
`StorageCluster` create is admitted or denied per the combination of its declared platform and
its requested mode.

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| I-06 | `Environment: Talos`, `hugePagesMode: Exclusive`: admitted                    | Positive | —    |
| I-07 | `Environment: OpenShift`, `hugePagesMode: Shared`: admitted                   | Positive | —    |
| I-08 | `Environment: Talos`, `hugePagesMode: Shared`: denied (wrong platform)        | Negative | —    |
| I-09 | `Environment: OpenShift`, `hugePagesMode: Exclusive`: denied (wrong platform) | Negative | —    |

---

## 3. E2E Tests

### 3.1 Live Precheck Outcome (design §5 — Phase 1, Implemented, Not Yet Automated)

Live cluster, one worker whose `hugepages-2Mi` is deliberately undersized or deliberately
held by an unrelated pod.

| #    | Scenario                                                                                                                                                           | Type     | Test |
|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | Worker with `hugepages-2Mi` capacity below the cluster's sizing: `Preprovisioned` document never produces a `StorageNode`, condition names `InsufficientHugePages` | Negative | —    |
| E-02 | Worker with enough capacity, a deliberately scheduled unrelated pod holding the difference: condition names `HugePagesContended` and that pod                      | Negative | —    |
| E-03 | Worker with enough free capacity: `StorageNode` is created and its SPDK pod schedules on the first attempt                                                         | Positive | —    |

### 3.2 Live `Exclusive` Outcome, Talos (design §14 — Planned)

| #    | Scenario                                                                                                                                                                              | Type     | Test |
|------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-04 | A Talos worker whose pool is short: `Exclusive` grows it through `talosctl`, the `StorageNode` is created, and its SPDK pod schedules on the first attempt                            | Positive | —    |
| E-05 | The same worker already carries a user's own huge pages (a baseline set before simplyblock ever ran): the grown pool adds simplyblock's pages on top rather than replacing the user's | Positive | —    |
| E-06 | The Talos API access Secret is missing: the node add fails naming it, before any Job runs                                                                                             | Negative | —    |

### 3.3 Live `Shared` Outcome, OpenShift (design §15 — Planned)

| #    | Scenario                                                                                                                                                                                                             | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| E-07 | An OpenShift worker with no prior huge-page reservation: `Shared` patches the storage `MachineConfig`'s kernel arguments, the worker reboots, and the `StorageNode` is created once it comes back                    | Positive | —    |
| E-08 | A second, unrelated pod on the same worker also requests `hugepages-2Mi` after the reboot: the pool is shared, and the precheck's own contention accounting (design §5 Step 3) still applies to later adds           | Positive | —    |
| E-09 | The kernel argument lands unevenly across NUMA nodes (P0-2, design §13 Q3): the per-NUMA capacity check (design §15 Step 4) fails the add naming the short socket, rather than starting SPDK against a lopsided pool | Negative | —    |

---

## 4. Manual Scenarios and Test Concepts

### M-01 — A pod is scheduled onto the worker between the `ClusterDeploymentConfig` check and the actual node add

**Design reference:** §6.1

**What to verify:** the `StorageNode` admission webhook's own evaluation (design §6.1's second call site) catches a worker that passed the document's check but has since been contended, rather than relying on the document's now-stale condition.

**Current behavior:** not implemented. Design §6.1 states the intent but nothing exercises the race.

**Test concept:**
1. Approve a `Preprovisioned` document against a worker with exactly enough free capacity.
2. Before the expansion creates the `StorageNode`, schedule an unrelated pod on that worker requesting enough `hugepages-2Mi` to leave less than required.
3. Assert the `StorageNode` create this cluster's expansion issues is denied with `HugePagesContended`, even though the document's own `HugePagesReady` condition, set before step 2, still reads `True`.

~~### M-02 — `/cluster/sizing/hugepages-estimate` is unreachable at `StorageNode` admission time~~

Superseded by U-28: design §13 Q1 resolved to fail closed, and the scenario needs no live cluster or failure injection, so it moved into §1 as an ordinary negative unit case rather than staying a manual one.

### M-03 — The OpenShift reboot wave lands between the `MachineConfig` patch and `_wait_for_kubelet_hugepages`'s own total-capacity check

**Design reference:** §15.3, §15.4

**What to verify:** the per-NUMA capacity check (design §15 Step 4) is what actually gates SPDK start, not the pre-existing total-capacity wait (`_wait_for_kubelet_hugepages`) that a cluster-wide kernel argument can satisfy while leaving one socket short.

**Current behavior:** not implemented. Design §15.4 states the intent; nothing exercises a lopsided placement on a real multi-socket worker, because P0-2 (design §13 Q3) is itself unverified.

**Test concept:**
1. On a multi-socket OpenShift worker, apply the `hugepagesz=2M hugepages=N` kernel argument through the storage `MachineConfig` with `N` sized for an even split.
2. After the reboot, read `/proc/meminfo`'s `HugeTLB` total alongside each NUMA node's own `nr_hugepages`; if the kernel happened to split evenly, force an uneven one by pinning memory on one socket before the reboot (`numactl --membind`).
3. Assert the node add fails naming the short socket, rather than starting SPDK once the cluster-wide total alone clears `_wait_for_kubelet_hugepages`.

### M-04 — A Talos worker's huge-page pool is shrunk out from under `Exclusive` by a reboot that resets `machine.sysfs`

**Design reference:** §14.3 (what does not change: `_talos_managed_pool`'s user-baseline arithmetic)

**What to verify:** `_talos_managed_pool`'s baseline tracking, reused unchanged under `Exclusive`, still recovers correctly when a Talos worker reboots with its machine config applied (expected) and nothing yet reflects it in `/proc/meminfo` (racing the node add).

**Current behavior:** not implemented; this is the same race `_wait_for_talos_pool` already waits out under `talosMachineConfig: Managed`, carried over unchanged, so it needs re-verification only to confirm `Exclusive` reaches the identical code path, not new behavior.

**Test concept:**
1. Add a Talos `StorageNode` with `hugePagesMode: Exclusive` while deliberately rebooting the worker moments after the `patch` Job completes.
2. Assert `_wait_for_talos_pool` blocks until the pool reappears post-reboot rather than timing out, and that `_save_talos_pool` records the same total it would have without the reboot.

---

## 5. Axis Coverage

| Axis                  | Values covered                                                                          | IDs                                  | Not covered                                                                                   |
|-----------------------|-----------------------------------------------------------------------------------------|--------------------------------------|-----------------------------------------------------------------------------------------------|
| Node count per check  | 0, 1, many                                                                              | U-14, U-09, U-13                     | —                                                                                             |
| Contention source     | none, one pod, this cluster's own pod (excluded)                                        | U-09, U-11, U-12                     | multiple contending pods summed together                                                      |
| Mode value            | unset, `Preprovisioned`, `Exclusive`, `Shared`                                          | U-15–U-18, U-23                      | —                                                                                             |
| Mode value × platform | `Exclusive`/Talos, `Shared`/OpenShift, each off its platform                            | U-32–U-35 (Planned)                  | —                                                                                             |
| Check site            | `ClusterDeploymentConfig` controller, `StorageNode` webhook                             | I-01, U-21                           | both sites disagreeing on one worker at the same instant (M-01 covers the sequential version) |
| Document shape        | template-carrying draft, growth document naming an existing cluster                     | I-01, I-05                           | —                                                                                             |
| Cluster topology      | single worker (E-01–E-03)                                                               | E-01                                 | multi-worker document where only one of several fails the check                               |
| Huge-page mechanism   | verify-only (Preprovisioned), Talos API (Exclusive), OpenShift `MachineConfig` (Shared) | E-01, E-04 (Planned), E-07 (Planned) | —                                                                                             |

---

## 6. Coverage Summary

| Class       | Scenarios | Covered | Not covered      |
|-------------|-----------|---------|------------------|
| Unit        | 48        | 31      | U-32 … U-48      |
| Integration | 9         | 5       | I-06 … I-09      |
| E2E         | 9         | 0       | E-01 … E-09      |
| Manual      | 3         | 0       | M-01, M-03, M-04 |

---

## 7. What Is Not Yet Covered

| #           | Gap                                             | Reason                                                                                                                                                                           |
|-------------|-------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-32 … U-35 | Per-platform `HugePagesModeSupported` cases     | `Platform` parameter not implemented yet (design §6.2, Phase 2/3)                                                                                                                |
| U-36 … U-40 | Talos huge-page/CPU-topology mode split         | `_talos_hugepages_managed` not implemented yet (design §4.3, §14)                                                                                                                |
| U-41 … U-44 | `HugePagesMode`-to-`TalosMachineConfig` default | The propagation-step default is not implemented yet (design §12)                                                                                                                 |
| U-45 … U-48 | OpenShift `MachineConfig` kernel argument       | `Shared` is not implemented yet (design §15)                                                                                                                                     |
| I-06 … I-09 | Platform-aware admission, end to end            | Depends on U-32 … U-35                                                                                                                                                           |
| E-01 … E-03 | Live Phase 1 scenarios                          | Implemented, but not yet automated against a live cluster                                                                                                                        |
| E-04 … E-09 | Live Phase 2/3 scenarios                        | `Exclusive`/`Shared` not implemented yet                                                                                                                                         |
| M-01        | The admission-time race                         | The `StorageNode` webhook's evaluation is implemented (§1.5); what is missing is the live orchestration to race it against the document's own check                              |
| M-03        | The OpenShift reboot/capacity-check race        | Needs `Shared` implemented first (design §15)                                                                                                                                    |
| M-04        | The Talos reboot/pool race under `Exclusive`    | Needs `Exclusive` implemented first (design §14); the underlying mechanism (`_wait_for_talos_pool`) is already exercised under `talosMachineConfig: Managed`                     |
| ~~M-02~~    | ~~Backend-unreachable behavior at admission~~   | Superseded by U-28 now that design §13 Q1 is resolved                                                                                                                            |
| —           | Multiple contending pods summed together        | Not enumerated as its own scenario. Step 3's sum (design §5) is linear in the number of pods, so U-11 is taken to generalize, but no row asserts two pods summed rather than one |
| —           | Multi-worker document, partial failure          | Not enumerated. I-01 covers a single-worker document only                                                                                                                        |
