# SIZE-19

**Mutation.** A pool of 256 pages, all promised to a mapping: `resv` 256, `free` 0

**Expected.** Indistinguishable from an untouched pool of 256, because the report drops `resv_hugepages`. See §14, gap G-18

**Harness.** `CM`

**Gap.** G-18. This case records what the generator does today, so that the day it changes the diff is the finding.
