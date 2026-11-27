// Staging a ReadOnlyMany raw-block volume read-only: what marks the volume,
// and what refuses a stage that could not enforce it.
//
// The enforcement is the dm-linear mapping, loaded read-only, which is the
// device the pod is handed: the kernel fails every write to it, whichever pod
// opened it and however. A plan without the mapping has nothing to enforce it
// with, so the stage is refused rather than handing up a writable device.

package node

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/volstack/plans"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// readOnlyBlock reports whether a capability is a raw-block volume every
// consumer only reads. A ReadOnlyMany filesystem never gets here: CreateVolume
// refuses one outside pNFS, and the stage refuses one CreateVolume did not see
// (refuseReadOnlyManyFilesystem).
func readOnlyBlock(volCap *csi.VolumeCapability) bool {
	return volCap.GetBlock() != nil &&
		volCap.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
}

// refuseUnenforcedReadOnly refuses to stage a read-only volume on a plan that
// cannot make it read-only: one without the dm-linear indirection, because the
// node runs with SPDKCSI_DM_INDIRECTION off, the host has no device-mapper, or
// the volume was staged here before the indirection existed.
func refuseUnenforcedReadOnly(volume plans.Volume, shape stackShape) error {
	if !volume.ReadOnly || shape == shapeIndirectRawBlock {
		return nil
	}
	return status.Error(codes.FailedPrecondition,
		"a ReadOnlyMany raw-block volume is made read-only by the dm-linear indirection, "+
			"which this node does not stage it with (SPDKCSI_DM_INDIRECTION off, no device-mapper, "+
			"or a volume staged before the indirection existed)")
}

// refuseReadOnlyManyFilesystem refuses a ReadOnlyMany filesystem on a block
// volume, as CreateVolume does: mounted read-only on several hosts, it is a
// filesystem CRI-O cannot relabel for SELinux, so no pod would start on it. The
// node checks it again because a static or pre-existing volume reaches the
// stage without passing CreateVolume.
func refuseReadOnlyManyFilesystem(volCap *csi.VolumeCapability) error {
	if volCap.GetMount() == nil ||
		volCap.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
		return nil
	}
	return status.Error(codes.InvalidArgument,
		"ReadOnlyMany is supported for raw block (volumeMode: Block) only: "+
			"a filesystem on a block volume is not mounted on several hosts")
}
