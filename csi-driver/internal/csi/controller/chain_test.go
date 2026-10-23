// Controller calls on a volume that moved (design-superclusters.md §8.3): a
// relocated PV keeps the handle of a source volume the move deleted, and its
// expand, snapshot, and clone reach the target through the replication
// relationship, starting at the PV's relocated-to mark when it carries one.
// DeleteVolume deletes the target only of a PV whose mark names the chain's
// active volume.
//
// Both ends of each chain are on the mock's one cluster, which ignores the
// cluster in a URL, as the replication tests beside this file do; the case
// that needs the source cluster gone names a cluster the secret never lists.

package controller

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/kube"
)

const (
	movedSourceID = "c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1"
	movedTargetID = "c2c2c2c2-c2c2-c2c2-c2c2-c2c2c2c2c2c2"
	movedOnwardID = "c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c3"
	movedHandle   = sanityClusterID + ":" + sanityPoolUUID + ":" + movedSourceID
	movedMark     = sanityClusterID + ":" + sanityPoolUUID + ":" + movedTargetID
)

// moved is a relationship a cutover with delete_source leaves: source names
// target, which is active unless active says otherwise.
func moved(source, target, active string) map[string]any {
	return map[string]any{
		"replication_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "direction": "to_target",
		"mode": "migration", "state": "cutover_done", "is_source": true,
		"source_cluster_id": sanityClusterID, "source_lvol_id": source,
		"target_cluster_id": sanityClusterID, "target_pool_id": sanityPoolUUID, "target_lvol_id": target,
		"target_nqn": "nqn.test", "target_ns_id": 1, "active": "target", "active_lvol_id": active,
	}
}

// movedVolume is the mock after a move: the source deleted, the target
// serving, and the relationship between them.
func movedVolume(m *mockSBCLI) {
	m.volumes[movedTargetID] = &mockVolume{UUID: movedTargetID, Name: "moved", Size: 1 << 30}
	m.replicationRelationship[movedSourceID] = moved(movedSourceID, movedTargetID, movedTargetID)
}

// markedPV is the PersistentVolume of the moved volume, marked with mark when
// it is not empty.
func markedPV(mark string) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-moved"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: testDriverName, VolumeHandle: movedHandle},
		}},
	}
	if mark != "" {
		pv.Annotations = map[string]string{kube.AnnoRelocatedTo: mark}
	}
	return pv
}

// U-116: expand, snapshot, and clone from a volume on a moved volume's
// original handle reach the target through the relationship.
func TestCallsOnAMovedVolumesHandleReachTheTarget(t *testing.T) {
	ctx := context.Background()

	t.Run("expand", func(t *testing.T) {
		mock := newMockSBCLI()
		defer mock.Close()
		cs := newTestControllerServer(t, mock)
		movedVolume(mock)

		_, err := cs.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
			VolumeId: movedHandle, CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
		})

		if err != nil {
			t.Fatalf("ControllerExpandVolume: %v", err)
		}
		if got := mock.volumes[movedTargetID].Size; got != 4<<30 {
			t.Errorf("target size = %d, want it grown to 4 GiB", got)
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		mock := newMockSBCLI()
		defer mock.Close()
		cs := newTestControllerServer(t, mock)
		movedVolume(mock)

		_, err := cs.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{SourceVolumeId: movedHandle, Name: "snap-1"})

		if err != nil {
			t.Fatalf("CreateSnapshot: %v", err)
		}
		var of string
		for _, s := range mock.snapshots {
			of = s.VolUUID
		}
		if of != movedTargetID {
			t.Errorf("the snapshot was taken of %q, want the target", of)
		}
	})

	t.Run("clone", func(t *testing.T) {
		mock := newMockSBCLI()
		defer mock.Close()
		cs := newTestControllerServer(t, mock)
		movedVolume(mock)
		req := basicCreateVolumeRequest("pvc-clone")
		req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
			Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: movedHandle},
		}}

		if _, err := cs.CreateVolume(ctx, req); err != nil {
			t.Fatalf("CreateVolume: %v", err)
		}
		if mock.lastCloneSource != movedTargetID {
			t.Errorf("cloned from %q, want the target", mock.lastCloneSource)
		}
	})
}

// U-117: a handle whose volume still exists is not redirected, and the
// relationship endpoint is not called.
func TestAHandleWhoseVolumeExistsIsNotRedirected(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	movedVolume(mock)
	mock.volumes[movedSourceID] = &mockVolume{UUID: movedSourceID, Name: "source", Size: 1 << 30}

	_, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: movedHandle, CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
	})

	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if mock.volumes[movedSourceID].Size != 4<<30 || mock.volumes[movedTargetID].Size != 1<<30 {
		t.Error("the call was redirected away from a volume that exists")
	}
	if mock.relationshipReads != 0 {
		t.Errorf("relationship reads = %d, want none", mock.relationshipReads)
	}
}

// U-192: a marked PV resolves from its mark, and follows a later fail-over of
// the destination; the source's own relationship is never needed.
func TestAMarkedVolumeResolvesFromItsMark(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	cs.kubeClient = k8sfake.NewClientset(markedPV(movedMark))
	mock.volumes[movedOnwardID] = &mockVolume{UUID: movedOnwardID, Name: "failed-over", Size: 1 << 30}
	mock.replicationRelationship[movedTargetID] = moved(movedTargetID, movedOnwardID, movedOnwardID)

	_, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: movedHandle, CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
	})

	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if mock.volumes[movedOnwardID].Size != 4<<30 {
		t.Error("the call did not follow the destination's fail-over from the mark")
	}
}

// U-189: DeleteVolume of a PV whose mark names the chain's active volume
// deletes it.
func TestDeletingAMarkedVolumeDeletesTheTarget(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	movedVolume(mock)
	cs.kubeClient = k8sfake.NewClientset(markedPV(movedMark))

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: movedHandle}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mock.volumes[movedTargetID]; ok {
		t.Error("the target the mark names was left behind")
	}
}

// U-190: the same chain without a mark is the disaster-recovery case, and
// deletes no active volume.
func TestDeletingAnUnmarkedVolumeLeavesTheActiveOne(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	movedVolume(mock)
	cs.kubeClient = k8sfake.NewClientset(markedPV(""))

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: movedHandle}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mock.volumes[movedTargetID]; !ok {
		t.Error("the active volume was deleted through an unmarked handle")
	}
}

// U-191: a mark naming a volume other than the active one deletes no active
// volume.
func TestAMarkThatIsNoLongerActiveDeletesNoActiveVolume(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	movedVolume(mock)
	mock.volumes[movedOnwardID] = &mockVolume{UUID: movedOnwardID, Name: "failed-over", Size: 1 << 30}
	mock.replicationRelationship[movedTargetID] = moved(movedTargetID, movedOnwardID, movedOnwardID)
	cs.kubeClient = k8sfake.NewClientset(markedPV(movedMark))

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: movedHandle}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mock.volumes[movedOnwardID]; !ok {
		t.Error("the active volume after the destination's fail-over was deleted")
	}
}

// U-193: a marked PV whose source cluster has left the secret deletes the
// destination rather than reporting the volume deleted.
func TestAMarkedVolumeWhoseSourceClusterIsGoneDeletesTheTarget(t *testing.T) {
	const goneCluster = "deadbeef-dead-beef-dead-beefdeadbeef"
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	movedVolume(mock)
	pv := markedPV(movedMark)
	pv.Spec.CSI.VolumeHandle = goneCluster + ":" + sanityPoolUUID + ":" + movedSourceID
	cs.kubeClient = k8sfake.NewClientset(pv)

	if _, err := cs.DeleteVolume(context.Background(),
		&csi.DeleteVolumeRequest{VolumeId: pv.Spec.CSI.VolumeHandle}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mock.volumes[movedTargetID]; ok {
		t.Error("the destination was left behind because the source cluster is gone")
	}
}
