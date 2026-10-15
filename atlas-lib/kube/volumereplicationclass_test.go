package kube

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	kfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/simplyblock/atlas/errs"
)

// newUnstructuredInformer is a never-started SharedIndexInformer over unstructured
// objects, seeded directly in tests through its store.
func newUnstructuredInformer() cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
}

func vrClassUnstructured(name, provisioner, method string) *unstructured.Unstructured {
	params := map[string]interface{}{}
	if method != "" {
		params[ReplicationMethodParam] = method
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "replication.storage.openshift.io/v1alpha1",
		"kind":       "VolumeReplicationClass",
		"metadata":   map[string]interface{}{"name": name},
		"spec": map[string]interface{}{
			"provisioner": provisioner,
			"parameters":  params,
		},
	}}
}

func TestVolumeReplicationClassNameKeys(t *testing.T) {
	sync := vrClassUnstructured("sb-sync-a", DriverName, ReplicationMethodSync)
	if got := VolumeReplicationClassNameKeys(sync); len(got) != 1 || got[0] != "sb-sync-a" {
		t.Errorf("keys = %v, want [sb-sync-a]", got)
	}

	// A class provisioned by a foreign driver gets no key, so it never resolves.
	foreign := vrClassUnstructured("other", "other.csi", ReplicationMethodSync)
	if got := VolumeReplicationClassNameKeys(foreign); got != nil {
		t.Errorf("foreign provisioner: keys = %v, want nil", got)
	}
}

func TestVolumeReplicationClassRefFrom(t *testing.T) {
	got := VolumeReplicationClassRefFrom(vrClassUnstructured("sb-sync-a", DriverName, ReplicationMethodSync))
	want := VolumeReplicationClassRef{Name: "sb-sync-a", Provisioner: DriverName, Method: ReplicationMethodSync}
	if got != want {
		t.Errorf("ref = %+v, want %+v", got, want)
	}
	if !got.IsSync() {
		t.Error("IsSync() = false, want true for method sync")
	}
	if VolumeReplicationClassRefFrom(vrClassUnstructured("a", DriverName, "async")).IsSync() {
		t.Error("IsSync() = true for method async")
	}
}

func newVRClassResolver(t *testing.T, vrInf, vrClassInf cache.SharedIndexInformer) *InformerResolver {
	t.Helper()
	kf := informers.NewSharedInformerFactory(kfake.NewSimpleClientset(), 0)
	r, err := NewResolver(ResolverConfig{
		PersistentVolumes:        kf.Core().V1().PersistentVolumes().Informer(),
		PersistentVolumeClaims:   kf.Core().V1().PersistentVolumeClaims().Informer(),
		VolumeReplications:       vrInf,
		VolumeReplicationClasses: vrClassInf,
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func TestInformerResolver_VolumeReplicationClassByName(t *testing.T) {
	vrClassInf := newUnstructuredInformer()
	r := newVRClassResolver(t, newUnstructuredInformer(), vrClassInf)
	if err := vrClassInf.GetStore().Add(vrClassUnstructured("sb-sync-a", DriverName, ReplicationMethodSync)); err != nil {
		t.Fatal(err)
	}
	if err := vrClassInf.GetStore().Add(vrClassUnstructured("foreign", "other.csi", ReplicationMethodSync)); err != nil {
		t.Fatal(err)
	}

	got, err := r.VolumeReplicationClassByName(context.Background(), "sb-sync-a")
	if err != nil {
		t.Fatalf("VolumeReplicationClassByName: %v", err)
	}
	if got.Name != "sb-sync-a" || !got.IsSync() {
		t.Errorf("ref = %+v", got)
	}

	// A foreign class is in the store but not indexed, so it never resolves.
	if _, err := r.VolumeReplicationClassByName(context.Background(), "foreign"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("foreign class: err = %v, want ErrNotFound", err)
	}
	if _, err := r.VolumeReplicationClassByName(context.Background(), "missing"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing class: err = %v, want ErrNotFound", err)
	}
}

func TestInformerResolver_VolumeReplicationClassByName_NotConfigured(t *testing.T) {
	r := newVRResolver(t, newUnstructuredInformer()) // VR informer only, no VRClass
	if _, err := r.VolumeReplicationClassByName(context.Background(), "sb-sync-a"); !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("no VRClass informer: err = %v, want ErrUnsupported", err)
	}
}

func TestNewSyncResolver(t *testing.T) {
	vrInf, vrClassInf := newUnstructuredInformer(), newUnstructuredInformer()
	r, err := NewSyncResolver(vrInf, vrClassInf)
	if err != nil {
		t.Fatalf("NewSyncResolver: %v", err)
	}
	if err := vrInf.GetStore().Add(vrUnstructured("vr1", "app", "claim1", "sb-sync-a", "primary")); err != nil {
		t.Fatal(err)
	}
	if err := vrClassInf.GetStore().Add(vrClassUnstructured("sb-sync-a", DriverName, ReplicationMethodSync)); err != nil {
		t.Fatal(err)
	}

	// The sync methods work without any PV/PVC informer.
	if _, err := r.SyncVolumeReplicationForClaim(context.Background(), types.NamespacedName{Namespace: "app", Name: "claim1"}); err != nil {
		t.Errorf("SyncVolumeReplicationForClaim: %v", err)
	}
	if _, err := r.VolumeReplicationClassByName(context.Background(), "sb-sync-a"); err != nil {
		t.Errorf("VolumeReplicationClassByName: %v", err)
	}
	// The PV/PVC lookups are unsupported on a sync-only resolver, not a panic.
	if _, err := r.PVByVolumeHandle(context.Background(), "c:p:v"); !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("PVByVolumeHandle on a sync-only resolver: err = %v, want ErrUnsupported", err)
	}

	if _, err := NewSyncResolver(nil, vrClassInf); !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("nil VR informer: err = %v, want ErrUnsupported", err)
	}
}
