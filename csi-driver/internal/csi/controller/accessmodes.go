// The access modes CreateVolume refuses, before anything is created:
//
//   - every multi-node mode for a class whose volumes one host owns at a time:
//     a striped class, or a client-side compression or deduplication class,
//     which the node stages as LVM (and VDO);
//   - ReadOnlyMany for a filesystem on a block class, which the driver does not
//     mount on several hosts;
//   - ReadOnlyMany next to a writable mode, which Kubernetes would stage
//     read-only under the writer.
//
// ReadOnlyMany raw block is served: the node hands up a read-only dm-linear
// device (node/readonly.go).

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
// without an export (isPNFSRequest), a multi-node host-bound volume, and the
// ReadOnlyMany claims refuseReadOnlyMany names.
func checkCapabilities(req *csi.CreateVolumeRequest) (pnfs bool, err error) {
	pnfs, err = isPNFSRequest(req.GetVolumeCapabilities())
	if err != nil {
		return false, err
	}
	if err := refuseHostBoundMultiNode(req); err != nil {
		return false, err
	}
	return pnfs, refuseReadOnlyMany(req.GetVolumeCapabilities())
}

// refuseReadOnlyMany refuses a ReadOnlyMany filesystem, which would be a block
// filesystem mounted on several hosts (a pNFS class refuses ReadOnlyMany in
// isPNFSRequest), and ReadOnlyMany next to a writable mode.
func refuseReadOnlyMany(caps []*csi.VolumeCapability) error {
	readerOnly, writer := false, false
	for _, c := range caps {
		switch c.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
			if c.GetMount() != nil {
				return status.Error(codes.InvalidArgument,
					"ReadOnlyMany is supported for raw block (volumeMode: Block) only: "+
						"a filesystem on a block volume is not mounted on several hosts")
			}
			readerOnly = true
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
		default:
			writer = true
		}
	}
	if readerOnly && writer {
		return status.Error(codes.InvalidArgument,
			"ReadOnlyMany cannot be combined with a writable access mode: "+
				"the volume is staged read-only on every host")
	}
	return nil
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
