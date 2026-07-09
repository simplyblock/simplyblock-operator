package volumemigration

// ComputeLatencyDeviationPct returns how much currentNS exceeds baselineNS as a
// percentage. The latencies are in nanoseconds at whichever percentile the operator
// configured (p50 or p99); the computation is percentile-agnostic. Returns 0 when
// either value is non-positive or when current latency is at or below baseline (no
// degradation).
func ComputeLatencyDeviationPct(
	baselineNS, currentNS int64,
) float64 {
	if baselineNS <= 0 || currentNS <= 0 {
		return 0
	}
	dev := float64(currentNS-baselineNS) / float64(baselineNS) * 100
	if dev < 0 {
		return 0
	}
	return dev
}

// VolumeIOScore computes the migration priority score for a single volume.
// A higher score means the volume contributes more I/O load and should be
// migrated first.
//
// throughputBytesPerSec is normalised to MB/s before weighting so both terms
// are on a comparable numerical scale. Sensible defaults: iopsWeight=1.0,
// throughputMBWeight=0.1 (1 MB/s ≈ 0.1 of a IOPS unit).
func VolumeIOScore(
	iops, throughputBytesPerSec, iopsWeight, throughputMBWeight float64,
) float64 {
	return iopsWeight*iops + throughputMBWeight*(throughputBytesPerSec/1e6)
}
