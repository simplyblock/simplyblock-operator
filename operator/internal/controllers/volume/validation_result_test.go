// What a finished validation Job counts as.
//
// A Job that completed says the process exited zero, and the validate mode
// exits zero both when it validated the target's paths and when it skipped the
// host. Only the result the Job reports decides whether the host is ready for
// the cutover, and a result that is anything but validated, or missing, fails
// the operation instead of retrying.

package volume

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// validatingOnOneHost drives the operation until its one consumer's validation
// Job exists, and returns that Job's record.
func validatingOnOneHost(t *testing.T) (*PersistentVolumeOpsReconciler, simplyblockv1alpha2.ValidationJob) {
	t.Helper()
	r := testReconciler(t, idleSubsystem(),
		testOperation(), testClusterObject(), testNodeObject(),
		claimedVolume(testPVName, "data-0"),
		runningPodOn("worker-1", "app", "data-0"))
	for range 3 {
		runPass(t, r)
	}
	ops := operationFrom(t, r)
	if ops.Status.Migration == nil || len(ops.Status.Migration.ValidationJobs) != 1 {
		t.Fatalf("no validation Job was started: %+v", ops.Status.Migration)
	}
	return r, ops.Status.Migration.ValidationJobs[0]
}

// completeWith marks the Job Complete and gives it the pod a finished Job
// leaves, with the termination message its container reported.
func completeWith(t *testing.T, r *PersistentVolumeOpsReconciler, record simplyblockv1alpha2.ValidationJob, message string) {
	t.Helper()
	ctx := context.Background()
	var job batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Namespace: record.Namespace, Name: record.Name}, &job); err != nil {
		t.Fatalf("read the validation Job: %v", err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(ctx, &job); err != nil {
		t.Fatalf("complete the validation Job: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: record.Name + "-x7k2q", Namespace: record.Namespace,
			Labels: map[string]string{"job-name": record.Name},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: containerFor(modeValidate),
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, Message: message,
				}},
			}},
		},
	}
	if err := r.Create(ctx, pod); err != nil {
		t.Fatalf("create the validation pod: %v", err)
	}
}

func TestAValidatedHostPassesTheCheck(t *testing.T) {
	r, record := validatingOnOneHost(t)
	completeWith(t, r, record, `{"outcome":"validated"}`)

	for range 3 {
		runPass(t, r)
	}

	if ops := operationFrom(t, r); ops.Status.Step.State != string(stepMigrating) {
		t.Errorf("step = %q, phase = %q (%s), want Migrating after a validated host",
			ops.Status.Step.State, ops.Status.Phase, ops.Status.Message)
	}
}

func TestACompletedJobThatSkippedTheHostFailsTheOperation(t *testing.T) {
	r, record := validatingOnOneHost(t)
	completeWith(t, r, record, `{"outcome":"skipped","detail":"no host connection to the subsystem"}`)

	for range 3 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Errorf("phase = %q, step = %q (%s), want Failed: the host's paths were never validated",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
}

func TestACompletedJobWithNoResultFailsTheOperation(t *testing.T) {
	r, record := validatingOnOneHost(t)
	completeWith(t, r, record, "")

	for range 3 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Errorf("phase = %q, step = %q (%s), want Failed: a Job that reports no result validated nothing",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
}
