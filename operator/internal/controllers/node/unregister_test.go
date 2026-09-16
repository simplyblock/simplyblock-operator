// Closing a node's device stream when the node goes.
//
// The scope is opened when the node resolves its backend id and closed when the
// node is torn down, and the two see different things. The open happens with the
// node freshly placed in its cluster, so the cluster's id is there to be had. The
// teardown happens on a path where the cluster may already be gone — a cluster
// deleted with its nodes is the ordinary case — and the close was written to need
// that id again.
//
// So it did not close. The control plane answers 404 for a node it no longer has,
// the manager reads that as a disconnect, and it reconnects on a backoff forever:
//
//	cpinformer stream disconnected, reconnecting
//	  {"subscription": "device", "scope": ".../b39b01f9-...", "err": "... 404"}

package node

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

func aResolvedNode(uuid string) *simplyblockv1alpha2.StorageNode {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "a-node", Namespace: "simplyblock"},
	}
	node.Status.UUID = uuid
	return node
}

// The stream closes even when the cluster it was opened against is gone.
func TestTheDeviceStreamClosesWithoutTheCluster(t *testing.T) {
	scopes := cpinformer.NewScopeSetForTest()
	scopes.Add(cpinformer.Scope{"cluster-uuid", "node-uuid"})
	r := &StorageNodeReconciler{DeviceScopes: scopes}

	// The cluster is unreadable at teardown, so there is no id to rebuild the
	// key with. That is the case the leak was found in.
	r.unregister(aResolvedNode("node-uuid"))

	if got := scopes.Len(); got != 0 {
		t.Errorf("%d scope(s) survived the node they stream", got)
	}
}

// It still closes on the ordinary path, where the cluster is right there.
func TestTheDeviceStreamClosesWithTheCluster(t *testing.T) {
	scopes := cpinformer.NewScopeSetForTest()
	scopes.Add(cpinformer.Scope{"cluster-uuid", "node-uuid"})
	r := &StorageNodeReconciler{DeviceScopes: scopes}

	r.unregister(aResolvedNode("node-uuid"))

	if got := scopes.Len(); got != 0 {
		t.Errorf("%d scope(s) survived the node they stream", got)
	}
}

// A node that never resolved an id opened no stream, so nothing is closed and
// no other node's stream is touched.
func TestANodeWithNoIdClosesNothing(t *testing.T) {
	scopes := cpinformer.NewScopeSetForTest()
	scopes.Add(cpinformer.Scope{"cluster-uuid", "another-node"})
	r := &StorageNodeReconciler{DeviceScopes: scopes}

	r.unregister(aResolvedNode(""))

	if got := scopes.Len(); got != 1 {
		t.Errorf("the set holds %d scope(s), want the other node's left alone", got)
	}
}
