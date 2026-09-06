//go:build linux

// The node this suite runs its plans on, and the targets the host-side driver
// published for the run.
//
// The plan shapes themselves are atlas-lib's, in volstack/plans, because a suite
// that composed its own layer lists would prove those lists work and say nothing
// about the ones the driver stages. What is left here is the two things only a
// run on a node can supply: which implementations fill the seams, and which
// namespaces exist.
//
// Every layer takes its side effects as an interface, which is what lets the
// unit tests run without a kernel. The value of running here is the opposite
// one: nothing is substituted. The prober opens the device with O_DIRECT and
// asks the kernel for its block size, the connector is nvme-cli, the resolver
// walks the node's own sysfs, and LVM is LVM. A test that swapped any of those
// for a stand-in would be exercising the stand-in, since those are the parts
// that only a real kernel can be wrong about.

package onnode

import (
	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack/plans"
)

// Target is the namespace the host-side driver published for this run, handed
// over rather than discovered: which nvmet subsystem exists is the driver's
// doing, and a suite that went looking for one could find a neighbor's.
type Target struct {
	NQN     string
	Address string
	Port    int
	NSID    uint32
}

// Connection is the target in the shape a plan takes it, which is the shape the
// control plane publishes: a namespace and the endpoints serving it, in priority
// order. Here the order is trivial because nvmet advertises one.
func (t Target) Connection() lvol.Connection {
	return lvol.Connection{
		NQN:  t.NQN,
		NSID: t.NSID,
		Endpoints: []lvol.Endpoint{{
			Transport: "tcp",
			Address:   t.Address,
			Port:      t.Port,
		}},
	}
}

// connections is the same for a plan whose bottom is several namespaces. The
// order is the driver's, and it is preserved: a stripe assembled over the same
// members in another order is a different device.
func connections(targets []Target) []lvol.Connection {
	all := make([]lvol.Connection, 0, len(targets))
	for _, t := range targets {
		all = append(all, t.Connection())
	}
	return all
}

// node is the suite's handle on the host: the plan builder, plus the two seams
// the assertions reach through directly. A case that has run a plan goes on to
// ask LVM what it made and the prober what is on a device, which is a question
// about the node rather than about a stack.
type node struct {
	*plans.Node

	manager *lvm.Manager
	content *blockdev.Prober
}

// newNode fills the plan builder's seams with the implementations that ship.
func newNode(hostNQN, hostID string) *node {
	cfg := nvme.SysfsConfig{}
	subs := nvme.NewSysfsSubsystemResolver(cfg)
	manager := lvm.NewManager()
	content := blockdev.NewProber()

	return &node{
		Node: plans.NewNode(plans.NodeConfig{
			HostNQN:    hostNQN,
			HostID:     hostID,
			Connector:  nvmeof.NewCLIConnector(subs),
			Devices:    nvme.NewSysfsDeviceResolver(cfg),
			Manager:    manager,
			Content:    content,
			Filesystem: shellFilesystem{},
		}),
		manager: manager,
		content: content,
	}
}
