// One migration per storage cluster at a time.
//
// The control plane runs one data migration per cluster and refuses the rest,
// so an operation that leaves Pending beside another one only waits inside its
// step and reports Running for a migration that is not running. The slot keeps
// every operation but one at Pending.

package volume

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	secondOpsName  = "move-2"
	secondVolumeID = "88888888-8888-8888-8888-888888888888"
	secondPVName   = "pvc-" + secondVolumeID
)

// secondOperation moves another volume of the same cluster, on a subsystem of
// its own, so the only thing the two operations share is the cluster.
func secondOperation() *simplyblockv1alpha2.PersistentVolumeOps {
	ops := testOperation()
	ops.Name = secondOpsName
	ops.UID = types.UID("uid-" + secondOpsName)
	ops.Spec.PersistentVolumeName = secondPVName
	return ops
}

func secondVolumeObject() *corev1.PersistentVolume {
	pv := testVolumeObject()
	pv.Name = secondPVName
	pv.Spec.CSI.VolumeHandle = string(lvol.NewVolumeHandle(testClusterID, testPoolID, secondVolumeID))
	return pv
}

func runPassOn(t *testing.T, r *PersistentVolumeOpsReconciler, name string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	return result
}

func operationNamed(t *testing.T, r *PersistentVolumeOpsReconciler, name string) *simplyblockv1alpha2.PersistentVolumeOps {
	t.Helper()
	var ops simplyblockv1alpha2.PersistentVolumeOps
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &ops); err != nil {
		t.Fatalf("read operation %s: %v", name, err)
	}
	return &ops
}

func slotHolder(t *testing.T, r *PersistentVolumeOpsReconciler) string {
	t.Helper()
	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: testClusterCR}, &cluster); err != nil {
		t.Fatalf("read the cluster: %v", err)
	}
	return cluster.Annotations[simplyblockv1alpha2.StorageClusterMigrationSlot]
}

func twoOperations(t *testing.T) *PersistentVolumeOpsReconciler {
	t.Helper()
	return testReconciler(t, idleSubsystem(),
		testOperation(), secondOperation(),
		testVolumeObject(), secondVolumeObject(),
		testClusterObject(), testNodeObject())
}

func TestOnlyOneOperationPerClusterLeavesPending(t *testing.T) {
	r := twoOperations(t)

	for range 3 {
		runPassOn(t, r, testOpsName)
		runPassOn(t, r, secondOpsName)
	}

	first, second := operationNamed(t, r, testOpsName), operationNamed(t, r, secondOpsName)
	if first.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning {
		t.Fatalf("first phase = %q (%s), want Running", first.Status.Phase, first.Status.Message)
	}
	if second.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhasePending {
		t.Fatalf("second phase = %q (%s), want Pending while the first holds the cluster's slot",
			second.Status.Phase, second.Status.Message)
	}
	if second.Status.Step.State != "" {
		t.Errorf("second entered step %q without the slot", second.Status.Step.State)
	}
	if !strings.Contains(second.Status.Message, testOpsName) {
		t.Errorf("second message = %q, want it to name the operation holding the slot", second.Status.Message)
	}
	if got := slotHolder(t, r); got != testOpsName {
		t.Errorf("the slot names %q, want %q", got, testOpsName)
	}
}

func TestTheSlotPassesOnWhenItsHolderFinishes(t *testing.T) {
	r := twoOperations(t)
	runPassOn(t, r, testOpsName)
	runPassOn(t, r, secondOpsName)

	first := operationNamed(t, r, testOpsName)
	if _, err := r.finish(context.Background(), first,
		simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded, "done"); err != nil {
		t.Fatalf("finish the first operation: %v", err)
	}
	if got := slotHolder(t, r); got != "" {
		t.Errorf("a finished operation still holds the slot: %q", got)
	}

	runPassOn(t, r, secondOpsName)

	if second := operationNamed(t, r, secondOpsName); second.Status.Phase !=
		simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning {
		t.Errorf("second phase = %q (%s), want Running once the slot is free",
			second.Status.Phase, second.Status.Message)
	}
}

func TestASlotHeldByAnOperationThatIsGoneIsBroken(t *testing.T) {
	cluster := testClusterObject()
	cluster.Annotations = map[string]string{simplyblockv1alpha2.StorageClusterMigrationSlot: "deleted-long-ago"}
	r := testReconciler(t, idleSubsystem(), testOperation(), testVolumeObject(), cluster, testNodeObject())

	runPass(t, r)

	if ops := operationFrom(t, r); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning {
		t.Errorf("phase = %q (%s), want Running: the slot's holder does not exist",
			ops.Status.Phase, ops.Status.Message)
	}
}

func TestASlotHeldByATerminalOperationIsBroken(t *testing.T) {
	holder := secondOperation()
	holder.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed
	cluster := testClusterObject()
	cluster.Annotations = map[string]string{simplyblockv1alpha2.StorageClusterMigrationSlot: holder.Name}
	r := testReconciler(t, idleSubsystem(),
		testOperation(), holder, testVolumeObject(), secondVolumeObject(), cluster, testNodeObject())

	runPass(t, r)

	if ops := operationFrom(t, r); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning {
		t.Errorf("phase = %q (%s), want Running: the slot's holder has finished",
			ops.Status.Phase, ops.Status.Message)
	}
}

func TestAnOperationWaitingOnItsVolumeHoldsNoSlot(t *testing.T) {
	holder := secondOperation()
	holder.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning
	pv := testVolumeObject()
	pv.Annotations = map[string]string{simplyblockv1alpha2.PersistentVolumeOpsLock: holder.Name}
	r := testReconciler(t, idleSubsystem(),
		testOperation(), holder, pv, secondVolumeObject(), testClusterObject(), testNodeObject())

	runPass(t, r)

	if got := slotHolder(t, r); got == testOpsName {
		t.Error("an operation queued on its volume's lock kept the cluster's slot")
	}
}
