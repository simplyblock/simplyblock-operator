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

// Package prometheus implements metrics providers backed by a Prometheus instance.
//
// A single Provider wraps the Prometheus HTTP client and exposes methods for
// all metric categories used by the operator:
//
//   - GetClusterMetrics / GetNodeMetrics — Phase 2 per-node SPDK I/O (NodeMetricsProvider)
//   - GetClusterCurrentP99              — Phase 1 fio p99 write latency per node
//   - GetClusterVolumeIO               — Phase 1 per-volume IOPS + throughput
package prometheus

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	prometheusapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/simplyblock/simplyblock-operator/internal/metrics"
)

// Provider wraps a Prometheus API client and exposes all metric queries used by
// the operator. Construct once per reconcile cycle via New.
type Provider struct {
	api promv1.API
}

// New constructs a Provider that connects to the given Prometheus URL.
func New(prometheusURL string) (*Provider, error) {
	c, err := prometheusapi.NewClient(prometheusapi.Config{Address: prometheusURL})
	if err != nil {
		return nil, fmt.Errorf("create prometheus client: %w", err)
	}
	return &Provider{api: promv1.NewAPI(c)}, nil
}

// ---------------------------------------------------------------------------
// NodeMetricsProvider (Phase 2) — per-node SPDK I/O via simplyblock_node_iops_total
// ---------------------------------------------------------------------------

const nodeIOPSMetric = "simplyblock_node_iops_total"

func (p *Provider) GetNodeMetrics(ctx context.Context, clusterUUID, nodeUUID string) (*metrics.NodeMetrics, error) {
	query := fmt.Sprintf(`%s{cluster=%q,node=%q}`, nodeIOPSMetric, clusterUUID, nodeUUID)
	result, err := p.queryVector(ctx, query)
	if err != nil {
		return nil, err
	}
	clusterData := parseNodeIOPSVector(result, time.Now())
	m, ok := clusterData[nodeUUID]
	if !ok {
		return nil, fmt.Errorf("prometheus: no series for node %s in cluster %s", nodeUUID, clusterUUID)
	}
	return m, nil
}

func (p *Provider) GetClusterMetrics(ctx context.Context, clusterUUID string) (map[string]*metrics.NodeMetrics, error) {
	query := fmt.Sprintf(`%s{cluster=%q}`, nodeIOPSMetric, clusterUUID)
	result, err := p.queryVector(ctx, query)
	if err != nil {
		return nil, err
	}
	return parseNodeIOPSVector(result, time.Now()), nil
}

func parseNodeIOPSVector(vec model.Vector, collectedAt time.Time) map[string]*metrics.NodeMetrics {
	out := make(map[string]*metrics.NodeMetrics)
	for _, sample := range vec {
		nodeUUID := string(sample.Metric["node"])
		if nodeUUID == "" {
			continue
		}
		blockSizeBytes, err := strconv.ParseInt(string(sample.Metric["blocksize"]), 10, 64)
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

// ---------------------------------------------------------------------------
// Latency metrics (Phase 1) — fio p99 write latency per node
// ---------------------------------------------------------------------------

// GetClusterCurrentP99 returns the most recent p99 write latency (nanoseconds) per node UUID
// for all nodes in the given cluster. Nodes with no scraped measurement are omitted.
// Baseline latency is not stored in Prometheus — it is kept in the StorageNode CR.
func (p *Provider) GetClusterCurrentP99(ctx context.Context, clusterUUID string) (map[string]int64, error) {
	vec, err := p.queryVector(ctx, fmt.Sprintf(`simplyblock_node_fio_write_latency_p99_ns{cluster=%q}`, clusterUUID))
	if err != nil {
		return nil, err
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

// ---------------------------------------------------------------------------
// Volume IO metrics (Phase 1) — per-volume IOPS + throughput from control plane
// ---------------------------------------------------------------------------

// VolumeIOMetrics holds combined read+write IOPS and throughput for a single volume.
type VolumeIOMetrics struct {
	IOPS                  float64
	ThroughputBytesPerSec float64
}

// GetClusterVolumeIO returns combined IOPS and throughput per volume UUID for all volumes
// in the given cluster, using the metrics exported by the simplyblock control plane:
//
//	lvol_read_io_ps / lvol_write_io_ps    — read/write IOPS, labelled by `lvol` (volume UUID)
//	lvol_read_bytes_ps / lvol_write_bytes_ps — read/write throughput (bytes/s)
//
// Volumes absent from Prometheus are omitted from the result.
func (p *Provider) GetClusterVolumeIO(ctx context.Context, clusterUUID string) (map[string]VolumeIOMetrics, error) {
	readIOPS, err := p.queryLvolScalar(ctx, "lvol_read_io_ps", clusterUUID)
	if err != nil {
		return nil, err
	}
	writeIOPS, err := p.queryLvolScalar(ctx, "lvol_write_io_ps", clusterUUID)
	if err != nil {
		return nil, err
	}
	readBytes, err := p.queryLvolScalar(ctx, "lvol_read_bytes_ps", clusterUUID)
	if err != nil {
		return nil, err
	}
	writeBytes, err := p.queryLvolScalar(ctx, "lvol_write_bytes_ps", clusterUUID)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(readIOPS))
	for k := range readIOPS {
		seen[k] = struct{}{}
	}
	for k := range writeIOPS {
		seen[k] = struct{}{}
	}
	for k := range readBytes {
		seen[k] = struct{}{}
	}
	for k := range writeBytes {
		seen[k] = struct{}{}
	}

	out := make(map[string]VolumeIOMetrics, len(seen))
	for volUUID := range seen {
		out[volUUID] = VolumeIOMetrics{
			IOPS:                  readIOPS[volUUID] + writeIOPS[volUUID],
			ThroughputBytesPerSec: readBytes[volUUID] + writeBytes[volUUID],
		}
	}
	return out, nil
}

// queryLvolScalar runs an instant query and returns the value keyed by the `lvol` label.
func (p *Provider) queryLvolScalar(ctx context.Context, metric, clusterUUID string) (map[string]float64, error) {
	vec, err := p.queryVector(ctx, fmt.Sprintf(`%s{cluster=%q}`, metric, clusterUUID))
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(vec))
	for _, sample := range vec {
		volUUID := string(sample.Metric["lvol"])
		if volUUID == "" {
			continue
		}
		out[volUUID] = float64(sample.Value)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

func (p *Provider) queryVector(ctx context.Context, query string) (model.Vector, error) {
	val, _, err := p.api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("prometheus query %q: %w", query, err)
	}
	vec, ok := val.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("prometheus query %q: unexpected result type %T", query, val)
	}
	return vec, nil
}
