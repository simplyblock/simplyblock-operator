// Tests for the prerequisite gate a local install passes before its first step:
// install what is missing when allowed, and hold naming it when not.

package controlplane

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/prerequisites"
)

var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

func crdExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crdGVK)
	return c.Get(context.Background(), client.ObjectKey{Name: name}, u) == nil
}

func reconcileOnce(t *testing.T, cp *simplyblockv1alpha2.ControlPlane) (client.Client, simplyblockv1alpha2.ControlPlane, ctrl.Result) {
	t.Helper()
	c := newClient(t, cp)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cp), &after); err != nil {
		t.Fatalf("read the object back: %v", err)
	}
	return c, after, result
}

// With the defaults, every missing add-on is installed on the first pass, and
// the install holds until the cluster serves them.
func TestMissingPrerequisitesAreInstalledByDefault(t *testing.T) {
	c, after, result := reconcileOnce(t, localControlPlane())

	for _, crd := range []string{
		"foundationdbclusters.apps.foundationdb.org",
		"certificates.cert-manager.io",
		"volumesnapshots.snapshot.storage.k8s.io",
	} {
		if !crdExists(t, c, crd) {
			t.Errorf("CRD %s was not installed", crd)
		}
	}
	var sc storagev1.StorageClass
	if err := c.Get(context.Background(), client.ObjectKey{Name: "local-path"}, &sc); err != nil {
		t.Errorf("local-path StorageClass was not installed: %v", err)
	} else if sc.Labels[prerequisites.LabelInstalledBy] != prerequisites.LocalPathProvisioner.Name {
		t.Errorf("local-path StorageClass labels = %v, want the installed-by label", sc.Labels)
	}
	if result.RequeueAfter == 0 {
		t.Error("not requeued, so it would never notice the add-ons becoming served")
	}
	if after.Status.Phase != simplyblockv1alpha2.ControlPlanePhaseInstalling ||
		!strings.Contains(after.Status.Message, "installing") {
		t.Errorf("status = %s %q, want Installing and a message saying what is being installed",
			after.Status.Phase, after.Status.Message)
	}
}

// With every flag off nothing is installed, and the hold names each add-on the
// control plane waits for.
func TestDisabledPrerequisitesAreOnlyChecked(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.Prerequisites = simplyblockv1alpha2.ControlPlanePrerequisites{
		InstallFoundationDBCRDs:     ptr.To(false),
		InstallCertManager:          ptr.To(false),
		InstallLocalPathProvisioner: ptr.To(false),
		InstallSnapshotController:   ptr.To(false),
	}
	c, after, result := reconcileOnce(t, cp)

	if crdExists(t, c, "foundationdbclusters.apps.foundationdb.org") || crdExists(t, c, "certificates.cert-manager.io") {
		t.Error("a CRD was installed although its flag was off")
	}
	if result.RequeueAfter == 0 {
		t.Error("not requeued, so it would never notice the add-ons arriving")
	}
	for _, want := range []string{"apps.foundationdb.org/v1beta2", "cert-manager.io/v1", "rancher.io/local-path"} {
		if !strings.Contains(after.Status.Message, want) {
			t.Errorf("status.message = %q, want it to name %s", after.Status.Message, want)
		}
	}
}

// cert-manager is neither installed nor required when the control plane does
// not get its certificate from it.
func TestCertManagerIsSkippedWhenTLSDoesNotUseIt(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.TLS.Provider = simplyblockv1alpha2.ControlPlaneTLSOpenShift
	cp.Spec.Source.Local.Prerequisites.InstallCertManager = ptr.To(false)
	_, after, _ := reconcileOnce(t, cp)

	if strings.Contains(after.Status.Message, "cert-manager") {
		t.Errorf("status.message = %q names cert-manager, which this TLS provider does not use", after.Status.Message)
	}
}

// local-path-provisioner is not installed when the FoundationDB volumes name
// their own StorageClass.
func TestLocalPathIsSkippedWhenAStorageClassIsNamed(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.FoundationDB = &simplyblockv1alpha2.FoundationDBSpec{StorageClassName: "gp3"}
	c, _, _ := reconcileOnce(t, cp)

	var sc storagev1.StorageClass
	if err := c.Get(context.Background(), client.ObjectKey{Name: "local-path"}, &sc); err == nil {
		t.Error("local-path was installed although the volumes name their own class")
	}
}

// With every add-on served, the gate applies the cert-manager issuer chain the
// control plane's certificates are signed from, and does not hold.
func TestTheIssuerChainIsAppliedOnceCertManagerIsServed(t *testing.T) {
	cp := localControlPlane()
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		fdbClusterGVK,
		{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"},
		{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"},
	} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRESTMapper(mapper).
		WithObjects(cp, &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "local-path"},
			Provisioner: "rancher.io/local-path",
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	held, message, err := r.ensurePrerequisites(context.Background(), cp)
	if err != nil {
		t.Fatalf("ensurePrerequisites: %v", err)
	}
	if held {
		t.Errorf("held with every add-on served: %s", message)
	}
	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuer"})
	if err := c.Get(context.Background(), client.ObjectKey{Name: prerequisites.CAIssuerName}, issuer); err != nil {
		t.Errorf("the CA ClusterIssuer was not applied: %v", err)
	}
}

// servedEverything is a mapper serving every add-on's API except those named.
func servedExcept(skip ...string) meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		fdbClusterGVK,
		{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"},
		{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"},
	} {
		if !slices.Contains(skip, gvk.Group) {
			mapper.Add(gvk, meta.RESTScopeNamespace)
		}
	}
	return mapper
}

func gateClient(t *testing.T, mapper meta.RESTMapper, objs ...client.Object) client.Client {
	t.Helper()
	objs = append(objs, &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "k3s-local"}, Provisioner: "rancher.io/local-path",
	})
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithRESTMapper(mapper).WithObjects(objs...).Build()
}

// A missing snapshot controller never holds the control plane: only the CSI
// driver needs it.
func TestAMissingSnapshotControllerDoesNotHoldTheInstall(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.Prerequisites.InstallSnapshotController = ptr.To(false)
	r := &ControlPlaneReconciler{Client: gateClient(t, servedExcept("snapshot.storage.k8s.io"), cp), Scheme: testScheme(t)}
	held, message, err := r.ensurePrerequisites(context.Background(), cp)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Errorf("held on the snapshot controller: %s", message)
	}
}

func installedCRD(name, owner, digest string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crdGVK)
	u.SetName(name)
	u.SetLabels(map[string]string{prerequisites.LabelInstalledBy: owner})
	u.SetAnnotations(map[string]string{prerequisites.AnnotationDigest: digest})
	return u
}

func digestOf(t *testing.T, c client.Client, name string) string {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crdGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, u); err != nil {
		t.Fatal(err)
	}
	return u.GetAnnotations()[prerequisites.AnnotationDigest]
}

// An outdated install of ours is upgraded where the add-on allows it, and only
// reported for cert-manager, whose upgrades go one minor version at a time.
func TestOutdatedInstallsAreUpgradedExceptCertManager(t *testing.T) {
	cp := localControlPlane()
	const fdbCRD, cmCRD = "foundationdbclusters.apps.foundationdb.org", "certificates.cert-manager.io"
	c := gateClient(t, servedExcept(), cp,
		installedCRD(fdbCRD, prerequisites.FoundationDBCRDs.Name, "old"),
		installedCRD(cmCRD, prerequisites.CertManager.Name, "old"))
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	held, message, err := r.ensurePrerequisites(context.Background(), cp)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Errorf("an upgrade held the install: %s", message)
	}
	if got := digestOf(t, c, fdbCRD); got != prerequisites.FoundationDBCRDs.Digest(prerequisites.InstallOptions{}) {
		t.Errorf("FoundationDB CRD digest = %q, want it upgraded", got)
	}
	if got := digestOf(t, c, cmCRD); got != "old" {
		t.Errorf("cert-manager CRD digest = %q, want it left for a person to upgrade", got)
	}
}

// rejectingApply refuses every server-side apply, as an API server does when the
// operator's role lacks a permission an upgraded manifest needs.
type rejectingApply struct{ client.Client }

func (rejectingApply) Apply(context.Context, runtime.ApplyConfiguration, ...client.ApplyOption) error {
	return errors.New("forbidden")
}

// A failed upgrade of an add-on that is already served is reported and does not
// stop the reconcile: the control plane it serves keeps working.
func TestAFailedUpgradeDoesNotStopTheReconcile(t *testing.T) {
	cp := localControlPlane()
	cp.Spec.Source.Local.TLS.EnableTLS = ptr.To(false)
	const crd = "volumesnapshots.snapshot.storage.k8s.io"
	current := prerequisites.FoundationDBCRDs.Digest(prerequisites.InstallOptions{Namespace: cp.Namespace})
	c := gateClient(t, servedExcept(), cp,
		installedCRD("foundationdbclusters.apps.foundationdb.org", prerequisites.FoundationDBCRDs.Name, current),
		installedCRD(crd, prerequisites.SnapshotController.Name, "old"))
	r := &ControlPlaneReconciler{Client: rejectingApply{c}, Scheme: testScheme(t)}

	held, message, err := r.ensurePrerequisites(context.Background(), cp)
	if err != nil {
		t.Fatalf("a failed upgrade of a served add-on stopped the reconcile: %v", err)
	}
	if held {
		t.Errorf("a failed upgrade of a served add-on held the install: %s", message)
	}
}
