# TMPL-06

**Mutation.** Any run on a two-socket fleet

**Expected.** `socketsToUse` and `nodesPerSocket` unset, so one node per worker on socket 0. See §14, gap G-12

**Harness.** `CM`

**Gap.** G-12. This case records what the generator does today, so that the day it changes the diff is the finding.
