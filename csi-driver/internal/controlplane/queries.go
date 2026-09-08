// The reads the node-side packages perform against the control plane, each as
// one method on ClusterClient.
//
// They live here rather than at their call sites because each was previously a
// raw client.API.do against a hand-built path, which only compiled while the
// initiator, the connection monitor, and the guardian shared this package's
// file. Splitting them out gave those callers a choice between exporting the
// transport or naming the question they are actually asking; this file is the
// second.
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// NodeInfo is the subset of a volume's control-plane record the connection
// monitor reads: which storage node serves the volume, which nodes the
// subsystem spans, and whether the volume is online.
type NodeInfo struct {
	NodeID string   `json:"storage_node_id"` // v2 VolumeDTO field
	Nodes  []string `json:"nodes"`           // URL paths in v2; converted to UUIDs after parsing
	Status string   `json:"status"`
}

// LvolConnections returns every NVMe-oF endpoint the control plane offers for
// lvolID, as authorized for hostNQN. The pool is resolved from the volume, so
// callers holding a cluster-scoped client need not know it.
//
// An empty response is an error: a volume with no endpoint cannot be attached,
// and reporting that as success would leave the caller connecting to nothing.
func (c *ClusterClient) LvolConnections(ctx context.Context, lvolID, hostNQN string) ([]*LvolConnectResp, error) {
	poolID, err := c.poolForVolume(ctx, lvolID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve pool for volume %s: %w", lvolID, err)
	}
	connections, err := c.API.getLvolConnections(ctx, poolID, lvolID, hostNQN)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch connection: %w", err)
	}
	if len(connections) == 0 {
		return nil, fmt.Errorf("empty connection response for volume %s", lvolID)
	}
	return connections, nil
}

// VolumeNodeInfo returns the storage-node placement of lvolID. The v2 API
// reports the subsystem's nodes as URL paths, which are reduced to bare UUIDs
// so callers compare identifiers rather than locations.
func (c *ClusterClient) VolumeNodeInfo(ctx context.Context, lvolID string) (*NodeInfo, error) {
	poolID, err := c.poolForVolume(ctx, lvolID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve pool for volume %s: %w", lvolID, err)
	}
	raw, err := c.API.do(ctx, http.MethodGet, c.API.v2volume(poolID, lvolID), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch node info: %w", err)
	}
	var info NodeInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal node info: %w", err)
	}
	// v2 nodes field returns URL paths; extract UUIDs from last path segment
	for i, n := range info.Nodes {
		info.Nodes[i] = locationToUUID(n)
	}
	return &info, nil
}

// StorageNodeStatus returns the control plane's status string for one storage
// node, lowercase as the API reports it.
func (c *ClusterClient) StorageNodeStatus(ctx context.Context, nodeID string) (string, error) {
	return c.API.getStorageNodeStatus(ctx, nodeID)
}

// ClusterStatus reports the cluster's own status, normalized to lowercase, and
// whether that status is one a volume may be served in. Both "active" and
// "degraded" are serving states: a degraded cluster has lost redundancy, not
// the ability to answer I/O.
func (c *ClusterClient) ClusterStatus(ctx context.Context) (serving bool, status string, err error) {
	raw, err := c.API.do(ctx, http.MethodGet, c.API.v2cluster(), nil)
	if err != nil {
		return false, "", err
	}

	var result ClusterStatus
	if err := json.Unmarshal(raw, &result); err != nil {
		return false, "", err
	}

	status = strings.ToLower(strings.TrimSpace(result.Status))
	return status == "active" || status == "degraded", status, nil
}
