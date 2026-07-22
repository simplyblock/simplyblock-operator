package autobalancing

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
}

// ResolveRebalancingConfig applies defaults and validates the spec. It returns an error
// when prometheusURL is missing, which is the only hard requirement.
func ResolveRebalancingConfig(
	spec *simplyblockv1alpha1.VolumeRebalancingSettings,
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
	}
	if spec.EvaluationInterval != nil && spec.EvaluationInterval.Duration > 0 {
		cfg.EvalInterval = spec.EvaluationInterval.Duration
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
