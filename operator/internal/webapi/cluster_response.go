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
	// DataRebalancing is the control plane's is_data_rebalancing: device and
	// balancing tasks only, without the cluster's own volume migrations. Nil
	// when the control plane predates the field.
	DataRebalancing   *bool
	NDCS              int
	NPCS              int
	MaxFaultTolerance int
}

type clusterResponsePayload struct {
	ID                string `json:"id"`
	Secret            string `json:"secret"`
	NQN               string `json:"nqn"`
	Status            string `json:"status"`
	Rebalancing       bool   `json:"is_re_balancing"`
	DataRebalancing   *bool  `json:"is_data_rebalancing"`
	NDCS              int    `json:"distr_ndcs"`
	NPCS              int    `json:"distr_npcs"`
	MaxFaultTolerance int    `json:"max_fault_tolerance"`
}

// IsDataRebalancing reports whether the cluster is moving data by itself --
// device migrations, post-restart or post-expansion balancing -- as opposed
// to migrating volumes on a caller's behalf. Against a control plane that
// does not report the distinction it falls back to Rebalancing, the old,
// wider flag.
func (r ClusterResponse) IsDataRebalancing() bool {
	if r.DataRebalancing != nil {
		return *r.DataRebalancing
	}
	return r.Rebalancing
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
		DataRebalancing:   payload.DataRebalancing,
		NDCS:              payload.NDCS,
		NPCS:              payload.NPCS,
		MaxFaultTolerance: payload.MaxFaultTolerance,
	}, nil
}
