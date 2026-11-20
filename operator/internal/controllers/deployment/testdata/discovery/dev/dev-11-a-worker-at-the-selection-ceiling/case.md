# DEV-11

**Mutation.** 128 NVMe disks on one worker

**Expected.** One group at the selection's `MaxItems`. The document still applies

**Harness.** `CM`

**Note.** 128 is the MaxItems of a group's device selection, so this is the largest worker the schema can describe.
