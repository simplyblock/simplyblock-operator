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
// the StorageCluster status subresource enabled (markClusterVolumeMoved patches
// StorageCluster status, not VolumeMigration status).
func newVMReconcilerForRealign(t *testing.T, objs ...client.Object) (*VolumeMigrationReconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{&simplyblockv1alpha1.StorageCluster{}}, objs...)
	return &VolumeMigrationReconciler{Client: cl, Scheme: scheme, Recorder: events.NewFakeRecorder(16)}, cl
}

func getClusterByName(t *testing.T, cl client.Client) *simplyblockv1alpha1.StorageCluster {
	t.Helper()
	cr := &simplyblockv1alpha1.StorageCluster{}
	key := types.NamespacedName{Namespace: realignNamespace, Name: realignClusterName}
	if err := cl.Get(context.Background(), key, cr); err != nil {
		t.Fatalf("get cluster %q: %v", realignClusterName, err)
	}
	return cr
}

func TestMarkClusterVolumeMoved_IncrementsGeneration(t *testing.T) {
	cr := testCluster(realignNamespace, realignClusterName, realignClusterUUID)
	r, cl := newVMReconcilerForRealign(t, cr)

	r.markClusterVolumeMoved(context.Background(), realignNamespace, realignClusterUUID)

	if got := ptr.Int64FromOrZero(getClusterByName(t, cl).Status.VolumeMoveGeneration); got != 1 {
		t.Fatalf("generation = %d, want 1", got)
	}
}

// Each completed migration must count, or MinMoves batching can never be reached.
// The superseded boolean was idempotent by design; the counter must not be.
func TestMarkClusterVolumeMoved_EveryMoveCounts(t *testing.T) {
	cr := testCluster(realignNamespace, realignClusterName, realignClusterUUID)
	r, cl := newVMReconcilerForRealign(t, cr)

	for range 5 {
		r.markClusterVolumeMoved(context.Background(), realignNamespace, realignClusterUUID)
	}

	if got := ptr.Int64FromOrZero(getClusterByName(t, cl).Status.VolumeMoveGeneration); got != 5 {
		t.Fatalf("generation = %d, want 5 (one per completed migration)", got)
	}
}

// A move counted while a realignment already covers an earlier generation must push the
// counter past realignedGeneration, which is what leaves the next realignment owed.
func TestMarkClusterVolumeMoved_CountsPastAlreadyRealigned(t *testing.T) {
	cr := testCluster(realignNamespace, realignClusterName, realignClusterUUID)
	cr.Status.VolumeMoveGeneration = ptr.To(int64(7))
	cr.Status.RealignedGeneration = ptr.To(int64(7))
	r, cl := newVMReconcilerForRealign(t, cr)

	r.markClusterVolumeMoved(context.Background(), realignNamespace, realignClusterUUID)

	got := getClusterByName(t, cl)
	if gen := ptr.Int64FromOrZero(got.Status.VolumeMoveGeneration); gen != 8 {
		t.Fatalf("generation = %d, want 8", gen)
	}
	if realigned := ptr.Int64FromOrZero(got.Status.RealignedGeneration); realigned != 7 {
		t.Fatalf("realignedGeneration = %d, want it untouched at 7", realigned)
	}
}

func TestMarkClusterVolumeMoved_NoMatchingClusterLeavesOthersAlone(t *testing.T) {
	// A cluster with a *different* UUID must not be counted against.
	cr := testCluster(realignNamespace, realignClusterName, "some-other-uuid")
	r, cl := newVMReconcilerForRealign(t, cr)

	r.markClusterVolumeMoved(context.Background(), realignNamespace, realignClusterUUID)

	if got := getClusterByName(t, cl).Status.VolumeMoveGeneration; got != nil {
		t.Fatalf("unrelated cluster counted: %d", *got)
	}
}

func TestMarkClusterVolumeMoved_EmptyUUIDIsNoOp(t *testing.T) {
	cr := testCluster(realignNamespace, realignClusterName, realignClusterUUID)
	r, cl := newVMReconcilerForRealign(t, cr)

	// Must not count against any cluster (and must not panic) when the volume carries
	// no resolved cluster UUID.
	r.markClusterVolumeMoved(context.Background(), realignNamespace, "")

	if got := getClusterByName(t, cl).Status.VolumeMoveGeneration; got != nil {
		t.Fatalf("cluster counted for empty UUID: %d", *got)
	}
}
