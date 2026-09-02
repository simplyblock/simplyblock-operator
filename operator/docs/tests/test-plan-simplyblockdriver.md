# Test Plan: SimplyblockDriver

Related design: [`designs/crd-redesign/design-simplyblockdriver.md`](../designs/crd-redesign/design-simplyblockdriver.md)

Scope is the operator and the Kubernetes surface this repository builds. The CSI
driver's own behavior is out of scope and is `csi-driver/`'s to test: what a row
asserts is that the operator applies the right objects and reports the right
phase, never that the driver attaches a volume correctly.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason.

The kind is new, so every row is a specification rather than a gap against
shipped behavior. The chart installs the CSI driver today, and nothing about that
install is asserted here.

| Class       | Prefix | Harness                                                               |
|-------------|--------|-----------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions and a fake `client.Client`                 |
| Integration | `I-`   | Full reconcile loop against `envtest`                                 |
| E2E         | `E-`   | Live simplyblock deployment with a real kubelet and a real workload   |
| Manual      | `M-`   | Needs two versions, failure injection, or orchestration not automated |

---

## 1. Unit Tests

Single reconcile calls against a fake client. What the controller applies is a
pure function of the spec, which is why most of this document's behavior lands
here.

### What the Controller Applies (design §4.1)

File: `operator/internal/controllers/driver/simplyblockdriver_controller_unit_test.go`

| #    | Scenario                                                                           | Type     | Test |
|------|------------------------------------------------------------------------------------|----------|------|
| U-01 | A fresh object: the node DaemonSet, controller StatefulSet, and RBAC applied       | Positive | —    |
| U-42 | Every per-driver sidecar is applied whatever the cluster already runs              | Positive | —    |
| U-43 | Each sidecar receives this driver's socket as `--csi-address`                      | Positive | —    |
| U-44 | The `csi-snapshotter` sidecar is applied even where a `snapshot-controller` exists | Boundary | —    |
| U-02 | The core `CSIDriver` registration is created with `spec.driverName`                | Positive | —    |
| U-33 | `spec.driverName` reaches the node plugin's kubelet registration path              | Positive | —    |
| U-34 | It reaches the hostPath the node plugin mounts                                     | Positive | —    |
| U-35 | It reaches the snapshot class's `driver` field when snapshots are enabled          | Positive | —    |
| U-36 | A non-default `driverName`: no object carries the default alongside it             | Negative | —    |
| U-03 | Every applied object carries a controller reference                                | Positive | —    |
| U-04 | The cluster serves `snapshot.storage.k8s.io/v1`: no CRD and no controller applied  | Negative | —    |
| U-05 | The cluster does not serve it: the CRDs and a controller are applied               | Positive | —    |
| U-37 | The `VolumeSnapshotClass` for `spec.driverName` is applied in both cases           | Positive | —    |
| U-38 | An installed CRD and controller carry no controller reference                      | Negative | —    |
| U-39 | `status.snapshotSupport` is `Detected` where the API was already served            | Positive | —    |
| U-40 | `status.snapshotSupport` is `Installed` where the operator applied them            | Positive | —    |
| U-41 | Deleting the object leaves an installed CRD and controller in place                | Boundary | —    |
| U-06 | One image reaches both plugins, never two                                          | Positive | —    |
| U-07 | `controllerReplicas` reaches the controller StatefulSet                            | Positive | —    |
| U-08 | `nodeSelector` and `tolerations` reach the node DaemonSet and not the controller   | Positive | —    |
| U-09 | The resource blocks reach their own plugin and not the other                       | Boundary | —    |
| U-10 | A second reconcile applies nothing new                                             | Negative | —    |
| U-26 | The node `ConfigMap` carries the endpoint the namespace's `ControlPlane` publishes | Positive | —    |
| U-27 | It carries the credentials for that control plane, and not the endpoint alone      | Positive | —    |
| U-28 | The published endpoint changes: the `ConfigMap` is rewritten                       | Positive | —    |
| U-29 | No `ControlPlane` in the namespace: the objects are applied and the driver waits   | Boundary | —    |
| U-30 | A `ControlPlane` that is not `Ready`: applied and waiting, not `Failed`            | Boundary | —    |

`U-44` holds the boundary design §4.1 draws. The `csi-snapshotter` sidecar is part
of this driver's controller plugin, and the detection applies to the cluster's
`snapshot-controller`.

`U-38` and `U-41` are the pair design §4.1 turns on. What the operator installs
here is cluster-scoped and shared, so it is applied without ownership and stays
when the driver goes, which design §9 Q3 records as unfinished.

`U-33` to `U-36` are the four places the name appears. Design §9 Q3 records that
the chart writes three of them literally, so a plan that asserted only `U-02`
would pass against that behavior.

`U-03` is the row that keeps design §4.1's ownership claim true. An object without
a controller reference is an object deleting the `SimplyblockDriver` leaves
behind.

`U-29` and `U-30` are the pair that keeps a missing backend from being reported as
a broken driver. The plugins are applied either way, because a plugin that cannot
reach a control plane is in the same position as one that has not been scheduled
yet, and neither is a fault of this object.

### The Phase and Its Counts (design §3.3, §4.2)

File: `operator/internal/controllers/driver/simplyblockdriver_controller_unit_test.go`

| #    | Scenario                                                                             | Type     | Test |
|------|--------------------------------------------------------------------------------------|----------|------|
| U-11 | `status.nodesReady` and `nodesTotal` reflect the DaemonSet                           | Positive | —    |
| U-12 | All node plugins ready and the controller serving: `Ready`                           | Positive | —    |
| U-13 | One node plugin of three not ready: `Degraded`, not `Unavailable`                    | Boundary | —    |
| U-14 | Zero ready node plugins while the controller serves: `Degraded`                      | Boundary | —    |
| U-15 | The controller plugin not running: `Unavailable`, whatever the node plugins do       | Negative | —    |
| U-16 | `status.nodesReady` is 0 and present, never omitted                                  | Boundary | —    |
| U-17 | A `nodeSelector` that matches no worker: `nodesTotal` is 0, reported not failed      | Boundary | —    |
| U-18 | `status.controllerReady` false while `nodesReady` equals `nodesTotal`: `Unavailable` | Boundary | —    |
| U-19 | `status.observedGeneration` matches `metadata.generation` after a reconcile          | Positive | —    |

`U-14` and `U-15` are the pair design §4.2 turns on. Every node plugin down
strands every worker's volumes and still leaves provisioning working, and a
controller plugin down stops provisioning while existing attachments survive. The
two are different events and must not collapse into one phase.

`U-17` is the row that keeps a written configuration from being reported as a
fault.

### Version Skew (design §5)

File: `operator/internal/controllers/driver/simplyblockdriver_skew_test.go`

| #    | Scenario                                                                          | Type     | Test |
|------|-----------------------------------------------------------------------------------|----------|------|
| U-20 | `status.version` is published from what the deployed driver reports               | Positive | —    |
| U-21 | Versions equal: no `VersionSkew` event                                            | Negative | —    |
| U-22 | Driver newer than the control plane: one `VersionSkew` event, naming both         | Positive | —    |
| U-31 | A control plane the driver's `compatible.controlplane` admits: no event           | Positive | —    |
| U-32 | A control plane older than every pattern: one `VersionSkew` event                 | Negative | —    |
| U-45 | A `26.2.x` pattern matching 26.2.7: no event                                      | Boundary | —    |
| U-46 | A full version pattern matching only itself                                       | Boundary | —    |
| U-47 | A control plane newer than every pattern: one `VersionTooOld` event               | Negative | —    |
| U-48 | Older against newer is decided by the component's list order, not by parsing      | Boundary | —    |
| U-49 | A driver release carrying no `compatible`: both versions published, neither event | Boundary | —    |
| U-50 | The driver's version absent from the document: neither event                      | Boundary | —    |
| U-51 | The document unreachable: neither event, and the cached copy is used where held   | Boundary | —    |
| U-52 | A `schema` the reader does not know: the document is not parsed further           | Negative | —    |
| U-53 | `compatible` naming a component the driver does not check: ignored, not an error  | Boundary | —    |
| U-23 | A skew does not change the phase, since the deployment is healthy                 | Boundary | —    |
| U-24 | A skew does not roll the driver forward on its own                                | Negative | —    |
| U-25 | No `ControlPlane` version reported: no skew is claimed either way                 | Boundary | —    |

`U-48` is the row design §5.1 turns on. Which of the two events fires is read from
the component's list order, since a version string alone does not order releases
that ship on separate cadences.

`U-49` to `U-51` are the three ways the document answers nothing, and all three
report both versions and emit no event. `U-24` is the row that holds design §5's refusal to repair. A driver rollout
replaces every node plugin in the cluster, and doing that as a side effect of a
control-plane upgrade is the surprise the design declines to build.

---

## 2. Integration Tests

Full reconcile loop against a real API server via `envtest`. The immutability and
defaulting rules are admission and cannot be exercised any other way.

| #    | Scenario                                                                 | Type     | Test |
|------|--------------------------------------------------------------------------|----------|------|
| I-01 | `spec.driverName` changed after creation: rejected as immutable          | Negative | —    |
| I-02 | `spec.driverName` unset: defaulted to `csi.simplyblock.io`               | Boundary | —    |
| I-03 | `spec.image` outside the trusted registries: rejected by the pattern     | Negative | —    |
| I-04 | `spec.image` omitted: rejected as `Required`                             | Negative | —    |
| I-05 | `spec.controllerReplicas` of 0: rejected by the minimum                  | Boundary | —    |
| I-06 | `spec.controllerReplicas` unset: defaulted to 1                          | Boundary | —    |
| I-07 | `spec.imagePullPolicy` outside the enum: rejected                        | Negative | —    |
| I-08 | The short name `sbd` resolves to the same list as the full kind          | Positive | —    |
| I-09 | A full apply against a real API server: every object exists afterward    | Positive | —    |
| I-10 | Deleting the object: garbage collection removes every applied child      | Positive | —    |
| I-11 | The controller's role covers every object the apply creates              | Positive | —    |
| I-12 | Two drivers with two `driverName` values in one namespace: both accepted | Boundary | —    |

`I-12` records what design §9 Q2 leaves open. The row asserts today's behavior,
which is that nothing forbids it, and it is the row to change if the kind becomes
a singleton.

---

## 3. End-to-End Tests

A live deployment with a real kubelet, because a CSI driver that is applied but
not registered fails only when a workload asks for a volume.

| #    | Scenario                                                                               | Type     | Test |
|------|----------------------------------------------------------------------------------------|----------|------|
| E-01 | A fresh `SimplyblockDriver`: the driver registers and a volume provisions              | Positive | —    |
| E-02 | A node plugin killed on one worker: `Degraded`, and other workers still attach         | Positive | —    |
| E-03 | The controller plugin killed: `Unavailable`, and existing attachments survive          | Negative | —    |
| E-04 | The controller plugin returns: provisioning resumes without intervention               | Positive | —    |
| E-05 | A CSI driver older than the control plane: the skew is visible in both gauges          | Negative | —    |
| E-06 | `spec.image` changed: the rollout replaces both plugins and the phase recovers         | Positive | —    |
| E-07 | Sustained I/O across a node-plugin restart: the data path is unaffected                | Positive | —    |
| E-08 | A cluster with no snapshot API: the CRDs and a controller appear, and a snapshot works | Positive | —    |

---

## 4. Manual Scenarios

### M-01: A driver and a control plane at two versions

**Design reference:** §1, §5.

**What to verify:** the failure the kind exists to prevent, and that it now
surfaces before a workload meets it.

**Test concept:**

1. Deploy a control plane and a driver at matching versions, and provision a
   volume.
2. Upgrade the control plane alone, leaving `spec.image` where it is.
3. Confirm `VersionSkew` fires and both gauges disagree, and confirm the phase
   stays `Ready`, because the deployment itself is healthy.
4. Attach a new volume and record what happens, which is what the driver's
   `compatible` declaration for that release predicts.
5. Set `spec.image` to the matching version and confirm the skew clears.

### M-02: The chart and the operator both own the driver

**Design reference:** §8.

**What to verify:** that moving the install out of the chart is not a flag day,
which is the same problem the control plane has.

**Test concept:**

1. Install the chart, which applies the driver's objects.
2. Create a `SimplyblockDriver` naming the same `driverName`.
3. Record whether the controller adopts the chart's objects, applies over them,
   or conflicts, and confirm no volume is orphaned in any of the three.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 53        | 0       | 53          |
| Integration | 12        | 0       | 12          |
| E2E         | 8         | 0       | 8           |
| Manual      | 2         | 0       | 2           |
| **Total**   | **75**    | **0**   | **75**      |

Nothing is covered, and nothing can be: the kind does not exist. Every row is a
specification, and the plan's value before implementation is that it says what
the kind has to do rather than what somebody remembers deciding.

---

## 6. What Is Not Yet Covered

| #                                    | Gap                                                                     | Reason                                                                                                                |
|--------------------------------------|-------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------|
| U-01 … U-10, U-26 … U-44             | What the controller applies, and the configuration it writes            | The kind does not exist. These are the rows to write first, because they are pure                                     |
| U-11 … U-19                          | The phase and its counts                                                | The kind does not exist. `U-14` and `U-15` are the pair that keeps a partial failure from being reported as an outage |
| U-20 … U-25, U-31, U-32, U-45 … U-53 | Version skew                                                            | The kind does not exist, and neither does the document design §5.1 reads                                              |
| I-01 … I-12                          | Every admission rule and the real-API-server apply                      | Needs `envtest`, because defaulting and immutability are enforced by the API server and a fake client applies neither |
| E-01 … E-08                          | All end-to-end scenarios                                                | Needs a live deployment with a real kubelet. The e2e harness under `test/` is not committed yet                       |
| M-01, M-02                           | Two versions, and the chart overlap                                     | Need two builds of the driver and a cluster the chart already installed into                                          |
| Metrics                              | The four metrics of design §6.2                                         | Designed, not built                                                                                                   |
| Q1 … Q4                              | Skew policy, the singleton question, snapshot cleanup, and `driverName` | Design §9 leaves all four open, so no row asserts what they do                                                        |

### Axis coverage

| Axis             | Value                             | Scenarios                  |
|------------------|-----------------------------------|----------------------------|
| Plugin health    | Both plugins serving              | U-12, E-01                 |
|                  | A node plugin down                | U-13, U-14, E-02           |
|                  | The controller plugin down        | U-15, U-18, E-03           |
|                  | Recovered                         | E-04, E-06                 |
| Worker scale     | No worker matches the selector    | U-17                       |
|                  | One worker                        | U-13                       |
|                  | Several workers                   | U-11, E-02                 |
| Version ordering | Driver equal to the control plane | U-21                       |
|                  | Driver older                      | U-31, U-32                 |
|                  | Driver newer                      | U-22, E-05, M-01           |
|                  | Unknown                           | U-25                       |
| Snapshot support | Detected                          | U-04, U-39                 |
|                  | Installed                         | U-05, U-40, E-08           |
| Control plane    | Present and `Ready`               | U-26, E-01                 |
|                  | Absent                            | U-29                       |
|                  | Present, not `Ready`              | U-30                       |
| Namespace count  | Single                            | Every scenario except I-12 |
|                  | Two drivers in one namespace      | I-12                       |

**The version-ordering axis is the one this kind exists for.** `U-22`, `E-05`, and
`M-01` cover the reported ordering, and `U-31` covers the supported one. Design §1
is why both belong here: an inverted ordering is otherwise discoverable only by
attaching a volume and watching it fail, and the supported ordering is what every
upgrade passes through.

**The plugin-health axis is where the phase is decided**, and `U-14` against
`U-15` is the distinction design §4.2 rests on: every node plugin down still
provisions, and a controller plugin down does not.
