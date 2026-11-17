# DEV-22

**Mutation.** 2 NVMe disks, members of an MD RAID array nothing uses

**Expected.** Both drafted; the draft notes the deployment stops `md0` and erases its superblocks

**Harness.** `CM`

**Note.** The array is assembled and nothing mounts it, holds it, or formatted it, on any environment. Both members are drafted, and the draft says the deployment stops md0 and erases the members' RAID superblocks.
