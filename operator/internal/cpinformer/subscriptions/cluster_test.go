// Tests for the cluster subscription: that a snapshot caches and marks the one
// scope synced, that individual events move the cache, and that a trigger
// names the StorageCluster object the backend cluster was adopted as.
//
// The scope is what distinguishes this subscription from every other one here,
// so it is asserted directly: the route takes no path parameter, the scope is
// empty, and one stream covers every cluster in every namespace.

package subscriptions

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

const (
	scClusterA = "33333333-3333-3333-3333-333333333333"
	scClusterB = "44444444-4444-4444-4444-444444444444"
)

// scObject is the StorageCluster the backend cluster was adopted as. Its
// namespace is the one the cluster's own resources live in rather than the
// operator's.
var scObject = types.NamespacedName{Namespace: "simplyblock", Name: "production"}

func registeredClusters(t *testing.T) *ClusterSubscription {
	t.Helper()
	sub := NewClusterSubscription()
	sub.RegisterCluster(scClusterA, scObject)
	return sub
}

func ingestCluster(t *testing.T, sub *ClusterSubscription, kind, data string) {
	t.Helper()
	err := sub.Ingest(context.Background(), cpinformer.Event{
		Kind: kind, Scope: RootScope, Data: []byte(data),
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", kind, err)
	}
}

// drainClusterTrigger returns the object key named by the next trigger.
func drainClusterTrigger(t *testing.T, sub *ClusterSubscription) types.NamespacedName {
	t.Helper()
	select {
	case ev := <-sub.Triggers():
		return types.NamespacedName{
			Namespace: ev.Object.GetNamespace(),
			Name:      ev.Object.GetName(),
		}
	case <-time.After(time.Second):
		t.Fatal("no reconcile trigger enqueued")
		return types.NamespacedName{}
	}
}

// The route takes no path parameter, so the scope is empty and one stream
// carries every cluster. That is the whole reason this subscription is not
// opened by a reconciler.
func TestTheClusterStreamIsRootScoped(t *testing.T) {
	sub := NewClusterSubscription()
	if got := sub.Path(RootScope); got != "/api/v2/clusters/" {
		t.Errorf("path = %q, want the cluster list route", got)
	}
	if len(RootScope) != 0 {
		t.Errorf("RootScope = %v, want the empty scope", RootScope)
	}
	if RootScope.Key() != "" {
		t.Errorf("RootScope.Key() = %q, want the empty key", RootScope.Key())
	}
}

// A snapshot caches every cluster it carries and marks the one scope synced,
// which is what lets a reader treat a miss as an answer rather than as a cold
// cache.
func TestAClusterSnapshotCachesAndSyncs(t *testing.T) {
	sub := registeredClusters(t)
	if sub.SyncedRoot() {
		t.Fatal("the scope reported synced before any snapshot arrived")
	}

	ingestCluster(t, sub, cpinformer.EventSnapshot, `[
		{"id":"`+scClusterA+`","name":"production","nqn":"nqn.a","status":"active",
		 "is_re_balancing":false,"distr_ndcs":2,"distr_npcs":1,"max_fault_tolerance":1},
		{"id":"`+scClusterB+`","name":"staging","nqn":"nqn.b","status":"unready",
		 "is_re_balancing":true,"distr_ndcs":4,"distr_npcs":2,"max_fault_tolerance":2}
	]`)

	if !sub.SyncedRoot() {
		t.Error("the scope did not report synced after its snapshot")
	}
	got, ok := sub.Lookup(scClusterA)
	if !ok {
		t.Fatalf("cluster %s is not cached", scClusterA)
	}
	if got.Status != "active" || got.NQN != "nqn.a" || got.NDCS != 2 || got.NPCS != 1 {
		t.Errorf("the snapshot decoded wrong: %+v", got)
	}
	if got.MaxFaultTolerance != 1 || got.Rebalancing {
		t.Errorf("the fault tolerance or the rebalancing flag decoded wrong: %+v", got)
	}
	if other, ok := sub.Lookup(scClusterB); !ok || !other.Rebalancing {
		t.Errorf("the second cluster decoded wrong: %+v", other)
	}
}

// An update moves the cache and triggers a reconcile of the object the cluster
// was adopted as. Both halves matter: the reconciler reads the cache, and
// nothing else would tell it to look.
func TestAClusterUpdateMovesTheCacheAndTriggers(t *testing.T) {
	sub := registeredClusters(t)
	ingestCluster(t, sub, cpinformer.EventSnapshot, `[
		{"id":"`+scClusterA+`","name":"production","nqn":"nqn.a","status":"active",
		 "is_re_balancing":false,"distr_ndcs":2,"distr_npcs":1,"max_fault_tolerance":1}
	]`)
	if got := drainClusterTrigger(t, sub); got != scObject {
		t.Fatalf("the snapshot named %v, want %v", got, scObject)
	}

	ingestCluster(t, sub, cpinformer.EventUpdated, `
		{"id":"`+scClusterA+`","name":"production","nqn":"nqn.a","status":"degraded",
		 "is_re_balancing":true,"distr_ndcs":2,"distr_npcs":1,"max_fault_tolerance":0}`)

	if got := drainClusterTrigger(t, sub); got != scObject {
		t.Errorf("the update named %v, want %v", got, scObject)
	}
	got, ok := sub.Lookup(scClusterA)
	if !ok {
		t.Fatal("the cluster left the cache on an update")
	}
	if got.Status != "degraded" || !got.Rebalancing || got.MaxFaultTolerance != 0 {
		t.Errorf("the update did not reach the cache: %+v", got)
	}
}

// A cluster the operator has not adopted yields no trigger: there is no object
// to name. Its state is still cached, because the adoption that follows reads
// the cache rather than the control plane.
func TestAnUnregisteredClusterIsCachedButNotTriggered(t *testing.T) {
	sub := registeredClusters(t)

	ingestCluster(t, sub, cpinformer.EventUpdated, `
		{"id":"`+scClusterB+`","name":"staging","nqn":"nqn.b","status":"active",
		 "is_re_balancing":false,"distr_ndcs":1,"distr_npcs":1,"max_fault_tolerance":1}`)

	if _, ok := sub.Lookup(scClusterB); !ok {
		t.Error("an unadopted cluster was not cached")
	}
	select {
	case ev := <-sub.Triggers():
		t.Errorf("an unadopted cluster triggered a reconcile of %s/%s",
			ev.Object.GetNamespace(), ev.Object.GetName())
	default:
	}
}

// A snapshot replaces its scope rather than merging into it, so a cluster
// deleted while the stream was down is gone after the reconnect. Without that
// a torn-down cluster would be reported as live forever.
func TestAClusterSnapshotReplacesRatherThanMerges(t *testing.T) {
	sub := registeredClusters(t)
	ingestCluster(t, sub, cpinformer.EventSnapshot, `[
		{"id":"`+scClusterA+`","name":"production","nqn":"nqn.a","status":"active",
		 "is_re_balancing":false,"distr_ndcs":2,"distr_npcs":1,"max_fault_tolerance":1},
		{"id":"`+scClusterB+`","name":"staging","nqn":"nqn.b","status":"active",
		 "is_re_balancing":false,"distr_ndcs":1,"distr_npcs":1,"max_fault_tolerance":1}
	]`)

	// The reconnect snapshot no longer carries the second cluster.
	ingestCluster(t, sub, cpinformer.EventSnapshot, `[
		{"id":"`+scClusterA+`","name":"production","nqn":"nqn.a","status":"active",
		 "is_re_balancing":false,"distr_ndcs":2,"distr_npcs":1,"max_fault_tolerance":1}
	]`)

	if _, ok := sub.Lookup(scClusterB); ok {
		t.Error("a cluster absent from the reconnect snapshot is still cached")
	}
	if _, ok := sub.Lookup(scClusterA); !ok {
		t.Error("the cluster the snapshot still carries was dropped")
	}
}

// Unregistering stops the subscription naming objects after a StorageCluster
// that is going away.
func TestUnregisteringAClusterStopsItsTriggers(t *testing.T) {
	sub := registeredClusters(t)
	sub.UnregisterCluster(scClusterA)

	ingestCluster(t, sub, cpinformer.EventUpdated, `
		{"id":"`+scClusterA+`","name":"production","nqn":"nqn.a","status":"active",
		 "is_re_balancing":false,"distr_ndcs":2,"distr_npcs":1,"max_fault_tolerance":1}`)

	select {
	case ev := <-sub.Triggers():
		t.Errorf("an unregistered cluster triggered a reconcile of %s/%s",
			ev.Object.GetNamespace(), ev.Object.GetName())
	default:
	}
}
