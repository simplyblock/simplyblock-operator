// Tests for the device subscription: that a snapshot caches and marks a scope
// synced, that individual events move the cache, and that a trigger is only
// emitted once the owning node's object name is known — the last being the one
// behavior a volume-shaped subscription does not have.

package subscriptions

import (
	"context"
	"testing"
	"time"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

const (
	sdCluster = "11111111-1111-1111-1111-111111111111"
	sdNode    = "44444444-4444-4444-4444-444444444444"
	sdDevice  = "5e0000a1-3b2c-4d5e-9f01-2a3b4c5d6e7f"
	sdNodeCR  = "production-7f3a9c"
)

func deviceScope() cpinformer.Scope { return cpinformer.Scope{sdCluster, sdNode} }

// registered returns a subscription that already knows the node's object name,
// which is the normal state: the StorageNode controller registers the name
// before it adds the scope that opens the stream.
func registered(t *testing.T) *DeviceSubscription {
	t.Helper()
	sub := NewDeviceSubscription("sb")
	sub.RegisterNode(sdNode, sdNodeCR)
	return sub
}

func ingestDevice(t *testing.T, sub *DeviceSubscription, kind, data string) {
	t.Helper()
	if err := sub.Ingest(context.Background(), cpinformer.Event{Kind: kind, Scope: deviceScope(), Data: []byte(data)}); err != nil {
		t.Fatalf("ingest %s: %v", kind, err)
	}
}

// drainDeviceTrigger returns the object name named by the next trigger.
func drainDeviceTrigger(t *testing.T, sub *DeviceSubscription) string {
	t.Helper()
	select {
	case ev := <-sub.Triggers():
		if ev.Object.GetNamespace() != "sb" {
			t.Errorf("trigger namespace = %q, want sb", ev.Object.GetNamespace())
		}
		return ev.Object.GetName()
	case <-time.After(time.Second):
		t.Fatal("no reconcile trigger enqueued")
		return ""
	}
}

func expectNoDeviceTrigger(t *testing.T, sub *DeviceSubscription) {
	t.Helper()
	select {
	case ev := <-sub.Triggers():
		t.Fatalf("unexpected trigger for %q", ev.Object.GetName())
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDeviceSubscriptionPathIsNodeScoped(t *testing.T) {
	sub := NewDeviceSubscription("sb")
	want := "/api/v2/clusters/" + sdCluster + "/storage-nodes/" + sdNode + "/devices/"
	if got := sub.Path(deviceScope()); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestDeviceSubscriptionSnapshotCachesSyncsAndTriggers(t *testing.T) {
	sub := registered(t)
	if sub.Synced(deviceScope()) {
		t.Fatal("scope should not be synced before a snapshot")
	}

	ingestDevice(t, sub, cpinformer.EventSnapshot,
		`[{"id":"`+sdDevice+`","cluster_id":"`+sdCluster+`","storage_node_id":"`+sdNode+`","status":"online","size":4096}]`)

	want := simplyblockv1alpha1.StorageDeviceName(sdNodeCR, sdDevice)
	if got := drainDeviceTrigger(t, sub); got != want {
		t.Errorf("trigger = %q, want %q", got, want)
	}
	if !sub.Synced(deviceScope()) {
		t.Error("scope should be synced after a snapshot")
	}
	scope, dto, ok := sub.Lookup(want)
	if !ok || dto.Size != 4096 || scope.Key() != deviceScope().Key() {
		t.Errorf("lookup = %v, %+v, %v", scope, dto, ok)
	}
}

func TestDeviceSubscriptionSnapshotRemovalTriggersVanishedDevice(t *testing.T) {
	sub := registered(t)
	ingestDevice(t, sub, cpinformer.EventCreated, `{"id":"`+sdDevice+`","status":"online"}`)
	drainDeviceTrigger(t, sub)

	// A device pulled while the operator was disconnected is absent from the
	// next snapshot; the relist must still enqueue it so its object is removed.
	ingestDevice(t, sub, cpinformer.EventSnapshot, `[]`)
	if got := drainDeviceTrigger(t, sub); got != simplyblockv1alpha1.StorageDeviceName(sdNodeCR, sdDevice) {
		t.Errorf("relist trigger = %q", got)
	}
	if _, _, ok := sub.Lookup(simplyblockv1alpha1.StorageDeviceName(sdNodeCR, sdDevice)); ok {
		t.Error("device absent from the snapshot should be gone from the cache")
	}
}

func TestDeviceSubscriptionDeleteDropsFromCacheAndTriggers(t *testing.T) {
	sub := registered(t)
	ingestDevice(t, sub, cpinformer.EventCreated, `{"id":"`+sdDevice+`","status":"online"}`)
	drainDeviceTrigger(t, sub)

	ingestDevice(t, sub, cpinformer.EventDeleted, `{"id":"`+sdDevice+`","status":"removed"}`)
	if got := drainDeviceTrigger(t, sub); got != simplyblockv1alpha1.StorageDeviceName(sdNodeCR, sdDevice) {
		t.Errorf("delete trigger = %q", got)
	}
	if _, _, ok := sub.Lookup(simplyblockv1alpha1.StorageDeviceName(sdNodeCR, sdDevice)); ok {
		t.Error("device should be gone from the cache after delete")
	}
}

func TestDeviceSubscriptionEmptyDeleteIsIgnored(t *testing.T) {
	sub := registered(t)
	ingestDevice(t, sub, cpinformer.EventCreated, `{"id":"`+sdDevice+`","status":"online"}`)
	drainDeviceTrigger(t, sub)

	// A physical removal carries {}; the snapshot relist covers it.
	ingestDevice(t, sub, cpinformer.EventDeleted, `{}`)
	expectNoDeviceTrigger(t, sub)
	if _, _, ok := sub.Lookup(simplyblockv1alpha1.StorageDeviceName(sdNodeCR, sdDevice)); !ok {
		t.Error("an empty delete must not drop the cached device")
	}
}

// A device event can arrive for a node whose object name the operator does not
// know — the node's own object was deleted, or its controller has not
// registered it yet. Caching it is right; naming an object after a node that is
// not there is not, so no trigger is emitted.
func TestDeviceSubscriptionCachesButDoesNotTriggerForUnknownNode(t *testing.T) {
	sub := NewDeviceSubscription("sb")

	ingestDevice(t, sub, cpinformer.EventSnapshot, `[{"id":"`+sdDevice+`","status":"online"}]`)

	expectNoDeviceTrigger(t, sub)
	if _, _, ok := sub.Lookup(simplyblockv1alpha1.StorageDeviceName(sdNodeCR, sdDevice)); ok {
		t.Error("a device with no node name cannot be indexed under an object name")
	}

	// Once the node registers, the next event reaches the reconciler.
	sub.RegisterNode(sdNode, sdNodeCR)
	ingestDevice(t, sub, cpinformer.EventUpdated, `{"id":"`+sdDevice+`","status":"online"}`)
	if got := drainDeviceTrigger(t, sub); got != simplyblockv1alpha1.StorageDeviceName(sdNodeCR, sdDevice) {
		t.Errorf("trigger after registration = %q", got)
	}
}

func TestDeviceSubscriptionUnregisterNodeStopsNaming(t *testing.T) {
	sub := registered(t)
	sub.UnregisterNode(sdNode)

	ingestDevice(t, sub, cpinformer.EventCreated, `{"id":"`+sdDevice+`","status":"online"}`)
	expectNoDeviceTrigger(t, sub)
}
