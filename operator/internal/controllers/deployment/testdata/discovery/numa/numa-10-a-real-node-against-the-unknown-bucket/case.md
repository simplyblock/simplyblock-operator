# NUMA-10

**Mutation.** 2 disks on node 0 and 2 on `-1`, node 0 carrying cores

**Expected.** Node 0 chosen on cores. The unknown bucket is never preferred

**Harness.** `CM`
