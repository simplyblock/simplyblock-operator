// Tests for the volume subscription: that a snapshot caches every volume and
// marks the scope synced, that individual events move the cache, that a
// disconnect's relist drops what vanished, and that All spans scopes. The last
// is what the aggregated metrics API reads, since it serves every pool at once
// and has no scope to ask for.

package subscriptions

import (
	"context"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

const (
	lvCluster = "11111111-1111-1111-1111-111111111111"
	lvPoolA   = "22222222-2222-2222-2222-222222222222"
	lvPoolB   = "33333333-3333-3333-3333-333333333333"
	lvVolume  = "7c0000a1-3b2c-4d5e-9f01-2a3b4c5d6e7f"
	lvOther   = "8d0000b2-4c3d-5e6f-a012-3b4c5d6e7f80"
)

func poolScope(pool string) cpinformer.Scope { return cpinformer.Scope{lvCluster, pool} }

func ingestVolume(t *testing.T, sub *VolumeSubscription, scope cpinformer.Scope, kind, data string) {
	t.Helper()
	if err := sub.Ingest(context.Background(), cpinformer.Event{Kind: kind, Scope: scope, Data: []byte(data)}); err != nil {
		t.Fatalf("ingest %s: %v", kind, err)
	}
}

// volumeJSON is a control-plane VolumeDTO with only the fields the subscription
// reads. The wire schema has thirty more, and ignoring them is the point: a
// field added upstream must not break the decode.
func volumeJSON(id, name string, sizeUsed int64) string {
	return `{
		"id": "` + id + `",
		"cluster_id": "` + lvCluster + `",
		"pool_uuid": "` + lvPoolA + `",
		"pool_name": "pool-a",
		"name": "` + name + `",
		"status": "online",
		"size": 107374182400,
		"unknown_future_field": {"nested": true},
		"capacity": {"date": 1756713600, "size_total": 107374182400, "size_prov": 107374182400, "size_used": ` + itoa(sizeUsed) + `, "size_free": 68719476736, "size_util": 36}
	}`
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

func TestVolumeSubscriptionPathIsPoolScoped(t *testing.T) {
	sub := NewVolumeSubscription()
	want := "/api/v2/clusters/" + lvCluster + "/storage-pools/" + lvPoolA + "/volumes/"
	if got := sub.Path(poolScope(lvPoolA)); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestVolumeSubscriptionSnapshotCachesAndMarksSynced(t *testing.T) {
	sub := NewVolumeSubscription()
	if sub.Synced(poolScope(lvPoolA)) {
		t.Fatal("scope reported synced before any snapshot")
	}

	ingestVolume(t, sub, poolScope(lvPoolA), cpinformer.EventSnapshot,
		"["+volumeJSON(lvVolume, "vol-one", 38654705664)+","+volumeJSON(lvOther, "vol-two", 0)+"]")

	if !sub.Synced(poolScope(lvPoolA)) {
		t.Error("scope not marked synced after its snapshot")
	}
	got, ok := sub.Get(lvVolume)
	if !ok {
		t.Fatalf("volume %s absent from the cache after the snapshot", lvVolume)
	}
	if got.Capacity.SizeUsed != 38654705664 {
		t.Errorf("SizeUsed = %d, want 38654705664", got.Capacity.SizeUsed)
	}
	if got.PoolName != "pool-a" {
		t.Errorf("PoolName = %q, want pool-a", got.PoolName)
	}
	if len(sub.All()) != 2 {
		t.Errorf("All() returned %d volumes, want 2", len(sub.All()))
	}
}

func TestVolumeSubscriptionCreatedUpdatedDeleted(t *testing.T) {
	sub := NewVolumeSubscription()

	ingestVolume(t, sub, poolScope(lvPoolA), cpinformer.EventCreated, volumeJSON(lvVolume, "vol-one", 1024))
	if _, ok := sub.Get(lvVolume); !ok {
		t.Fatal("created volume absent from the cache")
	}

	ingestVolume(t, sub, poolScope(lvPoolA), cpinformer.EventUpdated, volumeJSON(lvVolume, "vol-one", 4096))
	got, _ := sub.Get(lvVolume)
	if got.Capacity.SizeUsed != 4096 {
		t.Errorf("SizeUsed after update = %d, want 4096", got.Capacity.SizeUsed)
	}

	ingestVolume(t, sub, poolScope(lvPoolA), cpinformer.EventDeleted, volumeJSON(lvVolume, "vol-one", 4096))
	if _, ok := sub.Get(lvVolume); ok {
		t.Error("deleted volume still cached")
	}
}

// A physical removal carries an empty body. Acting on it would evict an
// arbitrary volume, so it is ignored and the reconnect's snapshot is what
// reconciles the cache.
func TestVolumeSubscriptionIgnoresAnonymousDelete(t *testing.T) {
	sub := NewVolumeSubscription()
	ingestVolume(t, sub, poolScope(lvPoolA), cpinformer.EventCreated, volumeJSON(lvVolume, "vol-one", 1024))

	ingestVolume(t, sub, poolScope(lvPoolA), cpinformer.EventDeleted, `{}`)

	if _, ok := sub.Get(lvVolume); !ok {
		t.Error("an empty delete evicted a volume it did not name")
	}
}

// A reconnect re-seeds from a fresh snapshot, which is the only signal that a
// volume was deleted while the stream was down.
func TestVolumeSubscriptionSnapshotDropsVanishedVolumes(t *testing.T) {
	sub := NewVolumeSubscription()
	ingestVolume(t, sub, poolScope(lvPoolA), cpinformer.EventSnapshot,
		"["+volumeJSON(lvVolume, "vol-one", 1024)+","+volumeJSON(lvOther, "vol-two", 2048)+"]")

	ingestVolume(t, sub, poolScope(lvPoolA), cpinformer.EventSnapshot, "["+volumeJSON(lvVolume, "vol-one", 1024)+"]")

	if _, ok := sub.Get(lvOther); ok {
		t.Error("a volume absent from the relist is still cached")
	}
	if _, ok := sub.Get(lvVolume); !ok {
		t.Error("a volume present in the relist was dropped")
	}
}

// All spans pools because the metrics API lists a namespace's volumes without
// knowing which pool each came from.
func TestVolumeSubscriptionAllSpansScopes(t *testing.T) {
	sub := NewVolumeSubscription()
	ingestVolume(t, sub, poolScope(lvPoolA), cpinformer.EventCreated, volumeJSON(lvVolume, "vol-one", 1024))
	ingestVolume(t, sub, poolScope(lvPoolB), cpinformer.EventCreated, volumeJSON(lvOther, "vol-two", 2048))

	all := sub.All()
	if len(all) != 2 {
		t.Fatalf("All() returned %d volumes, want 2 across two pools", len(all))
	}
	seen := map[string]bool{}
	for _, v := range all {
		seen[v.ID] = true
	}
	if !seen[lvVolume] || !seen[lvOther] {
		t.Errorf("All() = %v, want both pools' volumes", seen)
	}
}

func TestVolumeSubscriptionRejectsMalformedPayload(t *testing.T) {
	sub := NewVolumeSubscription()
	err := sub.Ingest(context.Background(), cpinformer.Event{
		Kind:  cpinformer.EventSnapshot,
		Scope: poolScope(lvPoolA),
		Data:  []byte(`{"not": "an array"}`),
	})
	if err == nil {
		t.Error("a malformed snapshot was accepted; the stream must reconnect instead")
	}
}
