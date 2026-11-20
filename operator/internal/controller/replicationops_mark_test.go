// Unit tests for markMigratedVolumes: a --delete-source migration records on
// each finished volume's PersistentVolume where its data lives now, so the CSI
// driver deletes that volume with the claim.

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

const (
	markClusterA = "aaaaaaaa-0000-0000-0000-00000000000a"
	markClusterB = "bbbbbbbb-0000-0000-0000-00000000000b"
	markVolA     = "11111111-1111-1111-1111-111111111111"
	markVolB     = "22222222-2222-2222-2222-222222222222"
	markVolA2    = "33333333-3333-3333-3333-333333333333"
)

var (
	markOnA  = lvol.Handle{ClusterID: markClusterA, PoolRef: "pool-a", VolumeID: markVolA}
	markOnB  = lvol.Handle{ClusterID: markClusterB, PoolRef: "pool-b", VolumeID: markVolB}
	markBack = lvol.Handle{ClusterID: markClusterA, PoolRef: "pool-a", VolumeID: markVolA2}
)

// moves is a relationship lookup in which each listed volume's data moved to
// the next handle (the cutover of a --delete-source migration).
func moves(next map[string]lvol.Handle) controlplane.RelationshipLookup {
	return func(_ context.Context, h lvol.Handle) (controlplane.Relationship, error) {
		to, ok := next[h.VolumeID]
		if !ok {
			return controlplane.Relationship{}, errs.ErrNotFound
		}
		return controlplane.Relationship{IsSource: true, TargetClusterID: to.ClusterID,
			TargetPoolID: to.PoolRef, TargetLvolID: to.VolumeID, ActiveLvolID: to.VolumeID}, nil
	}
}

func markFixture(annotations map[string]string) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "vol-1", Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-1"},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1", Annotations: annotations},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.simplyblock.io", VolumeHandle: markOnA.String()},
		}},
	}
	return pvc, pv
}

func markOf(t *testing.T, cl client.Client) string {
	t.Helper()
	pv := &corev1.PersistentVolume{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "pv-1"}, pv); err != nil {
		t.Fatalf("get pv: %v", err)
	}
	return pv.Annotations[kube.AnnoRelocatedTo]
}

func TestMarkMigratedVolume_RecordsTheTargetAfterTheCutover(t *testing.T) {
	pvc, pv := markFixture(nil)
	r, cl := newOpsReplReconciler(t, pvc, pv)

	err := r.markMigratedVolume(context.Background(), "default", "vol-1",
		moves(map[string]lvol.Handle{markVolA: markOnB}))
	if err != nil {
		t.Fatal(err)
	}
	if got := markOf(t, cl); got != markOnB.String() {
		t.Errorf("relocated-to = %q, want the volume on the target %q", got, markOnB.String())
	}
}

func TestMarkMigratedVolume_TheReturnTripMovesTheMarkOn(t *testing.T) {
	// The first leg left the mark on B; the volume then migrated back to A.
	pvc, pv := markFixture(map[string]string{kube.AnnoRelocatedTo: markOnB.String()})
	r, cl := newOpsReplReconciler(t, pvc, pv)

	err := r.markMigratedVolume(context.Background(), "default", "vol-1",
		moves(map[string]lvol.Handle{markVolB: markBack}))
	if err != nil {
		t.Fatal(err)
	}
	if got := markOf(t, cl); got != markBack.String() {
		t.Errorf("relocated-to = %q, want the volume the return trip created %q", got, markBack.String())
	}
}

func TestMarkMigratedVolume_AVolumeThatDidNotMoveGetsNoMark(t *testing.T) {
	pvc, pv := markFixture(nil)
	r, cl := newOpsReplReconciler(t, pvc, pv)

	if err := r.markMigratedVolume(context.Background(), "default", "vol-1", moves(nil)); err != nil {
		t.Fatal(err)
	}
	if got := markOf(t, cl); got != "" {
		t.Errorf("relocated-to = %q, want none for a volume still on its own handle", got)
	}
}

func TestMarkMigratedVolume_AGoneClaimIsNotAnError(t *testing.T) {
	r, _ := newOpsReplReconciler(t)
	if err := r.markMigratedVolume(context.Background(), "default", "vol-1", moves(nil)); err != nil {
		t.Errorf("err = %v, want nil for a claim that no longer exists", err)
	}
}

func TestMarkMigratedVolumes_OnlyForDeleteSourceAndFinishedVolumes(t *testing.T) {
	// No control plane is reachable here: the skips must not ask it anything.
	pvc, pv := markFixture(nil)
	r, cl := newOpsReplReconciler(t, pvc, pv)
	ops := migrationOps("cluster", markClusterA)

	finished := []simplyblockv1alpha1.ReplicationOpsResult{
		{PVCRef: "vol-1", Status: string(simplyblockv1alpha1.ReplicationOpsResultSucceeded)},
	}
	r.markMigratedVolumes(context.Background(), ops, nil, finished) // DeleteSource is false
	ops.Spec.DeleteSource = true
	pending := []simplyblockv1alpha1.ReplicationOpsResult{
		{PVCRef: "vol-1", Status: string(simplyblockv1alpha1.ReplicationOpsResultPending)},
	}
	r.markMigratedVolumes(context.Background(), ops, nil, pending)

	if got := markOf(t, cl); got != "" {
		t.Errorf("relocated-to = %q, want none: the source is kept, or the volume has not cut over", got)
	}
}
