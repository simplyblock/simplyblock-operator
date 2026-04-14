package webapi

import (
	"encoding/json"
	"fmt"
)

type ClusterResponse struct {
	UUID        string
	Secret      string
	NQN         string
	Status      string
	Rebalancing bool
	NDCS        int
	NPCS        int
}

type clusterResponseEnvelope struct {
	Results json.RawMessage `json:"results"`
}

type clusterResponsePayload struct {
	UUID        string `json:"uuid"`
	ID          string `json:"id"`
	Secret      string `json:"secret"`
	NQN         string `json:"nqn"`
	Status      string `json:"status"`
	Rebalancing *bool  `json:"is_re_balancing"`
	NDCS        *int   `json:"distr_ndcs"`
	NPCS        *int   `json:"distr_npcs"`
}

func ParseClusterResponse(body []byte) (ClusterResponse, error) {
	raw := body

	var env clusterResponseEnvelope
	if err := json.Unmarshal(body, &env); err == nil && len(env.Results) > 0 {
		raw = env.Results
	}

	var payload clusterResponsePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ClusterResponse{}, err
	}

	resp := ClusterResponse{
		UUID:   payload.UUID,
		Secret: payload.Secret,
		NQN:    payload.NQN,
		Status: payload.Status,
	}

	if resp.UUID == "" {
		resp.UUID = payload.ID
	}

	if payload.Rebalancing != nil {
		resp.Rebalancing = *payload.Rebalancing
	}

	if payload.NDCS != nil && payload.NPCS != nil {
		resp.NDCS = *payload.NDCS
		resp.NPCS = *payload.NPCS
	}

	if resp.UUID == "" {
		return ClusterResponse{}, fmt.Errorf("cluster response missing id/uuid: %s", string(body))
	}

	return resp, nil
}
