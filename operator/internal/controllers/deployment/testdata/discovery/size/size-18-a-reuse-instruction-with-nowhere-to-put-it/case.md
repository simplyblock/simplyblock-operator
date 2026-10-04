# SIZE-18

**Mutation.** A run meant to take 128 of the host's 256 pre-allocated pages

**Expected.** **Contested.** No field expresses it. `spec.discover` carries no huge-page input, and neither `minHugePagesSize` nor `spdkSystemMemory` is written by a run. See §14, gap G-17

**Harness.** `CM`

**Gap.** G-17. This case records what the generator does today, so that the day it changes the diff is the finding.
