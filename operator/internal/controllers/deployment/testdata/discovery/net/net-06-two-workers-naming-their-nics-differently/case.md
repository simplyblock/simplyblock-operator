# NET-06

**Mutation.** Two workers with identical disks calling their NIC `eth0` and `ens5f0`

**Expected.** 2 node sets, one per interface name, each a single-node member: workers that name their management interface differently cannot share a member (design-superclusters.md §5.5). Gap G-25, fixed

**Harness.** `CM`
