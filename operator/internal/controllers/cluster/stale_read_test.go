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
	"github.com/simplyblock/simplyblock-operator/internal/utils"
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
	cache.Reads = 0

	ops, _ := reconcileOps(t, r, 0)
	if got := ops.Status.Step.State; got != string(stepAwaiting) {
		t.Errorf("step = %q, want Awaiting", got)
	}
	if api.activateCalls != 1 {
		t.Errorf("the control plane was asked to activate %d times, want 1", api.activateCalls)
	}
}
