package webapi

import (
	"encoding/json"
	"fmt"
)

// BackendWorkloadNames are a cluster's workload names as the control plane
// records them: sent when the cluster is created and returned when it is read.
// A control plane that predates them returns none, which it treats as the
// constant names, and so does Names.
type BackendWorkloadNames struct {
	SnodeAPIService    string `json:"snode_api_service,omitempty"`
	SpdkProxyService   string `json:"spdk_proxy_service,omitempty"`
	SpdkProxyTLSSecret string `json:"spdk_proxy_tls_secret,omitempty"`
	NodeLabelSuffix    string `json:"node_label_suffix,omitempty"`
}

type ClusterResponse struct {
	BackendWorkloadNames

	UUID              string
	Secret            string
	NQN               string
	Status            string
	Rebalancing       bool
	Shrinking         bool
	NDCS              int
	NPCS              int
	MaxFaultTolerance int
}

type clusterResponsePayload struct {
	BackendWorkloadNames

	ID                string `json:"id"`
	Secret            string `json:"secret"`
	NQN               string `json:"nqn"`
	Status            string `json:"status"`
	Rebalancing       bool   `json:"is_re_balancing"`
	Shrinking         bool   `json:"is_shrinking"`
	NDCS              int    `json:"distr_ndcs"`
	NPCS              int    `json:"distr_npcs"`
	MaxFaultTolerance int    `json:"max_fault_tolerance"`
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
		BackendWorkloadNames: payload.BackendWorkloadNames,

		UUID:              payload.ID,
		Secret:            payload.Secret,
		NQN:               payload.NQN,
		Status:            payload.Status,
		Rebalancing:       payload.Rebalancing,
		Shrinking:         payload.Shrinking,
		NDCS:              payload.NDCS,
		NPCS:              payload.NPCS,
		MaxFaultTolerance: payload.MaxFaultTolerance,
	}, nil
}
