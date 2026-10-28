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
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

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

// definitionNetworks are the cluster's networks attached through a definition.
func definitionNetworks(network *simplyblockv1alpha2.StorageNetworkSpec) []simplyblockv1alpha2.StorageNetwork {
	if network == nil {
		return nil
	}
	var out []simplyblockv1alpha2.StorageNetwork
	if network.Management != nil {
		out = append(out, *network.Management)
	}
	out = append(out, network.Data...)
	kept := out[:0]
	for _, n := range out {
		if n.Attachment.NetworkAttachmentRef != nil {
			kept = append(kept, n)
		}
	}
	return kept
}

// adoptAddresses records, for every network attached through a definition that
// has none, the node's address: from the network's pool when it declares one,
// otherwise the address of the NIC the definition moves. An address a node holds
// is kept and never chosen again, except that a node that chose an address at the
// same moment as an earlier-named node of the namespace gives its choice up, so
// two nodes settle on two addresses without a lock.
func (r *StorageNodeReconciler) adoptAddresses(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, configs map[string]inventory.NetworkAttachmentConfig,
) error {
	networks := definitionNetworks(networkOf(cluster))
	var siblings []simplyblockv1alpha2.StorageNode
	if slices.ContainsFunc(networks, func(n simplyblockv1alpha2.StorageNetwork) bool { return n.AddressPool != nil }) {
		var err error
		if siblings, err = r.poolSiblings(ctx, node); err != nil {
			return err
		}
	}

	next := slices.Clone(node.Status.Network)
	for _, n := range networks {
		index := slices.IndexFunc(next, func(s simplyblockv1alpha2.NodeNetworkStatus) bool { return s.Network == n.Name })
		if index < 0 {
			next = append(next, simplyblockv1alpha2.NodeNetworkStatus{
				Network: n.Name, Phase: simplyblockv1alpha2.NodeNetworkPhasePending,
			})
			index = len(next) - 1
		}
		entry := &next[index]

		var held []string
		if n.AddressPool != nil {
			var earlier []string
			held, earlier = heldOnPool(siblings, node.Name, n.Name, n.AddressPool.Subnet)
			if entry.Assigned != "" && entry.Phase == simplyblockv1alpha2.NodeNetworkPhasePending &&
				slices.ContainsFunc(earlier, func(a string) bool { return sameHostAddress(a, entry.Assigned) }) {
				entry.Assigned = ""
			}
		}
		if entry.Assigned != "" {
			continue
		}

		address, err := r.chooseAddress(ctx, node, cluster, n, held, configs[n.Name])
		if err != nil {
			return err
		}
		entry.Assigned = address
	}

	if equalNetworkStatus(node.Status.Network, next) {
		return nil
	}
	return r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
		status.Network = next
	})
}

// chooseAddress is the node's address on one network: the first free address of
// its pool, or the address of its NIC.
func (r *StorageNodeReconciler) chooseAddress(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, network simplyblockv1alpha2.StorageNetwork,
	held []string, config inventory.NetworkAttachmentConfig,
) (string, error) {
	if network.AddressPool == nil {
		return r.readNIC(ctx, node, cluster, network.Name, config)
	}

	address, err := allocateAddress(*network.AddressPool, held)
	switch {
	case errors.Is(err, errPoolExhausted):
		return "", r.heldFor(cluster, NetworkAddressPoolExhausted,
			"the address pool of network %q (%s) has no address left: %d are held by other nodes",
			network.Name, network.AddressPool.Subnet, len(held))
	case err != nil:
		return "", r.heldFor(cluster, NetworkAttachmentFailed, "network %q: %v", network.Name, err)
	}
	return address, nil
}

// poolSiblings is every other storage node that may hold an address of the
// node's pools: all the nodes of the namespace, not only the node's cluster. The
// member clusters of a supercluster copy one network block from the document and
// draw from one pool, and an administrator may point two clusters at one
// definition.
func (r *StorageNodeReconciler) poolSiblings(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) ([]simplyblockv1alpha2.StorageNode, error) {
	var siblings simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &siblings, client.InNamespace(node.Namespace)); err != nil {
		return nil, fmt.Errorf("listing the storage nodes of the namespace: %w", err)
	}
	return siblings.Items, nil
}

// heldOnPool is the addresses the other nodes hold on a pool, and the subset
// held by nodes whose names sort before name. A sibling holds an address of the
// pool when its entry carries the same network name, or when the address lies in
// the pool's subnet under any network name: the clash is on the wire, whatever
// the clusters call the network.
func heldOnPool(siblings []simplyblockv1alpha2.StorageNode, name, network, subnet string) (held, earlier []string) {
	prefix, err := netip.ParsePrefix(subnet)
	inPool := func(entry simplyblockv1alpha2.NodeNetworkStatus) bool {
		if entry.Network == network {
			return true
		}
		if err != nil {
			return false
		}
		host, ok := hostOf(entry.Assigned)
		return ok && prefix.Contains(host)
	}
	for i := range siblings {
		sibling := &siblings[i]
		if sibling.Name == name {
			continue
		}
		for _, entry := range sibling.Status.Network {
			if entry.Assigned == "" || !inPool(entry) {
				continue
			}
			held = append(held, entry.Assigned)
			if strings.Compare(sibling.Name, name) < 0 {
				earlier = append(earlier, entry.Assigned)
			}
		}
	}
	return held, earlier
}

// sameHostAddress reports whether two addresses are one host address, whatever
// their prefix lengths.
func sameHostAddress(a, b string) bool {
	x, okA := hostOf(a)
	y, okB := hostOf(b)
	return okA && okB && x == y
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
			"network %q: its definition names no device or PCI address, so the NIC whose address the node takes cannot be found", network)
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
