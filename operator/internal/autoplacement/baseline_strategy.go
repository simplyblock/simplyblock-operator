package autoplacement

import (
	"context"
	"fmt"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	promlatency "github.com/simplyblock/simplyblock-operator/internal/metrics/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BaselineProvider resolves the per-node latency baseline (ns at the configured percentile)
// that forms the denominator of the rebalancing deviation signal. Nodes without a usable
// baseline are omitted from the returned map — ComputeLatencyDeviationPct then reads 0 for
// them, so they are neither flagged hot nor preferred as targets.
type BaselineProvider interface {
	BaselineNS(ctx context.Context, inputs ...StorageNodeSelectorInput) (map[string]int64, error)
}

// newBaselineProvider selects the BaselineProvider implementation for cfg.BaselineStrategy.
// "benchmark" reads the frozen one-shot fio measurement from the StorageNodeSet CRs;
// rollingWindow (the default, and the fallback for any unrecognised value) derives a robust
// estimate from a rolling window of the probe latency series in Prometheus.
func newBaselineProvider(k8sClient client.Client, cfg RebalancingConfig) (BaselineProvider, error) {
	if cfg.BaselineStrategy == string(simplyblockv1alpha1.BaselineStrategyBenchmark) {
		return &benchmarkBaselineProvider{client: k8sClient, percentile: cfg.LatencyPercentile}, nil
	}
	provider, err := promlatency.New(cfg.PrometheusURL)
	if err != nil {
		return nil, fmt.Errorf("create prometheus baseline provider: %w", err)
	}
	return &rollingWindowBaselineProvider{prom: provider, cfg: cfg}, nil
}

// benchmarkBaselineProvider reads the one-shot fio baseline recorded once per node by the
// baseline Job and stored on StorageNodeSet.status.latencyMetrics.
type benchmarkBaselineProvider struct {
	client     client.Client
	percentile string
}

func (b *benchmarkBaselineProvider) BaselineNS(
	ctx context.Context,
	inputs ...StorageNodeSelectorInput,
) (map[string]int64, error) {
	result := make(map[string]int64)
	for _, input := range inputs {
		var snodeList simplyblockv1alpha1.StorageNodeSetList
		if err := b.client.List(ctx, &snodeList, client.InNamespace(input.Namespace)); err != nil {
			// Stay resilient to a transient list error: skip this namespace rather than
			// failing the whole evaluation cycle (matches the previous CR-read behaviour).
			continue
		}
		for _, snode := range snodeList.Items {
			for _, lm := range snode.Status.LatencyMetrics {
				baseline := lm.BaselineP50NS
				if b.percentile == promlatency.PercentileP99 {
					baseline = lm.BaselineP99NS
				}
				if baseline > 0 {
					result[lm.NodeUUID] = baseline
				}
			}
		}
	}
	return result, nil
}

// rollingWindowBaselineProvider derives each node's baseline from a rolling window of the
// probe-sidecar latency series in Prometheus, reduced to a single value by robustBaselineNS
// (Hampel outlier rejection + median of survivors). It emits the per-node baseline and
// sample-count gauges as a side effect.
type rollingWindowBaselineProvider struct {
	prom *promlatency.Provider
	cfg  RebalancingConfig
}

func (r *rollingWindowBaselineProvider) BaselineNS(
	ctx context.Context,
	inputs ...StorageNodeSelectorInput,
) (map[string]int64, error) {
	clusterIDs := distinctClusterUUIDs(inputs)
	windowed, err := r.prom.GetClustersWindowedLatency(
		ctx, clusterIDs, r.cfg.LatencyPercentile, r.cfg.BaselineWindow, r.cfg.BaselineStep,
	)
	if err != nil {
		return nil, err
	}

	reduced := reduceWindowedBaselines(windowed, r.cfg)
	result := make(map[string]int64, len(reduced))
	for _, nb := range reduced {
		result[nb.nodeUUID] = nb.baselineNS
		setBaselineGauges(nb.clusterUUID, nb.nodeUUID, nb.baselineNS, nb.samplesTotal, nb.samplesRejected)
	}
	return result, nil
}

// nodeBaseline is one node's reduced rolling-window baseline plus the sample diagnostics.
type nodeBaseline struct {
	clusterUUID     string
	nodeUUID        string
	baselineNS      int64
	samplesTotal    int
	samplesRejected int
}

// reduceWindowedBaselines reduces per-node windowed samples to a single robust baseline each,
// applying the cold-start policy. It is pure (no Prometheus, no metrics) so the cold-start
// and estimator behaviour can be tested directly. A node is dropped when it is under-sampled
// under the "defer" policy, or when no positive baseline can be computed from its samples.
func reduceWindowedBaselines(
	windowed map[string]map[string][]float64,
	cfg RebalancingConfig,
) []nodeBaseline {
	deferUnderSampled := cfg.BaselineColdStart == string(simplyblockv1alpha1.BaselineColdStartDefer)

	var out []nodeBaseline
	for clusterUUID, byNode := range windowed {
		for nodeUUID, samples := range byNode {
			// Cold start: an under-sampled node is either skipped ("defer") or computed
			// from whatever samples exist ("partialWindow").
			if len(samples) < cfg.BaselineMinSamples && deferUnderSampled {
				continue
			}
			baselineNS, kept, rejected, ok := robustBaselineNS(samples, cfg.BaselineOutlierK)
			if !ok || baselineNS <= 0 {
				continue
			}
			out = append(out, nodeBaseline{
				clusterUUID:     clusterUUID,
				nodeUUID:        nodeUUID,
				baselineNS:      baselineNS,
				samplesTotal:    kept + rejected,
				samplesRejected: rejected,
			})
		}
	}
	return out
}
