// Growing the filesystem on an already-staged volume. The controller side of
// the same operation, growing the volume itself, is in the controller service.
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

	volumeMountPath := req.GetVolumePath()

	stagingParentPath := req.GetStagingTargetPath()
	volumeContext, err := lookupVolumeContext(stagingParentPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to retrieve volume context for volume %s: %v", volumeID, err)
	}

	devicePath, ok := volumeContext["devicePath"]
	if !ok || devicePath == "" {
		return nil, status.Errorf(codes.Internal, "could not find device path for volume %s", volumeID)
	}

	// For raw block volumes, the block device has already been resized at the
	// storage layer, so neither resize tool should be invoked. resize2fs (ext4)
	// can operate on an unmounted raw device, which is why it worked by
	// accident. xfs_growfs requires a mounted filesystem path and cannot
	// operate on a raw block device at all.
	if cap := req.GetVolumeCapability(); cap != nil && cap.GetBlock() != nil {
		klog.Infof("NodeExpandVolume: volume %s is a block device, skipping filesystem resize", volumeID)
		return &csi.NodeExpandVolumeResponse{}, nil
	}

	needsResize, err := ns.mounter.NeedsResize(devicePath, volumeMountPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check if volume %s needs resizing: %v", volumeID, err)
	}

	if needsResize {
		resized, err := ns.mounter.Resize(devicePath, volumeMountPath)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to resize volume %s: %v", volumeID, err)
		}
		if resized {
			klog.Infof(
				"Successfully resized volume %s (device: %s, mount path: %s)",
				volumeID,
				devicePath,
				volumeMountPath,
			)
		} else {
			klog.Warningf("Volume %s did not require resizing", volumeID)
		}
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}
