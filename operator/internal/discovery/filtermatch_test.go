// Which spelling of a device an allow or deny list is matched against.
//
// Every spelling the device answers to, and not just the one the draft writes.
// The draft names a block device by its persistent /dev/disk path, and a filter
// somebody wrote before that — or wrote by reading `lsblk` — names the kernel
// path. Matching only the drafted name breaks both lists, and breaks them in
// opposite directions: an allow list stops admitting the disk it was written
// for, and a deny list stops denying it.
//
// The second is the one that matters. A blockDenyList naming the boot disk is
// the single entry that keeps it out of every group of every draft, and a deny
// list that silently stops denying puts a mounted root disk into a document
// whose whole point is that it lists only free ones.

package discovery

import (
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// namedBlockDisk is a block device that answers to both spellings, which is
// every block device on a host whose udev published a link.
func namedBlockDisk(stable string) nodeprobe.Device {
	const name = "sdb"
	return nodeprobe.Device{
		Name:       name,
		Path:       "/dev/" + name,
		StablePath: stable,
		SizeBytes:  2 << 40,
		Kind:       string(blockdev.KindDisk),
		Transport:  string(blockdev.TransportSCSI),
		Available:  true,
	}
}

const bootDiskByID = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi0"

// A deny list written in kernel paths still denies. This is the regression the
// persistent name introduced: the list is matched against what the draft names,
// the draft now names the by-id path, and the entry stops matching.
func TestADenyListWrittenInKernelPathsStillDenies(t *testing.T) {
	rule := AllowDenyRule{Class: ClassBlock, Deny: []string{"/dev/sdb"}}

	admitted, reason := rule.Admit(nodeprobe.Report{}, namedBlockDisk(bootDiskByID))
	if admitted {
		t.Errorf("the deny list did not deny /dev/sdb, so the disk it was written to "+
			"keep out reaches the draft; the rule said %q", reason)
	}
}

// And a deny list written in persistent names denies too, which is what a list
// copied out of a draft looks like.
func TestADenyListWrittenInPersistentNamesStillDenies(t *testing.T) {
	rule := AllowDenyRule{Class: ClassBlock, Deny: []string{bootDiskByID}}

	if admitted, _ := rule.Admit(nodeprobe.Report{}, namedBlockDisk(bootDiskByID)); admitted {
		t.Error("the deny list did not deny the device it names by its persistent path")
	}
}

// An allow list written in kernel paths still admits. The failure here is the
// quieter one: the list excludes every disk it was written to include, and the
// run reports a fleet with no storage.
func TestAnAllowListWrittenInKernelPathsStillAdmits(t *testing.T) {
	rule := AllowDenyRule{Class: ClassBlock, Allow: []string{"/dev/sdb"}}

	admitted, reason := rule.Admit(nodeprobe.Report{}, namedBlockDisk(bootDiskByID))
	if !admitted {
		t.Errorf("the allow list did not admit /dev/sdb: %s", reason)
	}
}

// A device the list names by neither spelling is still refused, or the fix
// would be an allow list that admits everything.
func TestAListNamingAnotherDiskStillRefuses(t *testing.T) {
	rule := AllowDenyRule{Class: ClassBlock, Allow: []string{"/dev/sdc"}}

	if admitted, _ := rule.Admit(nodeprobe.Report{}, namedBlockDisk(bootDiskByID)); admitted {
		t.Error("an allow list naming another disk admitted this one")
	}
}

// The NVMe class is unchanged. Its devices are named by the slot, a filter
// names the slot, and neither has a second spelling.
func TestAnNVMeAllowListStillMatchesTheSlot(t *testing.T) {
	device := disk("nvme0n1", "0000:5e:00.0", 0, 3<<40)
	device.StablePath = "/dev/disk/by-id/nvme-eui.34333930547014240025384300000001"

	rule := AllowDenyRule{Class: ClassNVMe, Allow: []string{"0000:5e:00.0"}}
	if admitted, reason := rule.Admit(nodeprobe.Report{}, device); !admitted {
		t.Errorf("an NVMe allow list stopped matching the slot: %s", reason)
	}
}

// The iSCSI rule reads the same allow list, and a LUN named by its kernel path
// has to be takeable the way it always was: it is the one rule whose default is
// to refuse, so a list that stopped matching would make a named LUN unusable.
func TestTheISCSIRuleMatchesEitherSpelling(t *testing.T) {
	lun := namedBlockDisk("/dev/disk/by-id/scsi-360014051234567890abcdef")
	lun.Transport = string(blockdev.TransportISCSI)

	for _, allow := range [][]string{{"/dev/sdb"}, {lun.StablePath}} {
		rule := ISCSIRule{Class: ClassBlock, Allow: allow}
		if admitted, reason := rule.Admit(nodeprobe.Report{}, lun); !admitted {
			t.Errorf("the allow list %v did not take the LUN it names: %s", allow, reason)
		}
	}
}
