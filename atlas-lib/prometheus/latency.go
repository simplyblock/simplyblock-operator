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

	metric := latencyMetric(percentile)
	query := fmt.Sprintf(`%s{cluster=%q}`, metric, clusterUUIDs[0])
	if len(clusterUUIDs) > 1 {
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
		out[clusterUUID][nodeUUID] = whole(float64(sample.Value))
	}
	return out, nil
}
