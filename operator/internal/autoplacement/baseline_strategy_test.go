package autoplacement

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	promlatency "github.com/simplyblock/simplyblock-operator/internal/metrics/prometheus"
)

func baselineTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := simplyblockv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add simplyblock scheme: %v", err)
	}
	return s
}

func storageNodeSet(ns, name, nodeUUID string, p50, p99 int64) *simplyblockv1alpha1.StorageNodeSet {
	return &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			LatencyMetrics: []simplyblockv1alpha1.NodeLatencyMetrics{
				{NodeUUID: nodeUUID, BaselineP50NS: p50, BaselineP99NS: p99},
			},
		},
	}
}

func TestNewBaselineProvider_SelectsImplementation(t *testing.T) {
	scheme := baselineTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	bench, err := newBaselineProvider(cl, RebalancingConfig{
		BaselineStrategy: string(simplyblockv1alpha1.BaselineStrategyBenchmark),
		PrometheusURL:    "http://prom:9090",
	})
	if err != nil {
		t.Fatalf("benchmark: %v", err)
	}
	if _, ok := bench.(*benchmarkBaselineProvider); !ok {
		t.Errorf("strategy=benchmark gave %T, want *benchmarkBaselineProvider", bench)
	}

	for _, strategy := range []string{string(simplyblockv1alpha1.BaselineStrategyRollingWindow), "", "bogus"} {
		p, err := newBaselineProvider(cl, RebalancingConfig{BaselineStrategy: strategy, PrometheusURL: "http://prom:9090"})
		if err != nil {
			t.Fatalf("strategy=%q: %v", strategy, err)
		}
		if _, ok := p.(*rollingWindowBaselineProvider); !ok {
			t.Errorf("strategy=%q gave %T, want *rollingWindowBaselineProvider (default/fallback)", strategy, p)
		}
	}
}

func TestBenchmarkBaselineProvider_ReadsCRs(t *testing.T) {
	scheme := baselineTestScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			storageNodeSet("ns1", "set-a", "node-1", 1000, 5000),
			storageNodeSet("ns1", "set-b", "node-2", 2000, 6000),
			storageNodeSet("ns1", "set-zero", "node-3", 0, 0), // no baseline yet → omitted
		).
		Build()

	t.Run("p50", func(t *testing.T) {
		p := &benchmarkBaselineProvider{client: cl, percentile: promlatency.PercentileP50}
		got, err := p.BaselineNS(context.Background(), makeInput("ns1"))
		if err != nil {
			t.Fatalf("BaselineNS: %v", err)
		}
		want := map[string]int64{"node-1": 1000, "node-2": 2000}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("node %s = %d, want %d", k, got[k], v)
			}
		}
	})

	t.Run("p99", func(t *testing.T) {
		p := &benchmarkBaselineProvider{client: cl, percentile: promlatency.PercentileP99}
		got, err := p.BaselineNS(context.Background(), makeInput("ns1"))
		if err != nil {
			t.Fatalf("BaselineNS: %v", err)
		}
		if got["node-1"] != 5000 || got["node-2"] != 6000 {
			t.Errorf("p99 baselines = %v, want node-1=5000 node-2=6000", got)
		}
	})
}

func TestReduceWindowedBaselines_ColdStart(t *testing.T) {
	// node-hot has plenty of samples; node-new has only 2 (< MinSamples).
	windowed := map[string]map[string][]float64{
		"cluster-a": {
			"node-hot": {1000, 1010, 990, 1005, 995, 1002, 998, 1001},
			"node-new": {900, 1100},
		},
	}
	base := RebalancingConfig{BaselineMinSamples: 6, BaselineOutlierK: 3.0}

	t.Run("partialWindow includes under-sampled node", func(t *testing.T) {
		cfg := base
		cfg.BaselineColdStart = string(simplyblockv1alpha1.BaselineColdStartPartialWindow)
		got := baselineMap(reduceWindowedBaselines(windowed, cfg))
		if _, ok := got["node-new"]; !ok {
			t.Errorf("partialWindow should include node-new, got %v", got)
		}
		if got["node-new"] != 1000 { // plain median of {900,1100}
			t.Errorf("node-new baseline = %d, want 1000", got["node-new"])
		}
		if _, ok := got["node-hot"]; !ok {
			t.Errorf("node-hot should always be present, got %v", got)
		}
	})

	t.Run("defer omits under-sampled node", func(t *testing.T) {
		cfg := base
		cfg.BaselineColdStart = string(simplyblockv1alpha1.BaselineColdStartDefer)
		got := baselineMap(reduceWindowedBaselines(windowed, cfg))
		if _, ok := got["node-new"]; ok {
			t.Errorf("defer should omit under-sampled node-new, got %v", got)
		}
		if _, ok := got["node-hot"]; !ok {
			t.Errorf("defer should still include well-sampled node-hot, got %v", got)
		}
	})
}

func baselineMap(nbs []nodeBaseline) map[string]int64 {
	m := make(map[string]int64, len(nbs))
	for _, nb := range nbs {
		m[nb.nodeUUID] = nb.baselineNS
	}
	return m
}
