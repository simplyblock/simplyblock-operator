# DEV-22

**Mutation.** A RAID-1 array over two SATA disks and nothing else, block run, journal undecided

**Expected.** The array alone. It is the device its administrator built; its members are declined by `available` as held by `md0`, and the journal is carved from the array as from a single disk

**Harness.** `CM`
