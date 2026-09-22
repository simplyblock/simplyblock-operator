# FAIL-10

**Mutation.** A worker whose only disks are partitions, `enablePartitionedDevices` unset

**Expected.** Refused by `whole disk`, pre-filtered, so `Explain` gives the worker line alone

**Harness.** `CM`
