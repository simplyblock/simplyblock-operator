// Tests for detecting and installing the cluster add-ons a control plane needs.

package prerequisites

import (
	"context"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Every embedded manifest parses into at least one object, and each object
// carries the label that marks it as installed by the operator.
func TestEveryManifestParsesAndIsLabeled(t *testing.T) {
	for _, p := range All() {
		objs, err := p.Objects()
		if err != nil {
			t.Fatalf("%s: %v", p.Name, err)
		}
		if len(objs) == 0 {
			t.Fatalf("%s: no objects", p.Name)
		}
		for _, o := range objs {
			if o.GetLabels()[LabelInstalledBy] != p.Name {
				t.Errorf("%s: %s/%s lacks the %s label", p.Name, o.GetKind(), o.GetName(), LabelInstalledBy)
			}
		}
	}
}

// Namespaces and CRDs are applied before anything that lives in or uses them.
func TestObjectsAreOrderedNamespacesAndCRDsFirst(t *testing.T) {
	objs, err := CertManager.Objects()
	if err != nil {
		t.Fatal(err)
	}
	seenOther := false
	for _, o := range objs {
		first := o.GetKind() == kindNamespace || o.GetKind() == kindCRD
		if first && seenOther {
			t.Fatalf("%s %s comes after an object that depends on it", o.GetKind(), o.GetName())
		}
		if !first {
			seenOther = true
		}
	}
}

func mapperServing(gvks ...schema.GroupVersionKind) meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	for _, g := range gvks {
		m.Add(g, meta.RESTScopeNamespace)
	}
	return m
}

// An API-backed prerequisite is present exactly when its probe kind is served.
func TestPresentFollowsTheServedAPI(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	ctx := context.Background()

	for _, p := range []Prerequisite{FoundationDBCRDs, CertManager, SnapshotController} {
		if ok, err := p.Present(ctx, c, mapperServing()); err != nil || ok {
			t.Errorf("%s present with nothing served: %v, %v", p.Name, ok, err)
		}
		if ok, err := p.Present(ctx, c, mapperServing(p.probe)); err != nil || !ok {
			t.Errorf("%s absent while %s is served: %v, %v", p.Name, p.probe, ok, err)
		}
	}
}

// local-path-provisioner is present when any StorageClass uses its provisioner,
// whatever the class is called.
func TestLocalPathIsDetectedByProvisioner(t *testing.T) {
	ctx := context.Background()
	empty := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	if ok, err := LocalPathProvisioner.Present(ctx, empty, mapperServing()); err != nil || ok {
		t.Fatalf("present on an empty cluster: %v, %v", ok, err)
	}
	withClass := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(&storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "k3s-local"},
		Provisioner: localPathProvisionerName,
	}).Build()
	if ok, err := LocalPathProvisioner.Present(ctx, withClass, mapperServing()); err != nil || !ok {
		t.Fatalf("absent beside a class it provisions: %v, %v", ok, err)
	}
}

// The installed local-path class becomes the default only on a cluster that
// has none, so an existing default is never displaced.
func TestLocalPathClassIsDefaultOnlyWithoutAnotherDefault(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		existing    []client.Object
		wantDefault bool
	}{
		"no default class": {wantDefault: true},
		"another default": {existing: []client.Object{&storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "gp3",
				Annotations: map[string]string{defaultClassAnnotation: annotationTrue},
			},
			Provisioner: "ebs.csi.aws.com",
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(tc.existing...).Build()
			if err := LocalPathProvisioner.Install(ctx, c, InstallOptions{}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			var sc storagev1.StorageClass
			if err := c.Get(ctx, client.ObjectKey{Name: "local-path"}, &sc); err != nil {
				t.Fatalf("the class was not created: %v", err)
			}
			if got := sc.Annotations[defaultClassAnnotation] == annotationTrue; got != tc.wantDefault {
				t.Errorf("default = %v, want %v", got, tc.wantDefault)
			}
		})
	}
}

// Install writes every object of the manifest.
func TestInstallAppliesEveryObject(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	if err := SnapshotController.Install(ctx, c, InstallOptions{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	objs, _ := SnapshotController.Objects()
	for _, o := range objs {
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(o.GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(o), got); err != nil {
			t.Errorf("%s %s was not written: %v", o.GetKind(), o.GetName(), err)
		}
	}
}

// snapshot-controller runs with the CSIVolumeGroupSnapshot feature gate and
// exits at startup unless the group snapshot CRDs are served beside the
// snapshot ones, so the manifest carries all six.
func TestTheSnapshotControllerShipsEveryCRDItWaitsFor(t *testing.T) {
	objs, err := SnapshotController.Objects()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, o := range objs {
		if o.GetKind() == kindCRD {
			have[o.GetName()] = true
		}
	}
	for _, want := range []string{
		"volumesnapshots.snapshot.storage.k8s.io",
		"volumesnapshotcontents.snapshot.storage.k8s.io",
		"volumesnapshotclasses.snapshot.storage.k8s.io",
		"volumegroupsnapshots.groupsnapshot.storage.k8s.io",
		"volumegroupsnapshotcontents.groupsnapshot.storage.k8s.io",
		"volumegroupsnapshotclasses.groupsnapshot.storage.k8s.io",
	} {
		if !have[want] {
			t.Errorf("the snapshot manifest does not define %s", want)
		}
	}
}
