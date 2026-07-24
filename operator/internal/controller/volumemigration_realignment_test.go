package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// newVMReconcilerForRealign builds a VolumeMigrationReconciler whose fake client has
// the StorageCluster status subresource enabled (markClusterPendingRealignment patches
// StorageCluster status, not VolumeMigration status).
func newVMReconcilerForRealign(t *testing.T, objs ...client.Object) (*VolumeMigrationReconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{&simplyblockv1alpha1.StorageCluster{}}, objs...)
	return &VolumeMigrationReconciler{Client: cl, Scheme: scheme, Recorder: events.NewFakeRecorder(16)}, cl
}

func getClusterByName(t *testing.T, cl client.Client, name string) *simplyblockv1alpha1.StorageCluster {
	t.Helper()
	cr := &simplyblockv1alpha1.StorageCluster{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: realignNamespace, Name: name}, cr); err != nil {
		t.Fatalf("get cluster %q: %v", name, err)
	}
	return cr
}

func TestMarkClusterPendingRealignment_SetsFlag(t *testing.T) {
	cr := testCluster(realignNamespace, realignClusterName, realignClusterUUID)
	r, cl := newVMReconcilerForRealign(t, cr)

	r.markClusterPendingRealignment(context.Background(), realignNamespace, realignClusterUUID)

	got := getClusterByName(t, cl, realignClusterName)
	if got.Status.PendingDataRealignment == nil || !*got.Status.PendingDataRealignment {
		t.Fatalf("pending flag = %v, want true", got.Status.PendingDataRealignment)
	}
}

func TestMarkClusterPendingRealignment_AlreadyFlaggedIsIdempotent(t *testing.T) {
	cr := testCluster(realignNamespace, realignClusterName, realignClusterUUID)
	cr.Status.PendingDataRealignment = ptr.To(true)
	r, cl := newVMReconcilerForRealign(t, cr)

	r.markClusterPendingRealignment(context.Background(), realignNamespace, realignClusterUUID)

	got := getClusterByName(t, cl, realignClusterName)
	if got.Status.PendingDataRealignment == nil || !*got.Status.PendingDataRealignment {
		t.Fatalf("pending flag = %v, want true", got.Status.PendingDataRealignment)
	}
}

func TestMarkClusterPendingRealignment_NoMatchingClusterLeavesOthersAlone(t *testing.T) {
	// A cluster with a *different* UUID must not be flagged.
	cr := testCluster(realignNamespace, realignClusterName, "some-other-uuid")
	r, cl := newVMReconcilerForRealign(t, cr)

	r.markClusterPendingRealignment(context.Background(), realignNamespace, realignClusterUUID)

	got := getClusterByName(t, cl, realignClusterName)
	if got.Status.PendingDataRealignment != nil {
		t.Fatalf("unrelated cluster flagged: %v", *got.Status.PendingDataRealignment)
	}
}

func TestMarkClusterPendingRealignment_EmptyUUIDIsNoOp(t *testing.T) {
	cr := testCluster(realignNamespace, realignClusterName, realignClusterUUID)
	r, cl := newVMReconcilerForRealign(t, cr)

	// Must not flag any cluster (and must not panic) when the volume carries no
	// resolved cluster UUID.
	r.markClusterPendingRealignment(context.Background(), realignNamespace, "")

	got := getClusterByName(t, cl, realignClusterName)
	if got.Status.PendingDataRealignment != nil {
		t.Fatalf("cluster flagged for empty UUID: %v", *got.Status.PendingDataRealignment)
	}
}
