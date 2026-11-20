// What the event says about a task that has left the window.
//
// The window is what is running now, so a task's whole history is the one event
// emitted when it leaves. That event said only that the task was no longer
// running, which is true of one that succeeded and of one that exhausted its
// retries, and the difference is the whole of what a reader wants.
//
// The control plane publishes it. A node_add that gave up came back with retry
// 11 and a function_result of max retry reached, decoded into the DTO and read
// by nothing, while the event was raised as Normal from the previous snapshot.

package cluster

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

// aClusterRunning is a cluster whose status already records the task, which is
// what makes its disappearance visible on the next read.
func aClusterRunning(tasks ...simplyblockv1alpha2.ClusterTask) *simplyblockv1alpha2.StorageCluster {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "a-cluster", Namespace: "simplyblock"},
	}
	cluster.Status.UUID = "cluster-uuid"
	cluster.Status.Tasks = tasks
	return cluster
}

func aTaskReader(rec *recorder, reported ...subscriptions.TaskDTO) *StorageClusterReconciler {
	return &StorageClusterReconciler{
		Recorder: rec,
		API: &fakeControlPlane{
			tasks: func(string) ([]subscriptions.TaskDTO, error) { return reported, nil },
		},
	}
}

// A task that gave up says so, with what the control plane said about it.
func TestATaskThatExhaustedItsRetriesIsReportedAsOne(t *testing.T) {
	rec := &recorder{}
	cluster := aClusterRunning(simplyblockv1alpha2.ClusterTask{
		ID: "584ef009", Type: "node_add", Status: "running", Retry: 10,
	})
	r := aTaskReader(rec, subscriptions.TaskDTO{
		ID:     "584ef009",
		Type:   "node_add",
		Status: "done",
		Retry:  11,
		Result: "max retry reached (11/11)",
	})

	r.readTasks(context.Background(), cluster)

	if !rec.has(TaskGaveUp) {
		t.Fatalf("a task that exhausted its retries raised %+v", rec.events)
	}
	if got := rec.typeFor(TaskGaveUp); got != corev1.EventTypeWarning {
		t.Errorf("the event is %q, want a warning", got)
	}
	note := rec.noteFor(TaskGaveUp)
	for _, want := range []string{"node_add", "max retry reached (11/11)", "11"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not carry %q: %s", want, note)
		}
	}
	if rec.has(TaskCompleted) {
		t.Error("a task that gave up was also reported as completed")
	}
}

// A task that finished without ever being restarted is a completion, and stays
// the quiet event it was.
func TestATaskThatFinishedCleanlyIsStillACompletion(t *testing.T) {
	rec := &recorder{}
	cluster := aClusterRunning(simplyblockv1alpha2.ClusterTask{
		ID: "t-1", Type: "cluster_status", Status: "running",
	})
	r := aTaskReader(rec)

	r.readTasks(context.Background(), cluster)

	if !rec.has(TaskCompleted) {
		t.Fatalf("a clean finish raised %+v", rec.events)
	}
	if got := rec.typeFor(TaskCompleted); got != corev1.EventTypeNormal {
		t.Errorf("the event is %q, want a normal one", got)
	}
	if rec.has(TaskGaveUp) {
		t.Error("a task that never retried was reported as having given up")
	}
}

// The retry count is the schema's own signal that a task was failing rather
// than slow, so a task that left the window having been restarted is reported
// as having given up even when the control plane said nothing else about it.
func TestARetriedTaskIsReportedEvenWithNoResult(t *testing.T) {
	rec := &recorder{}
	cluster := aClusterRunning(simplyblockv1alpha2.ClusterTask{
		ID: "t-2", Type: "node_add", Status: "running", Retry: 3,
	})
	r := aTaskReader(rec)

	r.readTasks(context.Background(), cluster)

	if !rec.has(TaskGaveUp) {
		t.Fatalf("a retried task that vanished raised %+v", rec.events)
	}
	if note := rec.noteFor(TaskGaveUp); !strings.Contains(note, "3") {
		t.Errorf("the note does not carry the retry count: %s", note)
	}
}

// A task still running is not reported at all, which is what keeps the event a
// record of history rather than a heartbeat.
func TestARunningTaskRaisesNothing(t *testing.T) {
	rec := &recorder{}
	cluster := aClusterRunning(simplyblockv1alpha2.ClusterTask{
		ID: "t-3", Type: "node_add", Status: "running",
	})
	r := aTaskReader(rec, subscriptions.TaskDTO{
		ID: "t-3", Type: "node_add", Status: "running",
	})

	r.readTasks(context.Background(), cluster)

	if len(rec.events) != 0 {
		t.Errorf("a running task raised %+v", rec.events)
	}
}
