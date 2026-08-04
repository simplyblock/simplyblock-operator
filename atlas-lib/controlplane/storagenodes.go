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
	ds, err := payload("list storage nodes in "+clusterID, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]StorageNode, 0, len(*ds))
	for _, d := range *ds {
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
	// Detail is untyped in the spec; the body is a StorageNodeDTO.
	d, err := decodeBody[cpapi.StorageNodeDTO]("storage node "+nodeID, resp.StatusCode(), resp.Body)
	if err != nil {
		return StorageNode{}, err
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
	// The /nics body is untyped in the spec; decode its documented shape.
	raw, err := decodeBody[[]nicEntry]("NICs for node "+nodeID, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]NIC, 0, len(raw))
	for _, e := range raw {
		out = append(out, NIC(e))
	}
	return out, nil
}

// nicEntry mirrors the (untyped) /nics response element keys. Being
// hand-written, it carries its constraints as `validate` struct tags rather
// than in cpapi's rule table, and validates itself on decode the same way the
// generated DTOs do.
type nicEntry struct {
	ID      string `json:"ID" validate:"required"`
	Device  string `json:"Device name" validate:"required"`
	Address string `json:"Address" validate:"omitempty,ip"`
	NetType string `json:"Net type" validate:"required"`
	Status  string `json:"Status" validate:"required"`
}

// UnmarshalJSON decodes and validates one /nics entry.
func (e *nicEntry) UnmarshalJSON(data []byte) error {
	type plain nicEntry // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*e = nicEntry(v)
	return cpapi.Validate(data, e)
}
