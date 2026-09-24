// Which class may claim a partition.
//
// SPDK binds an NVMe controller through vfio-pci and is handed the whole
// device, so a partition of one was never something a run could propose. A
// logical block device is reached through the kernel instead, where a partition
// is an ordinary block device and the backend takes one: the journal share
// exists for the case where the smallest device selected is a partition.
//
// So the rule is the class's rather than the fleet's, and relaxing it for one
// costs the other nothing.

package discovery

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
)

func TestAnNVMeRunStillRefusesAPartition(t *testing.T) {
	part := disk("nvme0n1p1", "0000:5e:00.0", 0, tb)
	part.Kind = string(blockdev.KindPartition)

	if ok, why := admit(WholeDiskRule{Class: ClassNVMe}, part); ok {
		t.Error("an NVMe run admitted a partition, which SPDK cannot be handed")
	} else if !strings.Contains(why, "Partition") {
		t.Errorf("the reason %q does not say what it is", why)
	}
}

func TestABlockRunAdmitsAPartition(t *testing.T) {
	part := disk("sdb1", "", 0, tb)
	part.Kind = string(blockdev.KindPartition)

	if ok, why := admit(WholeDiskRule{Class: ClassBlock}, part); !ok {
		t.Errorf("a block run refused a partition: %s", why)
	}
}

// Every other kind stays refused in both classes. The rule is about partitions
// and not about anything else that is not a disk: a loopback device or a
// device-mapper node is no more a candidate for the block class than for NVMe.
func TestNeitherClassAdmitsWhatIsNotADiskOrAPartition(t *testing.T) {
	for _, class := range []DeviceClass{ClassNVMe, ClassBlock} {
		loop := disk("loop0", "", 0, tb)
		loop.Kind = string(blockdev.KindLoop)

		if ok, _ := admit(WholeDiskRule{Class: class}, loop); ok {
			t.Errorf("the %s class admitted a loopback device", class)
		}
	}
}

// A whole disk is admitted by both, which is what every deployment before the
// block class existed was built out of.
func TestBothClassesAdmitAWholeDisk(t *testing.T) {
	for _, class := range []DeviceClass{ClassNVMe, ClassBlock} {
		if ok, why := admit(WholeDiskRule{Class: class}, disk("nvme0n1", "0000:5e:00.0", 0, tb)); !ok {
			t.Errorf("the %s class refused a whole disk: %s", class, why)
		}
	}
}
