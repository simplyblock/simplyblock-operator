// The controller's own refusal of a migration onto a stretched cluster's remote
// node, which admission refuses too but checks only once.

package volume

import (
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// TestAMigrationOntoARemoteNodeFailsTerminally. On a stretched cluster the
// other site's nodes are listed as remote StorageNodes in the same cluster and
// report Online, so an operation that reached the controller without the
// webhook went on to create a migration onto a node this site does not manage.
//
// Regression: 2026-10-22-remote-node-migration-target.
func TestAMigrationOntoARemoteNodeFailsTerminally(t *testing.T) {
	node := testNodeObject()
	node.Spec.Config.Site = "site-b"
	node.Spec.Stretched = &simplyblockv1alpha2.StretchedNodeSpec{Remote: true}
	api := idleSubsystem()
	r := testReconciler(t, api, testOperation(), testVolumeObject(), testClusterObject(), node)

	for range 3 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed for a target on the other site",
			ops.Status.Phase, ops.Status.Message)
	}
	if !strings.Contains(ops.Status.Message, "RemoteNode:") || !strings.Contains(ops.Status.Message, "site-b") {
		t.Errorf("message = %q, want RemoteNode: and the node's site", ops.Status.Message)
	}
	if api.creates != 0 {
		t.Errorf("the control plane was asked for %d migrations, want none", api.creates)
	}
}
