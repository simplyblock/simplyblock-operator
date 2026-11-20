// A stretched cluster lists the other site's nodes as remote StorageNodes for
// visibility. Arbitration acts on this site's nodes, so the node list it works
// from must leave remote nodes out.

package twonode

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Regression: 2026-10-22-remote-nodes-in-arbitration.
// A Dual cluster's node list included the peer site's remote node, so
// arbitration treated another site's node as one of its own pair.
func TestStorageNodesExcludesRemoteNodes(t *testing.T) {
	r, c := setup(t, &fakeAPI{}, nil)
	remote := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-peer", Namespace: ns},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: "edge",
			Stretched:  &simplyblockv1alpha2.StretchedNodeSpec{Remote: true},
		},
	}
	remote.Spec.Config.Site = "site-b"
	if err := c.Create(context.Background(), remote); err != nil {
		t.Fatal(err)
	}
	var cluster simplyblockv1alpha2.StorageCluster
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "edge"}, &cluster); err != nil {
		t.Fatal(err)
	}

	nodes, err := r.storageNodes(context.Background(), &cluster)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	if len(names) != 2 || names[0] != "sn-a" || names[1] != "sn-b" {
		t.Errorf("nodes = %v, want the two local nodes [sn-a sn-b]", names)
	}
}
