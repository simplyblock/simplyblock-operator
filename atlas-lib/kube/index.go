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
	IndexSCByName         = "atlas.simplyblock.io/sc-by-name"
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

// StorageClassNameKeys returns the index keys for a StorageClass: its name if
// the class is provisioned by this driver, otherwise none. Foreign classes get
// no key, so an informer resolver using this index never resolves them — only
// simplyblock-managed StorageClasses are surfaced. It is the pure key function
// shared by the client-go indexer and any controller-runtime FieldIndexer
// registration.
func StorageClassNameKeys(sc *storagev1.StorageClass) []string {
	if sc == nil || sc.Provisioner != DriverName {
		return nil
	}
	return []string{sc.Name}
}
