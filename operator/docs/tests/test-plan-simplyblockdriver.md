# Test Plan: SimplyblockDriver

Related design: [`designs/crd-redesign/design-simplyblockdriver.md`](../designs/crd-redesign/design-simplyblockdriver.md)

Scope is the operator and the Kubernetes surface this repository builds. The CSI
driver's own behavior is out of scope and is `csi-driver/`'s to test: what a row
asserts is that the operator applies the right objects and reports the right
phase, never that the driver attaches a volume correctly.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason.

Most of the kind is still a specification rather than a gap against shipped
behavior. What exists is the API type and the singleton's validating webhook, and
the rows those cover carry a test name. What the chart installs is asserted only where the operator has
to take it over, which is §1's adoption group and design §4.3.

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
| U-26 | The node `ConfigMap` carries the endpoint the cluster's `ControlPlane` publishes   | Positive | —    |
| U-27 | It carries the credentials for that control plane, and not the endpoint alone      | Positive | —    |
| U-28 | The published endpoint changes: the `ConfigMap` is rewritten                       | Positive | —    |
| U-29 | No `ControlPlane` in the cluster: the objects are applied and the driver waits     | Boundary | —    |
| U-30 | A `ControlPlane` that is not `Ready`: applied and waiting, not `Failed`            | Boundary | —    |
| U-81 | Several `StorageCluster`s: one `clusters` entry each, all naming one endpoint      | Positive | —    |
| U-82 | A `ControlPlane` in another namespace than the driver: resolved all the same       | Boundary | —    |

`U-44` holds the boundary design §4.1 draws. The `csi-snapshotter` sidecar is part
of this driver's controller plugin, and the detection applies to the cluster's
`snapshot-controller`.

`U-38` and `U-41` are the pair design §4.1 turns on. What the operator installs
here is cluster-scoped and shared, so it is applied without ownership and stays
when the driver goes, which design §9 Q2 records as unfinished.

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

### Adoption (design §4.3)

File: `operator/internal/controllers/driver/simplyblockdriver_adoption_test.go`

Every row seeds the fake client with the objects a chart install leaves behind,
carrying the Helm labels and annotations a live release writes.

| #        | Scenario                                                                              | Type         | Test               |
|----------|---------------------------------------------------------------------------------------|--------------|--------------------|
| U-54     | A namespace holding the chart's objects: each one is taken over, none recreated       | Positive     | —                  |
| U-55     | The `DaemonSet`'s UID and creation timestamp are the ones it had before               | Boundary     | —                  |
| U-56     | An empty namespace: every object is created, and `status.origin` is `Created`         | Negative     | —                  |
| U-57     | `status.origin` is `Adopted` where any object was met rather than created             | Positive     | —                  |
| U-58     | `status.origin` survives a later reconcile that creates a missing object              | Boundary     | —                  |
| U-59     | A `SimplyblockDriver` named `simplyblock` derives the names the chart writes          | Positive     | —                  |
| U-60     | A driver named otherwise derives a disjoint set and adopts nothing                    | Negative     | —                  |
| U-61     | The namespaced objects come out with a controller reference                           | Positive     | —                  |
| U-62     | The cluster-scoped objects come out with `managed-by` and no owner reference          | Positive     | —                  |
| U-63     | The Helm labels and the two `meta.helm.sh` annotations are gone afterward             | Positive     | —                  |
| U-64     | `helm.sh/resource-policy: keep` is present afterward, and was added where absent      | Boundary     | —                  |
| U-65     | An object carrying no Helm metadata is not annotated with the resource policy         | Negative     | —                  |
| U-66     | The live `CSIDriver` names a driver other than `spec.driverName`: refused             | Negative     | —                  |
| U-67     | A refusal holds the phase at `Installing` and changes no adopted object               | Negative     | —                  |
| U-68     | A refusal still publishes `nodesReady`, `nodesTotal`, and `controllerReady`           | Boundary     | —                  |
| U-69     | The resolved control-plane endpoint differs from the adopted `ConfigMap`'s: refused   | Negative     | —                  |
| U-70     | The resolved credentials differ from the adopted `Secret`'s: refused                  | Negative     | —                  |
| U-71     | Endpoint and credentials agree: the objects are adopted and not rewritten             | Positive     | —                  |
| U-72     | The cluster serves the snapshot API: nothing installed, `snapshotSupport` `Detected`  | Boundary     | —                  |
| U-73     | `ConfigMap/simplyblock-clusters` is not touched                                       | Negative     | —                  |
| ~~U-74~~ | ~~The sidecar images become the operator's, and the driver image is left alone~~      | ~~Boundary~~ | Superseded by U-83 |
| U-75     | A second reconcile after adoption applies nothing and re-emits no event               | Negative     | —                  |
| U-76     | An object that is not the oldest in the cluster applies nothing                       | Negative     | —                  |
| U-77     | It holds at `Installing` and names the holder of the deployment in `status.message`   | Negative     | —                  |
| U-78     | It emits `DuplicateDriver`, and the oldest object emits none                          | Negative     | —                  |
| U-79     | Equal creation timestamps: namespace and name break the tie the same way for both     | Boundary     | —                  |
| U-80     | The oldest object reconciles normally while a younger one exists                      | Positive     | —                  |
| U-83     | A release that pinned a sidecar: the pin lands in `spec.sidecarImages`, unchanged     | Positive     | —                  |
| U-84     | A sidecar at the chart version's default: no override, and it moves to this release's | Boundary     | —                  |
| U-85     | `spec.sidecarImages` unset: every sidecar takes the version this operator ships       | Positive     | —                  |
| U-86     | One override set: it reaches its own container and no other                           | Boundary     | —                  |
| U-87     | The snapshot controller's image is not overridable and is not installed either        | Negative     | —                  |

`U-55` is the row that holds design §4.3's in-place claim. An object with a new
UID is an object that was deleted and reapplied, which for the node `DaemonSet`
means every node plugin in the cluster restarted at once.

`U-59` and `U-60` are the pair the derivation rests on. Adoption needs no mapping
table only because the chart's literal names are what the derivation produces for
an object named `simplyblock`, and a differently named object must therefore find
nothing to adopt rather than adopt the first one's deployment.

`U-66` and `U-69` are the two refusals of design §4.3's step 2, and `U-71` is the
case they exist to distinguish themselves from. Neither mismatch is repairable:
`driverName` is immutable, and rewriting the endpoint points a running driver at a
backend its volumes are not on.

`U-74` asserted that adoption re-images the sidecars, which design §3.1 now
answers with `spec.sidecarImages`. The row keeps its ID struck through, and
`U-83` to `U-87` are what replaced it.

`U-83` against `U-84` is the distinction design §4.3 turns on. A release that
moved a sidecar keeps where it moved it, and one that never touched the value is
not frozen at whatever tag its chart version happened to carry, so the comparison
is against that chart version's default rather than against this operator's.

`U-76` to `U-80` are the controller half of design §3.4's singleton, which is the
half that runs when the webhook was not serving. `U-79` is the row that keeps the
two controllers from disagreeing: both read one list and both must pick the same
object from it, so a tie broken differently on the two sides is the flap the
singleton exists to prevent.

### The Singleton at Admission (design §3.4)

File: `operator/internal/webhook/simplyblockdriver_validator_test.go`

These rows are the validator's decisions against a fake client. That the API
server actually calls it is `I-18` to `I-22`, which need a real one.

| #    | Scenario                                                           | Type     | Test                             |
|------|--------------------------------------------------------------------|----------|----------------------------------|
| U-88 | The first driver in an empty cluster is admitted                   | Positive | `TestSimplyblockDriverValidator` |
| U-89 | A second driver in the same namespace is denied                    | Negative | `TestSimplyblockDriverValidator` |
| U-90 | A second driver in another namespace is denied                     | Negative | `TestSimplyblockDriverValidator` |
| U-91 | The denial names the namespace and name that hold the deployment   | Positive | `TestSimplyblockDriverValidator` |
| U-92 | An update to the only driver is admitted, since the rule is CREATE | Boundary | `TestSimplyblockDriverValidator` |
| U-93 | A create after the only driver was deleted is admitted             | Boundary | `TestSimplyblockDriverValidator` |

`U-91` is the row that makes the denial actionable. A message saying only that
a driver exists leaves an administrator to find it, and it may be in a namespace
they were not looking at.

### The Phase and Its Counts (design §3.3, §4.2)

Files: `operator/internal/controllers/driver/phase_test.go` for the derivation,
and `simplyblockdriver_controller_test.go` for what reaches status.

| #    | Scenario                                                                             | Type     | Test                         |
|------|--------------------------------------------------------------------------------------|----------|------------------------------|
| U-11 | `status.nodesReady` and `nodesTotal` reflect the DaemonSet                           | Positive | `TestCounts`                 |
| U-12 | All node plugins ready and the controller serving: `Ready`                           | Positive | `TestPhase`                  |
| U-13 | One node plugin of three not ready: `Degraded`, not `Unavailable`                    | Boundary | `TestPhase`                  |
| U-14 | Zero ready node plugins while the controller serves: `Degraded`                      | Boundary | `TestPhase`                  |
| U-15 | The controller plugin not running: `Unavailable`, whatever the node plugins do       | Negative | `TestPhase`                  |
| U-16 | `status.nodesReady` is 0 and present, never omitted                                  | Boundary | `TestCounts`                 |
| U-17 | A `nodeSelector` that matches no worker: `nodesTotal` is 0, reported not failed      | Boundary | `TestPhase`                  |
| U-18 | `status.controllerReady` false while `nodesReady` equals `nodesTotal`: `Unavailable` | Boundary | `TestPhase`                  |
| U-19 | `status.observedGeneration` matches `metadata.generation` after a reconcile          | Positive | `TestStatusCarriesTheCounts` |

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

| #        | Scenario                                                                     | Type         | Test               |
|----------|------------------------------------------------------------------------------|--------------|--------------------|
| I-01     | `spec.driverName` changed after creation: rejected as immutable              | Negative     | —                  |
| I-02     | `spec.driverName` unset: defaulted to `csi.simplyblock.io`                   | Boundary     | —                  |
| I-03     | `spec.image` outside the trusted registries: rejected by the pattern         | Negative     | —                  |
| I-04     | `spec.image` omitted: rejected as `Required`                                 | Negative     | —                  |
| I-05     | `spec.controllerReplicas` of 0: rejected by the minimum                      | Boundary     | —                  |
| I-06     | `spec.controllerReplicas` unset: defaulted to 1                              | Boundary     | —                  |
| I-07     | `spec.imagePullPolicy` outside the enum: rejected                            | Negative     | —                  |
| I-08     | The short name `sbd` resolves to the same list as the full kind              | Positive     | —                  |
| I-09     | A full apply against a real API server: every object exists afterward        | Positive     | —                  |
| I-10     | Deleting the object: garbage collection removes every applied child          | Positive     | —                  |
| I-11     | The controller's role covers every object the apply creates                  | Positive     | —                  |
| ~~I-12~~ | ~~Two drivers with two `driverName` values in one namespace: both accepted~~ | ~~Boundary~~ | Superseded by I-18 |
| I-13     | `spec.enableVolumeSnapshots` unset: defaulted to true                        | Boundary     | —                  |
| I-14     | Field ownership is taken from a manager that already holds the fields        | Positive     | —                  |
| I-15     | An adopted object keeps its UID across the reconcile that adopts it          | Positive     | —                  |
| I-16     | The finalizer deletes the cluster-scoped objects the label marks             | Positive     | —                  |
| I-17     | A cluster-scoped object carrying another controller's label is left          | Negative     | —                  |
| I-18     | A second `SimplyblockDriver` in the same namespace: denied at admission      | Negative     | —                  |
| I-19     | A second one in another namespace: denied, whatever its `driverName`         | Negative     | —                  |
| I-20     | The first `SimplyblockDriver` in an empty cluster: admitted                  | Positive     | —                  |
| I-21     | The only object updated, not created: admitted, since the rule is CREATE     | Boundary     | —                  |
| I-22     | The only object deleted and another created: admitted                        | Boundary     | —                  |
| I-23     | A `spec.sidecarImages` entry outside the trusted registries: rejected        | Negative     | —                  |

`I-12` asserted that two drivers in one namespace were accepted, which design
§3.4 now rejects. The row keeps its ID struck through, and `I-18` is what replaced
it.

`I-18` to `I-22` are design §3.4's webhook, and the last three are the boundary
the rule is written against. It denies a `CREATE` where an object already exists,
so editing the one that does must not be denied and replacing it must not be
either. A rule written over every operation would lock the deployment's own spec.

`I-14` is the row a fake client cannot carry. Server-side apply against a field
another manager owns is the API server's arbitration, and design §4.3's step 3
either takes the fields or silently leaves Helm's values in place.

`I-16` and `I-17` are the pair design §4.1's ownership split turns on. The label
grants the right to delete, so a `ClusterRole` this controller marked goes and one
marked by anything else stays.

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
| E-09 | A chart-installed cluster upgraded: the driver is adopted and no plugin pod restarts   | Positive | —    |
| E-10 | Volumes stay attached and I/O continues across the reconcile that adopts               | Positive | —    |
| E-11 | `helm uninstall` after adoption: the driver keeps running and volumes keep attaching   | Positive | —    |
| E-12 | Deleting the adopted `SimplyblockDriver`: every object goes, cluster-scoped included   | Positive | —    |
| E-13 | A second object created while the webhook is down: the running driver is untouched     | Negative | —    |

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

### M-02: Adoption by hand, outside the upgrade sequence

**Design reference:** §4.3, and `design-api-upgrade.md` §12.

**What to verify:** that an administrator who creates the object against a
chart-installed deployment gets the same handover as the upgrade, including the
protection the installer would otherwise have applied before `helm upgrade`.

**Test concept:**

1. Install the chart at defaults and provision a volume onto a running workload.
2. Create a `SimplyblockDriver` named `simplyblock`, with the spec translated
   from the release's values, without running any step of the upgrade sequence.
3. Confirm every object of design §4.3's table is adopted in place, that no pod
   restarted, and that the volume stayed attached throughout.
4. Confirm `helm.sh/resource-policy: keep` is on each of them, and that the Helm
   labels and `meta.helm.sh` annotations are gone.
5. Run `helm uninstall` and confirm the driver survives and still attaches a
   volume, which is the property step 4 exists for.

### M-03: A release the translation cannot express

**Design reference:** §4.3, §9 Q5.

**What to verify:** what an adoption does to a deployment whose values were not
left at their defaults, which is the case design §4.3's translation table has no
row for.

**Test concept:**

1. Install the chart with a pinned `image.csiAttacher` tag, a non-default
   `driverName`, `controller.replicas` above one, and
   `snapshotcontroller.create` false.
2. Translate the values and create the `SimplyblockDriver`.
3. Confirm the driver name, the replica count, and the snapshot setting all come
   through, and that the deployment is not reconfigured on the first reconcile.
4. Record what happened to the pinned sidecar, which is the answer design §9 Q5
   needs and the one the table declines to carry a field for.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 92        | 15      | 77          |
| Integration | 22        | 0       | 22          |
| E2E         | 13        | 0       | 13          |
| Manual      | 3         | 0       | 3           |
| **Total**   | **130**   | **15**  | **115**     |

`I-12` and `U-74` are superseded and are not in the counts.

`U-11` to `U-19` and `U-88` to `U-93` are covered: the phase and its counts, and
the singleton at admission. The rest waits on adoption and on the version
document, and until then the plan's value is that it says what the kind has to do
rather than what somebody remembers deciding.

---

## 6. What Is Not Yet Covered

| #                                    | Gap                                                                  | Reason                                                                                                                |
|--------------------------------------|----------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------|
| U-01 … U-10, U-26 … U-44             | What the controller applies, and the configuration it writes         | The kind does not exist. These are the rows to write first, because they are pure                                     |
| U-20 … U-25, U-31, U-32, U-45 … U-53 | Version skew                                                         | The kind does not exist, and neither does the document design §5.1 reads                                              |
| U-54 … U-87                          | Adoption, the singleton's controller half, and the sidecar overrides | The kind does not exist. Every row is seedable from a fake client, so these follow the apply rows immediately         |
| I-01 … I-23                          | Every admission rule, the real-API-server apply, and field ownership | Needs `envtest`, because defaulting and immutability are enforced by the API server and a fake client applies neither |
| E-01 … E-13                          | All end-to-end scenarios                                             | Needs a live deployment with a real kubelet. The e2e harness under `test/` is not committed yet                       |
| M-01 … M-03                          | Two versions, adoption by hand, and a release at non-default values  | Need two builds of the driver and a cluster the chart already installed into                                          |
| Metrics                              | The four metrics of design §6.2                                      | Designed, not built                                                                                                   |
| Q2, Q3                               | Snapshot cleanup, and whether `driverName` stays settable            | Design §9 leaves both open, so no row asserts what they do                                                            |

### Axis coverage

| Axis              | Value                                  | Scenarios                 |
|-------------------|----------------------------------------|---------------------------|
| Plugin health     | Both plugins serving                   | U-12, E-01                |
|                   | A node plugin down                     | U-13, U-14, E-02          |
|                   | The controller plugin down             | U-15, U-18, E-03          |
|                   | Recovered                              | E-04, E-06                |
| Worker scale      | No worker matches the selector         | U-17                      |
|                   | One worker                             | U-13                      |
|                   | Several workers                        | U-11, E-02                |
| Version ordering  | Driver equal to the control plane      | U-21                      |
|                   | Driver older                           | U-31, U-32                |
|                   | Driver newer                           | U-22, E-05, M-01          |
|                   | Unknown                                | U-25                      |
| Snapshot support  | Detected                               | U-04, U-39                |
|                   | Installed                              | U-05, U-40, E-08          |
| Control plane     | Present and `Ready`                    | U-26, E-01                |
|                   | Absent                                 | U-29                      |
|                   | Present, not `Ready`                   | U-30                      |
|                   | In a namespace other than the driver's | U-82                      |
|                   | Fronting several `StorageCluster`s     | U-81                      |
| Driver count      | The only one in the cluster            | I-20, and every other row |
|                   | A second in the same namespace         | I-18                      |
|                   | A second in another namespace          | I-19                      |
|                   | A second past the webhook              | U-76 … U-80, E-13         |
| Deployment origin | Created from nothing                   | U-56, E-01                |
|                   | Adopted from a chart install           | U-54, U-57, E-09, M-02    |
|                   | Adoption refused                       | U-66, U-69, U-70          |
|                   | Adopted at non-default values          | M-03                      |
| Sidecar images    | Pinned by the adopted release          | U-83                      |
|                   | Left at the chart version's default    | U-84                      |
|                   | Unset on a fresh object                | U-85, U-56                |
|                   | Outside the trusted registries         | I-23                      |

**The version-ordering axis is the one this kind exists for.** `U-22`, `E-05`, and
`M-01` cover the reported ordering, and `U-31` covers the supported one. Design §1
is why both belong here: an inverted ordering is otherwise discoverable only by
attaching a volume and watching it fail, and the supported ordering is what every
upgrade passes through.

**The plugin-health axis is where the phase is decided**, and `U-14` against
`U-15` is the distinction design §4.2 rests on: every node plugin down still
provisions, and a controller plugin down does not.

**The driver-count axis replaced a namespace-count axis**, because design §3.4
settles that a Kubernetes cluster holds one `SimplyblockDriver`. What used to be a
question about how many namespaces run a driver is now a question about what stops
the second one, and `E-13` is the value that matters most: the webhook is the rule
and the controller is what holds when the webhook is not there.

**The deployment-origin axis is the one every existing cluster travels.** Design
§4.3 makes adoption the ordinary path rather than a migration case, so `U-56` is
the row for the cluster that has never run simplyblock and the three rows beside
it are the rest. `E-10` is where the axis costs the most to check and proves the
most: a volume that stays attached and a workload that keeps writing across the
reconcile that adopts is the whole claim, and nothing smaller than a live cluster
shows it.
