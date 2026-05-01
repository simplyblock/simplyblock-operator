package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func lvol(ns, name, lvolID string) simplyblockv1alpha1.AttachedLvol {
	return simplyblockv1alpha1.AttachedLvol{PVCNamespace: ns, PVCName: name, LvolID: lvolID}
}

func TestDiffAttachments_NoChange(t *testing.T) {
	a := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	b := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	if got := diffAttachments(a, b); len(got) != 0 {
		t.Fatalf("expected no diff, got %v", got)
	}
}

func TestDiffAttachments_NewAttachment(t *testing.T) {
	desired := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-aaa"),
		lvol("ns", "pvc2", "lvol-bbb"),
	}
	current := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	got := diffAttachments(desired, current)
	if len(got) != 1 || got[0].PVCName != "pvc2" {
		t.Fatalf("expected pvc2 to attach, got %v", got)
	}
}

func TestDiffAttachments_RemovedAttachment(t *testing.T) {
	desired := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-aaa")}
	current := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-aaa"),
		lvol("ns", "pvc2", "lvol-bbb"),
	}
	got := diffAttachments(current, desired)
	if len(got) != 1 || got[0].PVCName != "pvc2" {
		t.Fatalf("expected pvc2 to detach, got %v", got)
	}
}

// TestDiffAttachments_ReboundPVC is the regression test for the bug where
// a PVC rebound to a new lvol was invisible to the diff (same ns/name, different
// lvolID). The old lvol must appear in toDetach and the new one in toAttach.
func TestDiffAttachments_ReboundPVC(t *testing.T) {
	desired := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-new")}
	current := []simplyblockv1alpha1.AttachedLvol{lvol("ns", "pvc1", "lvol-old")}

	toAttach := diffAttachments(desired, current)
	toDetach := diffAttachments(current, desired)

	if len(toAttach) != 1 || toAttach[0].LvolID != "lvol-new" {
		t.Fatalf("expected lvol-new in toAttach, got %v", toAttach)
	}
	if len(toDetach) != 1 || toDetach[0].LvolID != "lvol-old" {
		t.Fatalf("expected lvol-old in toDetach, got %v", toDetach)
	}
}

func TestRemoveAttachment(t *testing.T) {
	slice := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-aaa"),
		lvol("ns", "pvc2", "lvol-bbb"),
	}
	result := removeAttachment(slice, lvol("ns", "pvc1", "lvol-aaa"))
	if len(result) != 1 || result[0].PVCName != "pvc2" {
		t.Fatalf("expected only pvc2 remaining, got %v", result)
	}
}

// Removing by PVC key alone must not drop an entry that shares the name but has
// a different lvolID (e.g. after a rebind, the new attachment must survive).
func TestRemoveAttachment_DoesNotMatchDifferentLvol(t *testing.T) {
	slice := []simplyblockv1alpha1.AttachedLvol{
		lvol("ns", "pvc1", "lvol-new"),
	}
	result := removeAttachment(slice, lvol("ns", "pvc1", "lvol-old"))
	if len(result) != 1 {
		t.Fatalf("expected new attachment to survive removal of old lvolID, got %v", result)
	}
}

// ---- resolvePVCLvolID tests ----

const (
	resolveTestClusterUUID = "cluster-uuid-1"
	resolveTestLvolID      = "lvol-resolve-1"
	resolveTestPVName      = "pv-resolve-1"
	resolveTestPVCName     = "pvc-resolve-1"
	resolveTestNamespace   = "default"
)

func resolveTestObjects(clusterUUID, lvolID string, annotations map[string]string) (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: resolveTestPVName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					VolumeHandle: clusterUUID + ":pool-a:" + lvolID,
				},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        resolveTestPVCName,
			Namespace:   resolveTestNamespace,
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: resolveTestPVName},
	}
	return pv, pvc
}

func TestResolvePVCLvolID_FromHandle(t *testing.T) {
	pv, pvc := resolveTestObjects(resolveTestClusterUUID, resolveTestLvolID, nil)
	scheme := newTestScheme(t, corev1.AddToScheme)
	k8s := newTestClient(t, scheme, nil, pv, pvc)

	got, err := resolvePVCLvolID(context.Background(), k8s, pvc, resolveTestClusterUUID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != resolveTestLvolID {
		t.Fatalf("expected %s, got %s", resolveTestLvolID, got)
	}
}

func TestResolvePVCLvolID_AnnotationMatchesHandle(t *testing.T) {
	ann := map[string]string{pvcLvolIDAnnotation: resolveTestLvolID}
	pv, pvc := resolveTestObjects(resolveTestClusterUUID, resolveTestLvolID, ann)
	scheme := newTestScheme(t, corev1.AddToScheme)
	k8s := newTestClient(t, scheme, nil, pv, pvc)

	got, err := resolvePVCLvolID(context.Background(), k8s, pvc, resolveTestClusterUUID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != resolveTestLvolID {
		t.Fatalf("expected %s, got %s", resolveTestLvolID, got)
	}
}

// TestResolvePVCLvolID_AnnotationMismatch is the core regression test: when the
// simplybk/lvol-id annotation disagrees with the PV CSI volume handle the call
// must return an error, not silently use the (potentially stale) annotation.
func TestResolvePVCLvolID_AnnotationMismatch(t *testing.T) {
	ann := map[string]string{pvcLvolIDAnnotation: "lvol-stale-annotation"}
	pv, pvc := resolveTestObjects(resolveTestClusterUUID, resolveTestLvolID, ann)
	scheme := newTestScheme(t, corev1.AddToScheme)
	k8s := newTestClient(t, scheme, nil, pv, pvc)

	_, err := resolvePVCLvolID(context.Background(), k8s, pvc, resolveTestClusterUUID)
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error message should mention mismatch, got: %v", err)
	}
}

func TestResolvePVCLvolID_WrongCluster(t *testing.T) {
	pv, pvc := resolveTestObjects("other-cluster-uuid", resolveTestLvolID, nil)
	scheme := newTestScheme(t, corev1.AddToScheme)
	k8s := newTestClient(t, scheme, nil, pv, pvc)

	_, err := resolvePVCLvolID(context.Background(), k8s, pvc, resolveTestClusterUUID)
	if err == nil {
		t.Fatal("expected cluster mismatch error, got nil")
	}
}

func TestResolvePVCLvolID_Unbound(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: resolveTestPVCName, Namespace: resolveTestNamespace},
		Spec:       corev1.PersistentVolumeClaimSpec{},
	}
	scheme := newTestScheme(t, corev1.AddToScheme)
	k8s := newTestClient(t, scheme, nil, pvc)

	_, err := resolvePVCLvolID(context.Background(), k8s, pvc, resolveTestClusterUUID)
	if err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("expected 'not bound' error, got: %v", err)
	}
}
