// The read-only RPCs: what a volume is, and whether the driver can serve it the
// way a caller intends to use it.
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

func (cs *Server) ValidateVolumeCapabilities(
	ctx context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest,
) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}

	spdkVol, err := csicommon.ParseVolumeHandle(volumeID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "volume %q not found: %v", volumeID, err)
	}
	sbclient, err := clusters.Client(ctx, spdkVol.ClusterID, spdkVol.PoolRef)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if _, err := sbclient.VolumeInfo(ctx, spdkVol.VolumeID, ""); err != nil {
		return nil, classifyValidateVolumeCapabilitiesError(err)
	}

	// make sure we support all requested caps
	for _, cap := range req.GetVolumeCapabilities() {
		supported := false
		for _, accessMode := range cs.Driver.GetVolumeCapabilityAccessModes() {
			if cap.GetAccessMode().GetMode() == accessMode.GetMode() {
				supported = true
				break
			}
		}
		if !supported {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: ""}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}

func (cs *Server) ControllerGetVolume(
	ctx context.Context,
	req *csi.ControllerGetVolumeRequest,
) (*csi.ControllerGetVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()

	spdkVol, err := csicommon.ParseVolumeHandle(volumeID)
	if err != nil {
		return nil, err
	}

	sbclient, err := clusters.Client(ctx, spdkVol.ClusterID, spdkVol.PoolRef)
	if err != nil {
		klog.Errorf("failed to create spdk client: %v", err)
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	volumeInfo, err := sbclient.VolumeInfo(ctx, spdkVol.VolumeID, "")
	if err != nil {
		klog.Errorf("failed to get spdkVol for %s: %v", volumeID, err)

		return &csi.ControllerGetVolumeResponse{
			Volume: &csi.Volume{
				VolumeId: volumeID,
			},
			Status: &csi.ControllerGetVolumeResponse_VolumeStatus{
				VolumeCondition: &csi.VolumeCondition{
					Abnormal: true,
					Message:  err.Error(),
				},
			},
		}, nil
	}

	volume := &csi.Volume{
		VolumeId:      spdkVol.VolumeID,
		VolumeContext: volumeInfo,
	}

	return &csi.ControllerGetVolumeResponse{
		Volume: volume,
		Status: &csi.ControllerGetVolumeResponse_VolumeStatus{
			VolumeCondition: &csi.VolumeCondition{
				Abnormal: false,
				Message:  "",
			},
		},
	}, nil
}
