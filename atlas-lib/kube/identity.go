package kube

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
)

// IsManaged reports whether pv is a CSI volume owned by this driver.
func IsManaged(pv *corev1.PersistentVolume) bool {
	return pv != nil && pv.Spec.CSI != nil && pv.Spec.CSI.Driver == DriverName
}

// PinnedNode returns the storage node a volume is pinned to, or "" if it is not
// pinned. The canonical AnnoSelectedStorageNode wins; for backward compatibility
// with PVCs created before it existed, the legacy AnnoHostID / DeprecatedAnnoHostID
// (whose original meaning was a hard placement) are honored as fallbacks. The
// one-shot AnnoPlacementHint is deliberately NOT a pin: hinted volumes stay
// eligible for rebalancing.
func PinnedNode(annotations map[string]string) string {
	if v := annotations[AnnoSelectedStorageNode]; v != "" {
		return v
	}
	if v := annotations[AnnoHostID]; v != "" {
		return v
	}
	return annotations[DeprecatedAnnoHostID]
}

// IsPinnedVolume reports whether the given (PVC) annotations pin a volume to a
// specific storage node — i.e. it must not be moved by auto-rebalancing and
// blocks a node drain. See PinnedNode for which annotations count.
func IsPinnedVolume(annotations map[string]string) bool {
	return PinnedNode(annotations) != ""
}

// VolumeHandleFromPV extracts the simplyblock logical-volume handle from a
// PV. It returns errs.ErrUnsupported if the PV is not a CSI volume owned by
// this driver.
func VolumeHandleFromPV(pv *corev1.PersistentVolume) (lvol.VolumeHandle, error) {
	if !IsManaged(pv) {
		return "", fmt.Errorf("pv %q: %w", pvName(pv), errs.ErrUnsupported)
	}
	return lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle), nil
}

// VolumeContextFromPV returns the CSI VolumeContext stored on the PV, or
// nil if the PV is not a managed CSI volume.
func VolumeContextFromPV(pv *corev1.PersistentVolume) map[string]string {
	if !IsManaged(pv) {
		return nil
	}
	return pv.Spec.CSI.VolumeAttributes
}

// ClaimRefFromPV returns the namespaced name of the PVC bound to pv and
// whether the PV is bound at all.
func ClaimRefFromPV(pv *corev1.PersistentVolume) (types.NamespacedName, bool) {
	if pv == nil || pv.Spec.ClaimRef == nil {
		return types.NamespacedName{}, false
	}
	ref := pv.Spec.ClaimRef
	return types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, true
}

func pvName(pv *corev1.PersistentVolume) string {
	if pv == nil {
		return "<nil>"
	}
	return pv.Name
}
