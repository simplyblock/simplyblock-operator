// Choosing the interface a storage node binds its management address to.
//
// The control plane refuses a node whose management interface it cannot find an
// IP on, and it refuses it inside the node_add task rather than at the request:
// what an administrator sees is a task that gave up, on a document that looked
// complete. So the draft names one, and names it from what the probe read rather
// than leaving it to be defaulted somewhere further down.
//
// The rule is a ranking rather than a match, because a fleet's machines do not
// agree on what their NICs are called and a draft that named eth0 everywhere
// would be wrong on the machines that call it ens5f0. What every candidate has
// in common is the shape of the answer: hardware, up, and holding an address
// something can reach it on.

package discovery

import (
	"cmp"
	"net"
	"slices"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// ManagementInterface is the interface a draft names for this worker, or empty
// when the machine presents none that would serve.
//
// nodeAddress is the address the cluster already reaches the machine on, and
// when it is known the interface holding it wins outright. That is not a
// preference among equals: the operator addresses the worker by that address
// everywhere else it talks to it, so any other choice would have the two halves
// of one deployment describing different networks.
//
// Returning empty is a real answer rather than a failure. A machine whose only
// addressed interfaces are its cluster's own bridges has no management interface,
// and a draft that named one anyway would produce a storage node the rest of the
// fleet cannot reach.
func ManagementInterface(report nodeprobe.Report, nodeAddress string) string {
	candidates := make([]nodeprobe.Interface, 0, len(report.Interfaces))
	for _, iface := range report.Interfaces {
		if !servesManagement(iface) {
			continue
		}
		if nodeAddress != "" && slices.Contains(iface.Addresses, nodeAddress) {
			return iface.Name
		}
		candidates = append(candidates, iface)
	}
	if len(candidates) == 0 {
		return ""
	}

	// Fastest first, then by name. The name is what makes the choice stable: the
	// probe's reading order is the kernel's, and a draft that changed between two
	// runs of the same fleet is one a reviewer cannot diff.
	slices.SortFunc(candidates, func(a, b nodeprobe.Interface) int {
		if a.SpeedMbps != b.SpeedMbps {
			return cmp.Compare(b.SpeedMbps, a.SpeedMbps)
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return candidates[0].Name
}

// servesManagement reports whether an interface could carry a storage node's
// management traffic at all.
//
// Each condition rules out a machine this product has actually been deployed
// onto. The virtual ones are the cluster's own plumbing — a CNI bridge, a veth
// to a pod, a flannel overlay, loopback — and every worker has several holding
// addresses that reach nothing outside the node. A link that is down keeps the
// address it was configured with and carries nothing. An interface with no
// address is what the control plane refuses by name.
func servesManagement(iface nodeprobe.Interface) bool {
	if iface.Virtual || iface.Bridge || iface.Loopback {
		return false
	}
	if iface.State != "" && iface.State != "up" && iface.State != "unknown" {
		return false
	}
	return slices.ContainsFunc(iface.Addresses, reachable)
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
