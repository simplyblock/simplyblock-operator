package autobalancing

import (
	"fmt"
	"time"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

const (
	// pinnedVolumeAnnotation is checked on the PVC; any non-empty value pins the volume.
	pinnedVolumeAnnotation = "simplyblock.io/pinned-volume"

	// Defaults applied when the spec field is nil.
	defaultEvaluationInterval    = 60 * time.Second
	defaultImbalanceThresholdPct = 80
	// defaultMinHotColdDifferencePct is the minimum latency-deviation gap (in
	// percentage points) a target node must have below the hot source before a
	// migration is worthwhile — prevents shuffling load between near-equally-loaded
	// nodes.
	defaultMinHotColdDifferencePct     = 20
	defaultCoolDownSeconds             = 60
	defaultMaxVolumeMigrationsPerCycle = 10

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
	IopsWeight              float64
	ThroughputWeight        float64
	MaxMigrations           int
	CoolDownSecs            int64
}

// ResolveRebalancingConfig applies defaults and validates the spec. It returns an error
// when prometheusURL is missing, which is the only hard requirement.
func ResolveRebalancingConfig(
	spec *simplyblockv1alpha1.VolumeRebalancingSpec,
) (RebalancingConfig, error) {
	cfg := RebalancingConfig{
		EvalInterval:            defaultEvaluationInterval,
		ImbalanceThreshold:      defaultImbalanceThresholdPct,
		MinHotColdDifferencePct: defaultMinHotColdDifferencePct,
		IopsWeight:              defaultIOPSWeight,
		ThroughputWeight:        defaultThroughputMBWeight,
		MaxMigrations:           defaultMaxVolumeMigrationsPerCycle,
		CoolDownSecs:            defaultCoolDownSeconds,
	}
	if spec.EvaluationInterval != nil && spec.EvaluationInterval.Duration > 0 {
		cfg.EvalInterval = spec.EvaluationInterval.Duration
	}
	if spec.PrometheusURL == nil || *spec.PrometheusURL == "" {
		return cfg, fmt.Errorf("spec.volumeRebalancing.prometheusURL is required")
	}
	cfg.PrometheusURL = *spec.PrometheusURL
	if spec.ImbalanceThreshold != nil {
		cfg.ImbalanceThreshold = float64(*spec.ImbalanceThreshold)
	}
	if spec.MinHotColdDifferencePct != nil {
		cfg.MinHotColdDifferencePct = float64(*spec.MinHotColdDifferencePct)
	}
	if spec.IOPSWeight != nil && *spec.IOPSWeight > 0 {
		cfg.IopsWeight = *spec.IOPSWeight
	}
	if spec.ThroughputWeight != nil && *spec.ThroughputWeight > 0 {
		cfg.ThroughputWeight = *spec.ThroughputWeight
	}
	if spec.MaxVolumeMigrationsPerCycle != nil && *spec.MaxVolumeMigrationsPerCycle > 0 {
		cfg.MaxMigrations = int(*spec.MaxVolumeMigrationsPerCycle)
	}
	if spec.DefaultCoolDownSeconds != nil {
		cfg.CoolDownSecs = int64(*spec.DefaultCoolDownSeconds)
	}
	return cfg, nil
}
