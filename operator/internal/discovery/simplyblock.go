// The rule that keeps a fleet's own volumes out of its own draft.
//
// A simplyblock volume attached to a worker is an NVMe-oF namespace, and the
// kernel presents it as a disk like any other. Nothing about its shape says
// whose it is: a fabric namespace is a volume somebody exported, and a fabric
// namespace whose subsystem NQN names a simplyblock cluster and a logical
// volume is one this product exported. Only the second is a disk a draft would
// be giving back to the cluster it came from, and only the NQN tells them apart.
//
// The class rule already refuses every fabric namespace, so this rule changes
// no draft today. What it changes is the answer: a reviewer asking why a
// machine full of disks proposed none is told that the disks are the fleet's
// own volumes rather than that they are on a bus the run does not scan. It is
// also the check that survives the class rule being relaxed, which is the one
// way a cluster could be told to take its own bytes as free space.

package discovery

import (
	"fmt"

	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// SimplyblockVolumeRule refuses a device that is one of this product's own
// logical volumes.
type SimplyblockVolumeRule struct{}

func (SimplyblockVolumeRule) Name() string { return "simplyblock volume" }

// Admit refuses a device whose subsystem NQN parses as a simplyblock
// logical-volume subsystem.
//
// Parsing rather than matching a prefix is what makes the refusal specific. The
// NQN carries the cluster and the volume it belongs to, so the reason can name
// the cluster, and a subsystem of this product that is not a logical volume
// does not read as one.
func (SimplyblockVolumeRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	if device.SubsystemNQN == "" {
		return true, ""
	}
	subsystem, isVolume := nqn.Parse(device.SubsystemNQN)
	if !isVolume {
		return true, ""
	}
	return false, fmt.Sprintf(
		"it is a simplyblock logical volume of cluster %s, which this fleet already serves",
		subsystem.ClusterID)
}
