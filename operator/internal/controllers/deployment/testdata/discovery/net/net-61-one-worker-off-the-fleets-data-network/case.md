# NET-61

**Mutation.** Four workers with a 25G data NIC, three on 10.20.0.0/24 and the fourth on 10.21.0.0/24

**Expected.** The fourth refused: the data networks are the ones most workers share

**Harness.** `CM`
