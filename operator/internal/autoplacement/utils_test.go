package autoplacement

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func TestResolveAutoPlacementConfig_BaselineDefaults(t *testing.T) {
	cfg, err := ResolveAutoPlacementConfig(simplyblockv1alpha1.VolumeAutoPlacementSettings{
		PrometheusURL: ptr.To("http://prom:9090"),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if cfg.BaselineStrategy != string(simplyblockv1alpha1.BaselineStrategyRollingWindow) {
		t.Errorf("BaselineStrategy = %q, want rollingWindow (default)", cfg.BaselineStrategy)
	}
	if cfg.BaselineWindow != 6*time.Hour {
		t.Errorf("BaselineWindow = %v, want 6h", cfg.BaselineWindow)
	}
	if cfg.BaselineColdStart != string(simplyblockv1alpha1.BaselineColdStartPartialWindow) {
		t.Errorf("BaselineColdStart = %q, want partialWindow", cfg.BaselineColdStart)
	}
	if cfg.BaselineMinSamples != 6 {
		t.Errorf("BaselineMinSamples = %d, want 6", cfg.BaselineMinSamples)
	}
	if cfg.BaselineOutlierK != 3.0 {
		t.Errorf("BaselineOutlierK = %v, want 3.0", cfg.BaselineOutlierK)
	}
	// Step defaults to 5m when LatencyBenchmarkInterval is unset.
	if cfg.BaselineStep != 5*time.Minute {
		t.Errorf("BaselineStep = %v, want 5m (default)", cfg.BaselineStep)
	}
}

func TestResolveAutoPlacementConfig_BaselineOverrides(t *testing.T) {
	cfg, err := ResolveAutoPlacementConfig(simplyblockv1alpha1.VolumeAutoPlacementSettings{
		PrometheusURL:            ptr.To("http://prom:9090"),
		BaselineStrategy:         ptr.To(simplyblockv1alpha1.BaselineStrategyBenchmark),
		BaselineWindow:           &metav1.Duration{Duration: 12 * time.Hour},
		BaselineColdStart:        ptr.To(simplyblockv1alpha1.BaselineColdStartDefer),
		BaselineMinSamples:       ptr.To(int32(12)),
		BaselineOutlierK:         ptr.To(2.5),
		LatencyBenchmarkInterval: &metav1.Duration{Duration: 2 * time.Minute},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if cfg.BaselineStrategy != string(simplyblockv1alpha1.BaselineStrategyBenchmark) {
		t.Errorf("BaselineStrategy = %q, want benchmark", cfg.BaselineStrategy)
	}
	if cfg.BaselineWindow != 12*time.Hour {
		t.Errorf("BaselineWindow = %v, want 12h", cfg.BaselineWindow)
	}
	if cfg.BaselineColdStart != string(simplyblockv1alpha1.BaselineColdStartDefer) {
		t.Errorf("BaselineColdStart = %q, want defer", cfg.BaselineColdStart)
	}
	if cfg.BaselineMinSamples != 12 {
		t.Errorf("BaselineMinSamples = %d, want 12", cfg.BaselineMinSamples)
	}
	if cfg.BaselineOutlierK != 2.5 {
		t.Errorf("BaselineOutlierK = %v, want 2.5", cfg.BaselineOutlierK)
	}
	// Step follows LatencyBenchmarkInterval.
	if cfg.BaselineStep != 2*time.Minute {
		t.Errorf("BaselineStep = %v, want 2m (from LatencyBenchmarkInterval)", cfg.BaselineStep)
	}
}

func TestResolveAutoPlacementConfig_RequiresPrometheusURL(t *testing.T) {
	if _, err := ResolveAutoPlacementConfig(simplyblockv1alpha1.VolumeAutoPlacementSettings{}); err == nil {
		t.Fatalf("expected error when prometheusURL is missing")
	}
}
