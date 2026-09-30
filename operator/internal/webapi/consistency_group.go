// Consistency-group reads the admission webhooks need: resolve a group by name
// and list its current members (design §9.4, §10). Both are cluster-scoped.
package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ConsistencyGroupInfo is a group summary (design §10). PolicyID is the group's
// replication policy: a group attached with attach_group_policy stores its policy
// on the group record, so the group drill reads the policy off the group rather
// than inferring it from the policy list's placement (which a group-first attach
// leaves empty). LvsName and NodeID carry the group's pinned placement.
type ConsistencyGroupInfo struct {
	UUID        string `json:"id"`
	Name        string `json:"name"`
	MemberCount int    `json:"member_count"`
	LvsName     string `json:"lvs_name"`
	NodeID      string `json:"node_id"`
	PolicyID    string `json:"policy_id"`
}

// ConsistencyGroupMember is one current member of a group (design §10 /members).
type ConsistencyGroupMember struct {
	LvolID string `json:"lvol_id"`
}

// GetConsistencyGroupByName resolves a group by its cluster-unique name, or
// returns nil when no such group exists.
func (c *Client) GetConsistencyGroupByName(
	ctx context.Context,
	clusterUUID, name string,
) (*ConsistencyGroupInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/consistency-groups/?name=%s", clusterUUID, name)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("get consistency group %q: %w", name, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("get consistency group %q: status %d: %s", name, statusCode, string(body))
	}
	var groups []ConsistencyGroupInfo
	if err := json.Unmarshal(body, &groups); err != nil {
		return nil, fmt.Errorf("unmarshal consistency group %q: %w", name, err)
	}
	for i := range groups {
		if groups[i].Name == name {
			return &groups[i], nil
		}
	}
	return nil, nil
}

// GetConsistencyGroupMembers returns the lvol ids currently in the group.
func (c *Client) GetConsistencyGroupMembers(
	ctx context.Context,
	clusterUUID, groupUUID string,
) ([]string, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/consistency-groups/%s/members", clusterUUID, groupUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("list group %q members: %w", groupUUID, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("list group %q members: status %d: %s", groupUUID, statusCode, string(body))
	}
	var members []ConsistencyGroupMember
	if err := json.Unmarshal(body, &members); err != nil {
		return nil, fmt.Errorf("unmarshal group %q members: %w", groupUUID, err)
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.LvolID)
	}
	return ids, nil
}

// GroupGenerationMember is one member's snapshot within a consistency-group
// generation, as cut or listed on the group's OWN cluster (the source). It is the
// in-place counterpart of ReplicatedGroupSnapshot, which lives on a DR target: the
// snapshot is on the source, in the member's own pool, so the caller pairs it to a
// member by lvol id and takes the pool from the member's source handle.
type GroupGenerationMember struct {
	LvolID     string `json:"lvol_id"`
	SnapshotID string `json:"snapshot_id"`
	Ready      bool   `json:"ready"`
}

// groupGenerationDTO is the wire shape of one consistency-group generation.
// created_at is unix seconds (the earliest member snapshot's creation time).
type groupGenerationDTO struct {
	GroupSeq  int                     `json:"group_seq"`
	CreatedAt int64                   `json:"created_at"`
	Complete  bool                    `json:"complete"`
	Members   []GroupGenerationMember `json:"members"`
}

// TakeGroupGeneration cuts one crash-consistent snapshot generation across every
// current member of the group on the group's OWN cluster, returning the generation
// number, its creation time (unix seconds), and each member's fresh snapshot. This
// is the in-place recovery point: the source's own newly cut generation, never a
// copy replicated to a peer.
func (c *Client) TakeGroupGeneration(
	ctx context.Context,
	clusterUUID, groupUUID string,
) (int, int64, []GroupGenerationMember, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/consistency-groups/%s/snapshots", clusterUUID, groupUUID)
	body, statusCode, err := c.Do(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("take group %q generation: %w", groupUUID, err)
	}
	if statusCode >= 300 {
		return 0, 0, nil, fmt.Errorf("take group %q generation: status %d: %s", groupUUID, statusCode, string(body))
	}
	var dto groupGenerationDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		return 0, 0, nil, fmt.Errorf("decode group generation: %w", err)
	}
	return dto.GroupSeq, dto.CreatedAt, dto.Members, nil
}

// LatestGroupGeneration returns the newest COMPLETE generation on the group's own
// cluster (every current member present), or found=false when none is complete.
// It reads the source's generations, not a target's replicated copies, so it is
// how the in-place path finds a generation an interrupted attempt already cut
// (ask-then-act) before cutting another.
func (c *Client) LatestGroupGeneration(
	ctx context.Context,
	clusterUUID, groupUUID string,
) (int, int64, []GroupGenerationMember, bool, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/consistency-groups/%s/snapshots", clusterUUID, groupUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, 0, nil, false, fmt.Errorf("list group %q generations: %w", groupUUID, err)
	}
	if statusCode >= 300 {
		return 0, 0, nil, false, fmt.Errorf("list group %q generations: status %d: %s", groupUUID, statusCode, string(body))
	}
	var gens []groupGenerationDTO
	if err := json.Unmarshal(body, &gens); err != nil {
		return 0, 0, nil, false, fmt.Errorf("decode group generations: %w", err)
	}
	best := -1
	for i := range gens {
		if gens[i].Complete && (best < 0 || gens[i].GroupSeq > gens[best].GroupSeq) {
			best = i
		}
	}
	if best < 0 {
		return 0, 0, nil, false, nil
	}
	return gens[best].GroupSeq, gens[best].CreatedAt, gens[best].Members, true, nil
}
