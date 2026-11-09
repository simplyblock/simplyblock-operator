# DEV-17

**Mutation.** An iSCSI LUN beside virtio disks, block run

**Expected.** Only the virtio disks. The worker has local disks, which a run takes first; the LUN is declined by `local disks first`, naming the switch

**Harness.** `CM`
