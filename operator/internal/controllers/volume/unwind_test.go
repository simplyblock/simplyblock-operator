// What taking a migration back does when the control plane refuses the cancel
// because the migration has already ended.
//
// An operation fails, or is aborted, most often because the migration itself
// ended: by then the control plane has marked it terminal and answers a cancel
// with a refusal saying it is not active. That is the state the cancel asks
// for, so the unwind goes on to release what the operation built on the
// consuming hosts.

package volume

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// notActive is the control plane's refusal of a cancel for a subsystem's
// migration group that has already failed.
const notActive = "Group " + testMigrationID + " is not active (status=failed)"

// Regression: SFAM-2824 — a migration that failed was answered with a 400 "not
// active" when the operation took it back, and the unwind stopped there: the
// validation Jobs stayed, the hosts that had connected the target paths were
// never told to release them, and the operation ended Failed still recording
// every path it built.
func TestAFailedMigrationReleasesItsPathsWhenTheCancelIsRefused(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api,
		testOperation(), testClusterObject(), testNodeObject(),
		claimedVolume(testPVName, "data-0"),
		runningPodOn("worker-1", "data-0"),
	)

	for range 3 {
		runPass(t, r)
	}
	ops := operationFrom(t, r)
	if ops.Status.Migration == nil || len(ops.Status.Migration.ValidationJobs) == 0 ||
		len(ops.Status.Migration.Connections) == 0 {
		t.Fatalf("the operation built nothing to release: %+v", ops.Status.Migration)
	}
	validationJobs := ops.Status.Migration.ValidationJobs

	// The migration fails while the operation waits on it, and the control
	// plane then refuses the cancel because it is over.
	api.cancelErr = cancelAnswer(t, http.StatusBadRequest, notActive)
	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	ops.Status.Step = statemachine.KubeSnapshot{State: ops.Status.Step.State, Deadline: &expired}
	if err := r.Status().Update(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
	runPass(t, r)

	ops = operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if strings.Contains(ops.Status.Message, "taking the migration back also failed") {
		t.Errorf("message = %q, want an ended migration read as taken back", ops.Status.Message)
	}
	if n := len(ops.Status.Migration.Connections); n != 0 {
		t.Errorf("the failed operation still records %d target paths as connected", n)
	}
	for _, record := range validationJobs {
		var job batchv1.Job
		err := r.Get(context.Background(), types.NamespacedName{Namespace: record.Namespace, Name: record.Name}, &job)
		if err == nil {
			t.Errorf("validation Job %s is still there", record.Name)
		}
	}
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(validationJobs[0].Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) == 0 {
		t.Error("no release was started on the host that connected the target paths")
	}
}

// Regression: SFAM-2824 — an abort of a migration that had already ended was
// requeued on the refused cancel forever, so the operation never finished and
// kept the volume locked against every later migration.
func TestAnAbortOfAnEndedMigrationFinishes(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api, testWorld()...)

	runPass(t, r)
	runPass(t, r)

	api.cancelErr = cancelAnswer(t, http.StatusBadRequest, notActive)
	ops := operationFrom(t, r)
	ops.Spec.Abort = true
	if err := r.Update(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
	runPass(t, r)

	ops = operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted {
		t.Fatalf("phase = %q (%s), want Aborted", ops.Status.Phase, ops.Status.Message)
	}
	if got := lockOn(t, r); got != "" {
		t.Errorf("the volume is still locked by %q", got)
	}
}

// Regression: SFAM-2824 — a migration the control plane no longer knows is as
// taken back as one it refuses to cancel for having ended, and a delete held on
// it would never be released.
func TestDeletingAnOperationWhoseMigrationIsGoneLetsTheObjectGo(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api, testWorld()...)

	runPass(t, r)
	runPass(t, r)

	api.cancelErr = cancelAnswer(t, http.StatusNotFound, "Migration "+testMigrationID+" not found")
	if err := r.Delete(context.Background(), operationFrom(t, r)); err != nil {
		t.Fatal(err)
	}
	runPass(t, r)

	var ops simplyblockv1alpha2.PersistentVolumeOps
	if err := r.Get(context.Background(), types.NamespacedName{Name: testOpsName}, &ops); err == nil {
		t.Errorf("the operation is still held with finalizers %v (%s)", ops.Finalizers, ops.Status.Message)
	}
}

// Any other refusal of the cancel is still one: the migration may be running,
// and ending the operation would abandon it with its paths connected.
func TestAnAbortHeldOnAnyOtherRefusedCancelWaits(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api, testWorld()...)

	runPass(t, r)
	runPass(t, r)

	api.cancelErr = cancelAnswer(t, http.StatusBadRequest, "LVolMigrationGroup g not found")
	ops := operationFrom(t, r)
	ops.Spec.Abort = true
	if err := r.Update(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
	runPass(t, r)

	if ops = operationFrom(t, r); terminal(ops.Status.Phase) {
		t.Errorf("phase = %q, want the abort held while the cancel is refused", ops.Status.Phase)
	}
}
