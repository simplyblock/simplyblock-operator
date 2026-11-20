# NUMA-15

**Mutation.** `cpu.numaNodes` empty while devices report nodes 0 and 1

**Expected.** Placement falls through to the id tie-break. `vcpuCount` floors at 4 with its note

**Harness.** `CM`
