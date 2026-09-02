# Test Plan: StoragePool and StoragePoolOps

Related design: [`designs/crd-redesign/design-storagepool.md`](../designs/crd-redesign/design-storagepool.md)

Scope is the operator and the Kubernetes surface this repository builds. The
control plane (`sbcli`) is a dependency, faked at the boundary: what a row
asserts is the operator's response to an answer, never the control plane's own
behavior.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason.

Scenario text names the target spelling. The parent reference is
`spec.clusterName` today and `spec.clusterRef` after design §3.1, the pool's
ceilings are `spec.capacityLimit` and `spec.qos` today and `spec.limits` after,
and the volume defaults are `spec.storageClassParameters` today and
`spec.volumeDefaults` after.

| Class       | Prefix | Harness                                                                |
|-------------|--------|------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, and a mock backend |
| Integration | `I-`   | Full reconcile loop against `envtest` and a mock backend               |
| E2E         | `E-`   | Live simplyblock cluster, real data path                               |
| Manual      | `M-`   | Needs failure injection or orchestration not automated yet             |

---

## 1. Unit Tests

Pure functions and single reconcile calls against a fake client, with the control
plane replaced by a mock HTTP server.

### The Class Assignment (design §5)

File: `operator/internal/controllers/pool/storageclass_assignment_test.go`

| #    | Scenario                                                                       | Type     | Test |
|------|--------------------------------------------------------------------------------|----------|------|
| U-01 | A class carrying the three labels is indexed to the pool they name             | Positive | —    |
| U-02 | Two classes naming one pool are both indexed to it                             | Positive | —    |
| U-03 | The same pool name in two namespaces: each class indexes to its own pool       | Boundary | —    |
| U-04 | The same pool name in two clusters of one namespace: likewise                  | Boundary | —    |
| U-05 | A class whose labels name no pool is indexed to nothing and fails no reconcile | Negative | —    |
| U-06 | A class with a class name that matches no convention is still indexed          | Positive | —    |

### Pool Creation (design §4.1)

File: `operator/internal/controllers/pool/storagepool_controller_unit_test.go`

| #    | Scenario                                                                 | Type     | Test |
|------|--------------------------------------------------------------------------|----------|------|
| U-07 | No `status.uuid`: the pool is created and the UUID recorded              | Positive | —    |
| U-08 | The claim is persisted before the `POST` is issued                       | Positive | —    |
| U-09 | A second reconciler at the same `resourceVersion`: 409, no second `POST` | Negative | —    |
| U-10 | The `POST` returns 5xx: retried, the UUID stays empty                    | Negative | —    |
| U-11 | The `POST` returns 4xx: `PoolCreationFailed` with the body in the event  | Negative | —    |
| U-12 | The cluster has no `status.uuid` yet: held with `ClusterNotReady`        | Negative | —    |
| U-13 | The cluster does not exist: held, not failed                             | Negative | —    |
| U-14 | `status.uuid` present: creation is not attempted again                   | Negative | —    |

### The StorageClass (design §5)

| #    | Scenario                                                                       | Type     | Test |
|------|--------------------------------------------------------------------------------|----------|------|
| U-15 | A non-default pool: no class is created by its reconcile, ever                 | Negative | —    |
| U-16 | A class assigned to the pool sets `cluster_id` and `pool_name` consistently    | Positive | —    |
| U-17 | A class whose `parameters` disagree with its labels is reported, not rewritten | Negative | —    |
| U-18 | The default pool: one class is written, labeled `managed-by`, and not repeated | Positive | —    |
| U-19 | A class assigned after the pool is `Ready`: `StorageClassAssigned` is emitted  | Positive | —    |
| U-20 | The default class deleted out of band: not recreated, `status` drops it        | Negative | —    |
| U-21 | The class carries no owner reference, since the scopes forbid one              | Negative | —    |
| U-22 | `status.storageClassNames` lists every assigned class and nothing else         | Positive | —    |
| U-23 | A class carrying both QoS spellings: new wins, `QoSParameterConflict` emitted  | Negative | —    |

### Deletion and the Bound-Volume Hold (design §6)

| #    | Scenario                                                                                 | Type     | Test |
|------|------------------------------------------------------------------------------------------|----------|------|
| U-24 | Deleting a pool with no bound volumes: class deleted, backend deleted, finalizer cleared | Positive | —    |
| U-25 | Deleting a pool with one bound volume: held, `VolumesStillBound` emitted                 | Negative | —    |
| U-26 | Nothing is deleted while held: the class and the backend pool both survive               | Negative | —    |
| U-27 | The last claim is deleted: the held pool deletion proceeds unattended                    | Positive | —    |
| U-28 | A pool with no `status.uuid`: the finalizer clears without a backend call                | Boundary | —    |
| U-29 | The backend `DELETE` returns 404: treated as success                                     | Boundary | —    |
| U-30 | The backend `DELETE` returns 5xx: retried, the finalizer is kept                         | Negative | —    |
| U-31 | A `PersistentVolume` of another pool's class does not hold this pool                     | Negative | —    |
| U-32 | A released `PersistentVolume` still counts as bound until it is deleted                  | Boundary | —    |
| U-33 | A control-plane volume with no `PersistentVolume` does not hold the deletion             | Boundary | —    |
| U-34 | Zero `PersistentVolume` objects: the count is 0 and the deletion proceeds                | Boundary | —    |

### Spec Grouping (design §3.1)

| #    | Scenario                                                                   | Type     | Test |
|------|----------------------------------------------------------------------------|----------|------|
| U-35 | `spec.limits` reaches the control plane as the pool's ceilings             | Positive | —    |
| U-36 | `spec.volumeDefaults` reaches the class and not the control plane          | Positive | —    |
| U-37 | Both set: neither is written into the other's destination                  | Negative | —    |
| U-38 | Neither set: the pool is created with the control plane's own defaults     | Boundary | —    |
| U-39 | `spec.limits.iops` of 0 is unlimited, not unset                            | Boundary | —    |
| U-40 | `spec.volumeDefaults.enableDHCHAP` reaches the class as the driver expects | Positive | —    |

### StoragePoolOps (design §7)

File: `operator/internal/controllers/pool/storagepoolops_controller_unit_test.go`

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| U-41 | The lock is free: acquired, phase becomes `Running`                           | Positive | —    |
| U-42 | Another operation holds the lock: this one stays `Pending`                    | Negative | —    |
| U-43 | Two reconcilers acquiring one free lock: the loser gets 409                   | Negative | —    |
| U-44 | Terminal re-reconcile: no side effect, the lock is released again             | Negative | —    |
| U-45 | The operation is deleted while `Running`: the finalizer releases the lock     | Positive | —    |
| U-46 | `Rebalance` `Validating`: the volumes on no-longer-allowed nodes are listed   | Positive | —    |
| U-47 | `Rebalance` with nothing misplaced: it succeeds without creating an operation | Negative | —    |
| U-48 | `Rebalance` `Migrating`: one `PersistentVolumeOps` per listed volume          | Positive | —    |
| U-49 | `Rebalance`: a fanned-out migration failing fails the pool operation          | Negative | —    |
| U-50 | An unknown action: terminal failure with the action in the message            | Negative | —    |
| U-51 | The target pool does not exist: the operation fails with a not-found message  | Negative | —    |
| U-52 | `Suspend` or `Resume` as `spec.action`: rejected by the action `Enum`         | Negative | —    |
| U-53 | Every declared state appears in the step `Enum` and in the CEL rule           | Boundary | —    |

---

## 2. Integration Tests

Full reconcile loop against a real Kubernetes API server via `envtest`. The join
of design §5 and the cascade of §6 are between objects that carry no reference,
so real garbage collection is the only way to exercise them.

| #    | Scenario                                                                                 | Type     | Test |
|------|------------------------------------------------------------------------------------------|----------|------|
| I-01 | `spec.clusterRef` omitted: rejected as `Required`                                        | Negative | —    |
| I-02 | `spec.clusterRef` changed after creation: rejected as immutable                          | Negative | —    |
| I-03 | `spec.volumeDefaults` unset at creation, set later: accepted, then frozen                | Boundary | —    |
| I-04 | `spec.volumeDefaults` changed after being set: rejected as immutable                     | Negative | —    |
| I-05 | `spec.limits` changed after creation: accepted                                           | Positive | —    |
| I-06 | `spec.volumeDefaults.filesystem` outside the enum: rejected                              | Negative | —    |
| I-07 | `spec.limits.iops` negative: rejected by the minimum                                     | Boundary | —    |
| I-08 | `spec.allowedNodes` with a duplicate: rejected by `listType=set`                         | Negative | —    |
| I-09 | `StoragePoolOps.spec.action` outside the enum: rejected                                  | Negative | —    |
| I-10 | `StoragePoolOps.spec.poolRef` changed after creation: rejected                           | Negative | —    |
| I-11 | Short names `sp` and `spops` resolve to the same lists as the full kinds                 | Positive | —    |
| I-12 | Deleting the `StorageCluster` cascades to a pool with no bound volumes                   | Positive | —    |
| I-13 | Deleting the `StorageCluster` with a pool holding bound volumes: both stay `Terminating` | Negative | —    |
| I-14 | Removing the claims then releases the pool, then the cluster, in that order              | Positive | —    |
| I-15 | The class outlives a force-removed finalizer, and `status.storageClassName` finds it     | Negative | —    |
| I-16 | Two pools of one cluster: two classes, neither deleting the other's                      | Positive | —    |
| I-17 | Two pools with the same name in two namespaces: two classes, no collision                | Positive | —    |
| I-18 | An operation on one pool does not lock another pool of the same cluster                  | Negative | —    |
| I-19 | The controller's role covers `storageclasses` at cluster scope                           | Positive | —    |

---

## 3. End-to-End Tests

A live simplyblock cluster with a real data path.

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| E-01 | A pool created: a claim against its class binds and serves I/O                | Positive | —    |
| E-02 | `spec.volumeDefaults.enableCompression`: a volume is created compressed       | Positive | —    |
| E-03 | `spec.volumeDefaults.enableDHCHAP`: the NVMe-oF connection authenticates      | Positive | —    |
| E-04 | `spec.limits.capacity` reached: a further claim fails with a legible reason   | Boundary | —    |
| E-05 | `spec.limits.iops` reached: I/O is throttled rather than failing              | Boundary | —    |
| E-06 | Raising `spec.limits.capacity`: the previously failing claim now binds        | Positive | —    |
| E-07 | Deleting a pool with a bound claim: held, and the workload keeps serving I/O  | Negative | —    |
| E-08 | The class is deleted out of band: an existing volume keeps serving I/O        | Boundary | —    |
| E-09 | The class is deleted out of band: a new claim fails until it is restored      | Negative | —    |
| E-10 | `action: Suspend`: existing volumes keep serving, new claims do not provision | Positive | —    |
| E-11 | `action: Resume`: new claims provision again                                  | Positive | —    |
| E-12 | Narrowing `spec.allowedNodes` then `action: Rebalance`: volumes move          | Positive | —    |
| E-13 | Sustained fio through a `Rebalance`: no I/O errors, checksums match after     | Positive | —    |

---

## 4. Manual Scenarios

### M-01: Deleting a cluster whose pool has bound volumes

**Design reference:** §6.

**What to verify:** the decision this design takes on
[`design-crd-model.md`](../designs/crd-redesign/design-crd-model.md) §9.3's open question. The
owner reference means a cluster delete cascades, and the pool's finalizer is the
only thing that stops it destroying tenant data.

**What to verify specifically:** that the hold is legible. Correct behavior here
looks exactly like a stuck finalizer, and an administrator who reaches for
`--force` gets the outcome the hold exists to prevent.

**Test concept:**

1. Create a cluster, a pool, and a bound claim with data on it.
2. `kubectl delete storagecluster production`.
3. Confirm the command does not return, the cluster is `Terminating`, and the
   pool is `Terminating` behind it.
4. Confirm `VolumesStillBound` names the claim, and that `kubectl describe` on
   either object leads to it.
5. Confirm the workload is still serving I/O throughout.
6. Delete the claim and confirm the pool, then the cluster, complete without
   further input.
7. Confirm the data was not destroyed at any point before step 6.

### M-02: A class orphaned by a force-removed finalizer

**Design reference:** §5.

**What to verify:** the failure mode §5 names, which is that nothing but §6's hold
keeps a class and its pool consistent, and formerly that only the finalizer
deleted the class, so any path that skips it leaves a class whose `parameters`
name a pool that no longer exists.

**Test concept:**

1. Create a pool with no bound volumes.
2. Remove its finalizer by hand, so the object is deleted without cleanup.
3. Confirm the `StorageClass` still exists.
4. Confirm a new claim against it fails at provision time with an error from the
   control plane rather than from Kubernetes.
5. Confirm the orphan is findable: its labels still name the namespace, cluster,
   and pool, which is what a cleanup would select on.

### M-03: A pool at its capacity limit under load

**Design reference:** §3.1, §9.2.

**What to verify:** what a tenant experiences when a pool fills, which is the
question `simplyblock_storagepool_used_bytes` against `capacity_bytes` exists to give
warning of.

**Test concept:**

1. Create a pool with a small `spec.limits.capacity`.
2. Fill it with claims until the next one cannot be satisfied.
3. Confirm the failing claim reports a legible reason rather than pending
   silently.
4. Confirm existing volumes keep serving I/O.
5. Confirm `CapacityExhausted` is emitted on the pool, once rather than per
   attempt.
6. Raise the limit and confirm the pending claim binds without intervention.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 53        | 0       | 53          |
| Integration | 19        | 0       | 19          |
| E2E         | 13        | 0       | 13          |
| Manual      | 3         | 0       | 3           |
| **Total**   | **88**    | **0**   | **88**      |

Nothing is covered. `StoragePoolReconciler` is 643 lines and has a
`_controller_test.go` and a `_controller_unit_test.go` beside it, neither of which
this plan can cite, because the behaviors above are the target model's rather
than the registered one's. What those files do test is not lost: it is the
creation and class-creation paths, which `U-07` to `U-23` restate against the
target spelling.

---

## 6. What Is Not Yet Covered

| #           | Gap                                             | Reason                                                                                                          |
|-------------|-------------------------------------------------|-----------------------------------------------------------------------------------------------------------------|
| U-01 … U-06 | The `StorageClass` naming convention            | It is a one-line pure function with four consumers, and nothing asserts they agree                              |
| U-07 … U-14 | Pool creation                                   | Partly covered today against the registered spelling. `U-08` and `U-09` are new: the claim does not exist yet   |
| U-15 … U-23 | The class and its labels                        | Partly covered today. `U-20`, `U-21`, and `U-22` are new                                                        |
| U-24 … U-34 | Deletion and the bound-volume hold              | Planned, not built. Nothing checks bound volumes today, so a pool deletes while claims are bound                |
| U-35 … U-40 | The regrouped spec                              | Planned, not built                                                                                              |
| U-41 … U-53 | Every `StoragePoolOps` scenario                 | The kind does not exist. `spec.action` is declared and unread                                                   |
| I-01 … I-11 | Every admission rule                            | Needs `envtest`, because CEL and `Required` are enforced by the API server and a fake client applies neither    |
| I-12 … I-19 | The cascade, the join, and namespace isolation  | Needs `envtest` for real garbage collection and real cluster-scoped objects                                     |
| E-01 … E-13 | All end-to-end scenarios                        | Needs a live cluster. The e2e harness under `test/` is not committed yet                                        |
| E-10 … E-12 | `Rebalance`                                     | Design §7 marks the action provisional, and it is a fan-out of `PersistentVolumeOps` rather than a backend call |
| M-01 … M-03 | The cascade, an orphaned class, and a full pool | Need a real cluster, a force-removed finalizer, and a workload filling a pool                                   |
| Metrics     | The eight metrics of design §9.2                | Designed, not built. Nothing exports a metric for either kind                                                   |
| Events      | The fourteen reasons of design §9.1             | The pool controller emits no event at all today                                                                 |
| Assignment  | Zero-or-more classes per pool                   | Design §5 replaced one generated class with an assignment, so `U-01` … `U-23` are rewritten and unbuilt         |

### Axis coverage

| Axis              | Value                            | Scenarios        |
|-------------------|----------------------------------|------------------|
| Pools per cluster | One                              | Most scenarios   |
|                   | Two                              | U-02, I-16, I-18 |
| Namespace count   | Single                           | Most scenarios   |
|                   | Multiple, same pool name         | U-03, I-17       |
| Cluster count     | One                              | Most scenarios   |
|                   | Two, same pool name              | U-04             |
| Bound volumes     | Zero                             | U-24, U-34       |
|                   | One                              | U-25, E-07, M-01 |
|                   | Released but not deleted         | U-32             |
|                   | Unmanaged, no `PersistentVolume` | U-33             |
| Deletion path     | Pool deleted directly            | U-24, U-25       |
|                   | Cluster deleted, cascading       | I-12, I-13, M-01 |
|                   | Finalizer removed by hand        | I-15, M-02       |
| Class state       | Present                          | U-19             |
|                   | Missing, restored                | U-20, E-09       |
|                   | Orphaned                         | I-15, M-02       |
| Capacity          | Below the limit                  | E-01             |
|                   | At the limit                     | E-04, M-03       |
|                   | Raised after being reached       | E-06, M-03       |

**The deletion-path axis is what this document exists to settle.** All three of
its values have rows, and `M-01` is the one that matters, because the hold it
verifies is the only thing standing between an owner reference and a
`kubectl delete storagecluster` that destroys tenant data.

**The class-state axis has an orphan value and no automated coverage of it.**
`I-15` and `M-02` are the only rows, and the orphan is reachable by any path that
skips the finalizer, which includes a force delete and a namespace deletion that
outruns it.
