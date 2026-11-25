// What CreateVolume refuses for a class whose volumes one host owns at a time:
// a striped class, or a client-side compression or deduplication class, which
// the node stages as LVM (and VDO). The limits come from LVM and VDO rather
// than from the control plane, so they are checked here, before anything is
// created.

package controller

import (
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/kube"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// checkCapabilities answers whether the requested capabilities ask for a pNFS
// export, and refuses the ones the class cannot serve: a multi-node filesystem
// without an export (isPNFSRequest) and a multi-node host-bound volume.
func checkCapabilities(req *csi.CreateVolumeRequest) (pnfs bool, err error) {
	pnfs, err = isPNFSRequest(req.GetVolumeCapabilities())
	if err != nil {
		return false, err
	}
	return pnfs, refuseHostBoundMultiNode(req)
}

// refuseHostBoundMultiNode refuses a multi-node access mode for a class that
// kube.HostBound names a reason for.
//
// LVM activates the volume group on the host that stages it, and VDO keeps its
// block map and deduplication index in that host's memory. A second host
// staging the same volume, as the target of a VM's live migration does, would
// activate the group again and run a second VDO over the same blocks, with
// nothing coordinating either between them.
func refuseHostBoundMultiNode(req *csi.CreateVolumeRequest) error {
	reason := kube.HostBound(req.GetParameters())
	if reason == "" {
		return nil
	}
	for _, c := range req.GetVolumeCapabilities() {
		switch mode := c.GetAccessMode().GetMode(); mode {
		case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			return status.Error(codes.InvalidArgument, fmt.Sprintf(
				"access mode %s is not supported for %s, which one host owns at a time", mode, reason))
		}
	}
	return nil
}
