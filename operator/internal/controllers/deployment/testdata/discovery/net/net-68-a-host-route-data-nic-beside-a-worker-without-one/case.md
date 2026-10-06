# NET-68

**Mutation.** Two workers, one with a data NIC holding a /32 and the other with no data NIC

**Expected.** No data interface on either: data runs on management everywhere unless every worker names a data interface

**Harness.** `CM`
