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

	// A pNFS client grows nothing and re-primes instead.
	//
	// Nothing to grow: the metadata server grew the filesystem on its own host,
	// and this node sees the new size through NFS. The plan here could not
	// answer anyway, since the filesystem it names is pnfs rather than anything
	// with a resize tool.
	//
	// The prime is the part that matters. Growing the namespace invalidates the
	// client's cached block device, and whoever causes the next I/O resolves it
	// again -- in their own mount namespace. A pod gets kubelet's minimal /dev
	// with no disk/, so the resolve fails, the fail bit is set, and every write
	// after it routes through the metadata server. Silently: the writes
	// succeed. This container has the host's /dev, and kubelet calls this RPC
	// on every node holding the volume, so it is where the race is won.
	if isPNFSVolume(volumeContext) {
		if err := primeLayout(ctx, getStagingTargetPath(req)); err != nil {
			return nil, status.Errorf(codes.Internal,
				"failed to re-prime the layout of volume %s after its expansion: %v", volumeID, err)
		}
		klog.Infof("volume %s is served by an export: grew nothing, and re-took its layout", volumeID)
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
