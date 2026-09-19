// What Resolving does when the add it is waiting on has already given up.
//
// Resolving matches the backend node the control plane was asked to create. A
// node_add that fails creates none, so the match never succeeds and the step
// waits out its deadline — holding the one node-add slot while it does, which
// stalls every other node of the cluster behind a node that is never coming.
//
// The control plane says so plainly: the task leaves the window. So a step that
// is waiting for a node, with nothing still working to produce one, is waiting
// for nothing, and the answer is to ask again rather than to keep waiting.

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
)

// noBackendNodes answers as a control plane on which the add produced nothing.
// The rest of the interface is embedded and nil: a call to anything else is a
// test reaching past what it is about, and should panic rather than pass.
type noBackendNodes struct {
	ControlPlane
}

func (noBackendNodes) StorageNodes(context.Context, string) ([]NodeReading, error) {
	return nil, nil
}

func aResolvingNode() *simplyblockv1alpha2.StorageNode {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a-cluster-worker-1-0",
			Namespace: "simplyblock",
		},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: "a-cluster",
			WorkerNode: "worker-1",
		},
	}
	node.Status.Step.State = string(stepResolving)
	return node
}

func aClusterWithTasks(tasks ...simplyblockv1alpha2.ClusterTask) *simplyblockv1alpha2.StorageCluster {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "a-cluster", Namespace: "simplyblock"},
	}
	cluster.Status.UUID = "cluster-uuid"
	cluster.Status.Tasks = tasks
	return cluster
}

func aResolver(t *testing.T, cluster *simplyblockv1alpha2.StorageCluster,
	node *simplyblockv1alpha2.StorageNode) *StorageNodeReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	worker := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "192.168.10.113"},
		}},
	}
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(worker, cluster, node).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNode{}).
		Build()

	return &StorageNodeReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(64),
		API:      noBackendNodes{},
	}
}

// A node_add that has left the task window without producing a node is one to
// ask for again.
func TestResolvingAsksAgainWhenTheAddIsOver(t *testing.T) {
	node := aResolvingNode()
	cluster := aClusterWithTasks(simplyblockv1alpha2.ClusterTask{
		ID: "task-1", Type: "node_add", Status: "done",
	})
	r := aResolver(t, cluster, node)

	next, done, err := r.resolve(context.Background(), node, cluster)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if next != stepPosting {
		t.Errorf("the step went to %q, want Posting so the add is asked for again", next)
	}
	if !done {
		t.Error("the step did not advance, so the retry never happens")
	}
}

// While the add is still working, waiting is right: a node that re-POSTed here
// would ask for a second one alongside the first.
func TestResolvingWaitsWhileTheAddIsStillRunning(t *testing.T) {
	node := aResolvingNode()
	cluster := aClusterWithTasks(simplyblockv1alpha2.ClusterTask{
		ID: "task-1", Type: "node_add", Status: "running",
	})
	r := aResolver(t, cluster, node)

	next, done, err := r.resolve(context.Background(), node, cluster)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if next != stepResolving || done {
		t.Errorf("the step went to %q (done %v), want to keep waiting", next, done)
	}
}

// A task window that names no node_add at all is the same case as one whose add
// has finished: nothing is working on producing the node this step is waiting
// for. The window is capped, so a task that has scrolled out of it is over.
func TestResolvingAsksAgainWhenNoAddIsInTheWindow(t *testing.T) {
	node := aResolvingNode()
	cluster := aClusterWithTasks(simplyblockv1alpha2.ClusterTask{
		ID: "task-9", Type: "cluster_status", Status: "running",
	})
	r := aResolver(t, cluster, node)

	next, _, err := r.resolve(context.Background(), node, cluster)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if next != stepPosting {
		t.Errorf("the step went to %q, want Posting", next)
	}
}

// Another cluster's business is not this node's. A migration running alongside
// says nothing about whether the add that this step is waiting on is over.
func TestAnUnrelatedRunningTaskDoesNotHoldResolving(t *testing.T) {
	node := aResolvingNode()
	cluster := aClusterWithTasks(
		simplyblockv1alpha2.ClusterTask{ID: "t1", Type: "node_add", Status: "done"},
		simplyblockv1alpha2.ClusterTask{ID: "t2", Type: "lvol_migration", Status: "running"},
	)
	r := aResolver(t, cluster, node)

	next, _, err := r.resolve(context.Background(), node, cluster)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if next != stepPosting {
		t.Errorf("the step went to %q, want Posting: a migration is not this node's add", next)
	}
}
