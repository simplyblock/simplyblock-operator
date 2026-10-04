# NET-06

**Mutation.** Two workers with identical disks calling their NIC `eth0` and `ens5f0`

**Expected.** 2 groups, which the draft validation then reports as unbuildable. See §14, gap G-25

**Harness.** `CM`

**Gap.** G-25. This case records what the generator does today, so that the day it changes the diff is the finding.
