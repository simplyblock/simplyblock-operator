package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ClusterArbitration is the two-node arbiter's record of a cluster, as the
// control plane serves it on GET /api/v2/clusters/{id}/arbitration
// (sbcli docs/design/two-node-arbitration.md §7.2, §7.4). The arbiter has no
// preferred node any more; an older control plane may still send
// preferred_node, which is ignored like every other unknown field.
type ClusterArbitration struct {
	ClusterID string                 `json:"cluster_id"`
	Epoch     int64                  `json:"epoch"`
	State     string                 `json:"state"`
	LVS       []ArbitrationLVS       `json:"lvs"`
	Leases    map[string]interface{} `json:"leases,omitempty"`
	// TaintRequests are the node ids the arbiter asks the operator to taint
	// as storage-fenced; it clears the list once healing is done.
	TaintRequests []string `json:"taint_requests"`
	// Enabled is the cluster's two_node_arbitration flag.
	Enabled bool `json:"enabled"`
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

// FencedNodes returns the control-plane node ids the arbiter asks the operator
// to taint (taint_requests). The arbiter keeps a node in the list through
// healing and clears it once the cluster is steady again.
func (a *ClusterArbitration) FencedNodes() []string {
	if a == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range a.TaintRequests {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
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
	// 404: no record; 409: not a two-node cluster, so there is none to follow.
	if status == http.StatusNotFound || status == http.StatusConflict {
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

// ReportArbitrationRemediation reports positive fencing evidence for a node
// (BMC fence done, node.kubernetes.io/out-of-service present), or clears it.
// The arbiter reads it as positive evidence that the node is down.
func (c *Client) ReportArbitrationRemediation(ctx context.Context, clusterUUID, nodeUUID string, fenced bool) error {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/arbitration/remediation", clusterUUID)
	body, status, err := c.Do(ctx, http.MethodPut, endpoint, map[string]any{"node_id": nodeUUID, "fenced": fenced})
	if err != nil {
		return fmt.Errorf("report remediation for cluster %s: %w", clusterUUID, err)
	}
	if status >= 300 {
		return fmt.Errorf("report remediation for cluster %s: status %d: %s", clusterUUID, status, string(body))
	}
	return nil
}
