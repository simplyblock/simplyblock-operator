// A node's address on a network attached through a definition: drawn from the
// network's address pool, or, for a network with no pool, the address the NIC the
// definition moves into the pod holds on the host, read before anything attaches
// and recorded in status.network.
//
// Specified by operator/docs/designs/design-storage-network.md §4.3 and §8.1.

package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/atlas/netcheck"
	"github.com/simplyblock/atlas/netcheck/netcheckrpc"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// ErrHostCheckUnavailable says a worker has no CSI node plugin that can answer
// a host check: it is not linked, or it does not advertise the capability.
var ErrHostCheckUnavailable = netcheckrpc.ErrUnavailable

// InterfaceSelector names the host interface a definition moves, by its name or
// by the PCI address of its device.
type InterfaceSelector = netcheck.Selector

// InterfaceReport is what the worker's host shows for one interface: whether it
// has it, and its addresses in CIDR form in the order the host reports them.
type InterfaceReport = netcheck.Report

// HostChecker is what the operator asks of the CSI node plugins, which run on
// the host network of their worker (design §8.1).
type HostChecker interface {
	// InterfaceAddresses reads one host interface of a worker.
	InterfaceAddresses(ctx context.Context, worker string, selector InterfaceSelector) (InterfaceReport, error)
	// Connect opens a TCP connection from a worker's host to an address and
	// reports whether it connected, and why not when it did not.
	Connect(ctx context.Context, worker, address string, port int) (connected bool, detail string, err error)
	// Workers lists the workers that have a plugin linked that can answer.
	Workers() []string
}

// usableAddress is the interface's first IPv4 address with its prefix length,
// and whether it has one. It is the address the control plane would take from
// the interface on the host. A loopback or link-local address is not usable,
// because nothing could reach the node on it.
func usableAddress(addresses []string) (string, bool) {
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address)
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		if ip := prefix.Addr(); ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			continue
		}
		return prefix.String(), true
	}
	return "", false
}

// selectorOf is the interface a definition's configuration names.
func selectorOf(config inventory.NetworkAttachmentConfig) InterfaceSelector {
	return InterfaceSelector{Name: config.HostInterface, PCIAddress: config.PCIAddress}
}

// readNIC asks the CSI node plugin of the node's worker for the address of the
// NIC a definition moves, and holds the node with the reason when it cannot be
// had.
func (r *StorageNodeReconciler) readNIC(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, network string, config inventory.NetworkAttachmentConfig,
) (string, error) {
	worker := node.Spec.WorkerNode
	selector := selectorOf(config)
	if selector.Validate() != nil {
		return "", r.heldFor(cluster, NetworkAttachmentFailed,
			"network %q: its definition names no device or PCI address, so the NIC whose address the node takes cannot be found; "+
				"a definition whose device a device plugin hands out (an SR-IOV VF pool) names none and needs an addressPool", network)
	}
	if r.HostCheck == nil {
		return "", r.heldFor(cluster, NetworkHostCheckUnavailable,
			"network %q: no host check is configured (it needs the CSI link, --csi-link), so the address of %s on worker %s cannot be read", network, selector, worker)
	}

	report, err := r.HostCheck.InterfaceAddresses(ctx, worker, selector)
	switch {
	case errors.Is(err, ErrHostCheckUnavailable):
		return "", r.heldFor(cluster, NetworkHostCheckUnavailable,
			"network %q: no CSI node plugin of worker %s can read its interfaces", network, worker)
	case err != nil:
		return "", fmt.Errorf("reading %s on worker %s: %w", selector, worker, err)
	case !report.Found:
		return "", r.heldFor(cluster, NetworkAttachmentFailed,
			"network %q: the interface %s that its definition names is not on worker %s", network, selector, worker)
	}

	address, ok := usableAddress(report.Addresses)
	if !ok {
		return "", r.heldFor(cluster, NetworkInterfaceNoAddress,
			"network %q: the interface %s of worker %s holds no usable IPv4 address", network, selector, worker)
	}
	return address, nil
}
