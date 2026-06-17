package controller

import (
	"math"
	"testing"

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
	r.migrationState.PushMigration("cluster", "", "v-hot", "", 600)

	vols := []webapi.VolumeInfo{
		makeVol("v-hot", "online", false), // in cool-down
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
	r.migrationState.PushMigration("cluster", "", "v-ex", "", -1)

	vols := []webapi.VolumeInfo{makeVol("v-ex", "online", false)}
	got := r.filterEligibleVolumes(vols, "cluster", map[string]bool{})
	if len(got) != 1 {
		t.Errorf("expected 1 eligible (expired cool-down), got %d", len(got))
	}
}

func TestFilterEligibleVolumes_Mixed(t *testing.T) {
	r := &VolumeRebalancerReconciler{}
	r.init()
	r.migrationState.PushMigration("cluster", "", "cd", "", 300)

	vols := []webapi.VolumeInfo{
		makeVol("pinned", "online", false),
		makeVol("cd", "online", false), // in cool-down
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
		"B":   makeNodeInfo("B", "offline", true), // offline → skip
		"C":   makeNodeInfo("C", "online", false), // unhealthy → skip
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
