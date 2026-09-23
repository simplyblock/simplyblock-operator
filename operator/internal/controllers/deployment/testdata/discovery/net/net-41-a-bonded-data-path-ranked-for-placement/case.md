# NET-41

**Mutation.** A worker whose only fast NIC is a 2x25G bond, ranked for placement

**Expected.** **Contested.** The memory node's fastest NIC reads as 0 Mbps: the placement reads the raw speed and the raw memory node, neither of which a bond has. See §14, gap G-27

**Harness.** `CM`

**Gap.** G-27. This case records what the generator does today, so that the day it changes the diff is the finding.
