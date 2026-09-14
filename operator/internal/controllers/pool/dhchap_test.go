// Tests for what a DHCHAP pool's class carries, for the class's ownership, and
// for the one PersistentVolume state that is easy to get wrong.
//
// The first pair is a regression guard carried over from the controller this
// package replaces. A DHCHAP pool restricts its volumes to the nodes it allows
// through one label per pool, and the class republishes that label's key so the
// driver can turn it into the volume's node affinity. The key is derived in two
// places, so a divergence pins every volume of the pool to a label nothing
// carries and no claim ever binds.

package pool

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// readDefaultClass returns the class the operator wrote for the default pool.
func readDefaultClass(t *testing.T, r *StoragePoolReconciler) *storagev1.StorageClass {
	t.Helper()
	var class storagev1.StorageClass
	key := client.ObjectKey{Name: DefaultStorageClassName(testNamespace, testCluster)}
	if err := r.Get(context.Background(), key, &class); err != nil {
		t.Fatalf("the default class was not written: %v", err)
	}
	return &class
}

// U-40 and U-55: the class selects on the exact key the reconcile writes on the
// allowed nodes.
func TestTheDHCHAPClassRepublishesTheNodeLabelKey(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	name := DefaultPoolName(testCluster)
	defaultPool := newPool(name, func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Spec.AllowedNodes = []string{"worker-1"}
		p.Spec.VolumeDefaults = &simplyblockv1alpha2.VolumeDefaults{EnableDHCHAP: ptr.To(true)}
	})
	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", UID: "worker-1-uid"}}
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), defaultPool, worker)

	reconcileSettled(t, r, name)

	selector := readDefaultClass(t, r).Parameters[paramDHCHAPNodeSelector]
	if selector == "" {
		t.Fatalf("the class carries no %s, so its volumes are not restricted at all",
			paramDHCHAPNodeSelector)
	}

	var node corev1.Node
	if err := r.Get(context.Background(), client.ObjectKey{Name: "worker-1"}, &node); err != nil {
		t.Fatalf("read the node back: %v", err)
	}
	if _, ok := node.Labels[selector]; !ok {
		t.Errorf("the class selects on %q and the node carries %v: nothing would ever match",
			selector, node.Labels)
	}
}

// A pool with DHCHAP on and no allowed nodes cannot enforce it, so the class
// gets no selector rather than one that would match every node.
func TestADHCHAPPoolWithNoAllowedNodesGetsNoSelector(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	name := DefaultPoolName(testCluster)
	defaultPool := newPool(name, func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Spec.VolumeDefaults = &simplyblockv1alpha2.VolumeDefaults{EnableDHCHAP: ptr.To(true)}
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), defaultPool)

	reconcileSettled(t, r, name)

	if selector := readDefaultClass(t, r).Parameters[paramDHCHAPNodeSelector]; selector != "" {
		t.Errorf("the class carries %s = %q for a pool that allows every node",
			paramDHCHAPNodeSelector, selector)
	}
}

// U-21: the class the operator writes carries no owner reference, because the
// scopes forbid one. A StorageClass is cluster-scoped and a StoragePool is
// namespaced, and Kubernetes garbage-collects a cluster-scoped object whose
// owner is namespaced — so an owner reference here would delete the class at an
// arbitrary moment and blame nobody.
func TestTheGeneratedClassCarriesNoOwnerReference(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	name := DefaultPoolName(testCluster)
	defaultPool := newPool(name, func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), defaultPool)

	reconcileSettled(t, r, name)

	if owners := readDefaultClass(t, r).OwnerReferences; len(owners) != 0 {
		t.Errorf("the class carries %v, which garbage collection would act on", owners)
	}
}

// U-32: a released PersistentVolume still counts as bound until it is deleted.
// A release is a state the volume is in rather than a statement that its data is
// gone, and a pool deleted out from under one takes the backend volume with it.
func TestAReleasedVolumeStillHolds(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	released := boundVolume("pv-released", testPoolUUID)
	released.Status.Phase = corev1.VolumeReleased
	r := newReconciler(t, cp, rec,
		newCluster(testClusterUUID), deleting("tenant-a"), released)

	reconcileTimes(t, r, "tenant-a", 1)

	if !poolExists(t, r, "tenant-a") {
		t.Error("a released volume did not hold the pool's deletion")
	}
}
