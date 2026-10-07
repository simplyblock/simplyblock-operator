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
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
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
	if job.UID == "" {
		// The fake client assigns none, and the pod's owner reference is how a
		// result is tied to this Job.
		job.UID = types.UID("uid-" + record.Name)
		if err := r.Update(ctx, &job); err != nil {
			t.Fatalf("give the validation Job a UID: %v", err)
		}
	}
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: record.Name + "-x7k2q", Namespace: record.Namespace,
			Labels: map[string]string{"job-name": record.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: record.Name, UID: job.UID, Controller: &controller,
			}},
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

// A validated host passes, and the operation goes on to the copy.
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

// Regression: 2026-10-07-pvops-complete-job-read-as-pass — a validation Job
// that completed counted as a pass, and the validate mode also exits zero when
// it skips a host with no connection to the subsystem.
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

// Regression: 2026-10-07-pvops-complete-job-read-as-pass — a Job that reports
// no result validated nothing.
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

// Regression: 2026-10-07-pvops-result-from-a-stale-pod — Job names are stable
// per operation and node, so a pod of an earlier Job of the same name can
// outlive it. Its termination message is not this Job's result.
func TestAResultFromAPodOfAnotherJobIsNotAccepted(t *testing.T) {
	r, record := validatingOnOneHost(t)
	completeWith(t, r, record, `{"outcome":"validated"}`)
	ctx := context.Background()
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: record.Namespace, Name: record.Name + "-x7k2q"}, &pod); err != nil {
		t.Fatalf("read the pod: %v", err)
	}
	controller := true
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: record.Name, UID: "uid-of-an-earlier-job", Controller: &controller,
	}}
	if err := r.Update(ctx, &pod); err != nil {
		t.Fatalf("give the pod an earlier owner: %v", err)
	}

	for range 3 {
		runPass(t, r)
	}

	if ops := operationFrom(t, r); ops.Status.Step.State == string(stepMigrating) {
		t.Errorf("the operation reached Migrating on the result of a pod another Job owns")
	}
}

// The binary writes its result where VMIG_RESULT_PATH says and the kubelet
// reads the container's terminationMessagePath. Both ends pass their own tests
// with any value, so the Job is what has to name the same path twice.
func TestTheValidationJobReadsTheResultFromWhereTheBinaryWritesIt(t *testing.T) {
	r, record := validatingOnOneHost(t)
	var job batchv1.Job
	if err := r.Get(context.Background(),
		types.NamespacedName{Namespace: record.Namespace, Name: record.Name}, &job); err != nil {
		t.Fatalf("read the validation Job: %v", err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	written := ""
	for _, env := range container.Env {
		if env.Name == vmigration.ValidationResultPathEnv {
			written = env.Value
		}
	}
	if written == "" {
		t.Fatalf("the Job does not tell the binary where to write its result")
	}
	if container.TerminationMessagePath != written {
		t.Errorf("the kubelet reads %q, the binary writes %q", container.TerminationMessagePath, written)
	}
}
