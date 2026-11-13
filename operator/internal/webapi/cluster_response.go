package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type ClusterResponse struct {
	UUID              string
	Secret            string
	NQN               string
	Status            string
	Rebalancing       bool
	Shrinking         bool
	NDCS              int
	NPCS              int
	MaxFaultTolerance int
	// SyncReplication, LostSite and LostSiteState are the sync-replication
	// facts: whether the cluster spans two sites, and the site the control
	// plane records as out with how far its drain or fence got.
	SyncReplication bool
	LostSite        string
	LostSiteState   string
	// LeaderSite is the site the cluster record names as its leader, empty
	// while unset (design-stretched-sync-replication-ramen.md §11.1).
	LeaderSite string
}

type clusterResponsePayload struct {
	ID                string `json:"id"`
	Secret            string `json:"secret"`
	NQN               string `json:"nqn"`
	Status            string `json:"status"`
	Rebalancing       bool   `json:"is_re_balancing"`
	Shrinking         bool   `json:"is_shrinking"`
	NDCS              int    `json:"distr_ndcs"`
	NPCS              int    `json:"distr_npcs"`
	MaxFaultTolerance int    `json:"max_fault_tolerance"`
	SyncReplication   bool   `json:"sync_replication"`
	LostSite          string `json:"lost_site"`
	LostSiteState     string `json:"lost_site_state"`
	LeaderSite        string `json:"leader_site"`
}

func ParseClusterResponse(body []byte) (ClusterResponse, error) {
	var payload clusterResponsePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return ClusterResponse{}, err
	}

	if payload.ID == "" {
		return ClusterResponse{}, fmt.Errorf("cluster response missing id: %s", string(body))
	}

	return ClusterResponse{
		UUID:              payload.ID,
		Secret:            payload.Secret,
		NQN:               payload.NQN,
		Status:            payload.Status,
		Rebalancing:       payload.Rebalancing,
		Shrinking:         payload.Shrinking,
		NDCS:              payload.NDCS,
		NPCS:              payload.NPCS,
		MaxFaultTolerance: payload.MaxFaultTolerance,
		SyncReplication:   payload.SyncReplication,
		LostSite:          payload.LostSite,
		LostSiteState:     payload.LostSiteState,
		LeaderSite:        payload.LeaderSite,
	}, nil
}

// GetCluster reads one cluster by its id, with the cluster secret the control
// plane returns to an administrator: the hub of a stretched cluster reads it to
// hand the follower site the credentials that adopt the cluster its leader
// created.
func (c *Client) GetCluster(ctx context.Context, clusterUUID string) (ClusterResponse, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/", clusterUUID)
	body, status, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ClusterResponse{}, fmt.Errorf("get cluster %s: %w", clusterUUID, err)
	}
	if status >= 300 {
		return ClusterResponse{}, fmt.Errorf("get cluster %s: status %d: %s", clusterUUID, status, string(body))
	}
	return ParseClusterResponse(body)
}
