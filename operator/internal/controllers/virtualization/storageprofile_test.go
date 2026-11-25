// Tests for filling CDI's StorageProfiles of simplyblock StorageClasses.

package virtualization

import (
	"context"
	"reflect"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// profileFor is the profile CDI creates for a class: the spec empty, and the
// provisioner and class reported in status.
func profileFor(class, provisioner string) *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(StorageProfileGVK)
	p.SetName(class)
	_ = unstructured.SetNestedField(p.Object, map[string]any{}, "spec")
	_ = unstructured.SetNestedField(p.Object, provisioner, "status", "provisioner")
	_ = unstructured.SetNestedField(p.Object, class, "status", "storageClass")
	return p
}

func class(name, provisioner string, params map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: provisioner,
		Parameters:  params,
	}
}

func newReconciler(t *testing.T, objects ...client.Object) *StorageProfileReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t, clientgoscheme.AddToScheme, func(s *runtime.Scheme) error {
		s.AddKnownTypeWithName(StorageProfileGVK, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(StorageProfileGVK.GroupVersion().WithKind("StorageProfileList"), &unstructured.UnstructuredList{})
		return nil
	})
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(StorageProfileGVK, meta.RESTScopeRoot)
	mapper.Add(storagev1.SchemeGroupVersion.WithKind("StorageClass"), meta.RESTScopeRoot)
	mapper.Add(simplyblockv1alpha2.GroupVersion.WithKind("SimplyblockDriver"), meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objects...).Build()
	return &StorageProfileReconciler{Client: c, Scheme: scheme}
}

func reconcileProfile(t *testing.T, r *StorageProfileReconciler, name string) *unstructured.Unstructured {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(StorageProfileGVK)
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, got); err != nil {
		t.Fatalf("get the profile: %v", err)
	}
	return got
}

func claimSets(modes ...[2]string) []any {
	out := make([]any, 0, len(modes))
	for _, m := range modes {
		out = append(out, map[string]any{"accessModes": []any{m[0]}, "volumeMode": m[1]})
	}
	return out
}

func spec(t *testing.T, p *unstructured.Unstructured) map[string]any {
	t.Helper()
	s, _, err := unstructured.NestedMap(p.Object, "spec")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	return s
}

func TestAnOrdinaryClassIsClaimedReadWriteManyBlockFirst(t *testing.T) {
	r := newReconciler(t,
		class("sb", "csi.simplyblock.io", map[string]string{"pool_name": "p"}),
		profileFor("sb", "csi.simplyblock.io"))

	got := spec(t, reconcileProfile(t, r, "sb"))
	want := map[string]any{
		"claimPropertySets": claimSets(
			[2]string{"ReadWriteMany", "Block"},
			[2]string{"ReadWriteOnce", "Block"},
			[2]string{"ReadWriteOnce", "Filesystem"}),
		"cloneStrategy":              "csi-clone",
		"dataImportCronSourceFormat": "snapshot",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spec = %v\nwant %v", got, want)
	}
}

func TestAHostBoundClassIsNeverClaimedReadWriteMany(t *testing.T) {
	for name, params := range map[string]map[string]string{
		"striped":     {"stripe_width": "3"},
		"compression": {"client_compression": "true"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReconciler(t, class("sb", "csi.simplyblock.io", params), profileFor("sb", "csi.simplyblock.io"))
			got := spec(t, reconcileProfile(t, r, "sb"))["claimPropertySets"]
			want := claimSets([2]string{"ReadWriteOnce", "Block"}, [2]string{"ReadWriteOnce", "Filesystem"})
			if !reflect.DeepEqual(got, want) {
				t.Errorf("claimPropertySets = %v, want %v", got, want)
			}
		})
	}
}

// A pNFS volume cannot be created from a snapshot or a volume, so CDI must copy
// into it, and keep golden images as volumes rather than snapshots.
func TestAPNFSClassIsAFilesystemCDICopiesInto(t *testing.T) {
	r := newReconciler(t,
		class("nfs", "csi.simplyblock.io", map[string]string{"csi.storage.k8s.io/fstype": "pnfs"}),
		profileFor("nfs", "csi.simplyblock.io"))

	got := spec(t, reconcileProfile(t, r, "nfs"))
	want := map[string]any{
		"claimPropertySets":          claimSets([2]string{"ReadWriteMany", "Filesystem"}, [2]string{"ReadWriteOnce", "Filesystem"}),
		"cloneStrategy":              "copy",
		"dataImportCronSourceFormat": "pvc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spec = %v\nwant %v", got, want)
	}
}

func TestAFieldSomeoneSetIsKept(t *testing.T) {
	profile := profileFor("sb", "csi.simplyblock.io")
	mine := claimSets([2]string{"ReadWriteOnce", "Filesystem"})
	_ = unstructured.SetNestedSlice(profile.Object, mine, "spec", "claimPropertySets")
	r := newReconciler(t, class("sb", "csi.simplyblock.io", nil), profile)

	got := spec(t, reconcileProfile(t, r, "sb"))
	if !reflect.DeepEqual(got["claimPropertySets"], mine) {
		t.Errorf("claimPropertySets = %v, want the one already set, %v", got["claimPropertySets"], mine)
	}
	if got["cloneStrategy"] != "csi-clone" {
		t.Errorf("cloneStrategy = %v, want the empty field filled with csi-clone", got["cloneStrategy"])
	}
}

func TestAnotherProvisionersProfileIsLeftAlone(t *testing.T) {
	r := newReconciler(t, class("ebs", "ebs.csi.aws.com", nil), profileFor("ebs", "ebs.csi.aws.com"))
	if got := spec(t, reconcileProfile(t, r, "ebs")); len(got) != 0 {
		t.Errorf("spec = %v, want it left empty", got)
	}
}

// A SimplyblockDriver may register under a name of its own, and its classes are
// ours as much as the default name's.
func TestADriverRegisteredUnderItsOwnNameIsRecognized(t *testing.T) {
	driver := &simplyblockv1alpha2.SimplyblockDriver{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock", Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha2.SimplyblockDriverSpec{DriverName: "block.example.com"},
	}
	r := newReconciler(t, driver, class("sb", "block.example.com", nil), profileFor("sb", "block.example.com"))
	if got := spec(t, reconcileProfile(t, r, "sb")); got["claimPropertySets"] == nil {
		t.Errorf("spec = %v, want it filled", got)
	}
}

func TestAFilledProfileIsNotWrittenAgain(t *testing.T) {
	r := newReconciler(t, class("sb", "csi.simplyblock.io", nil), profileFor("sb", "csi.simplyblock.io"))
	first := reconcileProfile(t, r, "sb")
	second := reconcileProfile(t, r, "sb")
	if first.GetResourceVersion() != second.GetResourceVersion() {
		t.Errorf("resourceVersion %s → %s, want a second pass to write nothing",
			first.GetResourceVersion(), second.GetResourceVersion())
	}
}

func TestADeletedProfileIsNotAnError(t *testing.T) {
	r := newReconciler(t)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "gone"}})
	if err != nil {
		t.Errorf("Reconcile = %v, want nil for a profile that no longer exists", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(StorageProfileGVK)
	if err := r.Get(context.Background(), types.NamespacedName{Name: "gone"}, got); !apierrors.IsNotFound(err) {
		t.Errorf("get = %v, want NotFound", err)
	}
}
