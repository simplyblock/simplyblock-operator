// Tests for the cert-manager issuer chain the operator applies when no chart
// created it.

package prerequisites

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	clusterIssuerGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuer"}
	certificateGVK   = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}
)

func get(t *testing.T, c client.Client, gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, u); err != nil {
		return nil
	}
	return u
}

func certManagerController(ns string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "cert-manager", Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/name": "cert-manager", "app.kubernetes.io/component": "controller"},
	}}
}

// On a cluster with no issuer chain, all four objects are created, the CA
// Certificate in the namespace cert-manager runs in.
func TestIssuerChainIsCreatedWhereMissing(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(certManagerController("security")).Build()
	if err := EnsureCertManagerIssuerChain(context.Background(), c, "simplyblock"); err != nil {
		t.Fatalf("EnsureCertManagerIssuerChain: %v", err)
	}
	if get(t, c, clusterIssuerGVK, "", SelfSignedIssuerName) == nil {
		t.Error("the self-signed ClusterIssuer was not created")
	}
	if get(t, c, certificateGVK, "security", caCertificateName) == nil {
		t.Error("the CA Certificate was not created in cert-manager's namespace")
	}
	if get(t, c, clusterIssuerGVK, "", CAIssuerName) == nil {
		t.Error("the CA ClusterIssuer was not created")
	}
	if b := get(t, c, certificateGVK, "simplyblock", caBundleCertificateName); b == nil {
		t.Error("the CA bundle Certificate was not created in the control plane's namespace")
	} else if s, _, _ := unstructured.NestedString(b.Object, "spec", "secretName"); s != "simplyblock-ca-bundle-tls" {
		t.Errorf("CA bundle secretName = %q, want simplyblock-ca-bundle-tls, which the driver mounts", s)
	}
}

// An existing CA ClusterIssuer is someone else's chain, chart or customer, and
// is left alone; only the CA bundle in this namespace is ensured.
func TestAnExistingCAIssuerIsLeftAlone(t *testing.T) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(clusterIssuerGVK)
	existing.SetName(CAIssuerName)
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(existing).Build()
	if err := EnsureCertManagerIssuerChain(context.Background(), c, "simplyblock"); err != nil {
		t.Fatalf("EnsureCertManagerIssuerChain: %v", err)
	}
	if get(t, c, clusterIssuerGVK, "", SelfSignedIssuerName) != nil {
		t.Error("a self-signed ClusterIssuer was created beside an existing chain")
	}
	if get(t, c, certificateGVK, "simplyblock", caBundleCertificateName) == nil {
		t.Error("the CA bundle Certificate was not ensured")
	}
}
