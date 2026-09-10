package webhook

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// consistencyGroupLabel is the PVC label that names a volume's consistency group
// (design §4.1). The value is the group name a labeled volume joins at creation.
const consistencyGroupLabel = "storage.simplyblock.io/consistency-group"

// splitCSIVolumeHandle splits a simplyblock CSI volume handle
// ({clusterUUID}:{poolUUID}:{volumeUUID}) into its parts. ok is false for a
// handle that is not this shape (e.g., a foreign driver's volume).
func splitCSIVolumeHandle(handle string) (clusterUUID, poolUUID, volumeUUID string, ok bool) {
	parts := strings.SplitN(handle, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// pvVolumeHandle reads a PV by name and returns its simplyblock CSI volume
// handle parts. ok is false for a PV that is not a simplyblock CSI volume.
func pvVolumeHandle(
	ctx context.Context, c client.Client, pvName string,
) (clusterUUID, poolUUID, volumeUUID string, ok bool, err error) {
	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
		return "", "", "", false, err
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return "", "", "", false, nil
	}
	clusterUUID, poolUUID, volumeUUID, ok = splitCSIVolumeHandle(pv.Spec.CSI.VolumeHandle)
	return clusterUUID, poolUUID, volumeUUID, ok, nil
}
