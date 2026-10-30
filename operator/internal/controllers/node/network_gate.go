// The reachability gate: once the control plane reports a node online and its
// SPDK pod holds the declared address, the CSI node plugins of the workers are
// asked to connect to the SPDK listener, because that connection is the path a
// volume attach takes. A network is attached only when every worker reaches it.
//
// Specified by operator/docs/designs/design-storage-network.md §5.4 and §8.1.

package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// gateReachability runs the gate over the networks the verification found
// attached, and returns the statuses with the result recorded. A network whose
// attach the verification no longer finds has no result to keep, so the gate runs
// again when it returns. A network that passed is not tested again.
func (r *StorageNodeReconciler) gateReachability(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, next []simplyblockv1alpha2.NodeNetworkStatus,
) ([]simplyblockv1alpha2.NodeNetworkStatus, error) {
	if r.HostCheck == nil {
		return next, nil
	}
	out := slices.Clone(next)
	for i := range out {
		n := &out[i]
		if n.Phase != simplyblockv1alpha2.NodeNetworkPhaseAttached {
			n.HostReachable = nil
			continue
		}
		if n.HostReachable != nil && *n.HostReachable {
			continue
		}
		if err := r.testListener(ctx, node, n); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// testListener asks every linked plugin to connect to the node's listener at the
// address its pod reported, and records what they found on the network's status.
func (r *StorageNodeReconciler) testListener(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, n *simplyblockv1alpha2.NodeNetworkStatus,
) error {
	port := listenerPort(node)
	address := hostAddress(n.Address)
	if !nodeIsOnline(node) || port == 0 || address == "" {
		// The listeners exist once the control plane has finished the add, and the
		// port is the one it assigned: there is nothing to connect to before then.
		n.Phase = simplyblockv1alpha2.NodeNetworkPhasePending
		return nil
	}

	var unreached []string
	answered := 0
	for _, worker := range r.HostCheck.Workers() {
		connected, detail, err := r.HostCheck.Connect(ctx, worker, address, port)
		switch {
		case errors.Is(err, ErrHostCheckUnavailable):
			continue
		case err != nil:
			return fmt.Errorf("testing the connection from worker %s to %s:%d: %w", worker, address, port, err)
		}
		answered++
		if !connected {
			unreached = append(unreached, fmt.Sprintf("worker %s (%s)", worker, detail))
		}
	}

	switch {
	case answered == 0:
		n.Phase = simplyblockv1alpha2.NodeNetworkPhasePending
		r.emit(node, corev1.EventTypeWarning, NetworkHostCheckUnavailable,
			fmt.Sprintf("No CSI node plugin can test the connection to %s:%d on network %q, so it is not verified yet",
				address, port, n.Network))
	case len(unreached) > 0:
		wasUnreachable := heldUnreachable(node, n.Network)
		n.Phase = simplyblockv1alpha2.NodeNetworkPhaseUnavailable
		n.HostReachable = ptr.To(false)
		if !wasUnreachable {
			recordAttachFailure(node.Spec.ClusterRef, DataNetworkHostUnreachable)
			r.emit(node, corev1.EventTypeWarning, DataNetworkHostUnreachable,
				fmt.Sprintf("Cannot reach the SPDK listener at %s:%d on network %q from %s, so volumes cannot be attached from it. "+
					"Check the routes in the definition, the security groups of the fabric, and, for macvlan or ipvlan, "+
					"that the host reaches the pod through a host-side sibling interface on the parent",
					address, port, n.Network, strings.Join(unreached, ", ")))
		}
	default:
		n.HostReachable = ptr.To(true)
	}
	return nil
}

// networkGateHolds reports whether a declared network has not passed the gate,
// which keeps an online node from being reported Online. A cluster with no host
// check has no gate to pass.
func (r *StorageNodeReconciler) networkGateHolds(
	node *simplyblockv1alpha2.StorageNode, cluster *simplyblockv1alpha2.StorageCluster,
) bool {
	if r.HostCheck == nil {
		return false
	}
	for _, declared := range pooledNetworks(networkOf(cluster)) {
		passed := false
		for _, n := range node.Status.Network {
			if n.Network == declared.Name() {
				passed = n.Phase == simplyblockv1alpha2.NodeNetworkPhaseAttached &&
					n.HostReachable != nil && *n.HostReachable
			}
		}
		if !passed {
			return true
		}
	}
	return false
}

// listenerPort is the NVMe-oF port the control plane reports for the node, or
// zero before it has assigned one.
func listenerPort(node *simplyblockv1alpha2.StorageNode) int {
	if node.Status.Ports == nil || node.Status.Ports.NvmeOf == nil {
		return 0
	}
	return int(*node.Status.Ports.NvmeOf)
}

// hostAddress is the address a pod reported, without a prefix length, or empty
// when it is not one.
func hostAddress(reported string) string {
	if prefix, err := netip.ParsePrefix(reported); err == nil {
		return prefix.Addr().String()
	}
	if addr, err := netip.ParseAddr(reported); err == nil {
		return addr.String()
	}
	return ""
}

// nodeIsOnline reports whether the control plane has finished adding the node,
// which is when its listeners exist.
func nodeIsOnline(node *simplyblockv1alpha2.StorageNode) bool {
	return node.Status.Status == nodeStatusOnline || node.Status.Status == nodeStatusActive
}

// heldUnreachable reports whether the node's recorded status already says the
// network is unreachable, so the event is raised on the change and not every pass.
func heldUnreachable(node *simplyblockv1alpha2.StorageNode, network string) bool {
	for _, n := range node.Status.Network {
		if n.Network == network {
			return n.HostReachable != nil && !*n.HostReachable
		}
	}
	return false
}
