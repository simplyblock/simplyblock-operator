# NET-76

**Mutation.** Three workers the cluster reaches on 10.248.192.0/23, 10.248.194.0/23, and 10.248.196.0/23

**Expected.** All three kept: each is managed on its node address, so the subnets are routed, as one for each availability zone is

**Harness.** `CM`
