// Group replication verbs on a consistency-group handle (design
// design-csi-addons-replication.md §14.4): the whole group replicates as one
// unit through the cluster-scoped /consistency-groups/{id}/replication/*
// endpoints, the group twins of the per-volume verbs in replication.go. The
// driver's Replication verbs route a cg: handle here (§14.4).
package controlplane

import (
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
		ctx, cluster, group)
	if err != nil {
		return fmt.Errorf("promote group %s: %w", gh.Handle(), err)
	}
	if code := resp.StatusCode(); code != http.StatusOK && code != http.StatusNoContent {
		return respError("promote group "+string(gh.Handle()), code, resp.Body)
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
		ctx, cluster, group)
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
		ctx, cluster, group)
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
