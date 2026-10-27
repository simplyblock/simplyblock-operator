// What a document's storage network does: it reaches the StorageCluster it
// creates with its address pool, the nodes it creates state no address because
// the operator assigns one from the pool, and a document that states no network
// expands exactly as it did before the field existed.

package deployment

import (
	"context"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// groupsInterface is the management interface the document fixture gives its group.
const groupsInterface = "eth1"

func aNetwork() *simplyblockv1alpha2.StorageNetworkSpec {
	return &simplyblockv1alpha2.StorageNetworkSpec{
		Data: []simplyblockv1alpha2.StorageNetwork{{
			Name: "fabric",
			Attachment: simplyblockv1alpha2.NetworkAttachment{
				NetworkAttachmentRef: &simplyblockv1alpha2.NetworkDefinitionReference{Name: "fabric-data"},
			},
			AddressPool: &simplyblockv1alpha2.NetworkAddressPool{
				Subnet: "10.10.10.0/24", First: "10.10.10.151", Last: "10.10.10.170",
			},
		}},
	}
}

// withNetwork states the network on the cluster template and drops the
// interfaces the network replaces.
func withNetwork(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
	c.Spec.Cluster.Network = aNetwork()
	group := &c.Spec.NodeSets[0].Groups[0]
	group.MgmtInterface, group.DataInterfaces = "", nil
}

func TestADocumentWithoutANetworkExpandsAsItAlwaysDid(t *testing.T) {
	config := aDocument(nil)
	r := reconcilerFor(t)

	cluster, err := r.buildCluster(config, config.Spec.Cluster.Name)
	if err != nil {
		t.Fatal(err)
	}
	node := r.buildNode(config, cluster, config.Spec.NodeSets[0], config.Spec.NodeSets[0].Groups[0], "worker-1", 0)

	if cluster.Spec.StorageNodes.Network != nil {
		t.Errorf("the cluster carries network %+v for a document that states none", cluster.Spec.StorageNodes.Network)
	}
	if node.Spec.WorkerNode != "worker-1" {
		t.Errorf("the node is on worker %q, want worker-1", node.Spec.WorkerNode)
	}
	if cluster.Spec.StorageNodes.MgmtInterface != groupsInterface {
		t.Errorf("mgmtInterface = %q, want the group's %s", cluster.Spec.StorageNodes.MgmtInterface, groupsInterface)
	}
}

func TestTheDocumentsNetworkReachesTheClusterItCreates(t *testing.T) {
	config := aDocument(withNetwork)
	r := reconcilerFor(t)

	cluster, err := r.buildCluster(config, config.Spec.Cluster.Name)
	if err != nil {
		t.Fatal(err)
	}

	got := cluster.Spec.StorageNodes.Network
	if got == nil || len(got.Data) != 1 || got.Data[0].Name != "fabric" {
		t.Fatalf("cluster network = %+v, want the document's", got)
	}
	if got.Data[0].AddressPool == nil || got.Data[0].AddressPool.First != "10.10.10.151" {
		t.Errorf("the cluster's network lost its address pool: %+v", got.Data[0])
	}
	if got == config.Spec.Cluster.Network {
		t.Error("the cluster shares the document's network, so an edit to one would change the other")
	}
	if cluster.Spec.StorageNodes.MgmtInterface != "" || len(cluster.Spec.StorageNodes.DataInterfaces) != 0 {
		t.Errorf("the cluster carries interfaces %q %v beside a network",
			cluster.Spec.StorageNodes.MgmtInterface, cluster.Spec.StorageNodes.DataInterfaces)
	}
}

func validated(t *testing.T, mutate func(*simplyblockv1alpha2.ClusterDeploymentConfig)) []finding {
	t.Helper()
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		withNetwork(c)
		if mutate != nil {
			mutate(c)
		}
	})
	r := reconcilerFor(t, append(workers("worker-1", "worker-2"), config)...)
	found, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return found
}

// A document on a network states no address for any node: the operator takes
// each one from the pool when the node is added.
func TestADocumentWithItsNetworkAndNoNodeAddressesIsValid(t *testing.T) {
	if found := validated(t, nil); len(found) != 0 {
		t.Fatalf("findings = %+v, want none", found)
	}
}
