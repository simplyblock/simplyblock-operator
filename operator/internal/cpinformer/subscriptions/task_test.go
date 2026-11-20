// Tests for the task subscription: that a snapshot caches and marks a scope
// synced, that a trigger names the StorageCluster whose window the task
// appears in, and that Finished reads the control plane's own status enum.
//
// Finished is the one with teeth. It decides which tasks are in the window and
// when a CancelTask operation is done, and the enum has four values of which
// only one is terminal — a suspended task is waiting, not finished.

package subscriptions

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

const taskCluster = "55555555-5555-5555-5555-555555555555"

// taskObject is the StorageCluster whose status.tasks window the cluster's
// tasks appear in.
var taskObject = types.NamespacedName{Namespace: "simplyblock", Name: "production"}

// taskScope is a cluster on its own: one stream serves every task of it.
func taskScope() cpinformer.Scope { return cpinformer.Scope{taskCluster} }

func registeredTasks(t *testing.T) *TaskSubscription {
	t.Helper()
	sub := NewTaskSubscription()
	sub.RegisterCluster(taskCluster, taskObject)
	return sub
}

func ingestTask(t *testing.T, sub *TaskSubscription, kind, data string) {
	t.Helper()
	err := sub.Ingest(context.Background(), cpinformer.Event{
		Kind: kind, Scope: taskScope(), Data: []byte(data),
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", kind, err)
	}
}

func drainTaskTrigger(t *testing.T, sub *TaskSubscription) types.NamespacedName {
	t.Helper()
	select {
	case ev := <-sub.Triggers():
		return types.NamespacedName{
			Namespace: ev.Object.GetNamespace(),
			Name:      ev.Object.GetName(),
		}
	case <-time.After(time.Second):
		t.Fatal("no reconcile trigger enqueued")
		return types.NamespacedName{}
	}
}

// The status enum is new, running, suspended, or done. Only done and a
// canceled flag end a task; suspended is one that is waiting, and reading it as
// finished would drop a task out of the window while the cluster is still
// holding it.
func TestOnlyDoneAndCanceledTasksAreFinished(t *testing.T) {
	for name, tc := range map[string]struct {
		task TaskDTO
		want bool
	}{
		"new":                 {TaskDTO{Status: "new"}, false},
		"running":             {TaskDTO{Status: "running"}, false},
		"suspended":           {TaskDTO{Status: "suspended"}, false},
		"done":                {TaskDTO{Status: "done"}, true},
		"canceled while new":  {TaskDTO{Status: "new", Canceled: true}, true},
		"canceled while done": {TaskDTO{Status: "done", Canceled: true}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.task.Finished(); got != tc.want {
				t.Errorf("Finished() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTheTaskStreamIsScopedPerCluster(t *testing.T) {
	sub := NewTaskSubscription()
	want := "/api/v2/clusters/" + taskCluster + "/tasks/"
	if got := sub.Path(taskScope()); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// A snapshot caches every task it carries and marks the cluster's scope
// synced. The gate matters most here: an unsynced cache holds no tasks, and a
// CancelTask reading that as "the task is gone" would report every cancel
// complete the moment it was asked for.
func TestATaskSnapshotCachesAndSyncs(t *testing.T) {
	sub := registeredTasks(t)
	if sub.Synced(taskScope()) {
		t.Fatal("the scope reported synced before any snapshot arrived")
	}

	ingestTask(t, sub, cpinformer.EventSnapshot, `[
		{"id":"task-1","function_name":"balancing_on_restart","status":"running",
		 "function_result":"","canceled":false,"retry":0},
		{"id":"task-2","function_name":"lvol_migration","status":"suspended",
		 "function_result":"","canceled":false,"retry":3}
	]`)

	if !sub.Synced(taskScope()) {
		t.Error("the scope did not report synced after its snapshot")
	}
	if got := sub.List(taskScope()); len(got) != 2 {
		t.Fatalf("the cache holds %d tasks, want 2", len(got))
	}
	task, ok := sub.Lookup("task-2")
	if !ok {
		t.Fatal("task-2 is not cached")
	}
	if task.Type != "lvol_migration" || task.Status != "suspended" || task.Retry != 3 {
		t.Errorf("the snapshot decoded wrong: %+v", task)
	}
}

// Every change triggers, including a task reaching a terminal status: a task
// leaving the window is as much a change to status.tasks as one arriving, and
// it is what the TaskCompleted event is raised from.
func TestATaskReachingDoneStillTriggers(t *testing.T) {
	sub := registeredTasks(t)
	ingestTask(t, sub, cpinformer.EventSnapshot, `[
		{"id":"task-1","function_name":"node_restart","status":"running",
		 "function_result":"","canceled":false,"retry":0}
	]`)
	if got := drainTaskTrigger(t, sub); got != taskObject {
		t.Fatalf("the snapshot named %v, want %v", got, taskObject)
	}

	ingestTask(t, sub, cpinformer.EventUpdated, `
		{"id":"task-1","function_name":"node_restart","status":"done",
		 "function_result":"Success","canceled":false,"retry":0}`)

	if got := drainTaskTrigger(t, sub); got != taskObject {
		t.Errorf("the completion named %v, want %v", got, taskObject)
	}
	task, ok := sub.Lookup("task-1")
	if !ok {
		t.Fatal("a finished task left the cache; the window needs it to know it ended")
	}
	if !task.Finished() {
		t.Errorf("the completion did not reach the cache: %+v", task)
	}
}

// A cluster the operator has not adopted yields no trigger: there is no object
// whose window the tasks would appear in.
func TestTasksOfAnUnregisteredClusterAreNotTriggered(t *testing.T) {
	sub := NewTaskSubscription()

	ingestTask(t, sub, cpinformer.EventUpdated, `
		{"id":"task-1","function_name":"node_restart","status":"running",
		 "function_result":"","canceled":false,"retry":0}`)

	if _, ok := sub.Lookup("task-1"); !ok {
		t.Error("a task of an unadopted cluster was not cached")
	}
	select {
	case ev := <-sub.Triggers():
		t.Errorf("an unadopted cluster's task triggered a reconcile of %s/%s",
			ev.Object.GetNamespace(), ev.Object.GetName())
	default:
	}
}

// A snapshot replaces its scope, so a task that ended while the stream was
// down is gone after the reconnect rather than held in the window forever.
func TestATaskSnapshotReplacesRatherThanMerges(t *testing.T) {
	sub := registeredTasks(t)
	ingestTask(t, sub, cpinformer.EventSnapshot, `[
		{"id":"task-1","function_name":"node_restart","status":"running",
		 "function_result":"","canceled":false,"retry":0},
		{"id":"task-2","function_name":"lvol_migration","status":"running",
		 "function_result":"","canceled":false,"retry":0}
	]`)

	ingestTask(t, sub, cpinformer.EventSnapshot, `[
		{"id":"task-1","function_name":"node_restart","status":"running",
		 "function_result":"","canceled":false,"retry":0}
	]`)

	if _, ok := sub.Lookup("task-2"); ok {
		t.Error("a task absent from the reconnect snapshot is still cached")
	}
	if got := sub.List(taskScope()); len(got) != 1 {
		t.Errorf("the cache holds %d tasks after the reconnect, want 1", len(got))
	}
}
