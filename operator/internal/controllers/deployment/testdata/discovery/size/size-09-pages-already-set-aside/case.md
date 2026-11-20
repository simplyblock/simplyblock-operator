# SIZE-09

**Mutation.** 512 × 1 GiB pages pre-allocated, 256 free per node

**Expected.** **Contested.** `minHugePagesSize: 256G`: another workload's reservation becomes simplyblock's own floor. See §14, gap G-14

**Harness.** `CM`

**Gap.** G-14. This case records what the generator does today, so that the day it changes the diff is the finding.
