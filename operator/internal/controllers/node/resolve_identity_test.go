// Which backend node belongs to which worker.
//
// Resolving has to recognize the node the add just produced, and the only thing
// that makes that possible is an identity both sides report. The control plane
// names a node by an address on the storage plane, which is the network its
// nodes talk to each other over and is not required to be the one Kubernetes
// runs on.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// The two networks of the cluster this was found on, and the host identity both
// sides carry: Kubernetes reports it as Node.status.nodeInfo.systemUUID and the
// control plane as the node's system_uuid, both read from the same firmware.
const (
	workerKubernetesIP = "10.0.0.15"
	workerStorageIP    = "192.168.10.15"
	workerSystemUUID   = "7aa0847c-6308-11ef-b0d9-02310e2a5e00"
)

// backendNodes answers with the readings it was built from.
type backendNodes struct {
	ControlPlane
	readings []NodeReading
}

func (b backendNodes) StorageNodes(context.Context, string) ([]NodeReading, error) {
	return b.readings, nil
}

// adds counts the node_add calls a case provoked, which is what separates
// "the step held" from "the step held after asking for a node anyway".
type countingBackend struct {
	ControlPlane
	adds *int
}

func (countingBackend) StorageNodes(context.Context, string) ([]NodeReading, error) {
	return nil, nil
}

func (c countingBackend) AddNode(context.Context, string, utils.StorageNodeSetAddParams) error {
	*c.adds++
	return nil
}

// aMatcher builds a reconciler over one worker whose Kubernetes address and
// storage-plane address differ, which is the ordinary shape of a deployment
// whose storage traffic is on its own network.
func aMatcher(t *testing.T, readings []NodeReading, systemUUID string) (
	*StorageNodeReconciler, *simplyblockv1alpha2.StorageNode, *simplyblockv1alpha2.StorageCluster,
) {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	worker := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: workerKubernetesIP}},
			NodeInfo:  corev1.NodeSystemInfo{SystemUUID: systemUUID},
		},
	}
	node := aResolvingNode()
	cluster := aClusterWithTasks()

	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(worker, cluster, node).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNode{}).
		Build()

	return &StorageNodeReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(64),
		API:      backendNodes{readings: readings},
	}, node, cluster
}

// TestANodeIsFoundWhenTheStoragePlaneIsItsOwnNetwork covers the node the add
// produced on a cluster whose storage network is not the Kubernetes one.
//
// Regression: 2026-09-20-backend-node-matched-by-the-kubernetes-address — the
// match kept a reading only where its mgmt_ip was the worker's Kubernetes
// InternalIP. The control plane reports the address the node holds on the
// storage plane, which on a two-network cluster is a different subnet
// (192.168.10.15 against 10.0.0.15), so every reading was dropped. The node was
// online and healthy and the operator reported that the add had produced
// nothing, re-posted once a second, and would have failed the node on its
// deadline with a backend node running underneath it.
func TestANodeIsFoundWhenTheStoragePlaneIsItsOwnNetwork(t *testing.T) {
	r, node, cluster := aMatcher(t, []NodeReading{{
		UUID:         "4a304439-2909-4199-ad2f-b8624d66a13d",
		Status:       nodeStatusOnline,
		ManagementIP: workerStorageIP,
		SystemUUID:   workerSystemUUID,
		Hostname:     "worker-1_4422",
		RPCPort:      4422,
	}}, workerSystemUUID)

	reading, found, err := r.matchBackendNode(context.Background(), node, cluster)
	if err != nil {
		t.Fatalf("matchBackendNode: %v", err)
	}
	if !found {
		t.Fatal("the node the add produced was not recognized, so the add reads as having produced nothing")
	}
	if reading.UUID != "4a304439-2909-4199-ad2f-b8624d66a13d" {
		t.Errorf("matched %q", reading.UUID)
	}
}

// A node of another worker is not this worker's, whatever network it is on.
func TestANodeOfAnotherWorkerIsNotMatched(t *testing.T) {
	r, node, cluster := aMatcher(t, []NodeReading{{
		UUID:         "someone-else",
		Status:       nodeStatusOnline,
		ManagementIP: "192.168.10.99",
		SystemUUID:   "ef7f45c7-ac2b-9a4f-ee58-08bfb8a41207",
		RPCPort:      4422,
	}}, workerSystemUUID)

	_, found, err := r.matchBackendNode(context.Background(), node, cluster)
	if err != nil {
		t.Fatalf("matchBackendNode: %v", err)
	}
	if found {
		t.Error("another worker's node was taken for this one")
	}
}

// A control plane that reports no system UUID is still matched on the address,
// which is what every single-network deployment has always been matched on.
func TestTheAddressStillMatchesWhereNoSystemUUIDIsReported(t *testing.T) {
	r, node, cluster := aMatcher(t, []NodeReading{{
		UUID:         "older-control-plane",
		Status:       nodeStatusOnline,
		ManagementIP: workerKubernetesIP,
		RPCPort:      4422,
	}}, workerSystemUUID)

	_, found, err := r.matchBackendNode(context.Background(), node, cluster)
	if err != nil {
		t.Fatalf("matchBackendNode: %v", err)
	}
	if !found {
		t.Error("a reading with no system UUID stopped matching on the address it always did")
	}
}
