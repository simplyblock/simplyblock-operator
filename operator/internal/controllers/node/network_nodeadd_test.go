// What the node-add call carries for a node on a network: the attachment list
// built from the node's declaration, and nothing at all for a cluster that
// states no network.

package node

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/multus"
	"github.com/simplyblock/simplyblock-operator/internal/utils"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const nodeAddDefinition = "storage-data"

func aDataNetwork() *simplyblockv1alpha2.StorageNetworkSpec {
	return onDefinition(nodeAddDefinition)
}

func aDefinition() *unstructured.Unstructured {
	nad := multus.NewDefinition()
	nad.SetNamespace(opsNamespace)
	nad.SetName(nodeAddDefinition)
	return nad
}

// I-26
func TestTheAddCarriesTheAttachmentsOfANodeOnANetwork(t *testing.T) {
	r, _ := onANetwork(t, aDataNetwork(), aDefinition())
	api := r.API.(*scriptedControlPlane)
	node, cluster := nodeAndClusterOf(t, r)

	if err := r.postNode(context.Background(), node, cluster); err != nil {
		t.Fatalf("postNode: %v", err)
	}

	want := []utils.NodeNetworkParams{{
		Name: nodeAddDefinition, Namespace: opsNamespace, Interface: "data0", IPs: []string{"10.10.10.101/24"},
	}}
	if len(api.adds) != 1 || !reflect.DeepEqual(api.adds[0].Networks, want) {
		t.Fatalf("add carried %+v, want networks %+v", api.adds, want)
	}
}

// I-26 for a cluster that states no network: nothing about networks is sent.
func TestTheAddOfAHostNetworkClusterCarriesNoNetworks(t *testing.T) {
	r, _ := aSteadyNode(t, aControlPlane())
	api := r.API.(*scriptedControlPlane)

	if err := r.postNode(context.Background(), anUnprovisionedNode(stepPosting), anOpsCluster()); err != nil {
		t.Fatalf("postNode: %v", err)
	}

	if api.adds[0].Networks != nil {
		t.Fatalf("networks = %+v, want none for a cluster with no network", api.adds[0].Networks)
	}
}

// I-27: a retried add sends the identical body, so the control plane's idempotency
// on it is what keeps the node count at one.
func TestARetriedAddSendsAnIdenticalBody(t *testing.T) {
	r, _ := onANetwork(t, aDataNetwork(), aDefinition())
	api := r.API.(*scriptedControlPlane)
	node, cluster := nodeAndClusterOf(t, r)

	for range 2 {
		if err := r.postNode(context.Background(), node, cluster); err != nil {
			t.Fatalf("postNode: %v", err)
		}
	}

	if !reflect.DeepEqual(api.adds[0], api.adds[1]) {
		t.Fatalf("the retry sent %+v after %+v", api.adds[1], api.adds[0])
	}
}

// I-28: a refused add records no task and surfaces the refusal.
func TestARefusedAddRecordsNoTask(t *testing.T) {
	r, apiClient := onANetwork(t, aDataNetwork(), aDefinition())
	r.API.(*scriptedControlPlane).refusing("AddNode", errors.New("400: networks not accepted"))
	node, cluster := nodeAndClusterOf(t, r)

	if err := r.postNode(context.Background(), node, cluster); err == nil {
		t.Fatal("postNode accepted a refused add")
	}
	if id := nodeRead(t, apiClient).Status.NodeAddTaskID; id != "" {
		t.Fatalf("task %q recorded for an add the control plane refused", id)
	}
}

// A node that states no address on a network it is attached to must not be added
// to the host network by a silent omission: the add is not made.
func TestAnAddWithoutTheNodesAddressIsNotMade(t *testing.T) {
	r, apiClient := onANetwork(t, aDataNetwork(), aDefinition())
	api := r.API.(*scriptedControlPlane)
	node, cluster := nodeAndClusterOf(t, r)
	node.Status.Network = nil
	_ = apiClient

	if err := r.postNode(context.Background(), node, cluster); err == nil {
		t.Fatal("postNode added a node with no address on its network")
	}
	if len(api.adds) != 0 {
		t.Fatalf("the add was made without the node's address: %+v", api.adds)
	}
}

// nodeAndClusterOf reads the node and its cluster as the API server holds them.
func nodeAndClusterOf(t *testing.T, r *StorageNodeReconciler) (*simplyblockv1alpha2.StorageNode, *simplyblockv1alpha2.StorageCluster) {
	t.Helper()
	node := &simplyblockv1alpha2.StorageNode{}
	cluster := &simplyblockv1alpha2.StorageCluster{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}, node); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: opsNamespace, Name: opsCluster}, cluster); err != nil {
		t.Fatal(err)
	}
	return node, cluster
}

// The add of a node on the pod network says so and carries no attachment, so the
// control plane starts the pod off the host network behind its Service.
func TestTheAddOfAPodNetworkNodeDeclaresTheModeAndNoNetworks(t *testing.T) {
	r, _ := onANetwork(t, viaPodNetwork())
	api := r.API.(*scriptedControlPlane)
	node, cluster := nodeAndClusterOf(t, r)

	if err := r.postNode(context.Background(), node, cluster); err != nil {
		t.Fatalf("postNode: %v", err)
	}

	if !api.adds[0].PodNetwork {
		t.Fatal("the add did not declare the pod network")
	}
	if api.adds[0].Networks != nil {
		t.Fatalf("networks = %+v, want none on the pod network", api.adds[0].Networks)
	}
}

func TestOnlyAPodNetworkDeclarationIsOnThePodNetwork(t *testing.T) {
	for name, cluster := range map[string]*simplyblockv1alpha2.StorageCluster{
		"no network":       {},
		"a definition":     clusterDeclaring(aDataNetwork()),
		"the host network": clusterDeclaring(viaHost()),
	} {
		t.Run(name, func(t *testing.T) {
			if onPodNetwork(cluster) {
				t.Fatal("reported on the pod network")
			}
		})
	}
	if !onPodNetwork(clusterDeclaring(viaPodNetwork())) {
		t.Fatal("a pod network declaration was not recognised")
	}
}

func clusterDeclaring(network *simplyblockv1alpha2.StorageNetworkSpec) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{Spec: simplyblockv1alpha2.StorageClusterSpec{
		StorageNodes: &simplyblockv1alpha2.StorageNodesSpec{Network: network},
	}}
}

// A definition whose device a device plugin hands out (an SR-IOV VF pool) names
// the plugin's resource in the k8s.v1.cni.cncf.io/resourceName annotation. The
// preflight reads it with the definition and records it on the node's status
// beside the address, so the one read serves the add too.
func TestThePreflightRecordsTheDeviceResourceOfTheDefinition(t *testing.T) {
	definition := aDefinitionConfigured(hostDeviceStatic)
	definition.SetAnnotations(map[string]string{
		"k8s.v1.cni.cncf.io/resourceName": "intel.com/sriov_storage",
	})
	r, apiClient := onANetwork(t, onDefinition(storageNetwork), definition)
	node := nodeRead(t, apiClient)
	node.Status.Network = nil
	if err := apiClient.Status().Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	r.HostCheck = aWorkerWithTheNIC(adoptedAddress)

	if _, _, err := runCheck(t, r); err != nil {
		t.Fatalf("checkNetwork: %v", err)
	}

	status := nodeRead(t, apiClient).Status.Network
	if len(status) != 1 || status[0].Resource != "intel.com/sriov_storage" {
		t.Fatalf("status = %+v, want the resource recorded on network %s", status, storageNetwork)
	}
}

// The add carries the recorded resource with the network, so the SPDK pod
// requests one of it: without the request the plugin allocates no device and
// Multus has none to attach.
func TestTheAddCarriesTheDeviceResourceOfTheDefinition(t *testing.T) {
	r, apiClient := onANetwork(t, aDataNetwork(), aDefinition())
	api := r.API.(*scriptedControlPlane)
	node := nodeRead(t, apiClient)
	node.Status.Network[0].Resource = "intel.com/sriov_storage"
	if err := apiClient.Status().Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	node, cluster := nodeAndClusterOf(t, r)

	if err := r.postNode(context.Background(), node, cluster); err != nil {
		t.Fatalf("postNode: %v", err)
	}

	want := []utils.NodeNetworkParams{{
		Name: nodeAddDefinition, Namespace: opsNamespace, Interface: "data0", IPs: []string{"10.10.10.101/24"},
		Resource: "intel.com/sriov_storage",
	}}
	if len(api.adds) != 1 || !reflect.DeepEqual(api.adds[0].Networks, want) {
		t.Fatalf("add carried %+v, want networks %+v", api.adds, want)
	}
}
