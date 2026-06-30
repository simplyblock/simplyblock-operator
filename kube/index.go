package kube

import (
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
)

// Index names registered on the shared informers by NewResolver. They are
// exported so a controller-runtime operator can register the same indexing
// on its manager cache via FieldIndexer and stay consistent with the CSI
// driver.
const (
	IndexPVByVolumeHandle = "atlas.simplyblock.io/pv-by-volume-handle"
	IndexVAByPV           = "atlas.simplyblock.io/va-by-pv"
)

// VolumeHandleKeys returns the index keys for a PV: its CSI volume handle
// if the PV is managed by this driver, otherwise none. It is the pure key
// function shared by the client-go indexer and any controller-runtime
// FieldIndexer registration.
func VolumeHandleKeys(pv *corev1.PersistentVolume) []string {
	if !IsManaged(pv) {
		return nil
	}
	return []string{pv.Spec.CSI.VolumeHandle}
}

// AttachmentPVKeys returns the index keys for a VolumeAttachment: the PV
// name it references, if any.
func AttachmentPVKeys(va *storagev1.VolumeAttachment) []string {
	if name, ok := AttachmentPVName(va); ok {
		return []string{name}
	}
	return nil
}
