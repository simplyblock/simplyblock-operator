# SIZE-16

**Mutation.** 256 GiB RAM, 200 GiB already in huge pages, 40 GiB available

**Expected.** **Contested.** `minHugePagesSize: 200G` and no arithmetic against the 40 GiB left. See §14, gap G-16

**Harness.** `CM`

**Gap.** G-16. This case records what the generator does today, so that the day it changes the diff is the finding.
