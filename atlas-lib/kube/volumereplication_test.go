package kube

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	kfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/simplyblock/atlas/errs"
)

// withVRSite sets the SyncSiteAnnotation on a VolumeReplication unstructured,
// the way the node plugin records the active site (design §6.2).
func withVRSite(u *unstructured.Unstructured, site string) *unstructured.Unstructured {
	u.SetAnnotations(map[string]string{SyncSiteAnnotation: site})
	return u
}

func TestVolumeReplicationRefFromSite(t *testing.T) {
	plain := VolumeReplicationRefFrom(vrUnstructured("vr1", "app", "claim1", "sb-sync", "primary"))
	if plain.Site != "" {
		t.Errorf("no annotation: Site = %q, want empty", plain.Site)
	}
	sited := VolumeReplicationRefFrom(withVRSite(vrUnstructured("vr1", "app", "claim1", "sb-sync", "primary"), "site-a"))
	if sited.Site != "site-a" {
		t.Errorf("Site = %q, want site-a", sited.Site)
	}
}

func TestInformerResolver_VolumeReplicationForVolumeHandle(t *testing.T) {
	kf := informers.NewSharedInformerFactory(kfake.NewSimpleClientset(), 0)
	pvInf := kf.Core().V1().PersistentVolumes().Informer()
	vrInf := newUnstructuredInformer()
	r, err := NewResolver(ResolverConfig{
		PersistentVolumes:      pvInf,
		PersistentVolumeClaims: kf.Core().V1().PersistentVolumeClaims().Informer(),
		VolumeReplications:     vrInf,
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	pv := managedPV("c:p:v", "sb")
	pv.Name = "pv1"
	pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: "app", Name: "claim1"}
	if err := pvInf.GetStore().Add(pv); err != nil {
		t.Fatal(err)
	}
	if err := vrInf.GetStore().Add(
		withVRSite(vrUnstructured("vr1", "app", "claim1", "sb-sync", "primary"), "site-b")); err != nil {
		t.Fatal(err)
	}

	got, err := r.VolumeReplicationForVolumeHandle(context.Background(), "c:p:v")
	if err != nil {
		t.Fatalf("VolumeReplicationForVolumeHandle: %v", err)
	}
	if got.Name != "vr1" || got.Site != "site-b" {
		t.Errorf("ref = %+v, want Name=vr1 Site=site-b", got)
	}

	// A handle no PV carries resolves to ErrNotFound.
	if _, err := r.VolumeReplicationForVolumeHandle(context.Background(), "no:such:handle"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing handle: err = %v, want ErrNotFound", err)
	}

	// A PV with no claimRef cannot resolve a VolumeReplication.
	bare := managedPV("d:e:f", "sb")
	bare.Name = "pv-bare"
	if err := pvInf.GetStore().Add(bare); err != nil {
		t.Fatal(err)
	}
	if _, err := r.VolumeReplicationForVolumeHandle(context.Background(), "d:e:f"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("no claimRef: err = %v, want ErrNotFound", err)
	}
}

// vrUnstructured builds a VolumeReplication the way a dynamic informer would
// hand it to the resolver, protecting the named PVC in the same namespace.
func vrUnstructured(name, ns, claim, class, state string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "replication.storage.openshift.io/v1alpha1",
		"kind":       "VolumeReplication",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec": map[string]interface{}{
			"volumeReplicationClass": class,
			"replicationState":           state,
			"dataSource": map[string]interface{}{
				"kind": "PersistentVolumeClaim", "name": claim, "apiGroup": "",
			},
		},
	}}
}

func TestVolumeReplicationClaimKeys(t *testing.T) {
	vr := vrUnstructured("vr1", "app", "claim1", "sb-sync-a", "primary")
	if got := VolumeReplicationClaimKeys(vr); len(got) != 1 || got[0] != "app/claim1" {
		t.Errorf("keys = %v, want [app/claim1]", got)
	}

	// A VR whose dataSource is not a PersistentVolumeClaim has no claim key, so it
	// is never resolved as protecting a volume.
	other := vrUnstructured("vr2", "app", "", "sb-sync-a", "primary")
	_ = unstructured.SetNestedField(other.Object, "VolumeSnapshot", "spec", "dataSource", "kind")
	_ = unstructured.SetNestedField(other.Object, "snap", "spec", "dataSource", "name")
	if got := VolumeReplicationClaimKeys(other); got != nil {
		t.Errorf("non-PVC dataSource: keys = %v, want nil", got)
	}

	// A VR with no dataSource at all has no key.
	bare := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "vr3", "namespace": "app"},
		"spec":     map[string]interface{}{},
	}}
	if got := VolumeReplicationClaimKeys(bare); got != nil {
		t.Errorf("no dataSource: keys = %v, want nil", got)
	}
}

func withVRStatus(u *unstructured.Unstructured, state string, completed bool) *unstructured.Unstructured {
	condStatus := "False"
	if completed {
		condStatus = "True"
	}
	u.Object["status"] = map[string]interface{}{
		"state": state,
		"conditions": []interface{}{
			map[string]interface{}{"type": "Degraded", "status": "False"},
			map[string]interface{}{"type": "Completed", "status": condStatus},
		},
	}
	return u
}

func TestVolumeReplicationRefFrom(t *testing.T) {
	// Spec only (no status yet): the gate fields stay zero.
	got := VolumeReplicationRefFrom(vrUnstructured("vr1", "app", "claim1", "sb-sync-a", "primary"))
	want := VolumeReplicationRef{Name: "vr1", Namespace: "app", ClassName: "sb-sync-a", ReplicationState: "primary"}
	if got != want {
		t.Errorf("ref = %+v, want %+v", got, want)
	}

	// With status: the promote-done signal the gate reads.
	done := VolumeReplicationRefFrom(withVRStatus(vrUnstructured("vr1", "app", "claim1", "sb-sync-a", "primary"), "Primary", true))
	if done.ObservedState != "Primary" || !done.Completed {
		t.Errorf("done ref = %+v, want ObservedState=Primary Completed=true", done)
	}
	notDone := VolumeReplicationRefFrom(withVRStatus(vrUnstructured("vr1", "app", "claim1", "sb-sync-a", "primary"), "Secondary", false))
	if notDone.Completed {
		t.Error("Completed=true for a Completed=False condition")
	}
}

func newVRResolver(t *testing.T, vrInf cache.SharedIndexInformer) *InformerResolver {
	t.Helper()
	kf := informers.NewSharedInformerFactory(kfake.NewSimpleClientset(), 0)
	r, err := NewResolver(ResolverConfig{
		PersistentVolumes:      kf.Core().V1().PersistentVolumes().Informer(),
		PersistentVolumeClaims: kf.Core().V1().PersistentVolumeClaims().Informer(),
		VolumeReplications:     vrInf,
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func TestInformerResolver_VolumeReplicationForClaim(t *testing.T) {
	vrInf := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
	r := newVRResolver(t, vrInf)

	// Populate the informer store directly (no need to start it): AddIndexers ran
	// in NewResolver, so additions are indexed as they land.
	if err := vrInf.GetStore().Add(vrUnstructured("vr1", "app", "claim1", "sb-sync-a", "primary")); err != nil {
		t.Fatal(err)
	}

	got, err := r.VolumeReplicationForClaim(context.Background(), types.NamespacedName{Namespace: "app", Name: "claim1"})
	if err != nil {
		t.Fatalf("VolumeReplicationForClaim: %v", err)
	}
	if got.Name != "vr1" || got.ClassName != "sb-sync-a" || got.ReplicationState != "primary" {
		t.Errorf("ref = %+v", got)
	}

	if _, err := r.VolumeReplicationForClaim(context.Background(), types.NamespacedName{Namespace: "app", Name: "none"}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing claim: err = %v, want ErrNotFound", err)
	}
}

func TestInformerResolver_VolumeReplicationForClaim_NotConfigured(t *testing.T) {
	r := newVRResolver(t, nil)
	_, err := r.VolumeReplicationForClaim(context.Background(), types.NamespacedName{Namespace: "app", Name: "claim1"})
	if !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("no VR informer: err = %v, want ErrUnsupported", err)
	}
}

func TestInformerResolver_SyncVolumeReplicationForClaim(t *testing.T) {
	vrInf, vrClassInf := newUnstructuredInformer(), newUnstructuredInformer()
	r := newVRClassResolver(t, vrInf, vrClassInf)

	add := func(inf cache.SharedIndexInformer, o *unstructured.Unstructured) {
		t.Helper()
		if err := inf.GetStore().Add(o); err != nil {
			t.Fatal(err)
		}
	}
	// A sync VR: its class carries method sync.
	add(vrInf, vrUnstructured("vr-sync", "app", "claim-sync", "sb-sync-a", "primary"))
	add(vrClassInf, vrClassUnstructured("sb-sync-a", DriverName, ReplicationMethodSync))
	// An async VR: a VR whose class is async stages as an ordinary volume.
	add(vrInf, vrUnstructured("vr-async", "app", "claim-async", "sb-async", "primary"))
	add(vrClassInf, vrClassUnstructured("sb-async", DriverName, "async"))
	// A VR whose class is absent (never resolves to sync).
	add(vrInf, vrUnstructured("vr-orphan", "app", "claim-orphan", "gone", "primary"))

	nn := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: "app", Name: name} }

	got, err := r.SyncVolumeReplicationForClaim(context.Background(), nn("claim-sync"))
	if err != nil {
		t.Fatalf("sync claim: %v", err)
	}
	if got.Name != "vr-sync" {
		t.Errorf("sync claim: ref = %+v, want vr-sync", got)
	}

	for _, name := range []string{"claim-async", "claim-orphan", "claim-plain"} {
		if _, err := r.SyncVolumeReplicationForClaim(context.Background(), nn(name)); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound (stages as an ordinary volume)", name, err)
		}
	}
}

func TestInformerResolver_SyncVolumeReplicationForClaim_NotConfigured(t *testing.T) {
	// A VR informer but no VRClass informer cannot determine sync.
	r := newVRResolver(t, newUnstructuredInformer())
	if err := r.vr.GetStore().Add(vrUnstructured("vr1", "app", "claim1", "sb-sync-a", "primary")); err != nil {
		t.Fatal(err)
	}
	_, err := r.SyncVolumeReplicationForClaim(context.Background(), types.NamespacedName{Namespace: "app", Name: "claim1"})
	if !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("no VRClass informer: err = %v, want ErrUnsupported", err)
	}
}
