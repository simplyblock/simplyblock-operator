// Tests for the StorageClusterOps reconciler: the lock, the step machine, the
// write-ahead discipline, and the terminal paths.
//
// The lock is what stops two operations acting on one cluster, and every path
// that leaves it taken is a cluster nothing can ever operate on again with
// nothing in the object to say why. Three paths release it, and each is tested
// on its own rather than through the one that happens to run first.
//
// The write-ahead property is tested by counting calls. A step is persisted
// before the side effect it performs, so a crash between the two restarts into
// a state saying the call may already have landed; what makes that safe is that
// every call is skipped when its target is already where the call would put it.
// The only way to see the difference between "skipped" and "made twice" is the
// counter on the scripted control plane.

package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

func newOpsReconciler(
	t *testing.T, api *fakeControlPlane, rec *recorder, objects ...client.Object,
) *StorageClusterOpsReconciler {
	t.Helper()
	if api != nil {
		api.t = t
	}
	return &StorageClusterOpsReconciler{
		Client:   newClient(t, objects...),
		Scheme:   testScheme(t),
		Recorder: rec,
		API:      api,
	}
}

// reconcileOps runs n passes and returns the operation and its target as they
// stand afterward. One reconcile advances at most one step, so a test that
// wants a finished operation says how many passes that took.
func reconcileOps(
	t *testing.T, r *StorageClusterOpsReconciler, n int,
) (*simplyblockv1alpha2.StorageClusterOps, *simplyblockv1alpha2.StorageCluster) {
	t.Helper()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testOpsName}
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile pass %d: %v", i+1, err)
		}
	}
	var ops simplyblockv1alpha2.StorageClusterOps
	if err := r.Get(ctx, key, &ops); err != nil {
		t.Fatalf("read the operation back: %v", err)
	}
	var cluster simplyblockv1alpha2.StorageCluster
	clusterKey := types.NamespacedName{Namespace: testNamespace, Name: testClusterName}
	if err := r.Get(ctx, clusterKey, &cluster); err != nil {
		return &ops, nil
	}
	return &ops, &cluster
}

// ---------------------------------------------------------------------------
// The lock
// ---------------------------------------------------------------------------

func TestAnOperationTakesTheClusterLockAndReleasesItWhenItFinishes(t *testing.T) {
	api := &fakeControlPlane{cluster: func(string) (webapi.ClusterResponse, error) {
		return activeCluster(), nil
	}}
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec,
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate))

	// One pass to take the lock, one to set the first step's deadline, one to
	// request, and one to observe the completion condition.
	_, cluster := reconcileOps(t, r, 2)
	if cluster.Status.ActiveOpsRef != testOpsName {
		t.Fatalf("activeOpsRef = %q, want the operation to hold the lock", cluster.Status.ActiveOpsRef)
	}

	ops, cluster := reconcileOps(t, r, 4)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message: %s)", ops.Status.Phase, ops.Status.Message)
	}
	if cluster.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock released", cluster.Status.ActiveOpsRef)
	}
	if !rec.has(OperationSucceeded) {
		t.Error("a finished operation emitted no OperationSucceeded")
	}
}

// A second operation is admitted, waits at Pending, and says so once. Pending
// is both where an operation starts and where it waits, and the event is the
// only thing that separates the two.
func TestAnOperationWaitsBehindAnotherOnesLock(t *testing.T) {
	held := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.ActiveOpsRef = otherOpsName
	})
	rec := &recorder{}
	r := newOpsReconciler(t, &fakeControlPlane{}, rec,
		held, newTestOps(simplyblockv1alpha2.StorageClusterOpsActionShutdown))

	ops, cluster := reconcileOps(t, r, 2)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhasePending {
		t.Errorf("phase = %q, want Pending while another operation holds the lock", ops.Status.Phase)
	}
	if cluster.Status.ActiveOpsRef != otherOpsName {
		t.Errorf("activeOpsRef = %q, want the other operation to keep it", cluster.Status.ActiveOpsRef)
	}
	if !rec.has(OperationQueued) {
		t.Error("a queued operation emitted no OperationQueued")
	}
}

// The release is idempotent and checks ownership. A pass that started before
// the lock changed hands must not clear a lock somebody else now holds, which
// is worse than not releasing at all: two operations would then be running
// against one cluster with neither of them knowing.
func TestATerminalOperationDoesNotReleaseSomebodyElsesLock(t *testing.T) {
	held := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.ActiveOpsRef = otherOpsName
	})
	finished := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionShutdown,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded
		})
	r := newOpsReconciler(t, &fakeControlPlane{}, &recorder{}, held, finished)

	_, cluster := reconcileOps(t, r, 2)
	if cluster.Status.ActiveOpsRef != otherOpsName {
		t.Errorf("activeOpsRef = %q, want the other operation's lock left alone",
			cluster.Status.ActiveOpsRef)
	}
}

// The terminal branch releases the lock a second time on purpose: a crash
// between persisting the phase and clearing activeOpsRef would otherwise leave
// the cluster locked by a finished operation forever.
func TestATerminalOperationReleasesALockItStillHolds(t *testing.T) {
	held := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.ActiveOpsRef = testOpsName
	})
	finished := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionShutdown,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded
		})
	r := newOpsReconciler(t, &fakeControlPlane{}, &recorder{}, held, finished)

	_, cluster := reconcileOps(t, r, 1)
	if cluster.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want a lock held by a finished operation cleared",
			cluster.Status.ActiveOpsRef)
	}
}

// The finalizer is the release path that matters most: `kubectl delete` on a
// running operation would otherwise leave the cluster locked by an object that
// no longer exists.
func TestDeletingARunningOperationReleasesTheLock(t *testing.T) {
	now := metav1.Now()
	held := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.ActiveOpsRef = testOpsName
	})
	deleting := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.DeletionTimestamp = &now
			o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
		})
	r := newOpsReconciler(t, &fakeControlPlane{}, &recorder{}, held, deleting)

	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testOpsName}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var cluster simplyblockv1alpha2.StorageCluster
	clusterKey := types.NamespacedName{Namespace: testNamespace, Name: testClusterName}
	if err := r.Get(ctx, clusterKey, &cluster); err != nil {
		t.Fatalf("read the cluster back: %v", err)
	}
	if cluster.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want a deleted operation's lock released",
			cluster.Status.ActiveOpsRef)
	}
}

// An operation against a cluster that does not exist has nothing to lock and
// nothing to do.
func TestAnOperationAgainstNoClusterFails(t *testing.T) {
	r := newOpsReconciler(t, &fakeControlPlane{}, &recorder{},
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate))

	ops, _ := reconcileOps(t, r, 1)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed", ops.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// The write-ahead discipline
// ---------------------------------------------------------------------------

// A step re-entered after a crash must not repeat its side effect where the
// cluster is already where the call would put it. Nothing records that the call
// fired, so the only thing that makes the re-entry safe is reading the state
// first.
func TestAnActivateAgainstAnActiveClusterIssuesNoCall(t *testing.T) {
	api := &fakeControlPlane{cluster: func(string) (webapi.ClusterResponse, error) {
		return activeCluster(), nil
	}}
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate))

	ops, _ := reconcileOps(t, r, 6)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message: %s)", ops.Status.Phase, ops.Status.Message)
	}
	if api.activateCalls != 0 {
		t.Errorf("the control plane was asked to activate %d times, want 0 on an active cluster",
			api.activateCalls)
	}
}

// The opposite half: a cluster that is not where the call would put it gets the
// call, and gets it once.
func TestAShutdownIssuesOneCallAndWaitsForTheCluster(t *testing.T) {
	active := true
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) {
			reading := activeCluster()
			if !active {
				reading.Status = "suspended"
			}
			return reading, nil
		},
		shutdown: func(string) error { active = false; return nil },
	}
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionShutdown))

	ops, _ := reconcileOps(t, r, 6)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message: %s)", ops.Status.Phase, ops.Status.Message)
	}
	if api.shutdownCalls != 1 {
		t.Errorf("the control plane was asked to shut down %d times, want 1", api.shutdownCalls)
	}
}

// Restart is the one action with two side effects, because the control plane
// has no restart endpoint of its own.
func TestARestartShutsDownThenStarts(t *testing.T) {
	status := utils.ClusterStatusActive
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) {
			reading := activeCluster()
			reading.Status = status
			return reading, nil
		},
		shutdown: func(string) error { status = "suspended"; return nil },
		start:    func(string) error { status = utils.ClusterStatusActive; return nil },
	}
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRestart))

	ops, _ := reconcileOps(t, r, 8)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message: %s)", ops.Status.Phase, ops.Status.Message)
	}
	if api.shutdownCalls != 1 || api.startCalls != 1 {
		t.Errorf("shutdown=%d start=%d, want one of each", api.shutdownCalls, api.startCalls)
	}
}

// ---------------------------------------------------------------------------
// Steps, deadlines, and aborts
// ---------------------------------------------------------------------------

// An unrecognized step is a downgrade, a hand-edited object, or a rename that
// shipped without a conversion, and none of them resolve by reconciling again.
func TestAnUnknownStepFailsTheOperationRatherThanRequeuing(t *testing.T) {
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
			o.Status.Step = statemachine.KubeSnapshot{State: "Teleporting"}
		})
	r := newOpsReconciler(t, &fakeControlPlane{}, &recorder{}, newTestCluster(), ops)

	got, cluster := reconcileOps(t, r, 1)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	if cluster.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock released on the failure",
			cluster.Status.ActiveOpsRef)
	}
}

// A step whose deadline passed while the operator was down restores as already
// expired, because the instant is absolute and travels in the status.
func TestAStepThatOutlivedItsDeadlineFailsTheOperation(t *testing.T) {
	expired := metav1.NewTime(time.Now().Add(-time.Hour))
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
			o.Status.Step = statemachine.KubeSnapshot{
				State:    string(stepAwaiting),
				Deadline: &expired,
			}
		})
	rec := &recorder{}
	r := newOpsReconciler(t, &fakeControlPlane{}, rec, newTestCluster(), ops)

	got, _ := reconcileOps(t, r, 1)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	if !rec.has(StepDeadlineExceeded) {
		t.Error("an expired step emitted no StepDeadlineExceeded")
	}
}

// An abort from a step the graph allows it from ends the operation as Aborted,
// which is terminal and distinct from Failed: a called-off operation did not go
// wrong.
func TestAnAbortFromAnAbortableStepEndsTheOperation(t *testing.T) {
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Spec.Abort = true
			o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
			o.Status.Step = statemachine.KubeSnapshot{State: string(stepRequesting)}
		})
	rec := &recorder{}
	r := newOpsReconciler(t, &fakeControlPlane{}, rec, newTestCluster(), ops)

	got, cluster := reconcileOps(t, r, 1)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseAborted {
		t.Errorf("phase = %q, want Aborted", got.Status.Phase)
	}
	if !rec.has(OperationAborted) {
		t.Error("an aborted operation emitted no OperationAborted")
	}
	if cluster.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock released", cluster.Status.ActiveOpsRef)
	}
}

// An abort that arrives at a step the control plane is part-way through is
// refused and said so, and the operation runs on. Stopping there would leave
// nothing driving the cluster back to a state somebody can reason about.
func TestAnAbortThatArrivesTooLateIsRefusedAndTheOperationRunsOn(t *testing.T) {
	api := &fakeControlPlane{cluster: func(string) (webapi.ClusterResponse, error) {
		return activeCluster(), nil
	}}
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionShutdown,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Spec.Abort = true
			o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
			o.Status.Step = statemachine.KubeSnapshot{State: string(stepAwaiting)}
		})
	r := newOpsReconciler(t, api, &recorder{}, newTestCluster(), ops)

	got, _ := reconcileOps(t, r, 1)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseRunning {
		t.Errorf("phase = %q, want the operation still Running", got.Status.Phase)
	}
	if got.Status.Message == "" {
		t.Error("a refused abort said nothing about why")
	}
}

// ---------------------------------------------------------------------------
// CancelTask
// ---------------------------------------------------------------------------

// A cancel the control plane accepted is one it has started, and it finishes
// the stopping in its own time. What is waited on is the task leaving the
// running set.
func TestCancelTaskWaitsForTheTaskToLeaveTheList(t *testing.T) {
	running := true
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
		tasks: func(string) ([]subscriptions.TaskDTO, error) {
			if !running {
				return nil, nil
			}
			return []subscriptions.TaskDTO{
				{ID: "task-1", Type: "balancing_on_restart", Status: "running"},
			}, nil
		},
		cancelTask: func(string, string) error { running = false; return nil },
	}
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionCancelTask,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Spec.CancelTask = &simplyblockv1alpha2.CancelTaskSpec{TaskID: "task-1"}
		})
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec, newTestCluster(), ops)

	got, _ := reconcileOps(t, r, 6)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message: %s)", got.Status.Phase, got.Status.Message)
	}
	if api.cancelTaskCalls != 1 {
		t.Errorf("the control plane was asked to cancel %d times, want 1", api.cancelTaskCalls)
	}
	if !rec.has(TaskCanceled) {
		t.Error("a canceled task emitted no TaskCanceled")
	}
}

// Canceling a task that is already gone succeeds. The task finished on its own
// between the operation being written and its Requesting step, which is the
// outcome the operation asked for reached by another route.
func TestCancelingATaskThatIsAlreadyGoneSucceedsWithoutCalling(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
		tasks:   func(string) ([]subscriptions.TaskDTO, error) { return nil, nil },
	}
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionCancelTask,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Spec.CancelTask = &simplyblockv1alpha2.CancelTaskSpec{TaskID: "task-1"}
		})
	r := newOpsReconciler(t, api, &recorder{}, newTestCluster(), ops)

	got, _ := reconcileOps(t, r, 5)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message: %s)", got.Status.Phase, got.Status.Message)
	}
	if api.cancelTaskCalls != 0 {
		t.Errorf("a task that was already gone was canceled %d times, want 0", api.cancelTaskCalls)
	}
}

// A CancelTask naming no task cannot be performed and cannot be fixed by
// retrying, so it is terminal rather than requeued.
func TestCancelTaskWithNoTaskIDFails(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	r := newOpsReconciler(t, api, &recorder{}, newTestCluster(),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionCancelTask))

	got, _ := reconcileOps(t, r, 3)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

// A control plane that is briefly unreachable is retried rather than failed,
// which is the distinction terminalStepError draws.
func TestAnUnreachableControlPlaneRequeuesRatherThanFailing(t *testing.T) {
	api := &fakeControlPlane{cluster: func(string) (webapi.ClusterResponse, error) {
		return webapi.ClusterResponse{}, errors.New("connection refused")
	}}
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate))

	got, _ := reconcileOps(t, r, 4)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseRunning {
		t.Errorf("phase = %q, want the operation still Running", got.Status.Phase)
	}
}
