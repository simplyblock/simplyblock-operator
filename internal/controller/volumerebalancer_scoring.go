/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"sort"
)

// computeLatencyDeviationPct returns how much currentP99 exceeds baselineP99 as a
// percentage. Returns 0 when either value is non-positive or when current latency
// is at or below baseline (no degradation).
func computeLatencyDeviationPct(baselineP99, currentP99 int64) float64 {
	if baselineP99 <= 0 || currentP99 <= 0 {
		return 0
	}
	dev := float64(currentP99-baselineP99) / float64(baselineP99) * 100
	if dev < 0 {
		return 0
	}
	return dev
}

// volumeIOScore computes the migration priority score for a single volume.
// A higher score means the volume contributes more I/O load and should be
// migrated first.
//
// throughputBytesPerSec is normalised to MB/s before weighting so both terms
// are on a comparable numerical scale. Sensible defaults: iopsWeight=1.0,
// throughputMBWeight=0.1 (1 MB/s ≈ 0.1 of a IOPS unit).
func volumeIOScore(iops, throughputBytesPerSec, iopsWeight, throughputMBWeight float64) float64 {
	return iopsWeight*iops + throughputMBWeight*(throughputBytesPerSec/1e6)
}

// nodesAboveThreshold returns the UUIDs of nodes whose latency deviation exceeds
// threshold, sorted by deviation descending (worst node first).
func nodesAboveThreshold(deviations map[string]float64, threshold float64) []string {
	type entry struct {
		uuid      string
		deviation float64
	}
	hot := make([]entry, 0, len(deviations))
	for uuid, dev := range deviations {
		if dev > threshold {
			hot = append(hot, entry{uuid, dev})
		}
	}
	sort.Slice(hot, func(i, j int) bool { return hot[i].deviation > hot[j].deviation })
	out := make([]string, len(hot))
	for i, e := range hot {
		out[i] = e.uuid
	}
	return out
}

// topKNodes returns up to k node UUIDs from the given score map sorted by score
// descending. Used for source-candidate selection when iterating hot nodes.
func topKNodes(scores map[string]float64, k int) []string {
	type entry struct {
		uuid  string
		score float64
	}
	entries := make([]entry, 0, len(scores))
	for u, s := range scores {
		entries = append(entries, entry{u, s})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].score > entries[j].score })
	if k > len(entries) {
		k = len(entries)
	}
	out := make([]string, k)
	for i := range out {
		out[i] = entries[i].uuid
	}
	return out
}
