package controller

import (
	"math"
	"sort"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func approxEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

func makeVol(uuid, status string, migrating bool) webapi.VolumeInfo {
	return webapi.VolumeInfo{UUID: uuid, Status: status, Migrating: migrating}
}

func makeNodeInfo(uuid, status string, healthy bool) webapi.StorageNodeInfo {
	return webapi.StorageNodeInfo{UUID: uuid, Status: status, Healthy: healthy}
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
		got := computeLatencyDeviationPct(tc.base, tc.current)
		if !approxEqual(got, tc.want, 0.001) {
			t.Errorf("%s: got %.3f, want %.3f", tc.name, got, tc.want)
		}
	}
}

// ── U-02: computeLatencyDeviationPct — fractional precision ──────────────────

func TestComputeLatencyDeviationPct_Precision(t *testing.T) {
	// 1333 ns / 1000 ns baseline → 33.3% deviation
	got := computeLatencyDeviationPct(1000, 1333)
	if !approxEqual(got, 33.3, 0.1) {
		t.Errorf("got %.3f, want ~33.3", got)
	}
}

// ── U-03: computeLatencyDeviationPct — threshold boundary ────────────────────

func TestComputeLatencyDeviationPct_ThresholdBoundary(t *testing.T) {
	threshold := 20.0

	// Exactly 20% above baseline.
	at := computeLatencyDeviationPct(1000, 1200)
	if !approxEqual(at, threshold, 0.001) {
		t.Errorf("expected exactly 20%%, got %.3f", at)
	}
	// nodesAboveThreshold uses strict >, so 20.0 must NOT trigger migration.
	if len(nodesAboveThreshold(map[string]float64{"n": at}, threshold)) != 0 {
		t.Error("node at exactly the threshold should not be above it")
	}

	// One nanosecond pushes it over: baseline=1000, current=1201 → 20.1%
	over := computeLatencyDeviationPct(1000, 1201)
	if over <= threshold {
		t.Errorf("expected >20%%, got %.3f", over)
	}
	if len(nodesAboveThreshold(map[string]float64{"n": over}, threshold)) != 1 {
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
		got := volumeIOScore(tc.iops, tc.throughputBps, tc.iopsWeight, tc.throughputMBWt)
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
		result[i] = scored{v.id, volumeIOScore(v.iops, v.tp, 1.0, 0.1)}
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
	low := volumeIOScore(100, 10e6, 1.0, 0.1)
	high := volumeIOScore(100, 100e6, 1.0, 0.1)
	if high <= low {
		t.Errorf("higher throughput should yield higher score: low=%.2f high=%.2f", low, high)
	}
}

// ── U-06: nodesAboveThreshold — ordering and boundary ────────────────────────

func TestNodesAboveThreshold_Ordering(t *testing.T) {
	deviations := map[string]float64{
		"A": 15, // below
		"B": 35,
		"C": 50,
		"D": 20, // at threshold → excluded (strict >)
	}
	got := nodesAboveThreshold(deviations, 20)
	if len(got) != 2 {
		t.Fatalf("expected 2 hot nodes, got %d: %v", len(got), got)
	}
	// C (50%) must come before B (35%).
	if got[0] != "C" || got[1] != "B" {
		t.Errorf("wrong order: %v (want [C B])", got)
	}
}

func TestNodesAboveThreshold_NoneAbove(t *testing.T) {
	got := nodesAboveThreshold(map[string]float64{"A": 10, "B": 19}, 20)
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestNodesAboveThreshold_AllAbove(t *testing.T) {
	got := nodesAboveThreshold(map[string]float64{"A": 21, "B": 22}, 20)
	if len(got) != 2 {
		t.Errorf("expected 2, got %d", len(got))
	}
}

func TestNodesAboveThreshold_EmptyMap(t *testing.T) {
	got := nodesAboveThreshold(map[string]float64{}, 20)
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

// Zero threshold: all positive deviations are hot.
func TestNodesAboveThreshold_ZeroThreshold(t *testing.T) {
	got := nodesAboveThreshold(map[string]float64{"A": 0.1, "B": 0}, 0)
	if len(got) != 1 || got[0] != "A" {
		t.Errorf("expected [A], got %v", got)
	}
}

// ── U-07: topKNodes ───────────────────────────────────────────────────────────

func TestTopKNodes_ExactK(t *testing.T) {
	got := topKNodes(map[string]float64{"a": 10, "b": 30, "c": 20}, 2)
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("expected [b c], got %v", got)
	}
}

func TestTopKNodes_KLargerThanMap(t *testing.T) {
	got := topKNodes(map[string]float64{"a": 1, "b": 2}, 10)
	if len(got) != 2 || got[0] != "b" {
		t.Errorf("expected [b a], got %v", got)
	}
}

func TestTopKNodes_KZero(t *testing.T) {
	got := topKNodes(map[string]float64{"a": 10}, 0)
	if len(got) != 0 {
		t.Errorf("expected empty for k=0, got %v", got)
	}
}

func TestTopKNodes_EmptyMap(t *testing.T) {
	got := topKNodes(map[string]float64{}, 3)
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestTopKNodes_K1(t *testing.T) {
	got := topKNodes(map[string]float64{"a": 5, "b": 50, "c": 15}, 1)
	if len(got) != 1 || got[0] != "b" {
		t.Errorf("expected [b], got %v", got)
	}
}

// ── U-08: deviationStats ─────────────────────────────────────────────────────

func TestDeviationStats_Basic(t *testing.T) {
	maxDev, avgDev, hottest, coolest := deviationStats(map[string]float64{
		"A": 10, "B": 30, "C": 20,
	})
	if !approxEqual(maxDev, 30, 0.001) {
		t.Errorf("maxDev: got %.3f, want 30", maxDev)
	}
	if !approxEqual(avgDev, 20, 0.001) {
		t.Errorf("avgDev: got %.3f, want 20", avgDev)
	}
	if hottest != "B" {
		t.Errorf("hottest: got %q, want B", hottest)
	}
	if coolest != "A" {
		t.Errorf("coolest: got %q, want A", coolest)
	}
}

func TestDeviationStats_Empty(t *testing.T) {
	maxDev, avgDev, hottest, coolest := deviationStats(map[string]float64{})
	if maxDev != 0 || avgDev != 0 || hottest != "" || coolest != "" {
		t.Errorf("expected all zero/empty, got max=%.1f avg=%.1f hot=%q cool=%q",
			maxDev, avgDev, hottest, coolest)
	}
}

func TestDeviationStats_SingleNode(t *testing.T) {
	maxDev, avgDev, hottest, coolest := deviationStats(map[string]float64{"n": 42})
	if !approxEqual(maxDev, 42, 0.001) || !approxEqual(avgDev, 42, 0.001) {
		t.Errorf("single-node: max=%.1f avg=%.1f", maxDev, avgDev)
	}
	if hottest != "n" || coolest != "n" {
		t.Errorf("single-node: hottest=%q coolest=%q", hottest, coolest)
	}
}

func TestDeviationStats_TwoNodes(t *testing.T) {
	maxDev, avgDev, hottest, coolest := deviationStats(map[string]float64{
		"hot": 60, "cool": 10,
	})
	if !approxEqual(maxDev, 60, 0.001) {
		t.Errorf("maxDev: got %.3f, want 60", maxDev)
	}
	if !approxEqual(avgDev, 35, 0.001) {
		t.Errorf("avgDev: got %.3f, want 35", avgDev)
	}
	if hottest != "hot" {
		t.Errorf("hottest: got %q, want hot", hottest)
	}
	if coolest != "cool" {
		t.Errorf("coolest: got %q, want cool", coolest)
	}
}

func TestDeviationStats_AllZero(t *testing.T) {
	maxDev, avgDev, _, _ := deviationStats(map[string]float64{"A": 0, "B": 0})
	if maxDev != 0 || avgDev != 0 {
		t.Errorf("all-zero: max=%.1f avg=%.1f", maxDev, avgDev)
	}
}

// ── U-09: selectLatencyTarget ─────────────────────────────────────────────────

func TestSelectLatencyTarget_PicksLowestDeviation(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	nodeMap := map[string]webapi.StorageNodeInfo{
		"src": makeNodeInfo("src", "online", true),
		"B":   makeNodeInfo("B", "online", true),
		"C":   makeNodeInfo("C", "online", true), // lowest → target
		"D":   makeNodeInfo("D", "online", true),
	}
	deviations := map[string]float64{"src": 50, "B": 15, "C": 5, "D": 25}
	if got := r.selectLatencyTarget(nodeMap, deviations, "src"); got != "C" {
		t.Errorf("expected C (deviation=5), got %q", got)
	}
}

func TestSelectLatencyTarget_SkipsOfflineAndUnhealthy(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	nodeMap := map[string]webapi.StorageNodeInfo{
		"src": makeNodeInfo("src", "online", true),
		"B":   makeNodeInfo("B", "offline", true),  // offline → skip
		"C":   makeNodeInfo("C", "online", false),   // unhealthy → skip
		"D":   makeNodeInfo("D", "online", true),
	}
	deviations := map[string]float64{"src": 50, "B": 0, "C": 0, "D": 10}
	if got := r.selectLatencyTarget(nodeMap, deviations, "src"); got != "D" {
		t.Errorf("expected D (only valid target), got %q", got)
	}
}

func TestSelectLatencyTarget_NoValidTargets(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	nodeMap := map[string]webapi.StorageNodeInfo{
		"src": makeNodeInfo("src", "online", true),
	}
	if got := r.selectLatencyTarget(nodeMap, map[string]float64{"src": 50}, "src"); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// Unmeasured nodes (no entry in deviations map) default to deviation=0 and rank lowest.
func TestSelectLatencyTarget_UnmeasuredNodesPreferred(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	nodeMap := map[string]webapi.StorageNodeInfo{
		"src": makeNodeInfo("src", "online", true),
		"B":   makeNodeInfo("B", "online", true), // no entry in deviations
		"C":   makeNodeInfo("C", "online", true),
	}
	deviations := map[string]float64{"src": 50, "C": 15}
	if got := r.selectLatencyTarget(nodeMap, deviations, "src"); got != "B" {
		t.Errorf("expected B (deviation=0), got %q", got)
	}
}

// Two-node cluster: only one target available.
func TestSelectLatencyTarget_TwoNodeCluster(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	nodeMap := map[string]webapi.StorageNodeInfo{
		"src":    makeNodeInfo("src", "online", true),
		"target": makeNodeInfo("target", "online", true),
	}
	deviations := map[string]float64{"src": 40, "target": 5}
	if got := r.selectLatencyTarget(nodeMap, deviations, "src"); got != "target" {
		t.Errorf("expected target, got %q", got)
	}
}

// Even if target is also degraded, it should still be picked (it's the best available).
func TestSelectLatencyTarget_AllTargetsDegraded(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	nodeMap := map[string]webapi.StorageNodeInfo{
		"src": makeNodeInfo("src", "online", true),
		"B":   makeNodeInfo("B", "online", true),
		"C":   makeNodeInfo("C", "online", true),
	}
	deviations := map[string]float64{"src": 80, "B": 30, "C": 25}
	// C has lower deviation → should be picked even though both are degraded.
	if got := r.selectLatencyTarget(nodeMap, deviations, "src"); got != "C" {
		t.Errorf("expected C (deviation=25, lowest), got %q", got)
	}
}

// ── U-10: filterEligibleVolumes ───────────────────────────────────────────────

func TestFilterEligibleVolumes_AllClean(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	r.init()
	vols := []webapi.VolumeInfo{
		makeVol("v1", "online", false),
		makeVol("v2", "online", false),
	}
	got := r.filterEligibleVolumes(vols, "cluster", map[string]bool{})
	if len(got) != 2 {
		t.Errorf("expected 2 eligible, got %d", len(got))
	}
}

func TestFilterEligibleVolumes_PinnedExcluded(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	r.init()
	vols := []webapi.VolumeInfo{
		makeVol("pinned", "online", false),
		makeVol("free", "online", false),
	}
	got := r.filterEligibleVolumes(vols, "cluster", map[string]bool{"pinned": true})
	if len(got) != 1 || got[0].UUID != "free" {
		t.Errorf("expected only 'free', got %+v", got)
	}
}

func TestFilterEligibleVolumes_OfflineExcluded(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	r.init()
	vols := []webapi.VolumeInfo{
		makeVol("degraded", "degraded", false),
		makeVol("ok", "online", false),
	}
	got := r.filterEligibleVolumes(vols, "cluster", map[string]bool{})
	if len(got) != 1 || got[0].UUID != "ok" {
		t.Errorf("expected only 'ok', got %+v", got)
	}
}

func TestFilterEligibleVolumes_MigratingExcluded(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	r.init()
	// online=true, migrating=true → must be excluded (handles operator-restart case)
	vols := []webapi.VolumeInfo{
		makeVol("migrating", "online", true),
		makeVol("stable", "online", false),
	}
	got := r.filterEligibleVolumes(vols, "cluster", map[string]bool{})
	if len(got) != 1 || got[0].UUID != "stable" {
		t.Errorf("expected only 'stable', got %+v", got)
	}
}

func TestFilterEligibleVolumes_ActiveCoolDown(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	r.init()
	r.mu.Lock()
	r.coolDownMap["cluster/v-hot"] = time.Now().Add(10 * time.Minute)
	r.mu.Unlock()

	vols := []webapi.VolumeInfo{
		makeVol("v-hot", "online", false),  // in cool-down
		makeVol("v-ok", "online", false),
	}
	got := r.filterEligibleVolumes(vols, "cluster", map[string]bool{})
	if len(got) != 1 || got[0].UUID != "v-ok" {
		t.Errorf("expected only 'v-ok', got %+v", got)
	}
}

func TestFilterEligibleVolumes_ExpiredCoolDown(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	r.init()
	// Expired cool-down: the volume should be eligible again.
	r.mu.Lock()
	r.coolDownMap["cluster/v-ex"] = time.Now().Add(-1 * time.Second)
	r.mu.Unlock()

	vols := []webapi.VolumeInfo{makeVol("v-ex", "online", false)}
	got := r.filterEligibleVolumes(vols, "cluster", map[string]bool{})
	if len(got) != 1 {
		t.Errorf("expected 1 eligible (expired cool-down), got %d", len(got))
	}
}

func TestFilterEligibleVolumes_Mixed(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	r.init()
	r.mu.Lock()
	r.coolDownMap["cluster/cd"] = time.Now().Add(5 * time.Minute)
	r.mu.Unlock()

	vols := []webapi.VolumeInfo{
		makeVol("pinned", "online", false),
		makeVol("cd", "online", false),       // in cool-down
		makeVol("offline", "degraded", false),
		makeVol("migrating", "online", true),
		makeVol("ok", "online", false),
	}
	pinned := map[string]bool{"pinned": true}
	got := r.filterEligibleVolumes(vols, "cluster", pinned)
	if len(got) != 1 || got[0].UUID != "ok" {
		t.Errorf("expected only 'ok', got %+v", got)
	}
}
