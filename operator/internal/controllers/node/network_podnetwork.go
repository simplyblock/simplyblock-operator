// A node on the pod network: its SPDK pod attaches to nothing beyond the
// cluster CNI, and a Service the control plane's node agent keeps in front of
// the pod is the address peers and clients hold. The operator declares the mode
// to node-add and reports the Service's address as the node's, since there is no
// Multus network-status to verify against.
//
// Specified by operator/docs/designs/design-storage-network.md §4.1a.

package node

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

const (
	// podNetworkInterface is the CNI's interface in the pod, the one the node
	// serves on.
	podNetworkInterface = "eth0"

	// podNetworkName is the key the pod network takes in the node's network status,
	// where a Multus network takes its definition's name.
	podNetworkName = "pod"
	// clusterLabel is the label the node agent puts on the Service and on the
	// SPDK pod with the control plane's full cluster id, which the Service's
	// selector uses too. The Service's name carries only a short id.
	clusterLabel = "simplyblock.io/cluster"
	// serviceNamePrefix is how the node agent names the Service in front of an
	// SPDK pod, followed by the RPC port and the cluster's short id, as the pod
	// itself is named.
	serviceNamePrefix = "snode-spdk-svc-"
	clusterShortID    = 6
)

// podNetworkServiceName is the Service the node agent keeps for the node's SPDK
// pod, named as the agent names it.
func podNetworkServiceName(rpcPort int32, clusterUUID string) string {
	short := clusterUUID
	if len(short) > clusterShortID {
		short = short[:clusterShortID]
	}
	return fmt.Sprintf("%s%d-%s", serviceNamePrefix, rpcPort, short)
}

// syncPodNetwork records the node's pod-network status: the Service's address
// and whether the pod serves on it. Attached once the pod runs behind an existing
// Service, Unavailable when the Service exists and the pod does not run, and
// Pending until the Service exists.
func (r *StorageNodeReconciler) syncPodNetwork(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	_, running, err := r.spdkPodNetworkStatus(ctx, node)
	if err != nil {
		return err
	}
	address, err := r.podNetworkAddress(ctx, node, cluster)
	if err != nil {
		return err
	}

	phase := simplyblockv1alpha2.NodeNetworkPhasePending
	switch {
	case address != "" && running:
		phase = simplyblockv1alpha2.NodeNetworkPhaseAttached
	case address != "":
		phase = simplyblockv1alpha2.NodeNetworkPhaseUnavailable
	}

	previous := node.Status.Network
	next := []simplyblockv1alpha2.NodeNetworkStatus{{
		Network: podNetworkName, Interface: podNetworkInterface, Address: address, Phase: phase,
	}}
	recordNetworkMetrics(node.Spec.ClusterRef, node.Name, next)
	if equalNetworkStatus(previous, next) {
		return nil
	}
	if err := r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
		status.Network = next
	}); err != nil {
		return err
	}
	wasAttached := len(previous) == 1 && previous[0].Phase == simplyblockv1alpha2.NodeNetworkPhaseAttached
	if phase == simplyblockv1alpha2.NodeNetworkPhaseAttached && !wasAttached {
		r.emit(node, corev1.EventTypeNormal, NetworkAttached,
			fmt.Sprintf("The SPDK pod serves on the pod network behind Service address %s", address))
	}
	return nil
}

// podNetworkAddress is the cluster address of the Service in front of the node's
// SPDK pod, "" while there is none or the node has no RPC port to name it by.
func (r *StorageNodeReconciler) podNetworkAddress(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, cluster *simplyblockv1alpha2.StorageCluster,
) (string, error) {
	if node.Status.Ports == nil || node.Status.Ports.Rpc == nil || cluster.Status.UUID == "" {
		return "", nil
	}
	var service corev1.Service
	name := podNetworkServiceName(*node.Status.Ports.Rpc, cluster.Status.UUID)
	if err := r.Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: name}, &service); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading the Service %s of the node's SPDK pod: %w", name, err)
	}
	// A Service of another cluster whose id shares the short prefix, or one made
	// by hand, carries another cluster label or none: it fronts no pod of this
	// node, and its address must not be reported as the node's.
	if service.Labels[clusterLabel] != cluster.Status.UUID {
		return "", nil
	}
	return service.Spec.ClusterIP, nil
}
