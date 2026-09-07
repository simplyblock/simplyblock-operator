package autoplacement

import (
	"fmt"
	"time"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

const (
	// DefaultEvaluationInterval is how often the rebalancer evaluates load when the spec
	// does not override it. Exported so callers can fall back to it (e.g. for requeue
	// timing) before a RebalancingConfig has been resolved.
	DefaultEvaluationInterval = 60 * time.Second

	// Defaults applied when the spec field is nil.
	defaultImbalanceThresholdPct = 80
	// defaultMinHotColdDifferencePct is the minimum latency-deviation gap (in
	// percentage points) a target node must have below the hot source before a
	// migration is worthwhile — prevents shuffling load between near-equally-loaded
	// nodes.
	defaultMinHotColdDifferencePct     = 20
	defaultCoolDownSeconds             = 600
	defaultMaxVolumeMigrationsPerCycle = 10
	// defaultLatencyPercentile is the fio write-latency percentile driving the
	// rebalancing deviation signal. p50 (median) is stable; p99 is dominated by
	// journal/EC/HA tail spikes. Overridden by the operator-wide --latency-percentile flag.
	defaultLatencyPercentile = "p50"

	// Rolling-window baseline defaults. The rollingWindow strategy derives each node's
	// baseline from a robust (outlier-rejecting) estimate over BaselineWindow of the probe
	// latency series in Prometheus, rather than the frozen one-shot fio benchmark.
	defaultBaselineStrategy   = string(simplyblockv1alpha1.BaselineStrategyRollingWindow)
	defaultBaselineWindow     = 6 * time.Hour
	defaultBaselineColdStart  = string(simplyblockv1alpha1.BaselineColdStartPartialWindow)
	defaultBaselineMinSamples = 6
	// defaultBaselineOutlierK is the Hampel-identifier threshold (samples beyond
	// k·1.4826·MAD from the median are rejected). 3.0 is the conventional value.
	defaultBaselineOutlierK = 3.0
	// defaultBaselineStep is the fallback query step when LatencyBenchmarkInterval is unset;
	// it must match the cadence at which the probe sidecar publishes latency samples.
	defaultBaselineStep = 5 * time.Minute

	// migrationBudgetFraction is the fraction of the source node's total volume IO score
	// that may be migrated in a single evaluation cycle.
	migrationBudgetFraction = 0.10

	// defaultIOPSWeight is the default weight applied to per-volume IOPS in volumeIOScore.
	defaultIOPSWeight = 1.0
	// defaultThroughputMBWeight is the default weight applied to per-volume throughput (MB/s).
	defaultThroughputMBWeight = 0.1
)

// RebalancingConfig holds resolved (defaults applied) values from VolumeRebalancingSpec.
type RebalancingConfig struct {
	EvalInterval       time.Duration
	PrometheusURL      string
	ImbalanceThreshold float64
	// MinHotColdDifferencePct is the minimum deviation gap (percentage points) the
	// target must be below the source for a migration to be selected.
	MinHotColdDifferencePct float64
	// LatencyPercentile selects the fio write-latency percentile ("p50" or "p99") that
	// the deviation signal is computed from. Set operator-wide (not per cluster).
	LatencyPercentile string
	// MigrationEnabled controls whether selected candidates are actually turned into
	// VolumeMigration CRs. When false the rebalancer evaluates and emits metrics but
	// creates no migrations (dry-run). Defaults to true.
	MigrationEnabled bool
	IopsWeight       float64
	ThroughputWeight float64
	MaxMigrations    int
	CoolDownSecs     int64

	// BaselineStrategy selects how the per-node baseline is derived: "rollingWindow"
	// (default) or "benchmark" (frozen one-shot fio measurement).
	BaselineStrategy string
	// BaselineWindow is the rollingWindow look-back period.
	BaselineWindow time.Duration
	// BaselineStep is the range-query step, matching the probe publish cadence.
	BaselineStep time.Duration
	// BaselineColdStart is the under-sampled-node policy: "partialWindow" (default) or "defer".
	BaselineColdStart string
	// BaselineMinSamples is the sample count below which a node is treated as under-sampled.
	BaselineMinSamples int
	// BaselineOutlierK is the Hampel-identifier rejection threshold.
	BaselineOutlierK float64
}

// ResolveAutoPlacementConfig applies defaults and validates the spec. It returns an error
// when prometheusURL is missing, which is the only hard requirement.
func ResolveAutoPlacementConfig(
	spec simplyblockv1alpha1.VolumeAutoPlacementSettings,
) (RebalancingConfig, error) {
	cfg := RebalancingConfig{
		EvalInterval:            DefaultEvaluationInterval,
		ImbalanceThreshold:      float64(ptr.From(spec.ImbalanceThreshold, defaultImbalanceThresholdPct)),
		MinHotColdDifferencePct: float64(ptr.From(spec.MinHotColdDifferencePct, defaultMinHotColdDifferencePct)),
		LatencyPercentile:       defaultLatencyPercentile,
		MigrationEnabled:        ptr.From(spec.MigrationEnabled, true),
		IopsWeight:              defaultIOPSWeight,
		ThroughputWeight:        defaultThroughputMBWeight,
		MaxMigrations:           defaultMaxVolumeMigrationsPerCycle,
		CoolDownSecs:            int64(ptr.From(spec.DefaultCoolDownSeconds, defaultCoolDownSeconds)),
		BaselineStrategy:        string(ptr.From(spec.BaselineStrategy, simplyblockv1alpha1.BaselineStrategy(defaultBaselineStrategy))),
		BaselineWindow:          defaultBaselineWindow,
		BaselineStep:            defaultBaselineStep,
		BaselineColdStart:       string(ptr.From(spec.BaselineColdStart, simplyblockv1alpha1.BaselineColdStartPolicy(defaultBaselineColdStart))),
		BaselineMinSamples:      int(ptr.From(spec.BaselineMinSamples, int32(defaultBaselineMinSamples))),
		BaselineOutlierK:        ptr.From(spec.BaselineOutlierK, defaultBaselineOutlierK),
	}

	if spec.EvaluationInterval != nil && spec.EvaluationInterval.Duration > 0 {
		cfg.EvalInterval = spec.EvaluationInterval.Duration
	}

	if spec.BaselineWindow != nil && spec.BaselineWindow.Duration > 0 {
		cfg.BaselineWindow = spec.BaselineWindow.Duration
	}

	// The query step follows the probe publish cadence (LatencyBenchmarkInterval).
	if spec.LatencyBenchmarkInterval != nil && spec.LatencyBenchmarkInterval.Duration > 0 {
		cfg.BaselineStep = spec.LatencyBenchmarkInterval.Duration
	}

	if cfg.BaselineOutlierK <= 0 {
		cfg.BaselineOutlierK = defaultBaselineOutlierK
	}
	if cfg.BaselineMinSamples < 1 {
		cfg.BaselineMinSamples = defaultBaselineMinSamples
	}

	if spec.PrometheusURL == nil || *spec.PrometheusURL == "" {
		return cfg, fmt.Errorf("spec.volumeRebalancing.prometheusURL is required")
	}
	cfg.PrometheusURL = *spec.PrometheusURL

	iopsWeight := ptr.FromOrZero(spec.IOPSWeight)
	if iopsWeight > 0 {
		cfg.IopsWeight = iopsWeight
	}

	throughputWeight := ptr.FromOrZero(spec.ThroughputWeight)
	if throughputWeight > 0 {
		cfg.ThroughputWeight = throughputWeight
	}

	maxMigrations := ptr.FromOrZero(spec.MaxVolumeMigrationsPerCycle)
	if maxMigrations > 0 {
		cfg.MaxMigrations = int(maxMigrations)
	}
	return cfg, nil
}
