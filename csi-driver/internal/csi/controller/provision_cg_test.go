package controller

import (
	"context"
	"encoding/json"
	"testing"

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
func TestCreateVolume_ForwardsConsistencyGroupLabel(t *testing.T) {
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
