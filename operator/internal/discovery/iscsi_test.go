// That an iSCSI LUN reaches a draft only where somebody named it.
//
// A LUN is a disk on the other side of a network, and a fleet that wants one in
// a storage cluster has decided something a discovery run cannot decide for it:
// that the network between the worker and the target is one a data path should
// run over. So the run does not propose one, and taking it is an instruction
// rather than a default.

package discovery

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// attachedLUN is an iSCSI disk as the probe reports one.
func attachedLUN(name string, size uint64) nodeprobe.Device {
	device := blockDisk(name, 0, size)
	device.Transport = string(blockdev.TransportISCSI)
	return device
}

func TestAnISCSILUNIsNotProposedUnlessItIsNamed(t *testing.T) {
	device := attachedLUN("sdb", 2*tb)

	ok, why := admit(ISCSIRule{Class: ClassBlock}, device)
	if ok {
		t.Fatal("a run proposed an iSCSI LUN nobody asked for")
	}
	if !strings.Contains(why, "iSCSI") || !strings.Contains(why, "allow list") {
		t.Errorf("the reason %q does not say it is iSCSI and has to be named", why)
	}
}

func TestAnNamedISCSILUNIsAdmitted(t *testing.T) {
	device := attachedLUN("sdb", 2*tb)
	rule := ISCSIRule{Class: ClassBlock, Allow: []string{"/dev/sdb"}}

	if ok, why := admit(rule, device); !ok {
		t.Errorf("a named iSCSI LUN was refused: %s", why)
	}

	// Naming a different disk does not name this one.
	other := ISCSIRule{Class: ClassBlock, Allow: []string{"/dev/sdc"}}
	if ok, _ := admit(other, device); ok {
		t.Error("an allow list naming another disk admitted this LUN")
	}
}

func TestTheRuleLeavesEveryOtherBusAlone(t *testing.T) {
	for _, transport := range []blockdev.Transport{
		blockdev.TransportSATA, blockdev.TransportSAS,
		blockdev.TransportSCSI, blockdev.TransportVirtio,
	} {
		device := blockDisk("sda", 0, tb)
		device.Transport = string(transport)
		if ok, why := admit(ISCSIRule{Class: ClassBlock}, device); !ok {
			t.Errorf("a %s disk was refused by the iSCSI rule: %s", transport, why)
		}
	}
}

func TestAnISCSILUNIsRefusedByAWholeRunUnlessNamed(t *testing.T) {
	report := report("worker-1")
	report.Devices = []nodeprobe.Device{attachedLUN("sdb", 2*tb), blockDisk("vdb", 0, 2*tb)}

	block := &simplyblockv1alpha2.DeviceFilter{EnableLogicalBlockDevices: ptr.To(true)}
	plan := Planner{}.Plan([]nodeprobe.Report{report},
		&simplyblockv1alpha2.DeviceFilter{EnableLogicalBlockDevices: block.EnableLogicalBlockDevices})

	if len(plan.NodeSets) != 1 {
		t.Fatalf("built %+v", plan.NodeSets)
	}
	group := plan.NodeSets[0].Groups[0]
	if len(group.Devices.Block) != 1 || group.Devices.Block[0] != "/dev/vdb" {
		t.Errorf("the group names %v, want the virtio disk alone", group.Devices.Block)
	}

	// Naming it is what takes it, and the allow list then bounds the draft to
	// what it names, which is the allow list's own rule.
	named := &simplyblockv1alpha2.DeviceFilter{
		EnableLogicalBlockDevices: ptr.To(true),
		BlockAllowList:            []string{"/dev/sdb", "/dev/vdb"},
	}
	plan = Planner{}.Plan([]nodeprobe.Report{report}, named)
	if len(plan.NodeSets) != 1 {
		t.Fatalf("built %+v", plan.NodeSets)
	}
	if got := plan.NodeSets[0].Groups[0].Devices.Block; len(got) != 2 {
		t.Errorf("the group names %v, want both disks once the LUN is named", got)
	}
}
