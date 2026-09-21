// Growing an already-staged volume onto the capacity it gained. The controller
// side of the same operation, growing the volume itself, is in the controller
// service and has already run by the time this does.
package node

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

func (ns *Server) NodeExpandVolume(
	ctx context.Context,
	req *csi.NodeExpandVolumeRequest,
) (*csi.NodeExpandVolumeResponse, error) {
	klog.Infof("NodeExpandVolume: called with args %+v", req)

	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	stagingParentPath := req.GetStagingTargetPath()
	volumeContext, err := lookupVolumeContext(stagingParentPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to retrieve volume context for volume %s: %v", volumeID, err)
	}

	// A pNFS client has nothing to grow: the metadata server grew the
	// filesystem on its own host, and this node sees the new size through NFS.
	// Its plan has no layer that could answer anyway, since the filesystem the
	// volume names here is pnfs rather than anything with a resize tool.
	if isPNFSVolume(volumeContext) {
		klog.Infof("volume %s is served by an export, so this node grows nothing", volumeID)
		return &csi.NodeExpandVolumeResponse{}, nil
	}

	plan, err := ns.attachPlan(ctx, volumeID, getStagingTargetPath(req), volumeContext, req.GetVolumeCapability())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to build the stack plan for volume %s: %v", volumeID, err)
	}

	// Bottom to top, skipping the layers that cannot grow. A raw block volume's
	// plan has none of those, so its expansion is a walk that changes nothing:
	// the block device was already resized at the storage layer, and neither
	// resize tool has anything to do with a device carrying no filesystem.
	//
	// Convergent, because kubelet reissues this after one that already
	// succeeded: both resize tools take the whole of what is now underneath them
	// and report success when that is where they already are.
	if err := ns.stack.runner.Grow(ctx, plan); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to grow volume %s: %v", volumeID, err)
	}

	klog.Infof("grew the stack of volume %s onto its new capacity", volumeID)
	return &csi.NodeExpandVolumeResponse{}, nil
}
