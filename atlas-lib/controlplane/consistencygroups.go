// Consistency-group reads: the backend consistency group a set of member
// volumes already belongs to, which the csi-addons VolumeGroup service needs
// (design-csi-addons-replication.md §14.3), and the group formed under a label,
// which placement across a supercluster's members needs
// (design-superclusters.md §7.2). A group is formed at provisioning by the
// storage.simplyblock.io/consistency-group label; this only reads it back.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/lvol"
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

// ConsistencyGroupByName returns the composite id of the consistency group named
// `name` in clusterID, the id a backup-policy attachment addresses the group by.
//
// A group's name is unique per cluster, so the name and the cluster identify it.
// Returns an error wrapping ErrNotFound when the cluster has no group by that
// name, which the caller reconciles by waiting rather than failing: a group is
// born from its first labeled member, so a policy can name one before it exists.
func (c *Client) ConsistencyGroupByName(ctx context.Context, clusterID, name string) (string, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return "", err
	}
	listResp, err := c.api.ClustersConsistencyGroupsListApiV2ClustersClusterIdConsistencyGroupsGetWithResponse(
		ctx, cluster, &cpapi.ClustersConsistencyGroupsListApiV2ClustersClusterIdConsistencyGroupsGetParams{Name: &name})
	if err != nil {
		return "", fmt.Errorf("list consistency groups in cluster %s: %w", clusterID, err)
	}
	groups, err := payload("consistency groups in cluster "+clusterID,
		listResp.JSON200, listResp.StatusCode(), listResp.Body)
	if err != nil {
		return "", err
	}
	for _, g := range *groups {
		if g.Name == name {
			return g.ClusterId.String() + "/" + g.Id.String(), nil
		}
	}
	return "", fmt.Errorf("no consistency group named %q in cluster %s: %w",
		name, clusterID, errs.ErrNotFound)
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

// ConsistencyGroupMemberHandles returns the volume handles
// ("<cluster>:<pool>:<lvol>") of a consistency group's current (open-epoch)
// members, in the order the control plane lists them.
//
// The member listing carries bare lvol ids. Every member of one group lives in
// one storage pool (the backend refuses a cross-pool join), so the pool is
// resolved once, from the first member, by probing the cluster's pools. An
// empty group is an error: a caller mapping members (csi-addons destination
// info) must never return an empty map as if it were complete.
func (c *Client) ConsistencyGroupMemberHandles(
	ctx context.Context, gh lvol.GroupHandle,
) ([]lvol.VolumeHandle, error) {
	cluster, err := parseUUID("cluster id", gh.ClusterID)
	if err != nil {
		return nil, err
	}
	group, err := parseUUID("consistency group id", gh.GroupID)
	if err != nil {
		return nil, err
	}
	members, err := c.consistencyGroupMembers(ctx, cluster, group)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("consistency group %s has no current members", gh)
	}
	pools, err := c.ListStoragePools(ctx, gh.ClusterID)
	if err != nil {
		return nil, err
	}
	poolID := ""
	for _, p := range pools {
		h := lvol.Handle{ClusterID: gh.ClusterID, PoolRef: p.ID, VolumeID: members[0]}
		_, err := c.Volume(ctx, h.Handle())
		if err == nil {
			poolID = p.ID
			break
		}
		if !errors.Is(err, errs.ErrNotFound) {
			return nil, err
		}
	}
	if poolID == "" {
		return nil, fmt.Errorf("member %s of consistency group %s is in none of the %d pools of cluster %s: %w",
			members[0], gh, len(pools), gh.ClusterID, errs.ErrNotFound)
	}
	out := make([]lvol.VolumeHandle, 0, len(members))
	for _, m := range members {
		out = append(out, lvol.Handle{ClusterID: gh.ClusterID, PoolRef: poolID, VolumeID: m}.Handle())
	}
	return out, nil
}

// groupUUID is the UUID a consistency-group endpoint keys on. Callers carry the
// group as a bare UUID or as the "<cluster>/<UUID>" composite ConsistencyGroupByName
// returns, so the last path segment is what is parsed.
func groupUUID(groupID string) (uuid.UUID, error) {
	if i := strings.LastIndex(groupID, "/"); i >= 0 {
		groupID = groupID[i+1:]
	}
	return parseUUID("consistency group id", groupID)
}

// TakeGroupSnapshot takes one crash-consistent generation across every current
// member of a consistency group and returns its sequence, the number a group
// backup of that generation is asked for by (CreateGroupBackup).
//
// requestID makes the take idempotent: the control plane stamps it on the
// generation, and a retry with the same id is answered with that generation
// instead of a new one. A caller that may lose the answer passes an id it can
// derive again, such as its own object's UID. Empty takes a new generation
// every time.
func (c *Client) TakeGroupSnapshot(ctx context.Context, clusterID, groupID, requestID string) (int, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return 0, err
	}
	group, err := groupUUID(groupID)
	if err != nil {
		return 0, err
	}
	what := "take a generation of consistency group " + groupID
	body := cpapi.UnderscoreGenerationTakeParams{}
	if requestID != "" {
		body.RequestId = &requestID
	}
	resp, err := c.api.ClustersConsistencyGroupsSnapshotsTakeApiV2ClustersClusterIdConsistencyGroupsGroupIdSnapshotsPostWithResponse(
		ctx, cluster, group, body)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	generation, err := payload(what, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return 0, err
	}
	return generation.GroupSeq, nil
}

// CreatedGroupBackup is a group backup the control plane accepted: its id, and
// how many member backups it fanned out, which is how many must complete before
// the group commits.
type CreatedGroupBackup struct {
	ID          string
	MemberCount int
}

// CreateGroupBackup backs up one existing generation of a consistency group to
// the cluster's store as one all-or-nothing unit. The generation is always
// named: taking it is TakeGroupSnapshot's, so a caller that records the sequence
// between the two calls never takes a second generation on a retry.
//
// The call returns once every member's upload is accepted. The group commits
// when every member completes, which the backup stream reports per member.
func (c *Client) CreateGroupBackup(ctx context.Context, clusterID, groupID string, groupSeq int) (CreatedGroupBackup, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return CreatedGroupBackup{}, err
	}
	group, err := groupUUID(groupID)
	if err != nil {
		return CreatedGroupBackup{}, err
	}
	what := fmt.Sprintf("back up generation %d of consistency group %s", groupSeq, groupID)
	resp, err := c.api.ClustersBackupsGroupsCreateApiV2ClustersClusterIdBackupsGroupsPostWithResponse(
		ctx, cluster, cpapi.UnderscoreGroupBackupCreateParams{ConsistencyGroupId: group, GroupSeq: &groupSeq})
	if err != nil {
		return CreatedGroupBackup{}, fmt.Errorf("%s: %w", what, err)
	}
	created, err := payload(what, resp.JSON201, resp.StatusCode(), resp.Body)
	if err != nil {
		return CreatedGroupBackup{}, err
	}
	return CreatedGroupBackup{ID: created.Id.String(), MemberCount: len(created.MemberBackupIds)}, nil
}

// ConsistencyGroup is a backend consistency group as the cluster lists it.
type ConsistencyGroup struct {
	ID   string
	Name string
	// MemberCount is the number of volumes in the group now.
	MemberCount int
}

// ConsistencyGroupNamed resolves the group formed under a claim label in
// clusterID, with its member count, and whether there is one. It is how a placement across the
// members of a supercluster finds the member that already holds a group.
func (c *Client) ConsistencyGroupNamed(
	ctx context.Context, clusterID, name string,
) (ConsistencyGroup, bool, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return ConsistencyGroup{}, false, err
	}
	resp, err := c.api.ClustersConsistencyGroupsListApiV2ClustersClusterIdConsistencyGroupsGetWithResponse(
		ctx, cluster, &cpapi.ClustersConsistencyGroupsListApiV2ClustersClusterIdConsistencyGroupsGetParams{Name: &name})
	if err != nil {
		return ConsistencyGroup{}, false, fmt.Errorf("find consistency group %q in cluster %s: %w", name, clusterID, err)
	}
	groups, err := payload("consistency groups in cluster "+clusterID, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return ConsistencyGroup{}, false, err
	}
	for _, g := range *groups {
		if g.Name == name {
			return ConsistencyGroup{ID: g.Id.String(), Name: g.Name, MemberCount: g.MemberCount}, true, nil
		}
	}
	return ConsistencyGroup{}, false, nil
}
