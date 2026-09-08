// Node write latency, which the rebalancer's placement decisions are made
// against. It is separated from the capacity and I/O families because its source
// is different: these samples come from the probe sidecar the rebalancer
// deploys, not from the control plane, so "no data yet" is a normal state with
// a name of its own rather than a failure.

package prometheus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

// ErrLatencyDataNotReady is returned by [Provider.ClusterLatencies] when
// Prometheus cannot be reached for the query, which on a freshly deployed
// cluster is what a probe sidecar that has not finished its first measurement
// cycle looks like. A caller should treat it as "not yet" and wait rather than
// as an error worth failing an operation over.
var ErrLatencyDataNotReady = errors.New("latency data not yet available from Prometheus")

// The percentile a latency reading is taken at. p50 is the stable signal. p99
// is dominated by journal, erasure-coding, and high-availability tail spikes,
// so it says more about the worst moment than about the node.
const (
	PercentileP50 = "p50"
	PercentileP99 = "p99"
)

// latencyMetric returns the metric for a percentile, falling back to p50 for
// any value that is not recognized. A misconfigured percentile should read the
// stable signal rather than fail the placement decision that asked for it.
func latencyMetric(percentile string) string {
	if percentile == PercentileP99 {
		return "simplyblock_node_fio_write_latency_p99_ns"
	}
	return "simplyblock_node_fio_write_latency_p50_ns"
}

// latencyQuery selects the percentile's metric for every named cluster. One
// cluster is matched exactly, and several by an alternation, so both the instant
// and the windowed read reach every cluster in a single query.
func latencyQuery(clusterUUIDs []string, percentile string) string {
	metric := latencyMetric(percentile)
	if len(clusterUUIDs) == 1 {
		return fmt.Sprintf(`%s{cluster=%q}`, metric, clusterUUIDs[0])
	}
	return fmt.Sprintf(`%s{cluster=~%q}`, metric, strings.Join(clusterUUIDs, "|"))
}

// ClusterLatencies returns the most recent write latency in nanoseconds at the
// given percentile, for every node of every named cluster, in one query. The
// result is keyed by cluster UUID and then by node UUID. A cluster or node with
// no scraped measurement is absent rather than zero, because zero latency and
// no reading are not the same claim.
func (p *Provider) ClusterLatencies(
	ctx context.Context,
	clusterUUIDs []string,
	percentile string,
) (map[string]map[string]int64, error) {
	if len(clusterUUIDs) == 0 {
		return map[string]map[string]int64{}, nil
	}

	vec, err := p.queryVector(ctx, latencyQuery(clusterUUIDs, percentile))
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
		out[clusterUUID][nodeUUID] = whole(float64(sample.Value))
	}
	return out, nil
}

// ClusterLatencySamples returns every write-latency sample at the given
// percentile in the [now-window, now] range, per node of every named cluster,
// keyed by cluster UUID and then by node UUID. It is what a rolling-window
// baseline is computed from: the caller reduces each node's slice to one robust
// number, which needs the samples rather than the latest of them.
//
// step should match the cadence the probe sidecar publishes at, since a finer
// step reports the same sample repeatedly and a coarser one drops readings the
// window contains. A cluster or node with no sample in the window is absent.
// The values are nanoseconds, left unrounded because the reduction is done in
// floating point.
func (p *Provider) ClusterLatencySamples(
	ctx context.Context,
	clusterUUIDs []string,
	percentile string,
	window, step time.Duration,
) (map[string]map[string][]float64, error) {
	if len(clusterUUIDs) == 0 {
		return map[string]map[string][]float64{}, nil
	}

	matrix, err := p.queryMatrix(ctx, latencyQuery(clusterUUIDs, percentile), window, step)
	if err != nil {
		var apiErr *promv1.Error
		if errors.As(err, &apiErr) && apiErr.Type == promv1.ErrClient {
			return nil, fmt.Errorf("%w: %w", ErrLatencyDataNotReady, err)
		}
		return nil, err
	}

	out := make(map[string]map[string][]float64)
	for _, series := range matrix {
		clusterUUID := string(series.Metric["cluster"])
		nodeUUID := string(series.Metric["node"])
		if clusterUUID == "" || nodeUUID == "" {
			continue
		}
		if out[clusterUUID] == nil {
			out[clusterUUID] = make(map[string][]float64)
		}
		samples := make([]float64, 0, len(series.Values))
		for _, pair := range series.Values {
			samples = append(samples, float64(pair.Value))
		}
		out[clusterUUID][nodeUUID] = append(out[clusterUUID][nodeUUID], samples...)
	}
	return out, nil
}
