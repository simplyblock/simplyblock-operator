// The step that gives a node its address on each network attached through a
// definition: drawn from the network's pool, or read from its NIC, and recorded
// in status before anything attaches, then never changed.
//
// Specified by operator/docs/designs/design-storage-network.md §4.3 and §4.5.

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

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// declaredNetworks are the cluster's data networks.
func declaredNetworks(network *simplyblockv1alpha2.StorageNetworkSpec) []simplyblockv1alpha2.StorageNetwork {
	if network == nil {
		return nil
	}
	return network.Data
}

// pooledNetworks are the networks attached through a definition, which are all of
// them: their node addresses come from a pool or from their NIC.
func pooledNetworks(network *simplyblockv1alpha2.StorageNetworkSpec) []simplyblockv1alpha2.StorageNetwork {
	return declaredNetworks(network)
}

// assignAddresses records, for every network attached through a definition that
// has none, the node's address: from the network's pool when it declares one,
// otherwise the address of the NIC the definition moves. An address a node holds
// is kept and never chosen again, except that a node that chose an address at the
// same moment as an earlier-named node of the namespace gives its choice up, so
// two nodes settle on two addresses without a lock.
func (r *StorageNodeReconciler) assignAddresses(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, definitions map[string]definition,
) error {
	networks := pooledNetworks(networkOf(cluster))
	var siblings []simplyblockv1alpha2.StorageNode
	if slices.ContainsFunc(networks, func(n simplyblockv1alpha2.StorageNetwork) bool { return n.Pool() != nil }) {
		var err error
		if siblings, err = r.poolSiblings(ctx, node); err != nil {
			return err
		}
	}

	next := slices.Clone(node.Status.Network)
	for _, n := range networks {
		index := slices.IndexFunc(next, func(s simplyblockv1alpha2.NodeNetworkStatus) bool { return s.Network == n.Name() })
		if index < 0 {
			next = append(next, simplyblockv1alpha2.NodeNetworkStatus{
				Network: n.Name(), Phase: simplyblockv1alpha2.NodeNetworkPhasePending,
			})
			index = len(next) - 1
		}
		entry := &next[index]
		entry.Resource = definitions[n.Name()].Resource

		var held []string
		if n.Pool() != nil {
			var earlier []string
			held, earlier = heldOnPool(siblings, node.Name, n.Name(), n.Pool().Subnet)
			if entry.Assigned != "" && entry.Phase == simplyblockv1alpha2.NodeNetworkPhasePending &&
				slices.ContainsFunc(earlier, func(a string) bool { return sameHostAddress(a, entry.Assigned) }) {
				entry.Assigned = ""
			}
		}
		if entry.Assigned != "" {
			continue
		}

		address, err := r.chooseAddress(ctx, node, cluster, n, held, definitions[n.Name()].Config)
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
	if network.Pool() == nil {
		return r.readNIC(ctx, node, cluster, network.Name(), config)
	}

	address, err := allocateAddress(*network.Pool(), held)
	switch {
	case errors.Is(err, errPoolExhausted):
		return "", r.heldFor(cluster, NetworkAddressPoolExhausted,
			"the address pool of network %q (%s) has no address left: %d are held by other nodes",
			network.Name(), network.Pool().Subnet, len(held))
	case err != nil:
		return "", r.heldFor(cluster, NetworkAttachmentFailed, "network %q: %v", network.Name(), err)
	}
	return address, nil
}

// poolSiblings is every other storage node that may hold an address of the
// node's pools: all the nodes of the namespace, not only the node's cluster. The
// member clusters of a supercluster copy one network block from the document
// and draw from one pool, and an administrator may point two clusters at one
// definition. Scoped to the cluster, every member started at the pool's first
// address and the duplicate-address check caught only the ones already live on
// the wire (Talos lab, 2026-10-25: workers 2 and 3 both assigned the address
// worker 1 held, then both the next one).
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
// the clusters call the network. An empty subnet matches by name only.
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
