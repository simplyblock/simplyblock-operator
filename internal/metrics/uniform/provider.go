/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package uniform provides a NodeMetricsProvider that returns a constant IOPS=1
// for every node, effectively making all nodes appear equally loaded.
// This disables IOPS-based rebalancing while keeping volume-count and capacity
// balancing active.
package uniform

import (
	"context"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/metrics"
)

// Provider implements NodeMetricsProvider with constant unit metrics.
// GetClusterMetrics returns an empty map because the provider has no knowledge
// of which nodes belong to a cluster; the reconciler already fetches the node
// list from the storage API and uses that as the authoritative source.
// GetNodeMetrics returns a single sample with IOPS=1 and BytesPerSecond=1,
// causing all nodes to receive identical weighted scores and an imbalance
// percentage of 0 — suppressing all migration decisions.
type Provider struct{}

// New returns a Provider.
func New() *Provider {
	return &Provider{}
}

func (p *Provider) GetNodeMetrics(_ context.Context, _, nodeUUID string) (*metrics.NodeMetrics, error) {
	return &metrics.NodeMetrics{
		NodeUUID:    nodeUUID,
		CollectedAt: time.Now(),
		IO: []metrics.BlockSizeIOMetrics{
			{
				BlockSizeBytes: 4096,
				Operation:      metrics.IOOperationRead,
				ErasureScheme:  metrics.ErasureScheme1Plus1,
				IOPS:           1,
				BytesPerSecond: 1,
			},
		},
	}, nil
}

// GetClusterMetrics returns an empty map. The caller is expected to iterate the
// storage API node list and call GetNodeMetrics per node if needed. Returning
// an empty map here means no new samples enter the sliding window, and all node
// scores decay uniformly to zero — preserving equal-load appearance.
func (p *Provider) GetClusterMetrics(_ context.Context, _ string) (map[string]*metrics.NodeMetrics, error) {
	return map[string]*metrics.NodeMetrics{}, nil
}
