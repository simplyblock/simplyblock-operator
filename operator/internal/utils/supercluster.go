// What a supercluster's members are, read the same way by every controller
// that acts on them (design-superclusters.md §4, §10.1): the supercluster
// reconciler rolls them up, and the pool controller creates a backend pool in
// each. Both need the same answer to which clusters are members and which of
// them accept new volumes, so it is answered here once.

package utils

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// IsServingPhase reports whether a cluster in this phase serves I/O.
func IsServingPhase(phase simplyblockv1alpha2.StorageClusterPhase) bool {
	switch phase {
	case simplyblockv1alpha2.StorageClusterPhaseOnline,
		simplyblockv1alpha2.StorageClusterPhaseDegraded,
		simplyblockv1alpha2.StorageClusterPhaseRebalancing,
		simplyblockv1alpha2.StorageClusterPhaseShrinking:
		return true
	}
	return false
}

// AcceptsPlacement reports whether a member may be given new volumes as far as
// the member itself says: it serves I/O and placement is not disabled on it.
// The supercluster adds that the member holds every pool of the supercluster.
func AcceptsPlacement(member *simplyblockv1alpha2.StorageCluster) bool {
	if member.Spec.DisablePlacement != nil && *member.Spec.DisablePlacement {
		return false
	}
	return IsServingPhase(member.Status.Phase)
}

// SuperclusterMembers lists the clusters naming the supercluster in
// spec.superclusterRef, in name order. It filters a namespace list rather than
// reading the supercluster reconciler's field index, so a controller that
// registers no index can call it.
func SuperclusterMembers(
	ctx context.Context, reader client.Reader, supercluster *simplyblockv1alpha2.StorageCluster,
) ([]simplyblockv1alpha2.StorageCluster, error) {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := reader.List(ctx, &clusters, client.InNamespace(supercluster.Namespace)); err != nil {
		return nil, fmt.Errorf("list the members of supercluster %s/%s: %w",
			supercluster.Namespace, supercluster.Name, err)
	}
	members := make([]simplyblockv1alpha2.StorageCluster, 0, len(clusters.Items))
	for i := range clusters.Items {
		if clusters.Items[i].Spec.SuperclusterRef == supercluster.Name {
			members = append(members, clusters.Items[i])
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	return members, nil
}
