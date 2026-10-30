// The PersistentVolumeOps webhook's refusal of a migration onto a stretched
// cluster's remote node, kept beside persistentvolumeops_validator_test.go and
// built from its helpers.

package webhook

import (
	"context"
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// TestPersistentVolumeOpsRefusesAMigrationOntoARemoteNode. On a stretched
// cluster the other site's nodes are listed as remote StorageNodes in the same
// cluster, so the cluster check passed and a migration onto a node this site
// does not manage was admitted.
//
// Regression: 2026-10-22-remote-node-migration-target.
func TestPersistentVolumeOpsRefusesAMigrationOntoARemoteNode(t *testing.T) {
	node := pvopsNodeObject()
	node.Spec.Config.Site = "site-b"
	node.Spec.Stretched = &simplyblockv1alpha2.StretchedNodeSpec{Remote: true}
	v := pvopsValidator(t, simplyblockVolume(), pvopsClusterObject(), node)

	got := v.Handle(context.Background(), pvopsCreateRequest(t, pvopsOperation()))
	if got.Allowed {
		t.Fatal("a migration onto the other site's node was admitted")
	}
	msg := got.Result.Message
	if !strings.HasPrefix(msg, "RemoteNode:") || !strings.Contains(msg, pvopsNode) ||
		!strings.Contains(msg, "site-b") {
		t.Errorf("the refusal does not start with RemoteNode: and name the node and its site: %s", msg)
	}
}
