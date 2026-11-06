# NET-76

**Mutation.** `spec.discover.network.spdkOnHostNetwork: true` on a fleet whose workers report no interfaces

**Expected.** The draft's network is `spdkOnHostNetwork`, not the `podNetwork` NET-05 proposes for the same fleet, and the note says the run stated it

**Harness.** `CM`
