// A node deleted after its cluster: the StorageCluster's finalizer is
// released only once the control plane deleted the backend cluster with
// force, which shut down and deleted every node of it, so a node whose
// StorageCluster no longer exists has no backend node left to drain.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

func TestANodeWhoseClusterIsGoneIsReleasedWithoutARemoval(t *testing.T) {
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	node := anOpsNode()
	node.Finalizers = []string{NodeFinalizer}
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNode{}, &simplyblockv1alpha2.StorageNodeOps{}).
		WithIndex(&simplyblockv1alpha2.StorageNode{}, clusterRefField,
			func(o client.Object) []string {
				return []string{o.(*simplyblockv1alpha2.StorageNode).Spec.ClusterRef}
			}).
		Build()
	// A nil control plane: a teardown that asks it anything panics.
	r := &StorageNodeReconciler{
		Client: apiClient, Scheme: scheme, Recorder: events.NewFakeRecorder(16),
		Workload: &Workload{Client: apiClient},
	}
	if err := apiClient.Delete(context.Background(), node); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName},
	}); err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	var left simplyblockv1alpha2.StorageNode
	err := apiClient.Get(context.Background(), client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}, &left)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the node is still there (%v), finalizers %v", err, left.Finalizers)
	}
	var ops simplyblockv1alpha2.StorageNodeOpsList
	if err := apiClient.List(context.Background(), &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops.Items) != 0 {
		t.Errorf("a removal was raised for a node whose cluster is gone: %s", ops.Items[0].Name)
	}
}
