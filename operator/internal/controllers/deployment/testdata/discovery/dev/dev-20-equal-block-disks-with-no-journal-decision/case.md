# DEV-20

**Mutation.** 4 equal virtio disks, block run, forceJournalDevice unset

**Expected.** Refused: nothing says which disk carries the journal, and the class has no partitioned one

**Harness.** `CM`

**Note.** The ordinary fleet: four disks of one size, which is how machines are bought. Nothing about them says which carries the journal, and taking one spends a disk the fleet did not set aside. An NVMe run in this shape leaves enableJournalDevice unset and the backend carves a journal partition out of every device; a block cluster has no such layout, so a document written here would create the cluster, format its drives, and then fail every node_add. The run refuses instead, and names the field that decides it.
