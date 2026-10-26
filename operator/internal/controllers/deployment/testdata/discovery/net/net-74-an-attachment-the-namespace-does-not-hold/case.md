# NET-74

**Mutation.** A run naming `storage-nope` in a namespace holding `storage-data` and `storage-mgmt`

**Expected.** Inspecting fails the run before probing. At writing, every worker is refused by the network attachment rule, naming the missing and the held definitions

**Harness.** `CM`
