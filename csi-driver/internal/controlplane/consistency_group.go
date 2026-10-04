// Consistency-group control-plane calls: resolve a volume's group, read a
// group's live membership, and take, read, or delete a snapshot generation.
// These back the CSI GroupController service (design §9, §10). Unlike the
// volume and snapshot calls these endpoints are cluster-scoped, not
// pool-scoped, so their URL builders take no pool.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ConsistencyGroupMember is one current member of a group (§10 /members).
type ConsistencyGroupMember struct {
	LvolID string `json:"lvol_id"`
}

// ConsistencyGroupSummary is one group as the backend lists it (§10). Name is
// empty for a legacy policy-owned group, which is how the label watcher tells
// a label-managed group from one it must not touch (design §4.5).
type ConsistencyGroupSummary struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	NodeID       string `json:"node_id"`
	LvsName      string `json:"lvs_name"`
	MemberCount  int    `json:"member_count"`
	LastGroupSeq int    `json:"last_group_seq"`
}

// ErrMembershipRefused wraps a backend 409 on a member join: a precondition
// (placement pin, pool alignment, member cap, or the one-way rule) refused the
// join. It is a user-fixable state, not a transient fault, so callers surface
// it instead of hot-retrying (design §4.5).
var ErrMembershipRefused = errors.New("consistency-group membership refused")

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

func (client APIClient) v2consistencyGroups() string {
	return fmt.Sprintf("api/v2/clusters/%s/consistency-groups/", client.ClusterID)
}

func (client APIClient) v2consistencyGroup(gid string) string {
	return fmt.Sprintf("api/v2/clusters/%s/consistency-groups/%s/", client.ClusterID, gid)
}

func (client APIClient) v2consistencyGroupMembers(gid string) string {
	return fmt.Sprintf("api/v2/clusters/%s/consistency-groups/%s/members", client.ClusterID, gid)
}

func (client APIClient) v2consistencyGroupMember(gid, lvolID string) string {
	return fmt.Sprintf("api/v2/clusters/%s/consistency-groups/%s/members/%s", client.ClusterID, gid, lvolID)
}

func (client APIClient) v2consistencyGroupSnapshots(gid string) string {
	return fmt.Sprintf("api/v2/clusters/%s/consistency-groups/%s/snapshots", client.ClusterID, gid)
}

func (client APIClient) v2consistencyGroupSnapshot(gid string, seq int) string {
	return fmt.Sprintf("api/v2/clusters/%s/consistency-groups/%s/snapshots/%d", client.ClusterID, gid, seq)
}

// --- low-level API methods ---

func (client APIClient) listConsistencyGroups(ctx context.Context, name string) ([]ConsistencyGroupSummary, error) {
	path := client.v2consistencyGroups()
	if name != "" {
		path += "?name=" + url.QueryEscape(name)
	}
	raw, err := client.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var groups []ConsistencyGroupSummary
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, fmt.Errorf("unexpected response for consistency groups: %w", err)
	}
	return groups, nil
}

func (client APIClient) getConsistencyGroup(ctx context.Context, gid string) (*ConsistencyGroupSummary, error) {
	raw, err := client.do(ctx, http.MethodGet, client.v2consistencyGroup(gid), nil)
	if err != nil {
		return nil, err
	}
	var group ConsistencyGroupSummary
	if err := json.Unmarshal(raw, &group); err != nil {
		return nil, fmt.Errorf("unexpected response for consistency group: %w", err)
	}
	return &group, nil
}

func (client APIClient) joinConsistencyGroupMember(ctx context.Context, gid, lvolID string) error {
	body := map[string]string{"lvol_id": lvolID}
	_, err := client.do(ctx, http.MethodPost, client.v2consistencyGroupMembers(gid), body)
	if err != nil && isHTTPStatus(err, http.StatusConflict) {
		return fmt.Errorf("%w: %s", ErrMembershipRefused, err.Error())
	}
	return err
}

func (client APIClient) detachConsistencyGroupMember(ctx context.Context, gid, lvolID string) error {
	_, err := client.do(ctx, http.MethodDelete, client.v2consistencyGroupMember(gid, lvolID), nil)
	// A missing group is success: the detach's purpose (the volume is not a
	// member) already holds, matching the delete-generation semantics above.
	if err != nil && isHTTPStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

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

// ResolveConsistencyGroupByName resolves a group by its label value, returning
// nil when no group of that name exists yet: a group is born from its first
// provisioned labeled volume, never by the watcher (design §4.1, §4.5).
func (c *ClusterClient) ResolveConsistencyGroupByName(
	ctx context.Context, name string,
) (*ConsistencyGroupSummary, error) {
	groups, err := c.API.listConsistencyGroups(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}
	return &groups[0], nil
}

// GetConsistencyGroup reads one group's summary (design §10).
func (c *ClusterClient) GetConsistencyGroup(
	ctx context.Context, groupID string,
) (*ConsistencyGroupSummary, error) {
	return c.API.getConsistencyGroup(ctx, groupUUID(groupID))
}

// JoinConsistencyGroupMember joins an EXISTING volume to the group (design
// §4.5). A backend precondition refusal is returned as ErrMembershipRefused;
// joining a current member is idempotent success.
func (c *ClusterClient) JoinConsistencyGroupMember(ctx context.Context, groupID, lvolID string) error {
	return c.API.joinConsistencyGroupMember(ctx, groupUUID(groupID), lvolID)
}

// DetachConsistencyGroupMember closes a member's epoch one-way, preserving
// prior generations (design §8.2); detaching a non-member is success.
func (c *ClusterClient) DetachConsistencyGroupMember(ctx context.Context, groupID, lvolID string) error {
	return c.API.detachConsistencyGroupMember(ctx, groupUUID(groupID), lvolID)
}
