package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ClusterArbitration is the two-node arbiter's record of a cluster, as the
// control plane serves it on GET /api/v2/clusters/{id}/arbitration
// (sbcli docs/design/two-node-arbitration.md §7.2, §7.4).
type ClusterArbitration struct {
	ClusterID     string                 `json:"cluster_id"`
	Epoch         int64                  `json:"epoch"`
	State         string                 `json:"state"`
	PreferredNode string                 `json:"preferred_node"`
	LVS           []ArbitrationLVS       `json:"lvs"`
	Leases        map[string]interface{} `json:"leases,omitempty"`
}

// ArbitrationLVS is the arbiter's state of one logical volume store.
type ArbitrationLVS struct {
	JMVuid     int64  `json:"jm_vuid"`
	Leader     string `json:"leader"`
	State      string `json:"state"`
	FencedNode string `json:"fenced_node"`
	Since      int64  `json:"since"`
}

// Arbiter states that keep a fenced node fenced. In steady the arbiter has
// finished healing and the taint is removed (§8 step 5).
const (
	ArbitrationSteady      = "steady"
	ArbitrationDeciding    = "deciding"
	ArbitrationPartitioned = "partitioned"
	ArbitrationDegraded    = "degraded"
	ArbitrationHealing     = "healing"
)

// FencedNodes returns the control-plane node ids the arbiter currently holds
// fenced. A node stays fenced through healing: the arbiter clears the taint
// only once the cluster is steady again.
func (a *ClusterArbitration) FencedNodes() []string {
	if a == nil || a.State == ArbitrationSteady || a.State == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range a.LVS {
		if l.FencedNode != "" && !seen[l.FencedNode] {
			seen[l.FencedNode] = true
			out = append(out, l.FencedNode)
		}
	}
	return out
}

// GetClusterArbitration reads the arbiter's record for a cluster. A 404 means
// the control plane has no record (arbitration off or not yet initialised) and
// returns nil without error.
func (c *Client) GetClusterArbitration(ctx context.Context, clusterUUID string) (*ClusterArbitration, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/arbitration", clusterUUID)
	body, status, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("get arbitration for cluster %s: %w", clusterUUID, err)
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status >= 300 {
		return nil, fmt.Errorf("get arbitration for cluster %s: status %d: %s", clusterUUID, status, string(body))
	}
	var rec ClusterArbitration
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("decode arbitration for cluster %s: %w", clusterUUID, err)
	}
	return &rec, nil
}

// SetArbitrationPreferredNode tells the arbiter which storage node is the
// preferred one.
func (c *Client) SetArbitrationPreferredNode(ctx context.Context, clusterUUID, nodeUUID string) error {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/arbitration/preferred", clusterUUID)
	body, status, err := c.Do(ctx, http.MethodPut, endpoint, map[string]string{"node_id": nodeUUID})
	if err != nil {
		return fmt.Errorf("set preferred node for cluster %s: %w", clusterUUID, err)
	}
	if status >= 300 {
		return fmt.Errorf("set preferred node for cluster %s: status %d: %s", clusterUUID, status, string(body))
	}
	return nil
}
