# Test Plan: StorageBackupPolicy, StorageBackup, and StorageBackupOps

Related design: [`designs/crd-redesign/design-storagebackup.md`](../designs/crd-redesign/design-storagebackup.md)

Scope is the operator and the Kubernetes surface this repository builds. The
control plane (`sbcli`) and the S3 target are dependencies, faked at the
boundary: what a row asserts is the operator's response to an answer, never how
a backup is written.

Scenario IDs are permanent and are never reused or renumbered. A `—` in the
`Test` column means nothing implements the scenario yet, and every such row
reappears in §6 with its reason.

Scenario text names the target spelling. The policy is `BackupPolicy` today and
`StorageBackupPolicy` after design §3, restore is `BackupRestore` today and the
`Restore` action of `StorageBackupOps` after, the parent
reference is `spec.clusterName` today and `spec.clusterRef` after, and the
backup's status is twenty-two flat fields today and `status.backup` plus
`status.source` after design §5.2.

| Class       | Prefix | Harness                                                                |
|-------------|--------|------------------------------------------------------------------------|
| Unit        | `U-`   | No cluster: pure functions, a fake `client.Client`, and a mock backend |
| Integration | `I-`   | Full reconcile loop against `envtest` and a mock backend               |
| E2E         | `E-`   | Live simplyblock cluster with a real S3 target and a real data path    |
| Manual      | `M-`   | Needs failure injection or orchestration not automated yet             |

---

## 1. Unit Tests

Pure functions and single reconcile calls against a fake client, with the control
plane replaced by a mock HTTP server.

### Policy Selection (design §4.1)

File: `operator/internal/controllers/backup/storagebackuppolicy_selector_test.go`

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| U-01 | A selector matching two claims: both are attached                             | Positive | —    |
| U-02 | An absent selector: nothing is attached, `SelectorEmpty` emitted              | Boundary | —    |
| U-03 | A selector matching nothing: nothing is attached, and it is not an error      | Boundary | —    |
| U-04 | A claim gains a matching label: it is attached on the next reconcile          | Positive | —    |
| U-05 | A claim loses the label: it is detached, and its backups are not deleted      | Negative | —    |
| U-06 | A claim whose volume is not backed by this cluster: `ClaimNotEligible`        | Negative | —    |
| U-07 | A claim that is not bound yet: skipped, and retried when it binds             | Negative | —    |
| U-08 | A deleted claim: detached, and its backups survive                            | Negative | —    |
| U-09 | Two policies selecting one claim: both attach, and neither detaches the other | Boundary | —    |
| U-10 | `status.attachedClaims` records the claim and its `PersistentVolume`          | Positive | —    |
| U-11 | An attach that the control plane rejects: retried, not marked attached        | Negative | —    |

### Policy Reconcile (design §4.2, §9)

| #    | Scenario                                                                 | Type     | Test |
|------|--------------------------------------------------------------------------|----------|------|
| U-12 | A new policy: created in the control plane, `status.policyID` recorded   | Positive | —    |
| U-13 | A changed `spec.schedule`: applied to the control plane                  | Positive | —    |
| U-14 | A changed `spec.maxVersions`: applied                                    | Positive | —    |
| U-15 | The operator never runs the schedule itself                              | Negative | —    |
| U-16 | The operator never prunes a backup itself                                | Negative | —    |
| U-17 | `spec.maxVersions` of 0 means no limit by count, not immediate deletion  | Boundary | —    |
| U-18 | A malformed `spec.schedule`: rejected before the control plane is called | Negative | —    |
| U-19 | Deleting a policy deletes its `StorageBackup` objects, not the backups   | Negative | —    |
| U-20 | `status.lastBackupAt` is updated from what the control plane reports     | Positive | —    |

### Backup Lifecycle (design §5)

File: `operator/internal/controllers/backup/storagebackup_controller_unit_test.go`

| #    | Scenario                                                                      | Type     | Test |
|------|-------------------------------------------------------------------------------|----------|------|
| U-21 | A backup of a bound claim: created, phase reaches `Available`                 | Positive | —    |
| U-22 | The claim does not exist: the backup fails with a not-found message           | Negative | —    |
| U-23 | The claim is not bound: held, not failed                                      | Negative | —    |
| U-24 | The cluster has no backup target configured: `BackupTargetMissing`            | Negative | —    |
| U-25 | `spec.snapshotName` set: that snapshot is used rather than a new one taken    | Positive | —    |
| U-26 | The control plane reports failure: phase becomes `Failed` with the reason     | Negative | —    |
| U-27 | `Available` is terminal: a later reconcile is a no-op                         | Negative | —    |
| U-28 | A backup created by a policy carries an owner reference to it                 | Positive | —    |
| U-29 | A backup created by hand carries no owner reference                           | Boundary | —    |
| U-30 | Deleting a `StorageBackup` deletes the backup in the control plane            | Positive | —    |
| U-31 | A backup the control plane pruned: the object is deleted silently, not failed | Boundary | —    |

### Status Grouping (design §5.2)

| #    | Scenario                                                                             | Type     | Test |
|------|--------------------------------------------------------------------------------------|----------|------|
| U-32 | `status.backup` carries the ID, size, and both timestamps                            | Positive | —    |
| U-33 | `status.source` carries the pool, the logical volume, and the filesystem type        | Positive | —    |
| U-34 | `status.source` is written once and never updated afterward                          | Negative | —    |
| U-35 | The source volume moves pool after the backup: `status.source.poolName` is unchanged | Negative | —    |
| U-36 | `status.backup.previousBackupID` is recorded when the backup is incremental          | Positive | —    |
| U-37 | A full backup: `previousBackupID` is absent, not empty-string                        | Boundary | —    |
| U-38 | The control plane returns no size: the field stays absent, not zero                  | Boundary | —    |

### Restore (design §6, §8)

File: `operator/internal/controllers/backup/storagebackupops_restore_test.go`

| #    | Scenario                                                                                                  | Type     | Test |
|------|-----------------------------------------------------------------------------------------------------------|----------|------|
| U-39 | A restore into a new claim: the claim is created and bound                                                | Positive | —    |
| U-40 | The claim already exists at create: the webhook rejects it (design §7)                                    | Negative | —    |
| U-41 | The claim appears after admission: `Binding` refuses with `ClaimExists`                                   | Negative | —    |
| U-42 | `spec.restore.targetPool` unset: the backup's own pool is used                                            | Positive | —    |
| U-43 | The backup's pool no longer exists: refused with `PoolNotFound`, naming it                                | Negative | —    |
| U-44 | `spec.restore.targetPool` set to a pool that does not exist: refused                                      | Negative | —    |
| U-45 | The created claim carries `spec.restore.claimLabels`                                                      | Positive | —    |
| U-46 | The created claim has no owner reference and survives the operation's deletion                            | Positive | —    |
| U-47 | The restored claim's size matches the backup's                                                            | Positive | —    |
| U-48 | The restored claim's filesystem matches `status.source.fsType`                                            | Positive | —    |
| U-49 | The backup is not `Available`: the restore is refused                                                     | Negative | —    |
| U-50 | `AwaitingVolume` holds until the control plane reports the volume restored                                | Negative | —    |
| U-51 | An abort during `Validating`: `Aborted`, and nothing was created                                          | Positive | —    |
| U-52 | An abort during `Binding`: refused by the graph, the operation runs on                                    | Negative | —    |
| U-71 | A restart mid-`Binding`: the operation completes its own labeled claim rather than tripping `ClaimExists` | Positive | —    |

### Import (withdrawn)

Design §13 retires `BackupImport` rather than absorbing it: the store is the
inventory, so a backup another cluster wrote is discovered by the walk of design
§5.1 rather than registered by an operation. The seven rows keep their identifiers,
which are never reused, and `U-72` and `U-73` cover the path that replaces them.

| #        | Scenario                                                                               | Type | Test |
|----------|----------------------------------------------------------------------------------------|------|------|
| ~~U-53~~ | An import of a foreign backup. Withdrawn with the action, replaced by `U-72`           | —    | —    |
| ~~U-54~~ | The registered backup is restorable afterward. Withdrawn, replaced by `U-73`           | —    | —    |
| ~~U-55~~ | `spec.import.sourceBackupID` the control plane does not know. Withdrawn with the field | —    | —    |
| ~~U-56~~ | `spec.import.sourceClusterID` that is not a UUID. Withdrawn with the field             | —    | —    |
| ~~U-57~~ | The registered backup's source cluster UUID. Withdrawn. `U-72` asserts it on the walk  | —    | —    |
| ~~U-58~~ | An import run twice for one backup. Withdrawn with the action                          | —    | —    |
| ~~U-59~~ | An abort during `Registering`. Withdrawn with the step                                 | —    | —    |

### The Action Discriminator (design §6)

| #        | Scenario                                                                 | Type     | Test |
|----------|--------------------------------------------------------------------------|----------|------|
| U-60     | `action: Restore` with `spec.backupRef` set: accepted                    | Positive | —    |
| ~~U-61~~ | `action: Import` with `spec.backupRef` absent. Withdrawn with the action | —        | —    |
| U-62     | An unknown action: terminal failure with the action in the message       | Negative | —    |
| U-63     | `spec.restore` absent for `action: Restore`: rejected                    | Negative | —    |
| ~~U-64~~ | `spec.import` absent for `action: Import`. Withdrawn with the action     | —        | —    |
| U-65     | Every declared state appears in the step `Enum` and in the CEL rule      | Boundary | —    |

### The Operation Lock (design §6)

| #    | Scenario                                                                  | Type     | Test |
|------|---------------------------------------------------------------------------|----------|------|
| U-66 | The lock is free: acquired, phase becomes `Running`                       | Positive | —    |
| U-67 | Another operation holds the backup's lock: this one stays `Pending`       | Negative | —    |
| U-68 | Two operations on two different backups run without contending            | Positive | —    |
| U-69 | Terminal re-reconcile: no side effect, the lock is released again         | Negative | —    |
| U-70 | The operation is deleted while `Running`: the finalizer releases the lock | Positive | —    |

### Store Discovery (design §5.1)

File: `operator/internal/controllers/backup/storagebackup_controller_unit_test.go`

The path that replaces the retired import action. A backup another cluster wrote
becomes visible because the walk finds it, so what an import used to assert about a
foreign backup is asserted here about a discovered one.

| #    | Scenario                                                                                                                                 | Type     | Test |
|------|------------------------------------------------------------------------------------------------------------------------------------------|----------|------|
| U-72 | A backup in the store this cluster did not write: one `StorageBackup` is created, carrying the writing cluster's UUID in `status.source` | Positive | —    |
| U-73 | A discovered foreign backup restores through the ordinary `Restore` action                                                               | Positive | —    |

---

## 2. Integration Tests

Full reconcile loop against a real Kubernetes API server via `envtest`. The CEL
rules and the ownership cascades cannot be exercised any other way.

| #        | Scenario                                                                         | Type     | Test |
|----------|----------------------------------------------------------------------------------|----------|------|
| I-01     | `action: Restore` without `spec.backupRef`: rejected by the CEL rule             | Negative | —    |
| ~~I-02~~ | `action: Import` with `spec.backupRef`. Withdrawn with the action                | —        | —    |
| I-03     | `spec.action` outside the enum: rejected                                         | Negative | —    |
| I-04     | `spec.action` changed after creation: rejected as immutable                      | Negative | —    |
| I-05     | `spec.backupRef` changed after creation: rejected as immutable                   | Negative | —    |
| I-06     | `spec.restore.claimName` changed after creation: rejected as immutable           | Negative | —    |
| I-07     | `StorageBackup.spec.claimRef` changed after creation: rejected as immutable      | Negative | —    |
| I-08     | `StorageBackupPolicy.spec.clusterRef` changed after creation: rejected           | Negative | —    |
| I-09     | `spec.maxVersions` negative: rejected by the minimum                             | Boundary | —    |
| I-10     | Short names `sbp`, `sb`, and `sbops` resolve to the same lists as the full kinds | Positive | —    |
| I-11     | Deleting a policy garbage-collects the `StorageBackup` objects it owns           | Positive | —    |
| I-12     | Deleting a policy leaves a hand-created `StorageBackup` alone                    | Negative | —    |
| I-13     | Deleting a `StorageBackupOps` garbage-collects the claim it restored             | Positive | —    |
| I-14     | Two policies in two namespaces with the same name: neither reads the other       | Negative | —    |
| I-15     | A policy selecting a claim in its own namespace only                             | Negative | —    |
| I-16     | The controller's role covers creating claims and reading volumes                 | Positive | —    |

---

## 3. End-to-End Tests

A live cluster with a real S3 target and a real data path. This is the only class
that can prove the layer does what it is for.

| #        | Scenario                                                                             | Type     | Test |
|----------|--------------------------------------------------------------------------------------|----------|------|
| E-01     | A backup of a claim with known data: reaches `Available`                             | Positive | —    |
| E-02     | Restoring that backup: the restored claim's checksums match the original exactly     | Positive | —    |
| E-03     | A backup taken under load: the restore's checksums match the snapshot point          | Positive | —    |
| E-04     | A policy on a schedule: backups appear at the interval, and retention prunes them    | Positive | —    |
| E-05     | `spec.maxVersions` of 3: the fourth backup prunes the first                          | Boundary | —    |
| E-06     | `spec.maxAge`: a backup older than it is pruned                                      | Boundary | —    |
| E-07     | An incremental chain: `previousBackupID` links them and each restores correctly      | Positive | —    |
| E-08     | Deleting a base backup an incremental one depends on: what happens is recorded       | Negative | —    |
| ~~E-09~~ | An import from a second cluster. Withdrawn with the action, replaced by `E-14`       | —        | —    |
| E-10     | A restore into a pool other than the source: the volume lands there and serves I/O   | Positive | —    |
| E-11     | The S3 target is unreachable: the backup fails with a legible reason                 | Negative | —    |
| E-12     | Detaching a claim from a policy: its existing backups remain restorable              | Negative | —    |
| E-13     | Restoring a claim whose original still exists: two independent volumes, both correct | Positive | —    |
| E-14     | A backup written by a second cluster: the walk discovers it and it restores here     | Positive | —    |

---

## 4. Manual Scenarios

### M-01: The round trip, with checksums

**Design reference:** §12.

**What to verify:** the only thing that matters about this layer, which is that a
backup can be restored and the data comes back. A backup that completes and
cannot be restored is worse than no backup, and nothing short of a round trip
catches it.

**Test concept:**

1. Create a claim, write a known dataset, and record checksums per offset.
2. Take a backup and wait for `Available`.
3. Write more data to the original, so the two diverge.
4. Restore the backup into a new claim.
5. Mount both and confirm the restored claim matches the checksums from step 1
   exactly, and that the original matches step 3.
6. Confirm neither volume is affected by the other.

### M-02: A restore attempted over a running workload

**Design reference:** §7 and §8.

**What to verify:** the refusal that stands between a restore and destroying a
running workload's data, at both of the places design §7 says it lives. `U-40` and
`U-41` assert the step's half against a fake client, and this verifies both where
somebody would actually make the mistake.

**Test concept:**

1. A claim with a workload writing to it continuously.
2. Create a `StorageBackupOps` with `action: Restore` and
   `spec.restore.claimName` set to that claim's name. Confirm the create is
   rejected by admission, naming the claim, and that no object was created.
3. Then the race the webhook cannot close: create the operation with a
   `claimName` that does not exist yet, and create a claim of that name before
   the operation reaches `Validating`. Confirm the operation reaches `Failed`
   with `ClaimExists` rather than adopting it.
4. Confirm the workload's I/O was not interrupted and its data is unchanged.
5. Confirm no logical volume was created in the control plane by either attempt.

### M-03: A policy that silently stops

**Design reference:** §11.2.

**What to verify:** the failure mode `simplyblock_storagebackup_age_seconds` exists for.
A policy that stops running produces no failure and no event: it just stops
producing backups, and the only symptom is the age of the newest one climbing.

**Test concept:**

1. A policy backing up a claim hourly, running normally.
2. Break it in a way that produces no error: detach the claim at the control
   plane out of band, or remove the policy from the control plane while leaving
   the Kubernetes object.
3. Confirm no event is emitted and the policy still reports `Active`.
4. Confirm `simplyblock_storagebackup_age_seconds` for the claim climbs past the
   schedule.
5. Record how long it takes for anything else to notice, which is the number that
   justifies the alert.

---

## 5. Coverage Summary

| Class       | Scenarios | Covered | Not covered | Withdrawn |
|-------------|-----------|---------|-------------|-----------|
| Unit        | 64        | 0       | 64          | 9         |
| Integration | 15        | 0       | 15          | 1         |
| E2E         | 13        | 0       | 13          | 1         |
| Manual      | 3         | 0       | 3           | 0         |
| **Total**   | **95**    | **0**   | **95**      | **11**    |

A withdrawn row is one whose behavior the design removed. Its identifier stays in
the matrix, struck through, because identifiers are never reused. It counts as
neither a scenario nor a gap. All eleven are the retired `Import` action.

Nothing is covered. Two of the four registered kinds have unit test files
(`backuppolicy_controller_unit_test.go` and `backuprestore_controller_test.go`),
and neither can be cited here: they test kinds that are being renamed or absorbed,
against a status shape design §5.2 regroups.

The distribution is unusual for this repository in one respect. Thirteen
end-to-end scenarios against sixty-four unit tests is a higher end-to-end share than
any other plan here, and it is not padding: a backup layer's correctness is
whether data comes back, and that is not a property a fake client has an opinion
about.

---

## 6. What Is Not Yet Covered

| #           | Gap                                                          | Reason                                                                                                                       |
|-------------|--------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------|
| U-01 … U-11 | Policy selection                                             | Planned, not built. The registered policy has no claim selector at all, so there is nothing to select with                   |
| U-12 … U-20 | Policy reconcile and retention                               | Partly covered today against the registered spelling. `U-15` and `U-16` are new: they assert what the operator must not do   |
| U-21 … U-31 | Backup lifecycle                                             | Partly covered today. `U-28`, `U-29`, and `U-31` are new                                                                     |
| U-32 … U-38 | The regrouped status                                         | Planned, not built. `U-34` and `U-35` are the rows that keep the source group a snapshot rather than a live view             |
| U-39 … U-52 | Restore                                                      | Planned, not built. `U-40` and `U-41` are the most important rows here: they stand between a restore and a running workload  |
| U-60 … U-70 | The action discriminator and the lock                        | The kind does not exist                                                                                                      |
| U-72, U-73  | Store discovery                                              | Planned, not built. They are what a foreign backup is proved by now that `BackupImport` is retired (design §13)              |
| I-01 … I-10 | Every admission rule                                         | Needs `envtest`, because CEL and immutability are enforced by the API server and a fake client applies neither               |
| I-11 … I-16 | Ownership cascades and namespace isolation                   | Needs `envtest` for real garbage collection                                                                                  |
| E-01 … E-14 | All end-to-end scenarios                                     | Needs a live cluster and a real S3 target. The e2e harness under `test/` is not committed yet                                |
| E-08        | Deleting a base backup an incremental one depends on         | Design §14 Q1 says nothing here can answer whether that is safe. The row records what happens rather than asserting a result |
| M-01 … M-03 | The round trip, a restore over a workload, and a silent stop | Need real data, a running workload, and an out-of-band break                                                                 |
| Metrics     | The eight metrics of design §11.2                            | Designed, not built                                                                                                          |
| Events      | The fourteen reasons of design §11.1                         | The backup controllers emit no event at all today                                                                            |
| Streaming   | Nothing asserts a `?watch=true` subscription                 | Design §10 now has both reads on a stream, so a coalesced delivery and a reconnect snapshot both need scenarios              |

### Axis coverage

| Axis              | Value                            | Scenarios          |
|-------------------|----------------------------------|--------------------|
| Policy selection  | Selector matching several claims | U-01, E-04         |
|                   | Selector matching nothing        | U-03               |
|                   | No selector                      | U-02               |
|                   | Two policies, one claim          | U-09               |
| Backup origin     | Taken by a policy                | U-28, E-04         |
|                   | Taken by hand                    | U-29, E-01         |
|                   | Written by another cluster       | U-72, E-14         |
| Backup chain      | Full                             | U-37, E-01         |
|                   | Incremental                      | U-36, E-07         |
|                   | Base of a chain, deleted         | E-08               |
| Restore target    | New claim, source pool           | U-42, E-02         |
|                   | New claim, different pool        | U-44, E-10         |
|                   | Existing claim                   | U-40, U-41, M-02   |
|                   | Source pool gone                 | U-43               |
|                   | Original still exists            | E-13               |
| Data verification | Checksums after restore          | E-02, M-01         |
|                   | Backup taken under load          | E-03               |
|                   | No verification                  | Every other E- row |
| Dependency health | Control plane and S3 reachable   | E-01               |
|                   | S3 unreachable                   | E-11               |
|                   | Policy silently stopped          | M-03               |
| Namespace count   | Single                           | Most scenarios     |
|                   | Multiple                         | I-14, I-15         |
| Cluster count     | One                              | Most scenarios     |
|                   | Two, discovering across          | U-72, U-73, E-14   |

**The data-verification axis is the one this plan is built around**, and only
three rows populate it. `E-02` and `M-01` are the round trip, `E-03` is the round
trip under load, and everything else asserts that objects reached the right phase.
A backup layer whose tests all pass and whose restores return the wrong bytes
would look exactly like a healthy one from every other row here.

**The restore-target axis has its most dangerous value covered three times.**
`U-40`, `U-41`, and `M-02` all assert that a restore refuses an existing claim,
at three different levels, because that refusal is the difference between a
recovery tool and a data-destruction tool.
