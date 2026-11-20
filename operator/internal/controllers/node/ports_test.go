// What the status says a node listens on, and what happens to it when a reading
// arrives that does not say.
//
// A port is not a constant: the control plane assigns it when the node starts,
// so a restart can move one, and the status has to follow. The other half is the
// one that bites — a reading that carries no ports is not a node with no ports,
// and writing zeros for it takes away the addresses everything else dials.

package node

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// managementIP is the address the control plane reports for the node under test.
const managementIP = "192.168.10.112"

// listening is a status of a node the control plane has reported ports for.
func listening() *simplyblockv1alpha2.StorageNodeStatus {
	return &simplyblockv1alpha2.StorageNodeStatus{
		Ports: &simplyblockv1alpha2.StorageNodePorts{
			Management: managementIP,
			NvmeOf:     ptr.To(int32(4421)),
			Lvol:       ptr.To(int32(4426)),
			Rpc:        ptr.To(int32(4420)),
		},
	}
}

func TestAPortThatMovedOnARestartIsRecorded(t *testing.T) {
	status := listening()

	applyReading(status, NodeReading{
		ManagementIP: managementIP,
		NVMeOFPort:   4431,
		LvolPort:     4436,
		RPCPort:      4430,
	})

	if got := ptr.IntFromOrZero(status.Ports.Rpc); got != 4430 {
		t.Errorf("the RPC port is %d, want the one the node came back on", got)
	}
	if got := ptr.IntFromOrZero(status.Ports.Lvol); got != 4436 {
		t.Errorf("the lvol port is %d, want the one the node came back on", got)
	}
	if got := ptr.IntFromOrZero(status.Ports.NvmeOf); got != 4431 {
		t.Errorf("the NVMe-oF port is %d, want the one the node came back on", got)
	}
}

func TestAReadingCarryingNoPortsLeavesTheOnesAlreadyRecorded(t *testing.T) {
	// The completeness of the stream's payload is what the whole subscription
	// rests on, and this is what it costs to be wrong about it: the latency
	// probe dials status.ports.lvol and the SPDK proxy's endpoints are built
	// from the management address, so a zero written over a good port is a node
	// nothing can reach until the next reading that carries one.
	status := listening()

	applyReading(status, NodeReading{Status: nodeStatusOnline})

	if got := ptr.IntFromOrZero(status.Ports.Rpc); got != 4420 {
		t.Errorf("the RPC port is %d after a reading that carried none, want the recorded 4420", got)
	}
	if got := ptr.IntFromOrZero(status.Ports.Lvol); got != 4426 {
		t.Errorf("the lvol port is %d after a reading that carried none, want the recorded 4426", got)
	}
	if got := ptr.IntFromOrZero(status.Ports.NvmeOf); got != 4421 {
		t.Errorf("the NVMe-oF port is %d after a reading that carried none, want the recorded 4421", got)
	}
	if status.Ports.Management != managementIP {
		t.Errorf("the management address is %q after a reading that carried none", status.Ports.Management)
	}
}

func TestTheFirstReadingRecordsWhateverItCarries(t *testing.T) {
	// A node reported before it is listening has no ports to keep, so nothing
	// here invents one: an absent port and a zero one both read as unknown.
	status := &simplyblockv1alpha2.StorageNodeStatus{}

	applyReading(status, NodeReading{ManagementIP: managementIP, RPCPort: 4420})

	if status.Ports == nil || status.Ports.Management != managementIP {
		t.Fatalf("ports = %+v, want what the first reading carried", status.Ports)
	}
	if got := ptr.IntFromOrZero(status.Ports.Rpc); got != 4420 {
		t.Errorf("the RPC port is %d, want 4420", got)
	}
	if got := ptr.IntFromOrZero(status.Ports.Lvol); got != 0 {
		t.Errorf("the lvol port is %d before anything reported one", got)
	}
}
