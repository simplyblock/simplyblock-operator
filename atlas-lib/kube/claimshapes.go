// The claim shapes a StorageClass's volumes can take: which access modes, in
// which volume mode, and whether one can be the target of a clone. The CSI
// driver enforces these at CreateVolume, and the operator describes them to
// KubeVirt's CDI through a StorageProfile, so both read the same rule here.

package kube

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// ClaimShape is one access mode in one volume mode.
type ClaimShape struct {
	AccessMode corev1.PersistentVolumeAccessMode
	VolumeMode corev1.PersistentVolumeMode
}

// ClaimShapesFor is every shape a volume of a class with params can be claimed
// in, most preferred first.
//
// A pNFS class serves a filesystem to any number of hosts and has no raw
// device. Every other class is a block device, which a host-bound class (see
// HostBound) serves to one host at a time. The rest serve raw block to several
// hosts at once, and list that first: it is what CDI gives a claim that names
// no mode, and what a VM needs to live-migrate. They also serve raw block
// ReadOnlyMany, as a device the node makes read-only. A filesystem on a block
// device is never shared, read-only or not, because nothing coordinates two
// hosts mounting it.
func ClaimShapesFor(params map[string]string) []ClaimShape {
	rwoBlock := ClaimShape{AccessMode: corev1.ReadWriteOnce, VolumeMode: corev1.PersistentVolumeBlock}
	rwoFS := ClaimShape{AccessMode: corev1.ReadWriteOnce, VolumeMode: corev1.PersistentVolumeFilesystem}
	switch {
	case isPNFS(params):
		return []ClaimShape{
			{AccessMode: corev1.ReadWriteMany, VolumeMode: corev1.PersistentVolumeFilesystem},
			rwoFS,
		}
	case HostBound(params) != "":
		return []ClaimShape{rwoBlock, rwoFS}
	}
	return []ClaimShape{
		{AccessMode: corev1.ReadWriteMany, VolumeMode: corev1.PersistentVolumeBlock},
		rwoBlock,
		rwoFS,
		{AccessMode: corev1.ReadOnlyMany, VolumeMode: corev1.PersistentVolumeBlock},
	}
}

// HostBound names the class feature that ties a volume to the one host that
// stages it, or returns "" when the volume can be staged on several at once.
//
// A striped volume is an LVM volume group across its members, and a client-side
// compression or deduplication volume is one holding a VDO volume. LVM
// activates a group on the host that stages it, VDO keeps its maps in that
// host's memory, and nothing coordinates either between hosts.
func HostBound(params map[string]string) string {
	if width, err := strconv.Atoi(strings.TrimSpace(params[ParamStripeWidth])); err == nil && width > 1 {
		return "a striped volume"
	}
	if on, _ := BoolParam(params, ParamClientCompression, false); on {
		return "client-side compression"
	}
	if on, _ := BoolParam(params, ParamClientDeduplication, false); on {
		return "client-side deduplication"
	}
	return ""
}

// CanBeCloneTarget reports whether a volume of the class can be created from a
// snapshot or another volume. A pNFS export cannot: it would carry the source's
// filesystem, the export's assembly would skip the format, and the export would
// half-work.
func CanBeCloneTarget(params map[string]string) bool {
	return !isPNFS(params)
}

// isPNFS reports whether the class asks for a pNFS export.
func isPNFS(params map[string]string) bool {
	return params[ParamFSType] == FSTypePNFS
}
