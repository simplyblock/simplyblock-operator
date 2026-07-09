package kube

import (
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/lvol"
)

// Binding is the resolved cross-resource view of one logical volume: its
// identity plus the Kubernetes objects currently representing it. Zero
// values mean "not bound / not attached".
type Binding struct {
	VolumeHandle          lvol.VolumeHandle    // == PV.Spec.CSI.VolumeHandle
	PersistentVolumeName  string               // PersistentVolumeName name
	PersistentVolumeClaim types.NamespacedName // bound claim; zero if unbound
	Node                  string               // node it is attached to; empty if none
	Attached              bool                 // VolumeAttachment reports Status.Attached
}

// AttachmentNode returns the node a VolumeAttachment targets.
func AttachmentNode(va *storagev1.VolumeAttachment) string {
	if va == nil {
		return ""
	}
	return va.Spec.NodeName
}

// AttachmentPVName returns the PV name a VolumeAttachment references, if
// it is a PV-sourced attachment.
func AttachmentPVName(va *storagev1.VolumeAttachment) (string, bool) {
	if va == nil || va.Spec.Source.PersistentVolumeName == nil {
		return "", false
	}
	return *va.Spec.Source.PersistentVolumeName, true
}
