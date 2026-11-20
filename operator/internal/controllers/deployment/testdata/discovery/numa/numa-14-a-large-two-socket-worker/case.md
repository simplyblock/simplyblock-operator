# NUMA-14

**Mutation.** A large worker: 2 nodes × 49 cores, 5 disks each, huge pages on both

**Expected.** 5 disks in the draft, `vcpuCount` 49, `minHugePagesSize` from node 0's free pages alone

**Harness.** `CM`
