# SIZE-21

**Mutation.** No hugetlbfs at all, `hugePages` absent from the report

**Expected.** The same unset output as a machine whose pools are full, so the two are not told apart. See §14, gap G-19

**Harness.** `CM`

**Gap.** G-19. This case records what the generator does today, so that the day it changes the diff is the finding.
