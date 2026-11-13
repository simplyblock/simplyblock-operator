// A stretched cluster lists the other site's nodes as remote StorageNodes for
// visibility. They belong to the peer site, so this site's cluster-wide counts
// must not include them.

package cluster

import (
	"context"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// remoteNodeOfTestCluster is a peer site's node listed on this cluster: no
// worker, the peer's site, and the failure domain the control plane projects.
func remoteNodeOfTestCluster(name, domain string) *simplyblockv1alpha2.StorageNode {
	node := nodeOfTestCluster(name, "")
	node.Spec.Stretched = &simplyblockv1alpha2.StretchedNodeSpec{Remote: true}
	node.Spec.Config.Site = "site-b"
	node.Status.UUID = name + "-uuid"
	node.Status.Status = "online"
	node.Status.FailureDomain = domain
	return node
}

// Regression: 2026-10-22-remote-nodes-count-toward-stripe.
// The activation's stripe-node gate counted the peer site's remote nodes as
// this site's, so a site with too few nodes of its own passed the gate.
func TestStorageNodeCountExcludesRemoteNodes(t *testing.T) {
	c := newClient(t,
		nodeOfTestCluster("node-1", "worker-1"),
		nodeOfTestCluster("node-2", "worker-2"),
		remoteNodeOfTestCluster("peer-1", "0"),
		remoteNodeOfTestCluster("peer-2", "1"),
		remoteNodeOfTestCluster("peer-3", "2"))

	count, err := storageNodeCount(context.Background(), c, testNamespace, testClusterName)
	if err != nil {
		t.Fatalf("count the nodes: %v", err)
	}
	if count != 2 {
		t.Errorf("counted %d nodes, want the 2 local ones", count)
	}
}

// Regression: 2026-10-22-remote-nodes-phantom-failure-domain-host.
// The peer site's remote nodes, which have no worker, collapsed into a phantom
// host "" carrying a failure domain of the peer site.
func TestFailureDomainHostsExcludesRemoteNodes(t *testing.T) {
	local := nodeOfTestCluster("node-1", "worker-1")
	local.Status.FailureDomain = "0"
	c := newClient(t, local,
		remoteNodeOfTestCluster("peer-1", "1"),
		remoteNodeOfTestCluster("peer-2", "2"))

	hosts, err := FailureDomainHosts(context.Background(), c, testNamespace, testClusterName)
	if err != nil {
		t.Fatalf("read the failure domains: %v", err)
	}
	if len(hosts) != 1 || hosts["worker-1"] != 0 {
		t.Errorf("hosts = %v, want only worker-1 in domain 0", hosts)
	}
}
