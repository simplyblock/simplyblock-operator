package controlplane

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/ptr"
)

// ManagementNode is one control-plane host or installation of a cluster as
// the control plane records it.
type ManagementNode struct {
	ID       string
	Hostname string
	IP       string
	Status   string
	// Site is the control-plane site the node runs in, empty on a cluster
	// whose control plane is not stretched.
	Site string
}

// RegisterManagementNodeParams registers one site's control plane with a
// cluster whose control plane is stretched (stretched-control-plane.md): the
// site it runs in and the URL the other site's standby redirects mutating
// calls to.
type RegisterManagementNodeParams struct {
	Site        string
	APIEndpoint string
}

func managementNodeFromDTO(d cpapi.ManagementNodeDTO) ManagementNode {
	return ManagementNode{
		ID:       d.Id.String(),
		Hostname: d.Hostname,
		IP:       d.Ip,
		Status:   d.Status,
		Site:     ptr.From(d.Site, ""),
	}
}

// ManagementNodes lists the management nodes of a cluster.
func (c *Client) ManagementNodes(ctx context.Context, clusterID string) ([]ManagementNode, error) {
	id, err := uuid.Parse(clusterID)
	if err != nil {
		return nil, fmt.Errorf("cluster id %q: %w", clusterID, err)
	}
	resp, err := c.api.ManagementNodesListApiV2ManagementNodesGetWithResponse(ctx,
		&cpapi.ManagementNodesListApiV2ManagementNodesGetParams{ClusterId: id})
	if err != nil {
		return nil, fmt.Errorf("list management nodes: %w", err)
	}
	ds, err := payload("list management nodes", resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]ManagementNode, 0, len(*ds))
	for _, d := range *ds {
		out = append(out, managementNodeFromDTO(d))
	}
	return out, nil
}

// RegisterManagementNode registers the calling site's control plane with the
// cluster. The control plane answers 400 when the site is missing or not
// allowed and 409 when the site is already registered; both come back as a
// *StatusError.
func (c *Client) RegisterManagementNode(
	ctx context.Context, clusterID string, params RegisterManagementNodeParams,
) (ManagementNode, error) {
	id, err := uuid.Parse(clusterID)
	if err != nil {
		return ManagementNode{}, fmt.Errorf("cluster id %q: %w", clusterID, err)
	}
	resp, err := c.api.ManagementNodesCreateApiV2ManagementNodesPostWithResponse(ctx,
		&cpapi.ManagementNodesCreateApiV2ManagementNodesPostParams{ClusterId: id},
		cpapi.ManagementNodeParams{
			Site:        nonEmpty(params.Site),
			ApiEndpoint: nonEmpty(params.APIEndpoint),
		})
	if err != nil {
		return ManagementNode{}, fmt.Errorf("register management node: %w", err)
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		return ManagementNode{}, respError("register management node", resp.StatusCode(), resp.Body)
	}
	// The creation response is the full record by default; its schema is
	// untyped in the spec because the response format is negotiable.
	var d cpapi.ManagementNodeDTO
	if err := json.Unmarshal(resp.Body, &d); err != nil {
		return ManagementNode{}, fmt.Errorf("register management node: decode response: %w", err)
	}
	return managementNodeFromDTO(d), nil
}
