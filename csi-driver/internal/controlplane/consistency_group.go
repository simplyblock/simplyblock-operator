// Consistency-group control-plane calls: resolve a volume's group, read a
// group's live membership, and take, read, or delete a snapshot generation.
// These back the CSI GroupController service (design §9, §10). Unlike the
// volume and snapshot calls these endpoints are cluster-scoped, not
// pool-scoped, so their URL builders take no pool.
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ConsistencyGroupMember is one current member of a group (§10 /members).
type ConsistencyGroupMember struct {
	LvolID string `json:"lvol_id"`
}

// GenerationMember is one member's snapshot within a generation.
type GenerationMember struct {
	LvolID     string `json:"lvol_id"`
	SnapshotID string `json:"snapshot_id"`
	Ready      bool   `json:"ready"`
}

// ConsistencyGroupGeneration is one snapshot generation of a group (§6.3).
type ConsistencyGroupGeneration struct {
	GroupSeq  int                `json:"group_seq"`
	CreatedAt int64              `json:"created_at"`
	Expected  int                `json:"expected"`
	Present   int                `json:"present"`
	Complete  bool               `json:"complete"`
	Members   []GenerationMember `json:"members"`
}

// groupUUID reduces a group id (the backend spells it "clusterID/uuid") to the
// bare UUID the cluster-scoped endpoints take as a path segment.
func groupUUID(groupID string) string {
	if i := strings.LastIndex(groupID, "/"); i >= 0 {
		return groupID[i+1:]
	}
	return groupID
}

// --- URL builders (cluster-scoped) ---

func (client APIClient) v2consistencyGroupMembers(gid string) string {
	return fmt.Sprintf("api/v2/clusters/%s/consistency-groups/%s/members", client.ClusterID, gid)
}

func (client APIClient) v2consistencyGroupSnapshots(gid string) string {
	return fmt.Sprintf("api/v2/clusters/%s/consistency-groups/%s/snapshots", client.ClusterID, gid)
}

func (client APIClient) v2consistencyGroupSnapshot(gid string, seq int) string {
	return fmt.Sprintf("api/v2/clusters/%s/consistency-groups/%s/snapshots/%d", client.ClusterID, gid, seq)
}

// --- low-level API methods ---

func (client APIClient) getConsistencyGroupMembers(ctx context.Context, gid string) ([]ConsistencyGroupMember, error) {
	raw, err := client.do(ctx, http.MethodGet, client.v2consistencyGroupMembers(gid), nil)
	if err != nil {
		return nil, err
	}
	var members []ConsistencyGroupMember
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, fmt.Errorf("unexpected response for group members: %w", err)
	}
	return members, nil
}

func (client APIClient) takeConsistencyGroupSnapshot(
	ctx context.Context, gid string,
) (*ConsistencyGroupGeneration, error) {
	raw, err := client.do(ctx, http.MethodPost, client.v2consistencyGroupSnapshots(gid), nil)
	if err != nil {
		return nil, err
	}
	var gen ConsistencyGroupGeneration
	if err := json.Unmarshal(raw, &gen); err != nil {
		return nil, fmt.Errorf("unexpected response for group snapshot: %w", err)
	}
	return &gen, nil
}

func (client APIClient) getConsistencyGroupGeneration(
	ctx context.Context, gid string, seq int,
) (*ConsistencyGroupGeneration, error) {
	raw, err := client.do(ctx, http.MethodGet, client.v2consistencyGroupSnapshot(gid, seq), nil)
	if err != nil {
		return nil, err
	}
	var gen ConsistencyGroupGeneration
	if err := json.Unmarshal(raw, &gen); err != nil {
		return nil, fmt.Errorf("unexpected response for group generation: %w", err)
	}
	return &gen, nil
}

func (client APIClient) deleteConsistencyGroupGeneration(ctx context.Context, gid string, seq int) error {
	_, err := client.do(ctx, http.MethodDelete, client.v2consistencyGroupSnapshot(gid, seq), nil)
	// A missing group or generation is success: a group deleted with its last
	// member leaves VolumeGroupSnapshot objects that later delete against
	// nothing (design §9.3).
	if err != nil && isHTTPStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (client APIClient) getVolumeGroupID(ctx context.Context, poolID, lvolID string) (string, error) {
	raw, err := client.do(ctx, http.MethodGet, client.v2volume(poolID, lvolID), nil)
	if err != nil {
		return "", err
	}
	var vol struct {
		GroupID string `json:"group_id"`
	}
	if err := json.Unmarshal(raw, &vol); err != nil {
		return "", fmt.Errorf("unexpected response for volume: %w", err)
	}
	return vol.GroupID, nil
}

// --- ClusterClient wrappers ---

// GetVolumeGroupID returns the consistency-group id a volume belongs to, or ""
// for a non-member (design §10). The migration webhook and the GroupController
// use it to resolve the group from a volume handle.
func (c *ClusterClient) GetVolumeGroupID(ctx context.Context, lvolID string) (string, error) {
	return c.API.getVolumeGroupID(ctx, c.poolID, lvolID)
}

// GetConsistencyGroupMembers returns the lvol ids currently in the group.
func (c *ClusterClient) GetConsistencyGroupMembers(ctx context.Context, groupID string) ([]string, error) {
	members, err := c.API.getConsistencyGroupMembers(ctx, groupUUID(groupID))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.LvolID)
	}
	return ids, nil
}

// TakeConsistencyGroupSnapshot takes one crash-consistent generation across
// every current member and returns it (design §5, §9.3).
func (c *ClusterClient) TakeConsistencyGroupSnapshot(
	ctx context.Context, groupID string,
) (*ConsistencyGroupGeneration, error) {
	return c.API.takeConsistencyGroupSnapshot(ctx, groupUUID(groupID))
}

// GetConsistencyGroupGeneration reads one generation's readiness and member
// snapshot handles (design §6.3).
func (c *ClusterClient) GetConsistencyGroupGeneration(
	ctx context.Context, groupID string, seq int,
) (*ConsistencyGroupGeneration, error) {
	return c.API.getConsistencyGroupGeneration(ctx, groupUUID(groupID), seq)
}

// DeleteConsistencyGroupGeneration deletes one generation and all its member
// snapshots; a missing generation is treated as success (design §9.3).
func (c *ClusterClient) DeleteConsistencyGroupGeneration(ctx context.Context, groupID string, seq int) error {
	return c.API.deleteConsistencyGroupGeneration(ctx, groupUUID(groupID), seq)
}
