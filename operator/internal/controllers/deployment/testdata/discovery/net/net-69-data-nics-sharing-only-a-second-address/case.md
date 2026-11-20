# NET-69

**Mutation.** Two workers whose data NICs hold 10.20.0.0/24 and 10.21.0.0/24 first and 10.30.0.0/24 second

**Expected.** No data interface on either: the control plane listens on the first IPv4 address, so only that one is compared

**Harness.** `CM`
