// A node whose backend node appears while it is queuing for a node-add slot.
//
// Adoption is what stops the operator adding a node the control plane already
// has, and the two gates before the queue both check for one. The queue itself
// did not: a node that reached AwaitingSlot before its backend node existed —
// because a previous operator posted the add, because the response was lost after
// the control plane committed, or because the object was rebuilt — waited for a
// slot it had no use for, and the check that would have found the node was behind
// it rather than in front.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// aQueueWithABackendNode is a node waiting for a slot on a worker the control
// plane already reports a node for.
func aQueueWithABackendNode(t *testing.T, readings []NodeReading) (
	*StorageNodeReconciler, *simplyblockv1alpha2.StorageNode, *simplyblockv1alpha2.StorageCluster,
) {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)

	node := waitingNode("w1")
	cluster := slotCluster(1)
	worker := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "w1"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: workerKubernetesIP},
			},
			NodeInfo: corev1.NodeSystemInfo{SystemUUID: workerSystemUUID},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(worker, cluster, node).
		WithStatusSubresource(&simplyblockv1alpha2.StorageCluster{}).
		WithIndex(&simplyblockv1alpha2.StorageNode{}, clusterRefField,
			func(o client.Object) []string {
				return []string{o.(*simplyblockv1alpha2.StorageNode).Spec.ClusterRef}
			}).
		Build()

	return &StorageNodeReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(64),
		API:      backendNodes{readings: readings},
	}, node, cluster
}

// TestANodeWaitingForASlotAdoptsTheNodeThatAppeared covers the gate the queue
// was missing.
//
// Regression: 2026-09-20-adoption-is-a-one-shot-gate-before-the-queue — adoption
// was checked at CheckingHost and CheckingConfig and nowhere after, so a node
// that passed both before its backend node existed never looked again. On the
// cluster this was found on, two workers had a running SPDK pod and a backend
// node the operator had added, and their objects sat at AwaitingSlot with no
// UUID: each held the queue for an add that had already happened, and the cap
// meant the rest of the fleet queued behind them.
func TestANodeWaitingForASlotAdoptsTheNodeThatAppeared(t *testing.T) {
	r, node, cluster := aQueueWithABackendNode(t, []NodeReading{{
		UUID:         "4a304439-2909-4199-ad2f-b8624d66a13d",
		Status:       nodeStatusOnline,
		ManagementIP: workerKubernetesIP,
		SystemUUID:   workerSystemUUID,
		RPCPort:      4422,
	}})

	next, done, err := r.awaitSlot(context.Background(), node, cluster)
	if err != nil {
		t.Fatalf("awaitSlot: %v", err)
	}
	if !done || next != stepAdopting {
		t.Fatalf("a node whose backend node already exists was sent to %s", next)
	}

	var stored simplyblockv1alpha2.StorageCluster
	key := client.ObjectKeyFromObject(cluster)
	if err := r.Get(context.Background(), key, &stored); err != nil {
		t.Fatalf("read the cluster back: %v", err)
	}
	if slots := stored.Status.ProvisioningSlots; len(slots) != 0 {
		t.Errorf("a node that had nothing to add took %d node-add slot(s)", len(slots))
	}
}

// With no backend node for the worker the queue behaves as it always has, so the
// check is a divert rather than a new way to decline a slot.
func TestANodeWithNoBackendNodeStillTakesItsSlot(t *testing.T) {
	r, node, cluster := aQueueWithABackendNode(t, nil)

	next, done, err := r.awaitSlot(context.Background(), node, cluster)
	if err != nil {
		t.Fatalf("awaitSlot: %v", err)
	}
	if !done || next != stepPosting {
		t.Fatalf("a node with nothing to adopt was sent to %s", next)
	}
}
