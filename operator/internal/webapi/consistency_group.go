// Consistency-group reads the admission webhooks need: resolve a group by name
// and list its current members (design §9.4, §10). Both are cluster-scoped.
package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ConsistencyGroupInfo is a group summary (design §10).
type ConsistencyGroupInfo struct {
	UUID        string `json:"id"`
	Name        string `json:"name"`
	MemberCount int    `json:"member_count"`
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
