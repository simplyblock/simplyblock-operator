# NET-57

**Mutation.** Two workers with identical disks, only one with a 25G NIC at MTU 9000

**Expected.** One group, no data interface: one worker of two is no majority for a data network. See §14, gap G-29

**Harness.** `CM`

**Gap.** G-29. This case records what the generator does today, so that the day it changes the diff is the finding.
