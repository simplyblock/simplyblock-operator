# NET-51

**Mutation.** A 40G Mellanox NIC at MTU 9000 holding no address, beside an addressed 1G NIC

**Expected.** No data interface: the control plane listens on a data NIC's IPv4 address and this one has none

**Harness.** `CM`
