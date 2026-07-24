package kube

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/errs"
)

// ── LiveResolver ──────────────────────────────────────────────────────────────

func TestLiveResolver_StorageClassByName(t *testing.T) {
	cs := kfake.NewSimpleClientset(sc("sb", DriverName, map[string]string{ParamMaxNamespacePerSubsys: "4"}))
	r := NewLiveResolver(cs)

	got, err := r.StorageClassByName(context.Background(), "sb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "sb" {
		t.Errorf("got %q, want sb", got.Name)
	}

	if _, err := r.StorageClassByName(context.Background(), "missing"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing class: err = %v, want ErrNotFound", err)
	}
}

func TestLiveResolver_PVByVolumeHandle(t *testing.T) {
	managed := managedPV("c:p:v", "sb")
	managed.Name = "pv-managed"
	foreign := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-foreign"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "other.csi", VolumeHandle: "x:y:z"},
		}},
	}
	r := NewLiveResolver(kfake.NewSimpleClientset(managed, foreign))

	got, err := r.PVByVolumeHandle(context.Background(), "c:p:v")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "pv-managed" {
		t.Errorf("got %q, want pv-managed", got.Name)
	}

	if _, err := r.PVByVolumeHandle(context.Background(), "no:such:handle"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing handle: err = %v, want ErrNotFound", err)
	}
}

func TestLiveResolver_PVForClaim(t *testing.T) {
	pv := managedPV("c:p:v", "sb")
	pv.Name = "pv1"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim1", Namespace: "app"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv1"},
	}
	r := NewLiveResolver(kfake.NewSimpleClientset(pv, pvc))

	got, err := r.PVForClaim(context.Background(), types.NamespacedName{Namespace: "app", Name: "claim1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "pv1" {
		t.Errorf("got %q, want pv1", got.Name)
	}

	if _, err := r.PVForClaim(context.Background(), types.NamespacedName{Namespace: "app", Name: "gone"}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing claim: err = %v, want ErrNotFound", err)
	}
}

// ── InformerResolver StorageClass ─────────────────────────────────────────────

func TestInformerResolver_StorageClassByName_ManagedOnly(t *testing.T) {
	f := informers.NewSharedInformerFactory(kfake.NewSimpleClientset(), 0)
	scInf := f.Storage().V1().StorageClasses().Informer()
	r, err := NewResolver(ResolverConfig{
		PersistentVolumes:      f.Core().V1().PersistentVolumes().Informer(),
		PersistentVolumeClaims: f.Core().V1().PersistentVolumeClaims().Informer(),
		StorageClasses:         scInf,
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	// Populate the informer store directly (no need to start it): AddIndexers
	// ran in NewResolver, so additions are indexed as they land.
	if err := scInf.GetStore().Add(sc("sb", DriverName, map[string]string{ParamMaxNamespacePerSubsys: "8"})); err != nil {
		t.Fatal(err)
	}
	if err := scInf.GetStore().Add(sc("foreign", "other.csi.driver", nil)); err != nil {
		t.Fatal(err)
	}

	got, err := r.StorageClassByName(context.Background(), "sb")
	if err != nil {
		t.Fatalf("managed class: unexpected error: %v", err)
	}
	if got.Name != "sb" {
		t.Errorf("got %q, want sb", got.Name)
	}

	// A foreign class is in the store but not indexed, so it never resolves —
	// the informer only surfaces simplyblock-provisioned StorageClasses.
	if _, err := r.StorageClassByName(context.Background(), "foreign"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("foreign class: err = %v, want ErrNotFound", err)
	}
}

func TestInformerResolver_StorageClassByName_NotConfigured(t *testing.T) {
	f := informers.NewSharedInformerFactory(kfake.NewSimpleClientset(), 0)
	r, err := NewResolver(ResolverConfig{
		PersistentVolumes:      f.Core().V1().PersistentVolumes().Informer(),
		PersistentVolumeClaims: f.Core().V1().PersistentVolumeClaims().Informer(),
		// StorageClasses intentionally omitted.
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if _, err := r.StorageClassByName(context.Background(), "sb"); !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("no SC informer: err = %v, want ErrUnsupported", err)
	}
}
