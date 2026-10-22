// Predicates over the stretched-cluster fields, kept beside the types so every
// controller and webhook asks the same question the same way rather than each
// re-deriving it from the pointers.

package v1alpha2

// IsStretchedFollower reports whether the cluster is the following site of a
// stretched cluster, whose operator adopts the backend cluster rather than
// creating it, never deletes it, and runs no cluster-wide operation.
func (c *StorageCluster) IsStretchedFollower() bool {
	return c.Spec.Stretched != nil && c.Spec.Stretched.Role == StretchedRoleFollower
}

// StretchedSite is the backend site of the Kubernetes cluster this StorageCluster
// lives on, empty when it is not one site of a stretched cluster.
func (c *StorageCluster) StretchedSite() string {
	if c.Spec.Stretched == nil {
		return ""
	}
	return c.Spec.Stretched.Site
}

// IsRemote reports whether the node is the other site's node, listed on this
// cluster from the control plane: it runs no workload here, admits no
// operation, and is managed by its own site's operator.
func (n *StorageNode) IsRemote() bool {
	return n.Spec.Stretched != nil && n.Spec.Stretched.Remote
}

// IsClusterWide reports whether the action acts on the backend cluster as a
// whole, which on a stretched cluster only its leader site runs. A task cancel
// targets one control-plane task the backend owns, so it is not cluster-wide.
func (a StorageClusterOpsAction) IsClusterWide() bool {
	switch a {
	case StorageClusterOpsActionActivate, StorageClusterOpsActionExpand,
		StorageClusterOpsActionShutdown, StorageClusterOpsActionStart,
		StorageClusterOpsActionRestart, StorageClusterOpsActionRollingRestart:
		return true
	default:
		return false
	}
}
