# Test Plan: PersistentVolumeOps

Related design: [`designs/crd-redesign/design-persistentvolumeops.md`](../designs/crd-redesign/design-persistentvolumeops.md)

Scope is the operator, its webhooks, and the Kubernetes surface this repository
builds. The control plane (`sbcli`) and SPDK are dependencies, faked at the
boundary: what a row asserts is the operator's response to an answer, never how
a copy is performed.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason.

Scenario text names the target spelling. The kind is `VolumeMigration` today and
`PersistentVolumeOps` after design §4, the target volume is `spec.pvName` today
and `spec.persistentVolumeName` after, the target node is `spec.targetNodeUUID`
today and `spec.migrate.targetNodeRef` after, and the phase and step are one enum
today and `status.phase` plus `status.step` after design §5.

| Class       | Prefix | Harness                                                                |
|-------------|--------|------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, and a mock backend |
| Integration | `I-`   | Full reconcile loop against `envtest` and a mock backend               |
| E2E         | `E-`   | Live simplyblock cluster, real data path, fio verification             |
| Manual      | `M-`   | Needs failure injection or orchestration not automated yet             |

---

## 1. Unit Tests

Pure functions and single reconcile calls against a fake client, with the control
plane replaced by a mock HTTP server.

### Cluster Resolution (design §3)

File: `operator/internal/controllers/volume/persistentvolumeops_resolve_test.go`

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| U-01 | A volume whose class carries `cluster_id` and `pool_name`: both resolved      | Positive | —    |
| U-02 | The volume does not exist: the operation fails with a not-found message       | Negative | —    |
| U-03 | The volume's `StorageClass` was deleted: `ClusterUnresolvable`, no guess made | Negative | —    |
| U-04 | The class exists but carries no `cluster_id`: `ClusterUnresolvable`           | Negative | —    |
| U-05 | The class names a cluster that does not exist: `ClusterUnresolvable`          | Negative | —    |
| U-06 | The volume was not provisioned by this driver: refused, not attempted         | Negative | —    |
| U-07 | The volume is `Released` rather than `Bound`: resolution still succeeds       | Boundary | —    |
| U-08 | A volume whose claim was deleted: resolution still succeeds from the volume   | Boundary | —    |

### Target Resolution (design §4.1)

| #    | Scenario                                                                     | Type     | Test |
|------|------------------------------------------------------------------------------|----------|------|
| U-09 | `spec.migrate.targetNodeRef` names a node: its `status.uuid` is used         | Positive | —    |
| U-10 | The target node does not exist: the operation fails with a not-found message | Negative | —    |
| U-11 | The target node has no `status.uuid`: held, not failed                       | Negative | —    |
| U-12 | The target node is not online: `TargetNodeNotReady`, held                    | Negative | —    |
| U-13 | The target node is the volume's current node: `TargetNodeIsSource`, failed   | Negative | —    |
| U-14 | The target node belongs to another cluster: refused before any backend call  | Negative | —    |
| U-15 | `spec.migrate` absent for `action: Migrate`: rejected                        | Negative | —    |

### The Step Machine (design §5)

File: `operator/internal/controllers/volume/persistentvolumeops_machine_test.go`

| #    | Scenario                                                                        | Type     | Test |
|------|---------------------------------------------------------------------------------|----------|------|
| U-16 | A new operation enters `Validating`                                             | Positive | —    |
| U-17 | `Validating` creates the backend migration and records its UUID                 | Positive | —    |
| U-18 | `Validating` starts one Job per new NVMe-oF path and records each               | Positive | —    |
| U-19 | Every validation Job succeeded: the step advances to `Running`                  | Positive | —    |
| U-20 | One validation Job failed: `ValidationFailed`, the operation fails              | Negative | —    |
| U-21 | A validation Job still pending: the step holds                                  | Negative | —    |
| U-22 | `Running` issues the continue call once across several reconciles               | Negative | —    |
| U-23 | `Running` completes on the migration's reported state, not on the call's return | Positive | —    |
| U-24 | The continue call times out but the migration completed: no second transfer     | Negative | —    |
| U-25 | `Verifying` deletes every validation Job it recorded                            | Positive | —    |
| U-26 | `Verifying` finds a path still connected: it is cleaned up, `StalePathCleaned`  | Negative | —    |
| U-27 | `Verifying` with nothing left: the phase becomes `Succeeded`                    | Positive | —    |
| U-28 | A restart at `Verifying`: the cleanup runs, and it is not skipped               | Negative | —    |
| U-29 | An abort during `Validating`: the backend migration is canceled, `Aborted`      | Positive | —    |
| U-30 | An abort during `Running`: the backend migration is canceled, `Aborted`         | Positive | —    |
| U-31 | An abort during `Verifying`: refused by the graph, the operation runs on        | Negative | —    |
| U-32 | `Running`'s deadline scales with the volume's member count                      | Boundary | —    |
| U-33 | A step's deadline expires: `StepDeadlineExceeded`, the operation fails          | Boundary | —    |
| U-34 | A step value the graph does not declare: `ErrUnknownState`, naming the set      | Negative | —    |
| U-35 | Every declared state appears in the step `Enum` and in the CEL rule             | Boundary | —    |

### Phase Against Step (design §4.2)

| #    | Scenario                                                            | Type     | Test |
|------|---------------------------------------------------------------------|----------|------|
| U-36 | No step value ever appears in `status.phase`                        | Negative | —    |
| U-37 | No phase value ever appears in `status.step.state`                  | Negative | —    |
| U-38 | The terminal success is `Succeeded`, never `Completed`              | Positive | —    |
| U-39 | A terminal operation re-reconciled: no side effect, no backend call | Negative | —    |
| U-40 | `status.migration` groups the UUIDs, connections, and Jobs          | Positive | —    |
| U-41 | `status.deferredSince` is stamped when the operation is first held  | Positive | —    |
| U-42 | `status.deferredSince` is not restamped on a later hold             | Boundary | —    |

### The Exclusion Webhook (design §6)

File: `operator/internal/webhook/persistentvolumeops_validator_test.go`

| #    | Scenario                                                                        | Type     | Test |
|------|---------------------------------------------------------------------------------|----------|------|
| U-43 | No existing operation for the volume: the create is admitted                    | Positive | —    |
| U-44 | A `Running` operation for the same volume: the create is rejected               | Negative | —    |
| U-45 | A `Pending` operation for the same volume: the create is rejected               | Negative | —    |
| U-46 | Only terminal operations for the volume: the create is admitted                 | Positive | —    |
| U-47 | A non-terminal operation for a different volume: the create is admitted         | Negative | —    |
| U-48 | A non-terminal operation in another namespace: the create is admitted           | Boundary | —    |
| U-49 | An update rather than a create: not inspected                                   | Boundary | —    |
| U-50 | A thousand terminal operations in the namespace: the check uses the field index | Boundary | —    |
| U-51 | The second `Validating` finds the volume already migrating and fails            | Negative | —    |

---

## 2. Integration Tests

Full reconcile loop against a real Kubernetes API server via `envtest`.

| #    | Scenario                                                                           | Type     | Test |
|------|------------------------------------------------------------------------------------|----------|------|
| I-01 | `spec.persistentVolumeName` omitted: rejected as `Required`                        | Negative | —    |
| I-02 | `spec.persistentVolumeName` changed after creation: rejected as immutable          | Negative | —    |
| I-03 | `spec.action` outside the enum: rejected                                           | Negative | —    |
| I-04 | `spec.action` changed after creation: rejected as immutable                        | Negative | —    |
| I-05 | `spec.migrate.targetNodeRef` changed after creation: rejected as immutable         | Negative | —    |
| I-06 | `spec.abort` set on a `Running` operation: accepted, since it is the mutable field | Positive | —    |
| I-07 | Short name `pvops` resolves to the same list as the full kind                      | Positive | —    |
| I-08 | Two concurrent creates for one volume: at most one is admitted                     | Negative | —    |
| I-09 | Two creates for two volumes: both admitted, both run                               | Positive | —    |
| I-10 | The webhook is unavailable: the create is rejected rather than admitted            | Negative | —    |
| I-11 | Deleting the owning `StorageNodeOps` garbage-collects the operations it created    | Positive | —    |
| I-12 | An operation created by hand has no owner and survives everything                  | Boundary | —    |
| I-13 | The field index on `spec.persistentVolumeName` is registered and queryable         | Positive | —    |
| I-14 | The controller's role covers reading volumes, classes, and creating Jobs           | Positive | —    |

---

## 3. End-to-End Tests

A live cluster with a real data path. Every row here that does not verify data is
a row that cannot distinguish a correct migration from a silently corrupting one.

| #    | Scenario                                                                              | Type       | Test |
|------|---------------------------------------------------------------------------------------|------------|------|
| E-01 | A migration of an idle volume: it completes and the volume serves I/O from the target | Positive   | —    |
| E-02 | fio verify across a migration: checksums match before and after, byte for byte        | Positive   | —    |
| E-03 | Continuous writes during a migration: every acknowledged write is readable after      | Positive   | —    |
| E-04 | A migration of a volume with many snapshots: it completes within its deadline         | Boundary   | —    |
| E-05 | Exactly one ANA freeze occurs during the cutover                                      | Regression | —    |
| E-06 | More than one freeze would be a defect: the count is asserted, not assumed            | Regression | —    |
| E-07 | The batch transfer's in-flight drain runs before the delta copy                       | Regression | —    |
| E-08 | A slow final step whose call times out: no second transfer, no lost writes            | Regression | —    |
| E-09 | No NVMe-oF path is left connected after the operation                                 | Regression | —    |
| E-10 | A later migration of the same volume succeeds, proving no path was poisoned           | Regression | —    |
| E-11 | An abort mid-copy: the volume is still readable and still on the source               | Negative   | —    |
| E-12 | A data realignment running concurrently: the migration is held, then completes        | Negative   | —    |
| E-13 | The target node goes offline mid-copy: the operation fails and the volume is intact   | Negative   | —    |
| E-14 | Twenty concurrent migrations to one target: all complete, none corrupts               | Boundary   | —    |
| E-15 | The operator is killed mid-copy: the operation resumes and does not restart the copy  | Negative   | —    |

---

## 4. Manual Scenarios

### M-01: Data integrity across a migration, under load

**Design reference:** §9.

**What to verify:** the only property that matters. A migration reporting
`Succeeded` while having lost writes is indistinguishable from a correct one at
every level above the bytes, and this repository has field evidence of exactly
that happening twice for two different reasons.

**Test concept:**

1. A volume with a sustained fio workload in verify mode, writing and re-reading.
2. Record the write acknowledgment log with offsets.
3. Migrate the volume to another node while the workload runs.
4. Confirm fio reports no verification failure at any point.
5. After completion, re-read every acknowledged offset and compare against what
   was written.
6. Repeat with a volume carrying many snapshots, which is the case that lengthens
   the copy and widens every window.

### M-02: The freeze count during cutover

**Design reference:** §9.

**What to verify:** a defect this repository has seen: more than one ANA freeze
during a migration's cutover lost writes silently, and the failure rate was total
in the affected configuration and zero otherwise. The count is the signal, and
nothing in the operation's status shows it.

**Test concept:**

1. Instrument the storage node to log every ANA state transition for the volume.
2. Run a migration under continuous writes.
3. Count the freezes between the start of the cutover and its end.
4. Confirm the count is exactly one.
5. Confirm no acknowledged write is missing from the target afterward.
6. Repeat across at least forty migrations, because the failure was intermittent
   and a single pass proves nothing.

### M-03: A path left connected

**Design reference:** §5, §8.1.

**What to verify:** the condition `Verifying` exists for. Validation Jobs connect
NVMe-oF paths, and when nothing disconnects them the paths outlive the Jobs,
poison the data path, and block every later migration on that volume.

**Test concept:**

1. Run a migration to completion.
2. On both the source and target nodes, list the connected NVMe-oF subsystems.
3. Confirm no path created by a validation Job remains.
4. Kill the operator during `Verifying` and restart it, then confirm the cleanup
   still runs.
5. Run a second migration of the same volume and confirm it succeeds, which is
   the observable that the first left nothing behind.

### M-04: A migration during a data realignment

**Design reference:** §8.2.

**What to verify:** an interaction this repository has measured: an
operator-triggered data realignment blocks every volume migration for tens of
minutes, and roughly half the migrations in an affected window timed out. The
question is whether the operation holds legibly or fails.

**Test concept:**

1. Trigger a data realignment on a loaded cluster.
2. Start a migration while it runs.
3. Confirm the operation holds rather than failing, and that
   `status.deferredSince` is stamped.
4. Measure how long the hold lasts and compare it against the step's deadline.
5. Confirm the migration completes once the realignment finishes, without
   intervention.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered |
|-------------|-----------|---------|-------------|
| Unit        | 51        | 0       | 51          |
| Integration | 14        | 0       | 14          |
| E2E         | 15        | 0       | 15          |
| Manual      | 4         | 0       | 4           |
| **Total**   | **84**    | **0**   | **84**      |

Nothing is covered against the target model. `VolumeMigration` has the most test
files of any kind in this repository, five of them, and none can be cited here:
they assert the merged phase enum, the `pvName` spelling, and a lifecycle with no
`Verifying` step.

That is worth reading precisely. The behavior those tests cover is real and will
survive the rename, so most of `U-16` to `U-30` are re-expressions rather than new
ground. What is genuinely new is `U-25` to `U-28`, the cleanup step, and `U-43` to
`U-51`, the exclusion webhook, and both exist because of failures that reached
production.

---

## 6. What Is Not Yet Covered

| #           | Gap                                                   | Reason                                                                                                           |
|-------------|-------------------------------------------------------|------------------------------------------------------------------------------------------------------------------|
| U-01 … U-08 | Cluster resolution                                    | Partly covered today. `U-03` to `U-05` are new: nothing asserts what happens when the `StorageClass` join breaks |
| U-09 … U-15 | Target resolution                                     | Planned, not built. The registered kind takes a backend UUID, so there is no Kubernetes object to resolve        |
| U-16 … U-24 | The machine up to the copy                            | Covered today against the merged enum, so these are re-expressions                                               |
| U-25 … U-28 | `Verifying`                                           | Planned, not built. The step exists because paths outlived their Jobs and blocked later migrations               |
| U-29 … U-35 | Aborts and deadlines                                  | Partly covered. `U-31`'s refusal is new and comes from the graph                                                 |
| U-36 … U-42 | The phase and step split                              | Planned, not built. They are one enum today                                                                      |
| U-43 … U-51 | The exclusion webhook                                 | Planned, not built. Nothing prevents two migrations of one volume today                                          |
| I-01 … I-14 | Admission, concurrency, and ownership                 | Needs `envtest`, because CEL, `Required`, and real webhook admission cannot be exercised against a fake client   |
| E-01 … E-15 | All end-to-end scenarios                              | Needs a live cluster and a real data path. The e2e harness under `test/` is not committed yet                    |
| E-05 … E-10 | The five regression rows                              | Each pins a defect this repository has seen in the field. None has an automated reproduction                     |
| M-01 … M-04 | Integrity, freeze count, stale paths, and realignment | Need sustained fio verification, node-level ANA instrumentation, and a loaded cluster                            |
| Metrics     | The seven metrics of design §8.2                      | Designed, not built. The existing rebalancer metrics describe its decisions rather than the operation            |
| Events      | The eleven reasons of design §8.1                     | Some exist under other names on the registered kind                                                              |
| Deletion    | A claim or volume deleted mid-migration               | Design §5.1 now specifies it: under Delete the operation aborts and removes itself, under Retain it continues    |

### Axis coverage

| Axis              | Value                           | Scenarios          |
|-------------------|---------------------------------|--------------------|
| Volume state      | Idle                            | E-01               |
|                   | Under continuous write          | E-03, M-01         |
|                   | Many snapshots                  | E-04, U-32         |
|                   | Released, claim deleted         | U-07, U-08         |
| Target node state | Online                          | U-09, E-01         |
|                   | Not yet provisioned             | U-11               |
|                   | Offline                         | U-12, E-13         |
|                   | The source itself               | U-13               |
|                   | In another cluster              | U-14               |
| Concurrency       | One operation per volume        | U-43, E-01         |
|                   | Two on one volume               | U-44, U-51, I-08   |
|                   | Two on two volumes              | U-47, I-09         |
|                   | Twenty to one target            | E-14               |
|                   | Concurrent with a realignment   | E-12, M-04         |
| Lifecycle         | Abort before the copy           | U-29, E-11         |
|                   | Abort during the copy           | U-30               |
|                   | Abort after the copy            | U-31               |
|                   | Operator killed mid-copy        | E-15               |
|                   | Operator killed during cleanup  | U-28, M-03         |
| Join integrity    | Class present                   | U-01               |
|                   | Class deleted                   | U-03               |
|                   | Class without `cluster_id`      | U-04               |
| Data verification | fio verify across the migration | E-02, E-03, M-01   |
|                   | Freeze count asserted           | E-05, E-06, M-02   |
|                   | Stale paths asserted absent     | E-09, E-10, M-03   |
|                   | None                            | Every other E- row |

**The data-verification axis is the only one that matters and it has three
values, all manual or unbuilt.** Every field failure this kind has produced was
invisible to status: a cutover that froze twice lost writes while reporting
success, a batch transfer that skipped its drain corrupted data every status
field said was fine, and an RPC timeout on a final step made the operator retry a
transfer that had already committed. None of those is catchable by a scenario that
checks a phase.

**The concurrency axis has its most dangerous value covered three times.**
`U-44`, `U-51`, and `I-08` all assert that two migrations of one volume cannot
run, at the webhook, at the step, and under real concurrent admission, because
design §6 is explicit that the webhook alone has a race the other kinds' lock
does not.
