package webapi

import (
	"encoding/json"
	"fmt"

	"github.com/simplyblock/simplyblock-operator/internal/sbnqn"
)

type ClusterResponse struct {
	UUID              string
	Secret            string
	NQN               sbnqn.ClusterNQN
	Status            string
	Rebalancing       bool
	NDCS              int
	NPCS              int
	MaxFaultTolerance int
}

type clusterResponsePayload struct {
	ID                string           `json:"id"`
	Secret            string           `json:"secret"`
	NQN               sbnqn.ClusterNQN `json:"nqn"`
	Status            string           `json:"status"`
	Rebalancing       bool             `json:"is_re_balancing"`
	NDCS              int              `json:"distr_ndcs"`
	NPCS              int              `json:"distr_npcs"`
	MaxFaultTolerance int              `json:"max_fault_tolerance"`
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
		NDCS:              payload.NDCS,
		NPCS:              payload.NPCS,
		MaxFaultTolerance: payload.MaxFaultTolerance,
	}, nil
}
