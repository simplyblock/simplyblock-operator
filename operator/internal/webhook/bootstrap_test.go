// Tests for the pre-start trust bootstrap.
//
// The behavior under test is an ordering one, and the cost of getting it wrong is
// an operator that cannot start at all: the manager syncs its caches before it
// runs any Runnable that could inject the conversion webhook's CA, and a cache
// sync that lists a converted kind makes the API server call that webhook. If the
// CA is not there yet the list fails, the cache never syncs, the manager exits,
// and the injection that would have fixed it never runs.
//
// So these tests assert that everything the API server needs is in place after a
// single synchronous call, before any manager exists.

package webhook

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// bootstrapNamespace is a namespace other than the one the shipped manifests
// name, so that a correction the bootstrap fails to make is visible.
const bootstrapNamespace = "tenant-a"

func newBootstrapper(t *testing.T, objs ...*apiextensionsv1.CustomResourceDefinition) *trustBootstrapper {
	t.Helper()

	builder := fake.NewClientBuilder().WithScheme(newScheme(t))
	for _, o := range objs {
		builder = builder.WithObjects(o)
	}
	c := builder.Build()

	return &trustBootstrapper{
		client:    c,
		apiReader: c,
		namespace: bootstrapNamespace,
		dnsName:   utils.WebhookServiceName + "." + bootstrapNamespace + ".svc",
		certDir:   t.TempDir(),
	}
}

// The whole point of the bootstrap: after it returns, a converted CRD carries a
// CA the API server can verify the webhook against, and a service reference that
// resolves in this operator's namespace.
func TestBootstrapMakesTheConversionWebhookTrustedBeforeAnyManagerRuns(t *testing.T) {
	crdName := ConvertedKindCRDNames()[0]

	crd := convertedCRD(crdName)
	crd.Spec.Conversion.Webhook.ClientConfig.Service = &apiextensionsv1.ServiceReference{
		Namespace: "simplyblock-operator-system",
		Name:      utils.WebhookServiceName,
	}

	b := newBootstrapper(t, crd)
	if err := b.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	var got apiextensionsv1.CustomResourceDefinition
	if err := b.client.Get(context.Background(), types.NamespacedName{Name: crdName}, &got); err != nil {
		t.Fatalf("get crd: %v", err)
	}
	cc := got.Spec.Conversion.Webhook.ClientConfig
	if len(cc.CABundle) == 0 {
		t.Error("caBundle is empty, so the API server cannot call the conversion webhook")
	}
	if cc.Service == nil || cc.Service.Namespace != bootstrapNamespace {
		t.Errorf("conversion service namespace = %+v, want %s", cc.Service, bootstrapNamespace)
	}

	// The bundle has to be a certificate, not merely non-empty.
	block, _ := pem.Decode(cc.CABundle)
	if block == nil {
		t.Fatal("caBundle is not PEM")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Errorf("caBundle is not a certificate: %v", err)
	}
}

// The webhook server reads its serving material from disk, and controller-runtime's
// certwatcher fails outright on a missing file, so the files have to exist before
// the manager starts rather than shortly after.
func TestBootstrapWritesTheServingCertToDisk(t *testing.T) {
	b := newBootstrapper(t)
	if err := b.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, name := range []string{tlsCertFileName, tlsKeyFileName} {
		data, err := os.ReadFile(filepath.Join(b.certDir, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}

// The generated material is stored in the shape cert-controller's rotator reads,
// so that the rotator adopts it on its first pass instead of immediately issuing
// a second CA and re-injecting.
func TestBootstrapStoresMaterialTheRotatorCanAdopt(t *testing.T) {
	b := newBootstrapper(t)
	if err := b.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: b.namespace, Name: utils.WebhookServerCertSecret}
	if err := b.apiReader.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("get serving secret: %v", err)
	}

	for _, k := range []string{caCrtKey, caKeyKey, tlsCertFileName, tlsKeyFileName} {
		if len(secret.Data[k]) == 0 {
			t.Errorf("secret is missing %s, so the rotator would treat it as unusable", k)
		}
	}
}

// The serving certificate has to be valid for the in-cluster DNS name the API
// server dials, or the conversion call fails TLS verification even with the right
// CA.
func TestBootstrapIssuesACertForTheWebhookServiceName(t *testing.T) {
	b := newBootstrapper(t)
	if err := b.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(b.certDir, tlsCertFileName))
	if err != nil {
		t.Fatalf("read serving cert: %v", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("serving cert is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse serving cert: %v", err)
	}

	if err := cert.VerifyHostname(b.dnsName); err != nil {
		t.Errorf("serving cert is not valid for %q: %v", b.dnsName, err)
	}
}

// Running twice must not issue a second CA: the operator restarts, and a fresh CA
// on every start would invalidate the bundle already injected and leave a window
// where the API server rejects the webhook it was just told to trust.
func TestBootstrapReusesExistingMaterial(t *testing.T) {
	b := newBootstrapper(t)
	if err := b.run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	first, err := os.ReadFile(filepath.Join(b.certDir, tlsCertFileName))
	if err != nil {
		t.Fatalf("read serving cert: %v", err)
	}

	if err := b.run(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(b.certDir, tlsCertFileName))
	if err != nil {
		t.Fatalf("re-read serving cert: %v", err)
	}

	if string(first) != string(second) {
		t.Error("the second run issued new material, so a restart would invalidate the injected CA")
	}
}

// A CRD that is not applied yet must not fail the bootstrap, or the operator
// cannot start before its CRDs exist — which is one of the two apply orders.
func TestBootstrapToleratesMissingCRDs(t *testing.T) {
	b := newBootstrapper(t)
	if err := b.run(context.Background()); err != nil {
		t.Fatalf("run with no CRDs present: %v", err)
	}
}
