// The subsystem as the unit of a migration.
//
// The control plane migrates an NVMe-oF subsystem rather than one volume inside
// it: every volume published under the subsystem moves at the one cutover. An
// operation names one volume, so the cases here are about the volumes it does
// not name, and what the operation owes them: the lock covers them, an
// operation naming one of them waits, and a subsystem that is already where it
// was asked to go is a move that has happened rather than one to make again.

package volume

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testSiblingID     = "88888888-8888-8888-8888-888888888888"
	testSiblingPVName = "pvc-" + testSiblingID
)

// siblingVolumeObject is a second PersistentVolume whose logical volume shares
// the test volume's subsystem.
func siblingVolumeObject() *corev1.PersistentVolume {
	pv := testVolumeObject()
	pv.Name = testSiblingPVName
	pv.Spec.CSI.VolumeHandle = string(lvol.NewVolumeHandle(testClusterID, testPoolID, testSiblingID))
	return pv
}

// sharedSubsystem is a control plane whose subsystem holds the test volume and
// its sibling, which no host consumes.
func sharedSubsystem() *fakeControlPlane {
	api := idleSubsystem()
	api.volume.StorageNodeID = testSourceID
	api.members = []lvol.Volume{
		{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, testVolumeID), NQN: testNQN, StorageNodeID: testSourceID},
		{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, testSiblingID), NQN: testNQN, StorageNodeID: testSourceID},
	}
	return api
}

// lockOnVolume reads the lock annotation off any PersistentVolume.
func lockOnVolume(t *testing.T, r *PersistentVolumeOpsReconciler, name string) string {
	t.Helper()
	var pv corev1.PersistentVolume
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &pv); err != nil {
		t.Fatalf("reading volume %s back: %v", name, err)
	}
	return pv.Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]
}

// Regression: 2026-10-05-pvops-per-volume-lock — the lock covered only the
// volume an operation named, so an operation naming a sibling in the same
// subsystem ran beside it and asked the control plane to move the whole
// subsystem a second time, to its own target.
func TestAnOperationHoldsEveryVolumeOfItsSubsystem(t *testing.T) {
	r := testReconciler(t, sharedSubsystem(), append(testWorld(), siblingVolumeObject())...)

	runPass(t, r)

	if got := lockOnVolume(t, r, testPVName); got != testOpsName {
		t.Errorf("the named volume's lock names %q, want %q", got, testOpsName)
	}
	if got := lockOnVolume(t, r, testSiblingPVName); got != testOpsName {
		t.Errorf("the sibling's lock names %q, want %q: the sibling moves with the subsystem, so "+
			"another operation may not move it meanwhile", got, testOpsName)
	}
}

// Regression: 2026-10-05-pvops-per-volume-lock — an operation naming a volume
// whose sibling another operation was moving took its own volume's lock and
// created a second migration of the same subsystem.
func TestAnOperationWaitsWhileASiblingOfItsVolumeIsBeingMoved(t *testing.T) {
	holder := testOperation()
	holder.Name = testHolderName
	holder.Spec.PersistentVolumeName = testSiblingPVName
	holder.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning

	sibling := siblingVolumeObject()
	sibling.Annotations = map[string]string{simplyblockv1alpha2.PersistentVolumeOpsLock: holder.Name}

	api := sharedSubsystem()
	r := testReconciler(t, api,
		holder, testOperation(), testVolumeObject(), sibling, testClusterObject(), testNodeObject())

	for range 3 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhasePending {
		t.Errorf("phase = %q, want Pending while the subsystem is being moved by %s",
			ops.Status.Phase, holder.Name)
	}
	if api.creates != 0 {
		t.Errorf("the control plane was asked for %d migrations of a subsystem another "+
			"operation is already moving, want none", api.creates)
	}
	if got := lockOnVolume(t, r, testPVName); got != "" {
		t.Errorf("the waiting operation holds volume %s (%q): a queued operation holds nothing",
			testPVName, got)
	}
	if got := lockOnVolume(t, r, testSiblingPVName); got != holder.Name {
		t.Errorf("the sibling's lock names %q, want the holder %q", got, holder.Name)
	}
}

// Regression: 2026-10-05-pvops-per-volume-lock — the release has to free every
// volume the acquisition took, or the subsystem's other volumes stay locked by
// a finished operation until somebody else breaks the lock.
func TestFinishingReleasesEveryVolumeOfItsSubsystem(t *testing.T) {
	ops := testOperation()
	named, sibling := testVolumeObject(), siblingVolumeObject()
	for _, pv := range []*corev1.PersistentVolume{named, sibling} {
		pv.Annotations = map[string]string{simplyblockv1alpha2.PersistentVolumeOpsLock: testOpsName}
	}
	r := testReconciler(t, sharedSubsystem(), ops, named, sibling)

	if err := r.releaseLock(context.Background(), ops); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{testPVName, testSiblingPVName} {
		if got := lockOnVolume(t, r, name); got != "" {
			t.Errorf("volume %s is still locked by %q after the operation released", name, got)
		}
	}
}

// Regression: 2026-10-05-pvops-per-volume-lock — an operation naming a volume
// whose subsystem had already been moved to the target, by an operation naming
// a sibling, asked the control plane to migrate it onto the node it was on. The
// control plane refuses that, and the operation retried the refusal until its
// step's deadline failed it, although what it asked for was already true.
func TestASubsystemAlreadyOnTheTargetSucceedsWithoutAMigration(t *testing.T) {
	api := sharedSubsystem()
	api.volume.StorageNodeID = testTargetID
	for i := range api.members {
		api.members[i].StorageNodeID = testTargetID
	}
	r := testReconciler(t, api, append(testWorld(), siblingVolumeObject())...)

	for range 4 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Errorf("phase = %q (%s), want Succeeded: the subsystem is already on the target",
			ops.Status.Phase, ops.Status.Message)
	}
	if api.creates != 0 {
		t.Errorf("the control plane was asked for %d migrations onto the node the volume is on, want none",
			api.creates)
	}
	for _, name := range []string{testPVName, testSiblingPVName} {
		if got := lockOnVolume(t, r, name); got != "" {
			t.Errorf("volume %s is still locked by %q after the operation finished", name, got)
		}
	}
}
