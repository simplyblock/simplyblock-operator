// Tests for telling an add-on the operator installed, and whether it is
// current, from one installed by somebody else.

package prerequisites

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func crd(name string, labels, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("apiextensions.k8s.io/v1")
	u.SetKind(kindCRD)
	u.SetName(name)
	u.SetLabels(labels)
	u.SetAnnotations(annotations)
	return u
}

// Every installed object records the digest of what it was installed from.
func TestObjectsCarryTheManifestDigest(t *testing.T) {
	objs, err := CertManager.objects(InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := CertManager.Digest(InstallOptions{})
	for _, o := range objs {
		if got := o.GetAnnotations()[AnnotationDigest]; got != want {
			t.Fatalf("%s %s digest = %q, want %q", o.GetKind(), o.GetName(), got, want)
		}
	}
	if CertManager.Digest(InstallOptions{ImageRegistry: "mirror.example"}) == want {
		t.Error("a different mirror registry produced the same digest, so switching it would not re-apply")
	}
}

func TestStateTellsOursFromTheirs(t *testing.T) {
	ctx := context.Background()
	current := FoundationDBCRDs.Digest(InstallOptions{})
	ours := map[string]string{LabelInstalledBy: FoundationDBCRDs.Name}
	for name, tc := range map[string]struct {
		existing []client.Object
		want     State
	}{
		"absent":         {want: Absent},
		"customer's":     {existing: []client.Object{crd(FoundationDBCRDs.marker.name, nil, nil)}, want: Foreign},
		"ours, current":  {existing: []client.Object{crd(FoundationDBCRDs.marker.name, ours, map[string]string{AnnotationDigest: current})}, want: Current},
		"ours, outdated": {existing: []client.Object{crd(FoundationDBCRDs.marker.name, ours, map[string]string{AnnotationDigest: "old"})}, want: Outdated},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(tc.existing...).Build()
			got, err := FoundationDBCRDs.State(ctx, c, InstallOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("State = %v, want %v", got, tc.want)
			}
		})
	}
}

// local-path is judged by its class: one of ours is upgraded, while any other
// class using the provisioner means somebody else runs it.
func TestLocalPathStateFollowsItsClass(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(&storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "k3s-local"},
		Provisioner: localPathProvisionerName,
	}).Build()
	if got, err := LocalPathProvisioner.State(ctx, c, InstallOptions{}); err != nil || got != Foreign {
		t.Errorf("State = %v, %v; want Foreign", got, err)
	}
}

// Re-installing over an outdated install moves it to the current digest.
func TestInstallUpgradesAnOutdatedCRD(t *testing.T) {
	ctx := context.Background()
	old := crd(FoundationDBCRDs.marker.name, map[string]string{LabelInstalledBy: FoundationDBCRDs.Name},
		map[string]string{AnnotationDigest: "old"})
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(old).Build()
	if err := FoundationDBCRDs.Install(ctx, c, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := FoundationDBCRDs.State(ctx, c, InstallOptions{}); got != Current {
		t.Errorf("State after install = %v, want Current", got)
	}
}

// failingOn refuses the apply of one named object, as an API server does when
// the operator's role lacks the permission for it.
type failingOn struct {
	client.Client
	name string
}

func (f failingOn) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	if u, ok := obj.(interface{ GetName() string }); ok && u.GetName() == f.name {
		return errors.New("forbidden")
	}
	return f.Client.Apply(ctx, obj, opts...)
}

// An install that fails part way leaves the add-on Outdated, so the next pass
// finishes it: the marker records the new digest only once every other object
// has been applied.
func TestAPartialInstallIsNotRecordedAsCurrent(t *testing.T) {
	ctx := context.Background()
	old := crd(SnapshotController.marker.name, map[string]string{LabelInstalledBy: SnapshotController.Name},
		map[string]string{AnnotationDigest: "old"})
	base := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(old).Build()
	c := failingOn{Client: base, name: "volumegroupsnapshotclasses.groupsnapshot.storage.k8s.io"}

	if err := SnapshotController.Install(ctx, c, InstallOptions{}); err == nil {
		t.Fatal("the install succeeded although one object was refused")
	}
	if got, _ := SnapshotController.State(ctx, base, InstallOptions{}); got != Outdated {
		t.Errorf("State after a partial install = %v, want Outdated so the next pass retries", got)
	}
}

// Regression: a marker of ours whose workload is gone, such as local-path's
// class after its namespace was deleted, reads as Absent so the add-on is
// reinstalled rather than reported as running.
func TestAMarkerWithoutItsWorkloadIsAbsent(t *testing.T) {
	ctx := context.Background()
	opts := InstallOptions{Namespace: "simplyblock"}
	class := &unstructured.Unstructured{}
	class.SetAPIVersion("storage.k8s.io/v1")
	class.SetKind("StorageClass")
	class.SetName("local-path")
	class.SetLabels(map[string]string{LabelInstalledBy: LocalPathProvisioner.Name})
	class.SetAnnotations(map[string]string{AnnotationDigest: LocalPathProvisioner.Digest(opts)})
	class.Object["provisioner"] = localPathProvisionerName

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(class).Build()
	if got, err := LocalPathProvisioner.State(ctx, c, opts); err != nil || got != Absent {
		t.Errorf("State without the provisioner Deployment = %v, %v; want Absent", got, err)
	}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "simplyblock", Name: "local-path-provisioner"}}
	c = fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(class, deploy).Build()
	if got, err := LocalPathProvisioner.State(ctx, c, opts); err != nil || got != Current {
		t.Errorf("State with the provisioner Deployment = %v, %v; want Current", got, err)
	}
}
