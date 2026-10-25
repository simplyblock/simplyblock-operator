// The step that gives a node its address on each network attached through a
// definition: chosen from the pool and recorded in status before anything
// attaches, then never changed. A probe that finds another host on it turns it away.
//
// Specified by operator/docs/designs/design-storage-network.md §4.5 and §8.1.

package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/netprobe"
)

// maxRejected is how many addresses a node may turn away on one network. It is
// the bound the status field carries.
const maxRejected = 64

// declaredNetworks are the cluster's networks, the management network first.
func declaredNetworks(network *simplyblockv1alpha2.StorageNetworkSpec) []simplyblockv1alpha2.StorageNetwork {
	if network == nil {
		return nil
	}
	var declared []simplyblockv1alpha2.StorageNetwork
	if network.Management != nil {
		declared = append(declared, *network.Management)
	}
	return append(declared, network.Data...)
}

// pooledNetworks are the networks attached through a definition, which are the
// ones whose node addresses come from a pool.
func pooledNetworks(network *simplyblockv1alpha2.StorageNetworkSpec) []simplyblockv1alpha2.StorageNetwork {
	var pooled []simplyblockv1alpha2.StorageNetwork
	for _, n := range declaredNetworks(network) {
		if n.Attachment.NetworkAttachmentRef != nil {
			pooled = append(pooled, n)
		}
	}
	return pooled
}

// assignAddresses gives the node an address on every pooled network that has none
// and records them. An address a node holds is kept. A node that chose an address
// at the same moment as an earlier-named node of the cluster gives its choice up,
// so two nodes settle on two addresses without a lock.
func (r *StorageNodeReconciler) assignAddresses(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, network *simplyblockv1alpha2.StorageNetworkSpec,
) error {
	pooled := pooledNetworks(network)
	if len(pooled) == 0 {
		return nil
	}

	var siblings simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &siblings, client.InNamespace(node.Namespace),
		client.MatchingFields{clusterRefField: node.Spec.ClusterRef}); err != nil {
		return fmt.Errorf("listing the cluster's nodes: %w", err)
	}

	next := slices.Clone(node.Status.Network)
	for _, n := range pooled {
		index := slices.IndexFunc(next, func(s simplyblockv1alpha2.NodeNetworkStatus) bool { return s.Network == n.Name })
		if index < 0 {
			next = append(next, simplyblockv1alpha2.NodeNetworkStatus{
				Network: n.Name, Phase: simplyblockv1alpha2.NodeNetworkPhasePending,
			})
			index = len(next) - 1
		}
		entry := &next[index]

		held, earlier := heldOnNetwork(siblings.Items, node.Name, n.Name)
		if entry.Assigned != "" && entry.Phase == simplyblockv1alpha2.NodeNetworkPhasePending &&
			slices.ContainsFunc(earlier, func(a string) bool { return sameHostAddress(a, entry.Assigned) }) {
			entry.Assigned = ""
		}
		if entry.Assigned != "" {
			continue
		}
		if len(entry.Rejected) >= maxRejected {
			return r.heldFor(cluster, NetworkAddressPoolExhausted,
				"network %q: %d addresses were turned away, which is the most a node keeps", n.Name, maxRejected)
		}

		address, err := allocateAddress(*n.AddressPool, held, entry.Rejected)
		switch {
		case errors.Is(err, errPoolExhausted):
			return r.heldFor(cluster, NetworkAddressPoolExhausted,
				"the address pool of network %q (%s to %s) has no address left: %d are held by other nodes and %d were turned away",
				n.Name, n.AddressPool.First, n.AddressPool.Last, len(held), len(entry.Rejected))
		case err != nil:
			return r.heldFor(cluster, NetworkAddressPoolInvalid, "network %q: %v", n.Name, err)
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

// heldOnNetwork is the addresses the other nodes of the cluster hold on a
// network, and the subset held by nodes whose names sort before name.
func heldOnNetwork(siblings []simplyblockv1alpha2.StorageNode, name, network string) (held, earlier []string) {
	for i := range siblings {
		sibling := &siblings[i]
		if sibling.Name == name {
			continue
		}
		address := assignedAddresses(sibling.Status.Network)[network]
		if address == "" {
			continue
		}
		held = append(held, address)
		if strings.Compare(sibling.Name, name) < 0 {
			earlier = append(earlier, address)
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

// turnAwayAddress records the addresses other hosts answer for, takes the node's
// address back, and removes the probes so the next pass chooses again. The hold
// for a node that has turned away all it keeps happens on the next pass.
func (r *StorageNodeReconciler) turnAwayAddress(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, a Attachment, answering []string,
) error {
	recordAttachFailure(cluster.Name, NetworkAddressInUse)
	foreign, err := r.notHeldBySiblings(ctx, node, a.Network, answering)
	if err != nil {
		return err
	}
	err = r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
		for i := range status.Network {
			entry := &status.Network[i]
			if entry.Network != a.Network || entry.Assigned != a.IPs[0] {
				continue
			}
			entry.Rejected = mergeRejected(entry.Rejected, withPrefixOf(foreign, a.IPs[0]))
			entry.Assigned = ""
		}
	})
	if err != nil {
		return err
	}
	r.emit(node, corev1.EventTypeWarning, NetworkAddressInUse, fmt.Sprintf(
		"Another host answers for %s on network %q, so the node takes another address of the pool",
		strings.Join(answering, ", "), a.Network))
	return r.removeProbes(ctx, node)
}

// rememberAnswering records the candidates the probe found taken when the
// address it holds was free, so that no later choice tries them. It records
// nothing for a result that cannot be read, which judgeProbe has already refused.
func (r *StorageNodeReconciler) rememberAnswering(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, pod *corev1.Pod, attachments []Attachment,
) error {
	result, err := netprobe.ParseGuest(terminationMessage(pod))
	if err != nil {
		return nil
	}
	remembered := map[string][]string{}
	for _, a := range attachments {
		answering := result.AnsweringAddresses(a.Interface)
		foreign, err := r.notHeldBySiblings(ctx, node, a.Network, answering)
		if err != nil {
			return err
		}
		if len(foreign) > 0 {
			remembered[a.Network] = withPrefixOf(foreign, a.IPs[0])
		}
	}
	if len(remembered) == 0 {
		return nil
	}
	return r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
		for i := range status.Network {
			entry := &status.Network[i]
			entry.Rejected = mergeRejected(entry.Rejected, remembered[entry.Network])
		}
	})
}

// notHeldBySiblings is the addresses that answered and that no other node holds.
// An answer from a sibling's address is the sibling's probe, not a foreign host.
func (r *StorageNodeReconciler) notHeldBySiblings(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, network string, answering []string,
) ([]string, error) {
	if len(answering) == 0 {
		return nil, nil
	}
	var siblings simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &siblings, client.InNamespace(node.Namespace),
		client.MatchingFields{clusterRefField: node.Spec.ClusterRef}); err != nil {
		return nil, fmt.Errorf("listing the cluster's nodes: %w", err)
	}
	held, _ := heldOnNetwork(siblings.Items, node.Name, network)
	foreign := make([]string, 0, len(answering))
	for _, address := range answering {
		if !slices.ContainsFunc(held, func(h string) bool { return sameHostAddress(h, address) }) {
			foreign = append(foreign, address)
		}
	}
	return foreign, nil
}

// candidatesAfter is the addresses after the attachment's own that the probe
// asks about, as bare host addresses: the next ones of the network's pool that no
// other node holds and this node was not turned away from.
func candidatesAfter(
	network *simplyblockv1alpha2.StorageNetworkSpec, node *simplyblockv1alpha2.StorageNode,
	siblings []simplyblockv1alpha2.StorageNode, a Attachment,
) ([]string, error) {
	pool := poolDeclared(network, a.Network)
	if pool == nil {
		return nil, nil
	}
	held, _ := heldOnNetwork(siblings, node.Name, a.Network)
	var rejected []string
	for _, entry := range node.Status.Network {
		if entry.Network == a.Network {
			rejected = entry.Rejected
		}
	}
	found, err := candidateAddresses(*pool, append(held, a.IPs[0]), rejected, candidateBatch)
	switch {
	case errors.Is(err, errPoolExhausted):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("network %q: %w", a.Network, err)
	}
	hosts := make([]string, 0, len(found))
	for _, address := range found {
		if host, ok := hostOf(address); ok {
			hosts = append(hosts, host.String())
		}
	}
	return hosts, nil
}

// poolDeclared is the address pool of the named network, or nil for one that has none.
func poolDeclared(network *simplyblockv1alpha2.StorageNetworkSpec, name string) *simplyblockv1alpha2.NetworkAddressPool {
	for _, n := range declaredNetworks(network) {
		if n.Name == name {
			return n.AddressPool
		}
	}
	return nil
}

// withPrefixOf writes bare host addresses with the prefix length of like.
func withPrefixOf(hosts []string, like string) []string {
	prefix, err := netip.ParsePrefix(like)
	if err != nil {
		return nil
	}
	written := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if addr, err := netip.ParseAddr(h); err == nil {
			written = append(written, netip.PrefixFrom(addr, prefix.Bits()).String())
		}
	}
	return written
}

// mergeRejected adds addresses to a node's turned-away list without repeating a
// host address, and keeps no more than the bound.
func mergeRejected(current, added []string) []string {
	merged := slices.Clone(current)
	for _, a := range added {
		if len(merged) >= maxRejected {
			break
		}
		if !slices.ContainsFunc(merged, func(m string) bool { return sameHostAddress(m, a) }) {
			merged = append(merged, a)
		}
	}
	return merged
}
