// Consistency-group reads the csi-addons VolumeGroup service needs: resolving the
// backend consistency group a set of member volumes already belongs to (design
// design-csi-addons-replication.md §14.3). A group is formed at provisioning by
// the storage.simplyblock.io/consistency-group label; this only reads it back.
package controlplane

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/simplyblock/atlas/internal/cpapi"
)

// ConsistencyGroupForLvols returns the id of the backend consistency group in
// clusterID whose current (open-epoch) membership is exactly lvolIDs.
//
// The members provisioned under one storage.simplyblock.io/consistency-group
// label share one backend group, so the VolumeGroup service resolves the group
// by its members rather than by the name it is handed (which is the caller's own
// generated name, not the group's). Returns an error when no group's membership
// matches exactly, so a partial or mixed selection is never silently grouped.
func (c *Client) ConsistencyGroupForLvols(ctx context.Context, clusterID string, lvolIDs []string) (string, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return "", err
	}
	if len(lvolIDs) == 0 {
		return "", fmt.Errorf("no volumes given to resolve a consistency group")
	}
	want := make(map[string]bool, len(lvolIDs))
	for _, id := range lvolIDs {
		want[id] = true
	}

	listResp, err := c.api.ClustersConsistencyGroupsListApiV2ClustersClusterIdConsistencyGroupsGetWithResponse(
		ctx, cluster, &cpapi.ClustersConsistencyGroupsListApiV2ClustersClusterIdConsistencyGroupsGetParams{})
	if err != nil {
		return "", fmt.Errorf("list consistency groups in cluster %s: %w", clusterID, err)
	}
	groups, err := payload("consistency groups in cluster "+clusterID,
		listResp.JSON200, listResp.StatusCode(), listResp.Body)
	if err != nil {
		return "", err
	}

	for _, g := range *groups {
		members, err := c.consistencyGroupMembers(ctx, cluster, g.Id)
		if err != nil {
			return "", err
		}
		if sameStringSet(members, want) {
			return g.Id.String(), nil
		}
	}
	return "", fmt.Errorf("no consistency group in cluster %s has exactly the %d requested member(s)",
		clusterID, len(lvolIDs))
}

// consistencyGroupMembers returns a group's current open-epoch member lvol ids.
func (c *Client) consistencyGroupMembers(ctx context.Context, cluster, group uuid.UUID) ([]string, error) {
	resp, err := c.api.ClustersConsistencyGroupsMembersApiV2ClustersClusterIdConsistencyGroupsGroupIdMembersGetWithResponse(
		ctx, cluster, group)
	if err != nil {
		return nil, fmt.Errorf("members of consistency group %s: %w", group, err)
	}
	dtos, err := payload("members of consistency group "+group.String(),
		resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}
	open := make([]string, 0, len(*dtos))
	for _, m := range *dtos {
		if m.RemovedSeq == 0 {
			open = append(open, m.LvolId)
		}
	}
	return open, nil
}

// sameStringSet reports whether ids is exactly the set want (same length, same
// elements), so a resolved group is neither a superset nor a subset of the
// requested members.
func sameStringSet(ids []string, want map[string]bool) bool {
	if len(ids) != len(want) {
		return false
	}
	for _, id := range ids {
		if !want[id] {
			return false
		}
	}
	return true
}
