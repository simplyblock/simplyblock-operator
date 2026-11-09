# DEV-27

**Mutation.** An LVM logical volume beside a multipath map and a virtio disk, block run

**Expected.** The virtio disk alone, the map declined by the preference. The logical volume is refused by `whole disk` as what it is, in both modes: a slice of a volume group is not a device

**Harness.** `CM`
