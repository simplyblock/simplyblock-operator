// Tests for a pass that reads the operation from a cache one write behind. The
// manager's client serves reads from an informer, and a pass that runs straight
// after another one can read the operation as it stood before that pass made its
// call. Every node call here waits in its step for the node to move, so the
// second pass finds the node where the first one found it and, without a claim,
// makes the call again: a second forced restart, a second promote, a second
// removal.
//
// Regression: lblk_outage_matrix_k8s-20261002-111746, where the same read
// sent an Activate to the control plane twice, 32ms apart.

package node

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

func TestAPassReadingTheOperationBeforeTheCallDoesNotCallAgain(t *testing.T) {
	forced := func(o *simplyblockv1alpha2.StorageNodeOps) { o.Spec.Force = ptr.To(true) }
	relocating := func(o *simplyblockv1alpha2.StorageNodeOps) {
		o.Spec.Migrate = &simplyblockv1alpha2.MigrateSpec{TargetWorkerNode: opsTarget}
	}

	cases := []struct {
		name   string
		action simplyblockv1alpha2.StorageNodeOpsAction
		at     step
		status string
		call   string
		mutate func(*simplyblockv1alpha2.StorageNodeOps)
	}{
		{"Shutdown", simplyblockv1alpha2.StorageNodeOpsActionShutdown,
			stepRequesting, nodeStatusOnline, "ShutdownNode", nil},
		{"forced Restart", simplyblockv1alpha2.StorageNodeOpsActionRestart,
			stepRequesting, nodeStatusOnline, "RestartNode", forced},
		{"Suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend,
			stepRequesting, nodeStatusOnline, "Suspend", nil},
		{"Resume", simplyblockv1alpha2.StorageNodeOpsActionResume,
			stepRequesting, nodeStatusSuspended, "Resume", nil},
		{"Remove shutting down", simplyblockv1alpha2.StorageNodeOpsActionRemove,
			stepShuttingDown, nodeStatusOnline, "ShutdownNode", nil},
		{"Remove preparing an offline node", simplyblockv1alpha2.StorageNodeOpsActionRemove,
			stepMigratingDevices, nodeStatusOffline, "PrepareRemoval", nil},
		{"Remove removing", simplyblockv1alpha2.StorageNodeOpsActionRemove,
			stepRemoving, nodeStatusSuspended, "RemoveNode", nil},
		{"HostMaintenance restarting", simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance,
			stepRestarting, nodeStatusOffline, "RestartNode", nil},
		{"Migrate relocating", simplyblockv1alpha2.StorageNodeOpsActionMigrate,
			stepRelocating, nodeStatusOnline, "RestartNode", relocating},
		{"Migrate promoting", simplyblockv1alpha2.StorageNodeOpsActionMigrate,
			stepPromoting, nodeStatusOnline, "Promote", relocating},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := anAdvancingOperation("an-operation", tc.action, tc.at)
			if tc.mutate != nil {
				tc.mutate(ops)
			}
			api := aControlPlane().reporting(tc.status)
			r, apiClient := anOpsWorld(t, api, ops)
			cache := &testsupport.LaggingClient{Client: r.Client}
			r.Client = cache
			holdTheNodeLock(t, apiClient, "an-operation")

			var before *simplyblockv1alpha2.StorageNodeOps
			var node simplyblockv1alpha2.StorageNode
			for range 5 {
				before = operationRead(t, apiClient, "an-operation")
				if err := apiClient.Get(context.Background(),
					types.NamespacedName{Namespace: opsNamespace, Name: opsNodeName}, &node); err != nil {
					t.Fatalf("read the node: %v", err)
				}
				pass(t, r, "an-operation")
				if api.asked(tc.call) > 0 {
					break
				}
			}
			if api.asked(tc.call) != 1 {
				t.Fatalf("%s was issued %d times on the way to it, want 1; calls: %v",
					tc.call, api.asked(tc.call), api.calls)
			}

			// The node object lags with the operation for the whole pass,
			// because one cache serves both, and a promote is skipped on what
			// the node says.
			cache.Lag(before, 1)
			cache.Lag(&node, 10)
			pass(t, r, "an-operation")
			cache.CatchUp()

			if got := api.asked(tc.call); got != 1 {
				t.Errorf("%s was issued %d times, want 1: the second pass read the "+
					"operation before the first one made the call; calls: %v",
					tc.call, got, api.calls)
			}
		})
	}
}

// holdTheNodeLock gives the operation the node's lock before the first pass, so
// that a stale copy of the node read on a later pass still names the operation
// as its holder, as it would once the operation has been running for a while.
func holdTheNodeLock(t *testing.T, apiClient client.Client, name string) {
	t.Helper()
	ctx := context.Background()
	var node simplyblockv1alpha2.StorageNode
	if err := apiClient.Get(ctx,
		types.NamespacedName{Namespace: opsNamespace, Name: opsNodeName}, &node); err != nil {
		t.Fatalf("read the node: %v", err)
	}
	node.Status.ActiveOpsRef = name
	if err := apiClient.Status().Update(ctx, &node); err != nil {
		t.Fatalf("lock the node: %v", err)
	}
}
