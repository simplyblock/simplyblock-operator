// Which node operations wait for their cluster to be active.
//
// The gate exists for an operation that moves data: a relocation or a maintenance
// window into a cluster that is rebalancing or unready is either rejected by the
// control plane or succeeds into a layout nobody described, so holding until the
// cluster settles is the correct behavior and the event says so.
//
// The rest are exempt, and each for the same reason: the operation is how the
// cluster's reading changes rather than something the reading protects. A
// removal is how an unready cluster becomes ready. A shutdown or a suspend is
// what makes a cluster degraded, and a restart or a resume is what makes it
// active again. Holding any of them on the cluster being active closes a loop
// with no way out, which a shutdown demonstrated on 2026-09-28: the node it took
// down degraded the cluster, and the degraded cluster held the shutdown.

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

// The four single-step actions run whatever the cluster says about itself,
// because each of them is what moves the cluster between active and degraded.
//
// Regression: 2026-09-28-shutdown-holds-lock-in-degraded-cluster — a Shutdown
// degraded its cluster and was then held by the gate on the degraded cluster,
// and the Restart that would have recovered it was held the same way.
func TestTheSingleStepActionsDoNotWaitOnTheCluster(t *testing.T) {
	for _, action := range []simplyblockv1alpha2.StorageNodeOpsAction{
		simplyblockv1alpha2.StorageNodeOpsActionShutdown,
		simplyblockv1alpha2.StorageNodeOpsActionRestart,
		simplyblockv1alpha2.StorageNodeOpsActionSuspend,
		simplyblockv1alpha2.StorageNodeOpsActionResume,
	} {
		if !skipsClusterGate(opsFor(action)) {
			t.Errorf("%s waits for a cluster whose reading %s is what changes", action, action)
		}
	}
}

// The two actions that move data still wait, because the gate is what keeps an
// operation that moves data off a cluster that cannot take it.
func TestTheActionsThatMoveDataWaitOnTheCluster(t *testing.T) {
	for _, action := range []simplyblockv1alpha2.StorageNodeOpsAction{
		simplyblockv1alpha2.StorageNodeOpsActionMigrate,
		simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance,
	} {
		if skipsClusterGate(opsFor(action)) {
			t.Errorf("%s skipped the cluster gate", action)
		}
	}
}
