// The csi-addons Replication service: EnableVolumeReplication,
// DisableVolumeReplication, and GetVolumeReplicationInfo (design §5.1). Each
// verb is a thin adapter onto the atlas-lib control-plane client's
// replication calls, resolved through the same {clusterID}:{poolID}:{lvolID}
// handle every other RPC uses. The remaining Replication verbs
// (PromoteVolume, DemoteVolume, ResyncVolume) fall through to the embedded
// UnimplementedControllerServer until Phase 2.
package controller

import (
	"context"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/simplyblock/csi-driver/internal/clusters"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// replicationPolicyParam is the VolumeReplicationClass parameter key naming
// the backend policy to attach. It is spelled as an id rather than the
// design's own `replicationPolicy` (a name, §7.1), because resolving a name
// to an id needs a policy-list-and-match call this phase does not yet wrap
// in atlas-lib. A future change adds that resolution and accepts either.
const replicationPolicyParam = "replicationPolicyID"

// EnableVolumeReplication attaches the volume to the policy named by the
// VolumeReplicationClass. Attaching to the policy the volume already follows
// is success (the backend's own idempotency, P0-2).
//
// The design (§5.1, §10) wants a different-policy attach refused with
// FAILED_PRECONDITION, because a silent re-attach forces a full re-sync. That
// refusal is NOT implemented here: it needs the volume's CURRENTLY attached
// policy id to compare against, and neither the P0-1 status read nor any
// other backend endpoint exposes it today (attach_policy in sbcli's
// replication_policy_controller.py detaches and re-attaches on a policy
// change without refusing). Until the backend adds that field, a
// different-policy attach silently re-syncs, exactly as it does through
// every other existing caller of this same endpoint.
func (cs *Server) EnableVolumeReplication(
	ctx context.Context,
	req *replication.EnableVolumeReplicationRequest,
) (*replication.EnableVolumeReplicationResponse, error) {
	policyID := req.GetParameters()[replicationPolicyParam]
	if policyID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "VolumeReplicationClass parameter %q is required", replicationPolicyParam)
	}
	h, err := csicommon.ParseVolumeHandle(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	client, err := clusters.ReplicationClient(ctx, h.ClusterID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if err := client.EnableVolumeReplication(ctx, h.Handle(), policyID); err != nil {
		return nil, classifyEnableVolumeReplicationError(err)
	}
	return &replication.EnableVolumeReplicationResponse{}, nil
}

// DisableVolumeReplication detaches the volume from whatever policy it
// follows. Detaching an already-detached volume is success; a cutover in
// flight (409) is a retryable ABORTED, since Ramen re-drives every reconcile.
func (cs *Server) DisableVolumeReplication(
	ctx context.Context,
	req *replication.DisableVolumeReplicationRequest,
) (*replication.DisableVolumeReplicationResponse, error) {
	h, err := csicommon.ParseVolumeHandle(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	client, err := clusters.ReplicationClient(ctx, h.ClusterID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if err := client.DisableVolumeReplication(ctx, h.Handle()); err != nil {
		return nil, classifyDisableVolumeReplicationError(err)
	}
	return &replication.DisableVolumeReplicationResponse{}, nil
}

// GetVolumeReplicationInfo returns the volume's replicated-life status.
//
// The spec's response carries only LastSyncTime in this version
// (github.com/csi-addons/spec v0.2.0); lastSyncBytes/lastSyncDuration are not
// yet part of the wire contract this driver builds against, so they cannot
// be set even though the backend status read already computes them (design
// §6.1). Ramen's condition derivation (§6.2) does not depend on them.
func (cs *Server) GetVolumeReplicationInfo(
	ctx context.Context,
	req *replication.GetVolumeReplicationInfoRequest,
) (*replication.GetVolumeReplicationInfoResponse, error) {
	h, err := csicommon.ParseVolumeHandle(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	client, err := clusters.ReplicationClient(ctx, h.ClusterID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	info, err := client.GetVolumeReplicationInfo(ctx, h.Handle())
	if err != nil {
		return nil, classifyGetVolumeReplicationInfoError(err)
	}
	resp := &replication.GetVolumeReplicationInfoResponse{}
	if info.LastReplicatedAt != nil {
		resp.LastSyncTime = timestamppb.New(*info.LastReplicatedAt)
	}
	return resp, nil
}
