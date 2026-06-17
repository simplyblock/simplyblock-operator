package volumemigration

import (
	"sort"
)

// ComputeLatencyDeviationPct returns how much currentP99 exceeds baselineP99 as a
// percentage. Returns 0 when either value is non-positive or when current latency
// is at or below baseline (no degradation).
func ComputeLatencyDeviationPct(
	baselineP99, currentP99 int64,
) float64 {
	if baselineP99 <= 0 || currentP99 <= 0 {
		return 0
	}
	dev := float64(currentP99-baselineP99) / float64(baselineP99) * 100
	if dev < 0 {
		return 0
	}
	return dev
}

// topKNodes returns the top-k StorageNodeCandidates sorted by Score descending.
// If k exceeds the number of candidates the full list is returned.
// Used in Phase 2 source-candidate selection to evaluate the k hottest nodes
// for migratable load before picking the best source (§6 Step 2, Phase 2).
func topKNodes(
	candidates []StorageNodeCandidate,
	k int,
) []StorageNodeCandidate {
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	if k > len(candidates) {
		k = len(candidates)
	}
	out := make([]StorageNodeCandidate, k)
	for i := range out {
		out[i] = candidates[i]
	}
	return out
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
