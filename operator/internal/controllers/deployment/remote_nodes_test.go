// A stretched cluster lists the other site's nodes as remote StorageNodes for
// visibility. The erasure-coding checks count the nodes this site's deployment
// already has, so remote nodes must not be part of that count.

package deployment

import (
	"context"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Regression: 2026-10-22-remote-nodes-phantom-footprint.
// The peer site's remote nodes, which have no worker, entered this site's
// footprint as a phantom node on a host with an empty name.
func TestExistingFootprintExcludesRemoteNodes(t *testing.T) {
	remote := aNode("peer-1", "").(*simplyblockv1alpha2.StorageNode)
	remote.Spec.Stretched = &simplyblockv1alpha2.StretchedNodeSpec{Remote: true}
	remote.Spec.Config.Site = "site-b"
	r := reconcilerFor(t, aNode("node-1", "worker-1"), remote)

	held, err := existingFootprint(context.Background(), r.Client, theNamespace, theCluster)
	if err != nil {
		t.Fatalf("read the footprint: %v", err)
	}
	if held.nodes() != 1 || len(held.workers) != 1 {
		t.Errorf("footprint has %d nodes on hosts %v, want worker-1's one node",
			held.nodes(), held.workers)
	}
}
