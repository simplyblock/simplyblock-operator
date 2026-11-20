// Tests for the node-add gate of a multi-attach cluster with a storage array:
// adding a node creates its LUN on the array, so no node takes a node-add slot
// before the cluster reports the array registered.

package node

import (
	"context"
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func withUnregisteredArray(cluster *simplyblockv1alpha2.StorageCluster) {
	cluster.Spec.MultiAttach = &simplyblockv1alpha2.MultiAttachSpec{
		FaultTolerance: 1,
		Array:          &simplyblockv1alpha2.StorageArraySpec{Plugin: "pure-flasharray"},
	}
	cluster.Status.Array = &simplyblockv1alpha2.StorageArrayStatus{Message: "could not connect"}
}

func TestANodeWaitsForItsClustersArray(t *testing.T) {
	r, node, cluster := aQueueWithABackendNode(t, nil)
	withUnregisteredArray(cluster)

	next, done, err := r.awaitSlot(context.Background(), node, cluster)
	if done || next != stepAwaitingSlot {
		t.Fatalf("the node went to %s before the array was registered", next)
	}
	if err == nil || !strings.Contains(err.Error(), "could not connect") {
		t.Fatalf("the hold does not say why: %v", err)
	}
	if slots := heldSlotCount(t, r, cluster); slots != 0 {
		t.Errorf("a node waiting for the array holds %d node-add slot(s)", slots)
	}

	cluster.Status.Array = &simplyblockv1alpha2.StorageArrayStatus{Registered: true}
	next, done, err = r.awaitSlot(context.Background(), node, cluster)
	if err != nil || !done || next != stepPosting {
		t.Fatalf("with the array registered the node went to %s (%v)", next, err)
	}
}

func heldSlotCount(t *testing.T, r *StorageNodeReconciler,
	cluster *simplyblockv1alpha2.StorageCluster) int {
	t.Helper()
	held, err := r.heldSlots(context.Background(), cluster)
	if err != nil {
		t.Fatalf("heldSlots: %v", err)
	}
	return len(held)
}

func TestArrayReadiness(t *testing.T) {
	cluster := &simplyblockv1alpha2.StorageCluster{}
	if !cluster.ArrayReady() {
		t.Error("a cluster without multiAttach is not ready")
	}
	cluster.Spec.MultiAttach = &simplyblockv1alpha2.MultiAttachSpec{FaultTolerance: 1}
	if !cluster.ArrayReady() {
		t.Error("a multi-attach cluster without an array is not ready")
	}
	withUnregisteredArray(cluster)
	if cluster.ArrayReady() {
		t.Error("an unregistered array reads ready")
	}
	cluster.Status.Array = nil
	if cluster.ArrayReady() {
		t.Error("an array never registered reads ready")
	}
}
