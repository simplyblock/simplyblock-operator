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

package static

import (
	"context"
	"fmt"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/metrics"
)

// Provider returns pre-configured fixture data. Used in unit and integration tests.
type Provider struct {
	// Data is keyed by nodeUUID. Calls to GetClusterMetrics return all entries;
	// calls to GetNodeMetrics return the entry for the requested nodeUUID.
	Data map[string]*metrics.NodeMetrics
}

// New constructs a Provider with the given fixture data.
func New(data map[string]*metrics.NodeMetrics) *Provider {
	return &Provider{Data: data}
}

func (p *Provider) GetNodeMetrics(_ context.Context, _, nodeUUID string) (*metrics.NodeMetrics, error) {
	m, ok := p.Data[nodeUUID]
	if !ok {
		return nil, fmt.Errorf("static provider: no metrics for node %s", nodeUUID)
	}
	if m.CollectedAt.IsZero() {
		m.CollectedAt = time.Now()
	}
	return m, nil
}

func (p *Provider) GetClusterMetrics(_ context.Context, _ string) (map[string]*metrics.NodeMetrics, error) {
	now := time.Now()
	out := make(map[string]*metrics.NodeMetrics, len(p.Data))
	for k, v := range p.Data {
		if v.CollectedAt.IsZero() {
			v.CollectedAt = now
		}
		out[k] = v
	}
	return out, nil
}
