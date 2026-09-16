// The CSI GroupController service: crash-consistent snapshots of a
// consistency group through VolumeGroupSnapshot (design §9). The backend group
// is persistent and placement-pinned, so the driver does not snapshot whatever
// the selector matched; it resolves the group from the source volume handles,
// verifies the handle set equals the group's current membership, and takes one
// generation. Restore stays per member (§7), so there is no group-clone verb.
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/lvol"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog/v2"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// A group snapshot id is {clusterID}:{poolID}:{groupUUID}:{groupSeq}; its
// grammar lives in atlas (lvol.GroupSnapshotHandle) beside the volume handle,
// because both are read back out of Kubernetes objects by more than one
// consumer.

func lastPathSegment(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// GroupControllerGetCapabilities advertises the one capability this service
// implements: create, delete, and get a volume group snapshot.
func (cs *Server) GroupControllerGetCapabilities(
	_ context.Context,
	_ *csi.GroupControllerGetCapabilitiesRequest,
) (*csi.GroupControllerGetCapabilitiesResponse, error) {
	return &csi.GroupControllerGetCapabilitiesResponse{
		Capabilities: []*csi.GroupControllerServiceCapability{
			{
				Type: &csi.GroupControllerServiceCapability_Rpc{
					Rpc: &csi.GroupControllerServiceCapability_RPC{
						Type: csi.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT,
					},
				},
			},
		},
	}, nil
}

// CreateVolumeGroupSnapshot verifies the source handles equal the group's
// current membership, then takes one generation (design §9.2, §9.3).
func (cs *Server) CreateVolumeGroupSnapshot(
	ctx context.Context,
	req *csi.CreateVolumeGroupSnapshotRequest,
) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	name := req.GetName()
	sourceIDs := req.GetSourceVolumeIds()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot name is required")
	}
	if len(sourceIDs) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one source volume id is required")
	}
	klog.Infof("CreateVolumeGroupSnapshot: name=%s members=%d", name, len(sourceIDs))

	// Every member shares the group's placement, so the first handle names the
	// cluster and pool for all of them.
	first, err := csicommon.ParseVolumeHandle(sourceIDs[0])
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid source volume id %q: %v", sourceIDs[0], err)
	}
	handleByLvol := make(map[string]string, len(sourceIDs))
	for _, id := range sourceIDs {
		h, err := csicommon.ParseVolumeHandle(id)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid source volume id %q: %v", id, err)
		}
		handleByLvol[h.VolumeID] = id
	}

	sbclient, err := clusters.Client(ctx, first.ClusterID, first.PoolRef)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	// Resolve the group from a member volume, then verify the requested set
	// equals the group's current membership (§9.2). The webhook already checked
	// this at admission; this is the backstop for the window it admitted through.
	groupID, err := sbclient.GetVolumeGroupID(ctx, first.VolumeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to resolve consistency group: %v", err)
	}
	if groupID == "" {
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %s is not a consistency-group member", first.VolumeID)
	}
	members, err := sbclient.GetConsistencyGroupMembers(ctx, groupID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to read group membership: %v", err)
	}
	if !sameLvolSet(handleByLvol, members) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"selector resolves to %d volume(s) but consistency group has %d member(s); "+
				"the set must equal the group's current membership", len(handleByLvol), len(members))
	}

	gen, err := sbclient.TakeConsistencyGroupSnapshot(ctx, groupID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "group snapshot failed: %v", err)
	}

	return &csi.CreateVolumeGroupSnapshotResponse{
		GroupSnapshot: buildVolumeGroupSnapshot(first.ClusterID, first.PoolRef, groupID, gen, handleByLvol),
	}, nil
}

// DeleteVolumeGroupSnapshot deletes one generation and its member snapshots.
// A missing group or generation is success (design §9.3).
func (cs *Server) DeleteVolumeGroupSnapshot(
	ctx context.Context,
	req *csi.DeleteVolumeGroupSnapshotRequest,
) (*csi.DeleteVolumeGroupSnapshotResponse, error) {
	gsID := req.GetGroupSnapshotId()
	if gsID == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	parsed, ok := lvol.ParseGroupSnapshotHandle(gsID)
	if !ok {
		// An unparsable id names nothing to delete; treat as already gone.
		klog.Warningf("DeleteVolumeGroupSnapshot: invalid group snapshot id %q, treating as deleted", gsID)
		return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
	}
	klog.Infof("DeleteVolumeGroupSnapshot: group=%s gen=%d", parsed.GroupID, parsed.Seq)

	sbclient, err := clusters.Client(ctx, parsed.ClusterID, parsed.PoolRef)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if err := sbclient.DeleteConsistencyGroupGeneration(ctx, parsed.GroupID, parsed.Seq); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete group generation: %v", err)
	}
	return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
}

// GetVolumeGroupSnapshot reads one generation's readiness and member handles.
func (cs *Server) GetVolumeGroupSnapshot(
	ctx context.Context,
	req *csi.GetVolumeGroupSnapshotRequest,
) (*csi.GetVolumeGroupSnapshotResponse, error) {
	gsID := req.GetGroupSnapshotId()
	if gsID == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	parsed, ok := lvol.ParseGroupSnapshotHandle(gsID)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "invalid group snapshot id %q", gsID)
	}
	sbclient, err := clusters.Client(ctx, parsed.ClusterID, parsed.PoolRef)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	gen, err := sbclient.GetConsistencyGroupGeneration(ctx, parsed.GroupID, parsed.Seq)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to read group generation: %v", err)
	}
	// The source handles are reconstructed from the generation's members, since
	// Get carries no original selector.
	handleByLvol := make(map[string]string, len(gen.Members))
	for _, m := range gen.Members {
		handleByLvol[m.LvolID] = fmt.Sprintf("%s:%s:%s", parsed.ClusterID, parsed.PoolRef, m.LvolID)
	}
	groupRef := fmt.Sprintf("%s/%s", parsed.ClusterID, parsed.GroupID)
	return &csi.GetVolumeGroupSnapshotResponse{
		GroupSnapshot: buildVolumeGroupSnapshot(parsed.ClusterID, parsed.PoolRef, groupRef, gen, handleByLvol),
	}, nil
}

// buildVolumeGroupSnapshot assembles the CSI response from a backend
// generation. Each member snapshot gets the 3-part CSI id parseSnapshotID
// expects, mapped back to its source volume handle.
func buildVolumeGroupSnapshot(
	clusterID, poolID, groupID string,
	gen *controlplane.ConsistencyGroupGeneration,
	handleByLvol map[string]string,
) *csi.VolumeGroupSnapshot {
	groupSnapshotID := lvol.NewGroupSnapshotHandle(clusterID, poolID, lastPathSegment(groupID), gen.GroupSeq).String()
	creationTime := timestamppb.New(time.Unix(gen.CreatedAt, 0))

	snapshots := make([]*csi.Snapshot, 0, len(gen.Members))
	for _, m := range gen.Members {
		snapshots = append(snapshots, &csi.Snapshot{
			SnapshotId:      fmt.Sprintf("%s:%s:%s", clusterID, poolID, m.SnapshotID),
			SourceVolumeId:  handleByLvol[m.LvolID],
			GroupSnapshotId: groupSnapshotID,
			ReadyToUse:      m.Ready,
			CreationTime:    creationTime,
		})
	}
	return &csi.VolumeGroupSnapshot{
		GroupSnapshotId: groupSnapshotID,
		Snapshots:       snapshots,
		CreationTime:    creationTime,
		ReadyToUse:      gen.Complete,
	}
}

// sameLvolSet reports whether the requested source volumes (keyed by lvol UUID)
// are exactly the group's current members.
func sameLvolSet(requested map[string]string, members []string) bool {
	if len(requested) != len(members) {
		return false
	}
	for _, lvol := range members {
		if _, ok := requested[lastPathSegment(lvol)]; !ok {
			return false
		}
	}
	return true
}
