//nolint:revive // csi spec
package csicommon

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

type DefaultNodeServer struct {
	Driver *CSIDriver
	csi.UnimplementedNodeServer
}

func (ns *DefaultNodeServer) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *DefaultNodeServer) NodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest,
) (*csi.NodeUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *DefaultNodeServer) NodeGetInfo(
	ctx context.Context,
	req *csi.NodeGetInfoRequest,
) (*csi.NodeGetInfoResponse, error) {
	klog.V(5).Infof("Using default NodeGetInfo")

	return &csi.NodeGetInfoResponse{
		NodeId: ns.Driver.nodeID,
	}, nil
}

func (ns *DefaultNodeServer) NodeGetCapabilities(
	ctx context.Context,
	req *csi.NodeGetCapabilitiesRequest,
) (*csi.NodeGetCapabilitiesResponse, error) {
	klog.V(5).Infof("Using default NodeGetCapabilities")

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_UNKNOWN,
					},
				},
			},
		},
	}, nil
}

func (ns *DefaultNodeServer) NodeGetVolumeStats(
	ctx context.Context,
	in *csi.NodeGetVolumeStatsRequest,
) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *DefaultNodeServer) NodeStageVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *DefaultNodeServer) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *DefaultNodeServer) NodeExpandVolume(
	ctx context.Context,
	req *csi.NodeExpandVolumeRequest,
) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}
