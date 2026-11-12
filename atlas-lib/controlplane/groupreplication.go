// Group replication verbs on a consistency-group handle (design
// design-csi-addons-replication.md §14.4): the whole group replicates as one
// unit through the cluster-scoped /consistency-groups/{id}/replication/*
// endpoints, the group twins of the per-volume verbs in replication.go. The
// driver's Replication verbs route a cg: handle here (§14.4).
package controlplane

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/lvol"
)

// groupIDs parses a group handle into the cluster and group UUIDs the v2 path
// parameters require.
func groupIDs(gh lvol.GroupHandle) (cluster, group uuid.UUID, err error) {
	cluster, err = parseUUID("cluster id", gh.ClusterID)
	if err != nil {
		return
	}
	group, err = parseUUID("consistency group id", gh.GroupID)
	return
}

// EnableGroupReplication attaches the consistency group to a group replication
// policy, so the whole group replicates as one unit.
func (c *Client) EnableGroupReplication(ctx context.Context, gh lvol.GroupHandle, policyID string) error {
	cluster, group, err := groupIDs(gh)
	if err != nil {
		return err
	}
	policy, err := parseUUID("replication policy id", policyID)
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersConsistencyGroupsReplicationConfigureApiV2ClustersClusterIdConsistencyGroupsGroupIdReplicationPutWithResponse(
		ctx, cluster, group, cpapi.ConsistencyGroupReplicationIntentDTO{ReplicationPolicyId: &policy})
	if err != nil {
		return fmt.Errorf("enable group replication %s: %w", gh.Handle(), err)
	}
	if code := resp.StatusCode(); code != http.StatusOK && code != http.StatusNoContent {
		return respError("enable group replication "+string(gh.Handle()), code, resp.Body)
	}
	return nil
}

// DisableGroupReplication detaches the consistency group from its policy,
// stopping replication without dissolving the group.
func (c *Client) DisableGroupReplication(ctx context.Context, gh lvol.GroupHandle) error {
	cluster, group, err := groupIDs(gh)
	if err != nil {
		return err
	}
	// ReplicationPolicyId is not omitempty on the intent DTO, so a nil pointer
	// marshals as an explicit null. The backend reads that null as a detach,
	// distinct from an absent key.
	resp, err := c.api.ClustersConsistencyGroupsReplicationConfigureApiV2ClustersClusterIdConsistencyGroupsGroupIdReplicationPutWithResponse(
		ctx, cluster, group, cpapi.ConsistencyGroupReplicationIntentDTO{ReplicationPolicyId: nil})
	if err != nil {
		return fmt.Errorf("disable group replication %s: %w", gh.Handle(), err)
	}
	if code := resp.StatusCode(); code != http.StatusOK && code != http.StatusNoContent {
		return respError("disable group replication "+string(gh.Handle()), code, resp.Body)
	}
	return nil
}

// PromoteGroup fails the whole group over as one unit: every member is cloned
// from the same group-snapshot generation on the target, atomically.
func (c *Client) PromoteGroup(ctx context.Context, gh lvol.GroupHandle) error {
	cluster, group, err := groupIDs(gh)
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersConsistencyGroupsReplicationFailoverApiV2ClustersClusterIdConsistencyGroupsGroupIdReplicationFailoverPostWithResponse(
		ctx, cluster, group, nil)
	if err != nil {
		return fmt.Errorf("promote group %s: %w", gh.Handle(), err)
	}
	if code := resp.StatusCode(); code != http.StatusOK && code != http.StatusNoContent {
		return respError("promote group "+string(gh.Handle()), code, resp.Body)
	}
	return nil
}

// FailoverReplicatedGroup fails the group gh over onto local, a cluster of this
// control plane, from the replicated copies there. It is the promote for a
// split control plane, where gh names a cluster on another control plane that
// may be cut off: this control plane holds no record of that group, only the
// copies, and each copy carries what its clone needs. A 409 while no
// generation with a copy of every member is there, or while a member failed,
// is retryable.
func (c *Client) FailoverReplicatedGroup(ctx context.Context, local string, gh lvol.GroupHandle) error {
	localID, err := parseUUID("cluster id", local)
	if err != nil {
		return err
	}
	source, group, err := groupIDs(gh)
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersReplicationReplicatedGroupsFailoverApiV2ClustersClusterIdReplicationReplicatedGroupsSourceClusterIdGroupIdFailoverPostWithResponse(
		ctx, localID, source, group)
	if err != nil {
		return fmt.Errorf("fail over replicated group %s on %s: %w", gh.Handle(), local, err)
	}
	if code := resp.StatusCode(); code != http.StatusOK {
		return respError("fail over replicated group "+string(gh.Handle()), code, resp.Body)
	}
	return nil
}

// DemoteGroup demotes the whole group: quiesce every member, ship one final
// group snapshot, confirm, fence all. Re-drivable, not queued: done=false while
// any member is still converging, so the caller calls again.
func (c *Client) DemoteGroup(ctx context.Context, gh lvol.GroupHandle) (bool, error) {
	cluster, group, err := groupIDs(gh)
	if err != nil {
		return false, err
	}
	resp, err := c.api.ClustersConsistencyGroupsReplicationDemoteApiV2ClustersClusterIdConsistencyGroupsGroupIdReplicationDemotePostWithResponse(
		ctx, cluster, group, nil)
	if err != nil {
		return false, fmt.Errorf("demote group %s: %w", gh.Handle(), err)
	}
	switch code := resp.StatusCode(); code {
	case http.StatusNoContent:
		return true, nil
	case http.StatusAccepted:
		return false, nil
	default:
		return false, respError("demote group "+string(gh.Handle()), code, resp.Body)
	}
}

// ResyncGroup reverses the shipping direction for the whole group. It never cuts
// over, matching the per-volume ResyncVolume. sourceClusterID selects the source
// explicitly; "" leaves it unset.
func (c *Client) ResyncGroup(ctx context.Context, gh lvol.GroupHandle, sourceClusterID string) error {
	cluster, group, err := groupIDs(gh)
	if err != nil {
		return err
	}
	var body cpapi.GroupFailbackParams
	if sourceClusterID != "" {
		id, err := parseUUID("source cluster id", sourceClusterID)
		if err != nil {
			return err
		}
		body.SourceClusterId = &id
	}
	resp, err := c.api.ClustersConsistencyGroupsReplicationFailbackApiV2ClustersClusterIdConsistencyGroupsGroupIdReplicationFailbackPostWithResponse(
		ctx, cluster, group, body)
	if err != nil {
		return fmt.Errorf("resync group %s: %w", gh.Handle(), err)
	}
	if code := resp.StatusCode(); code != http.StatusOK && code != http.StatusNoContent {
		return respError("resync group "+string(gh.Handle()), code, resp.Body)
	}
	return nil
}

// GetGroupReplicationInfo returns the group's aggregate replication status
// (oldest recovery point, worst lag, and health; design §14.6), in the same
// shape as the per-volume status so the driver's Info verb maps it uniformly.
func (c *Client) GetGroupReplicationInfo(ctx context.Context, gh lvol.GroupHandle) (ReplicationStatus, error) {
	cluster, group, err := groupIDs(gh)
	if err != nil {
		return ReplicationStatus{}, err
	}
	resp, err := c.api.ClustersConsistencyGroupsReplicationStatusApiV2ClustersClusterIdConsistencyGroupsGroupIdReplicationStatusGetWithResponse(
		ctx, cluster, group, nil)
	if err != nil {
		return ReplicationStatus{}, fmt.Errorf("group replication status %s: %w", gh.Handle(), err)
	}
	d, err := payload("group replication status "+string(gh.Handle()), resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return ReplicationStatus{}, err
	}
	return ReplicationStatus{
		Role:             string(d.Role),
		State:            string(d.State),
		LastReplicatedAt: d.LastReplicatedAt,
		LagSeconds:       d.LagSeconds,
		OutstandingCount: derefInt(d.OutstandingCount),
		OutstandingBytes: derefInt(d.OutstandingBytes),
		Resyncing:        d.Resyncing != nil && *d.Resyncing,
	}, nil
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// GroupResolution is where a consistency group's data lives now, keyed by the
// handles its PersistentVolumes keep (sbcli GET
// /consistency-groups/{id}/replication/resolution).
type GroupResolution struct {
	// Active is the group holding live members: the group itself while it has
	// any, else its peer group of the same name on another cluster. Nil when no
	// group holds a live member.
	Active *lvol.GroupHandle
	// Members has one entry per protected volume with a live volume at the end
	// of its lineage: Origin is the handle its PV carries, Active the volume
	// serving the data now.
	Members []GroupMemberResolution
	// Legacy reports a control plane without the resolution endpoint (404): the
	// caller then treats the group as live where it is, as before.
	Legacy bool
}

// GroupMemberResolution maps one PV handle to the volume serving its data.
type GroupMemberResolution struct {
	Origin lvol.VolumeHandle
	Active lvol.VolumeHandle
}

// ResolveGroup resolves a group handle to where the group's data lives now.
//
// A VolumeGroupReplication keeps its original group handle across a relocate,
// while the group it names is emptied by design (its demoted members are
// deleted so a relocate back stays possible) and the data moves to the peer
// group as clones. The group verbs resolve the handle here, the group analogue
// of the per-volume relationship chain (2026-10-04: WordPress's VRG waited for
// destination info for ever against the emptied source group).
func (c *Client) ResolveGroup(ctx context.Context, gh lvol.GroupHandle) (GroupResolution, error) {
	cluster, group, err := groupIDs(gh)
	if err != nil {
		return GroupResolution{}, err
	}
	resp, err := c.api.ClustersConsistencyGroupsReplicationResolutionApiV2ClustersClusterIdConsistencyGroupsGroupIdReplicationResolutionGetWithResponse(
		ctx, cluster, group)
	if err != nil {
		return GroupResolution{}, fmt.Errorf("resolve group %s: %w", gh.Handle(), err)
	}
	if resp.StatusCode() == http.StatusNotFound && resp.JSON200 == nil && !groupNotFound(resp.Body) {
		return GroupResolution{Active: &gh, Legacy: true}, nil
	}
	d, err := payload("resolve group "+string(gh.Handle()), resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return GroupResolution{}, err
	}
	return groupResolution(d), nil
}

// ResolveReplicatedGroup is ResolveGroup for a group of a cluster on another
// control plane (a split control plane), answered by this site's cluster local
// from its own records: the group a fail-over made there from the copies, and
// each member's clone. Active is nil while the group was not failed over
// there, and the group's own control plane is the one to ask.
func (c *Client) ResolveReplicatedGroup(ctx context.Context, local string, gh lvol.GroupHandle) (GroupResolution, error) {
	localID, err := parseUUID("cluster id", local)
	if err != nil {
		return GroupResolution{}, err
	}
	source, group, err := groupIDs(gh)
	if err != nil {
		return GroupResolution{}, err
	}
	resp, err := c.api.ClustersReplicationReplicatedGroupsResolutionApiV2ClustersClusterIdReplicationReplicatedGroupsSourceClusterIdGroupIdResolutionGetWithResponse(
		ctx, localID, source, group)
	if err != nil {
		return GroupResolution{}, fmt.Errorf("resolve replicated group %s on %s: %w", gh.Handle(), local, err)
	}
	d, err := payload("resolve replicated group "+string(gh.Handle()), resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return GroupResolution{}, err
	}
	return groupResolution(d), nil
}

// groupResolution decodes the answer of either resolution endpoint.
func groupResolution(d *cpapi.ConsistencyGroupResolutionDTO) GroupResolution {
	out := GroupResolution{}
	if d.ActiveGroupId != nil && *d.ActiveGroupId != "" && d.ActiveClusterId != nil && *d.ActiveClusterId != "" {
		out.Active = &lvol.GroupHandle{ClusterID: *d.ActiveClusterId, GroupID: *d.ActiveGroupId}
	}
	if d.Members != nil {
		for _, m := range *d.Members {
			out.Members = append(out.Members, GroupMemberResolution{
				Origin: lvol.VolumeHandle(m.OriginHandle), Active: lvol.VolumeHandle(m.ActiveHandle)})
		}
	}
	return out
}

// groupNotFound tells the group-level 404 ("ConsistencyGroup <id> not found",
// the resource dependency's answer) from a route-level 404 (FastAPI's
// {"detail":"Not Found"} on a control plane that predates the endpoint).
func groupNotFound(body []byte) bool {
	return bytes.Contains(bytes.ToLower(body), []byte("consistencygroup"))
}
