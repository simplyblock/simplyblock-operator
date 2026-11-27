// The SPDK images a storage node should run and whether it runs them. A node's
// desired images come from its own spec, then its cluster's, then the control
// plane's defaults, and every restart the operator asks for carries them, which
// is what moves a fleet onto new images (design-release-upgrade.md §5). This
// lives beside the status sync because the status is where both halves meet:
// what the control plane reports and what the objects ask for.

package node

import (
	"context"

	"k8s.io/utils/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// applyImages records what the node runs and should run, and whether those
// agree. It reports whether the node just became out of date, which is the one
// moment worth an event.
//
// A reading that names no image is a control plane that does not report them,
// not a node running nothing, so the answer is unknown rather than false. An
// image nobody asks for is no reason to restart, so an empty desired image
// agrees with whatever runs.
func applyImages(
	status *simplyblockv1alpha2.StorageNodeStatus,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
	reading NodeReading,
) bool {
	wasCurrent := status.ImagesCurrent == nil || *status.ImagesCurrent

	status.DesiredSpdkImage, status.DesiredSpdkProxyImage = desiredImages(node, cluster)
	if reading.SpdkImage == "" && reading.SpdkProxyImage == "" {
		status.ImagesCurrent = nil
		return false
	}
	status.SpdkImage = reading.SpdkImage
	status.SpdkProxyImage = reading.SpdkProxyImage

	current := agrees(status.DesiredSpdkImage, status.SpdkImage) &&
		agrees(status.DesiredSpdkProxyImage, status.SpdkProxyImage)
	status.ImagesCurrent = ptr.To(current)
	return wasCurrent && !current
}

// desiredImages resolves the SPDK and SPDK proxy images the node should run:
// its own spec.config, else its cluster's spec.storageNodes, else the defaults
// the control plane reports. Each resolves on its own, so a node may pin one
// and follow the cluster for the other.
func desiredImages(
	node *simplyblockv1alpha2.StorageNode, cluster *simplyblockv1alpha2.StorageCluster,
) (spdk, proxy string) {
	var clusterSpdk, clusterProxy, defaultSpdk, defaultProxy string
	if cluster != nil {
		if workload := cluster.Spec.StorageNodes; workload != nil {
			clusterSpdk, clusterProxy = string(workload.SpdkImage), string(workload.SpdkProxyImage)
		}
		defaultSpdk, defaultProxy = cluster.Status.DefaultSpdkImage, cluster.Status.DefaultSpdkProxyImage
	}
	return firstNonEmpty(node.Spec.Config.SpdkImage, firstNonEmpty(clusterSpdk, defaultSpdk)),
		firstNonEmpty(node.Spec.Config.SpdkProxyImage, firstNonEmpty(clusterProxy, defaultProxy))
}

func agrees(desired, running string) bool { return desired == "" || desired == running }

// withDesiredImages is a restart's parameters carrying the node's desired
// images, so the control plane starts it on them rather than on the images it
// was last started with. The node is read fresh, because the desired images
// are its reconciler's to write and an operation's cached copy may predate them.
func (r *StorageNodeOpsReconciler) withDesiredImages(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, params RestartParams,
) (RestartParams, error) {
	node, err := r.node(ctx, ops)
	if err != nil {
		return params, err
	}
	params.SpdkImage = node.Status.DesiredSpdkImage
	params.SpdkProxyImage = node.Status.DesiredSpdkProxyImage
	return params, nil
}

// observeImages publishes whether the node is out of date. Unknown reads as
// not out of date: a gauge that fired for every node of a control plane that
// reports no images would be an alert nobody can act on.
func observeImages(node *simplyblockv1alpha2.StorageNode) {
	value := 0.0
	if node.Status.ImagesCurrent != nil && !*node.Status.ImagesCurrent {
		value = 1
	}
	imagesOutOfDate.WithLabelValues(node.Spec.ClusterRef, node.Name).Set(value)
}
