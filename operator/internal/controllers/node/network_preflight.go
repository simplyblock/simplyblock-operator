// The checks that hold a node at CheckingNetwork until its networks can be
// attached: every definition exists, and every network has an address the
// operator has recorded. Nothing here attaches a network, so no pod holds the
// NIC before the control plane starts the SPDK pod.
//
// Specified by operator/docs/designs/design-storage-network.md §5.4 and §8.1.

package node

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/inventory"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/multus"
)

// declaresDefinitions reports whether the declaration attaches any network
// through a definition, which is what the checks have something to look at.
func declaresDefinitions(network *simplyblockv1alpha2.StorageNetworkSpec) bool {
	return len(pooledNetworks(network)) > 0
}

// checkNetwork is the gate. A cluster that attaches no network through a
// definition passes at once. Otherwise, every referenced definition has to exist
// and every network has to have a recorded address.
func (r *StorageNodeReconciler) checkNetwork(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) (nodeStep, bool, error) {
	network := networkOf(cluster)
	if !declaresDefinitions(network) {
		return stepAwaitingSlot, true, nil
	}

	definitions, err := r.requireDefinitions(ctx, node, cluster, network)
	if err != nil {
		return stepCheckingNetwork, false, err
	}
	if err := r.assignAddresses(ctx, node, cluster, definitions); err != nil {
		return stepCheckingNetwork, false, err
	}
	if _, err := BuildAttachments(node.Namespace, network, assignedAddresses(node.Status.Network)); err != nil {
		return stepCheckingNetwork, false, r.heldFor(cluster, NetworkAttachmentFailed, "%v", err)
	}
	return stepAwaitingSlot, true, nil
}

// heldFor counts the failure and returns the error that holds the node with its
// reason.
func (r *StorageNodeReconciler) heldFor(
	cluster *simplyblockv1alpha2.StorageCluster, reason, format string, args ...any,
) error {
	recordAttachFailure(cluster.Name, reason)
	return blockedf(reason, format, args...)
}

// definition is what the preflight reads of one NetworkAttachmentDefinition: its
// CNI configuration, and the extended resource a device plugin hands its device
// out under (the k8s.v1.cni.cncf.io/resourceName annotation), "" for a
// definition whose plugin creates the interface itself (macvlan, ipvlan) or names
// its device (host-device with device or pciBusID).
type definition struct {
	Config   inventory.NetworkAttachmentConfig
	Resource string
}

// requireDefinitions holds the node when a referenced definition cannot be read.
// It returns what it read of each definition by the name of the network that
// references it. This is the one read of a definition a node makes: what node-add
// needs of it later is recorded on the node's status with its address.
func (r *StorageNodeReconciler) requireDefinitions(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster, network *simplyblockv1alpha2.StorageNetworkSpec,
) (map[string]definition, error) {
	definitions := map[string]definition{}
	for _, n := range pooledNetworks(network) {
		ref := n.NetworkAttachmentRef
		nad, err := multus.GetDefinition(ctx, r.Client, client.ObjectKey{Namespace: node.Namespace, Name: ref.Name})
		switch {
		case err == nil:
		case multus.KindNotServed(err):
			return nil, r.heldFor(cluster, NetworkAttachmentUnsupported,
				"the cluster serves no NetworkAttachmentDefinition kind, so Multus is not installed")
		case apierrors.IsNotFound(err):
			return nil, r.heldFor(cluster, NetworkAttachmentNotFound,
				"the NetworkAttachmentDefinition %s/%s for network %q does not exist",
				node.Namespace, ref.Name, n.Name())
		default:
			return nil, fmt.Errorf("reading the definition %q: %w", ref.Name, err)
		}

		definitions[n.Name()] = definition{
			Config:   inventory.ParseNetworkAttachmentConfig(definitionConfig(nad)),
			Resource: nad.GetAnnotations()[inventory.NetworkAttachmentResourceNameAnnotation],
		}
	}
	return definitions, nil
}

// definitionConfig is the CNI configuration a definition carries.
func definitionConfig(definition *unstructured.Unstructured) string {
	config, _, _ := unstructured.NestedString(definition.Object, "spec", "config")
	return config
}
