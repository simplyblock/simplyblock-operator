# Test Plan: Consistency-Group Backup and Restore

**Related design:** [`designs/design-consistency-group-backup.md`](../designs/design-consistency-group-backup.md)  
**Status:** Draft (scenarios expand as the design settles)

Legend. `U` is a control-plane or operator unit test (mocked data plane, fake or
real database per tier), `I` is a control-plane integration test (real
FoundationDB, mocked nodes), `E` is an operator envtest, and `M` is a manual or
live-cluster scenario. Type is one of `Positive`, `Negative`, `Boundary`, or
`Regression`. A `—` in the Test column is a scenario nothing covers yet.

This plan tracks the design's Phase 1 (control plane) and Phase 2 (operator)
first. The risk concentrates in two places, so both get a red-first regression:
the all-or-nothing commit barrier (a group backup must not be visible while a
member is still uploading) and the group-restore unwind (a failed group restore
must leave no half-group).

---

## 1. Control-plane unit (the commit barrier and the records)

The harness boundary: the data plane is mocked, so these drive the group-backup
controller and task runner against fake RPCs and assert the record, manifest, and
barrier behavior. They ride the existing backup unit harness (`sbcli/tests/unit/backup/`)
and the consistency-group harness.

### Commit barrier (design §3.3, §5)

| #   | Scenario                                                                                        | Type       | Test |
|-----|-------------------------------------------------------------------------------------------------|------------|------|
| U-1 | A group backup stays `in_progress` and writes no group manifest while any member is uploading   | Positive   | —    |
| U-2 | The group manifest is written and the record goes `completed` only after every member completed | Positive   | —    |
| U-3 | A member upload that fails forever fails the whole group backup; completed members survive      | Negative   | —    |
| U-4 | Writing the group manifest twice onto the same key is idempotent (group-task retry)             | Boundary   | —    |
| U-5 | A group backup commits while a member is still uploading (must be impossible)                   | Regression | —    |

### Records and manifest (design §4)

| #   | Scenario                                                                                            | Type     | Test |
|-----|-----------------------------------------------------------------------------------------------------|----------|------|
| U-6 | The `GroupBackup` record names exactly the generation's member backups                              | Positive | —    |
| U-7 | Each member `Backup` carries `group_backup_id` and `group_seq`; a standalone backup carries neither | Positive | —    |
| U-8 | The group manifest lists every member backup id and the generation's expected membership            | Positive | —    |
| U-9 | A homogeneity violation (a member on a different bucket or encryption) is refused, members named    | Negative | —    |

---

## 2. Control-plane integration (real FoundationDB, mocked nodes)

The harness boundary: real `DBController` against a testcontainers FoundationDB,
nodes mocked. These prove the records round-trip and the indexes resolve.

| #   | Scenario                                                                                  | Type     | Test |
|-----|-------------------------------------------------------------------------------------------|----------|------|
| I-1 | A `GroupBackup` and its member `Backup`s persist and read back whole                      | Positive | —    |
| I-2 | The `group_backup_id` index resolves "members of a group backup" as a point read          | Positive | —    |
| I-3 | The `consistency_group_id` index resolves "group backups of a group"                      | Positive | —    |
| I-4 | Import of a group backup preserves `uuid`, `s3_id`, and `group_backup_id`, all-or-nothing | Positive | —    |
| I-5 | Import with an id collision across clusters is rejected before any write                  | Negative | —    |

---

## 3. Group restore (design §6)

| #    | Scenario                                                                               | Type       | Test |
|------|----------------------------------------------------------------------------------------|------------|------|
| U-10 | A group restore creates N volumes on one store and joins them into one new group       | Positive   | —    |
| U-11 | A restore of an incomplete group backup is refused, missing members named              | Negative   | —    |
| U-12 | A member recovery failure unwinds the volumes already created; no half-group remains   | Regression | —    |
| U-13 | A restore that cannot place all members on one store fails rather than scattering them | Negative   | —    |

---

## 4. Operator envtest (the CRD extensions, design §7)

The harness boundary: envtest with a real API server; the control-plane client is
faked. These prove admission, the mirror grouping, and the lock.

| #    | Scenario                                                                                                   | Type     | Test                                                     |
|------|------------------------------------------------------------------------------------------------------------|----------|----------------------------------------------------------|
| E-1  | A `StorageBackupPolicy` with `consistencyGroupName` resolves its named group and attaches the policy to it | Positive | `TestPolicyWithConsistencyGroupNameAttachesTheGroup`     |
| E-2  | A policy that sets both `claimSelector` and `consistencyGroupName` is refused                              | Negative | —                                                        |
| E-3  | The mirror groups member `StorageBackup`s by the `group-backup-id` label                                   | Positive | `TestMirrorRecordsAGroupMemberBackup`                    |
| E-4  | A `StorageBackupOps{groupBackupID}` with no matching `StorageBackup` is refused at admission               | Negative | `TestGroupRestoreWithNoMembersIsRefused`                 |
| E-5  | Two group restores of one generation serialize on the per-member `activeOpsRef` lock                       | Positive | `TestSecondGroupRestoreWaitsForTheMemberLock`            |
| E-6  | A group restore cannot abort after it has created a volume (IllegalTransitionError)                        | Boundary | —                                                        |
| E-13 | A policy naming a group that does not exist yet waits rather than failing, and attaches once it appears    | Positive | `TestPolicyWaitsForAConsistencyGroupThatDoesNotExistYet` |

E-2's `claimSelector`/`consistencyGroupName` mutual exclusion is a CEL rule on the generated CRD, so the API server rejects it; an envtest for that CEL rule is a gap. A policy that names a group which does not exist is deliberately **not** refused (§7.4): a group is born from its first labeled member, so the policy waits in `Pending` (E-13) rather than being rejected at admission.

### Group-restore admission (design §7.3, §7.4, Appendix C)

The harness boundary: the webhook validator runs against a fake client, no API server. These prove the group-restore reference check at admission.

| #    | Scenario                                                                            | Type     | Test                                           |
|------|-------------------------------------------------------------------------------------|----------|------------------------------------------------|
| E-8  | A group restore whose group backup has a failed member is refused, the member named | Negative | `TestGroupRestoreWithAFailedMemberIsRefused`   |
| E-9  | A group restore of a complete, healthy group backup is admitted                     | Positive | `TestGroupRestoreWithHealthyMembersIsAdmitted` |
| E-10 | A group restore resolves only members of its own group backup, not another group's  | Boundary | `TestGroupRestoreIgnoresMembersOfOtherGroups`  |

### Group-restore drive (design §6, §7.3)

The harness boundary: the reconciler drives the whole Restore graph against a fake control plane and a fake API server, the same harness the single-volume restore uses. These prove the group path end to end.

| #    | Scenario                                                                                                             | Type     | Test                                                 |
|------|----------------------------------------------------------------------------------------------------------------------|----------|------------------------------------------------------|
| E-12 | A group restore walks `Validating → Restoring → AwaitingVolumes → Binding` and produces one labeled claim per member | Positive | `TestGroupRestoreRunsTheWholeGraphAndProducesClaims` |
| E-14 | A group restore emits GroupRestoreStarted and GroupRestored on the operation (§10)                                   | Positive | `TestGroupRestoreEmitsItsOwnEvents`                  |

The rule that exactly one of `backupRef` and `groupBackupID` is set, and the `groupBackupID` immutability, are CEL rules on the generated CRD, rejected by the API server. An envtest covering them is a gap (§7), as is E-6 (abort-after-volume) and the group-restore unwind.

### Conversion fidelity (design §7.2, Appendix B)

The harness boundary: the `api/v1alpha1` conversion unit tests, no API server. The hub-only `status.source.group` has no v1alpha1 field, so a round trip through the spoke must preserve it.

| #   | Scenario                                                                                                      | Type       | Test                                                          |
|-----|---------------------------------------------------------------------------------------------------------------|------------|---------------------------------------------------------------|
| E-7 | A group member's `status.source.group` survives a v1alpha2 → v1alpha1 → v1alpha2 round trip through the stash | Regression | `TestStorageBackupCarriesTheHubSourceGroupThroughThisVersion` |

---

## 5. Live cluster (design §11)

| #   | Scenario                                                                                                             | Type     | Test |
|-----|----------------------------------------------------------------------------------------------------------------------|----------|------|
| M-1 | Back up a group under load, restore on another cluster, verify every member restores to the same generation boundary | Positive | —    |
| M-2 | An incremental group backup of generation N+1 ships only each member's delta since generation N                      | Positive | —    |
| M-3 | A twenty-member group (`MAX_CONSISTENCY_GROUP_MEMBERS`) backs up and restores within its deadline                    | Boundary | —    |

### M-1: cross-cluster group restore to a generation boundary

**What to verify:** the restored group is crash-consistent at the generation cut,
not torn across members.

**Test concept:**
1. Create a consistency group of three members, run correlated I/O so a torn cut
   is detectable (a counter written round-robin across the members).
2. Take a group backup; record the generation.
3. On a second cluster with only the bucket, discover and restore the group backup.
4. Mount every restored member and verify the counters agree at one cut (no member
   ahead of or behind the others by more than the generation boundary allows).

---

## 6. Coverage Summary

| Axis            | Values                                      | Covered by                         |
|-----------------|---------------------------------------------|------------------------------------|
| Member count    | 1, 3, 20 (`MAX_CONSISTENCY_GROUP_MEMBERS`)  | U-6, M-1 (3), M-3 (20); 1 is a gap |
| Commit outcome  | all-complete, member-fails, retry           | U-2, U-3, U-4                      |
| Restore outcome | all-restored, member-fails, placement-fails | U-10, U-12, U-13                   |
| Homogeneity     | homogeneous, heterogeneous (refused)        | U-9                                |
| Portability     | same cluster, cross-cluster import          | I-4, M-1                           |
| Operator source | claim policy, group policy                  | E-1, E-2                           |

---

## 7. What Is Not Yet Covered

- **Every row above is `—`.** This plan names the scenarios the Draft design implies. The test functions are written when Phase 1 and Phase 2 land, red-first
  (`regression-test` skill), and the IDs fill in then.
- **Single-member group backup (member count 1).** A group of one is a degenerate
  but legal case, and it needs a positive row once the behavior is decided (does it
  collapse to a single-volume backup, or stay a group of one?). Tied to design
  §12 and the group-of-one question.
- **Heterogeneous members.** Refused in the first version (U-9). The positive path
  is deferred with design §12 Q1 and is not planned here.
- **Retention and scheduling (Phase 4).** Pruning whole generations and the group
  schedule are out of this plan until the design's Phase 4 settles (design §9).
- **Group-backup deadline (design §12 Q3).** The timeout policy is an open question,
  so M-3's boundary is provisional until it is fixed.
