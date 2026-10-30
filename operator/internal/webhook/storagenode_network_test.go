// What the StorageNode guard says about the storage networks of its cluster: a
// node states no address on a network, because the operator assigns it one from
// the network's pool, so the guard has nothing to refuse on that account.

package webhook

import (
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func networkCluster(network *simplyblockv1alpha2.StorageNetworkSpec) *simplyblockv1alpha2.StorageCluster {
	return nodeTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{Network: network}
	})
}

func definitionNetwork(name string) simplyblockv1alpha2.StorageNetwork {
	return simplyblockv1alpha2.StorageNetwork{
		NetworkAttachmentRef: simplyblockv1alpha2.NetworkDefinitionReference{Name: name + "-definition"},
	}
}

// I-09: a node of a cluster whose networks are attached through definitions is
// admitted without an address, whatever the other nodes hold.
func TestANodeOfAPooledClusterIsAdmittedWithoutAnAddress(t *testing.T) {
	for name, cluster := range map[string]*simplyblockv1alpha2.StorageCluster{
		"no network group": nodeTestCluster(nil),
		"host interfaces": networkCluster(&simplyblockv1alpha2.StorageNetworkSpec{
			SpdkOnHostNetwork: true,
		}),
		"definition networks": networkCluster(&simplyblockv1alpha2.StorageNetworkSpec{
			PodNetwork: true,
			Data:       []simplyblockv1alpha2.StorageNetwork{definitionNetwork("storage-a")},
		}),
	} {
		t.Run(name, func(t *testing.T) {
			resp := validatorFor(t, cluster).Handle(t.Context(), createReq(t, testNode(nil), operatorUser))
			if !resp.Allowed {
				t.Fatalf("expected the node to be admitted, got: %v", resp.Result)
			}
		})
	}
}

// A node names host NICs only on a cluster whose SPDK pods run on the host network,
// or on a cluster that states no network, which is the legacy host-network mode.
func TestANodeNamesDataInterfacesOnlyOnTheHostNetwork(t *testing.T) {
	for name, tc := range map[string]struct {
		cluster *simplyblockv1alpha2.StorageCluster
		allowed bool
	}{
		"the host network":     {networkCluster(&simplyblockv1alpha2.StorageNetworkSpec{SpdkOnHostNetwork: true}), true},
		"no network":           {nodeTestCluster(nil), true},
		"the pod network":      {networkCluster(&simplyblockv1alpha2.StorageNetworkSpec{PodNetwork: true}), false},
		"a definition network": {networkCluster(&simplyblockv1alpha2.StorageNetworkSpec{PodNetwork: true, Data: []simplyblockv1alpha2.StorageNetwork{definitionNetwork("storage-a")}}), false},
	} {
		t.Run(name, func(t *testing.T) {
			node := testNode(func(n *simplyblockv1alpha2.StorageNode) { n.Spec.Config.DataInterfaces = []string{"eth1"} })

			resp := validatorFor(t, tc.cluster).Handle(t.Context(), createReq(t, node, operatorUser))

			if resp.Allowed != tc.allowed {
				t.Fatalf("allowed = %v, want %v: %v", resp.Allowed, tc.allowed, resp.Result)
			}
		})
	}
}
