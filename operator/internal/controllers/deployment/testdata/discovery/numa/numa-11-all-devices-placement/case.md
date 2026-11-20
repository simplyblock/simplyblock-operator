# NUMA-11

**Mutation.** The same worker with `Placement: AllDevices`

**Expected.** Every disk used. No single chosen node, so `vcpuCount` floors and huge pages go unset

**Harness.** `GO`

**Note.** Driven in the discovery package with Planner{Placement: AllDevices{}} over the NUMA-02 fleet, because the controller always builds the default Planner.
