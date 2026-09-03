// Tests for the storage-node subscription: that a snapshot caches and marks a
// scope synced, that individual events move the cache, and that a trigger names
// the StorageNode object the backend node was adopted as. The last of those is
// what the reconciler depends on, and what a per-cluster stream cannot derive
// on its own.

package subscriptions

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

const (
	snCluster = "22222222-2222-2222-2222-222222222222"
	snNode    = "fd687dfd-9b5d-4eca-8cb1-23bcf550ad21"
)

// snObject is the StorageNode the backend node was adopted as. Its namespace is
// the storage cluster's rather than the operator's, which is where these CRs
// actually live.
var snObject = types.NamespacedName{Namespace: "default", Name: "simplyblock-node-asxeub"}

// nodeScope is a cluster on its own: one stream serves every node of it.
func nodeScope() cpinformer.Scope { return cpinformer.Scope{snCluster} }

func registeredNodes(t *testing.T) *NodeSubscription {
	t.Helper()
	sub := NewNodeSubscription()
	sub.RegisterNode(snNode, snObject)
	return sub
}

func ingestNode(t *testing.T, sub *NodeSubscription, kind, data string) {
	t.Helper()
	err := sub.Ingest(context.Background(), cpinformer.Event{
		Kind: kind, Scope: nodeScope(), Data: []byte(data),
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", kind, err)
	}
}

// drainNodeTrigger returns the object key named by the next trigger.
func drainNodeTrigger(t *testing.T, sub *NodeSubscription) types.NamespacedName {
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

func expectNoNodeTrigger(t *testing.T, sub *NodeSubscription) {
	t.Helper()
	select {
	case ev := <-sub.Triggers():
		t.Fatalf("unexpected trigger for %s/%s", ev.Object.GetNamespace(), ev.Object.GetName())
	case <-time.After(50 * time.Millisecond):
	}
}

// The path is per cluster, because the control plane serves every node of a
// cluster from one route. A per-node path would open one stream per node for
// data one stream already carries.
func TestNodeSubscriptionStreamsPerCluster(t *testing.T) {
	got := NewNodeSubscription().Path(nodeScope())
	want := "/api/v2/clusters/" + snCluster + "/storage-nodes/"
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

// The snapshot fills the cache, marks the scope authoritative, and triggers a
// reconcile of every node it names.
func TestNodeSubscriptionSnapshotCachesSyncsAndTriggers(t *testing.T) {
	sub := registeredNodes(t)
	if sub.Synced(nodeScope()) {
		t.Fatal("scope should not be synced before a snapshot")
	}

	ingestNode(t, sub, cpinformer.EventSnapshot, `[{
		"id":"`+snNode+`","status":"online","mgmt_ip":"192.168.10.112",
		"health_check":true,"hostname":"vm02_4420","cpu_spdk_count":6,"lvols":3,
		"rpc_port":4420,"lvol_subsys_port":4426,"nvmf_port":4421,"failure_domain":-1
	}]`)

	if got := drainNodeTrigger(t, sub); got != snObject {
		t.Errorf("trigger named %v, want the adopted object %v", got, snObject)
	}
	if !sub.Synced(nodeScope()) {
		t.Error("scope should be synced after a snapshot")
	}

	scope, dto, ok := sub.Lookup(snNode)
	if !ok {
		t.Fatal("the node should be cached after a snapshot")
	}
	if scope.Key() != nodeScope().Key() {
		t.Errorf("scope = %v, want %v", scope, nodeScope())
	}
	if dto.Status != "online" || !dto.HealthCheck || dto.Hostname != "vm02_4420" {
		t.Errorf("status/health/hostname = %q/%v/%q", dto.Status, dto.HealthCheck, dto.Hostname)
	}
	if dto.CPUCount != 6 || dto.Volumes != 3 || dto.ManagementIP != "192.168.10.112" {
		t.Errorf("cpu/volumes/ip = %d/%d/%q", dto.CPUCount, dto.Volumes, dto.ManagementIP)
	}
	if dto.RPCPort != 4420 || dto.LvolPort != 4426 || dto.NVMeOFPort != 4421 {
		t.Errorf("ports = %d/%d/%d", dto.RPCPort, dto.LvolPort, dto.NVMeOFPort)
	}
	if dto.FailureDomain != -1 {
		t.Errorf("failureDomain = %d, want -1", dto.FailureDomain)
	}
}

// An update replaces what the cache holds. This is the path the shutdown of a
// real node exercised: the status moves and the mirror has to follow.
func TestNodeSubscriptionUpdateMovesTheCachedStatus(t *testing.T) {
	sub := registeredNodes(t)
	ingestNode(t, sub, cpinformer.EventSnapshot,
		`[{"id":"`+snNode+`","status":"online","health_check":true}]`)
	drainNodeTrigger(t, sub)

	ingestNode(t, sub, cpinformer.EventUpdated,
		`{"id":"`+snNode+`","status":"in_shutdown","health_check":false}`)

	if got := drainNodeTrigger(t, sub); got != snObject {
		t.Errorf("trigger named %v, want %v", got, snObject)
	}
	_, dto, ok := sub.Lookup(snNode)
	if !ok {
		t.Fatal("the node should still be cached after an update")
	}
	if dto.Status != "in_shutdown" || dto.HealthCheck {
		t.Errorf("status/health = %q/%v, want in_shutdown/false", dto.Status, dto.HealthCheck)
	}
}

// A node that vanished while the stream was disconnected still has a CR, and
// only a reconcile can decide what that CR should now say, so its disappearance
// has to trigger one.
func TestNodeSubscriptionSnapshotTriggersAVanishedNode(t *testing.T) {
	sub := registeredNodes(t)
	ingestNode(t, sub, cpinformer.EventCreated, `{"id":"`+snNode+`","status":"online"}`)
	drainNodeTrigger(t, sub)

	// A relist that no longer mentions it: the node left the cluster.
	ingestNode(t, sub, cpinformer.EventSnapshot, `[]`)

	if got := drainNodeTrigger(t, sub); got != snObject {
		t.Errorf("trigger named %v, want %v", got, snObject)
	}
	if _, _, ok := sub.Lookup(snNode); ok {
		t.Error("a node absent from the relist should be gone from the cache")
	}
}

// A delete carrying no id says only that something went, and the relist is what
// says which. Removing nothing beats removing a guess.
func TestNodeSubscriptionEmptyDeleteIsIgnored(t *testing.T) {
	sub := registeredNodes(t)
	ingestNode(t, sub, cpinformer.EventCreated, `{"id":"`+snNode+`","status":"online"}`)
	drainNodeTrigger(t, sub)

	ingestNode(t, sub, cpinformer.EventDeleted, `{}`)

	expectNoNodeTrigger(t, sub)
	if _, _, ok := sub.Lookup(snNode); !ok {
		t.Error("an empty delete should leave the cache alone")
	}
}

// A node the operator has not adopted is cached but triggers nothing: there is
// no CR to name. The cluster may hold nodes this operator does not manage.
func TestNodeSubscriptionCachesButDoesNotTriggerAnUnadoptedNode(t *testing.T) {
	sub := NewNodeSubscription() // no RegisterNode

	ingestNode(t, sub, cpinformer.EventSnapshot, `[{"id":"`+snNode+`","status":"online"}]`)

	expectNoNodeTrigger(t, sub)
	if _, _, ok := sub.Lookup(snNode); !ok {
		t.Error("an unadopted node should still be cached, so adoption finds it waiting")
	}
}

// Unregistering stops the naming, which is what keeps a StorageNode on its way
// out from being reconciled by an event that arrives after it is gone.
func TestNodeSubscriptionUnregisterStopsTriggering(t *testing.T) {
	sub := registeredNodes(t)
	sub.UnregisterNode(snNode)

	ingestNode(t, sub, cpinformer.EventUpdated, `{"id":"`+snNode+`","status":"offline"}`)

	expectNoNodeTrigger(t, sub)
}

// An empty id has no cache entry by construction, and looking one up must not
// return whatever the store happens to hold first.
func TestNodeSubscriptionLookupOfAnEmptyIDMisses(t *testing.T) {
	sub := registeredNodes(t)
	ingestNode(t, sub, cpinformer.EventSnapshot, `[{"id":"`+snNode+`","status":"online"}]`)
	drainNodeTrigger(t, sub)

	if _, _, ok := sub.Lookup(""); ok {
		t.Error("an empty node id should miss rather than match a cached node")
	}
}
