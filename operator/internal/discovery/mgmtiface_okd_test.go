// The management interface of a real OVN-Kubernetes worker.
//
// Every other case in this package is a shape reduced to the two or three
// interfaces it turns on. This one is the whole machine: fifteen interfaces as
// atlas-lib/inventory read them off worker-5 of the lab fleet on 2026-09-20,
// which is the fleet the choice was wrong on.

package discovery

import (
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// The machine's own vocabulary: the states the kernel reported, the kinds the
// probe read, and the bridge the node is reached on.
const (
	stateUnknown = "unknown"
	stateDown    = "down"
	kindPhysical = "physical"
	nodeBridge   = "br-ex"
)

// okdWorker is that machine's interface list. The names are the machine's own.
func okdWorker() nodeprobe.Report {
	iface := func(name string, edit func(*nodeprobe.Interface)) nodeprobe.Interface {
		out := nodeprobe.Interface{Name: name, State: "up", NUMANode: -1, Virtual: true, Kind: "virtual"}
		edit(&out)
		return out
	}
	pod := func(name string) nodeprobe.Interface {
		return iface(name, func(i *nodeprobe.Interface) {
			i.Peered = true
			i.SpeedMbps = 10000
		})
	}

	report := report("worker-5.ocp.simplyblock.ai")
	report.Interfaces = []nodeprobe.Interface{
		pod("3070a31ea98cce0"),
		pod("74a4fe9314cec06"),
		pod("b7960592644e3e1"),
		pod("ee1dd2e3fa266ff"),
		pod("feb8ae72b6d20d4"),
		// The node's own bridge: undeclared, standing alone, and holding the
		// address the cluster reaches the machine on.
		iface(nodeBridge, func(i *nodeprobe.Interface) {
			i.State = stateUnknown
			i.Addresses = []string{"10.0.0.15", "169.254.0.2"}
		}),
		iface("br-int", func(i *nodeprobe.Interface) { i.State = stateDown }),
		iface("ovs-system", func(i *nodeprobe.Interface) { i.State = stateDown }),
		iface("genev_sys_6081", func(i *nodeprobe.Interface) {
			i.State = stateUnknown
			i.Kind = "geneve"
		}),
		iface("ovn-k8s-mp0", func(i *nodeprobe.Interface) {
			i.State = stateUnknown
			i.Addresses = []string{"10.130.2.2"}
		}),
		iface("lo", func(i *nodeprobe.Interface) {
			i.State = stateUnknown
			i.Kind = "loopback"
			i.Loopback = true
			i.Addresses = []string{"127.0.0.1"}
		}),
		// The storage plane: the fastest physical NICs, and the ones the old
		// rule named.
		iface("enp2s0f0", func(i *nodeprobe.Interface) {
			i.Virtual, i.Kind, i.SpeedMbps = false, kindPhysical, 10000
			i.Addresses = []string{"192.168.10.15"}
		}),
		iface("enp2s0f1", func(i *nodeprobe.Interface) {
			i.Virtual, i.Kind, i.SpeedMbps = false, kindPhysical, 10000
			i.Addresses = []string{"192.168.20.15"}
		}),
		iface("enp8s0", func(i *nodeprobe.Interface) {
			i.Virtual, i.Kind, i.SpeedMbps = false, kindPhysical, 1000
		}),
		iface("enp8s0.4000", func(i *nodeprobe.Interface) {
			i.Kind, i.SpeedMbps, i.Peered = "vlan", 1000, true
			i.Lower = []string{"enp8s0"}
		}),
	}
	return report
}

// TestTheOKDWorkerNamesTheBridgeItsNodeAddressIsOn is the whole thread in one
// assertion.
//
// Regression: 2026-09-20-management-interface-chosen-off-the-cluster-network —
// the run named enp2s0f0, the fastest physical NIC, because br-ex reads as
// virtual and undeclared and was refused before its address was ever consulted.
// The control plane then recorded 192.168.10.15 as the node's mgmt_ip, and the
// operator matches a backend node by the worker's InternalIP: 10.0.0.15. The
// node came up online and healthy and no StorageNode could ever be matched to
// it.
func TestTheOKDWorkerNamesTheBridgeItsNodeAddressIsOn(t *testing.T) {
	if got := ManagementInterface(okdWorker(), "10.0.0.15"); got != nodeBridge {
		t.Errorf("named %q, want br-ex: it holds the address the operator matches by", got)
	}
}

// Without an address to match, the machine's own bridge is not a candidate and
// the storage NIC is the honest answer: nothing then says which network the
// cluster is on, which is why the node address is passed at all.
func TestTheOKDWorkerFallsBackToAPhysicalNIC(t *testing.T) {
	got := ManagementInterface(okdWorker(), "")
	if got == nodeBridge {
		t.Error("named br-ex with no address to match, which nothing in the reading justifies")
	}
	if got != "enp2s0f0" {
		t.Errorf("named %q, want the fastest physical NIC holding a reachable address", got)
	}
}

// The pod links the CNI leaves on the host are never named, whatever address
// they are asked about.
func TestTheOKDWorkerNeverNamesAPodLink(t *testing.T) {
	for _, address := range []string{"10.0.0.15", "10.130.2.2", ""} {
		got := ManagementInterface(okdWorker(), address)
		for _, link := range []string{"3070a31ea98cce0", "74a4fe9314cec06", "b7960592644e3e1"} {
			if got == link {
				t.Errorf("named the pod link %q for address %q", got, address)
			}
		}
	}
}
