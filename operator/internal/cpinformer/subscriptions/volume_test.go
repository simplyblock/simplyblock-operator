package subscriptions

import (
	"context"
	"testing"
	"time"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

const (
	lvCluster = "11111111-1111-1111-1111-111111111111"
	lvPool    = "22222222-2222-2222-2222-222222222222"
	lvVolume  = "33333333-3333-3333-3333-333333333333"
)

func volumeScope() cpinformer.Scope { return cpinformer.Scope{lvCluster, lvPool} }

func ingest(t *testing.T, sub *VolumeSubscription, kind, data string) {
	t.Helper()
	if err := sub.Ingest(context.Background(), cpinformer.Event{Kind: kind, Scope: volumeScope(), Data: []byte(data)}); err != nil {
		t.Fatalf("ingest %s: %v", kind, err)
	}
}

// drainTrigger returns the volume id named by the next trigger.
func drainTrigger(t *testing.T, sub *VolumeSubscription) string {
	t.Helper()
	select {
	case ev := <-sub.Triggers():
		name := ev.Object.GetName()
		if ev.Object.GetNamespace() != "sb" {
			t.Errorf("trigger namespace = %q, want sb", ev.Object.GetNamespace())
		}
		return name
	case <-time.After(time.Second):
		t.Fatal("no reconcile trigger enqueued")
		return ""
	}
}

func TestVolumeSubscriptionSnapshotCachesSyncsAndTriggers(t *testing.T) {
	sub := NewVolumeSubscription("sb", nil)
	if sub.Synced(volumeScope()) {
		t.Fatal("scope should not be synced before a snapshot")
	}

	ingest(t, sub, cpinformer.EventSnapshot, `[{"id":"`+lvVolume+`","name":"vol1"}]`)

	if got := drainTrigger(t, sub); got != simplyblockv1alpha1.LogicalVolumeName(lvVolume) {
		t.Errorf("trigger = %q, want %q", got, simplyblockv1alpha1.LogicalVolumeName(lvVolume))
	}
	if !sub.Synced(volumeScope()) {
		t.Error("scope should be synced after a snapshot")
	}
	scope, dto, ok := sub.Lookup(lvVolume)
	if !ok || dto.Name != "vol1" || scope.Key() != volumeScope().Key() {
		t.Errorf("lookup = %v, %+v, %v", scope, dto, ok)
	}
}

func TestVolumeSubscriptionDeleteDropsFromCacheAndTriggers(t *testing.T) {
	sub := NewVolumeSubscription("sb", nil)
	ingest(t, sub, cpinformer.EventCreated, `{"id":"`+lvVolume+`"}`)
	drainTrigger(t, sub)

	ingest(t, sub, cpinformer.EventDeleted, `{"id":"`+lvVolume+`","status":"deleted"}`)
	if got := drainTrigger(t, sub); got != simplyblockv1alpha1.LogicalVolumeName(lvVolume) {
		t.Errorf("delete trigger = %q", got)
	}
	if _, _, ok := sub.Lookup(lvVolume); ok {
		t.Error("volume should be gone from the cache after delete")
	}
}

func TestVolumeSubscriptionFilter(t *testing.T) {
	sub := NewVolumeSubscription("sb", func(v VolumeDTO) bool { return v.Name == "keep" })
	ingest(t, sub, cpinformer.EventSnapshot, `[{"id":"a","name":"keep"},{"id":"b","name":"drop"}]`)
	drainTrigger(t, sub) // only the kept volume yields a trigger

	if _, _, ok := sub.Lookup("a"); !ok {
		t.Error("kept volume should be cached")
	}
	if _, _, ok := sub.Lookup("b"); ok {
		t.Error("filtered-out volume should not be cached")
	}

	// An update that fails the filter drops it and still triggers (to delete its CR).
	ingest(t, sub, cpinformer.EventUpdated, `{"id":"a","name":"now-drop"}`)
	drainTrigger(t, sub)
	if _, _, ok := sub.Lookup("a"); ok {
		t.Error("filtered-out update should drop from cache")
	}
}
