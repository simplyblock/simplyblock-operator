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
		Name: name,
		Attachment: simplyblockv1alpha2.NetworkAttachment{
			NetworkAttachmentRef: &simplyblockv1alpha2.NetworkDefinitionReference{Name: name + "-definition"},
		},
	}
}

func hostNetwork(name, iface string) simplyblockv1alpha2.StorageNetwork {
	return simplyblockv1alpha2.StorageNetwork{
		Name:       name,
		Attachment: simplyblockv1alpha2.NetworkAttachment{HostInterface: iface},
	}
}

// I-09: a node of a cluster whose networks are attached through definitions is
// admitted without an address, whatever the other nodes hold.
func TestANodeOfAPooledClusterIsAdmittedWithoutAnAddress(t *testing.T) {
	for name, cluster := range map[string]*simplyblockv1alpha2.StorageCluster{
		"no network group": nodeTestCluster(nil),
		"host interfaces": networkCluster(&simplyblockv1alpha2.StorageNetworkSpec{
			Data: []simplyblockv1alpha2.StorageNetwork{hostNetwork("storage-a", "eth1")},
		}),
		"definition networks": networkCluster(&simplyblockv1alpha2.StorageNetworkSpec{
			Management: &[]simplyblockv1alpha2.StorageNetwork{definitionNetwork("management")}[0],
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
