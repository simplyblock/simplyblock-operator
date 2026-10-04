// Tests for a pass that reads the operation from a cache one write behind. The
// manager's client serves reads from an informer, and a pass that runs straight
// after another one can read the step as it stood before that pass advanced it.
// Each single-call action must then make its call once, not once per pass.
//
// Regression: lblk_outage_matrix_k8s-20261002-111746, where an Activate was
// accepted by the control plane twice, 32ms apart, under two reconcile IDs. The
// control plane ran both activations at once, and their secondary-node
// assignments overwrote each other's node records.

package cluster

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// lagged is a reconciler for the operation behind a client that can answer from
// a copy one write behind, and the operation as it stands before any pass.
func lagged(
	t *testing.T, api *fakeControlPlane, ops *simplyblockv1alpha2.StorageClusterOps,
) (*StorageClusterOpsReconciler, *testsupport.LaggingClient, *simplyblockv1alpha2.StorageClusterOps) {
	t.Helper()
	r := activationRig(t, api, &recorder{}, ops)
	cache := &testsupport.LaggingClient{Client: r.Client}
	r.Client = cache

	var before simplyblockv1alpha2.StorageClusterOps
	key := types.NamespacedName{Namespace: testNamespace, Name: testOpsName}
	if err := r.Get(context.Background(), key, &before); err != nil {
		t.Fatalf("read the operation: %v", err)
	}
	return r, cache, &before
}

// atStep is an operation that holds the cluster's lock and is about to make the
// call of the given step.
func atStep(current step) func(*simplyblockv1alpha2.StorageClusterOps) {
	return func(o *simplyblockv1alpha2.StorageClusterOps) {
		deadline := metav1.NewTime(time.Now().Add(requestingDeadline))
		started := metav1.Now()
		o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
		o.Status.Step = statemachine.KubeSnapshot{State: string(current), Deadline: &deadline}
		o.Status.StartedAt = &started
	}
}

func TestAPassReadingTheStepBeforeTheLastWriteDoesNotCallAgain(t *testing.T) {
	activate := func(f *fakeControlPlane) int { return f.activateCalls }
	expand := func(f *fakeControlPlane) int { return f.expandCalls }
	shutdown := func(f *fakeControlPlane) int { return f.shutdownCalls }
	start := func(f *fakeControlPlane) int { return f.startCalls }

	// The cluster's reading never changes, so a step that waits on its call
	// in the same step, as both halves of a Restart do, is still waiting when
	// the second pass arrives.
	cases := []struct {
		name   string
		action simplyblockv1alpha2.StorageClusterOpsAction
		from   step
		status string
		calls  func(*fakeControlPlane) int
		after  step
	}{
		{"Activate", simplyblockv1alpha2.StorageClusterOpsActionActivate,
			stepRequesting, utils.ClusterStatusUnready, activate, stepAwaiting},
		{"Expand", simplyblockv1alpha2.StorageClusterOpsActionExpand,
			stepRequesting, utils.ClusterStatusActive, expand, stepAwaiting},
		{"Shutdown", simplyblockv1alpha2.StorageClusterOpsActionShutdown,
			stepRequesting, utils.ClusterStatusActive, shutdown, stepAwaiting},
		{"Start", simplyblockv1alpha2.StorageClusterOpsActionStart,
			stepRequesting, utils.ClusterStatusSuspended, start, stepAwaiting},
		{"Restart shutting down", simplyblockv1alpha2.StorageClusterOpsActionRestart,
			stepShuttingDown, utils.ClusterStatusActive, shutdown, stepShuttingDown},
		{"Restart starting", simplyblockv1alpha2.StorageClusterOpsActionRestart,
			stepStarting, utils.ClusterStatusSuspended, start, stepStarting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			api := clusterReadingAt(tc.status)
			r, cache, before := lagged(t, api, newTestOps(tc.action, atStep(tc.from)))
			key := types.NamespacedName{Namespace: testNamespace, Name: testOpsName}

			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("first pass: %v", err)
			}
			if got := tc.calls(api); got != 1 {
				t.Fatalf("the first pass made %d calls, want 1", got)
			}

			// The second pass reads the operation as it stood before the
			// first pass advanced it.
			cache.Lag(before, 1)
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("second pass: %v", err)
			}

			if got := tc.calls(api); got != 1 {
				t.Errorf("the control plane was called %d times, want 1: the second pass "+
					"read a step the first pass had already left", got)
			}
			ops, _ := reconcileOps(t, r, 0)
			if got := ops.Status.Step.State; got != string(tc.after) {
				t.Errorf("step = %q, want %s", got, tc.after)
			}
		})
	}
}

// The pass that won the claim writes the next step straight afterward, and its
// cache has not seen the claim either. That write must not depend on the cache
// catching up: if it gave up, the step would stay claimed until the lease ran
// out, and the call would then be made a second time.
func TestThePassThatClaimedTheStepRecordsTheNextOneBeforeItsCacheSeesTheClaim(t *testing.T) {
	ctx := context.Background()
	api := clusterReadingAt(utils.ClusterStatusUnready)
	r, cache, before := lagged(t, api,
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate, atStep(stepRequesting)))
	cache.Lag(before, 20)

	key := types.NamespacedName{Namespace: testNamespace, Name: testOpsName}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("the pass that made the call failed to record the next step: %v", err)
	}
	cache.CatchUp()

	ops, _ := reconcileOps(t, r, 0)
	if got := ops.Status.Step.State; got != string(stepAwaiting) {
		t.Errorf("step = %q, want Awaiting", got)
	}
	if api.activateCalls != 1 {
		t.Errorf("the control plane was asked to activate %d times, want 1", api.activateCalls)
	}
}

// The same read against the calls a pass makes and then waits on in the same
// step: a cancel the control plane has not finished, and a rolling restart's
// node that has not moved yet. Each is driven fresh until its call is made, and
// the pass after that reads the operation from before it.
func TestAPassReadingTheStepBeforeAWaitedOnCallDoesNotCallAgain(t *testing.T) {
	cancelTask := func(o *simplyblockv1alpha2.StorageClusterOps) {
		o.Spec.CancelTask = &simplyblockv1alpha2.CancelTaskSpec{TaskID: "task-1"}
	}
	stillRunning := func(string) ([]subscriptions.TaskDTO, error) {
		return []subscriptions.TaskDTO{{ID: "task-1", Type: "balancing_on_restart", Status: "running"}}, nil
	}
	// A fleet whose shutdown lands and whose restart has not been published.
	stuckRestart := func() *fakeControlPlane {
		fleet := newRollingFleet(nodeA)
		api := rollingAPI(fleet)
		api.restartNode = func(string, string) error { return nil }
		return api
	}
	// A fleet whose shutdown has not been published.
	stuckShutdown := func() *fakeControlPlane {
		api := rollingAPI(newRollingFleet(nodeA))
		api.shutdownNode = func(string, string) error { return nil }
		return api
	}

	cases := []struct {
		name   string
		action simplyblockv1alpha2.StorageClusterOpsAction
		mutate func(*simplyblockv1alpha2.StorageClusterOps)
		api    func() *fakeControlPlane
		calls  func(*fakeControlPlane) int
	}{
		{"CancelTask", simplyblockv1alpha2.StorageClusterOpsActionCancelTask, cancelTask,
			func() *fakeControlPlane {
				return &fakeControlPlane{
					cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
					tasks:   stillRunning,
				}
			},
			func(f *fakeControlPlane) int { return f.cancelTaskCalls }},
		{"RollingRestart shutting a node down", simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
			nil, stuckShutdown, func(f *fakeControlPlane) int { return f.shutdownNodeCalls }},
		{"RollingRestart restarting a node", simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
			nil, stuckRestart, func(f *fakeControlPlane) int { return f.restartNodeCalls }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			api := tc.api()
			var mutate []func(*simplyblockv1alpha2.StorageClusterOps)
			if tc.mutate != nil {
				mutate = append(mutate, tc.mutate)
			}
			r := newOpsReconciler(t, api, &recorder{}, newTestCluster(), newTestOps(tc.action, mutate...))
			cache := &testsupport.LaggingClient{Client: r.Client}
			r.Client = cache

			key := types.NamespacedName{Namespace: testNamespace, Name: testOpsName}
			var before simplyblockv1alpha2.StorageClusterOps
			for range 12 {
				if err := r.Get(ctx, key, &before); err != nil {
					t.Fatalf("read the operation: %v", err)
				}
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
					t.Fatalf("pass: %v", err)
				}
				if tc.calls(api) > 0 {
					break
				}
			}
			if got := tc.calls(api); got != 1 {
				t.Fatalf("the call was made %d times on the way to it, want 1", got)
			}

			cache.Lag(&before, 1)
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("second pass: %v", err)
			}
			cache.CatchUp()

			if got := tc.calls(api); got != 1 {
				t.Errorf("the call was made %d times, want 1: the second pass read the step "+
					"from before the first one made it", got)
			}
		})
	}
}
