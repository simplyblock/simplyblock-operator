package autobalancing

import (
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func makeStorageNode(uuid, clusterUUID string) volumemigration.StorageNode {
	return volumemigration.StorageNode{UUID: uuid, ClusterUUID: clusterUUID}
}

func makeInput(ns string, nodes ...volumemigration.StorageNode) StorageNodeSelectorInput {
	return StorageNodeSelectorInput{Namespace: ns, StorageNodes: nodes}
}

func makeVP(uuid, nodeUUID, poolUUID, status string, migrating bool, iops float64) VolumePlacement {
	return VolumePlacement{
		VolumeInfo: webapi.VolumeInfo{
			UUID:            uuid,
			PrimaryNodeUUID: nodeUUID,
			Status:          status,
			Migrating:       migrating,
			IOPS:            iops,
		},
		PoolUUID: poolUUID,
	}
}

func neverCooling(_ string) bool { return false }

// volsByNode builds the volumesByNode map expected by SelectVolumesForMigration.
func volsByNode(vps ...VolumePlacement) map[string][]VolumePlacement {
	m := make(map[string][]VolumePlacement)
	for _, vp := range vps {
		m[vp.PrimaryNodeUUID] = append(m[vp.PrimaryNodeUUID], vp)
	}
	return m
}

// ── distinctClusterUUIDs ─────────────────────────────────────────────────────

func TestDistinctClusterUUIDs_Basic(t *testing.T) {
	inputs := []StorageNodeSelectorInput{
		makeInput("ns1",
			makeStorageNode("n1", "c1"),
			makeStorageNode("n2", "c1"),
			makeStorageNode("n3", "c2"),
		),
		makeInput("ns2",
			makeStorageNode("n4", "c2"),
			makeStorageNode("n5", "c3"),
		),
	}
	if len(distinctClusterUUIDs(inputs)) != 3 {
		t.Fatalf("expected 3 distinct clusters, got %v", distinctClusterUUIDs(inputs))
	}
}

func TestDistinctClusterUUIDs_SkipsEmpty(t *testing.T) {
	inputs := []StorageNodeSelectorInput{
		makeInput("ns", makeStorageNode("n1", ""), makeStorageNode("n2", "c1")),
	}
	got := distinctClusterUUIDs(inputs)
	if len(got) != 1 || got[0] != "c1" {
		t.Errorf("expected [c1], got %v", got)
	}
}

func TestDistinctClusterUUIDs_Empty(t *testing.T) {
	if len(distinctClusterUUIDs(nil)) != 0 {
		t.Error("expected empty")
	}
}

// ── deviationStats ────────────────────────────────────────────────────────────

func TestDeviationStats_SingleCluster(t *testing.T) {
	latency := map[string]nodeLatencyData{
		"n1": {clusterUUID: "c1"},
		"n2": {clusterUUID: "c1"},
		"n3": {clusterUUID: "c1"},
	}
	devs := map[string]float64{"n1": 50, "n2": 20, "n3": 10}
	s, ok := deviationStats(latency, devs)["c1"]
	if !ok {
		t.Fatal("expected cluster c1")
	}
	if s.HottestNodeUUID != "n1" {
		t.Errorf("hottest = %q, want n1", s.HottestNodeUUID)
	}
	if s.CoolestNodeUUID != "n3" {
		t.Errorf("coolest = %q, want n3", s.CoolestNodeUUID)
	}
	if s.MaxDeviationPct != 50 {
		t.Errorf("max = %.1f, want 50", s.MaxDeviationPct)
	}
	if s.AvgDeviationPct != 80.0/3.0 {
		t.Errorf("avg = %.4f, want %.4f", s.AvgDeviationPct, 80.0/3.0)
	}
}

func TestDeviationStats_MultiCluster(t *testing.T) {
	latency := map[string]nodeLatencyData{
		"n1": {clusterUUID: "c1"},
		"n2": {clusterUUID: "c2"},
	}
	devs := map[string]float64{"n1": 30, "n2": 60}
	stats := deviationStats(latency, devs)
	if len(stats) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(stats))
	}
	if stats["c1"].MaxDeviationPct != 30 || stats["c2"].MaxDeviationPct != 60 {
		t.Errorf("unexpected stats: %v", stats)
	}
}

func TestDeviationStats_NodeMissingFromLatency(t *testing.T) {
	// n2 appears in devs but not latency — must be ignored.
	latency := map[string]nodeLatencyData{"n1": {clusterUUID: "c1"}}
	devs := map[string]float64{"n1": 20, "n2": 80}
	stats := deviationStats(latency, devs)
	if stats["c1"].MaxDeviationPct != 20 {
		t.Errorf("max = %.1f, want 20 (n2 should be ignored)", stats["c1"].MaxDeviationPct)
	}
}

func TestDeviationStats_Empty(t *testing.T) {
	if len(deviationStats(nil, nil)) != 0 {
		t.Error("expected empty")
	}
}

// ── FilterEligibleVolumes ─────────────────────────────────────────────────────

func TestFilterEligibleVolumes_AllPass(t *testing.T) {
	lvs := &LogicalVolumeSelector{}
	got := lvs.FilterEligibleVolumes(LogicalVolumeSelectorInput{IsCoolingDown: neverCooling}, []VolumePlacement{
		makeVP("v1", "n1", "p1", "online", false, 100),
		makeVP("v2", "n1", "p1", "online", false, 200),
	})
	if len(got) != 2 {
		t.Errorf("expected 2, got %d", len(got))
	}
}

func TestFilterEligibleVolumes_Pinned(t *testing.T) {
	lvs := &LogicalVolumeSelector{}
	got := lvs.FilterEligibleVolumes(
		LogicalVolumeSelectorInput{Pinned: map[string]bool{"v1": true}},
		[]VolumePlacement{makeVP("v1", "n1", "p1", "online", false, 0), makeVP("v2", "n1", "p1", "online", false, 0)},
	)
	if len(got) != 1 || got[0].UUID != "v2" {
		t.Errorf("expected only v2, got %v", got)
	}
}

func TestFilterEligibleVolumes_CoolingDown(t *testing.T) {
	lvs := &LogicalVolumeSelector{}
	got := lvs.FilterEligibleVolumes(
		LogicalVolumeSelectorInput{IsCoolingDown: func(uuid string) bool { return uuid == "v1" }},
		[]VolumePlacement{makeVP("v1", "n1", "p1", "online", false, 0), makeVP("v2", "n1", "p1", "online", false, 0)},
	)
	if len(got) != 1 || got[0].UUID != "v2" {
		t.Errorf("expected only v2, got %v", got)
	}
}

func TestFilterEligibleVolumes_NotOnline(t *testing.T) {
	lvs := &LogicalVolumeSelector{}
	got := lvs.FilterEligibleVolumes(LogicalVolumeSelectorInput{}, []VolumePlacement{
		makeVP("v1", "n1", "p1", "degraded", false, 0),
		makeVP("v2", "n1", "p1", "online", false, 0),
	})
	if len(got) != 1 || got[0].UUID != "v2" {
		t.Errorf("expected only v2, got %v", got)
	}
}

func TestFilterEligibleVolumes_Migrating(t *testing.T) {
	lvs := &LogicalVolumeSelector{}
	got := lvs.FilterEligibleVolumes(LogicalVolumeSelectorInput{}, []VolumePlacement{
		makeVP("v1", "n1", "p1", "online", true, 0),
		makeVP("v2", "n1", "p1", "online", false, 0),
	})
	if len(got) != 1 || got[0].UUID != "v2" {
		t.Errorf("expected only v2, got %v", got)
	}
}

func TestFilterEligibleVolumes_NilCoolingDown(t *testing.T) {
	lvs := &LogicalVolumeSelector{}
	got := lvs.FilterEligibleVolumes(LogicalVolumeSelectorInput{IsCoolingDown: nil}, []VolumePlacement{
		makeVP("v1", "n1", "p1", "online", false, 0),
	})
	if len(got) != 1 {
		t.Errorf("nil IsCoolingDown must not exclude volumes, got %d", len(got))
	}
}

// ── SelectVolumesForMigration (tests selectMigrationSet indirectly) ───────────

func TestSelectVolumesForMigration_AlwaysIncludesFirst(t *testing.T) {
	// Single very-high-score volume: budget = 10%*1000 = 100, score=1000 > budget,
	// but must be included as first candidate.
	lvs := &LogicalVolumeSelector{}
	cfg := RebalancingConfig{IopsWeight: 1.0, MaxMigrations: 10}
	src, got := lvs.SelectVolumesForMigration(
		LogicalVolumeSelectorInput{},
		[]string{"n1"},
		volsByNode(makeVP("v1", "n1", "p", "online", false, 1000)),
		cfg,
	)
	if src != "n1" || len(got) != 1 || got[0].Vol.UUID != "v1" {
		t.Errorf("expected (n1,[v1]), got (%q,%v)", src, got)
	}
}

func TestSelectVolumesForMigration_BudgetCapExcludesMiddle(t *testing.T) {
	// Scores: v1=9, v2=5, v3=1. Total=15. Budget=1.5.
	// v1 included (first); budget becomes 1.5-9 = negative; v2 score 5 > remaining → excluded;
	// v3 score 1 ≤ remaining? No — budget went negative after v1.
	// But: after v1 budget = migrationBudgetFraction*15 - 9 = 1.5-9 = -7.5.
	// v3.score=1 > -7.5, excluded.
	// Only v1 should be returned.
	lvs := &LogicalVolumeSelector{}
	cfg := RebalancingConfig{IopsWeight: 1.0, MaxMigrations: 10}
	_, got := lvs.SelectVolumesForMigration(
		LogicalVolumeSelectorInput{},
		[]string{"n1"},
		volsByNode(
			makeVP("v1", "n1", "p", "online", false, 9),
			makeVP("v2", "n1", "p", "online", false, 5),
			makeVP("v3", "n1", "p", "online", false, 1),
		),
		cfg,
	)
	if len(got) != 1 || got[0].Vol.UUID != "v1" {
		t.Errorf("expected only v1 after budget cap, got %v", got)
	}
}

func TestSelectVolumesForMigration_MaxMigrationsCap(t *testing.T) {
	lvs := &LogicalVolumeSelector{}
	cfg := RebalancingConfig{IopsWeight: 1.0, MaxMigrations: 2}
	vols := make([]VolumePlacement, 5)
	for i := range vols {
		vols[i] = makeVP("v"+string(rune('a'+i)), "n1", "p", "online", false, 0)
	}
	_, got := lvs.SelectVolumesForMigration(LogicalVolumeSelectorInput{}, []string{"n1"}, volsByNode(vols...), cfg)
	if len(got) != 2 {
		t.Errorf("MaxMigrations=2, got %d", len(got))
	}
}

func TestSelectVolumesForMigration_ZeroScoresAllIncluded(t *testing.T) {
	// All scores=0 → budget=0; every score≤budget → all included up to MaxMigrations.
	lvs := &LogicalVolumeSelector{}
	cfg := RebalancingConfig{IopsWeight: 1.0, MaxMigrations: 10}
	vols := []VolumePlacement{
		makeVP("v1", "n1", "p", "online", false, 0),
		makeVP("v2", "n1", "p", "online", false, 0),
		makeVP("v3", "n1", "p", "online", false, 0),
	}
	_, got := lvs.SelectVolumesForMigration(LogicalVolumeSelectorInput{}, []string{"n1"}, volsByNode(vols...), cfg)
	if len(got) != 3 {
		t.Errorf("all-zero scores: expected 3, got %d", len(got))
	}
}

func TestSelectVolumesForMigration_SkipsNodeWithNoEligible(t *testing.T) {
	lvs := &LogicalVolumeSelector{}
	cfg := RebalancingConfig{IopsWeight: 1.0, MaxMigrations: 10}
	// hot1's only volume is pinned; selection must fall through to hot2.
	input := LogicalVolumeSelectorInput{Pinned: map[string]bool{"v1": true}}
	src, got := lvs.SelectVolumesForMigration(input, []string{"hot1", "hot2"},
		volsByNode(
			makeVP("v1", "hot1", "p", "online", false, 100),
			makeVP("v2", "hot2", "p", "online", false, 200),
		), cfg)
	if src != "hot2" || len(got) != 1 || got[0].Vol.UUID != "v2" {
		t.Errorf("expected (hot2,[v2]), got (%q,%v)", src, got)
	}
}

func TestSelectVolumesForMigration_RanksHighestScoreFirst(t *testing.T) {
	lvs := &LogicalVolumeSelector{}
	cfg := RebalancingConfig{IopsWeight: 1.0, MaxMigrations: 10}
	_, got := lvs.SelectVolumesForMigration(
		LogicalVolumeSelectorInput{},
		[]string{"n1"},
		volsByNode(
			makeVP("low", "n1", "p", "online", false, 10),
			makeVP("high", "n1", "p", "online", false, 500),
			makeVP("mid", "n1", "p", "online", false, 100),
		), cfg)
	if len(got) == 0 || got[0].Vol.UUID != "high" {
		t.Errorf("expected high-score first, got %v", got)
	}
}

func TestSelectVolumesForMigration_NoHotNodes(t *testing.T) {
	lvs := &LogicalVolumeSelector{}
	cfg := RebalancingConfig{IopsWeight: 1.0, MaxMigrations: 10}
	src, got := lvs.SelectVolumesForMigration(LogicalVolumeSelectorInput{}, nil, nil, cfg)
	if src != "" || len(got) != 0 {
		t.Errorf("expected empty, got src=%q candidates=%v", src, got)
	}
}

// ── SelectStorageNodes (pure-logic unit, no external deps) ───────────────────

func TestSelectStorageNodes_DeviationStatsAndPairing(t *testing.T) {
	// Validate the building blocks SelectStorageNodes relies on:
	// deviationStats groups by cluster and picks hottest/coolest correctly.
	latency := map[string]nodeLatencyData{
		"hot":  {clusterUUID: "c1"},
		"warm": {clusterUUID: "c1"},
		"cool": {clusterUUID: "c1"},
	}
	devs := map[string]float64{"hot": 60, "warm": 25, "cool": 5}
	stats := deviationStats(latency, devs)["c1"]

	if stats.HottestNodeUUID != "hot" {
		t.Errorf("hottest = %q, want hot", stats.HottestNodeUUID)
	}
	if stats.CoolestNodeUUID != "cool" {
		t.Errorf("coolest = %q, want cool", stats.CoolestNodeUUID)
	}

	// Nodes above threshold 20 %: hot(60) and warm(25); cool(5) excluded.
	hotNodes := volumemigration.NodesAboveThreshold(devs, 20)
	if len(hotNodes) != 2 {
		t.Fatalf("expected 2 hot nodes, got %d: %v", len(hotNodes), hotNodes)
	}

	// Simulate the pairing loop in SelectStorageNodes.
	var pairs []NodeMigrationPair
	for _, src := range hotNodes {
		if stats.CoolestNodeUUID != "" && stats.CoolestNodeUUID != src {
			pairs = append(pairs, NodeMigrationPair{
				ClusterUUID:    "c1",
				SourceNodeUUID: src,
				TargetNodeUUID: stats.CoolestNodeUUID,
			})
		}
	}
	if len(pairs) != 2 {
		t.Fatalf("expected 2 pairs, got %d", len(pairs))
	}
	for _, p := range pairs {
		if p.TargetNodeUUID != "cool" {
			t.Errorf("pair %s→%s: target must be cool", p.SourceNodeUUID, p.TargetNodeUUID)
		}
	}
}

func TestSelectStorageNodes_SourceEqualsCoolest_NoPair(t *testing.T) {
	// Single-node cluster: source == coolest, so no pair should be produced.
	latency := map[string]nodeLatencyData{"only": {clusterUUID: "c1"}}
	devs := map[string]float64{"only": 50}
	stats := deviationStats(latency, devs)["c1"]

	var pairs []NodeMigrationPair
	for _, src := range volumemigration.NodesAboveThreshold(devs, 20) {
		if stats.CoolestNodeUUID != "" && stats.CoolestNodeUUID != src {
			pairs = append(pairs, NodeMigrationPair{SourceNodeUUID: src, TargetNodeUUID: stats.CoolestNodeUUID})
		}
	}
	if len(pairs) != 0 {
		t.Errorf("single node: expected no pairs, got %v", pairs)
	}
}

// ── isCoolingDown closure capture ────────────────────────────────────────────

func TestIsCoolingDownClosureCapturesClusterUUID(t *testing.T) {
	called := map[string]string{}
	isCoolingDown := func(clusterUUID, volumeUUID string) bool {
		called[volumeUUID] = clusterUUID
		return false
	}

	for _, cUUID := range []string{"c1", "c2"} {
		fn := func(clusterUUID string) func(string) bool {
			return func(volumeUUID string) bool {
				return isCoolingDown(clusterUUID, volumeUUID)
			}
		}(cUUID)
		fn("vol-" + cUUID)
	}

	if called["vol-c1"] != "c1" || called["vol-c2"] != "c2" {
		t.Errorf("closure captured wrong cluster UUID: %v", called)
	}
}