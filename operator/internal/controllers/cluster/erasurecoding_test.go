// The activation gate for a cluster whose fleet is too small for its stripe.
//
// It is the backstop the deployment config's checks cannot be: a cluster and its
// nodes can be written by hand, by a chart, or by a document from before the
// checks existed, and an activation is the moment the layout stops being a
// proposal. The control plane refuses none of it — its own gate counts devices
// rather than nodes — so a four-node cluster configured 4+2 activates, and what
// it bought is a cluster that loses data on the second failure it was configured
// to survive.

package cluster

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// statusSuspended is the control plane's own spelling of a cluster that is not
// serving, which is the reading every case here is written against.
const statusSuspended = "suspended"

// suspendedCluster is a control-plane reading of a cluster that is not active,
// which is the only state an activation is asked for from.
func suspendedCluster() webapi.ClusterResponse {
	reading := activeCluster()
	reading.Status = statusSuspended
	return reading
}

// nodeOfTestCluster is one storage node of the cluster under test.
func nodeOfTestCluster(name, worker string) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: objectMeta(name),
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: testClusterName,
			WorkerNode: worker,
			Slot:       ptr.To(int32(0)),
		},
	}
}

// withStripe states a cluster's erasure coding.
func withStripe(data, parity int32) func(*simplyblockv1alpha2.StorageCluster) {
	return func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(data), ParityChunks: ptr.To(parity),
		}
		c.Status.Status = statusSuspended
		c.Status.Phase = simplyblockv1alpha2.StorageClusterPhasePending
	}
}

// A 1+1 cluster needs three storage nodes: two to place a stripe across and one
// spare to rebuild onto. Two nodes is a cluster that survives no failure it was
// configured to survive, so the activation waits rather than firing.
func TestAnActivationBelowTheStripesMinimumIsHeld(t *testing.T) {
	api := &fakeControlPlane{cluster: func(string) (webapi.ClusterResponse, error) {
		return suspendedCluster(), nil
	}}
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec,
		newTestCluster(withStripe(1, 1)),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate),
		nodeOfTestCluster("node-1", "worker-1"),
		nodeOfTestCluster("node-2", "worker-2"))

	ops, _ := reconcileOps(t, r, 6)
	if api.activateCalls != 0 {
		t.Errorf("the control plane was asked to activate %d times, want 0 below the minimum",
			api.activateCalls)
	}
	if ops.Status.Phase == simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Errorf("the operation reported success without activating anything")
	}
	if !rec.has(StripeNodesNotReady) {
		t.Errorf("nothing said why the activation is waiting; events: %+v", rec.events)
	}
}

// The same cluster with the third node is activated, because the hold is on the
// node count and not on the stripe.
func TestAnActivationWithTheNodesTheStripeNeedsIsRequested(t *testing.T) {
	api := &fakeControlPlane{cluster: func(string) (webapi.ClusterResponse, error) {
		return suspendedCluster(), nil
	}}
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(withStripe(1, 1)),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate),
		nodeOfTestCluster("node-1", "worker-1"),
		nodeOfTestCluster("node-2", "worker-2"),
		nodeOfTestCluster("node-3", "worker-3"))

	reconcileOps(t, r, 6)
	if api.activateCalls != 1 {
		t.Errorf("the control plane was asked to activate %d times, want 1", api.activateCalls)
	}
}

// A cluster that is already active is not held, whatever its node count says.
// The gate exists to stop a layout being brought up wrong, and a live cluster is
// one whose layout is already the fact; holding a re-activation on it would wedge
// a cluster that is serving.
func TestAReactivationOfALiveClusterIsNotHeld(t *testing.T) {
	api := &fakeControlPlane{cluster: func(string) (webapi.ClusterResponse, error) {
		return activeCluster(), nil
	}}
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
			c.Spec.Stripe = &simplyblockv1alpha2.StripeSpec{
				DataChunks: ptr.To(int32(4)), ParityChunks: ptr.To(int32(2)),
			}
		}),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate))

	ops, _ := reconcileOps(t, r, 6)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded (message: %s)", ops.Status.Phase, ops.Status.Message)
	}
}

// The rule itself, stated against the cases the callers cannot reach: a cluster
// with no nodes reported yet, and the 1+0 that needs one.
func TestTheActivationNodeCountRule(t *testing.T) {
	for _, testCase := range []struct {
		data, parity int32
		nodes        int
		held         bool
	}{
		{1, 0, 1, false},
		{1, 0, 0, true},
		{1, 1, 2, true},
		{1, 1, 3, false},
		{2, 1, 4, false},
		{4, 2, 7, true},
		{4, 2, 8, false},
	} {
		scheme := simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(testCase.data), ParityChunks: ptr.To(testCase.parity),
		}
		reason := ActivationNodeCountViolation(&scheme, testCase.nodes)
		if held := reason != ""; held != testCase.held {
			t.Errorf("%d+%d with %d nodes: held = %v, want %v (%s)",
				testCase.data, testCase.parity, testCase.nodes, held, testCase.held, reason)
		}
	}
}
