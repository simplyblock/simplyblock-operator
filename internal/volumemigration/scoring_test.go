package volumemigration

import (
	"math"
	"sort"
	"testing"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func approxEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

// ── U-01: computeLatencyDeviationPct — basic cases ───────────────────────────

func TestComputeLatencyDeviationPct_Basic(t *testing.T) {
	cases := []struct {
		name    string
		base    int64
		current int64
		want    float64
	}{
		{"equal — no degradation", 1000, 1000, 0},
		{"50% above baseline", 1000, 1500, 50},
		{"100% above baseline", 1000, 2000, 100},
		{"current below baseline — clamped to 0", 1000, 800, 0},
		{"zero baseline — returns 0", 0, 1500, 0},
		{"zero current — returns 0", 1000, 0, 0},
		{"both zero — returns 0", 0, 0, 0},
		{"negative baseline — returns 0", -100, 1500, 0},
	}
	for _, tc := range cases {
		got := ComputeLatencyDeviationPct(tc.base, tc.current)
		if !approxEqual(got, tc.want, 0.001) {
			t.Errorf("%s: got %.3f, want %.3f", tc.name, got, tc.want)
		}
	}
}

// ── U-02: computeLatencyDeviationPct — fractional precision ──────────────────

func TestComputeLatencyDeviationPct_Precision(t *testing.T) {
	// 1333 ns / 1000 ns baseline → 33.3% deviation
	got := ComputeLatencyDeviationPct(1000, 1333)
	if !approxEqual(got, 33.3, 0.1) {
		t.Errorf("got %.3f, want ~33.3", got)
	}
}

// ── U-03: computeLatencyDeviationPct — threshold boundary ────────────────────

func TestComputeLatencyDeviationPct_ThresholdBoundary(t *testing.T) {
	threshold := 20.0

	// Exactly 20% above baseline.
	at := ComputeLatencyDeviationPct(1000, 1200)
	if !approxEqual(at, threshold, 0.001) {
		t.Errorf("expected exactly 20%%, got %.3f", at)
	}
	// NodesAboveThreshold uses strict >, so 20.0 must NOT trigger migration.
	if len(NodesAboveThreshold(map[string]float64{"n": at}, threshold)) != 0 {
		t.Error("node at exactly the threshold should not be above it")
	}

	// One nanosecond pushes it over: baseline=1000, current=1201 → 20.1%
	over := ComputeLatencyDeviationPct(1000, 1201)
	if over <= threshold {
		t.Errorf("expected >20%%, got %.3f", over)
	}
	if len(NodesAboveThreshold(map[string]float64{"n": over}, threshold)) != 1 {
		t.Error("node just above threshold should trigger migration")
	}
}

// ── U-04: volumeIOScore — weight formula ─────────────────────────────────────

func TestVolumeIOScore_Formula(t *testing.T) {
	cases := []struct {
		name           string
		iops           float64
		throughputBps  float64
		iopsWeight     float64
		throughputMBWt float64
		want           float64
	}{
		{
			"iops-only",
			1000, 0, 1.0, 0.1,
			1000,
		},
		{
			"throughput-only — bytes converted to MB before weighting",
			0, 100e6, 1.0, 0.1,
			10, // 0.1 × 100
		},
		{
			"combined",
			500, 200e6, 1.0, 0.1,
			520, // 500 + 0.1×200
		},
		{
			"custom weights",
			100, 50e6, 2.0, 0.5,
			225, // 2×100 + 0.5×50
		},
		{
			"both zero",
			0, 0, 1.0, 0.1,
			0,
		},
	}
	for _, tc := range cases {
		got := VolumeIOScore(tc.iops, tc.throughputBps, tc.iopsWeight, tc.throughputMBWt)
		if !approxEqual(got, tc.want, 0.01) {
			t.Errorf("%s: got %.4f, want %.4f", tc.name, got, tc.want)
		}
	}
}

// ── U-05: volumeIOScore — ranking order ──────────────────────────────────────

func TestVolumeIOScore_Ranking(t *testing.T) {
	type vol struct {
		id   string
		iops float64
		tp   float64
	}
	vols := []vol{
		{"high", 1000, 500e6},
		{"mid", 200, 100e6},
		{"low", 10, 5e6},
	}
	type scored struct {
		id    string
		score float64
	}
	result := make([]scored, len(vols))
	for i, v := range vols {
		result[i] = scored{v.id, VolumeIOScore(v.iops, v.tp, 1.0, 0.1)}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].score > result[j].score })

	want := []string{"high", "mid", "low"}
	for i, w := range want {
		if result[i].id != w {
			t.Errorf("rank %d: got %q, want %q", i, result[i].id, w)
		}
	}
}

// throughput-dominated score still ranks correctly when IOPS is the same.
func TestVolumeIOScore_ThroughputTieBreak(t *testing.T) {
	// Same IOPS, different throughput.
	low := VolumeIOScore(100, 10e6, 1.0, 0.1)
	high := VolumeIOScore(100, 100e6, 1.0, 0.1)
	if high <= low {
		t.Errorf("higher throughput should yield higher score: low=%.2f high=%.2f", low, high)
	}
}

// ── U-06: NodesAboveThreshold — ordering and boundary ────────────────────────

func TestNodesAboveThreshold_Ordering(t *testing.T) {
	deviations := map[string]float64{
		"A": 15, // below
		"B": 35,
		"C": 50,
		"D": 20, // at threshold → excluded (strict >)
	}
	got := NodesAboveThreshold(deviations, 20)
	if len(got) != 2 {
		t.Fatalf("expected 2 hot nodes, got %d: %v", len(got), got)
	}
	// C (50%) must come before B (35%).
	if got[0] != "C" || got[1] != "B" {
		t.Errorf("wrong order: %v (want [C B])", got)
	}
}

func TestNodesAboveThreshold_NoneAbove(t *testing.T) {
	got := NodesAboveThreshold(map[string]float64{"A": 10, "B": 19}, 20)
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestNodesAboveThreshold_AllAbove(t *testing.T) {
	got := NodesAboveThreshold(map[string]float64{"A": 21, "B": 22}, 20)
	if len(got) != 2 {
		t.Errorf("expected 2, got %d", len(got))
	}
}

func TestNodesAboveThreshold_EmptyMap(t *testing.T) {
	got := NodesAboveThreshold(map[string]float64{}, 20)
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

// Zero threshold: all positive deviations are hot.
func TestNodesAboveThreshold_ZeroThreshold(t *testing.T) {
	got := NodesAboveThreshold(map[string]float64{"A": 0.1, "B": 0}, 0)
	if len(got) != 1 || got[0] != "A" {
		t.Errorf("expected [A], got %v", got)
	}
}

// ── U-07: topKNodes ───────────────────────────────────────────────────────────

func candidates(scores map[string]float64) []StorageNodeCandidate {
	out := make([]StorageNodeCandidate, 0, len(scores))
	for uuid, score := range scores {
		out = append(out, StorageNodeCandidate{StorageNode: StorageNode{UUID: uuid}, Score: score})
	}
	return out
}

func uuids(cs []StorageNodeCandidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.UUID
	}
	return out
}

func TestTopKNodes_ExactK(t *testing.T) {
	got := topKNodes(candidates(map[string]float64{"a": 10, "b": 30, "c": 20}), 2)
	ids := uuids(got)
	if len(ids) != 2 || ids[0] != "b" || ids[1] != "c" {
		t.Errorf("expected [b c], got %v", ids)
	}
}

func TestTopKNodes_KLargerThanSlice(t *testing.T) {
	got := topKNodes(candidates(map[string]float64{"a": 1, "b": 2}), 10)
	ids := uuids(got)
	if len(ids) != 2 || ids[0] != "b" {
		t.Errorf("expected [b a], got %v", ids)
	}
}

func TestTopKNodes_KZero(t *testing.T) {
	got := topKNodes(candidates(map[string]float64{"a": 10}), 0)
	if len(got) != 0 {
		t.Errorf("expected empty for k=0, got %v", got)
	}
}

func TestTopKNodes_EmptySlice(t *testing.T) {
	got := topKNodes([]StorageNodeCandidate{}, 3)
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestTopKNodes_K1(t *testing.T) {
	got := topKNodes(candidates(map[string]float64{"a": 5, "b": 50, "c": 15}), 1)
	ids := uuids(got)
	if len(ids) != 1 || ids[0] != "b" {
		t.Errorf("expected [b], got %v", ids)
	}
}
