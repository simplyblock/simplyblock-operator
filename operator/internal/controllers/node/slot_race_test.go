// Who takes a node-add slot when several nodes want one.
//
// The cap exists because a node add reboots its host, and two hosts rebooting at
// once costs the control plane its own fault tolerance. It was enforced by each
// node counting how many siblings had already claimed a slot, which works for
// exactly one of them: controller-runtime serializes reconciles per object, not
// across objects, so every waiting node reads the count before any of them has
// written its claim.
//
// The first add is therefore correctly alone, and the moment it finishes every
// remaining node reads a free slot in the same instant and takes it. The cap
// holds once and then means nothing, which is the shape a cap fails in when it
// is a count of other people's writes.
//
// A slot is now taken rather than counted: the holders are a list on the cluster
// and the taking is an optimistic-locked patch of it. These cases are the cap's
// arithmetic — one free slot admits one, two admit two, a slot already held
// admits nobody — and provisioningslots_test.go is the mutual exclusion itself.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// aBackendClusterID is the id the control plane knows these fixtures' cluster by.
const aBackendClusterID = "cluster-uuid"

// waitingNode is a node that has reached AwaitingSlot and claimed nothing.
func waitingNode(worker string) *simplyblockv1alpha2.StorageNode {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "c-" + worker + "-0", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: "c", WorkerNode: worker,
		},
	}
	node.Status.Step.State = string(stepAwaitingSlot)
	return node
}

// slotCluster is a cluster with the parallel-add cap given.
func slotCluster(limit int32) *simplyblockv1alpha2.StorageCluster {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			StorageNodes: &simplyblockv1alpha2.StorageNodesSpec{
				NodeProvisioningBudget: ptr.To(limit),
			},
		},
	}
	cluster.Status.UUID = aBackendClusterID
	return cluster
}

func slotReconciler(
	t *testing.T,
	cluster *simplyblockv1alpha2.StorageCluster,
	nodes ...*simplyblockv1alpha2.StorageNode,
) *StorageNodeReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)

	objects := make([]client.Object, 0, 1+len(nodes))
	objects = append(objects, cluster)
	for _, n := range nodes {
		objects = append(objects, n)
	}
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.StorageCluster{}).
		WithIndex(&simplyblockv1alpha2.StorageNode{}, clusterRefField,
			func(o client.Object) []string {
				return []string{o.(*simplyblockv1alpha2.StorageNode).Spec.ClusterRef}
			}).
		Build()

	return &StorageNodeReconciler{Client: apiClient, Scheme: scheme}
}

// heldBy is a cluster whose slots are already taken by the workers given, as a
// node that has posted its add and is waiting for the UUID leaves it.
func heldBy(
	cluster *simplyblockv1alpha2.StorageCluster, nodes ...*simplyblockv1alpha2.StorageNode,
) *simplyblockv1alpha2.StorageCluster {
	for _, node := range nodes {
		cluster.Status.ProvisioningSlots = append(
			cluster.Status.ProvisioningSlots, simplyblockv1alpha2.ProvisioningSlot{
				Worker: node.Spec.WorkerNode, Node: node.Name, TakenAt: metav1.Now(),
			})
	}
	return cluster
}

// admitted is how many of the waiting nodes would advance on one pass, each
// reading the cluster and deciding for itself as it does in a real reconcile.
func admitted(
	t *testing.T, r *StorageNodeReconciler, nodes ...*simplyblockv1alpha2.StorageNode,
) int {
	t.Helper()
	count := 0
	for _, node := range nodes {
		var cluster simplyblockv1alpha2.StorageCluster
		key := client.ObjectKey{Name: "c", Namespace: "simplyblock"}
		if err := r.Get(context.Background(), key, &cluster); err != nil {
			t.Fatalf("read the cluster: %v", err)
		}
		next, _, err := r.awaitSlot(context.Background(), node, &cluster)
		if err != nil {
			continue
		}
		if next == stepPosting {
			count++
		}
	}
	return count
}

// With nothing in flight and a cap of one, exactly one of three waiting nodes
// takes the slot.
func TestOnlyOneNodeTakesAFreeSlot(t *testing.T) {
	a, b, c := waitingNode("w1"), waitingNode("w2"), waitingNode("w3")
	r := slotReconciler(t, slotCluster(1), a, b, c)

	if got := admitted(t, r, a, b, c); got != 1 {
		t.Errorf("%d nodes took a single free slot", got)
	}
}

// The cap is honored above one too: two free slots admit two of three.
func TestACapOfTwoAdmitsTwo(t *testing.T) {
	a, b, c := waitingNode("w1"), waitingNode("w2"), waitingNode("w3")
	r := slotReconciler(t, slotCluster(2), a, b, c)

	if got := admitted(t, r, a, b, c); got != 2 {
		t.Errorf("%d nodes took two free slots", got)
	}
}

// The same node wins every time it is asked, so a node that was told to wait
// does not overtake one that was told to go on the next pass.
func TestTheChoiceIsStable(t *testing.T) {
	a, b, c := waitingNode("w1"), waitingNode("w2"), waitingNode("w3")
	r := slotReconciler(t, slotCluster(1), a, b, c)

	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Name: "c", Namespace: "simplyblock"}
	if err := r.Get(context.Background(), key, &cluster); err != nil {
		t.Fatal(err)
	}
	first, _, err := r.awaitSlot(context.Background(), a, &cluster)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), key, &cluster); err != nil {
		t.Fatal(err)
	}
	second, _, err := r.awaitSlot(context.Background(), a, &cluster)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("the same node was told %q then %q", first, second)
	}
}

// A node already in flight fills the cap, so nobody else advances.
func TestANodeInFlightFillsTheCap(t *testing.T) {
	inflight := waitingNode("w1")
	inflight.Status.Step.State = string(stepPosting)
	b, c := waitingNode("w2"), waitingNode("w3")
	r := slotReconciler(t, heldBy(slotCluster(1), inflight), inflight, b, c)

	if got := admitted(t, r, b, c); got != 0 {
		t.Errorf("%d nodes advanced past a full cap", got)
	}
}
