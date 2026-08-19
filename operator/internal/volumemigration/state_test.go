package volumemigration

import (
	"slices"
	"sync"
	"testing"
	"time"
)

const (
	stateCluster = "cluster-a"
	statePool    = "pool-a"
	stateVolume  = "vol-a"
)

func TestMigrationState_PushAndGet(t *testing.T) {
	ms := NewMigrationState()

	if _, ok := ms.GetPendingMigration(stateCluster, stateVolume); ok {
		t.Fatalf("a fresh state must report no pending migration")
	}

	ms.PushMigration(stateCluster, statePool, stateVolume, "vmig-1", "sb", 60)

	pm, ok := ms.GetPendingMigration(stateCluster, stateVolume)
	if !ok {
		t.Fatalf("pending migration not found after push")
	}
	if pm.CRName != "vmig-1" || pm.CRNamespace != "sb" {
		t.Errorf("CR = %s/%s, want sb/vmig-1", pm.CRNamespace, pm.CRName)
	}
	if pm.ClusterUUID != stateCluster || pm.PoolUUID != statePool || pm.VolumeUUID != stateVolume {
		t.Errorf("identity = %+v, want %s/%s/%s", pm, stateCluster, statePool, stateVolume)
	}
	if pm.MigrationStart.IsZero() {
		t.Errorf("MigrationStart must be stamped; the stuck-detection window is measured from it")
	}
	if pm.StuckWarned {
		t.Errorf("StuckWarned must start false")
	}

	// Keys are cluster-scoped: the same volume UUID under another cluster is a
	// different migration.
	if _, ok := ms.GetPendingMigration("other-cluster", stateVolume); ok {
		t.Errorf("lookup must not cross cluster boundaries")
	}
}

func TestMigrationState_ByKeyAndPrefix(t *testing.T) {
	ms := NewMigrationState()
	ms.PushMigration(stateCluster, statePool, "vol-1", "vmig-1", "sb", 60)
	ms.PushMigration(stateCluster, statePool, "vol-2", "vmig-2", "sb", 60)
	ms.PushMigration("cluster-b", statePool, "vol-3", "vmig-3", "sb", 60)

	if _, ok := ms.GetPendingMigrationByKey(stateCluster + "/vol-1"); !ok {
		t.Errorf("GetPendingMigrationByKey did not find the expected key")
	}
	if _, ok := ms.GetPendingMigrationByKey("nope/nope"); ok {
		t.Errorf("GetPendingMigrationByKey found a key that was never pushed")
	}

	got := ms.GetPendingMigrationKeysWithPrefix(stateCluster + "/")
	slices.Sort(got)
	if want := []string{stateCluster + "/vol-1", stateCluster + "/vol-2"}; !slices.Equal(got, want) {
		t.Errorf("keys with prefix = %v, want %v", got, want)
	}
	if all := ms.GetPendingMigrationKeys(); len(all) != 3 {
		t.Errorf("GetPendingMigrationKeys = %v, want all 3", all)
	}

	if !ms.HasPendingMigrationForCluster(stateCluster) {
		t.Errorf("HasPendingMigrationForCluster = false, want true")
	}
	if ms.HasPendingMigrationForCluster("cluster-empty") {
		t.Errorf("HasPendingMigrationForCluster = true for a cluster with no migrations")
	}
}

func TestMigrationState_Delete(t *testing.T) {
	ms := NewMigrationState()
	ms.PushMigration(stateCluster, statePool, stateVolume, "vmig-1", "sb", 60)

	ms.DeletePendingMigration(stateCluster, stateVolume)
	if _, ok := ms.GetPendingMigration(stateCluster, stateVolume); ok {
		t.Errorf("pending migration still present after delete")
	}
	// Deleting twice must not panic — the rebalancer reaps on every terminal poll.
	ms.DeletePendingMigration(stateCluster, stateVolume)

	// The cooldown outlives the pending entry: a volume that just moved must not be
	// picked again immediately, even though its migration is no longer tracked.
	if !ms.IsVolumeCooledDown(stateCluster, stateVolume, time.Now()) {
		t.Errorf("cooldown must survive deletion of the pending migration")
	}
}

func TestMigrationState_Cooldown(t *testing.T) {
	ms := NewMigrationState()
	now := time.Now()
	ms.PushMigration(stateCluster, statePool, stateVolume, "vmig-1", "sb", 60)

	if !ms.IsVolumeCooledDown(stateCluster, stateVolume, now) {
		t.Errorf("volume must be cooled down right after a migration")
	}
	// Just inside the window it still holds; past it, it does not. The exact expiry
	// instant is not asserted: PushMigration stamps its own time.Now(), so from out
	// here the boundary is only known to within the call's own duration.
	if !ms.IsVolumeCooledDown(stateCluster, stateVolume, now.Add(59*time.Second)) {
		t.Errorf("cooldown must still hold just inside the window")
	}
	if ms.IsVolumeCooledDown(stateCluster, stateVolume, now.Add(61*time.Second)) {
		t.Errorf("cooldown must have expired after the window")
	}
	if ms.IsVolumeCooledDown(stateCluster, "never-migrated", now) {
		t.Errorf("an unknown volume must not be reported as cooled down")
	}

	// A zero cooldown expires immediately, which is how the caller disables it.
	ms.PushMigration(stateCluster, statePool, "vol-nocool", "vmig-2", "sb", 0)
	if ms.IsVolumeCooledDown(stateCluster, "vol-nocool", time.Now()) {
		t.Errorf("a zero cooldown must not hold the volume back")
	}
}

func TestMigrationState_CooldownCountByCluster(t *testing.T) {
	ms := NewMigrationState()
	now := time.Now()
	ms.PushMigration(stateCluster, statePool, "vol-1", "vmig-1", "sb", 60)
	ms.PushMigration(stateCluster, statePool, "vol-2", "vmig-2", "sb", 60)
	ms.PushMigration(stateCluster, statePool, "vol-expired", "vmig-3", "sb", 1)
	ms.PushMigration("cluster-b", statePool, "vol-4", "vmig-4", "sb", 60)

	if got := ms.GetCooldownCountByCluster(stateCluster, now); got != 3 {
		t.Errorf("cooldown count = %d, want 3 (all still within their window)", got)
	}
	// Only unexpired entries count — this is what caps concurrent rebalancing.
	if got := ms.GetCooldownCountByCluster(stateCluster, now.Add(2*time.Second)); got != 2 {
		t.Errorf("cooldown count after one expiry = %d, want 2", got)
	}
	if got := ms.GetCooldownCountByCluster("cluster-none", now); got != 0 {
		t.Errorf("cooldown count for an unknown cluster = %d, want 0", got)
	}
}

func TestMigrationState_MarkMigrationStuck(t *testing.T) {
	ms := NewMigrationState()
	ms.PushMigration(stateCluster, statePool, stateVolume, "vmig-1", "sb", 60)

	ms.MarkMigrationStuck(stateCluster, stateVolume)
	pm, ok := ms.GetPendingMigration(stateCluster, stateVolume)
	if !ok || !pm.StuckWarned {
		t.Errorf("StuckWarned = %v, want true — it suppresses repeat warnings", ok && pm.StuckWarned)
	}
	// Marking an unknown volume must be a no-op, not a panic on a nil map entry.
	ms.MarkMigrationStuck(stateCluster, "unknown-vol")
}

// The rebalancer reconciles concurrently with its own polling, so the map must be
// safe under parallel use. Run with -race to make this meaningful.
func TestMigrationState_ConcurrentAccess(t *testing.T) {
	ms := NewMigrationState()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(3)
		vol := "vol-" + string(rune('a'+i%26))
		go func() { defer wg.Done(); ms.PushMigration(stateCluster, statePool, vol, "cr", "sb", 60) }()
		go func() { defer wg.Done(); ms.GetPendingMigrationKeys() }()
		go func() { defer wg.Done(); ms.DeletePendingMigration(stateCluster, vol) }()
	}
	wg.Wait()
}
