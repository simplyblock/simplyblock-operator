// Maintenance of a whole failure domain at once.
//
// A cluster in failure-domain mode with at least as many domains as a stripe
// has chunks keeps one chunk of a stripe in a domain at most, so every worker
// of one domain being down costs a stripe what one worker being down costs it.
// The gate therefore counts failure domains on such a cluster, exactly as it
// counts two sockets of one host as one worker, and the cluster gate admits a
// window to a cluster that only the window's own domain has degraded.
//
// design-storagenode.md §10.

package node

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	rackA = "rack-a"
	rackB = "rack-b"
	rackC = "rack-c"
)

// aNodeIn is one more node of the fixture's cluster, on a worker of its own in
// the given failure domain.
func aNodeIn(name, worker, domain string) *simplyblockv1alpha2.StorageNode {
	node := anOpsNode()
	node.Name = name
	node.Spec.WorkerNode = worker
	node.Spec.Config.FailureDomain = domain
	node.Status.UUID = "uuid-of-" + name
	node.Status.Status = nodeStatusOnline
	return node
}

// aWindowIn is somebody else's window, past Holding, on a node of the given
// failure domain.
func aWindowIn(
	name, nodeName, worker, domain string,
) (*simplyblockv1alpha2.StorageNodeOps, *simplyblockv1alpha2.StorageNode) {
	ops, node := aRunningWindow(name, nodeName, worker)
	node.Spec.Config.FailureDomain = domain
	node.Status.UUID = "uuid-of-" + nodeName
	node.Status.Status = nodeStatusOffline
	ops.Status.Step = kubeStep(stepAwaitingHost)
	return ops, node
}

// laidOutInDomains puts the fixture's cluster in failure-domain mode with the
// given layout, and the fixture's own node in rack-a.
func laidOutInDomains(t *testing.T, apiClient client.Client, scheme string) {
	t.Helper()
	ctx := context.Background()

	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsCluster}
	if err := apiClient.Get(ctx, key, &cluster); err != nil {
		t.Fatalf("reading the cluster: %v", err)
	}
	cluster.Spec.EnableFailureDomains = ptr.To(true)
	if err := apiClient.Update(ctx, &cluster); err != nil {
		t.Fatalf("putting the cluster in failure-domain mode: %v", err)
	}
	cluster.Status.ErasureCodingScheme = scheme
	if err := apiClient.Status().Update(ctx, &cluster); err != nil {
		t.Fatalf("stating the cluster's layout: %v", err)
	}

	var node simplyblockv1alpha2.StorageNode
	key = client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}
	if err := apiClient.Get(ctx, key, &node); err != nil {
		t.Fatalf("reading the node: %v", err)
	}
	node.Spec.Config.FailureDomain = rackA
	if err := apiClient.Update(ctx, &node); err != nil {
		t.Fatalf("placing the node in %s: %v", rackA, err)
	}
	node.Status.Status = nodeStatusOnline
	if err := apiClient.Status().Update(ctx, &node); err != nil {
		t.Fatalf("reporting the node online: %v", err)
	}
}

// Every worker of one failure domain is one domain's worth of unavailability,
// so a second worker of the domain already in maintenance does not wait.
func TestWorkersOfOneFailureDomainEnterMaintenanceTogether(t *testing.T) {
	other, itsNode := aWindowIn("another-window", "another-node", opsElsewhere, rackA)
	r, apiClient := anOpsWorld(t, aControlPlane(), other, itsNode,
		aNodeIn("node-b", "worker-b", rackB), aNodeIn("node-c", "worker-c", rackC))
	laidOutInDomains(t, apiClient, "2x1")

	if held(t, r, aWindow("a-window")) {
		t.Error("a worker waited for another worker of its own failure domain")
	}
}

// A worker of another failure domain is a second domain's worth, and waits.
func TestAWorkerOfAnotherFailureDomainWaitsItsTurn(t *testing.T) {
	other, itsNode := aWindowIn("another-window", "another-node", opsElsewhere, rackB)
	r, apiClient := anOpsWorld(t, aControlPlane(), other, itsNode,
		aNodeIn("node-c", "worker-c", rackC))
	laidOutInDomains(t, apiClient, "2x1")

	if !held(t, r, aWindow("a-window")) {
		t.Error("two failure domains entered maintenance at once at a fault tolerance of one")
	}
}

// What the cluster can afford is counted in failure domains, so a fault
// tolerance of two admits a second domain and not a third.
func TestTheLimitCountsFailureDomains(t *testing.T) {
	first, firstNode := aWindowIn("window-b", "node-b", "worker-b", rackB)
	sibling, siblingNode := aWindowIn("window-b2", "node-b2", "worker-b2", rackB)
	r, apiClient := anOpsWorld(t, aControlPlane(), first, firstNode, sibling, siblingNode,
		aNodeIn("node-c", "worker-c", rackC), aNodeIn("node-d", "worker-d", "rack-d"))
	laidOutInDomains(t, apiClient, "2x2")
	allowing(t, apiClient, 2)

	if held(t, r, aWindow("a-window")) {
		t.Error("two workers of one failure domain were counted as two of the two slots")
	}

	third, thirdNode := aWindowIn("window-c", "node-c2", "worker-c2", rackC)
	for _, object := range []client.Object{thirdNode, third} {
		if err := apiClient.Create(context.Background(), object); err != nil {
			t.Fatalf("raising a window in a third domain: %v", err)
		}
	}
	if !held(t, r, aWindow("a-window")) {
		t.Error("a third failure domain entered maintenance at a fault tolerance of two")
	}
}

// With fewer failure domains than a stripe has chunks, a domain holds more
// than one chunk of a stripe, and two of its workers down are two chunks lost.
func TestTooFewFailureDomainsAreCountedByWorker(t *testing.T) {
	other, itsNode := aWindowIn("another-window", "another-node", opsElsewhere, rackA)
	r, apiClient := anOpsWorld(t, aControlPlane(), other, itsNode,
		aNodeIn("node-b", "worker-b", rackB))
	laidOutInDomains(t, apiClient, "2x1")

	if !held(t, r, aWindow("a-window")) {
		t.Error("two workers of one domain went down together although " +
			"two domains hold the three chunks of a stripe")
	}
}

// A label on a node means nothing to a cluster that is not in failure-domain
// mode: the control plane placed its chunks without it.
func TestAClusterOutsideFailureDomainModeIsCountedByWorker(t *testing.T) {
	other, itsNode := aWindowIn("another-window", "another-node", opsElsewhere, rackA)
	r, apiClient := anOpsWorld(t, aControlPlane(), other, itsNode,
		aNodeIn("node-b", "worker-b", rackB), aNodeIn("node-c", "worker-c", rackC))
	laidOutInDomains(t, apiClient, "2x1")

	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsCluster}
	if err := apiClient.Get(context.Background(), key, &cluster); err != nil {
		t.Fatalf("reading the cluster: %v", err)
	}
	cluster.Spec.EnableFailureDomains = nil
	if err := apiClient.Update(context.Background(), &cluster); err != nil {
		t.Fatalf("taking the cluster out of failure-domain mode: %v", err)
	}

	if !held(t, r, aWindow("a-window")) {
		t.Error("two workers went down together on a cluster that is not in failure-domain mode")
	}
}

// A node that declares no failure domain is one whose chunks cannot be placed
// by domain, so the count goes back to workers.
func TestANodeWithNoFailureDomainMakesTheCountByWorker(t *testing.T) {
	other, itsNode := aWindowIn("another-window", "another-node", opsElsewhere, rackA)
	r, apiClient := anOpsWorld(t, aControlPlane(), other, itsNode,
		aNodeIn("node-b", "worker-b", rackB), aNodeIn("node-c", "worker-c", rackC),
		aNodeIn("node-nowhere", "worker-n", ""))
	laidOutInDomains(t, apiClient, "2x1")

	if !held(t, r, aWindow("a-window")) {
		t.Error("two workers went down together although a node of the cluster is in no domain")
	}
}

// The nodes of a failure domain go down one after another, because the
// control plane refuses a shutdown while another is in flight. Waiting for the
// slot is where that wait belongs: a window that waits in ShuttingDown spends
// the budget of a step that is meant to be short.
func TestAWindowWaitsWhileAWorkerOfItsDomainIsStillGoingDown(t *testing.T) {
	other, itsNode := aWindowIn("another-window", "another-node", opsElsewhere, rackA)
	other.Status.Step = kubeStep(stepShuttingDown)
	itsNode.Status.Status = nodeStatusInShutdown
	r, apiClient := anOpsWorld(t, aControlPlane(), other, itsNode,
		aNodeIn("node-b", "worker-b", rackB), aNodeIn("node-c", "worker-c", rackC))
	laidOutInDomains(t, apiClient, "2x1")

	if !held(t, r, aWindow("a-window")) {
		t.Error("a window was admitted while a worker of its domain was still shutting down")
	}
}

// A shutdown is not sent into another node's shutdown or restart: the control
// plane refuses it, and the step would read the refusal as its own failure.
func TestAShutdownIsNotSentWhileAnotherNodeIsInTransition(t *testing.T) {
	for _, status := range []string{nodeStatusInShutdown, nodeStatusInRestart} {
		t.Run(status, func(t *testing.T) {
			api := aControlPlane()
			peer := aNodeIn("node-b", "worker-b", rackA)
			peer.Status.Status = status
			r, _ := anOpsWorld(t, api, aReadyStoragePod(opsWorker), peer)

			done, err := performing(t, r, aWindow("a-window"), stepShuttingDown)
			if err != nil {
				t.Fatalf("shutting down: %v", err)
			}
			if done {
				t.Error("the step finished while the node was still online")
			}
			if asked := api.asked("ShutdownNode"); asked != 0 {
				t.Errorf("ShutdownNode was issued %d time(s) while node-b was %s", asked, status)
			}
		})
	}
}

// degradedBy runs one pass of a new window against a cluster the control plane
// reports degraded, with the named node down, and reports whether the cluster
// gate held the window.
func degradedBy(t *testing.T, scheme string, down *simplyblockv1alpha2.StorageNode) bool {
	t.Helper()
	down.Status.Status = nodeStatusOffline
	ops := aWindow("a-window")
	ops.Finalizers = []string{OpsFinalizer}
	r, apiClient := anOpsWorld(t, aControlPlane(), ops, down,
		aNodeIn("node-b", "worker-b", rackB), aNodeIn("node-c", "worker-c", rackC))
	laidOutInDomains(t, apiClient, scheme)
	r.Clusters = &deliveredCluster{synced: true, reading: subscriptions.ClusterDTO{
		ID: opsClusterID, Status: utils.ClusterStatusDegraded,
	}}

	pass(t, r, "a-window")
	return announcedReason(r, ClusterNotReady)
}

// A cluster degraded only by nodes of the window's own failure domain can take
// one more of them down, and holding the window would make a whole-domain
// maintenance one worker at a time again.
func TestAWindowIsAdmittedToAClusterItsOwnDomainDegraded(t *testing.T) {
	if degradedBy(t, "2x1", aNodeIn("node-a2", "worker-a2", rackA)) {
		t.Error("the window was held on a cluster that only its own failure domain degraded")
	}
}

// A node down in another failure domain is a chunk already missing from the
// stripes this window's node holds a chunk of.
func TestAWindowWaitsOnAClusterAnotherDomainDegraded(t *testing.T) {
	if !degradedBy(t, "2x1", aNodeIn("node-b2", "worker-b2", rackB)) {
		t.Error("the window was admitted although a node of another failure domain is down")
	}
}

// With too few failure domains a degraded cluster holds the window, whichever
// domain the node that is down belongs to.
func TestAWindowWaitsOnADegradedClusterWithTooFewDomains(t *testing.T) {
	if !degradedBy(t, "4x2", aNodeIn("node-a2", "worker-a2", rackA)) {
		t.Error("the window was admitted to a degraded cluster whose three domains " +
			"hold the six chunks of a stripe")
	}
}

// Only a maintenance window is admitted this way. A relocation moves data
// into the cluster, and a degraded cluster cannot take it.
func TestOnlyAMaintenanceWindowIsAdmittedToADegradedCluster(t *testing.T) {
	down := aNodeIn("node-a2", "worker-a2", rackA)
	down.Status.Status = nodeStatusOffline
	ops := anOperation("a-relocation", simplyblockv1alpha2.StorageNodeOpsActionMigrate)
	ops.Finalizers = []string{OpsFinalizer}
	r, apiClient := anOpsWorld(t, aControlPlane(), ops, down,
		aNodeIn("node-b", "worker-b", rackB), aNodeIn("node-c", "worker-c", rackC))
	laidOutInDomains(t, apiClient, "2x1")
	r.Clusters = &deliveredCluster{synced: true, reading: subscriptions.ClusterDTO{
		ID: opsClusterID, Status: utils.ClusterStatusDegraded,
	}}

	pass(t, r, "a-relocation")

	if !announcedReason(r, ClusterNotReady) {
		t.Error("a relocation was admitted to a degraded cluster")
	}
}
