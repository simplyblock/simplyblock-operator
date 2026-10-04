package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// sentConsistencyGroup drives one CreateVolume against the mock control plane
// with the given PVC and returns the consistency_group field of the create POST.
func sentConsistencyGroup(t *testing.T, pvc *corev1.PersistentVolumeClaim) string {
	t.Helper()
	mock := newMockSBCLI()
	t.Cleanup(mock.Close)
	cs := newTestControllerServer(t, mock)
	cs.kubeClient = fake.NewSimpleClientset(pvc)

	if _, err := cs.CreateVolume(context.Background(), placementCreateVolumeRequest("cg-vol")); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if len(mock.createVolumeBodies) != 1 {
		t.Fatalf("expected exactly 1 create POST, got %d", len(mock.createVolumeBodies))
	}
	var body struct {
		ConsistencyGroup string `json:"consistency_group"`
	}
	if err := json.Unmarshal(mock.createVolumeBodies[0], &body); err != nil {
		t.Fatalf("unmarshal create body %s: %v", mock.createVolumeBodies[0], err)
	}
	return body.ConsistencyGroup
}

func cgLabeledPVC(labels map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      placementPVCName,
			Namespace: placementPVCNamespace,
			Labels:    labels,
		},
	}
}

// The provisioner must forward a PVC's consistency-group label so the volume
// joins the backend group at creation (design §4.1); without this the group is
// never populated and the GroupController rejects the snapshot as a non-member.
func TestCreateVolume_SendsConsistencyGroupLabel(t *testing.T) {
	got := sentConsistencyGroup(t, cgLabeledPVC(map[string]string{consistencyGroupLabel: "db-group"}))
	if got != "db-group" {
		t.Fatalf("consistency_group = %q, want %q", got, "db-group")
	}
}

// An unlabeled PVC sends no consistency_group, so a plain volume is never joined
// to a group (omitempty keeps the field off the wire).
func TestCreateVolume_NoLabelOmitsConsistencyGroup(t *testing.T) {
	if got := sentConsistencyGroup(t, cgLabeledPVC(nil)); got != "" {
		t.Fatalf("consistency_group = %q, want empty", got)
	}
}

// sentCloneConsistencyGroup drives one CreateVolume whose content source is a
// snapshot (the restore path) and returns the consistency_group field of the
// clone POST.
func sentCloneConsistencyGroup(t *testing.T, pvc *corev1.PersistentVolumeClaim) string {
	t.Helper()
	mock := newMockSBCLI()
	t.Cleanup(mock.Close)
	cs := newTestControllerServer(t, mock)
	cs.kubeClient = fake.NewSimpleClientset(pvc)

	const sourceSnapshotID = "22222222-2222-2222-2222-222222222222"
	mock.snapshots[sourceSnapshotID] = &mockSnapshot{
		UUID: sourceSnapshotID, Name: "src-snap", Size: 1 << 30,
	}

	req := placementCreateVolumeRequest("cg-clone")
	req.VolumeContentSource = &csi.VolumeContentSource{
		Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{
				SnapshotId: fmt.Sprintf("%s:%s:%s",
					sanityClusterID, sanityPoolUUID, sourceSnapshotID),
			},
		},
	}
	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume (clone): %v", err)
	}
	if len(mock.createVolumeBodies) != 1 {
		t.Fatalf("expected exactly 1 clone POST, got %d", len(mock.createVolumeBodies))
	}
	var body struct {
		ConsistencyGroup string `json:"consistency_group"`
	}
	if err := json.Unmarshal(mock.createVolumeBodies[0], &body); err != nil {
		t.Fatalf("unmarshal clone body %s: %v", mock.createVolumeBodies[0], err)
	}
	return body.ConsistencyGroup
}

// A restore PVC carrying the consistency-group label must forward it on the
// CLONE body too, so the clones form a new group at provisioning (design §7.2).
// Without this the group-forming restore silently produces ungrouped clones.
func TestCloneVolume_SendsConsistencyGroupLabel(t *testing.T) {
	got := sentCloneConsistencyGroup(t, cgLabeledPVC(map[string]string{consistencyGroupLabel: "db-restored"}))
	if got != "db-restored" {
		t.Fatalf("clone consistency_group = %q, want %q", got, "db-restored")
	}
}

// An unlabeled restore PVC keeps the clone group-neutral (design §7.1).
func TestCloneVolume_NoLabelOmitsConsistencyGroup(t *testing.T) {
	if got := sentCloneConsistencyGroup(t, cgLabeledPVC(nil)); got != "" {
		t.Fatalf("clone consistency_group = %q, want empty", got)
	}
}
