// What CreateVolume refuses for a client-side compression or deduplication
// class. The node stages such a volume as an LVM volume group holding a VDO
// volume, so the limits come from LVM and VDO rather than from the control
// plane, and they are checked here, before anything is created.

package controller

import (
	"fmt"
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/kube"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// checkCapabilities answers whether the requested capabilities ask for a pNFS
// export, and refuses the ones the class cannot serve: a multi-node filesystem
// without an export (isPNFSRequest) and a multi-node VDO volume.
func checkCapabilities(req *csi.CreateVolumeRequest) (pnfs bool, err error) {
	pnfs, err = isPNFSRequest(req.GetVolumeCapabilities())
	if err != nil {
		return false, err
	}
	return pnfs, refuseVDOMultiNode(req)
}

// refuseVDOMultiNode refuses a multi-node access mode for a class that asks for
// client-side compression or deduplication.
//
// LVM activates the volume group on the host that stages it, and VDO keeps its
// block map and deduplication index in that host's memory. A second host
// staging the same volume, as the target of a VM's live migration does, would
// activate the group again and run a second VDO over the same blocks, with
// nothing coordinating either between them.
func refuseVDOMultiNode(req *csi.CreateVolumeRequest) error {
	params := req.GetParameters()
	compression, _ := strconv.ParseBool(params[kube.ParamClientCompression])
	deduplication, _ := strconv.ParseBool(params[kube.ParamClientDeduplication])
	if !compression && !deduplication {
		return nil
	}
	for _, c := range req.GetVolumeCapabilities() {
		switch mode := c.GetAccessMode().GetMode(); mode {
		case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			return status.Error(codes.InvalidArgument, fmt.Sprintf(
				"access mode %s is not supported with client-side compression or deduplication: "+
					"the volume is staged as LVM and VDO, which one host owns at a time", mode))
		}
	}
	return nil
}
