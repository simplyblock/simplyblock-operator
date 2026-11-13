// The reconcile step that compares the SPDK pod's network-status with the
// declaration and records the result in status.network, with an event on the
// first full attach and on a mismatch.
//
// Specified by operator/docs/designs/design-storage-network.md §5.4.

package node

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// spdkPodRole is the label value that marks a storage node's SPDK pod.
const spdkPodRole = "simplyblock-storage-node"

// syncNetwork verifies the node's attachments against its SPDK pod and records
// the result. It returns an error only for what a retry can mend: an API read
// that failed, or an annotation that cannot be read.
func (r *StorageNodeReconciler) syncNetwork(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	if onPodNetwork(cluster) {
		// Nothing is attached, so there is no network-status to verify: the
		// Service in front of the pod is what the node's status reports.
		return r.syncPodNetwork(ctx, node, cluster)
	}
	declared, err := BuildAttachments(node.Namespace, networkOf(cluster), assignedAddresses(node.Status.Network))
	if errors.Is(err, errNoAddress) {
		// The node has not reached the step that assigns its addresses, so there is
		// nothing declared to verify.
		return nil
	}
	if err != nil {
		return fmt.Errorf("building the node's attachments: %w", err)
	}
	if len(declared) == 0 {
		return nil
	}

	annotation, running, err := r.spdkPodNetworkStatus(ctx, node)
	if err != nil {
		return err
	}
	verdicts, err := VerifyAttachments(declared, annotation, running)
	if err != nil {
		return fmt.Errorf("verifying the node's attachments: %w", err)
	}

	previous := node.Status.Network
	next, err := r.gateReachability(ctx, node, networkStatusOf(verdicts, previous))
	if err != nil {
		return err
	}
	recordNetworkMetrics(node.Spec.ClusterRef, node.Name, next)
	if !equalNetworkStatus(previous, next) {
		if err := r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
			status.Network = next
		}); err != nil {
			return err
		}
		r.announceNetwork(node, previous, next)
	}
	return r.shutDownMismatched(ctx, node, cluster, next)
}

// shutDownMismatched takes a node out of service when its SPDK pod holds an
// address other than the declared one. It repeats each pass until the control
// plane reports the node offline or going offline, and never restarts the node.
func (r *StorageNodeReconciler) shutDownMismatched(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, status []simplyblockv1alpha2.NodeNetworkStatus,
) error {
	mismatched := false
	for _, n := range status {
		mismatched = mismatched || n.Phase == simplyblockv1alpha2.NodeNetworkPhaseMismatched
	}
	if !mismatched || node.Status.Status == nodeStatusOffline || node.Status.Status == nodeStatusInShutdown {
		return nil
	}
	if err := r.API.ShutdownNode(ctx, cluster.Status.UUID, node.Status.UUID); err != nil {
		return fmt.Errorf("shutting down the node whose SPDK pod holds a different address: %w", err)
	}
	return nil
}

// networkOf is the cluster's declared networks, or nil for one that declares
// none.
func networkOf(cluster *simplyblockv1alpha2.StorageCluster) *simplyblockv1alpha2.StorageNetworkSpec {
	if cluster == nil || cluster.Spec.StorageNodes == nil {
		return nil
	}
	return cluster.Spec.StorageNodes.Network
}

// spdkPodNetworkStatus is the network-status annotation of the node's SPDK pod
// and whether the pod is running. A node with no pod yet, or no RPC port to find
// one by, has neither.
func (r *StorageNodeReconciler) spdkPodNetworkStatus(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) (annotation string, running bool, err error) {
	if node.Status.Ports == nil || node.Status.Ports.Rpc == nil {
		return "", false, nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(node.Namespace),
		client.MatchingLabels{"role": spdkPodRole}); err != nil {
		return "", false, fmt.Errorf("listing the SPDK pods: %w", err)
	}
	pod, found := SPDKPodFor(pods.Items, node.Spec.WorkerNode, *node.Status.Ports.Rpc)
	if !found {
		return "", false, nil
	}
	return pod.Annotations[NetworkStatusAnnotation], pod.Status.Phase == corev1.PodRunning, nil
}

// networkStatusOf turns the verdicts into the node's status entries, carrying
// forward what the preflight recorded and the verification does not know.
func networkStatusOf(
	verdicts []NetworkVerdict, previous []simplyblockv1alpha2.NodeNetworkStatus,
) []simplyblockv1alpha2.NodeNetworkStatus {
	kept := map[string]simplyblockv1alpha2.NodeNetworkStatus{}
	for _, p := range previous {
		kept[p.Network] = p
	}
	next := make([]simplyblockv1alpha2.NodeNetworkStatus, 0, len(verdicts))
	for _, v := range verdicts {
		next = append(next, simplyblockv1alpha2.NodeNetworkStatus{
			Network: v.Network, Interface: v.Interface, Address: v.Reported,
			Assigned:      kept[v.Network].Assigned,
			HostReachable: kept[v.Network].HostReachable, Phase: v.Phase,
		})
	}
	return next
}

// announceNetwork raises the events a change of phase owes: one when every
// network has just become attached, and one for each network that has just
// become mismatched.
func (r *StorageNodeReconciler) announceNetwork(
	node *simplyblockv1alpha2.StorageNode, previous, next []simplyblockv1alpha2.NodeNetworkStatus,
) {
	was := map[string]simplyblockv1alpha2.NodeNetworkPhase{}
	for _, p := range previous {
		was[p.Network] = p.Phase
	}

	allAttached, wasAllAttached := true, len(previous) == len(next)
	for _, n := range next {
		if n.Phase != simplyblockv1alpha2.NodeNetworkPhaseAttached {
			allAttached = false
		}
		if was[n.Network] != simplyblockv1alpha2.NodeNetworkPhaseAttached {
			wasAllAttached = false
		}
		if n.Phase == simplyblockv1alpha2.NodeNetworkPhaseMismatched &&
			was[n.Network] != simplyblockv1alpha2.NodeNetworkPhaseMismatched {
			recordAttachFailure(node.Spec.ClusterRef, NetworkAddressMismatch)
			r.emit(node, corev1.EventTypeWarning, NetworkAddressMismatch,
				fmt.Sprintf("The SPDK pod holds %s on network %q, which differs from the declared address",
					n.Address, n.Network))
		}
	}
	if allAttached && !wasAllAttached {
		r.emit(node, corev1.EventTypeNormal, NetworkAttached, "Every declared network is attached and verified")
	}
}

// recordNetworkMetrics sets the node's gauges from its network status. A network
// the preflight has not checked has no host-reachable series, so an alert on 0
// does not fire for a check that has not run.
func recordNetworkMetrics(cluster, node string, status []simplyblockv1alpha2.NodeNetworkStatus) {
	for _, n := range status {
		attached := 0.0
		if n.Phase == simplyblockv1alpha2.NodeNetworkPhaseAttached {
			attached = 1
		}
		networkAttached.WithLabelValues(cluster, node, n.Network).Set(attached)

		if n.HostReachable == nil {
			continue
		}
		reachable := 0.0
		if *n.HostReachable {
			reachable = 1
		}
		networkHostReachable.WithLabelValues(cluster, node, n.Network).Set(reachable)
	}
}

// recordAttachFailure counts an attach attempt that ended in the given event
// reason.
func recordAttachFailure(cluster, reason string) {
	networkAttachFailuresTotal.WithLabelValues(cluster, reason).Inc()
}

func equalNetworkStatus(a, b []simplyblockv1alpha2.NodeNetworkStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Network != b[i].Network || a[i].Interface != b[i].Interface ||
			a[i].Address != b[i].Address || a[i].Assigned != b[i].Assigned || a[i].Resource != b[i].Resource ||
			a[i].Phase != b[i].Phase || !equalBool(a[i].HostReachable, b[i].HostReachable) {
			return false
		}
	}
	return true
}

func equalBool(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
