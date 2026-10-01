// The csi-addons VolumeGroup (GroupController) service (design
// design-csi-addons-replication.md §14.3): the stock kubernetes-csi-addons
// controller-manager dials it to form a backend consistency group before
// replicating it as one unit. CreateVolumeGroup resolves the group the member
// volumes already belong to (they joined at provisioning by the
// storage.simplyblock.io/consistency-group label) and hands back a group handle
// the Replication verbs route on. Membership and lifecycle are owned by that
// label, not by this service, so ModifyVolumeGroupMembership and
// DeleteVolumeGroup never reshape or delete the backend group.
package controller

import (
	"context"

	"github.com/csi-addons/spec/lib/go/volumegroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/lvol"

	"github.com/simplyblock/csi-driver/internal/clusters"
)

// volumeGroupUnimplemented embeds the csi-addons VolumeGroup unimplemented server
// so Server can satisfy the service's forward-compat guard under a name that does
// not collide with the replication base's own UnimplementedControllerServer.
type volumeGroupUnimplemented struct {
	volumegroup.UnimplementedControllerServer
}

// CreateVolumeGroup resolves the backend consistency group the given member
// volumes belong to and returns its group handle (design §14.3). It creates no
// group of its own: the group is label-formed at provisioning, so this is
// idempotent and returns the existing group's handle.
func (cs *Server) CreateVolumeGroup(
	ctx context.Context,
	req *volumegroup.CreateVolumeGroupRequest,
) (*volumegroup.CreateVolumeGroupResponse, error) {
	clusterID, lvolIDs, err := parseGroupMembers(req.GetVolumeIds())
	if err != nil {
		return nil, err
	}
	client, err := clusters.ReplicationClient(ctx, clusterID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	groupID, err := client.ConsistencyGroupForLvols(ctx, clusterID, lvolIDs)
	if err != nil {
		// No backend group matches the selection exactly: the members are not
		// one whole consistency group (design §14.3, the admission webhook's
		// invariant), so refuse rather than group a partial set.
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	gh := lvol.GroupHandle{ClusterID: clusterID, GroupID: groupID}
	return &volumegroup.CreateVolumeGroupResponse{
		VolumeGroup: &volumegroup.VolumeGroup{VolumeGroupId: string(gh.Handle())},
	}, nil
}

// ModifyVolumeGroupMembership is a success no-op: a group's membership is owned
// by the storage.simplyblock.io/consistency-group label at provisioning, and
// dynamic membership through this RPC is deferred (design §14.3,
// design-consistency-groups.md Phase 4). Returning success also lets the
// controller-manager's teardown, which empties a group before deleting it,
// proceed without error.
func (cs *Server) ModifyVolumeGroupMembership(
	_ context.Context,
	req *volumegroup.ModifyVolumeGroupMembershipRequest,
) (*volumegroup.ModifyVolumeGroupMembershipResponse, error) {
	if _, ok := lvol.ParseGroupHandle(lvol.VolumeHandle(req.GetVolumeGroupId())); !ok {
		return nil, status.Errorf(codes.InvalidArgument,
			"not a consistency-group handle: %q", req.GetVolumeGroupId())
	}
	return &volumegroup.ModifyVolumeGroupMembershipResponse{
		VolumeGroup: &volumegroup.VolumeGroup{VolumeGroupId: req.GetVolumeGroupId()},
	}, nil
}

// DeleteVolumeGroup is a success no-op: the backend consistency group is
// label-formed and lives as long as a labeled member exists, so deleting the
// csi-addons grouping never deletes the backend group or its volumes (design
// §14.3). Idempotent.
func (cs *Server) DeleteVolumeGroup(
	_ context.Context,
	req *volumegroup.DeleteVolumeGroupRequest,
) (*volumegroup.DeleteVolumeGroupResponse, error) {
	if _, ok := lvol.ParseGroupHandle(lvol.VolumeHandle(req.GetVolumeGroupId())); !ok {
		return nil, status.Errorf(codes.InvalidArgument,
			"not a consistency-group handle: %q", req.GetVolumeGroupId())
	}
	return &volumegroup.DeleteVolumeGroupResponse{}, nil
}

// parseGroupMembers parses the member volume handles of a CreateVolumeGroup
// request into a shared cluster id and the member lvol ids, rejecting a
// malformed handle or members that span clusters.
func parseGroupMembers(volumeIDs []string) (clusterID string, lvolIDs []string, err error) {
	if len(volumeIDs) == 0 {
		return "", nil, status.Error(codes.InvalidArgument,
			"CreateVolumeGroup requires at least one volume")
	}
	lvolIDs = make([]string, 0, len(volumeIDs))
	for _, vid := range volumeIDs {
		h, ok := lvol.ParseHandle(lvol.VolumeHandle(vid))
		if !ok {
			return "", nil, status.Errorf(codes.InvalidArgument, "malformed volume handle %q", vid)
		}
		if clusterID != "" && h.ClusterID != clusterID {
			return "", nil, status.Error(codes.InvalidArgument,
				"a volume group's members must all live in one cluster")
		}
		clusterID = h.ClusterID
		lvolIDs = append(lvolIDs, h.VolumeID)
	}
	return clusterID, lvolIDs, nil
}
