// Which SimplyblockDriver holds the deployment when more than one exists.
//
// The webhook denies a second object at admission and the reconciler refuses to
// act on one that got past it, and the two have to pick the same object out of
// the same list or they alternate between them. One function is what guarantees
// that, which is why this is exported rather than duplicated on each side.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §3.4.

package driver

import (
	"sort"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// DeploymentHolder is the oldest object in the list, with the namespace and the
// name breaking a tie. Age decides it rather than a lock, because the object
// that owns the running deployment is the one that has been there, and a tie
// needs a rule both callers apply the same way.
//
// The list must not be empty.
func DeploymentHolder(
	items []simplyblockv1alpha2.SimplyblockDriver,
) simplyblockv1alpha2.SimplyblockDriver {
	sorted := make([]simplyblockv1alpha2.SimplyblockDriver, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return sorted[0]
}
