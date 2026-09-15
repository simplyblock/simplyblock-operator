// Which interface a draft names as the management one.
//
// The draft named none, and a storage node added without one is refused by the
// control plane with "No management interface with IP found in provided
// interfaces" — after the node_add task has already started, so the failure
// arrives as a task that gave up rather than as a document that was wrong.

package discovery

import (
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

func iface(name string, edit func(*nodeprobe.Interface)) nodeprobe.Interface {
	out := nodeprobe.Interface{Name: name, State: "up", NUMANode: 0}
	if edit != nil {
		edit(&out)
	}
	return out
}

// The interface carrying the address the cluster already reaches the machine on
// wins, whatever else is present.
//
// It is the one answer that cannot be wrong: the operator addresses the worker
// by that address everywhere else, so naming the interface that holds it keeps
// the two halves talking about one network.
func TestTheInterfaceHoldingTheNodeAddressWins(t *testing.T) {
	report := report("worker-1")
	report.Interfaces = []nodeprobe.Interface{
		iface("eth1", func(i *nodeprobe.Interface) {
			i.Addresses = []string{"10.10.10.113"}
			i.SpeedMbps = 40000
		}),
		iface("eth0", func(i *nodeprobe.Interface) {
			i.Addresses = []string{"192.168.10.113"}
		}),
	}

	if got := ManagementInterface(report, "192.168.10.113"); got != "eth0" {
		t.Errorf("named %q, want the interface holding the node's address", got)
	}
}

// With no address to match, a physical interface holding an address is named,
// and the fastest such one wins.
func TestTheFastestAddressedPhysicalInterfaceIsNamed(t *testing.T) {
	report := report("worker-1")
	report.Interfaces = []nodeprobe.Interface{
		iface("eth0", func(i *nodeprobe.Interface) {
			i.Addresses = []string{"192.168.10.113"}
			i.SpeedMbps = 1000
		}),
		iface("eth1", func(i *nodeprobe.Interface) {
			i.Addresses = []string{"10.10.10.113"}
			i.SpeedMbps = 40000
		}),
	}

	if got := ManagementInterface(report, ""); got != "eth1" {
		t.Errorf("named %q, want the fastest addressed interface", got)
	}
}

// The interfaces a cluster leaves on every worker are never named, even when
// they hold an address and even when nothing else does.
//
// A bridge carries somebody else's traffic, a veth is one end of a pod's link,
// and loopback reaches nothing. Naming any of them produces a storage node the
// rest of the fleet cannot talk to.
func TestTheClustersOwnInterfacesAreNeverNamed(t *testing.T) {
	report := report("worker-1")
	report.Interfaces = []nodeprobe.Interface{
		iface("cni0", func(i *nodeprobe.Interface) {
			i.Addresses = []string{"10.42.2.1"}
			i.Virtual = true
			i.Bridge = true
		}),
		iface("flannel.1", func(i *nodeprobe.Interface) {
			i.Addresses = []string{"10.42.2.0"}
			i.Virtual = true
		}),
		iface("lo", func(i *nodeprobe.Interface) {
			i.Addresses = []string{"127.0.0.1"}
			i.Virtual = true
			i.Loopback = true
		}),
	}

	if got := ManagementInterface(report, ""); got != "" {
		t.Errorf("named %q, want nothing rather than a virtual interface", got)
	}
}

// A physical interface with no address is not a management interface: the
// control plane refuses exactly that, and naming one moves the refusal from the
// draft to a task that has already started.
func TestAnInterfaceWithNoAddressIsNotNamed(t *testing.T) {
	report := report("worker-1")
	report.Interfaces = []nodeprobe.Interface{
		iface("eth0", func(i *nodeprobe.Interface) { i.SpeedMbps = 40000 }),
	}

	if got := ManagementInterface(report, ""); got != "" {
		t.Errorf("named %q, want nothing rather than an interface with no address", got)
	}
}

// A link that is down holds its address and carries nothing, so it is passed
// over while any interface that is up remains.
func TestALinkThatIsDownIsPassedOver(t *testing.T) {
	report := report("worker-1")
	report.Interfaces = []nodeprobe.Interface{
		iface("eth0", func(i *nodeprobe.Interface) {
			i.Addresses = []string{"192.168.10.113"}
			i.SpeedMbps = 40000
			i.State = "down"
		}),
		iface("eth1", func(i *nodeprobe.Interface) {
			i.Addresses = []string{"10.10.10.113"}
			i.SpeedMbps = 1000
		}),
	}

	if got := ManagementInterface(report, ""); got != "eth1" {
		t.Errorf("named %q, want the interface that is up", got)
	}
}

// A link-local address is not one anything reaches the machine on.
func TestALinkLocalAddressDoesNotCount(t *testing.T) {
	report := report("worker-1")
	report.Interfaces = []nodeprobe.Interface{
		iface("eth0", func(i *nodeprobe.Interface) {
			i.Addresses = []string{"169.254.1.1", "fe80::1"}
		}),
	}

	if got := ManagementInterface(report, ""); got != "" {
		t.Errorf("named %q, want nothing rather than a link-local address", got)
	}
}

// Ties are broken by name so that two runs against one fleet write the same
// document, which is what makes a draft reviewable.
func TestTheChoiceIsStableAcrossRuns(t *testing.T) {
	report := report("worker-1")
	report.Interfaces = []nodeprobe.Interface{
		iface("eth1", func(i *nodeprobe.Interface) { i.Addresses = []string{"10.10.10.113"} }),
		iface("eth0", func(i *nodeprobe.Interface) { i.Addresses = []string{"192.168.10.113"} }),
	}

	first := ManagementInterface(report, "")
	report.Interfaces[0], report.Interfaces[1] = report.Interfaces[1], report.Interfaces[0]
	if second := ManagementInterface(report, ""); second != first {
		t.Errorf("the reading order changed the answer: %q then %q", first, second)
	}
	if first != "eth0" {
		t.Errorf("named %q, want the first by name", first)
	}
}
