# Test Plan: ControlPlane and ControlPlaneOps

Related design: [`designs/crd-redesign/design-controlplane.md`](../designs/crd-redesign/design-controlplane.md)

Scope is the operator, its webhooks, and the Kubernetes surface this repository
builds. The control plane (`sbcli`), FoundationDB, and the FoundationDB operator
are dependencies, faked at the boundary: what a row asserts is this operator's
response to an answer, never the dependency's own behavior.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason. The CSI driver's deployment has its own plan,
[`test-plan-simplyblockdriver.md`](test-plan-simplyblockdriver.md), and no row here asserts anything about
it.

Scenario text names the target spelling. `status.phase` is
`Initializing;Ready` today and `Installing;Ready;Degraded;Unavailable` after design §3.3, the
image is `spec.image` today and `spec.source.managed.image` after design §5.1,
and the endpoint is the `SIMPLYBLOCK_WEBAPI_BASE_URL` environment variable today
and `status.endpoint` after design §3.3.

| Class       | Prefix | Harness                                                                |
|-------------|--------|------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, and a mock backend |
| Integration | `I-`   | Full reconcile loop against `envtest` and a mock backend               |
| E2E         | `E-`   | Live simplyblock deployment, real FoundationDB                         |
| Manual      | `M-`   | Needs failure injection or orchestration not automated yet             |

---

## 1. Unit Tests

Pure functions and single reconcile calls against a fake client, with the control
plane replaced by a mock HTTP server. No Kubernetes API server is involved.

### The Singleton Guard (design §3.1)

File: `operator/internal/controllers/controlplane/controlplane_controller_unit_test.go`

| #    | Scenario                                                              | Type     | Test |
|------|-----------------------------------------------------------------------|----------|------|
| U-01 | A CR named `simplyblock` is reconciled                                | Positive | —    |
| U-02 | A CR named anything else returns without requeue and writes no status | Negative | —    |
| U-03 | The singleton is absent: reconcile returns without error              | Boundary | —    |

### The Readiness Probe (design §4.3)

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| U-04 | The probe returns 200: the phase becomes `Available` and `message` is cleared | Positive | —    |
| U-05 | The probe returns 503: the phase becomes `Unavailable`                        | Negative | —    |
| U-06 | The probe returns 500 with a body: the body reaches `status.message` verbatim | Negative | —    |
| U-07 | The probe times out: the phase becomes `Unavailable` with the transport error | Negative | —    |
| U-08 | Connection refused: the phase becomes `Unavailable`, not `Installing`         | Boundary | —    |
| U-09 | A probe that never succeeded: the phase stays `Installing`, not `Unavailable` | Boundary | —    |
| U-10 | `status.lastChecked` is stamped on every probe, passing or failing            | Positive | —    |
| U-11 | A 2xx that is not 200 is treated as success                                   | Boundary | —    |
| U-12 | A 3xx is treated as failure, since the client does not follow redirects       | Boundary | —    |

### Workload Health and the Phase It Decides (design §4.3)

The probe alone cannot produce `Degraded`, so these rows drive both signals. The
phase is the worst verdict across the probe and every component, and only an
essential component can reach `Unavailable` (design §4.3).

| #     | Scenario                                                                              | Type     | Test |
|-------|---------------------------------------------------------------------------------------|----------|------|
| U-72  | Probe passes, every component at its desired count: `Ready`                           | Positive | —    |
| U-73  | `webappapi` at 1 of 2 ready: `Degraded`, and the event names it                       | Positive | —    |
| U-74  | The `FoundationDBCluster` reporting below its desired count: `Degraded`               | Positive | —    |
| U-75  | `webappapi` at 0 of 2 ready: `Unavailable`, since it is essential                     | Negative | —    |
| U-132 | `replicas` of 1 with the pod restarting: `Unavailable`, never `Degraded`              | Boundary | —    |
| U-133 | `replicas` of 2 with one pod restarting: `Degraded`, since the probe still passes     | Boundary | —    |
| U-76  | Probe fails while every component is at its desired count: `Unavailable`              | Boundary | —    |
| U-77  | Probe fails while a component is below desired: `Unavailable`, the worse verdict wins | Boundary | —    |
| U-78  | `source.external` with the probe passing: `Ready`, and `status.components` is empty   | Positive | —    |
| U-79  | `source.external` with the probe failing: `Unavailable`, never `Degraded`             | Negative | —    |
| U-80  | A component returning to its desired count: `Degraded` back to `Ready`                | Positive | —    |
| U-81  | `Degraded` does not set whatever downstream controllers read to hold                  | Negative | —    |
| U-82  | `Unavailable` does set it, as does `Installing`                                       | Positive | —    |
| U-87  | Grafana at 0 of 1 ready: `Degraded`, never `Unavailable`, since it is not essential   | Boundary | —    |
| U-88  | A component absent from the table entirely: it takes the non-essential default        | Boundary | —    |
| U-134 | The task runner at zero ready: `Degraded`, never `Unavailable`                        | Boundary | —    |
| U-89  | Two components below desired: one phase, and both appear in `status.components`       | Boundary | —    |
| U-90  | `status.components` carries the desired count, the ready count, and `essential`       | Positive | —    |
| U-91  | An essential component at zero while a non-essential one is also down: `Unavailable`  | Boundary | —    |
| U-92  | Every component at zero: `Unavailable` once, naming the essential ones                | Boundary | —    |

`U-76` and `U-77` fix the precedence. A control plane that is not answering is
`Unavailable` whatever its components report, because the phase says what a caller
would experience rather than what the deployment contains.

`U-87` and `U-88` are the two that keep design §4.3's safety property. A
non-essential component at zero must not halt the operator, and a component nobody
classified must land on the side that cannot.

`U-81` is the row the whole change turns on. `Degraded` that held the operator
would be the old `Degraded` under a new name.

### Events on Transition (design §4.3, §9.1)

| #    | Scenario                                                                   | Type     | Test |
|------|----------------------------------------------------------------------------|----------|------|
| U-13 | `Ready` to `Unavailable`: exactly one `ControlPlaneNotReady` event         | Positive | —    |
| U-14 | `Unavailable` to `Ready`: exactly one `ControlPlaneReady` event            | Positive | —    |
| U-15 | Ten consecutive failing probes: exactly one event, not ten                 | Boundary | —    |
| U-16 | Ten consecutive passing probes after a failure: exactly one recovery event | Boundary | —    |
| U-17 | The first probe ever, failing: an event is emitted from an empty phase     | Boundary | —    |
| U-83 | `Ready` to `Degraded`: exactly one `ControlPlaneDegraded` event            | Positive | —    |
| U-84 | `Degraded` to `Unavailable`: `ControlPlaneNotReady`, not a second degraded | Boundary | —    |
| U-85 | `Degraded` to `Ready`: exactly one `ControlPlaneReady` event               | Positive | —    |
| U-86 | Ten consecutive probes with a pod restarting: one event, not ten           | Boundary | —    |

### The Source Block (design §3.2, §5)

| #    | Scenario                                                                       | Type     | Test |
|------|--------------------------------------------------------------------------------|----------|------|
| U-18 | `source.external`: `status.endpoint` echoes the spec endpoint                  | Positive | —    |
| U-19 | `source.managed`: `status.endpoint` is derived from the installed Service      | Positive | —    |
| U-20 | `source.external` with a missing credentials Secret: `CredentialsError`, held  | Negative | —    |
| U-21 | `source.external` with a Secret missing its token key: `CredentialsError`      | Negative | —    |
| U-22 | `source.external` with a loopback endpoint: rejected by the outbound-URL guard | Negative | —    |
| U-23 | `source.external` with a link-local endpoint: rejected                         | Negative | —    |
| U-24 | `source.external` with a CA bundle: the probe verifies against it              | Positive | —    |
| U-25 | `source.external` with no CA bundle: the probe uses the system trust store     | Boundary | —    |
| U-26 | Every controller reads `status.endpoint` rather than the environment variable  | Positive | —    |
| U-27 | `status.endpoint` empty because the control plane is not ready: callers hold   | Negative | —    |

### The Installation Machine (design §4.2)

| #     | Scenario                                                                                | Type     | Test |
|-------|-----------------------------------------------------------------------------------------|----------|------|
| U-28  | `source.managed` on a fresh namespace: the machine enters `ApplyingFoundationDB`        | Positive | —    |
| U-29  | Each applied object carries a controller reference to the `ControlPlane`                | Positive | —    |
| U-30  | Re-entering `ApplyingFoundationDB`: the apply is idempotent, nothing is duplicated      | Negative | —    |
| U-31  | `AwaitingFoundationDB` with 2 of 3 coordinators: holds, and says so in `message`        | Negative | —    |
| U-32  | `AwaitingFoundationDB` with the cluster available: advances                             | Positive | —    |
| U-33  | `AwaitingAPI` with a failing probe: holds                                               | Negative | —    |
| U-34  | `AwaitingAPI` with a passing probe: the phase becomes `Available`                       | Positive | —    |
| U-35  | A step's deadline expires: `StepDeadlineExceeded`, and the phase does not advance       | Boundary | —    |
| U-36  | A step value the graph does not declare: `ErrUnknownState`, naming the declared set     | Negative | —    |
| U-37  | An empty `status.step`: restores to `ApplyingFoundationDB`                              | Boundary | —    |
| U-38  | Every declared state appears in the step `Enum` and in the CEL rule                     | Boundary | —    |
| U-39  | `source.external`: the installation machine is never entered                            | Negative | —    |
| U-137 | `apps.foundationdb.org/v1beta2` served: no FoundationDB CRD or controller applied       | Negative | —    |
| U-138 | It is not served: the CRDs and the controller are applied                               | Positive | —    |
| U-139 | An applied FoundationDB CRD carries no controller reference                             | Negative | —    |
| U-140 | `mongodbcommunity.mongodb.com/v1` served: the document store is applied against it      | Positive | —    |
| U-141 | It is not served: `Installing` holds, naming the missing operator                       | Negative | —    |
| U-142 | `cert-manager.io/v1` served: certificates are issued through it, with no `tls.provider` | Positive | —    |
| U-143 | An OpenShift cluster with no cert-manager: the platform's service CA is used            | Positive | —    |
| U-144 | Neither available with TLS wanted: `Installing` holds and `status.message` says which   | Negative | —    |
| U-145 | A default `StorageClass` exists: it is used, and no provisioner is applied              | Positive | —    |
| U-146 | No default `StorageClass`: `Installing` holds rather than applying hostpath             | Boundary | —    |

### Deletion (design §4.4)

| #    | Scenario                                                                       | Type     | Test |
|------|--------------------------------------------------------------------------------|----------|------|
| U-40 | Deletion with a `StorageCluster` present: held, `ClustersStillPresent` emitted | Negative | —    |
| U-41 | Deletion with no `StorageCluster`: the finalizer is removed                    | Positive | —    |
| U-42 | The last `StorageCluster` is removed: the held deletion proceeds unattended    | Positive | —    |
| U-43 | `source.external` deletion: the endpoint is never called, nothing is torn down | Negative | —    |
| U-44 | `source.managed` deletion: the owned objects are garbage-collected             | Positive | —    |

### ControlPlaneOps (design §6, §7)

File: `operator/internal/controllers/controlplane/controlplaneops_controller_unit_test.go`

| #     | Scenario                                                                              | Type     | Test |
|-------|---------------------------------------------------------------------------------------|----------|------|
| U-45  | The lock is free: acquired, phase becomes `Running`                                   | Positive | —    |
| U-46  | Another operation holds the lock: this one stays `Pending`                            | Negative | —    |
| U-47  | Two reconcilers acquiring one free lock: the loser gets 409                           | Negative | —    |
| U-48  | Terminal re-reconcile: no side effect, the lock is released again                     | Negative | —    |
| U-49  | The operation is deleted while `Running`: the finalizer releases the lock             | Positive | —    |
| U-50  | `Restart`: `Draining` holds while a `StorageNodeOps` is `Running`                     | Negative | —    |
| U-51  | `Restart`: `Draining` holds while a `StorageClusterOps` is `Running`                  | Negative | —    |
| U-52  | `Restart`: the last in-flight operation finishes and `Draining` advances              | Positive | —    |
| U-53  | `Restart`: no operations in flight, so `Draining` advances immediately                | Boundary | —    |
| U-54  | `Restart`: in-flight operations are never canceled to make the restart proceed        | Negative | —    |
| U-55  | `Restart`: `Restarting` recycles the workload, `Awaiting` completes on a good probe   | Positive | —    |
| U-56  | `Upgrade`: `Preflight` fails when the control plane is not `Ready`                    | Negative | —    |
| U-93  | `Upgrade`: `Preflight` fails when the requested image is the one already running      | Boundary | —    |
| U-94  | `Upgrade`: `Preflight` passes on a `Ready` control plane with a different image       | Positive | —    |
| U-95  | `Restart` enters `Draining` directly, with no preflight step of its own               | Positive | —    |
| U-96  | An operation whose target is deleted after admission: a missing-target failure        | Boundary | —    |
| U-97  | Every declared step appears in the step `Enum` and in the CEL rule                    | Boundary | —    |
| U-99  | `Restart` with no components: the whole control plane is recycled                     | Positive | —    |
| U-100 | `Restart` naming one component: only that workload is recycled                        | Positive | —    |
| U-101 | `Restart` naming a component absent from the §4.3 table: rejected with the name       | Negative | —    |
| U-102 | `Restart` naming only non-essential components: `Draining` is skipped                 | Boundary | —    |
| U-103 | `Restart` naming an essential component: `Draining` runs                              | Boundary | —    |
| U-104 | `Restart` with an empty component list: `Draining` runs                               | Boundary | —    |
| U-135 | `Restart` naming only the task runner: `Draining` is skipped                          | Boundary | —    |
| U-136 | The same restart with an operation in flight: it is not held                          | Negative | —    |
| U-126 | `Upgrade`: `Draining` holds while a `StorageNodeOps` is `Running`                     | Negative | —    |
| U-127 | `Upgrade`: `Draining` runs after `Preflight`, never before it                         | Boundary | —    |
| U-128 | `Upgrade` refused at `Preflight`: no drain is entered and nothing waits               | Boundary | —    |
| U-129 | `Upgrade`: the last in-flight operation finishes and `Applying` proceeds              | Positive | —    |
| U-130 | `Upgrade`: in-flight operations are never canceled to let the upgrade proceed         | Negative | —    |
| U-131 | `Backup` carries no `Draining` and does not wait on in-flight operations              | Negative | —    |
| U-105 | `Backup`: `Requesting` creates a `FoundationDBBackup` naming the cluster              | Positive | —    |
| U-106 | `Backup` where one already exists: it is triggered, not duplicated                    | Boundary | —    |
| U-107 | The `FoundationDBBackup` carries no owner reference to the operation                  | Negative | —    |
| U-108 | `Backup`: `Awaiting` completes when the backup reports a snapshot                     | Positive | —    |
| U-109 | `Backup` with no `blobStore` and no existing backup: `Failed` with the reason         | Negative | —    |
| U-110 | `status.backupRef` names what the run created or triggered                            | Positive | —    |
| U-57  | `Upgrade` on `source.managed`: the image is applied and `Verifying` compares versions | Positive | —    |
| U-58  | `Upgrade` whose reported version disagrees: `VersionMismatch`, operation `Failed`     | Negative | —    |
| U-59  | `Upgrade` succeeding: `spec.source.managed.image` is updated to match                 | Positive | —    |
| U-60  | `spec.upgrade` absent for `action: Upgrade`: rejected                                 | Negative | —    |
| U-61  | An unknown action: terminal failure with the action in the message                    | Negative | —    |

## 2. Integration Tests

Full reconcile loop against a real Kubernetes API server via `envtest`, with the
control plane still mocked. These cover what a fake client cannot: real
admission, real `resourceVersion` semantics, and real garbage collection. The
installation rows additionally need the FoundationDB CRDs installed into
`envtest`.

### Admission (design §3.2)

| #    | Scenario                                                                             | Type     | Test |
|------|--------------------------------------------------------------------------------------|----------|------|
| I-01 | `spec.source` omitted: rejected as `Required`                                        | Negative | —    |
| I-02 | Both `managed` and `external` set: rejected by the CEL rule                          | Negative | —    |
| I-03 | Neither set: rejected by the same rule                                               | Boundary | —    |
| I-04 | `spec.source` changed from `managed` to `external`: rejected as immutable            | Negative | —    |
| I-05 | `spec.source.managed.image` outside the trusted registries: rejected                 | Negative | —    |
| I-06 | `spec.source.external.endpoint` malformed: rejected by the pattern                   | Negative | —    |
| I-07 | `spec.source.external.credentialsSecretRef` omitted: rejected as `Required`          | Negative | —    |
| I-08 | `spec.source.managed.foundationDB.replicas` of 0: rejected by the minimum            | Boundary | —    |
| I-09 | `spec.source.managed.foundationDB.replicas` unset: defaulted to 3                    | Boundary | —    |
| I-40 | `spec.source.managed.replicas` unset: defaulted to 2, matching what the chart ships  | Boundary | —    |
| I-41 | `spec.source.managed.replicas` of 0: rejected by the minimum                         | Boundary | —    |
| I-42 | `spec.source.managed.replicas` of 1: accepted, since an edge deployment may want it  | Boundary | —    |
| I-12 | `ControlPlaneOps.spec.action` outside the enum: rejected by admission                | Negative | —    |
| I-13 | `ControlPlaneOps.spec.controlPlaneRef` changed after creation: rejected              | Negative | —    |
| I-14 | Short names `cp` and `cpops` resolve to the same lists as the full kinds             | Positive | —    |
| I-30 | A `ControlPlaneOps` naming an external `ControlPlane`: denied by the webhook         | Negative | —    |
| I-31 | The same for `action: Restart` and for `action: Upgrade`, with one message shape     | Negative | —    |
| I-32 | A `ControlPlaneOps` naming a managed `ControlPlane`: admitted                        | Positive | —    |
| I-33 | A `ControlPlaneOps` naming no `ControlPlane` at all: denied, naming the ref          | Negative | —    |
| I-34 | The webhook runs on `create` only, so a status update on an existing one is admitted | Boundary | —    |
| I-35 | The webhook denies rather than erroring when the target is external                  | Negative | —    |
| I-36 | `spec.action` of `Backup`: accepted by the enum                                      | Positive | —    |
| I-37 | `spec.backup.blobStore` omitted for `action: Backup`: rejected as `Required`         | Negative | —    |
| I-38 | `spec.restart.components` with duplicate entries: rejected by `listType=set`         | Negative | —    |

### Controller Behavior Under a Real API Server (design §4, §7)

| #    | Scenario                                                                          | Type     | Test |
|------|-----------------------------------------------------------------------------------|----------|------|
| I-15 | Two `ControlPlaneOps` for one control plane: the second stays `Pending`           | Positive | —    |
| I-16 | The lock is released: the queued operation wakes from the watch                   | Positive | —    |
| I-17 | `kubectl delete` on a `Running` operation: `activeOpsRef` is cleared              | Positive | —    |
| I-18 | Deleting a managed `ControlPlane` cascades to every object it installed           | Positive | —    |
| I-19 | Two namespaces each with a `simplyblock` singleton: neither reads the other       | Negative | —    |
| I-20 | A second `ControlPlane` in one namespace: accepted, inert, reports nothing        | Negative | —    |
| I-21 | The install applies over objects the chart already created                        | Negative | —    |
| I-22 | The controller's role covers every object the install applies                     | Positive | —    |
| I-23 | `status.phase` of `Unavailable` is accepted by the enum                           | Positive | —    |
| I-24 | A managed API pod deleted with two replicas: `Degraded` while requests succeed    | Positive | —    |
| I-25 | The replacement pod settles: the phase returns to `Available` without a probe gap | Positive | —    |

---

## 3. End-to-End Tests

A live deployment with a real FoundationDB and a real management API.

| #    | Scenario                                                                                                 | Type     | Test |
|------|----------------------------------------------------------------------------------------------------------|----------|------|
| E-01 | `source.managed` on an empty namespace: reaches `Available` and a cluster can be made                    | Positive | —    |
| E-02 | `source.external` against a control plane outside the Kubernetes cluster                                 | Positive | —    |
| E-03 | The management API is killed: the phase becomes `Unavailable` within one probe period                    | Negative | —    |
| E-04 | The management API returns: the phase recovers without intervention                                      | Positive | —    |
| E-05 | One FoundationDB coordinator is lost while quorum holds: the phase is `Degraded`, not `Available`        | Boundary | —    |
| E-06 | Quorum is lost: the phase becomes `Unavailable` and downstream controllers hold                          | Negative | —    |
| E-07 | `action: Restart` with an operation in flight: the restart waits for it                                  | Positive | —    |
| E-15 | Any action against an external control plane: the apply is rejected outright                             | Negative | —    |
| E-16 | `action: Backup` against a live control plane: a backup lands in the blob store                          | Positive | —    |
| E-18 | `action: Restart` naming only the task runner: the management API keeps serving, and queued work resumes | Positive | —    |
| E-08 | `action: Upgrade` to a new image: the version is verified after the rollout                              | Positive | —    |
| E-09 | `action: Upgrade` whose rollout fails back: the operation reports `Failed`                               | Negative | —    |
| E-11 | Deleting the singleton with clusters present: held, and named in an event                                | Negative | —    |
| E-12 | Sustained I/O across a control-plane restart: the data path is unaffected                                | Positive | —    |
| E-13 | One of two management API replicas killed: `Degraded`, and clusters still reconcile                      | Positive | —    |
| E-14 | Both replicas killed: `Unavailable`, and downstream controllers hold                                     | Negative | —    |

---

## 4. Manual Scenarios

### M-01: The chart and the operator both own the install

**Design reference:** §5.1, §12 Q2.

**What to verify:** what actually happens when an operator capable of applying
the control plane is deployed into a namespace whose chart already applied it.
This is the scenario Q2 exists to avoid, and the reason to run it is that
somebody will do it by accident before the transition is designed.

**Test concept:**

1. Install the chart at a version that renders the control-plane templates.
2. Deploy an operator that applies them too, with `source.managed`.
3. Observe whether the two fight over the `FoundationDBCluster`, and in
   particular whether the operator's apply removes fields the chart set.
4. Record what a Helm upgrade then does to objects the operator owns.

### M-02: An external control plane with a second writer

**Design reference:** §5.2.

**What to verify:** the property §5.2 asserts and nothing exercises, which is that
this operator never assumes it is the only writer against a shared control plane.

**Test concept:**

1. Point two operators, in two Kubernetes clusters, at one external control plane.
2. Create a `StorageCluster` from each.
3. Confirm neither reconciles the other's cluster, adopts its nodes, or reports
   its state.
4. Delete one deployment's cluster and confirm the other is unaffected.

### M-03: FoundationDB never reaches quorum

**Design reference:** §4.2.

**What to verify:** that `AwaitingFoundationDB` expires rather than holding
forever, and that its message names the coordinator rather than the operator.

**Test concept:**

1. Apply `source.managed` with a `storageClassName` no provisioner serves, so the
   coordinators' volumes never bind.
2. Confirm `status.message` reports what FoundationDB itself says.
3. Confirm the step's deadline expires and `StepDeadlineExceeded` is emitted.
4. Confirm the phase does not become `Available` at any point.

---

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 120       | 0       | 120         |
| Integration | 35        | 0       | 35          |
| E2E         | 16        | 0       | 16          |
| Manual      | 3         | 0       | 3           |
| **Total**   | **174**   | **0**   | **174**     |

Nothing is covered. `ControlPlaneReconciler` has no test file at any level, which
is the finding rather than a matter of this plan being new: it is the root of the
ownership spine, every other controller holds on its verdict, and its entire
behavior is untested. `ControlPlaneOps` does not exist yet,
so their rows are specifications rather than gaps.

---

## 6. What Is Not Yet Covered

| #                                                      | Gap                                                       | Reason                                                                                                                                                                                   |
|--------------------------------------------------------|-----------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| U-01 … U-17                                            | The singleton guard, the probe, and the transition events | `ControlPlaneReconciler` has no test file. All of this is shipped behavior and none of it is asserted                                                                                    |
| U-72 … U-92, U-132 … U-134                             | The component table, the phase it decides, and its events | The controller has one signal today. `U-81` holds design §4.3's rule that `Degraded` stops nothing, and `U-87` and `U-88` hold the rule that only an essential component halts the fleet |
| U-18 … U-27                                            | The `Source` block                                        | Planned, not built. `spec.source` does not exist, and the endpoint is an environment variable                                                                                            |
| U-28 … U-39, U-137 … U-146                             | The installation machine and its four detections          | Planned, not built. The chart installs the control plane today (§12, Q2), and `U-139` keeps a shared CRD from being owned by one deployment                                              |
| U-40 … U-44                                            | Deletion and the cluster hold                             | Planned, not built. The kind carries no finalizer today, so deleting it removes an object and nothing else                                                                               |
| U-45 … U-61, U-93 … U-110, U-126 … U-131, U-135, U-136 | Every `ControlPlaneOps` scenario                          | The kind does not exist. `U-107` keeps an audit record from owning a backup, and `U-127` pins the drain behind the preflight that can refuse the upgrade outright                        |
| U-62 … U-71, I-10, I-11, E-10                          | Retired                                                   | The `SimplyblockDriver` rows moved to [`test-plan-simplyblockdriver.md`](test-plan-simplyblockdriver.md) with the kind. The IDs are not reused                                           |
| I-01 … I-14, I-30 … I-42                               | Every admission rule, including the operations webhook    | Needs `envtest`, because CEL, `Required`, defaulting, and a webhook are enforced by the API server and a fake client applies none of them                                                |
| I-15 … I-22                                            | Lock, cascade, and namespace isolation                    | Needs `envtest` for real `resourceVersion` conflicts and real garbage collection. I-18 additionally needs the FoundationDB CRDs                                                          |
| I-21                                                   | The install applying over the chart's objects             | The behavior is undecided, not merely untested. §12, Q2 owns it                                                                                                                          |
| E-01 … E-18                                            | All end-to-end scenarios                                  | Needs a live deployment with a real FoundationDB. The e2e harness under `test/` is not committed yet. `E-17` is retired with the restore step                                            |
| M-01 … M-03                                            | Dual ownership, a shared external plane, and lost quorum  | Need two Kubernetes clusters, a shared backend, and provisioner-level failure injection                                                                                                  |
| Metrics                                                | The nine metrics of design §9.2                           | Designed, not built. Nothing exports a metric for any of these kinds                                                                                                                     |

### Axis coverage

| Axis                     | Value                          | Scenarios                                                    |
|--------------------------|--------------------------------|--------------------------------------------------------------|
| Control-plane source     | Managed                        | U-19, U-28 … U-38, U-44, E-01                                |
|                          | External                       | U-18, U-20 … U-27, U-39, U-43, E-02, M-02                    |
| Namespace count          | Single namespace               | Every scenario except those below                            |
|                          | Multiple namespaces            | I-19                                                         |
| Kubernetes cluster count | One                            | Every scenario except M-02                                   |
|                          | Two sharing one control plane  | M-02                                                         |
| Preinstalled component   | FoundationDB operator present  | U-137                                                        |
|                          | MongoDB operator present       | U-140                                                        |
|                          | Certificate issuer present     | U-142, U-143                                                 |
|                          | `StorageClass` present         | U-145                                                        |
|                          | Absent, installed              | U-138                                                        |
|                          | Absent, the install holds      | U-141, U-144, U-146                                          |
| Dependency health        | Healthy                        | U-04, U-72, E-01                                             |
|                          | Degraded but serving           | U-73, U-74, U-80, U-87, E-05, E-13                           |
|                          | Degraded, external source      | U-79, which asserts it cannot happen                         |
|                          | Unavailable                    | U-05 … U-08, U-75 … U-77, U-91, U-92, E-03, E-06, E-14, M-03 |
| Operation source         | Managed                        | U-50 … U-55, U-95, E-07                                      |
|                          | External, every action refused | I-30, I-31, I-33, E-15                                       |
| Lifecycle                | Never ready                    | U-09, M-03                                                   |
|                          | Ready then lost                | U-13, E-03                                                   |
|                          | Recovered                      | U-14, E-04                                                   |
|                          | Deleted                        | U-40 … U-44, E-11                                            |

**The dependency-health axis is where this kind earns its tests**, because every
other controller in the operator holds on its verdict. `E-05` and `E-06` are the
pair that matters: a FoundationDB that has lost one coordinator is still serving,
so it reports `Degraded` and must not stop the fleet, and one that has lost quorum
reports `Unavailable` and must.

**The two values that phase splits into are the reason for most of the new rows.**
Before design §4.3 there was one failing state and every row that reached it
asserted the same thing. `U-76` and `U-77` pin which signal wins when the two
disagree, and `U-79` is the row asserting that a source with only one signal
cannot reach the state that needs two.

**The two-Kubernetes-cluster row has one scenario and no automation.** A shared
external control plane is a supported deployment per design §5.2, and `M-02` is
the only thing that would catch this operator assuming it is the only writer.
