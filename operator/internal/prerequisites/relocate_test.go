// Tests for re-installing local-path-provisioner when the ControlPlane is
// re-created in another namespace: the earlier install's namespaced objects
// are removed, so one provisioner serves the class.

package prerequisites

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// namespacedObject is one namespaced object of the local-path manifest.
type namespacedObject struct {
	obj  func() client.Object
	name string
}

// localPathNamespaced lists one object of every namespaced kind the manifest has.
var localPathNamespaced = []namespacedObject{
	{func() client.Object { return &appsv1.Deployment{} }, "local-path-provisioner"},
	{func() client.Object { return &corev1.ConfigMap{} }, "local-path-config"},
	{func() client.Object { return &corev1.ServiceAccount{} }, "local-path-provisioner-service-account"},
	{func() client.Object { return &rbacv1.Role{} }, "local-path-provisioner-role"},
	{func() client.Object { return &rbacv1.RoleBinding{} }, "local-path-provisioner-bind"},
}

func installLocalPath(t *testing.T, c client.Client, namespace string) {
	t.Helper()
	if err := LocalPathProvisioner.Install(context.Background(), c, InstallOptions{Namespace: namespace}); err != nil {
		t.Fatalf("installing into %s: %v", namespace, err)
	}
}

// Regression (#839): a ControlPlane deleted from namespace a and re-created in
// namespace b re-installed local-path into b and left the provisioner, its
// ConfigMap, and its RBAC running in a.
func TestLocalPathRecreatedInAnotherNamespaceLeavesOneProvisioner(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	installLocalPath(t, c, "a")

	if got, err := LocalPathProvisioner.State(ctx, c, InstallOptions{Namespace: "b"}); err != nil || got != Outdated {
		t.Fatalf("State for namespace b = %v, %v; want Outdated", got, err)
	}
	installLocalPath(t, c, "b")

	for _, n := range localPathNamespaced {
		if err := c.Get(ctx, client.ObjectKey{Namespace: "a", Name: n.name}, n.obj()); err == nil {
			t.Errorf("%T %s is still in the old namespace a", n.obj(), n.name)
		}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "b", Name: n.name}, n.obj()); err != nil {
			t.Errorf("%T %s is not in namespace b: %v", n.obj(), n.name, err)
		}
	}
	var crb rbacv1.ClusterRoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: "local-path-provisioner-bind"}, &crb); err != nil {
		t.Fatal(err)
	}
	for _, s := range crb.Subjects {
		if s.Namespace != "b" {
			t.Errorf("ClusterRoleBinding subject %s is in %q, want b", s.Name, s.Namespace)
		}
	}
}

// An object in the old namespace without the operator's label is somebody
// else's, even when its name matches, and stays.
func TestLocalPathKeepsAnUnlabeledObjectInTheOldNamespace(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	installLocalPath(t, c, "a")

	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: "a", Name: "local-path-provisioner"}, &d); err != nil {
		t.Fatal(err)
	}
	delete(d.Labels, LabelInstalledBy)
	if err := c.Update(ctx, &d); err != nil {
		t.Fatal(err)
	}
	installLocalPath(t, c, "b")

	if err := c.Get(ctx, client.ObjectKey{Namespace: "a", Name: "local-path-provisioner"}, &d); err != nil {
		t.Errorf("the unlabeled Deployment in a was deleted: %v", err)
	}
}

// The old namespace may be gone already, with only the cluster-scoped objects
// left: the install into b goes ahead.
func TestLocalPathInstallsWhenTheOldNamespaceIsGone(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	installLocalPath(t, c, "a")
	for _, n := range localPathNamespaced {
		o := n.obj()
		o.SetNamespace("a")
		o.SetName(n.name)
		if err := c.Delete(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	installLocalPath(t, c, "b")

	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: "b", Name: "local-path-provisioner"}, &d); err != nil {
		t.Errorf("the provisioner is not in namespace b: %v", err)
	}
}

// Re-installing into the same namespace, as an operator upgrade does, deletes
// nothing.
func TestLocalPathReinstallInTheSameNamespaceKeepsItsObjects(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	installLocalPath(t, c, "a")
	installLocalPath(t, c, "a")

	for _, n := range localPathNamespaced {
		if err := c.Get(ctx, client.ObjectKey{Namespace: "a", Name: n.name}, n.obj()); err != nil {
			t.Errorf("%T %s is gone after a re-install into a: %v", n.obj(), n.name, err)
		}
	}
}

// failingDelete refuses every delete, as an API server does when the
// operator's role lacks the permission.
type failingDelete struct{ client.Client }

func (failingDelete) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return errors.New("forbidden")
}

// A removal that fails stops the install before the marker records namespace
// b, so the next pass reads Outdated and tries the removal again.
func TestLocalPathFailedRemovalIsRetried(t *testing.T) {
	ctx := context.Background()
	base := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	installLocalPath(t, base, "a")

	opts := InstallOptions{Namespace: "b"}
	if err := LocalPathProvisioner.Install(ctx, failingDelete{base}, opts); err == nil {
		t.Fatal("the install succeeded although the old provisioner could not be removed")
	}
	if got, _ := LocalPathProvisioner.State(ctx, base, opts); got != Outdated {
		t.Errorf("State after a failed removal = %v, want Outdated so the next pass retries", got)
	}
}
