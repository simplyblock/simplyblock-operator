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

package prometheus

import (
	"context"
	"fmt"
	"math"
	"time"

	prometheusapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

const latencyCurrentMetric = "simplyblock_node_fio_write_latency_p99_ns"

// LatencyProvider queries Prometheus for the most recent fio p99 write latency per node.
// It reads simplyblock_node_fio_write_latency_p99_ns, which is written to the textfile
// collector by the fio-bench-probe sidecar after each periodic fio run.
// Baseline latency is intentionally not stored in Prometheus — it is a one-time measurement
// kept in the StorageNode CR status.
type LatencyProvider struct {
	api promv1.API
}

// NewLatencyProvider constructs a LatencyProvider targeting the given Prometheus URL.
func NewLatencyProvider(prometheusURL string) (*LatencyProvider, error) {
	c, err := prometheusapi.NewClient(prometheusapi.Config{Address: prometheusURL})
	if err != nil {
		return nil, fmt.Errorf("create prometheus client: %w", err)
	}
	return &LatencyProvider{api: promv1.NewAPI(c)}, nil
}

// GetClusterCurrentP99 returns the most recent p99 write latency (nanoseconds) per node UUID
// for all nodes in the given cluster. Nodes for which no metric has been scraped yet are omitted.
func (p *LatencyProvider) GetClusterCurrentP99(ctx context.Context, clusterUUID string) (map[string]int64, error) {
	query := fmt.Sprintf(`%s{cluster=%q}`, latencyCurrentMetric, clusterUUID)
	val, _, err := p.api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("prometheus query %q: %w", query, err)
	}
	vec, ok := val.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("prometheus query %q: unexpected result type %T", query, val)
	}
	out := make(map[string]int64, len(vec))
	for _, sample := range vec {
		nodeUUID := string(sample.Metric["node"])
		if nodeUUID == "" {
			continue
		}
		out[nodeUUID] = int64(math.Round(float64(sample.Value)))
	}
	return out, nil
}
