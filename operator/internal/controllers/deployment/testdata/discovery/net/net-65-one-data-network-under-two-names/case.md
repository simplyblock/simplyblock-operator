# NET-65

**Mutation.** Two workers on 10.20.0.0/24, one calling its data NIC `ens5f0` and the other `ens6f0`

**Expected.** Both kept, in 2 groups: the network agrees and the names do not

**Harness.** `CM`
