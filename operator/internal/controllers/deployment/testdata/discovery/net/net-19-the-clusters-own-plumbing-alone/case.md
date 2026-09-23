# NET-19

**Mutation.** `cni0` and `docker0` both addressed, one physical NIC with no address

**Expected.** `mgmtInterface` empty: a bridge is refused and an unaddressed NIC is not a candidate

**Harness.** `CM`
