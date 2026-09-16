// That both controllers read the control-plane streams rather than asking the
// control plane the same question on every pass.
//
// Each test scripts the control plane to fail if the streamed call is made at
// all, which is how "reads the cache" is asserted rather than assumed. The
// mirror of each is the unsynced case: a cache that has not had its snapshot
// holds nothing, and reading that as an answer is how a live cluster gets
// reported gone, a shutdown gets reported complete, and every cancel gets
// reported finished the moment it is asked for.

package cluster

import (
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// streamedCluster is the cluster cache holding one active cluster.
func streamedCluster(mutate ...func(*subscriptions.ClusterDTO)) *fakeClusterCache {
	dto := subscriptions.ClusterDTO{
		ID:                testClusterUUID,
		Name:              testClusterName,
		NQN:               "nqn.2023-02.io.simplyblock:4f2c8a11",
		Status:            utils.ClusterStatusActive,
		NDCS:              2,
		NPCS:              1,
		MaxFaultTolerance: 1,
	}
	for _, m := range mutate {
		m(&dto)
	}
	return &fakeClusterCache{
		synced:   true,
		clusters: map[string]subscriptions.ClusterDTO{testClusterUUID: dto},
	}
}

// refuseClusterReads makes any read of the cluster fail the test, which is
// what turns "prefers the cache" into an assertion.
func refuseClusterReads(t *testing.T, api *fakeControlPlane) {
	t.Helper()
	api.cluster = func(string) (webapi.ClusterResponse, error) {
		t.Error("the cluster was read from the control plane, not from the stream")
		return activeCluster(), nil
	}
}

// ---------------------------------------------------------------------------
// The entity reconciler
// ---------------------------------------------------------------------------

// The steady-state pass writes status from the cluster stream. It runs on
// every reconcile of every cluster, so it is the read that a poll costs most.
func TestTheEntityReadsTheClusterStream(t *testing.T) {
	api := &fakeControlPlane{}
	refuseClusterReads(t, api)
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster())
	r.Clusters = streamedCluster(func(dto *subscriptions.ClusterDTO) {
		dto.Status = "degraded"
	})

	cluster := reconcileCluster(t, r, 1)
	if cluster.Status.Phase != simplyblockv1alpha2.StorageClusterPhaseDegraded {
		t.Errorf("phase = %q, want the streamed status to have reached it",
			cluster.Status.Phase)
	}
	if cluster.Status.ErasureCodingScheme != "2x1" {
		t.Errorf("erasureCodingScheme = %q, want 2x1 from the stream",
			cluster.Status.ErasureCodingScheme)
	}
}

// An unsynced cache is not read. A cluster missing from one and a cluster the
// control plane has forgotten look identical, and reading the first as the
// second would report a live cluster as gone.
func TestAnUnsyncedClusterCacheFallsBackToTheControlPlane(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster())
	r.Clusters = &fakeClusterCache{synced: false}

	cluster := reconcileCluster(t, r, 1)
	if cluster.Status.Phase != simplyblockv1alpha2.StorageClusterPhaseOnline {
		t.Errorf("phase = %q, want the control plane's answer", cluster.Status.Phase)
	}
}

// The task window is built from the task stream, not from a read per pass.
func TestTheTaskWindowIsBuiltFromTheTaskStream(t *testing.T) {
	api := &fakeControlPlane{}
	refuseClusterReads(t, api)
	api.tasks = func(string) ([]subscriptions.TaskDTO, error) {
		t.Error("the tasks were read from the control plane, not from the stream")
		return nil, nil
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster())
	r.Clusters = streamedCluster()
	r.Tasks = &fakeTaskCache{synced: true, tasks: map[string][]subscriptions.TaskDTO{
		testClusterUUID: {
			{ID: "task-1", Type: "balancing_on_restart", Status: "running", Retry: 2},
			{ID: "task-2", Type: "lvol_migration", Status: "done"},
		},
	}}

	cluster := reconcileCluster(t, r, 1)
	if len(cluster.Status.Tasks) != 1 {
		t.Fatalf("status.tasks holds %d entries, want the one that is running",
			len(cluster.Status.Tasks))
	}
	task := cluster.Status.Tasks[0]
	if task.ID != "task-1" || task.Type != "balancing_on_restart" || task.Retry != 2 {
		t.Errorf("the streamed task did not reach the window: %+v", task)
	}
}

// An unsynced task cache holds nothing, and nothing is not the same statement
// as no task running.
func TestAnUnsyncedTaskCacheFallsBackToTheControlPlane(t *testing.T) {
	asked := false
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
		tasks: func(string) ([]subscriptions.TaskDTO, error) {
			asked = true
			return []subscriptions.TaskDTO{
				{ID: "task-1", Type: "node_restart", Status: "running"},
			}, nil
		},
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster())
	r.Tasks = &fakeTaskCache{synced: false}

	cluster := reconcileCluster(t, r, 1)
	if !asked {
		t.Error("an unsynced task cache was read as though it were an answer")
	}
	if len(cluster.Status.Tasks) != 1 {
		t.Errorf("status.tasks holds %d entries, want the control plane's one",
			len(cluster.Status.Tasks))
	}
}

// ---------------------------------------------------------------------------
// The operation reconciler
// ---------------------------------------------------------------------------

// Every completion condition four of the seven actions share is a read of the
// cluster, asked on every pass of every step. A shutdown that takes twenty
// minutes is eighty of them.
func TestAnOperationReadsTheClusterStream(t *testing.T) {
	api := &fakeControlPlane{}
	refuseClusterReads(t, api)
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate))
	r.Clusters = streamedCluster()

	ops, _ := reconcileOps(t, r, 6)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (step %q, message %q)",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	if api.activateCalls != 0 {
		t.Errorf("a cluster the stream reports active was activated %d times",
			api.activateCalls)
	}
}

// A CancelTask waits for the task to leave the running set, which is a read of
// the task stream. The gate matters more here than anywhere else: an unsynced
// cache holds no tasks, so reading it would report the cancel complete before
// it was issued.
func TestCancelTaskReadsTheTaskStream(t *testing.T) {
	api := &fakeControlPlane{}
	refuseClusterReads(t, api)
	api.tasks = func(string) ([]subscriptions.TaskDTO, error) {
		t.Error("the tasks were read from the control plane, not from the stream")
		return nil, nil
	}

	tasks := &fakeTaskCache{synced: true, tasks: map[string][]subscriptions.TaskDTO{
		testClusterUUID: {{ID: "task-1", Type: "lvol_migration", Status: "running"}},
	}}
	api.cancelTask = func(string, string) error {
		// The control plane accepting a cancel is what makes the task stop, so
		// the stream reports it done on the next frame.
		tasks.tasks[testClusterUUID] = []subscriptions.TaskDTO{
			{ID: "task-1", Type: "lvol_migration", Status: "done", Canceled: true},
		}
		return nil
	}

	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionCancelTask,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Spec.CancelTask = &simplyblockv1alpha2.CancelTaskSpec{TaskID: "task-1"}
		})
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec, newTestCluster(), ops)
	r.Clusters = streamedCluster()
	r.Tasks = tasks

	got, _ := reconcileOps(t, r, 6)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (step %q, message %q)",
			got.Status.Phase, got.Status.Step.State, got.Status.Message)
	}
	if api.cancelTaskCalls != 1 {
		t.Errorf("the control plane was asked to cancel %d times, want 1",
			api.cancelTaskCalls)
	}
	if !rec.has(TaskCanceled) {
		t.Error("a canceled task emitted no TaskCanceled")
	}
}

// A suspended task is waiting rather than finished, so a cancel against one
// does not report itself complete before the control plane has stopped it.
func TestCancelTaskDoesNotFinishWhileTheTaskIsSuspended(t *testing.T) {
	api := &fakeControlPlane{}
	refuseClusterReads(t, api)

	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionCancelTask,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Spec.CancelTask = &simplyblockv1alpha2.CancelTaskSpec{TaskID: "task-1"}
		})
	r := newOpsReconciler(t, api, &recorder{}, newTestCluster(), ops)
	r.Clusters = streamedCluster()
	r.Tasks = &fakeTaskCache{synced: true, tasks: map[string][]subscriptions.TaskDTO{
		testClusterUUID: {{ID: "task-1", Type: "lvol_migration", Status: "suspended"}},
	}}

	got, _ := reconcileOps(t, r, 6)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseRunning {
		t.Errorf("phase = %q, want the operation still waiting on a suspended task",
			got.Status.Phase)
	}
}

// The rebalancing wait is the rolling restart's last step per node, and it is
// a read of the cluster rather than of the node.
func TestTheRebalancingWaitReadsTheClusterStream(t *testing.T) {
	api := rollingAPI(newRollingFleet(nodeA))
	api.cluster = func(string) (webapi.ClusterResponse, error) {
		t.Error("the cluster was read from the control plane, not from the stream")
		return activeCluster(), nil
	}

	clusters := streamedCluster(func(dto *subscriptions.ClusterDTO) {
		dto.Rebalancing = true
	})
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))
	r.Clusters = clusters

	// The walk reaches Rebalancing and holds there while the cluster reports
	// it is still redistributing.
	ops, _ := reconcileOps(t, r, 10)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseRunning {
		t.Fatalf("phase = %q, want the walk still holding at Rebalancing", ops.Status.Phase)
	}
	if got := ops.Status.Step.State; got != string(stepRebalancing) {
		t.Fatalf("step = %q, want Rebalancing", got)
	}

	// The stream reports the rebalance done, and the walk finishes.
	dto := clusters.clusters[testClusterUUID]
	dto.Rebalancing = false
	clusters.clusters[testClusterUUID] = dto

	ops, _ = reconcileOps(t, r, 5)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded once the rebalance finished (step %q)",
			ops.Status.Phase, ops.Status.Step.State)
	}
}
