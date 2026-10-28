// The schema rules of a document's storage network, enforced by the apiserver:
// the network replaces the interfaces rather than joining them, and a network
// attached through a definition carries the pool its nodes' addresses come from.

package deployment

import (
	"context"
	"strings"
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

func wantRefused(t *testing.T, err error, mentions string) {
	t.Helper()
	if err == nil {
		t.Fatal("the apiserver accepted a document it should refuse")
	}
	if !strings.Contains(err.Error(), mentions) {
		t.Fatalf("refused for the wrong reason: %v (want it to mention %q)", err, mentions)
	}
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

func TestANetworkBesideAGroupsInterfacesIsRefused(t *testing.T) {
	err := createDocument(t, "net-both", func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		withNetwork(c)
		c.Spec.NodeSets[0].Groups[0].MgmtInterface = groupsInterface
	})

	wantRefused(t, err, "network")
}
