package controller

import (
	"context"
	"fmt"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/csi-driver/internal/clusters"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// GetReplicationDestinationInfo answers csi-addons' destination-info call
// (capability GET_REPLICATION_DESTINATION_INFO, csi-addons/spec replication.proto,
// kubernetes-csi-addons >= v0.15.0).
//
// The csi-addons v0.15 controller lists every member of a
// VolumeGroupReplicationContent in status.persistentVolumeMappingList and fills
// each destinationVolumeHandle (and status.destinationVolumeGroupID) only from
// this call. Without it the destinations stay empty, and Ramen's restore on the
// target refuses the whole group: "destination volume ID is empty for VGRC …"
// (vrg_volgrouprep.go updateVGRCVolumeHandlesForRestore). That stopped the
// first consistency-group relocate on the DR test bed with the application
// demoted on the source and not started on the target (2026-10-03, WordPress
// site-a -> site-b). A single volume did not need it: Ramen skips a
// VolumeReplication without the destination-info condition.
//
// The destination of a simplyblock volume or group is addressed by its own
// handle. One control plane manages both clusters of a replication pair; every
// Replication verb resolves a handle through the replication relationship to
// the member it must act on (resolveChain, the group endpoints), and the PV
// keeps the original handle across every move -- the behaviour all relocate and
// fail-over cases were proven with. Answering with the replica's raw volume
// (the landing copy on the peer) would make Ramen rewrite the restored PV to a
// volume that a promote replaces with a clone, bypassing that resolution. So
// the answer is the source handle itself.
//
// A single volume's answer needs no lookup and never fails for a valid handle:
// once the capability is advertised csi-addons asks for every
// VolumeReplication too, and a failure sets DestinationInfoAvailable=False,
// which makes Ramen refuse the VRG instead of skipping it (vrg_volrep.go
// destinationInfoAvailableOrSkip) -- an error here would block protecting a
// volume that has not replicated yet. A group needs its current members for the
// complete map; a group the control plane cannot list is UNAVAILABLE
// (retryable).
func (cs *Server) GetReplicationDestinationInfo(
	ctx context.Context,
	req *replication.GetReplicationDestinationInfoRequest,
) (*replication.GetReplicationDestinationInfoResponse, error) {
	if g := req.GetReplicationSource().GetVolumegroup().GetVolumeGroupId(); g != "" {
		return groupDestinationInfo(ctx, g)
	}
	volumeID := req.GetReplicationSource().GetVolume().GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "replication source names no volume or volume group")
	}
	if _, err := csicommon.ParseVolumeHandle(volumeID); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &replication.GetReplicationDestinationInfoResponse{
		ReplicationDestination: &replication.ReplicationDestination{
			Type: &replication.ReplicationDestination_Volume{
				Volume: &replication.ReplicationDestination_VolumeDestination{VolumeId: volumeID},
			},
		},
	}, nil
}

// groupPVHandles is every handle a PersistentVolume of the group carries. It
// is resolved by the control plane (ResolveGroup): after a relocate the group a
// VGR names is empty -- its demoted members were deleted so a relocate back
// stays possible -- while its PVs keep their original handles, and listing the
// empty group left Ramen's VRG waiting for destination info for ever
// (2026-10-04, WordPress A -> B). The handles are the lineages' origins, the
// keys csi-addons matches; each maps to itself, the contract above. A control
// plane without the resolution endpoint is asked for the current members as
// before.
func groupPVHandles(ctx context.Context, client *atlascp.Client, gh lvol.GroupHandle) ([]lvol.VolumeHandle, error) {
	res, err := client.ResolveGroup(ctx, gh)
	if err != nil {
		return nil, err
	}
	if res.Legacy {
		return client.ConsistencyGroupMemberHandles(ctx, gh)
	}
	if len(res.Members) == 0 {
		return nil, fmt.Errorf("consistency group %s has no live member", gh.Handle())
	}
	handles := make([]lvol.VolumeHandle, 0, len(res.Members))
	for _, m := range res.Members {
		handles = append(handles, m.Origin)
	}
	return handles, nil
}

// groupDestinationInfo is the group branch: the group's own handle, and a
// complete source -> destination map over the group's current members (the spec
// forbids a partial map). The keys are the members' volume handles exactly as
// their PersistentVolumes carry them, which is what csi-addons matches them
// against.
func groupDestinationInfo(
	ctx context.Context, groupID string,
) (*replication.GetReplicationDestinationInfoResponse, error) {
	gh, ok := lvol.ParseGroupHandle(lvol.VolumeHandle(groupID))
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume group handle %q", groupID)
	}
	client, err := clusters.ReplicationClient(ctx, gh.ClusterID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	members, err := groupPVHandles(ctx, client, gh)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "members of %s: %v", groupID, err)
	}
	ids := make(map[string]string, len(members))
	for _, m := range members {
		ids[string(m)] = string(m)
	}
	return &replication.GetReplicationDestinationInfoResponse{
		ReplicationDestination: &replication.ReplicationDestination{
			Type: &replication.ReplicationDestination_Volumegroup{
				Volumegroup: &replication.ReplicationDestination_VolumeGroupDestination{
					VolumeGroupId: groupID,
					VolumeIds:     ids,
				},
			},
		},
	}, nil
}
