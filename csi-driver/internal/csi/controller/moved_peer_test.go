// Tests for controller calls on a volume of another site's control plane (a
// split control plane) after it failed over onto this site: the PV keeps the
// handle of the source volume on the other site, which may be cut off, and
// expand, snapshot, and clone reach the clone here through the relationship
// this site's control plane recorded, without asking the other site first.

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/simplyblock/csi-driver/internal/controlplane"
)

// failedOverVolume is this site after the fail-over of movedSourceID: the
// clone movedTargetID here, and the relationship that leads to it.
func failedOverVolume(m *mockSBCLI) {
	m.volumes[movedTargetID] = &mockVolume{UUID: movedTargetID, Name: "clone", Size: 1 << 30}
	rel := moved(movedSourceID, movedTargetID, movedTargetID)
	rel["state"], rel["mode"], rel["target_cluster_id"] = "failed_over", "replication", siteBClusterID
	m.replicationRelationship[movedSourceID] = rel
}

func TestCallsOnAPeerVolumeFailedOverHereReachTheClone(t *testing.T) {
	ctx := context.Background()

	t.Run("expand", func(t *testing.T) {
		local := newMockSBCLI()
		defer local.Close()
		cs := splitSite(t, local, true)
		failedOverVolume(local)

		_, err := cs.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
			VolumeId: movedHandle, CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
		})

		if err != nil {
			t.Fatalf("ControllerExpandVolume: %v (asked the cut-off source's control plane?)", err)
		}
		if got := local.volumes[movedTargetID].Size; got != 4<<30 {
			t.Errorf("clone size = %d, want it grown to 4 GiB", got)
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		local := newMockSBCLI()
		defer local.Close()
		cs := splitSite(t, local, true)
		failedOverVolume(local)

		_, err := cs.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{SourceVolumeId: movedHandle, Name: "snap-1"})

		if err != nil {
			t.Fatalf("CreateSnapshot: %v (asked the cut-off source's control plane?)", err)
		}
		var of string
		for _, s := range local.snapshots {
			of = s.VolUUID
		}
		if of != movedTargetID {
			t.Errorf("the snapshot was taken of %q, want the clone", of)
		}
	})

	t.Run("clone", func(t *testing.T) {
		local := newMockSBCLI()
		defer local.Close()
		cs := splitSite(t, local, true)
		failedOverVolume(local)
		req := basicCreateVolumeRequest("pvc-clone")
		req.Parameters["cluster_id"] = siteBClusterID // this site's storage class
		req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
			Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: movedHandle},
		}}

		if _, err := cs.CreateVolume(ctx, req); err != nil {
			t.Fatalf("CreateVolume: %v (asked the cut-off source's control plane?)", err)
		}
		if local.lastCloneSource != movedTargetID {
			t.Errorf("cloned from %q, want the clone", local.lastCloneSource)
		}
	})
}

// The volume health check (ControllerGetVolume, polled by the external
// health monitor) reports the clone here as healthy. Asking the cut-off
// source instead reported every failed-over PV abnormal until the heal (run
// 20261112-1712-ebs-splitdr).
func TestHealthOfAPeerVolumeFailedOverHereIsTheClones(t *testing.T) {
	local := newMockSBCLI()
	defer local.Close()
	cs := splitSite(t, local, true)
	failedOverVolume(local)

	resp, err := cs.ControllerGetVolume(context.Background(), &csi.ControllerGetVolumeRequest{VolumeId: movedHandle})

	if err != nil {
		t.Fatalf("ControllerGetVolume: %v", err)
	}
	if c := resp.GetStatus().GetVolumeCondition(); c.GetAbnormal() {
		t.Fatalf("volume reported abnormal: %s (asked the cut-off source's control plane?)", c.GetMessage())
	}
	if id := resp.GetVolume().GetVolumeId(); id != movedHandle {
		t.Errorf("volume id = %q, want the handle asked about, %q", id, movedHandle)
	}
}

// The membership watcher reads a failed-over volume's group from the clone
// here. It used to ask the cut-off source once a minute and fail each time
// until the heal (run 20261112-1712-ebs-splitdr).
func TestMembershipOfAPeerVolumeFailedOverHereIsReadFromTheClone(t *testing.T) {
	local := newMockSBCLI()
	defer local.Close()
	splitSite(t, local, true)
	failedOverVolume(local)

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data", Namespace: "gitea", Labels: map[string]string{consistencyGroupLabel: "gitea-cg"},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: "pv-data"},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-data"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: testDriver, VolumeHandle: movedHandle},
		}},
	}
	group := &controlplane.ConsistencyGroupSummary{ID: "gid-here", Name: "gitea-cg"}
	membership := &fakeMembership{
		groupIDByLvol: map[string]string{movedTargetID: group.ID},
		groupsByID:    map[string]*controlplane.ConsistencyGroupSummary{group.ID: group},
	}
	var asked []string
	watcher := &cgMembershipWatcher{
		kube:       fake.NewSimpleClientset(pvc, pv),
		driverName: testDriver,
		clientFor: func(_ context.Context, clusterID, _ string) (membershipClient, error) {
			asked = append(asked, clusterID)
			if clusterID != siteBClusterID {
				return nil, errors.New("cluster " + clusterID + " is cut off")
			}
			return membership, nil
		},
		recorder: record.NewFakeRecorder(16),
	}

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v (clusters asked: %v)", err, asked)
	}
	if len(membership.joined) != 0 || len(membership.detached) != 0 {
		t.Errorf("the clone is already in group %s; joined %v, detached %v", group.ID, membership.joined, membership.detached)
	}
}

func TestAPeerVolumeNotFailedOverHereIsLeftToItsControlPlane(t *testing.T) {
	// No fail-over here: the volume's own control plane serves it. With that
	// one cut off, the expand fails rather than acting on something here.
	local := newMockSBCLI()
	defer local.Close()
	cs := splitSite(t, local, true)

	_, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: movedHandle, CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
	})

	if err == nil {
		t.Fatal("ControllerExpandVolume succeeded for a volume nothing here serves")
	}
}
