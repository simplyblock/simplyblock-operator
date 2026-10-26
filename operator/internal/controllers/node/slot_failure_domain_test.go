// Which waiting worker takes a node-add slot when the cluster has failure domains.
//
// The control plane admits an expansion's node add only when the per-domain host
// counts it leaves stay within one of each other, and it checks that inside the
// add task, after the POST has been accepted. A worker sent from the domain that
// is already ahead is refused there and retried, and it keeps the slot while it
// is: the worker from the short domain, the one add that would restore the
// balance, waits behind it until the refused node's step runs out. The ranking
// in awaitSlot is what decides which of them goes first.

package node

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// addedNode is a node of the cluster that the control plane already has, in the
// failure domain given.
func addedNode(worker, domain string) *simplyblockv1alpha2.StorageNode {
	node := waitingNode(worker)
	node.Spec.Config.FailureDomain = domain
	node.Status.Step.State = ""
	node.Status.UUID = "uuid-" + worker
	return node
}

// waitingNodeIn is a node waiting for a slot, in the failure domain given.
func waitingNodeIn(worker, domain string) *simplyblockv1alpha2.StorageNode {
	node := waitingNode(worker)
	node.Spec.Config.FailureDomain = domain
	return node
}

func failureDomainCluster(limit int32) *simplyblockv1alpha2.StorageCluster {
	cluster := slotCluster(limit)
	cluster.Spec.EnableFailureDomains = ptr.To(true)
	return cluster
}

// Regression: 2026-09-28-expansion-add-refused-by-failure-domain-balance
// (release/26.3.2 #570, e0bbfdb1). Domain a has two hosts and domain b one. The
// waiting worker from a sorts first by name, and the control plane refuses its
// add, so the slot has to go to the worker from b.
func TestTheSlotGoesToTheShortFailureDomain(t *testing.T) {
	fromA, fromB := waitingNodeIn("w4", "a"), waitingNodeIn("w5", "b")
	r := slotReconciler(t, failureDomainCluster(1),
		addedNode("w1", "a"), addedNode("w2", "a"), addedNode("w3", "b"),
		fromA, fromB)

	if got := admitted(t, r, fromA); got != 0 {
		t.Errorf("the worker from domain a, already a host ahead, took the slot")
	}
	if got := admitted(t, r, fromB); got != 1 {
		t.Errorf("the worker from domain b, a host short, did not take the slot")
	}
}

// Balanced domains keep the name order: with a and b level, w4 from a still goes
// before w5 from b.
func TestBalancedFailureDomainsKeepTheNameOrder(t *testing.T) {
	fromA, fromB := waitingNodeIn("w4", "a"), waitingNodeIn("w5", "b")
	r := slotReconciler(t, failureDomainCluster(1),
		addedNode("w1", "a"), addedNode("w2", "b"),
		fromA, fromB)

	if got := admitted(t, r, fromB); got != 0 {
		t.Errorf("w5 took the slot ahead of w4 with the domains level")
	}
	if got := admitted(t, r, fromA); got != 1 {
		t.Errorf("w4 did not take the slot with the domains level")
	}
}

// A worker already in flight counts toward its domain. With w4 from b holding
// one of two slots, b is level with a, so w5 from b is ranked behind w6 from a.
func TestAnAddInFlightCountsTowardItsDomain(t *testing.T) {
	inflight := waitingNodeIn("w4", "b")
	inflight.Status.Step.State = string(stepPosting)
	fromB, fromA := waitingNodeIn("w5", "b"), waitingNodeIn("w6", "a")
	r := slotReconciler(t, heldBy(failureDomainCluster(2), inflight),
		addedNode("w1", "a"), addedNode("w2", "b"), addedNode("w3", "a"),
		inflight, fromA, fromB)

	if got := admitted(t, r, fromB); got != 0 {
		t.Errorf("w5 from b took the slot, putting b a host ahead")
	}
	if got := admitted(t, r, fromA); got != 1 {
		t.Errorf("w6 from a did not take the slot")
	}
}

// Without failure domains the domain labels are ignored and the name order holds.
func TestWithoutFailureDomainsTheNameOrderHolds(t *testing.T) {
	fromA, fromB := waitingNodeIn("w4", "a"), waitingNodeIn("w5", "b")
	r := slotReconciler(t, slotCluster(1),
		addedNode("w1", "a"), addedNode("w2", "a"), addedNode("w3", "b"),
		fromA, fromB)

	if got := admitted(t, r, fromB); got != 0 {
		t.Errorf("w5 took the slot ahead of w4 on a cluster without failure domains")
	}
	if got := admitted(t, r, fromA); got != 1 {
		t.Errorf("w4 did not take the slot on a cluster without failure domains")
	}
}
