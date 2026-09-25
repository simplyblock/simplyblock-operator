# HOST-03

**Mutation.** A QEMU worker with two NVMe disks and a DVD-ROM, on a block run

**Expected.** One group, `devices.block` holding `/dev/nvme0n1` and `/dev/nvme1n1`. The optical drive refused as removable, the boot disk as partitioned

**Harness.** `CM`

**Note.** One worker off a QEMU host rather than a cloud one, and the family's smallest fleet. It carries two devices no cloud image presents, and each is why it is here. Its two Samsung 1.92 TB NVMe disks are the case for the block class taking local NVMe: the classes are not disjoint sets of hardware, and a machine whose only real storage is NVMe has to be deployable as logical block devices. Its QEMU DVD-ROM is the case for refusing a removable device: with install media in it the drive reports a whole disk of 924 MB on the SATA bus and reads as blank, so every other ground admits it. Beside them are a partitioned virtio boot disk and sixteen nbd devices. Memory is stated rather than captured; see statedMemory.
