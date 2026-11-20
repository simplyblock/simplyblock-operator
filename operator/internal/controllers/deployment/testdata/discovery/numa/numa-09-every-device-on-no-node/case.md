# NUMA-09

**Mutation.** Every device reports `numaNode: -1`

**Expected.** The unknown bucket is used. `vcpuCount` floors at 4, `minHugePagesSize` unset, both noted

**Harness.** `CM`
