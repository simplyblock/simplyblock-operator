// Capacity rounding. The control plane provisions in whole GiB, and a volume
// reported smaller than it was asked for fails the external provisioner's own
// check, so the create and the expand path round the same way.

package controller

import (
	"context"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvm"
)

const (
	mib = int64(1024 * 1024)
	gib = mib * 1024
)

// toGiB rounds up bytes to gigabytes
func toGiB(bytes int64) int64 {
	return (bytes + gib - 1) / gib
}

// alignToGiBBytes rounds bytes up to the next GiB boundary and returns bytes.
func alignToGiBBytes(bytes int64) int64 {
	return toGiB(bytes) * gib
}

// lvmBackedBytes is the size of a control-plane volume that leaves at least
// bytes to a logical volume LVM builds over it: bytes, plus what LVM keeps of a
// physical volume (lvm.PhysicalVolumeOverheadBytes), in whole GiB. A volume
// under an LVM stack reports bytes and is created at this, or its device would
// be short of what it reports.
func lvmBackedBytes(bytes int64) int64 {
	return alignToGiBBytes(bytes + lvm.PhysicalVolumeOverheadBytes)
}

// vdoBackedBytes is the size of the control-plane volume under a client-side
// compressed or deduplicated volume that reports bytes: bytes, plus what LVM
// keeps of the physical volume, plus what VDO keeps of its pool
// (lvm.VDOOverheadBytes), in whole GiB. The node creates the VDO volume at
// exactly bytes (csicommon.CtxVDOLogicalBytes), so the pool behind it holds
// all of it even when nothing compresses or deduplicates.
func vdoBackedBytes(bytes int64) int64 {
	return alignToGiBBytes(bytes + lvm.PhysicalVolumeOverheadBytes + lvm.VDOOverheadBytes(bytes))
}

// stagedUnderLVM reports whether the volume volumeID names is one the node
// stages under an LVM stack, a client-side compressed or deduplicated one, read
// from its PersistentVolume's attributes: an expansion receives no volume
// context. A PersistentVolume that cannot be read is answered false, which
// sizes the volume as an ordinary one, as every expansion did before.
func (cs *Server) stagedUnderLVM(ctx context.Context, volumeID string) bool {
	if cs.kubeClient == nil {
		return false
	}
	pvs, err := cs.kubeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Warningf("reading the PersistentVolume of %s to size its expansion: %v", volumeID, err)
		return false
	}
	for i := range pvs.Items {
		c := pvs.Items[i].Spec.CSI
		if c == nil || c.VolumeHandle != volumeID {
			continue
		}
		compression, _ := strconv.ParseBool(c.VolumeAttributes[kube.ParamClientCompression])
		deduplication, _ := strconv.ParseBool(c.VolumeAttributes[kube.ParamClientDeduplication])
		return compression || deduplication
	}
	return false
}
