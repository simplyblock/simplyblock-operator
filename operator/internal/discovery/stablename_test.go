// Which name a draft gives a logical block device.
//
// A draft is written once and read back on every configure the deployment ever
// performs, the first of them possibly after a reboot. The kernel name it used
// to carry is a position in one boot's enumeration order: on the lab worker
// this was developed against, the disk the kernel calls sdb is the one the
// hypervisor calls drive-scsi0, and sda is drive-scsi2. Nothing about the first
// spelling predicts the second, and a document naming a disk in the first would
// select a different disk after the order changed, silently, because both names
// exist and both resolve.

package discovery

import (
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// namedDisk is a free block device with both spellings the probe reports.
func namedDisk(name, stable string) nodeprobe.Device {
	return nodeprobe.Device{
		Name:       name,
		Path:       "/dev/" + name,
		StablePath: stable,
		SizeBytes:  1600321314816,
		Kind:       string(blockdev.KindDisk),
		Transport:  string(blockdev.TransportSCSI),
		NUMANode:   0,
		Available:  true,
		Content:    "Blank",
	}
}

func TestABlockDraftNamesADiskByItsPersistentName(t *testing.T) {
	const stable = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2"

	got := ClassBlock.Address(namedDisk("sda", stable))
	if got != stable {
		t.Errorf("the draft names the disk %q, want %q", got, stable)
	}
}

// A device udev published no link for is named by its kernel path, which is all
// there is. It is the weaker name and the run says so, but refusing the device
// would hold up a deployment on a host whose disks are all perfectly usable.
func TestADiskWithNoPersistentNameIsNamedByItsPath(t *testing.T) {
	got := ClassBlock.Address(namedDisk("sda", ""))
	if got != "/dev/sda" {
		t.Errorf("the draft names the disk %q, want /dev/sda", got)
	}
}

// The NVMe class is unaffected. Its devices are named by the slot the
// controller sits in, which SPDK binds by address and which no udev link
// substitutes for.
func TestAnNVMeDraftStillNamesADiskByItsSlot(t *testing.T) {
	device := disk("nvme0n1", "0000:5e:00.0", 0, 3200631791616)
	device.StablePath = "/dev/disk/by-id/nvme-eui.34333930547014240025384300000001"

	if got := ClassNVMe.Address(device); got != "0000:5e:00.0" {
		t.Errorf("the draft names the disk %q, want its slot 0000:5e:00.0", got)
	}
}

// Two workers whose disks carry the same persistent names are one group, and
// two whose disks differ are two.
//
// The grouping key is the device list, so this is what the change costs and
// what it buys. A fleet of cloned virtual machines names its disks identically
// — drive-scsi0 is drive-scsi0 on every one of them — and groups as it did
// before. A fleet of real machines names each disk by something only that disk
// carries, so each worker describes its own group, which is the honest document:
// one device list shared between workers was only ever correct because kernel
// names repeat.
func TestWorkersGroupOnThePersistentNamesTheyShare(t *testing.T) {
	cloned := []Worker{
		{Name: "worker-1", Class: ClassBlock, Devices: []nodeprobe.Device{
			namedDisk("sda", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2"),
			namedDisk("sdc", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1"),
		}},
		{Name: "worker-2", Class: ClassBlock, Devices: []nodeprobe.Device{
			namedDisk("sdb", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2"),
			namedDisk("sdd", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1"),
		}},
	}
	if groups := (GroupByHardware{}).Group(cloned); len(groups) != 1 {
		t.Errorf("two clones made %d groups, want 1: they hand over the same "+
			"drive ids however the kernel numbered them this boot", len(groups))
	}

	distinct := []Worker{
		{Name: "worker-1", Class: ClassBlock, Devices: []nodeprobe.Device{
			namedDisk("sda", "/dev/disk/by-id/wwn-0x5000c500a1b2c3d4"),
		}},
		{Name: "worker-2", Class: ClassBlock, Devices: []nodeprobe.Device{
			namedDisk("sda", "/dev/disk/by-id/wwn-0x5000c500e5f6a7b8"),
		}},
	}
	if groups := (GroupByHardware{}).Group(distinct); len(groups) != 2 {
		t.Errorf("two machines with different disks made %d groups, want 2", len(groups))
	}
}
