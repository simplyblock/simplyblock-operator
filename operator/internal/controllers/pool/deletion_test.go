// Tests for the two holds in front of a pool's deletion, and for what happens
// once neither applies.
//
// These are the U-24 to U-34 rows of docs/tests/test-plan-storagepool.md and the
// most load-bearing tests in the package, because the behavior they pin is what
// stops `kubectl delete storagecluster` from destroying tenant data. Each hold
// is asserted twice over: that the pool stays, and that nothing was deleted
// while it was held. The second half is the one that matters — a hold that
// deletes the backend pool first and then refuses to finish is worse than no
// hold at all.

package pool

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// deleting returns a pool that has a UUID and is being deleted.
func deleting(name string) *simplyblockv1alpha2.StoragePool {
	return newPool(name, func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		now := metav1.Now()
		p.DeletionTimestamp = &now
	})
}

// boundVolume returns a PersistentVolume provisioned out of the named pool
// segment, which is either the pool's UUID or, for a volume from before the v2
// API migration, its name.
func boundVolume(name, poolSegment string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "csi.simplyblock.io",
					VolumeHandle: testClusterUUID + ":" + poolSegment + ":a-volume",
				},
			},
		},
	}
}

func poolExists(t *testing.T, r *StoragePoolReconciler, name string) bool {
	t.Helper()
	var p simplyblockv1alpha2.StoragePool
	key := client.ObjectKey{Namespace: testNamespace, Name: name}
	err := r.Get(context.Background(), key, &p)
	return err == nil
}

// U-24: with nothing referring to it, the pool's class goes, then the backend
// pool, then the finalizer.
func TestDeletesAPoolNothingRefersTo(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), deleting("tenant-a"))

	reconcileTimes(t, r, "tenant-a", 1)

	if cp.deletes != 1 {
		t.Errorf("the control plane was asked to delete the pool %d times, want 1", cp.deletes)
	}
	if poolExists(t, r, "tenant-a") {
		t.Error("the pool still exists, so the finalizer was not released")
	}
}

// U-25 and U-26: a bound volume holds the deletion, and nothing is deleted while
// it is held. The second assertion is the one that matters.
func TestABoundVolumeHoldsTheDeletionAndNothingIsDeleted(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	r := newReconciler(t, cp, rec,
		newCluster(testClusterUUID), deleting("tenant-a"),
		boundVolume("pv-1", testPoolUUID),
	)

	reconcileTimes(t, r, "tenant-a", 2)

	if !poolExists(t, r, "tenant-a") {
		t.Fatal("the pool was deleted while a volume was bound to it")
	}
	if cp.deletes != 0 {
		t.Errorf("the control plane was asked to delete the pool %d times while held, want 0",
			cp.deletes)
	}
	if !rec.has(VolumesStillBound) {
		t.Errorf("no %s event, so the hold is indistinguishable from a stuck finalizer: %+v",
			VolumesStillBound, rec.events)
	}
}

// U-27: deleting the last claim releases the pool without anybody touching it
// again.
func TestTheHoldClearsWhenTheLastVolumeGoes(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	volume := boundVolume("pv-1", testPoolUUID)
	r := newReconciler(t, cp, rec,
		newCluster(testClusterUUID), deleting("tenant-a"), volume,
	)

	reconcileTimes(t, r, "tenant-a", 1)
	if !poolExists(t, r, "tenant-a") {
		t.Fatal("the pool was deleted while a volume was bound to it")
	}

	if err := r.Delete(context.Background(), volume); err != nil {
		t.Fatalf("delete the volume: %v", err)
	}
	reconcileTimes(t, r, "tenant-a", 1)

	if poolExists(t, r, "tenant-a") {
		t.Error("the pool is still held after its last volume went")
	}
}

// U-31: a volume of another pool does not hold this one.
func TestAnotherPoolsVolumeDoesNotHoldThisOne(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	r := newReconciler(t, cp, rec,
		newCluster(testClusterUUID), deleting("tenant-a"),
		boundVolume("pv-other", "a-different-pool-uuid"),
	)

	reconcileTimes(t, r, "tenant-a", 1)

	if poolExists(t, r, "tenant-a") {
		t.Error("another pool's volume held this pool's deletion")
	}
}

// A volume provisioned before the v2 API migration encodes the pool's name
// rather than its UUID, and the field it lives in is immutable. It still holds.
func TestAVolumeWithANameKeyedHandleStillHolds(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	r := newReconciler(t, cp, rec,
		newCluster(testClusterUUID), deleting("tenant-a"),
		boundVolume("pv-old", "tenant-a"),
	)

	reconcileTimes(t, r, "tenant-a", 1)

	if !poolExists(t, r, "tenant-a") {
		t.Error("a volume whose handle names the pool did not hold the deletion")
	}
}

// U-33: a control-plane volume with no PersistentVolume does not hold. Only
// objects Kubernetes knows about hold a deletion, because a hold is a promise to
// a person that something they can see still needs them.
func TestAVolumeKubernetesCannotSeeDoesNotHold(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	foreign := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-foreign"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "another.csi.driver",
					VolumeHandle: testClusterUUID + ":" + testPoolUUID + ":a-volume",
				},
			},
		},
	}
	r := newReconciler(t, cp, rec,
		newCluster(testClusterUUID), deleting("tenant-a"), foreign)

	reconcileTimes(t, r, "tenant-a", 1)

	if poolExists(t, r, "tenant-a") {
		t.Error("a volume this driver did not provision held the deletion")
	}
}

// An authored class holds the deletion, because the operator neither owns it nor
// knows why it exists, and deleting somebody's provisioning contract to let a
// pool go is not a trade it gets to make.
func TestAnAuthoredClassHoldsTheDeletion(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	authored := newClass("fast-xfs", assignmentLabels("tenant-a", false), nil)
	r := newReconciler(t, cp, rec,
		newCluster(testClusterUUID), deleting("tenant-a"), authored)

	reconcileTimes(t, r, "tenant-a", 2)

	if !poolExists(t, r, "tenant-a") {
		t.Fatal("the pool was deleted while a class was assigned to it")
	}
	if cp.deletes != 0 {
		t.Errorf("the control plane was asked to delete the pool %d times while held, want 0",
			cp.deletes)
	}
	var class storagev1.StorageClass
	if err := r.Get(context.Background(), client.ObjectKey{Name: "fast-xfs"}, &class); err != nil {
		t.Errorf("the operator deleted a class it did not write: %v", err)
	}
	if !rec.has(StorageClassStillAssigned) {
		t.Errorf("no %s event: %+v", StorageClassStillAssigned, rec.events)
	}
}

// The class the operator wrote does not hold, because deleting it is cleanup
// rather than a decision. One label tells the two cases apart.
func TestTheOperatorsOwnClassDoesNotHold(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	name := DefaultPoolName(testCluster)
	managed := newClass(DefaultStorageClassName(testCluster), assignmentLabels(name, true), nil)
	r := newReconciler(t, cp, rec,
		newCluster(testClusterUUID), deleting(name), managed)

	reconcileTimes(t, r, name, 1)

	if poolExists(t, r, name) {
		t.Fatal("the operator's own class held the pool's deletion")
	}
	var class storagev1.StorageClass
	key := client.ObjectKey{Name: DefaultStorageClassName(testCluster)}
	if err := r.Get(context.Background(), key, &class); err == nil {
		t.Error("the operator's own class outlived the pool it was written for")
	}
}

// U-29: a backend DELETE that 404s is success, since a pool already gone is a
// pool deleted.
func TestA404FromTheControlPlaneIsSuccess(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	cp.deleteStatus = http.StatusNotFound
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), deleting("tenant-a"))

	reconcileTimes(t, r, "tenant-a", 1)

	if poolExists(t, r, "tenant-a") {
		t.Error("a 404 from the control plane did not finish the deletion")
	}
}

// U-30: a backend DELETE that fails for any other reason is retried and the
// finalizer is kept. The likeliest refusal is the backend saying the pool still
// holds volumes, and treating that as success is how data is lost.
func TestAFailedBackendDeleteKeepsTheFinalizer(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	cp.deleteStatus = http.StatusInternalServerError
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), deleting("tenant-a"))

	_, result := reconcileTimes(t, r, "tenant-a", 1)

	if !poolExists(t, r, "tenant-a") {
		t.Fatal("the pool was released although the control plane refused to delete it")
	}
	if !rec.has(PoolDeletionFailed) {
		t.Errorf("no %s event: %+v", PoolDeletionFailed, rec.events)
	}
	if result.RequeueAfter == 0 {
		t.Error("the failed deletion was not scheduled to be retried")
	}
}

// U-28: a pool that never reached the control plane releases without a backend
// call, so it is not stuck in Terminating over a pool that does not exist.
func TestAPoolWithNoUUIDReleasesWithoutABackendCall(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	never := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	})
	r := newReconciler(t, cp, rec, newCluster(testClusterUUID), never)

	reconcileTimes(t, r, "tenant-a", 1)

	if cp.deletes != 0 {
		t.Errorf("the control plane was called %d times for a pool it never had, want 0", cp.deletes)
	}
	if poolExists(t, r, "tenant-a") {
		t.Error("the pool is stuck in Terminating with nothing to clean up")
	}
}

// A pool whose cluster has already gone still has to finish deleting, and there
// is no control plane left to reach.
func TestAPoolWhoseClusterWentFirstStillReleases(t *testing.T) {
	cp, rec := newControlPlane(t), &recorder{}
	r := newReconciler(t, cp, rec, deleting("tenant-a"))

	reconcileTimes(t, r, "tenant-a", 1)

	if poolExists(t, r, "tenant-a") {
		t.Error("the pool is stuck in Terminating behind a cluster that no longer exists")
	}
}
