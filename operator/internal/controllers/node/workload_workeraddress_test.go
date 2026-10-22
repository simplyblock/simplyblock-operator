// What wakes the workload pass when a Kubernetes node's address changes. The
// API EndpointSlice publishes a worker only once its Node has an internal IP, so
// a Node that registers late, or moves to another address, is an input of the
// slice and has to be a trigger of it. It lives beside the workload controller's
// other tests because the pass is that controller's.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

const otherNamespace = "other"

// aNodeOfAnotherCluster is a StorageNode of a second cluster, in its own
// namespace, on a worker of its own.
func aNodeOfAnotherCluster() *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "their-node", Namespace: otherNamespace},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: "their-cluster",
			WorkerNode: "worker-9",
		},
	}
}

// Regression: 2026-10-22-slice-never-sees-a-late-worker-address — the slice reads
// each worker's Node for its internal IP and skips a worker that has none yet,
// but nothing watched Nodes. A worker whose Node registered after its StorageNode
// appeared stayed out of the slice until something else happened to touch the
// cluster, and its per-pod DNS name never resolved.
//
// A Node wakes the pass of every cluster that has a StorageNode on it, and no
// other.
func TestAWorkersNodeObjectWakesTheClustersRunningOnIt(t *testing.T) {
	scheme := testsupport.NewScheme(t)
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(anOpsNode(), aNodeOfAnotherCluster()).
		Build()
	r := &StorageNodeWorkloadReconciler{Client: apiClient, Scheme: scheme, Namespace: opsNamespace}

	woken := r.clusterOfWorker(context.Background(), aWorker(opsWorker, true))
	if len(woken) != 1 || woken[0].Name != opsCluster || woken[0].Namespace != opsNamespace {
		t.Errorf("the worker's Node woke %v, want only the cluster running on it", woken)
	}

	if woken := r.clusterOfWorker(context.Background(), aWorker("worker-unused", true)); len(woken) != 0 {
		t.Errorf("a Node no StorageNode runs on woke %v, want nothing", woken)
	}
}

// Only a change to the address the slice carries wakes the pass. A Node's status
// is rewritten on every kubelet heartbeat, and none of that changes the slice.
func TestOnlyAChangedAddressWakesThePassOnANodeUpdate(t *testing.T) {
	worker := aWorker(opsWorker, true)
	heartbeat := worker.DeepCopy()
	heartbeat.Status.Conditions[0].LastHeartbeatTime = metav1.Now()
	moved := worker.DeepCopy()
	moved.Status.Addresses[0].Address = "10.0.9.9"
	addressless := worker.DeepCopy()
	addressless.Status.Addresses = []corev1.NodeAddress{}

	filter := workerAddressChanged()
	if !filter.Create(event.CreateEvent{Object: worker}) {
		t.Error("a Node appearing must wake the pass")
	}
	if !filter.Delete(event.DeleteEvent{Object: worker}) {
		t.Error("a Node going away must wake the pass")
	}
	if !filter.Update(event.UpdateEvent{ObjectOld: addressless, ObjectNew: worker}) {
		t.Error("a Node gaining its first address must wake the pass")
	}
	if !filter.Update(event.UpdateEvent{ObjectOld: worker, ObjectNew: moved}) {
		t.Error("a changed address must wake the pass")
	}
	if filter.Update(event.UpdateEvent{ObjectOld: worker, ObjectNew: heartbeat}) {
		t.Error("a heartbeat must not wake the pass")
	}
}
