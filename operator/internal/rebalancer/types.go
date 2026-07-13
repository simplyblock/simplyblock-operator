// Package rebalancer holds the JSON wire types exchanged between the operator and the
// simplyblock-rebalancer CLI binary. Each type is one schema with a producer on one side
// and a consumer on the other; defining it once here makes the inter-process contract
// compile-time enforced on both sides (a field/tag change can no longer silently drift).
//
// This package depends only on the standard library so the minimal rebalancer CLI does
// not pull in the operator's Kubernetes / controller-runtime dependencies.
package rebalancer

// NodeConfig is one element of the JSON array the operator writes per k8s hostname to the
// simplyblock-rebalancer ConfigMap. The rebalancer probe (--config) iterates the array to
// benchmark every NUMA node on its host independently.
type NodeConfig struct {
	NQN         string `json:"nqn"`
	Addr        string `json:"addr"`
	Port        int32  `json:"port"`
	NodeUUID    string `json:"nodeUUID"`
	ClusterUUID string `json:"clusterUUID"`
}

// LatencyResult is the JSON the rebalancer baseline run writes to its container
// termination log; the operator reads it back to store the per-node baseline latency.
type LatencyResult struct {
	P50NS int64 `json:"p50_ns"`
	P99NS int64 `json:"p99_ns"`
}
