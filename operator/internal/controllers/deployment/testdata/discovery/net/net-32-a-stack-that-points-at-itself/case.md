# NET-32

**Mutation.** A bond whose `lower` names a second bond whose `lower` names the first

**Expected.** The bond is named and the resolution terminates

**Harness.** `GO`

**Note.** Driven in the discovery package, because no probe can write a report whose lower_* links form a cycle and the case is about the resolution terminating anyway.
