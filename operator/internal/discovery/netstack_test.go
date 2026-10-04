// The management-interface rule over a stacked host, and the stack resolution it
// rests on.
//
// Every case here is a host this product has been deployed onto and the earlier
// rule named no interface on: a bonded management network, a tagged one, a
// hypervisor host whose address is on a bridge. All three are virtual devices,
// which is why the kind reading had to exist before the rule could tell them
// from the veth pairs beside them.

package discovery

import (
	"slices"
	"testing"

	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// stacked is the interface list of a bonded host with a tagged management
// network: two NICs in a bond, a VLAN over the bond, a bridge for guests over a
// third NIC, the cluster's own overlay, and a veth to a pod.
//
// Which interface holds the management address is the caller's to set, because
// that is the only thing that differs between the cases below.
func stacked(addressed map[string][]string) []nodeprobe.Interface {
	kinds := []struct {
		name  string
		kind  inventory.LinkKind
		speed int
		lower []string
		upper []string
	}{
		{name: bond0, kind: inventory.LinkBond, lower: []string{eth0, eth1}, upper: []string{vlan100}},
		{name: vlan100, kind: inventory.LinkVLAN, lower: []string{bond0}},
		{name: "br0", kind: inventory.LinkBridge, lower: []string{"eth2"}},
		{name: eth0, kind: inventory.LinkPhysical, speed: 25000, upper: []string{bond0}},
		{name: eth1, kind: inventory.LinkPhysical, speed: 25000, upper: []string{bond0}},
		{name: "eth2", kind: inventory.LinkPhysical, speed: 10000, upper: []string{"br0"}},
		{name: "flannel.1", kind: inventory.LinkVXLAN},
		{name: "lo", kind: inventory.LinkLoopback},
		{name: "veth7a1c", kind: inventory.LinkVirtual},
	}

	out := make([]nodeprobe.Interface, 0, len(kinds))
	for _, entry := range kinds {
		out = append(out, nodeprobe.Interface{
			Name:      entry.name,
			Kind:      string(entry.kind),
			SpeedMbps: entry.speed,
			State:     "up",
			Lower:     entry.lower,
			Upper:     entry.upper,
			Virtual:   entry.kind != inventory.LinkPhysical,
			Bridge:    entry.kind == inventory.LinkBridge,
			Loopback:  entry.kind == inventory.LinkLoopback,
			NUMANode:  0,
			Addresses: addressed[entry.name],
		})
	}
	return out
}

// stackedReport is that host as a report, with the management address where the
// case wants it.
// peeredReport is stackedReport with the named interfaces marked as naming
// another device as their link, which is what iflink reports for a veth's peer
// and for a VLAN's parent alike.
func peeredReport(addressed map[string][]string, peered ...string) nodeprobe.Report {
	out := stackedReport(addressed)
	for i := range out.Interfaces {
		if slices.Contains(peered, out.Interfaces[i].Name) {
			out.Interfaces[i].Peered = true
		}
	}
	return out
}

func stackedReport(addressed map[string][]string) nodeprobe.Report {
	out := report("worker-1")
	out.Interfaces = stacked(addressed)
	return out
}

func TestABondHoldingTheNodeAddressIsNamed(t *testing.T) {
	// The bond is what an address is bound to on a bonded host. Its members hold
	// none, so the earlier rule found no candidate at all and named nothing.
	r := stackedReport(map[string][]string{bond0: {"10.10.10.113"}})

	if got := ManagementInterface(r, "10.10.10.113"); got != bond0 {
		t.Errorf("named %q, want the bond holding the node's address", got)
	}
}

func TestATaggedVLANHoldingTheNodeAddressIsNamed(t *testing.T) {
	r := stackedReport(map[string][]string{vlan100: {"10.10.10.113"}})

	if got := ManagementInterface(r, "10.10.10.113"); got != vlan100 {
		t.Errorf("named %q, want the VLAN holding the node's address", got)
	}
}

func TestAHostBridgeHoldingTheNodeAddressIsNamed(t *testing.T) {
	// A hypervisor host keeps its own address on the bridge its guests are on,
	// and that address is the one the cluster reaches the machine by.
	r := stackedReport(map[string][]string{"br0": {"192.168.1.10"}})

	if got := ManagementInterface(r, "192.168.1.10"); got != "br0" {
		t.Errorf("named %q, want the bridge holding the node's address", got)
	}
}

func TestABridgeHoldingSomebodyElsesAddressIsNotNamed(t *testing.T) {
	// The same kind of device put to the opposite purpose: a CNI bridge holds
	// the pod network and nothing outside the node reaches the machine on it.
	r := stackedReport(map[string][]string{"br0": {"10.42.2.1"}})

	if got := ManagementInterface(r, ""); got != "" {
		t.Errorf("named %q, want nothing: the bridge carries somebody else's network", got)
	}
}

func TestAnOverlayIsNamedOnlyWhenTheClusterItselfUsesIt(t *testing.T) {
	pod := stackedReport(map[string][]string{"flannel.1": {"10.42.2.0"}})
	if got := ManagementInterface(pod, ""); got != "" {
		t.Errorf("named %q, want nothing: the overlay is the pod network", got)
	}

	// A cluster that genuinely addresses its nodes over an overlay says so by
	// the node address being on it, and then it is the management interface.
	own := stackedReport(map[string][]string{"flannel.1": {"10.42.2.0"}})
	if got := ManagementInterface(own, "10.42.2.0"); got != "flannel.1" {
		t.Errorf("named %q, want the overlay the node address is on", got)
	}
}

func TestAnUnidentifiedVirtualDeviceIsNeverNamed(t *testing.T) {
	// A veth reports no kind of its own and names its peer as its link, and
	// admitting a device nothing identified would admit every pod link on the
	// machine. What it holds is a pod's address, and the node is reached on a
	// different one.
	r := peeredReport(map[string][]string{"veth7a1c": {"10.42.2.7"}}, "veth7a1c")

	if got := ManagementInterface(r, "10.0.0.15"); got != "" {
		t.Errorf("named %q, want nothing: nothing identified the device", got)
	}
	if got := ManagementInterface(r, ""); got != "" {
		t.Errorf("named %q with no address to match, want nothing", got)
	}
	if got := ManagementInterface(stackedReport(map[string][]string{"lo": {"127.0.0.1"}}), ""); got != "" {
		t.Errorf("named %q, want nothing for loopback", got)
	}
}

// The exception, and the reason the rule above is about what a device holds
// rather than about what it is.
//
// An interface holding the address the cluster reaches the machine on is the
// management interface whatever the probe called it, which is the same answer
// TestABondHoldingTheNodeAddressIsNamed and the overlay case give. Only loopback
// is still refused, since an address on it reaches nothing off the machine.
// On OpenShift that address is on br-ex, which sysfs describes no better than it
// describes a veth: same type, no bridge directory, no device link.
func TestADeviceHoldingTheNodeAddressIsNamedEvenUnidentified(t *testing.T) {
	// The machine's own virtual device: nothing identifies it either, and it
	// stands alone rather than naming a peer. This is br-ex on an OVN host.
	r := stackedReport(map[string][]string{"veth7a1c": {"10.0.0.15"}})

	if got := ManagementInterface(r, "10.0.0.15"); got != "veth7a1c" {
		t.Errorf("named %q, want the device holding the address the node is reached on", got)
	}

	// The same device, now one end of a pair: that is a pod's link, and it is
	// refused however the address got onto it.
	peered := peeredReport(map[string][]string{"veth7a1c": {"10.0.0.15"}}, "veth7a1c")
	if got := ManagementInterface(peered, "10.0.0.15"); got != "" {
		t.Errorf("named %q, want nothing: a device naming a peer is a pod's link", got)
	}

	// A VLAN names its parent the same way and is identified, so it stays
	// eligible: refusing on the link alone would refuse a tagged interface.
	tagged := peeredReport(map[string][]string{vlan100: {"10.0.0.15"}}, vlan100)
	if got := ManagementInterface(tagged, "10.0.0.15"); got != vlan100 {
		t.Errorf("named %q, want the VLAN: its link is its parent, and it is declared", got)
	}
	if got := ManagementInterface(stackedReport(map[string][]string{"lo": {"127.0.0.1"}}), "127.0.0.1"); got != "" {
		t.Errorf("named %q, want nothing: loopback reaches nothing off the machine", got)
	}
}

func TestTheEffectiveSpeedOfAnAggregateIsItsMembers(t *testing.T) {
	// A bond reports no speed of its own, so ranking it on what it reports puts
	// it behind every physical NIC. What it can carry is what its members can.
	r := stackedReport(map[string][]string{
		bond0:  {"10.10.10.113"},
		"eth2": {"192.168.1.10"},
	})

	if got := ManagementInterface(r, ""); got != bond0 {
		t.Errorf("named %q, want the bond: 2x25G carries more than one 10G NIC", got)
	}
}

func TestADerivedInterfaceInheritsTheSpeedOfWhatItIsBuiltOn(t *testing.T) {
	r := stackedReport(map[string][]string{
		vlan100: {"10.10.10.113"},
		"eth2":  {"192.168.1.10"},
	})

	if got := ManagementInterface(r, ""); got != vlan100 {
		t.Errorf("named %q, want the VLAN over the bond", got)
	}
}

func TestAPhysicalInterfaceWinsATieWithADerivedOne(t *testing.T) {
	// Both carry the same traffic over the same hardware, and the untagged one
	// is the simpler answer for a reviewer to read.
	r := report("worker-1")
	r.Interfaces = []nodeprobe.Interface{
		{
			Name: eth0, Kind: string(inventory.LinkPhysical), SpeedMbps: 25000,
			State: "up", Addresses: []string{"192.168.1.10"}, Upper: []string{"eth0.100"},
		},
		{
			Name: "eth0.100", Kind: string(inventory.LinkVLAN), SpeedMbps: 25000, Virtual: true,
			State: "up", Addresses: []string{"10.10.10.113"}, Lower: []string{eth0},
		},
	}

	if got := ManagementInterface(r, ""); got != eth0 {
		t.Errorf("named %q, want the physical interface", got)
	}
}

func TestTheHardwareUnderTheChosenInterfaceIsReported(t *testing.T) {
	// What a bond amounts to is not readable from the bond: it carries no slot,
	// no driver, and no memory node. The members are the only route to all three.
	r := stackedReport(map[string][]string{bond0: {"10.10.10.113"}})

	mgmt := ManagementOf(r, "10.10.10.113")
	if mgmt.Name != bond0 || mgmt.Kind != string(inventory.LinkBond) {
		t.Fatalf("chose %+v", mgmt)
	}
	if !slices.Equal(mgmt.Members, []string{eth0, eth1}) {
		t.Errorf("reported members %v, want both NICs", mgmt.Members)
	}
	if mgmt.SpeedMbps != 50000 {
		t.Errorf("reported %d Mbps, want the sum of the members", mgmt.SpeedMbps)
	}
	if mgmt.NUMANode != 0 {
		t.Errorf("reported memory node %d, want 0", mgmt.NUMANode)
	}
	if mgmt.Reason == "" {
		t.Error("the choice carries no reason")
	}
}

func TestTheHardwareUnderADerivedInterfaceResolvesThroughItsParent(t *testing.T) {
	r := stackedReport(map[string][]string{vlan100: {"10.10.10.113"}})

	mgmt := ManagementOf(r, "10.10.10.113")
	if !slices.Equal(mgmt.Members, []string{eth0, eth1}) {
		t.Errorf("the VLAN reports members %v, want the bond's NICs", mgmt.Members)
	}
}

func TestMembersOnDifferentMemoryNodesReportNone(t *testing.T) {
	// A bond across two sockets has no memory node, and reporting one of them
	// would claim an affinity the interface does not have.
	r := stackedReport(map[string][]string{bond0: {"10.10.10.113"}})
	for i := range r.Interfaces {
		if r.Interfaces[i].Name == eth1 {
			r.Interfaces[i].NUMANode = 1
		}
	}

	if got := ManagementOf(r, "10.10.10.113").NUMANode; got != inventory.NUMANodeUnknown {
		t.Errorf("reported memory node %d, want %d", got, inventory.NUMANodeUnknown)
	}
}

func TestAnInterfaceThatNamesNoKindFallsBackToWhatElseWasReported(t *testing.T) {
	// A report written before the kind existed is refused outright, but a
	// fixture or a probe that left the field empty should still be read the way
	// it was before: physical unless something said otherwise.
	r := report("worker-1")
	r.Interfaces = []nodeprobe.Interface{
		{Name: eth0, State: "up", Addresses: []string{"192.168.1.10"}, SpeedMbps: 10000},
		{Name: "cni0", State: "up", Addresses: []string{"10.42.2.1"}, Virtual: true, Bridge: true},
		{Name: "flannel.1", State: "up", Addresses: []string{"10.42.2.0"}, Virtual: true},
	}

	if got := ManagementInterface(r, ""); got != eth0 {
		t.Errorf("named %q, want the one interface nothing marked virtual", got)
	}
}

func TestAStackThatPointsAtItselfTerminates(t *testing.T) {
	// Nothing in sysfs produces a cycle, and a resolution that would hang on one
	// is a resolution that trusts its input.
	r := report("worker-1")
	r.Interfaces = []nodeprobe.Interface{
		{
			Name: bond0, Kind: string(inventory.LinkBond), State: "up", Virtual: true,
			Addresses: []string{"10.10.10.113"}, Lower: []string{"bond1"},
		},
		{
			Name: "bond1", Kind: string(inventory.LinkBond), State: "up", Virtual: true,
			Lower: []string{bond0},
		},
	}

	if got := ManagementOf(r, "10.10.10.113").Name; got != bond0 {
		t.Errorf("chose %q", got)
	}
}
