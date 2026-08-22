package volumemigration

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/ctrltest"
)

// The cluster the counter is kept on. Named to match the rebalancing tests on the
// reading end of it, which exercise the same three values.
const (
	realignNamespace   = "sb"
	realignClusterName = "cluster-a"
	realignClusterUUID = "cluster-uuid-a"
)

// newMoveCounterClient builds a fake client with the StorageCluster status subresource
// enabled — MarkVolumeMoved patches StorageCluster status, not VolumeMigration status.
func newMoveCounterClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := ctrltest.NewScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	return ctrltest.NewClient(t, scheme, []client.Object{&simplyblockv1alpha1.StorageCluster{}}, objs...)
}

func moveCounterCluster(uuid string) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: realignClusterName, Namespace: realignNamespace},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: uuid},
	}
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

func TestMarkVolumeMoved_IncrementsGeneration(t *testing.T) {
	cl := newMoveCounterClient(t, moveCounterCluster(realignClusterUUID))

	MarkVolumeMoved(context.Background(), cl, realignNamespace, realignClusterUUID)

	if got := ptr.Int64FromOrZero(getClusterByName(t, cl).Status.VolumeMoveGeneration); got != 1 {
		t.Fatalf("generation = %d, want 1", got)
	}
}

// Each completed migration must count, or MinMoves batching can never be reached.
// The superseded boolean was idempotent by design; the counter must not be.
func TestMarkVolumeMoved_EveryMoveCounts(t *testing.T) {
	cl := newMoveCounterClient(t, moveCounterCluster(realignClusterUUID))

	for range 5 {
		MarkVolumeMoved(context.Background(), cl, realignNamespace, realignClusterUUID)
	}

	if got := ptr.Int64FromOrZero(getClusterByName(t, cl).Status.VolumeMoveGeneration); got != 5 {
		t.Fatalf("generation = %d, want 5 (one per completed migration)", got)
	}
}

// A move counted while a realignment already covers an earlier generation must push the
// counter past realignedGeneration, which is what leaves the next realignment owed.
func TestMarkVolumeMoved_CountsPastAlreadyRealigned(t *testing.T) {
	cr := moveCounterCluster(realignClusterUUID)
	cr.Status.VolumeMoveGeneration = ptr.To(int64(7))
	cr.Status.RealignedGeneration = ptr.To(int64(7))
	cl := newMoveCounterClient(t, cr)

	MarkVolumeMoved(context.Background(), cl, realignNamespace, realignClusterUUID)

	got := getClusterByName(t, cl)
	if gen := ptr.Int64FromOrZero(got.Status.VolumeMoveGeneration); gen != 8 {
		t.Fatalf("generation = %d, want 8", gen)
	}
	if realigned := ptr.Int64FromOrZero(got.Status.RealignedGeneration); realigned != 7 {
		t.Fatalf("realignedGeneration = %d, want it untouched at 7", realigned)
	}
}

func TestMarkVolumeMoved_NoMatchingClusterLeavesOthersAlone(t *testing.T) {
	// A cluster with a *different* UUID must not be counted against.
	cl := newMoveCounterClient(t, moveCounterCluster("some-other-uuid"))

	MarkVolumeMoved(context.Background(), cl, realignNamespace, realignClusterUUID)

	if got := getClusterByName(t, cl).Status.VolumeMoveGeneration; got != nil {
		t.Fatalf("unrelated cluster counted: %d", *got)
	}
}

func TestMarkVolumeMoved_EmptyUUIDIsNoOp(t *testing.T) {
	cl := newMoveCounterClient(t, moveCounterCluster(realignClusterUUID))

	// Must not count against any cluster (and must not panic) when the volume carries
	// no resolved cluster UUID.
	MarkVolumeMoved(context.Background(), cl, realignNamespace, "")

	if got := getClusterByName(t, cl).Status.VolumeMoveGeneration; got != nil {
		t.Fatalf("cluster counted for empty UUID: %d", *got)
	}
}
