# HOST-02

**Mutation.** The three block workers of e2e run 35975993533, on a block run

**Expected.** One group per worker, `devices.block` holding `sdb` and `sdc`. The partitioned boot disk refused, and the extra worker with it

**Harness.** `CM`

**Note.** The three storage workers of e2e run 35975993533 and the extra worker beside them. Each storage worker has two unpartitioned virtio disks beside a partitioned boot disk and no NVMe at all, which is the fleet the block class exists for.
