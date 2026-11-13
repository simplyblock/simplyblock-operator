// What a node-add carries for a node on a network: the attachment list built
// from the node's declaration. A cluster that states no network sends nothing.
//
// Specified by operator/docs/designs/design-storage-network.md §5.1 and §6.

package node

import (
	"fmt"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// networkParams is the node's attachments in the shape node-add takes, nil for a
// cluster with no network. A missing address is an error, because leaving the
// network out would start the SPDK pod on the host network. The device resource
// of a network is the one the preflight recorded on the status with the address.
func (r *StorageNodeReconciler) networkParams(
	node *simplyblockv1alpha2.StorageNode, cluster *simplyblockv1alpha2.StorageCluster,
) ([]utils.NodeNetworkParams, error) {
	network := networkOf(cluster)
	attachments, err := BuildAttachments(node.Namespace, network, assignedAddresses(node.Status.Network))
	if err != nil {
		return nil, fmt.Errorf("building the node's attachments: %w", err)
	}
	if len(attachments) == 0 {
		return nil, nil
	}

	resources := assignedResources(node.Status.Network)
	params := make([]utils.NodeNetworkParams, 0, len(attachments))
	for _, a := range attachments {
		params = append(params, utils.NodeNetworkParams{
			Name: a.Name, Namespace: a.Namespace, Interface: a.Interface, IPs: a.IPs, Resource: resources[a.Network],
		})
	}
	return params, nil
}

// onPodNetwork reports whether the cluster puts its SPDK pods on the pod network
// alone, behind a Service. With data networks the pod also attaches to them, and
// those addresses are stable, so no Service is kept.
func onPodNetwork(cluster *simplyblockv1alpha2.StorageCluster) bool {
	network := networkOf(cluster)
	return network != nil && network.PodNetwork && len(network.Data) == 0
}
