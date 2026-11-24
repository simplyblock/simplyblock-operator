// Tests for the rolling restart of a cluster in failure-domain mode, where the
// walk takes the nodes of one failure domain down together.
//
// The property that matters is the same one the peer gate protects, drawn
// around a failure domain instead of a node: nodes are down in one failure
// domain at a time, and only in a cluster whose stripe survives losing one.

package cluster

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// domainFleet is a control plane whose nodes carry a failure domain. A restart
// leaves its node restarting until settle is called, which is what makes a
// second restart sent before the first one finished observable.
type domainFleet struct {
	order  []string
	domain map[string]int
	status map[string]string

	// mostDown is the largest number of nodes that were not online at once,
	// and domainsDown the largest number of failure domains they were in.
	mostDown    int
	domainsDown int

	// restartingTogether is the largest number of nodes that were restarting
	// at once.
	restartingTogether int

	shutdowns []string
	restarts  []string
}

// newDomainFleet builds a fleet of perDomain nodes in each of domains failure
// domains, listed by the control plane with the domains interleaved, so a walk
// that restarts a failure domain together has to group them itself.
func newDomainFleet(domains, perDomain int) *domainFleet {
	f := &domainFleet{domain: map[string]int{}, status: map[string]string{}}
	for n := range perDomain {
		for d := range domains {
			id := domainNode(d, n)
			f.order = append(f.order, id)
			f.domain[id] = d
			f.status[id] = utils.NodeStatusOnline
		}
	}
	return f
}

// domainNode names the n-th node of failure domain d.
func domainNode(d, n int) string {
	return string(rune('a'+d)) + "-" + string(rune('1'+n))
}

func (f *domainFleet) nodes() []utils.NodeStatusResponse {
	out := make([]utils.NodeStatusResponse, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, utils.NodeStatusResponse{
			UUID: id, Status: f.status[id], FailureDomain: ptr.To(f.domain[id]),
		})
	}
	return out
}

// observe records how much of the fleet is down now.
func (f *domainFleet) observe() {
	down, restarting := 0, 0
	domains := map[int]struct{}{}
	for _, id := range f.order {
		if f.status[id] == utils.NodeStatusOnline {
			continue
		}
		down++
		domains[f.domain[id]] = struct{}{}
		if f.status[id] == utils.NodeStatusInRestart {
			restarting++
		}
	}
	f.mostDown = max(f.mostDown, down)
	f.domainsDown = max(f.domainsDown, len(domains))
	f.restartingTogether = max(f.restartingTogether, restarting)
}

// settle finishes every restart in flight.
func (f *domainFleet) settle() {
	for id, status := range f.status {
		if status == utils.NodeStatusInRestart {
			f.status[id] = utils.NodeStatusOnline
		}
	}
}

func domainAPI(fleet *domainFleet) *fakeControlPlane {
	return &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		storageNodes: func(string) ([]utils.NodeStatusResponse, error) {
			return fleet.nodes(), nil
		},
		shutdownNode: func(_, id string) error {
			fleet.shutdowns = append(fleet.shutdowns, id)
			fleet.status[id] = utils.NodeStatusOffline
			fleet.observe()
			return nil
		},
		restartNode: func(_, id string) error {
			fleet.restarts = append(fleet.restarts, id)
			fleet.status[id] = utils.NodeStatusInRestart
			fleet.observe()
			return nil
		},
	}
}

// domainCluster is a cluster in failure-domain mode.
func domainCluster() *simplyblockv1alpha2.StorageCluster {
	return newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.EnableFailureDomains = ptr.To(true)
	})
}

// walkDomains reconciles until the operation is terminal, finishing the
// restarts in flight every settleEvery passes, and fails the test when the
// walk does not end.
func walkDomains(
	t *testing.T, r *StorageClusterOpsReconciler, fleet *domainFleet, settleEvery int,
) *simplyblockv1alpha2.StorageClusterOps {
	t.Helper()
	for pass := 1; pass <= 400; pass++ {
		ops, _ := reconcileOps(t, r, 1)
		if terminalOps(ops.Status.Phase) {
			return ops
		}
		if pass%settleEvery == 0 {
			fleet.settle()
		}
	}
	t.Fatal("the walk did not finish")
	return nil
}

// The stripe is two data chunks and one parity chunk, and the cluster has three
// failure domains, so each holds one chunk of a stripe and a whole one may be
// down. The walk takes each failure domain down together and restarts its
// nodes together.
func TestARollingRestartTakesAWholeFailureDomainDownTogether(t *testing.T) {
	fleet := newDomainFleet(3, 2)
	api := domainAPI(fleet)
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec, domainCluster(),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	ops := walkDomains(t, r, fleet, 3)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (step %q, message %q)",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	want := []string{"a-1", "a-2", "b-1", "b-2", "c-1", "c-2"}
	if diff := cmp.Diff(want, ops.Status.RollingRestart.Nodes); diff != "" {
		t.Errorf("the walk did not list each failure domain's nodes together (-want +got):\n%s", diff)
	}
	if fleet.mostDown != 2 {
		t.Errorf("at most %d node(s) were down at once, want the 2 of a failure domain", fleet.mostDown)
	}
	if fleet.domainsDown != 1 {
		t.Errorf("nodes were down in %d failure domains at once, want 1", fleet.domainsDown)
	}
	if fleet.restartingTogether != 2 {
		t.Errorf("at most %d node(s) were restarting at once, want the 2 of a failure domain",
			fleet.restartingTogether)
	}
	if len(fleet.shutdowns) != 6 || len(fleet.restarts) != 6 {
		t.Errorf("shutdowns = %v, restarts = %v, want one of each per node",
			fleet.shutdowns, fleet.restarts)
	}
	if rec.count(NodeRestarted) != 6 {
		t.Errorf("NodeRestarted was emitted %d times, want once per node", rec.count(NodeRestarted))
	}
}

// Two failure domains hold three chunks between them, so one of them holds two
// and losing it loses more than the one parity chunk covers. The walk restarts
// such a cluster one node at a time.
func TestTooFewFailureDomainsAreRestartedOneNodeAtATime(t *testing.T) {
	fleet := newDomainFleet(2, 2)
	r := newOpsReconciler(t, domainAPI(fleet), &recorder{}, domainCluster(),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	ops := walkDomains(t, r, fleet, 1)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message %q)", ops.Status.Phase, ops.Status.Message)
	}
	if fleet.mostDown != 1 {
		t.Errorf("%d nodes were down at once, want 1: a failure domain here holds two chunks "+
			"of a stripe", fleet.mostDown)
	}
}

// A cluster that is not in failure-domain mode places its chunks without
// regard to the index a node happens to carry, so the index groups nothing.
func TestAClusterOutsideFailureDomainModeIsRestartedOneNodeAtATime(t *testing.T) {
	fleet := newDomainFleet(3, 2)
	r := newOpsReconciler(t, domainAPI(fleet), &recorder{}, newTestCluster(),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	ops := walkDomains(t, r, fleet, 1)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message %q)", ops.Status.Phase, ops.Status.Message)
	}
	if fleet.mostDown != 1 {
		t.Errorf("%d nodes were down at once, want 1", fleet.mostDown)
	}
}

// The peer gate is drawn around the failure domain: a node that is not online
// in another failure domain holds the walk, and nothing is sent to the batch.
func TestANodeDownInAnotherFailureDomainHoldsTheBatch(t *testing.T) {
	fleet := newDomainFleet(3, 2)
	fleet.status["c-1"] = utils.NodeStatusOffline
	api := domainAPI(fleet)
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec, domainCluster(),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	ops, _ := reconcileOps(t, r, 8)
	if got := ops.Status.Step.State; got != string(stepCheckingPeers) {
		t.Errorf("step = %q, want the walk holding at CheckingPeers", got)
	}
	if len(fleet.shutdowns) != 0 {
		t.Errorf("the walk shut down %v while a node of another failure domain was offline",
			fleet.shutdowns)
	}
	if !rec.has(PeerNodeNotOnline) {
		t.Error("the walk held for a peer and said nothing")
	}
}

// A node of the batch that is already down does not hold the batch it belongs
// to, which is what lets a walk resumed with half a failure domain down go on.
func TestANodeOfTheBatchAlreadyDownDoesNotHoldIt(t *testing.T) {
	fleet := newDomainFleet(3, 2)
	fleet.status["a-2"] = utils.NodeStatusOffline
	r := newOpsReconciler(t, domainAPI(fleet), &recorder{}, domainCluster(),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	ops := walkDomains(t, r, fleet, 3)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (step %q, message %q)",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	if slices.Contains(fleet.shutdowns, "a-2") {
		t.Errorf("shutdowns = %v, want none for a-2, which was already offline", fleet.shutdowns)
	}
}

// The control plane has the last word on every shutdown. When it refuses a
// later node of a batch, the nodes already down are restarted without it, and
// the refused node is walked afterward: holding for it would leave part of a
// failure domain down for as long as the refusal lasts.
func TestARefusedShutdownEndsTheBatchAtTheNodesAlreadyDown(t *testing.T) {
	fleet := newDomainFleet(3, 2)
	api := domainAPI(fleet)
	accept := api.shutdownNode
	refused := false
	api.shutdownNode = func(cluster, id string) error {
		if id == "a-2" && !refused {
			refused = true
			return errors.New("409: the cluster is rebalancing")
		}
		return accept(cluster, id)
	}
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec, domainCluster(),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	ops := walkDomains(t, r, fleet, 3)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (step %q, message %q)",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	wantRestarts := []string{"a-1", "a-2", "b-1", "b-2", "c-1", "c-2"}
	if diff := cmp.Diff(wantRestarts, fleet.restarts); diff != "" {
		t.Errorf("the nodes were restarted in the wrong order (-want +got):\n%s", diff)
	}
	if fleet.domainsDown != 1 {
		t.Errorf("nodes were down in %d failure domains at once, want 1", fleet.domainsDown)
	}
	if !rec.has(BatchShortened) {
		t.Error("the batch was cut short and nothing said so")
	}
}

// A restart is asked for once per node. The control plane answers before the
// node's status moves, so the passes that follow still read every node of the
// batch as offline, and only the record of the call tells them it was made.
func TestEachNodeOfABatchIsAskedToRestartOnce(t *testing.T) {
	fleet := newDomainFleet(3, 2)
	api := domainAPI(fleet)
	pending := map[string]bool{}
	api.restartNode = func(_, id string) error {
		fleet.restarts = append(fleet.restarts, id)
		pending[id] = true
		return nil
	}
	r := newOpsReconciler(t, api, &recorder{}, domainCluster(),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))

	for pass := 1; pass <= 300; pass++ {
		ops, _ := reconcileOps(t, r, 1)
		if terminalOps(ops.Status.Phase) {
			break
		}
		// The restarts land ten passes after they were asked for.
		if pass%10 == 0 {
			for id := range pending {
				fleet.status[id] = utils.NodeStatusOnline
				delete(pending, id)
			}
		}
	}
	want := []string{"a-1", "a-2", "b-1", "b-2", "c-1", "c-2"}
	if diff := cmp.Diff(want, fleet.restarts); diff != "" {
		t.Errorf("the restarts asked for were not one per node (-want +got):\n%s", diff)
	}
}

// status.message names the batch, which is what `kubectl describe scops` shows.
func TestTheWalkReportsABatchByItsRange(t *testing.T) {
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Status.RollingRestart = &simplyblockv1alpha2.RollingRestartStatus{
				Nodes:     []string{"a-1", "a-2", "b-1", "b-2"},
				NodeIndex: 2,
				BatchSize: 2,
			}
		})

	got := walkMessage(ops, string(stepRestartingNode))
	want := "Nodes 3-4/4: RestartingNode"
	if got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// Regression: an object written before status.rollingRestart.batchSize existed
// carries none, and it is walked one node at a time as it was planned.
func TestAWalkPlannedBeforeBatchesIsWalkedOneNodeAtATime(t *testing.T) {
	fleet := newDomainFleet(3, 2)
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Status.RollingRestart = &simplyblockv1alpha2.RollingRestartStatus{
				Nodes: []string{"a-1", "b-1", "c-1", "a-2", "b-2", "c-2"},
			}
		})
	r := newOpsReconciler(t, domainAPI(fleet), &recorder{}, domainCluster(), ops)

	got := walkDomains(t, r, fleet, 1)
	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message %q)", got.Status.Phase, got.Status.Message)
	}
	if fleet.mostDown != 1 {
		t.Errorf("%d nodes were down at once, want 1: the planned order does not list a "+
			"failure domain's nodes together", fleet.mostDown)
	}
}
