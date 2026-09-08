// Growing a volume from the controller side. The node side of the same
// operation, growing the filesystem on it, is in the node service.
/*
Copyright (c) Arm Limited and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
