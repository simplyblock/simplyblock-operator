// Growing a volume from the controller side. The node side of the same
// operation, growing the filesystem on it, is in the node service.
package controller

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/clusters"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

func (cs *Server) ControllerExpandVolume(
	ctx context.Context,
	req *csi.ControllerExpandVolumeRequest,
) (*csi.ControllerExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.GetCapacityRange() == nil {
		return nil, status.Error(codes.InvalidArgument, "capacity range is required")
	}

	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()

	updatedSize := req.GetCapacityRange().GetRequiredBytes()

	// Simplyblock backends are GiB aligned, so we round up to GiB.
	capacityBytes := alignToGiBBytes(updatedSize)

	spdkVol, err := csicommon.ParseVolumeHandle(volumeID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume ID %q: %v", volumeID, err)
	}

	sbclient, err := clusters.Client(ctx, spdkVol.ClusterID, spdkVol.PoolRef)
	if err != nil {
		return nil, err
	}

	err = sbclient.ResizeVolume(ctx, spdkVol.VolumeID, capacityBytes)
	if err != nil {
		klog.Errorf("failed to resize lvol, LVolID: %s err: %v", spdkVol.VolumeID, err)
		return nil, classifyControllerExpandVolumeError(err)
	}
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         capacityBytes,
		NodeExpansionRequired: true,
	}, nil
}
