// The schema rules of a document's storage network, enforced by the apiserver:
// the network replaces the interfaces rather than joining them, and a network
// attached through a definition carries the pool its nodes' addresses come from.

package deployment

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// createDocument stores a draft built from the fixture with the mutation applied
// and answers the apiserver's verdict on it.
func createDocument(
	t *testing.T, name string, mutate func(*simplyblockv1alpha2.ClusterDeploymentConfig),
) error {
	t.Helper()
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.ObjectMeta = metav1.ObjectMeta{Name: name, Namespace: "default"}
		c.Spec.Approved = false
		if mutate != nil {
			mutate(c)
		}
	})
	return apiServer(t).Create(context.Background(), config)
}

func TestADocumentOnANetworkWithItsPoolIsAccepted(t *testing.T) {
	if err := createDocument(t, "net-ok", withNetwork); err != nil {
		t.Fatalf("a document with a network and its pool was refused: %v", err)
	}
}

func TestADocumentWithoutANetworkIsAcceptedAsBefore(t *testing.T) {
	if err := createDocument(t, "net-none", nil); err != nil {
		t.Fatalf("a document with no network was refused: %v", err)
	}
}

// A host-interface network states only that the data network is a host NIC, and
// each group names its own NICs, so the two sit side by side.
func TestAHostInterfaceNetworkBesideAGroupsDataInterfacesIsAccepted(t *testing.T) {
	err := createDocument(t, "net-both", func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		withNetwork(c)
		c.Spec.Cluster.Network = &simplyblockv1alpha2.StorageNetworkSpec{SpdkOnHostNetwork: true}
		c.Spec.NodeSets[0].Groups[0].MgmtInterface = groupsInterface
		c.Spec.NodeSets[0].Groups[0].DataInterfaces = []string{"eth2"}
	})

	if err != nil {
		t.Fatalf("a host-interface network beside a group's interfaces was refused: %v", err)
	}
}

func TestANetworkBesideAGroupsManagementInterfaceIsAccepted(t *testing.T) {
	err := createDocument(t, "net-mgmt", func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		withNetwork(c)
		c.Spec.NodeSets[0].Groups[0].MgmtInterface = groupsInterface
	})

	if err != nil {
		t.Fatalf("a network beside a management interface was refused: %v", err)
	}
}
