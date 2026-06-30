// Package prometheus implements metrics providers backed by a Prometheus instance.
//
// A single Provider wraps the Prometheus HTTP client and exposes methods for
// all metric categories used by the operator:
//
//   - GetClusterMetrics / GetNodeMetrics — per-node SPDK I/O (NodeMetricsProvider)
//   - GetClustersCurrentLatency         — fio write latency per node at the configured percentile (p50/p99)
//   - GetClusterVolumeIO                — per-volume IOPS + throughput
package prometheus

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	prometheusapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/simplyblock/simplyblock-operator/internal/metrics"
)

// ErrLatencyDataNotReady is returned by GetClustersCurrentLatency when Prometheus has
// not yet received any latency samples — the rebalancer probe sidecar has not completed
// its first measurement cycle. Callers should treat this as a transient "not yet
// available" condition and log a warning rather than an error.
var ErrLatencyDataNotReady = errors.New("latency data not yet available from Prometheus")

// Provider wraps a Prometheus API client and exposes all metric queries used by
// the operator. Construct once per reconcile cycle via New.
type Provider struct {
	api promv1.API
}

// New constructs a Provider that connects to the given Prometheus URL.
func New(
	prometheusURL string,
) (*Provider, error) {
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

func (p *Provider) GetNodeMetrics(
	ctx context.Context,
	clusterUUID, nodeUUID string,
) (*metrics.NodeMetrics, error) {
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

func (p *Provider) GetClusterMetrics(
	ctx context.Context,
	clusterUUID string,
) (map[string]*metrics.NodeMetrics, error) {
	query := fmt.Sprintf(`%s{cluster=%q}`, nodeIOPSMetric, clusterUUID)
	result, err := p.queryVector(ctx, query)
	if err != nil {
		return nil, err
	}
	return parseNodeIOPSVector(result, time.Now()), nil
}

func parseNodeIOPSVector(
	vec model.Vector,
	collectedAt time.Time,
) map[string]*metrics.NodeMetrics {
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

func validScheme(
	s metrics.ErasureScheme,
) bool {
	switch s {
	case metrics.ErasureScheme1Plus1, metrics.ErasureScheme2Plus1, metrics.ErasureScheme4Plus1,
		metrics.ErasureScheme1Plus2, metrics.ErasureScheme2Plus2, metrics.ErasureScheme4Plus2:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Latency metrics (Phase 1) — fio write latency per node (p50 or p99)
// ---------------------------------------------------------------------------

// LatencyPercentile selects which fio write-latency percentile the deviation signal
// uses. p50 (median) is stable; p99 is dominated by journal/EC/HA tail spikes.
const (
	PercentileP50 = "p50"
	PercentileP99 = "p99"
)

// latencyMetricName returns the Prometheus metric for the given percentile, falling
// back to p50 for any unrecognised value.
func latencyMetricName(percentile string) string {
	if percentile == PercentileP99 {
		return "simplyblock_node_fio_write_latency_p99_ns"
	}
	return "simplyblock_node_fio_write_latency_p50_ns"
}

// GetClusterCurrentLatency returns the most recent write latency (nanoseconds) at the
// given percentile per node UUID for all nodes in the cluster. Nodes with no scraped
// measurement are omitted. Baseline latency is not stored in Prometheus — it lives in
// the StorageNode CR.
func (p *Provider) GetClusterCurrentLatency(
	ctx context.Context,
	clusterUUID string,
	percentile string,
) (map[string]int64, error) {
	all, err := p.GetClustersCurrentLatency(ctx, []string{clusterUUID}, percentile)
	if err != nil {
		return nil, err
	}
	return all[clusterUUID], nil
}

// GetClustersCurrentLatency returns the most recent write latency at the given
// percentile per node for all given clusters in a single Prometheus query, keyed by
// [clusterUUID][nodeUUID]. Clusters or nodes with no scraped measurement are omitted.
// Returns ErrLatencyDataNotReady (wrapped) on client-level connectivity errors.
func (p *Provider) GetClustersCurrentLatency(
	ctx context.Context,
	clusterUUIDs []string,
	percentile string,
) (map[string]map[string]int64, error) {
	if len(clusterUUIDs) == 0 {
		return map[string]map[string]int64{}, nil
	}

	metric := latencyMetricName(percentile)
	var query string
	if len(clusterUUIDs) == 1 {
		query = fmt.Sprintf(`%s{cluster=%q}`, metric, clusterUUIDs[0])
	} else {
		query = fmt.Sprintf(`%s{cluster=~%q}`, metric, strings.Join(clusterUUIDs, "|"))
	}

	vec, err := p.queryVector(ctx, query)
	if err != nil {
		var apiErr *promv1.Error
		if errors.As(err, &apiErr) && apiErr.Type == promv1.ErrClient {
			return nil, fmt.Errorf("%w: %w", ErrLatencyDataNotReady, err)
		}
		return nil, err
	}

	out := make(map[string]map[string]int64)
	for _, sample := range vec {
		clusterUUID := string(sample.Metric["cluster"])
		nodeUUID := string(sample.Metric["node"])
		if clusterUUID == "" || nodeUUID == "" {
			continue
		}
		if out[clusterUUID] == nil {
			out[clusterUUID] = make(map[string]int64)
		}
		out[clusterUUID][nodeUUID] = int64(math.Round(float64(sample.Value)))
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
func (p *Provider) GetClusterVolumeIO(
	ctx context.Context,
	clusterUUID string,
) (map[string]VolumeIOMetrics, error) {
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
func (p *Provider) queryLvolScalar(
	ctx context.Context,
	metric, clusterUUID string,
) (map[string]float64, error) {
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

func (p *Provider) queryVector(
	ctx context.Context,
	query string,
) (model.Vector, error) {
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
