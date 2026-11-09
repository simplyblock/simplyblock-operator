# DEV-26

**Mutation.** A RAID-1 array over two NVMe namespaces, NVMe run

**Expected.** Nothing. The array has no controller to bind and is refused by `whole disk`; its members by `available` as held. No draft

**Harness.** `CM`
