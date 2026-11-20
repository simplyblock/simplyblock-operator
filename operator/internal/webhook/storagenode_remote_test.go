// Who may create a stretched cluster's remote StorageNode record, kept beside
// storagenode_validator_test.go and built from its helpers.

package webhook

import (
	"context"
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// remoteTestNode is the other site's node as the operator lists it: no worker,
// the peer's site, and the remote mark.
func remoteTestNode() *simplyblockv1alpha2.StorageNode {
	return testNode(func(n *simplyblockv1alpha2.StorageNode) {
		n.Spec.WorkerNode = ""
		n.Spec.Config.Site = "site-b"
		n.Spec.Stretched = &simplyblockv1alpha2.StretchedNodeSpec{Remote: true}
	})
}

// TestAHandMadeRemoteNodeIsRefused. Any user could create a StorageNode marked
// remote, which is a record of a node that no site's operator manages and that
// the migration and placement paths then resolve a peer-site UUID to.
//
// Regression: 2026-10-22-remote-node-migration-target.
func TestAHandMadeRemoteNodeIsRefused(t *testing.T) {
	v := validatorFor(t, nodeTestCluster(nil))

	got := v.Handle(context.Background(), createReq(t, remoteTestNode(), someoneElse))
	if got.Allowed {
		t.Fatal("a remote StorageNode created by someone other than the operator was admitted")
	}
	if msg := got.Result.Message; !strings.HasPrefix(msg, "RemoteNode:") {
		t.Errorf("the refusal does not start with RemoteNode: %s", msg)
	}
}

// TestTheOperatorMayCreateARemoteNode. Listing the other site's nodes is the
// operator's job, and refusing it would leave this site blind to them.
func TestTheOperatorMayCreateARemoteNode(t *testing.T) {
	v := validatorFor(t, nodeTestCluster(nil))

	got := v.Handle(context.Background(), createReq(t, remoteTestNode(), operatorUser))
	if !got.Allowed {
		t.Fatalf("the operator's remote StorageNode was refused: %s", got.Result.Message)
	}
}

// TestAUserMayStillCreateAnOrdinaryNode. The remote check is about the mark,
// and a node of this site written by hand is admitted as before.
func TestAUserMayStillCreateAnOrdinaryNode(t *testing.T) {
	v := validatorFor(t, nodeTestCluster(nil))

	got := v.Handle(context.Background(), createReq(t, testNode(nil), someoneElse))
	if !got.Allowed {
		t.Fatalf("an ordinary StorageNode created by a user was refused: %s", got.Result.Message)
	}
}
