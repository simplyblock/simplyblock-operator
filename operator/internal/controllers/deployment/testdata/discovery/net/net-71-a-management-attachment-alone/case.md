# NET-71

**Mutation.** A run naming the definition `storage-mgmt`, a macvlan on `eth0`, which every worker has

**Expected.** `networkAttachment.management` names it in every group, and no group names an interface: data flows through the management attachment

**Harness.** `CM`
