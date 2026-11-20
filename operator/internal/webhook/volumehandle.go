// volumehandle.go resolves a PersistentVolume to its simplyblock volume
// handle for admission checks. The handle grammar and the PV ownership check
// live in atlas (kube.NormalizedVolumeHandleFromPV, which applies §16.4's rule
// that a resolved handle recorded in an annotation is preferred to the legacy
// spelling the immutable field keeps); this file only composes them with the
// client read the webhooks need.
package webhook

import (
	"context"

	"github.com/simplyblock/atlas/kube"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// consistencyGroupLabel is the PVC label that names a volume's consistency group
// (design §4.1). The value is the group name a labeled volume joins at creation.
const consistencyGroupLabel = "storage.simplyblock.io/consistency-group"

// pvVolumeHandle reads a PV by name and returns its simplyblock CSI volume
// handle parts. ok is false for a PV that is not a simplyblock CSI volume, or
// whose handle does not parse; err reports only the client read failing.
func pvVolumeHandle(
	ctx context.Context, c client.Client, pvName string,
) (clusterUUID, poolRef, volumeUUID string, ok bool, err error) {
	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
		return "", "", "", false, err
	}
	normalized, err := kube.NormalizedVolumeHandleFromPV(pv)
	if err != nil {
		// Not a volume this driver owns, or one whose handle does not parse:
		// undeterminable, not a failure.
		return "", "", "", false, nil
	}
	h := normalized.Handle
	return h.ClusterID, h.PoolRef, h.VolumeID, true, nil
}
