// The volume's lock, which is the one mechanism in this package whose failure
// is silent and expensive: two migrations of one volume at once would have two
// backend migrations copying the same logical volume to two places.
//
// Every other Ops kind takes status.activeOpsRef on its target. A
// PersistentVolume is a core type this operator must not add a field to, so the
// lock is an annotation — and an annotation is only a lock if it has the three
// properties the group requires of one. Each of them is a case below.

package volume

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// volumeFrom reads the volume the way a reconcile does. The resource version
// is what the optimistic lock patches against, so a lock taken on a
// hand-built copy would not be taking one at all.
func volumeFrom(t *testing.T, r *PersistentVolumeOpsReconciler) *corev1.PersistentVolume {
	t.Helper()
	var pv corev1.PersistentVolume
	if err := r.Get(context.Background(), types.NamespacedName{Name: testPVName}, &pv); err != nil {
		t.Fatalf("reading the volume: %v", err)
	}
	return &pv
}

// testHolderName is the other operation every contention case here is against.
const testHolderName = "move-0"

// lockOn reads the annotation back off the volume.
func lockOn(t *testing.T, r *PersistentVolumeOpsReconciler) string {
	t.Helper()
	var pv corev1.PersistentVolume
	if err := r.Get(context.Background(), types.NamespacedName{Name: testPVName}, &pv); err != nil {
		t.Fatalf("reading the volume back: %v", err)
	}
	return pv.Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]
}

// A free volume is taken, and the annotation names the operation that took it.
func TestTheLockIsTakenWhenTheVolumeIsFree(t *testing.T) {
	ops := testOperation()
	r := testReconciler(t, &fakeControlPlane{}, ops, testVolumeObject())

	acquired, err := r.acquireLock(context.Background(), ops, volumeFrom(t, r))
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("the operation did not take a lock nobody holds")
	}
	if got := lockOn(t, r); got != testOpsName {
		t.Errorf("the volume's lock names %q, want %q", got, testOpsName)
	}
}

// Re-acquiring a lock this operation already holds is not a second acquisition.
// Every pass takes the lock again, so a reconcile that treated its own lock as
// somebody else's would never get past Pending.
func TestTheHolderReadoptsItsOwnLock(t *testing.T) {
	ops := testOperation()
	pv := testVolumeObject()
	pv.Annotations = map[string]string{
		simplyblockv1alpha2.PersistentVolumeOpsLock: testOpsName,
	}
	r := testReconciler(t, &fakeControlPlane{}, ops, pv)

	acquired, err := r.acquireLock(context.Background(), ops, volumeFrom(t, r))
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Error("the operation could not re-adopt the lock it already holds")
	}
}

// A lock held by a live operation is waited on rather than taken. Failing here
// instead would make the order two people applied two objects in decide which
// of them runs.
func TestALockHeldByALiveOperationIsWaitedOn(t *testing.T) {
	holder := testOperation()
	holder.Name = testHolderName
	holder.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning

	ops := testOperation()
	pv := testVolumeObject()
	pv.Annotations = map[string]string{
		simplyblockv1alpha2.PersistentVolumeOpsLock: holder.Name,
	}
	r := testReconciler(t, &fakeControlPlane{}, holder, ops, pv)

	acquired, err := r.acquireLock(context.Background(), ops, volumeFrom(t, r))
	if err != nil {
		t.Fatal(err)
	}
	if acquired {
		t.Fatal("the operation took a lock another running operation holds")
	}
	if got := lockOn(t, r); got != holder.Name {
		t.Errorf("the volume's lock now names %q, want the holder %q", got, holder.Name)
	}
}

// A lock whose holder finished is broken rather than waited out. The release
// runs on every terminal path, so a lock still naming a terminal operation
// means the release did not run — a crash between the two — and waiting for a
// release that will never come would block the volume forever.
func TestALockHeldByATerminalOperationIsBroken(t *testing.T) {
	for _, phase := range []simplyblockv1alpha2.PersistentVolumeOpsPhase{
		simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded,
		simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed,
		simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted,
	} {
		t.Run(string(phase), func(t *testing.T) {
			holder := testOperation()
			holder.Name = testHolderName
			holder.Status.Phase = phase

			ops := testOperation()
			pv := testVolumeObject()
			pv.Annotations = map[string]string{
				simplyblockv1alpha2.PersistentVolumeOpsLock: holder.Name,
			}
			r := testReconciler(t, &fakeControlPlane{}, holder, ops, pv)

			acquired, err := r.acquireLock(context.Background(), ops, volumeFrom(t, r))
			if err != nil {
				t.Fatal(err)
			}
			if !acquired {
				t.Fatal("the operation waited on a lock whose holder had finished")
			}
			if got := lockOn(t, r); got != testOpsName {
				t.Errorf("the volume's lock names %q, want %q", got, testOpsName)
			}
		})
	}
}

// A lock naming an operation that no longer exists is broken too. That is the
// object deleted with its finalizer forced or removed out of band, and the
// annotation is the only thing left of it.
func TestALockHeldByNobodyIsBroken(t *testing.T) {
	ops := testOperation()
	pv := testVolumeObject()
	pv.Annotations = map[string]string{
		simplyblockv1alpha2.PersistentVolumeOpsLock: testHolderName,
	}
	r := testReconciler(t, &fakeControlPlane{}, ops, pv)

	acquired, err := r.acquireLock(context.Background(), ops, volumeFrom(t, r))
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("the operation waited on a lock held by an operation that does not exist")
	}
	if got := lockOn(t, r); got != testOpsName {
		t.Errorf("the volume's lock names %q, want %q", got, testOpsName)
	}
}

// Release clears the annotation, and only while it still names the releaser. A
// late release that cleared somebody else's lock would be worse than not
// releasing at all: two operations would then run against one volume with
// neither of them knowing.
func TestReleaseClearsOnlyThisOperationsLock(t *testing.T) {
	t.Run("its own lock is cleared", func(t *testing.T) {
		ops := testOperation()
		pv := testVolumeObject()
		pv.Annotations = map[string]string{
			simplyblockv1alpha2.PersistentVolumeOpsLock: testOpsName,
		}
		r := testReconciler(t, &fakeControlPlane{}, ops, pv)

		if err := r.releaseLock(context.Background(), ops); err != nil {
			t.Fatal(err)
		}
		if got := lockOn(t, r); got != "" {
			t.Errorf("the volume is still locked by %q", got)
		}
	})

	t.Run("somebody else's lock is left alone", func(t *testing.T) {
		ops := testOperation()
		pv := testVolumeObject()
		pv.Annotations = map[string]string{
			simplyblockv1alpha2.PersistentVolumeOpsLock: testHolderName,
		}
		r := testReconciler(t, &fakeControlPlane{}, ops, pv)

		if err := r.releaseLock(context.Background(), ops); err != nil {
			t.Fatal(err)
		}
		if got := lockOn(t, r); got != testHolderName {
			t.Errorf("the volume's lock is now %q, want it untouched", got)
		}
	})
}

// A volume that is gone takes its lock with it, which is correct for the lock
// and says nothing about the copy: the backing logical volume outlives the
// Kubernetes object. Releasing is what must not fail here, because the release
// runs on the deletion path and a failure would hold the object open forever.
func TestReleasingALockOnAVolumeThatIsGoneSucceeds(t *testing.T) {
	ops := testOperation()
	r := testReconciler(t, &fakeControlPlane{}, ops)

	if err := r.releaseLock(context.Background(), ops); err != nil {
		t.Errorf("releasing the lock on a deleted volume failed: %v", err)
	}
}

// The annotation is under the group's own prefix, which is what makes it
// identifiable at a glance on an object this API group does not own, and
// matchable by one RBAC or admission rule.
func TestTheLockKeyCarriesTheGroupsPrefix(t *testing.T) {
	const prefix = "storage.simplyblock.io/"
	if key := simplyblockv1alpha2.PersistentVolumeOpsLock; len(key) <= len(prefix) ||
		key[:len(prefix)] != prefix {
		t.Errorf("the lock key is %q, which does not carry %q", key, prefix)
	}
}

// The lock touches neither spec nor status, so it cannot conflict with the
// provisioner or the volume's own controllers.
func TestTakingTheLockLeavesTheVolumeOtherwiseUntouched(t *testing.T) {
	ops := testOperation()
	pv := testVolumeObject()
	pv.Labels = map[string]string{"kept": "yes"}
	r := testReconciler(t, &fakeControlPlane{}, ops, pv)

	if _, err := r.acquireLock(context.Background(), ops, volumeFrom(t, r)); err != nil {
		t.Fatal(err)
	}

	var got corev1.PersistentVolume
	if err := r.Get(context.Background(), types.NamespacedName{Name: testPVName}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels["kept"] != "yes" {
		t.Errorf("labels = %v, want the ones the volume had", got.Labels)
	}
	if got.Spec.CSI == nil || got.Spec.CSI.VolumeHandle != pv.Spec.CSI.VolumeHandle {
		t.Errorf("the volume's spec changed: %+v", got.Spec.CSI)
	}
	if got.Status.Phase != pv.Status.Phase || got.Status.Message != pv.Status.Message {
		t.Errorf("the volume's status changed: %+v", got.Status)
	}
}
