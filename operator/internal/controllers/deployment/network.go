// What a document states about the storage network, and what validation asks of
// it. The network is the cluster template's, with the address pool of each
// network attached through a definition. The document names no node address:
// the operator takes each node's from the pool when the node is added.
//
// Specified by operator/docs/designs/design-storage-network.md §4.

package deployment

import (
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// onANetwork reports whether the document creates a cluster on a storage
// network.
func onANetwork(config *simplyblockv1alpha2.ClusterDeploymentConfig) bool {
	return config.Spec.Cluster != nil && config.Spec.Cluster.Network != nil
}
