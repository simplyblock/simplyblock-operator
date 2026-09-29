// The node-add cap under reconcilers that do not see each other.
//
// The cap used to be an election: every waiting node ordered the waiting workers
// and advanced only if its own place was within the number free. Two nodes
// reading one set reach one answer, which is true and is the whole of the
// assumption. Reconciles are not served from the API server but from an informer
// cache, and a cache that has just been filled — an operator restart, a new lease
// holder, a resync — does not hold every sibling yet. Nodes reading different
// sets reach different answers, and each of them is alone in the set it can see.
//
// These cases starve the sibling listing to model that, and assert on the cap
// rather than on how it is enforced.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// blindReconcilers builds one reconciler per node over a single shared backing
// store, each of which can list only its own node.
//
// The share is the point: the objects are one set of objects, as they are on a
// cluster, and only the listing is starved. Anything the reconcilers do to reach
// an agreement has to go through the store they have in common.
func blindReconcilers(
	t *testing.T,
	cluster *simplyblockv1alpha2.StorageCluster,
	nodes ...*simplyblockv1alpha2.StorageNode,
) []*StorageNodeReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)

	objects := make([]client.Object, 0, 1+len(nodes))
	objects = append(objects, cluster)
	for _, n := range nodes {
		objects = append(objects, n)
	}
	shared := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.StorageCluster{}).
		WithIndex(&simplyblockv1alpha2.StorageNode{}, clusterRefField,
			func(o client.Object) []string {
				return []string{o.(*simplyblockv1alpha2.StorageNode).Spec.ClusterRef}
			}).
		Build()

	reconcilers := make([]*StorageNodeReconciler, 0, len(nodes))
	for _, node := range nodes {
		only := node
		blind := interceptor.NewClient(shared, interceptor.Funcs{
			List: func(
				_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption,
			) error {
				nodeList, ok := list.(*simplyblockv1alpha2.StorageNodeList)
				if !ok {
					return nil
				}
				nodeList.Items = []simplyblockv1alpha2.StorageNode{*only}
				return nil
			},
		})
		reconcilers = append(reconcilers, &StorageNodeReconciler{Client: blind, Scheme: scheme})
	}
	return reconcilers
}

// admittedBlind reconciles every node once, each reading the cluster for itself
// first, and reports how many were sent on to post.
//
// The read per node is what a reconcile does: provision fetches the cluster at
// the top of every pass. Modeling it as one read shared by all three would be
// modeling three reconciles that began in the same instant, which is the case
// the optimistic lock is for and not the case a cap is measured in.
func admittedBlind(
	t *testing.T,
	reconcilers []*StorageNodeReconciler,
	nodes ...*simplyblockv1alpha2.StorageNode,
) int {
	t.Helper()
	count := 0
	for i, node := range nodes {
		next, _, err := reconcilers[i].awaitSlot(
			context.Background(), node, storedCluster(t, reconcilers[i]))
		if err != nil {
			continue
		}
		if next == stepPosting {
			count++
		}
	}
	return count
}

// storedCluster reads the cluster back from a reconciler's store.
func storedCluster(
	t *testing.T, r *StorageNodeReconciler,
) *simplyblockv1alpha2.StorageCluster {
	t.Helper()
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Name: "c", Namespace: "simplyblock"}
	if err := r.Get(context.Background(), key, &cluster); err != nil {
		t.Fatalf("read the cluster back: %v", err)
	}
	return &cluster
}

// TestTheCapHoldsWhenNodesCannotSeeEachOther is the cap under a cold cache.
//
// Regression: 2026-09-20-node-add-cap-lost-under-a-cold-cache — the cap was an
// election over the set of waiting nodes, so a reconciler whose cache did not yet
// hold the siblings elected itself and posted. On a six-worker cluster that is
// six simultaneous adds against a cap of one, and a node add reboots its host:
// the cap exists so that two hosts do not reboot at once and cost the control
// plane its own fault tolerance.
func TestTheCapHoldsWhenNodesCannotSeeEachOther(t *testing.T) {
	a, b, c := waitingNode("w1"), waitingNode("w2"), waitingNode("w3")
	cluster := slotCluster(1)
	reconcilers := blindReconcilers(t, cluster, a, b, c)

	if got := admittedBlind(t, reconcilers, a, b, c); got != 1 {
		t.Errorf("%d nodes took a single free slot while none could see the others", got)
	}
}

// The cap holds above one as well, which is what separates a cap from a mutex.
func TestACapOfTwoHoldsWhenNodesCannotSeeEachOther(t *testing.T) {
	a, b, c := waitingNode("w1"), waitingNode("w2"), waitingNode("w3")
	cluster := slotCluster(2)
	reconcilers := blindReconcilers(t, cluster, a, b, c)

	if got := admittedBlind(t, reconcilers, a, b, c); got != 2 {
		t.Errorf("%d nodes took two free slots while none could see the others", got)
	}
}

// A node that took a slot is readable from the cluster, by the worker that holds
// it and the object that took it.
func TestTakingASlotIsRecordedOnTheCluster(t *testing.T) {
	a := waitingNode("w1")
	cluster := slotCluster(1)
	reconcilers := blindReconcilers(t, cluster, a)

	next, _, err := reconcilers[0].awaitSlot(context.Background(), a, cluster.DeepCopy())
	if err != nil {
		t.Fatalf("awaitSlot: %v", err)
	}
	if next != stepPosting {
		t.Fatalf("the only waiting node was not admitted, it was sent to %s", next)
	}

	slots := storedCluster(t, reconcilers[0]).Status.ProvisioningSlots
	if len(slots) != 1 {
		t.Fatalf("the cluster records %d slots after one was taken", len(slots))
	}
	if slots[0].Worker != "w1" || slots[0].Node != a.Name {
		t.Errorf("the slot reads worker %q node %q", slots[0].Worker, slots[0].Node)
	}
}

// Re-entering the step with a slot already held keeps the one entry, because a
// held slot is not a reason to take a second.
func TestASlotIsNotTakenTwice(t *testing.T) {
	a := waitingNode("w1")
	cluster := slotCluster(1)
	reconcilers := blindReconcilers(t, cluster, a)

	for range 3 {
		if _, _, err := reconcilers[0].awaitSlot(
			context.Background(), a, storedCluster(t, reconcilers[0]),
		); err != nil {
			t.Fatalf("awaitSlot: %v", err)
		}
	}

	if slots := storedCluster(t, reconcilers[0]).Status.ProvisioningSlots; len(slots) != 1 {
		t.Errorf("three passes of one node left %d slots taken", len(slots))
	}
}

// A slot is released by the node that took it and by nothing else, which is the
// same discipline the cluster operation lock is released under.
func TestASlotIsReleasedOnlyByItsHolder(t *testing.T) {
	a, b := waitingNode("w1"), waitingNode("w2")
	cluster := slotCluster(1)
	cluster.Status.ProvisioningSlots = []simplyblockv1alpha2.ProvisioningSlot{
		{Worker: "w1", Node: a.Name, TakenAt: metav1.Now()},
	}
	reconcilers := blindReconcilers(t, cluster, a, b)

	if err := reconcilers[1].releaseSlot(context.Background(), b); err != nil {
		t.Fatalf("releaseSlot: %v", err)
	}
	if slots := storedCluster(t, reconcilers[1]).Status.ProvisioningSlots; len(slots) != 1 {
		t.Fatalf("a node released a slot it did not hold, leaving %d", len(slots))
	}

	if err := reconcilers[0].releaseSlot(context.Background(), a); err != nil {
		t.Fatalf("releaseSlot: %v", err)
	}
	if slots := storedCluster(t, reconcilers[0]).Status.ProvisioningSlots; len(slots) != 0 {
		t.Errorf("the holder released its slot and %d remain", len(slots))
	}
}

// A slot whose node object is gone is reaped, because nothing will ever release
// it and a cap held by a deleted object never reopens.
func TestASlotWhoseNodeIsGoneIsReaped(t *testing.T) {
	b := waitingNode("w2")
	cluster := slotCluster(1)
	cluster.Status.ProvisioningSlots = []simplyblockv1alpha2.ProvisioningSlot{
		{Worker: "w1", Node: "c-w1-0", TakenAt: metav1.Now()},
	}
	reconcilers := blindReconcilers(t, cluster, b)

	next, _, err := reconcilers[0].awaitSlot(context.Background(), b, cluster.DeepCopy())
	if err != nil {
		t.Fatalf("awaitSlot: %v", err)
	}
	if next != stepPosting {
		t.Fatalf("a slot held by an object that no longer exists still blocked the cap")
	}
}

// A slot whose node has already resolved its UUID is reaped too: the add it was
// taken for is finished, whatever the object has got round to writing.
func TestASlotWhoseNodeIsFinishedIsReaped(t *testing.T) {
	done, b := waitingNode("w1"), waitingNode("w2")
	done.Status.UUID = "4a304439-2909-4199-ad2f-b8624d66a13d"
	cluster := slotCluster(1)
	cluster.Status.ProvisioningSlots = []simplyblockv1alpha2.ProvisioningSlot{
		{Worker: "w1", Node: done.Name, TakenAt: metav1.Now()},
	}
	reconcilers := blindReconcilers(t, cluster, done, b)

	next, _, err := reconcilers[1].awaitSlot(context.Background(), b, cluster.DeepCopy())
	if err != nil {
		t.Fatalf("awaitSlot: %v", err)
	}
	if next != stepPosting {
		t.Fatalf("a slot held by a node that already has its UUID still blocked the cap")
	}
}

// TestASlotTakenFromAStaleReadIsRefused is the mutual exclusion itself.
//
// Two nodes beginning a reconcile in the same instant both read a cluster with a
// free slot. The second one's patch is conditioned on the resourceVersion it
// read, which the first one's patch has moved, so it is refused and it counts
// again rather than appending to a list it never saw. Without the condition both
// appends land and the cap is the number of nodes.
func TestASlotTakenFromAStaleReadIsRefused(t *testing.T) {
	a, b := waitingNode("w1"), waitingNode("w2")
	cluster := slotCluster(2)
	reconcilers := blindReconcilers(t, cluster, a, b)

	// Both hold the cluster as it was before either of them wrote.
	stale := storedCluster(t, reconcilers[0])

	first, _, err := reconcilers[0].awaitSlot(context.Background(), a, stale.DeepCopy())
	if err != nil {
		t.Fatalf("awaitSlot: %v", err)
	}
	if first != stepPosting {
		t.Fatalf("the first node was not admitted to a cluster with two free slots")
	}

	second, _, err := reconcilers[1].awaitSlot(context.Background(), b, stale.DeepCopy())
	if err == nil && second == stepPosting {
		t.Fatal("a node took a slot against a cluster it had not seen the last write to")
	}

	if slots := storedCluster(t, reconcilers[0]).Status.ProvisioningSlots; len(slots) != 1 {
		t.Errorf("two nodes patching one read left %d slots", len(slots))
	}
}

// TestASecondSocketDoesNotTakeASecondSlot covers the two-socket worker.
//
// One POST adds every socket of a worker, so a host with two of them must
// consume one slot and the second object must resolve against the add the first
// one asked for. The step-based sibling check answers this a pass later than the
// slot does — a node that has taken a slot is still recorded at AwaitingSlot
// until its transition is written — so with more than one slot free the second
// socket reaches the cap with room in it.
func TestASecondSocketDoesNotTakeASecondSlot(t *testing.T) {
	first := waitingNode("w1")
	second := waitingNode("w1")
	second.Name = "c-w1-1"
	cluster := slotCluster(2)
	reconcilers := blindReconcilers(t, cluster, first, second)

	if next, _, err := reconcilers[0].awaitSlot(
		context.Background(), first, storedCluster(t, reconcilers[0]),
	); err != nil || next != stepPosting {
		t.Fatalf("the first socket was sent to %s: %v", next, err)
	}

	next, _, err := reconcilers[1].awaitSlot(
		context.Background(), second, storedCluster(t, reconcilers[1]))
	if err != nil {
		t.Fatalf("awaitSlot: %v", err)
	}
	if next == stepPosting {
		t.Error("the second socket of one worker posted an add of its own")
	}
	if slots := storedCluster(t, reconcilers[1]).Status.ProvisioningSlots; len(slots) != 1 {
		t.Errorf("one worker holds %d slots", len(slots))
	}
}

// TestASlotIsHeldUntilTheAddIsFinished is what the cap is measured in.
//
// Regression: 2026-09-21-the-slot-was-released-when-the-uuid-appeared — the
// control plane writes the node object at the start of add_node, with
// status=in_creation, so its UUID exists seconds into an add that runs for
// minutes. Releasing on the UUID made every add look finished the moment it
// began: on a six-worker cluster with nodeProvisioningBudget 1, five node_add tasks
// ran at once and five SPDK pods came up together. The cap exists because an add
// reboots its host.
func TestASlotIsHeldUntilTheAddIsFinished(t *testing.T) {
	adding, waiting := waitingNode("w1"), waitingNode("w2")
	adding.Status.UUID = "10fe8da7-55d6-4162-a5a2-9575421ee31f"
	adding.Status.Status = nodeStatusInCreation

	cluster := slotCluster(1)
	cluster.Status.ProvisioningSlots = []simplyblockv1alpha2.ProvisioningSlot{
		{Worker: "w1", Node: adding.Name, TakenAt: metav1.Now()},
	}
	reconcilers := blindReconcilers(t, cluster, adding, waiting)

	next, _, err := reconcilers[1].awaitSlot(context.Background(), waiting, cluster.DeepCopy())
	if err == nil && next == stepPosting {
		t.Error("a second add was posted while the first worker was still being created")
	}
	if slots := storedCluster(t, reconcilers[1]).Status.ProvisioningSlots; len(slots) != 1 {
		t.Errorf("the cluster records %d slots while one add is running", len(slots))
	}
}

// Once the add is over the slot goes back, which is what keeps the queue moving.
func TestTheSlotGoesBackWhenTheNodeLeavesCreation(t *testing.T) {
	added, waiting := waitingNode("w1"), waitingNode("w2")
	added.Status.UUID = "10fe8da7-55d6-4162-a5a2-9575421ee31f"
	added.Status.Status = nodeStatusOnline

	cluster := slotCluster(1)
	cluster.Status.ProvisioningSlots = []simplyblockv1alpha2.ProvisioningSlot{
		{Worker: "w1", Node: added.Name, TakenAt: metav1.Now()},
	}
	reconcilers := blindReconcilers(t, cluster, added, waiting)

	next, _, err := reconcilers[1].awaitSlot(context.Background(), waiting, cluster.DeepCopy())
	if err != nil {
		t.Fatalf("awaitSlot: %v", err)
	}
	if next != stepPosting {
		t.Errorf("the next worker was sent to %s after the previous add finished", next)
	}
}
