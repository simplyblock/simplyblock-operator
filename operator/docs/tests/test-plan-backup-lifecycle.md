# Test Plan: Backup Status Lifecycle

Scope: what `StorageBackup.status` reports over a backup's whole life, including the
transitions that happen after the backup itself finishes, and what a `BackupRestore`
does with each of them.

This plan covers the status lifecycle only. It is deliberately narrower than the
backup feature: creation from a PVC, the internal snapshot, retention policy
attachment and evaluation, import and export, and cross-cluster restore have no
matrix here yet, and a full backup matrix is still owed.

| Prefix | Class         | Harness                                           |
|--------|---------------|---------------------------------------------------|
| `BL-`  | Operator unit | fake client, `httptest` control plane, no cluster |
| `BE-`  | End-to-end    | live cluster, a retention policy that merges      |

Types are `Positive`, `Negative`, `Boundary`, and `Regression`. The `Test` column
names the implementing function, or `—` when nothing covers the scenario yet.

---

## 1. Operator unit tests (`operator/internal/controller`)

Fake client and a mock control plane. The reconciler is driven directly, so a
backend status that a live cluster reaches only after a retention cycle can be
served on demand.

### Polling and phase mapping

| #     | Scenario                                                                                                                                 | Type       | Test                                                             |
|-------|------------------------------------------------------------------------------------------------------------------------------------------|------------|------------------------------------------------------------------|
| BL-01 | A backup the control plane reports as `completed` is polled again, so a merge that happens at an arbitrary later point is still observed | Regression | `TestStorageBackupKeepsPollingAfterCompletedSoALaterMergeIsSeen` |
| BL-02 | Backend `merging` is reported as phase `Merging`, and polling continues                                                                  | Positive   | `TestStorageBackupPropagatesMergePhases`                         |
| BL-03 | Backend `merged` is reported as phase `Merged`, not `Pending`, and is terminal                                                           | Regression | `TestStorageBackupPropagatesMergePhases`                         |
| BL-04 | Backend `failed` stays terminal and is not re-polled                                                                                     | Positive   | `TestStorageBackupPropagatesMergePhases`                         |
| BL-05 | A completed backup is re-polled on the slow merge-watch interval rather than the progress interval                                       | Boundary   | —                                                                |

### BackupRestore against a non-restorable backup

| #     | Scenario                                                                                                           | Type       | Test                                        |
|-------|--------------------------------------------------------------------------------------------------------------------|------------|---------------------------------------------|
| BL-06 | A restore whose `StorageBackup` is `Merged` fails terminally instead of waiting for a `Done` that can never arrive | Regression | `TestBackupRestoreFailsWhenBackupWasMerged` |
| BL-07 | A restore whose `StorageBackup` is `Failed` fails terminally and carries the backup's reason                       | Negative   | `TestBackupRestoreFailsWhenBackupIsFailed`  |
| BL-08 | A restore whose `StorageBackup` does not exist terminates rather than polling forever                              | Negative   | —                                           |

---

## 2. End-to-end (live cluster)

| #     | Scenario                                                                                                                                                                   | Type       | Test |
|-------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------|------|
| BE-01 | Under a retention policy that merges, the merged backup's CR reaches phase `Merged` and the successor stays `Done`                                                         | Regression | —    |
| BE-02 | Retention success is asserted from CR phases, not from a shrinking backup count: a merged backup is retained in the control plane's database, so the count does not shrink | Regression | —    |
| BE-03 | The successor's `status.prevBackupID` is re-read after a merge rewrites the chain link                                                                                     | Positive   | —    |
