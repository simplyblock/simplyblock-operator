// Whether a cluster's workload puts its workers into the storage plane.
//
// The DaemonSet that runs SPDK selects workers by the io.simplyblock.storagenodeset
// label, so a worker without it runs nothing. Writing that label was the retired
// StorageNodeSet's job, and when provisioning was rebuilt around StorageNode the
// migration path kept writing it while nothing on the provisioning path did.
//
// The result deadlocks rather than fails, which is what makes it worth a test of
// its own: a node holds at CheckingHost waiting for the worker's storage-node API
// to answer, and the process that would answer cannot be scheduled until the
// label it is waiting on has been written. Neither side reports an error.

package node

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

const (
	enrollNamespace = "simplyblock"
	enrollCluster   = "a-cluster"
	enrollWorker    = "worker-1"
)

// aWorkloadReconciler drives the cluster's workload over a fake client.
func aWorkloadReconciler(t *testing.T, objects ...client.Object) *StorageNodeWorkloadReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t,
		corev1.AddToScheme, appsv1.AddToScheme, discoveryv1.AddToScheme, rbacv1.AddToScheme)
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.StorageCluster{}).
		Build()

	return &StorageNodeWorkloadReconciler{
		Client:    apiClient,
		Scheme:    scheme,
		Recorder:  events.NewFakeRecorder(64),
		Namespace: enrollNamespace,
		Workload:  &Workload{Client: apiClient},
	}
}

// aSizedCluster carries the sizing its workers boot from, without which the
// workload refuses before it reaches anything this file is about.
func aSizedCluster() *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: enrollCluster, Namespace: enrollNamespace},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(30)),
			VCPUCount:         ptr.To(int32(4)),
			StorageNodes: &simplyblockv1alpha2.StorageNodesSpec{
				Image: "example.test/storage-node:test",
			},
		},
	}
}

// aNodeOn is a StorageNode bound to a worker, as the deployment config creates one.
func aNodeOn(worker string) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      enrollCluster + "-" + worker + "-0",
			Namespace: enrollNamespace,
		},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: enrollCluster,
			WorkerNode: worker,
		},
	}
}

// Every worker holding a node of the cluster is enrolled, so the DaemonSet has
// somewhere to schedule.
func TestTheWorkloadEnrollsEveryWorkerItHasANodeOn(t *testing.T) {
	cluster := aSizedCluster()
	objects := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"}},
		cluster, aNodeOn("worker-1"), aNodeOn("worker-2"),
	}
	r := aWorkloadReconciler(t, objects...)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: enrollNamespace, Name: enrollCluster},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, name := range []string{"worker-1", "worker-2"} {
		var fresh corev1.Node
		if err := r.Get(context.Background(), client.ObjectKey{Name: name}, &fresh); err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if got := fresh.Labels[atlaskube.LabelStorageNodeSet]; got != enrollCluster {
			t.Errorf("%s carries %q for the node-set label, want %q",
				name, got, enrollCluster)
		}
	}
}

// A worker with no node of this cluster on it is left alone, so a fleet running
// something else on its other machines is not enrolled into this one.
func TestTheWorkloadLeavesUnrelatedWorkersAlone(t *testing.T) {
	cluster := aSizedCluster()
	objects := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bystander"}},
		cluster, aNodeOn("worker-1"),
	}
	r := aWorkloadReconciler(t, objects...)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: enrollNamespace, Name: enrollCluster},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var bystander corev1.Node
	if err := r.Get(context.Background(), client.ObjectKey{Name: "bystander"}, &bystander); err != nil {
		t.Fatalf("reading the bystander: %v", err)
	}
	if _, enrolled := bystander.Labels[atlaskube.LabelStorageNodeSet]; enrolled {
		t.Errorf("a worker with no node of this cluster was enrolled: %v", bystander.Labels)
	}
}
