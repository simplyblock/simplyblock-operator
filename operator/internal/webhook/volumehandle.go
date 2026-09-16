// volumehandle.go resolves a PersistentVolume to its simplyblock volume
// handle for admission checks. The handle grammar and the PV ownership check
// live in atlas (lvol.ParseHandle, kube.VolumeHandleFromPV); this file only
// composes them with the client read the webhooks need.
package webhook

import (
	"context"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
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
	raw, err := kube.VolumeHandleFromPV(pv)
	if err != nil {
		// Not a volume this driver owns: undeterminable, not a failure.
		return "", "", "", false, nil
	}
	h, parsed := lvol.ParseHandle(raw)
	if !parsed {
		return "", "", "", false, nil
	}
	return h.ClusterID, h.PoolRef, h.VolumeID, true, nil
}
