# DEV-20

**Mutation.** 4 equal virtio disks, block run, forceJournalDevice unset

**Expected.** Drafted with the journal split: enableJournalDevice unset, every disk carries data

**Harness.** `CM`

**Note.** The ordinary fleet: four disks of one size, which is how machines are bought. Nothing about them says which carries the journal, and taking one spends a disk the fleet did not set aside. The run leaves enableJournalDevice unset, and the node's configure carves the journal partition out of the smallest disk, so every disk keeps carrying data. The note names forceJournalDevice for a run that wants a disk dedicated.
