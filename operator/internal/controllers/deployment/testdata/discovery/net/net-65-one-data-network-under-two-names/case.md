# NET-65

**Mutation.** Two workers on 10.20.0.0/24, one calling its data NIC `ens5f0` and the other `ens6f0`

**Expected.** Both kept, in 2 node sets: the network agrees and the names do not, and a member names one set of data interfaces (design-superclusters.md §5.5)

**Harness.** `CM`
