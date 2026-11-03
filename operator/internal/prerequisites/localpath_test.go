// Tests for installing local-path-provisioner into the control plane's
// namespace, where the OpenShift SCC and Pod Security grants already apply.

package prerequisites

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLocalPathInstallsIntoTheGivenNamespace(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	if err := LocalPathProvisioner.Install(ctx, c, InstallOptions{Namespace: "simplyblock"}); err != nil {
		t.Fatal(err)
	}

	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "local-path-storage"}, &ns); err == nil {
		t.Error("the local-path-storage namespace was created")
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: "simplyblock", Name: "local-path-provisioner"}, &d); err != nil {
		t.Errorf("the provisioner is not in the given namespace: %v", err)
	}
	var crb rbacv1.ClusterRoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: "local-path-provisioner-bind"}, &crb); err != nil {
		t.Fatal(err)
	}
	for _, s := range crb.Subjects {
		if s.Namespace != "simplyblock" {
			t.Errorf("ClusterRoleBinding subject %s is in %q, want simplyblock", s.Name, s.Namespace)
		}
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "simplyblock", Name: "local-path-config"}, &cm); err != nil {
		t.Fatal(err)
	}
	if helper := cm.Data["helperPod.yaml"]; !strings.Contains(helper, "privileged: true") {
		t.Errorf("the helper pod is not privileged, which OpenShift needs for its hostPath:\n%s", helper)
	}
}

// A different namespace is a different install, so it re-applies.
func TestTheNamespaceIsPartOfTheDigest(t *testing.T) {
	if LocalPathProvisioner.Digest(InstallOptions{Namespace: "a"}) == LocalPathProvisioner.Digest(InstallOptions{Namespace: "b"}) {
		t.Error("two namespaces produced one digest")
	}
}

// Volumes go under /var, which is writable on every node OS this ships to.
// /opt is read-only on Talos.
func TestLocalPathStoresVolumesUnderVar(t *testing.T) {
	objs, err := LocalPathProvisioner.Objects()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.GetKind() != "ConfigMap" {
			continue
		}
		config, _, _ := unstructured.NestedString(o.Object, "data", "config.json")
		if !strings.Contains(config, `"/var/local-path-provisioner"`) || strings.Contains(config, "/opt/") {
			t.Errorf("config.json = %s, want volumes under /var/local-path-provisioner", config)
		}
		return
	}
	t.Fatal("no ConfigMap in the local-path manifest")
}
