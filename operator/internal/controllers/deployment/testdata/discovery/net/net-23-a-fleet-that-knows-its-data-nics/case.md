# NET-23

**Mutation.** A run told that `ens5f1` and `ens5f2` are the data NICs

**Expected.** **Contested.** `dataInterfaces` is never written, so the draft leaves the data plane unnamed. See §14, gap G-24

**Harness.** `CM`

**Gap.** G-24. This case records what the generator does today, so that the day it changes the diff is the finding.
