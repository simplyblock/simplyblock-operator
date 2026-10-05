// Tests for the rolling restart: the walk, the peer gate, and the two ways a
// node can leave the fleet mid-walk.
//
// The peer gate is the safety property of the whole action. Taking a node down
// while another is already offline can exceed the cluster's fault tolerance and
// lose data, so the test that matters most here is the one asserting that a
// degraded cluster receives no shutdown at all.

package cluster

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	nodeA = "1a4f7c22-0e6b-4a19-9c33-b8d2e5f10477"
	nodeB = "7b90d3e4-52a1-4f0c-8d76-3ca9017be255"
)

// rollingFleet is a control plane whose nodes move through the states a
// restart puts them in, driven by the calls the walk makes. It is a state
// machine of its own rather than a fixed answer, because every predicate the
// walk evaluates is over what the control plane reports now.
type rollingFleet struct {
	status map[string]string
}

func newRollingFleet(ids ...string) *rollingFleet {
	f := &rollingFleet{status: map[string]string{}}
	for _, id := range ids {
		f.status[id] = utils.NodeStatusOnline
	}
	return f
}

func (f *rollingFleet) nodes() []utils.NodeStatusResponse {
	// The order is the one newRollingFleet was given, so a walk's plan is
	// predictable; a map iteration would make it a coin toss.
	out := make([]utils.NodeStatusResponse, 0, len(f.status))
	for i, id := range []string{nodeA, nodeB} {
		if status, ok := f.status[id]; ok {
			out = append(out, utils.NodeStatusResponse{
				UUID: id, Status: status, IP: ipFor(i),
			})
		}
	}
	return out
}

func ipFor(i int) string { return []string{"10.0.0.1", "10.0.0.2"}[i] }

// rollingAPI wires a fleet into the scripted control plane: a shutdown takes a
// node offline and a restart brings it back, which is what lets the walk reach
// its own next step.
func rollingAPI(fleet *rollingFleet) *fakeControlPlane {
	return &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
		storageNodes: func(string) ([]utils.NodeStatusResponse, error) {
			return fleet.nodes(), nil
		},
		shutdownNode: func(_, id string) error {
			fleet.status[id] = utils.NodeStatusOffline
			return nil
		},
		restartNode: func(_, id string) error {
			fleet.status[id] = utils.NodeStatusOnline
			return nil
		},
	}
}

// The walk covers every node the cluster had when it started, in order, and
// ends when the index reaches the end of the list.
func TestARollingRestartWalksEveryNodeAndFinishes(t *testing.T) {
	fleet := newRollingFleet(nodeA, nodeB)
	api := rollingAPI(fleet)
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec,
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	// Two nodes, four steps each without the pod refresh, plus the lock, the
	// initial step, and the plan. Twenty passes is comfortably more than the
	// walk needs and the assertion is on where it ended, not on how long it
	// took.
	ops, cluster := reconcileOps(t, r, 20)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (step %q, message %q)",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	if diff := cmp.Diff([]string{nodeA, nodeB}, ops.Status.RollingRestart.Nodes); diff != "" {
		t.Errorf("the walk covered the wrong nodes (-want +got):\n%s", diff)
	}
	if got := ops.Status.RollingRestart.NodeIndex; got != 2 {
		t.Errorf("nodeIndex = %d, want it to reach the end of the list", got)
	}
	if api.shutdownNodeCalls != 2 || api.restartNodeCalls != 2 {
		t.Errorf("shutdown=%d restart=%d, want one of each per node",
			api.shutdownNodeCalls, api.restartNodeCalls)
	}
	if rec.count(NodeRestarted) != 2 {
		t.Errorf("NodeRestarted was emitted %d times, want once per node",
			rec.count(NodeRestarted))
	}
	if cluster.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock released", cluster.Status.ActiveOpsRef)
	}
}

// The safety property. A peer that is not online holds the walk before the
// node it was about to take down, and nothing is sent to that node: the peer
// check performs no side effect, and proceeding could exceed the cluster's
// fault tolerance.
func TestARollingRestartHoldsWhileAPeerIsOffline(t *testing.T) {
	fleet := newRollingFleet(nodeA, nodeB)
	fleet.status[nodeB] = utils.NodeStatusOffline
	api := rollingAPI(fleet)
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec,
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	ops, _ := reconcileOps(t, r, 6)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseRunning {
		t.Fatalf("phase = %q, want the walk still Running and holding", ops.Status.Phase)
	}
	if got := ops.Status.Step.State; got != string(stepCheckingPeers) {
		t.Errorf("step = %q, want the walk holding at CheckingPeers", got)
	}
	if api.shutdownNodeCalls != 0 {
		t.Errorf("the walk shut a node down %d times while a peer was offline, want 0",
			api.shutdownNodeCalls)
	}
	if !rec.has(PeerNodeNotOnline) {
		t.Error("a walk holding for a peer emitted no PeerNodeNotOnline, which makes it " +
			"indistinguishable from a stalled controller")
	}
}

// The hold resolves on its own once the peer returns, without anybody touching
// the operation: the step's completion condition is a predicate over current
// state rather than an observation of a transition.
func TestARollingRestartResumesWhenThePeerComesBack(t *testing.T) {
	fleet := newRollingFleet(nodeA, nodeB)
	fleet.status[nodeB] = utils.NodeStatusOffline
	api := rollingAPI(fleet)
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	reconcileOps(t, r, 5)
	fleet.status[nodeB] = utils.NodeStatusOnline

	ops, _ := reconcileOps(t, r, 20)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want the walk to finish once the peer returned (step %q)",
			ops.Status.Phase, ops.Status.Step.State)
	}
}

// The node list is written once. A node added mid-walk is not restarted,
// because a rolling restart is over the fleet it was started against.
func TestANodeAddedMidWalkIsNotRestarted(t *testing.T) {
	fleet := newRollingFleet(nodeA)
	api := rollingAPI(fleet)
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	// Far enough in for the walk to have been planned.
	reconcileOps(t, r, 3)
	fleet.status[nodeB] = utils.NodeStatusOnline

	ops, _ := reconcileOps(t, r, 20)
	if diff := cmp.Diff([]string{nodeA}, ops.Status.RollingRestart.Nodes); diff != "" {
		t.Errorf("the walk re-listed the fleet (-want +got):\n%s", diff)
	}
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", ops.Status.Phase)
	}
}

// A node removed mid-walk is skipped rather than waited on. The control plane
// stops listing it, and nothing it could report would ever satisfy the step.
func TestANodeRemovedMidWalkIsSkipped(t *testing.T) {
	fleet := newRollingFleet(nodeA, nodeB)
	api := rollingAPI(fleet)
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	reconcileOps(t, r, 3)
	delete(fleet.status, nodeB)

	ops, _ := reconcileOps(t, r, 20)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (step %q, message %q)",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	if api.shutdownNodeCalls != 1 {
		t.Errorf("the walk shut down %d nodes, want only the one still in the fleet",
			api.shutdownNodeCalls)
	}
}

// A node already offline receives no second shutdown. Nothing records that the
// call fired, so re-entering the step reads the state and advances.
func TestAnAlreadyOfflineNodeIsNotShutDownAgain(t *testing.T) {
	fleet := newRollingFleet(nodeA)
	fleet.status[nodeA] = utils.NodeStatusOffline
	api := rollingAPI(fleet)
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	ops, _ := reconcileOps(t, r, 20)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (step %q, message %q)",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	if api.shutdownNodeCalls != 0 {
		t.Errorf("an already-offline node was shut down %d times, want 0",
			api.shutdownNodeCalls)
	}
}

// A cluster with no nodes is a walk with nothing to do, and that is a success
// rather than a failure.
func TestARollingRestartOfAnEmptyClusterSucceeds(t *testing.T) {
	api := rollingAPI(newRollingFleet())
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	ops, _ := reconcileOps(t, r, 5)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message %q)", ops.Status.Phase, ops.Status.Message)
	}
	if api.shutdownNodeCalls != 0 {
		t.Errorf("an empty cluster had %d nodes shut down", api.shutdownNodeCalls)
	}
}

// status.message locates the walk without anybody reading the cluster object,
// which is what `kubectl describe scops` shows.
func TestTheWalkReportsItsPosition(t *testing.T) {
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Status.RollingRestart = &simplyblockv1alpha2.RollingRestartStatus{
				Nodes:     []string{nodeA, nodeB},
				NodeIndex: 1,
			}
		})

	got := walkMessage(ops, string(stepRestartingNode))
	want := "Node 2/2 (" + nodeB + "): RestartingNode"
	if got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// The walk reads the storage-node stream's cache rather than asking the control
// plane once per step per node (design §4.4). A twenty-node fleet is eighty
// reads that are all answers the informer is already holding, and the cluster
// whose scope opens that stream is the one the operation is acting on.
func TestTheWalkReadsTheNodeStreamRatherThanTheControlPlane(t *testing.T) {
	fleet := newRollingFleet(nodeA)
	api := rollingAPI(fleet)
	// Asking the control plane for the node list now fails the test, which is
	// how "read the cache" is asserted rather than assumed.
	api.storageNodes = func(string) ([]utils.NodeStatusResponse, error) {
		t.Error("the walk asked the control plane for a node list the stream already holds")
		return fleet.nodes(), nil
	}

	cache := &fakeNodeCache{synced: true, nodes: map[string][]subscriptions.NodeDTO{
		testClusterUUID: {{ID: nodeA, Status: utils.NodeStatusOnline, ManagementIP: "10.0.0.1"}},
	}}
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))
	r.Nodes = cache

	// The cache is the fleet: a shutdown that the scripted control plane
	// applies has to reach the cached view too, or the walk never advances.
	api.shutdownNode = func(_, id string) error {
		cache.nodes[testClusterUUID] = []subscriptions.NodeDTO{
			{ID: id, Status: utils.NodeStatusOffline, ManagementIP: "10.0.0.1"},
		}
		return nil
	}
	api.restartNode = func(_, id string) error {
		cache.nodes[testClusterUUID] = []subscriptions.NodeDTO{
			{ID: id, Status: utils.NodeStatusOnline, ManagementIP: "10.0.0.1"},
		}
		return nil
	}

	ops, _ := reconcileOps(t, r, 20)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (step %q, message %q)",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	if api.shutdownNodeCalls != 1 || api.restartNodeCalls != 1 {
		t.Errorf("shutdown=%d restart=%d, want one of each",
			api.shutdownNodeCalls, api.restartNodeCalls)
	}
}

// An unsynced cache is not read. An empty unsynced cache and a cluster with no
// nodes look identical, and reading the first as the second would plan a walk
// over nothing and report it a success.
func TestAnUnsyncedNodeCacheFallsBackToTheControlPlane(t *testing.T) {
	fleet := newRollingFleet(nodeA)
	api := rollingAPI(fleet)
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(), newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))
	r.Nodes = &fakeNodeCache{synced: false}

	ops, _ := reconcileOps(t, r, 20)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (step %q, message %q)",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	if diff := cmp.Diff([]string{nodeA}, ops.Status.RollingRestart.Nodes); diff != "" {
		t.Errorf("the walk planned the wrong fleet (-want +got):\n%s", diff)
	}
	if api.shutdownNodeCalls != 1 {
		t.Errorf("the node was shut down %d times, want 1", api.shutdownNodeCalls)
	}
}

// abortedAtShutdown is a one-node rolling restart at ShuttingDownNode, a step
// with no abort edge, with spec.abort already set.
func abortedAtShutdown(deadline *metav1.Time) (*simplyblockv1alpha2.StorageClusterOps, *simplyblockv1alpha2.StorageCluster) {
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Spec.Abort = true
			o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
			o.Status.Step = statemachine.KubeSnapshot{
				State: string(stepShuttingDownNode), Deadline: deadline,
			}
			o.Status.RollingRestart = &simplyblockv1alpha2.RollingRestartStatus{Nodes: []string{nodeA}}
		})
	cluster := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.ActiveOpsRef = testOpsName
	})
	return ops, cluster
}

// Regression: 2026-10-05-abort-refused-freezes-walk — an abort refused at a step
// with no abort edge returned before the step ran, so with spec.abort still set
// the walk never advanced and the node it had shut down stayed offline.
func TestARefusedAbortLetsTheWalkBringTheNodeBack(t *testing.T) {
	fleet := newRollingFleet(nodeA)
	fleet.status[nodeA] = utils.NodeStatusOffline
	api := rollingAPI(fleet)
	rec := &recorder{}
	ops, cluster := abortedAtShutdown(nil)
	r := newOpsReconciler(t, api, rec, cluster, ops)

	got, _ := reconcileOps(t, r, 20)
	// Refused while the node is down, honored at Rebalancing once it is back.
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseAborted ||
		got.Status.Step.State != string(stepRebalancing) {
		t.Fatalf("phase = %q at step %q, want Aborted at Rebalancing (message %q)",
			got.Status.Phase, got.Status.Step.State, got.Status.Message)
	}
	if api.restartNodeCalls != 1 || !rec.has(AbortRefused) {
		t.Errorf("restarts = %d, AbortRefused emitted = %t, want 1 and true",
			api.restartNodeCalls, rec.has(AbortRefused))
	}
}

// Regression: 2026-10-05-abort-refused-freezes-walk — the same early return
// skipped the step deadline, so a step held by a refused abort never timed out.
func TestARefusedAbortDoesNotSuspendTheStepDeadline(t *testing.T) {
	fleet := newRollingFleet(nodeA)
	fleet.status[nodeA] = utils.NodeStatusInShutdown
	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	ops, cluster := abortedAtShutdown(&expired)
	r := newOpsReconciler(t, rollingAPI(fleet), &recorder{}, cluster, ops)

	got, _ := reconcileOps(t, r, 3)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed: the step is past its deadline", got.Status.Phase)
	}
}
