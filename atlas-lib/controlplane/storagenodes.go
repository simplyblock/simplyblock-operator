package controlplane

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/simplyblock/atlas/internal/cpapi"
)

// StorageNode is a simplyblock storage node in a cluster.
type StorageNode struct {
	ID              string
	ClusterID       string
	Hostname        string
	Status          string
	MgmtIP          string
	Lvols           int
	MaxLvols        int
	DeviceCount     int
	SecondaryNodeID string // empty when the node has no secondary
}

// NIC is a storage node's data network interface.
type NIC struct {
	ID      string
	Device  string
	Address string // IPv4 address
	NetType string // transport type, e.g. "tcp"
	Status  string
}

func storageNodeFromDTO(d cpapi.StorageNodeDTO) StorageNode {
	n := StorageNode{
		ID:          d.Id.String(),
		ClusterID:   d.ClusterId.String(),
		Hostname:    d.Hostname,
		Status:      string(d.Status),
		MgmtIP:      d.MgmtIp,
		Lvols:       d.Lvols,
		MaxLvols:    d.LvolsMax,
		DeviceCount: d.DeviceCount,
	}
	if d.SecondaryNodeId != nil {
		n.SecondaryNodeID = d.SecondaryNodeId.String()
	}
	return n
}

// ListStorageNodes returns every storage node in a cluster.
func (c *Client) ListStorageNodes(ctx context.Context, clusterID string) ([]StorageNode, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.ClustersStorageNodesListApiV2ClustersClusterIdStorageNodesGetWithResponse(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("list storage nodes in %s: %w", clusterID, err)
	}
	if resp.JSON200 == nil {
		return nil, respError("list storage nodes in "+clusterID, resp.StatusCode(), resp.Body)
	}
	out := make([]StorageNode, 0, len(*resp.JSON200))
	for _, d := range *resp.JSON200 {
		out = append(out, storageNodeFromDTO(d))
	}
	return out, nil
}

// GetStorageNode returns a single storage node by id. It wraps
// errs.ErrNotFound when the node does not exist.
func (c *Client) GetStorageNode(ctx context.Context, clusterID, nodeID string) (StorageNode, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return StorageNode{}, err
	}
	node, err := parseUUID("storage node id", nodeID)
	if err != nil {
		return StorageNode{}, err
	}
	resp, err := c.api.ClustersStorageNodesDetailApiV2ClustersClusterIdStorageNodesStorageNodeIdGetWithResponse(ctx, cluster, node)
	if err != nil {
		return StorageNode{}, fmt.Errorf("get storage node %s: %w", nodeID, err)
	}
	if resp.StatusCode() != 200 {
		return StorageNode{}, respError("storage node "+nodeID, resp.StatusCode(), resp.Body)
	}
	// Detail is untyped in the spec; the body is a StorageNodeDTO.
	var d cpapi.StorageNodeDTO
	if err := json.Unmarshal(resp.Body, &d); err != nil {
		return StorageNode{}, fmt.Errorf("get storage node %s: decode response: %w", nodeID, err)
	}
	return storageNodeFromDTO(d), nil
}

// ListStorageNodeNICs returns a storage node's data network interfaces.
func (c *Client) ListStorageNodeNICs(ctx context.Context, clusterID, nodeID string) ([]NIC, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return nil, err
	}
	node, err := parseUUID("storage node id", nodeID)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.ClustersStorageNodesNicsListApiV2ClustersClusterIdStorageNodesStorageNodeIdNicsGetWithResponse(ctx, cluster, node)
	if err != nil {
		return nil, fmt.Errorf("list NICs for node %s: %w", nodeID, err)
	}
	if resp.StatusCode() != 200 {
		return nil, respError("NICs for node "+nodeID, resp.StatusCode(), resp.Body)
	}
	// The /nics body is untyped in the spec; decode its documented shape.
	var raw []nicEntry
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return nil, fmt.Errorf("list NICs for node %s: decode response: %w", nodeID, err)
	}
	out := make([]NIC, 0, len(raw))
	for _, e := range raw {
		out = append(out, NIC{ID: e.ID, Device: e.Device, Address: e.Address, NetType: e.NetType, Status: e.Status})
	}
	return out, nil
}

// nicEntry mirrors the (untyped) /nics response element keys.
type nicEntry struct {
	ID      string `json:"ID"`
	Device  string `json:"Device name"`
	Address string `json:"Address"`
	NetType string `json:"Net type"`
	Status  string `json:"Status"`
}
