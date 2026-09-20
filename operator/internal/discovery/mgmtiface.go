// Choosing the interface a storage node binds its management address to.
//
// The control plane refuses a node whose management interface it cannot find an
// IP on, and it refuses it inside the node_add task rather than at the request:
// what an administrator sees is a task that gave up, on a document that looked
// complete. So the draft names one, and names it from what the probe read rather
// than leaving it to be defaulted somewhere further down.
//
// The rule is a ladder rather than a match, because a fleet's machines do not
// agree on what their NICs are called and a draft that named eth0 everywhere
// would be wrong on the machines that call it ens5f0. What every candidate has
// in common is the shape of the answer: a device something can be bound to, up,
// and holding an address something can reach it on.
//
// The kind of device is the rung that carries the most weight, and it is the one
// this rule could not read until the probe reported it. Every software interface
// a worker has sits under devices/virtual: a bond, a tagged VLAN, a CNI bridge,
// a veth to a pod. A rule that refused virtual devices therefore refused a
// bonded or tagged management network along with the cluster's own plumbing,
// which is to say it refused the ordinary enterprise host. The kinds are told apart now, and
// the two that need a judgment rather than a rule get one: a bridge and an
// overlay are named only where the cluster's own address is on them, because the
// same bridge is a hypervisor host's management network or a pod network
// depending on nothing readable but that.

package discovery

import (
	"cmp"
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// Management is the interface a draft names for management, with the evidence
// behind the choice.
type Management struct {
	// Name is the interface, or empty when the machine presents none that would
	// serve.
	Name string

	// Kind is what sort of device it is, in inventory.LinkKind's spelling.
	Kind string

	// Members are the physical interfaces underneath it, ascending by name: the
	// slaves of a bond, the ports of a bridge, or the hardware a VLAN's parent
	// resolves to. It is empty for an interface that is itself physical.
	Members []string

	// SpeedMbps is what the interface can carry, resolved through the stack
	// where the interface reports nothing of its own.
	SpeedMbps int

	// NUMANode is the memory node the hardware underneath it sits on, or
	// inventory.NUMANodeUnknown when there is none or the members disagree.
	NUMANode int

	// Reason says what was chosen and why, for the record a reviewer reads.
	Reason string
}

// ManagementInterface is the interface a draft names for this worker, or empty
// when the machine presents none that would serve.
func ManagementInterface(report nodeprobe.Report, nodeAddress string) string {
	return ManagementOf(report, nodeAddress).Name
}

// ManagementOf applies the ladder and returns what it chose.
//
// nodeAddress is the address the cluster already reaches the machine on, and
// when it is known the interface holding it wins outright. That is not a
// preference among equals: the operator addresses the worker by that address
// everywhere else it talks to it, so any other choice would have the two halves
// of one deployment describing different networks. It is also what settles the
// two kinds no rule can settle, since a bridge or an overlay carrying the
// cluster's own address is the management network by definition.
//
// An empty name is a real answer rather than a failure. A machine whose only
// addressed interfaces are its cluster's pod network has no management
// interface, and a draft that named one anyway would produce a storage node the
// rest of the fleet cannot reach.
func ManagementOf(report nodeprobe.Report, nodeAddress string) Management {
	index := interfacesByName(report)

	var candidates []nodeprobe.Interface
	for _, iface := range report.Interfaces {
		holdsNodeAddress := nodeAddress != "" && slices.Contains(iface.Addresses, nodeAddress)
		if !servesManagement(iface, holdsNodeAddress) {
			continue
		}
		if holdsNodeAddress {
			return resolve(iface, index, fmt.Sprintf(
				"it holds %s, the address the cluster reaches the machine on", nodeAddress))
		}
		candidates = append(candidates, iface)
	}
	if len(candidates) == 0 {
		return Management{NUMANode: inventory.NUMANodeUnknown}
	}

	// Fastest first, then the simpler kind, then by name. The name is what makes
	// the choice stable: the probe's reading order is the kernel's, and a draft
	// that changed between two runs of the same fleet is one a reviewer cannot
	// diff.
	speeds := make(map[string]int, len(candidates))
	for _, iface := range candidates {
		speeds[iface.Name] = effectiveSpeed(iface.Name, index, map[string]bool{})
	}
	slices.SortFunc(candidates, func(a, b nodeprobe.Interface) int {
		return cmp.Or(
			cmp.Compare(speeds[b.Name], speeds[a.Name]),
			cmp.Compare(simplicity(interfaceKind(a)), simplicity(interfaceKind(b))),
			cmp.Compare(a.Name, b.Name),
		)
	})

	best := candidates[0]
	why := fmt.Sprintf("it is the fastest interface holding a reachable address, at %d Mbps",
		speeds[best.Name])
	if len(candidates) == 1 {
		why = "it is the only interface holding a reachable address"
	}
	return resolve(best, index, why)
}

// resolve fills in what the stack says about the interface that was chosen.
func resolve(iface nodeprobe.Interface, index map[string]nodeprobe.Interface, why string) Management {
	out := Management{
		Name:      iface.Name,
		Kind:      string(interfaceKind(iface)),
		Members:   physicalUnder(iface.Lower, index, map[string]bool{}),
		SpeedMbps: effectiveSpeed(iface.Name, index, map[string]bool{}),
	}

	out.NUMANode = iface.NUMANode
	if len(out.Members) > 0 {
		out.NUMANode = numaNodeOf(out.Members, index)
	}

	out.Reason = fmt.Sprintf("%s is %s and %s", out.Name, describeKind(out.Kind, out.Members), why)
	return out
}

// describeKind names what sort of device was chosen, and what is under it where
// that is the only place the hardware appears.
func describeKind(kind string, members []string) string {
	if inventory.LinkKind(kind) == inventory.LinkPhysical {
		return "a physical interface"
	}
	if len(members) == 0 {
		return "a " + kind + " interface"
	}
	return fmt.Sprintf("a %s interface over %s", kind, strings.Join(members, " and "))
}

// servesManagement reports whether an interface could carry a storage node's
// management traffic at all.
//
// Each condition rules out a machine this product has been deployed onto. An
// unidentified virtual device is a veth to a pod or a dummy, and there are
// dozens of the first on every worker. A link that is down keeps the address it
// was configured with and carries nothing. An interface with no address is what
// the control plane refuses by name.
//
// The bridge and overlay condition is the one that is a judgment. Both kinds can
// be bound and both are ordinarily somebody else's network: a CNI bridge and a
// flannel overlay hold the pod network, and a hypervisor host's bridge holds the
// address the cluster reaches the machine on. Nothing readable separates the two
// except which address is on them, so that is what separates them here.
func servesManagement(iface nodeprobe.Interface, holdsNodeAddress bool) bool {
	kind := interfaceKind(iface)
	if iface.State != "" && iface.State != "up" && iface.State != "unknown" {
		return false
	}

	// The interface holding the address the cluster reaches this machine on is
	// the management interface, whatever kind the probe called it. It is not a
	// preference among candidates: the operator addresses the worker by that
	// address everywhere else, and it matches the backend node the control plane
	// reports against it, so naming any other interface hands the control plane
	// an address the operator cannot recognize the node by.
	//
	// The kind cannot be trusted to decide this. On OpenShift with
	// OVN-Kubernetes the node's own address lives on br-ex, which the probe
	// reports as virtual rather than as a bridge, so the bridge exemption below
	// never reached it and Bindable discarded it first.
	//
	// Two are still refused. Loopback reaches nothing off the machine. And an
	// interface that nothing identified, which also names another device as its
	// link, is one end of a veth pair whose other end is in a pod: admitting it
	// would admit every link the cluster's CNI leaves on the host, which is what
	// the kind test was reaching for and missing.
	//
	// Both halves of that are needed. A VLAN names its parent the same way, and
	// refusing on the link alone would refuse a tagged interface a fleet is
	// perfectly entitled to be reached on — the kernel declares that one, so it
	// is not unidentified.
	if holdsNodeAddress {
		unidentifiedPeer := kind == inventory.LinkVirtual && iface.Peered
		return kind != inventory.LinkLoopback && !unidentifiedPeer
	}

	if !kind.Bindable() {
		return false
	}
	// A bridge or an overlay that does not hold that address is somebody else's
	// network — the cluster's own fabric, most often — and is never named.
	if kind == inventory.LinkBridge || kind == inventory.LinkVXLAN {
		return false
	}
	return slices.ContainsFunc(iface.Addresses, reachable)
}

// simplicity orders the kinds for a tie, lowest first: the fewest layers
// between the address and the wire.
//
// It breaks a tie and never more than that, because the layering is not what
// decides whether an interface works. Two interfaces carrying the same traffic
// over the same hardware are equally usable, and the untagged one is the
// simpler answer for a reviewer to read.
func simplicity(kind inventory.LinkKind) int {
	switch kind {
	case inventory.LinkPhysical:
		return 0
	case inventory.LinkBond, inventory.LinkTeam:
		return 1
	case inventory.LinkVLAN, inventory.LinkMACVLAN, inventory.LinkIPVLAN:
		return 2
	default:
		return 3
	}
}

// reachable reports whether an address is one something could contact the
// machine on.
//
// A link-local address is configured without anybody assigning it and routes
// nowhere, so an interface holding only those holds nothing usable. An
// unspecified or loopback address is the same case read differently.
func reachable(address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	return !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() &&
		!ip.IsLoopback() && !ip.IsUnspecified()
}
