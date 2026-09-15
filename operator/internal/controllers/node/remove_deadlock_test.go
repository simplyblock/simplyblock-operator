// Whether a node operation waits for its cluster to be active.
//
// Most of them must. An operation that moves data while the cluster is
// rebalancing or unready is either rejected by the control plane or succeeds
// into a layout nobody described, so holding until the cluster settles is the
// correct behavior and the event says so.
//
// Removal is the exception, because it is the repair rather than the thing being
// protected: a node is removed from an unready cluster precisely to make the
// cluster ready. Holding it closes a loop with no way out — the node cannot be
// removed until the cluster is active, and the cluster cannot become active
// while the node it is stuck on is still there.

package node

import (
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func opsFor(action simplyblockv1alpha2.StorageNodeOpsAction) *simplyblockv1alpha2.StorageNodeOps {
	ops := &simplyblockv1alpha2.StorageNodeOps{}
	ops.Spec.Action = action
	return ops
}

// A removal runs whatever the cluster says about itself.
func TestRemovalDoesNotWaitOnTheCluster(t *testing.T) {
	if !skipsClusterGate(opsFor(simplyblockv1alpha2.StorageNodeOpsActionRemove)) {
		t.Error("a removal waits for a cluster that removal is how you repair")
	}
}

// Everything else still waits, because the gate is what keeps an operation that
// moves data off a cluster that cannot take it.
func TestEveryOtherActionWaitsOnTheCluster(t *testing.T) {
	for _, action := range []simplyblockv1alpha2.StorageNodeOpsAction{
		simplyblockv1alpha2.StorageNodeOpsActionMigrate,
		simplyblockv1alpha2.StorageNodeOpsActionRestart,
		simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance,
	} {
		if skipsClusterGate(opsFor(action)) {
			t.Errorf("%s skipped the cluster gate", action)
		}
	}
}
