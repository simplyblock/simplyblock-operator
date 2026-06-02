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

// Package prometheus implements NodeMetricsProvider using PromQL queries against
// a Prometheus instance that scrapes SPDK per-node I/O statistics.
//
// Expected metric:
//
//	simplyblock_node_iops_total{cluster="<clusterUUID>", node="<nodeUUID>",
//	                             blocksize="<bytes>", operation="read|write",
//	                             erasure_scheme="1+1|2+1|..."}
//
// Label names must be agreed upon with the backend/SPDK team.
package prometheus

import (
	"context"
	"fmt"
	"strconv"
	"time"

	prometheusapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/simplyblock/simplyblock-operator/internal/metrics"
)

const metricName = "simplyblock_node_iops_total"

// Provider queries Prometheus for per-node SPDK I/O statistics.
type Provider struct {
	api promv1.API
}

// New constructs a Provider that connects to the given Prometheus URL.
func New(prometheusURL string) (*Provider, error) {
	client, err := prometheusapi.NewClient(prometheusapi.Config{Address: prometheusURL})
	if err != nil {
		return nil, fmt.Errorf("create prometheus client: %w", err)
	}
	return &Provider{api: promv1.NewAPI(client)}, nil
}

func (p *Provider) GetNodeMetrics(ctx context.Context, clusterUUID, nodeUUID string) (*metrics.NodeMetrics, error) {
	query := fmt.Sprintf(`%s{cluster=%q,node=%q}`, metricName, clusterUUID, nodeUUID)
	result, err := p.queryInstant(ctx, query)
	if err != nil {
		return nil, err
	}
	clusterData := parseVector(result, time.Now())
	m, ok := clusterData[nodeUUID]
	if !ok {
		return nil, fmt.Errorf("prometheus: no series for node %s in cluster %s", nodeUUID, clusterUUID)
	}
	return m, nil
}

func (p *Provider) GetClusterMetrics(ctx context.Context, clusterUUID string) (map[string]*metrics.NodeMetrics, error) {
	query := fmt.Sprintf(`%s{cluster=%q}`, metricName, clusterUUID)
	result, err := p.queryInstant(ctx, query)
	if err != nil {
		return nil, err
	}
	return parseVector(result, time.Now()), nil
}

func (p *Provider) queryInstant(ctx context.Context, query string) (model.Vector, error) {
	val, warnings, err := p.api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("prometheus query %q: %w", query, err)
	}
	for _, w := range warnings {
		_ = w // warnings are informational; callers may log them if desired
	}
	vec, ok := val.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("prometheus query %q: unexpected result type %T", query, val)
	}
	return vec, nil
}

// parseVector groups the instant-query result by node UUID and maps labels to
// BlockSizeIOMetrics entries.
func parseVector(vec model.Vector, collectedAt time.Time) map[string]*metrics.NodeMetrics {
	out := make(map[string]*metrics.NodeMetrics)
	for _, sample := range vec {
		nodeUUID := string(sample.Metric["node"])
		if nodeUUID == "" {
			continue
		}

		blockSizeStr := string(sample.Metric["blocksize"])
		blockSizeBytes, err := strconv.ParseInt(blockSizeStr, 10, 64)
		if err != nil {
			continue
		}

		operation := metrics.IOOperation(sample.Metric["operation"])
		if operation != metrics.IOOperationRead && operation != metrics.IOOperationWrite {
			continue
		}

		scheme := metrics.ErasureScheme(sample.Metric["erasure_scheme"])
		if !validScheme(scheme) {
			continue
		}

		nm, exists := out[nodeUUID]
		if !exists {
			nm = &metrics.NodeMetrics{NodeUUID: nodeUUID, CollectedAt: collectedAt}
			out[nodeUUID] = nm
		}

		nm.IO = append(nm.IO, metrics.BlockSizeIOMetrics{
			BlockSizeBytes: blockSizeBytes,
			Operation:      operation,
			ErasureScheme:  scheme,
			IOPS:           float64(sample.Value),
		})
	}
	return out
}

func validScheme(s metrics.ErasureScheme) bool {
	switch s {
	case metrics.ErasureScheme1Plus1, metrics.ErasureScheme2Plus1, metrics.ErasureScheme4Plus1,
		metrics.ErasureScheme1Plus2, metrics.ErasureScheme2Plus2, metrics.ErasureScheme4Plus2:
		return true
	}
	return false
}
