// That a run never proposes a disk that is already a simplyblock volume.
//
// A volume this product exported and some node attached is an NVMe-oF namespace
// like any other, and the machine it is attached to presents it as a disk. What
// separates it from a disk the fleet owns is its subsystem NQN, which names the
// cluster and the logical volume it belongs to. Handing one back to a cluster as
// backend storage would give a volume's own bytes away as free space, and would
// do it to the product's own data.

package discovery

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// attached is a simplyblock volume as a worker that has connected it sees: a
// namespace on a fabric, carrying the NQN the product builds.
func attached(name, clusterID, volumeID string) nodeprobe.Device {
	device := blockDisk(name, tb)
	device.Transport = string(blockdev.TransportNVMeFabric)
	device.SubsystemNQN = nqn.Make(clusterID, volumeID)
	return device
}

func TestASimplyblockVolumeIsNeverProposed(t *testing.T) {
	const cluster = "c30a691a-1d2e-4f3a-9b8c-5d6e7f809a1b"
	volume := attached("nvme3n1", cluster, "792e184c-0a1b-2c3d-4e5f-60718293a4b5")

	ok, why := admit(SimplyblockVolumeRule{}, volume)
	if ok {
		t.Fatal("a run admitted a disk that is one of this product's own volumes")
	}
	if !strings.Contains(why, cluster) {
		t.Errorf("the reason %q does not name the cluster the volume belongs to", why)
	}
}

func TestADiskThatIsNotAVolumeIsUntouchedByTheRule(t *testing.T) {
	for _, device := range []nodeprobe.Device{
		disk("nvme0n1", "0000:5e:00.0", 0, tb),
		blockDisk("vdb", tb),
	} {
		if ok, why := admit(SimplyblockVolumeRule{}, device); !ok {
			t.Errorf("%s was refused as a simplyblock volume: %s", device.Name, why)
		}
	}

	// A fabric namespace some other product exported is refused for being on a
	// fabric, by the class rule, and not by this one: what this rule says is
	// that the disk is ours, and it is not.
	foreign := blockDisk("nvme4n1", tb)
	foreign.Transport = string(blockdev.TransportNVMeFabric)
	foreign.SubsystemNQN = "nqn.2019-08.org.ceph:rbd.pool.image"
	if ok, _ := admit(SimplyblockVolumeRule{}, foreign); !ok {
		t.Error("a namespace another product exported was called a simplyblock volume")
	}
}

func TestTheRuleIsInEveryRunsPipeline(t *testing.T) {
	// It has to hold for both classes and for a run with no filter at all: a
	// worker with volumes attached is the ordinary case on a fleet this product
	// already serves, and the rule that keeps them out cannot be one a filter
	// switches on.
	volume := attached("nvme3n1", "c30a691a", "792e184c")

	runs := []*simplyblockv1alpha2.DeviceFilter{
		nil,
		{EnableLogicalBlockDevices: ptr.To(true)},
	}

	for _, filter := range runs {
		var found bool
		for _, rule := range BasicDeviceRules(filter) {
			if _, isRule := rule.(SimplyblockVolumeRule); isRule {
				found = true
			}
		}
		if !found {
			t.Errorf("a %s run has no rule refusing this product's own volumes", ClassOf(filter))
		}
	}

	// And the whole pipeline refuses it, whichever class is scanned.
	for _, filter := range runs {
		report := report("worker-1")
		report.Devices = []nodeprobe.Device{volume}
		plan := Planner{}.Plan([]nodeprobe.Report{report}, filter)
		if len(plan.NodeSets) != 0 {
			t.Errorf("a %s run drafted a simplyblock volume", ClassOf(filter))
		}
	}
}

func TestTheRefusalIsReadableRatherThanFilteredAway(t *testing.T) {
	// A worker whose disks are all attached volumes is a worker somebody will
	// ask about, so the answer has to survive into the explanation rather than
	// being folded away as a device of the wrong class.
	report := report("worker-1")
	report.Devices = []nodeprobe.Device{
		attached("nvme3n1", "c30a691a", "792e184c"),
		attached("nvme4n1", "c30a691a", "8a3f0b12"),
	}

	plan := Planner{}.Plan([]nodeprobe.Report{report}, nil)

	explained := strings.Join(plan.Explain(), " ")
	if !strings.Contains(explained, "simplyblock") {
		t.Errorf("the explanation %q does not say the disks are already volumes", explained)
	}
	if !strings.Contains(explained, "2 devices") {
		t.Errorf("the explanation %q does not count the two disks together", explained)
	}
}
