package webhook

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func newProvisioner(t *testing.T, objs ...client.Object) *certManagerProvisioner {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	return &certManagerProvisioner{
		client:    c,
		apiReader: c,
		namespace: "simplyblock-operator-system",
		dnsName:   utils.WebhookServiceName + ".simplyblock-operator-system.svc",
		certDir:   t.TempDir(),
		ready:     make(chan struct{}),
	}
}

func servingSecret(ca, crt, key string) *corev1.Secret {
	data := map[string][]byte{tlsCertFileName: []byte(crt), tlsKeyFileName: []byte(key)}
	if ca != "" {
		data[caCrtKey] = []byte(ca)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: utils.WebhookServerCertSecret, Namespace: "simplyblock-operator-system"},
		Data:       data,
	}
}

func webhookConfig() *admissionregistrationv1.MutatingWebhookConfiguration {
	return &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: utils.WebhookConfigurationName},
		Webhooks: []admissionregistrationv1.MutatingWebhook{
			{Name: "simplyblock-rebalancer-injector.simplyblock.io"},
		},
	}
}

func validatingWebhookConfig() *admissionregistrationv1.ValidatingWebhookConfiguration {
	return &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: utils.WebhookValidatingConfigurationName},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			{Name: "vstoragenode.simplyblock.io"},
		},
	}
}

func TestReconcileCertWritesFilesAndInjectsCA(t *testing.T) {
	p := newProvisioner(t, servingSecret("CA-DATA", "CRT-DATA", "KEY-DATA"), webhookConfig(), validatingWebhookConfig())

	if err := p.reconcileCert(context.Background()); err != nil {
		t.Fatalf("reconcileCert: %v", err)
	}

	// Serving cert written to disk under the expected file names.
	gotCrt, err := os.ReadFile(filepath.Join(p.certDir, tlsCertFileName))
	if err != nil || string(gotCrt) != "CRT-DATA" {
		t.Fatalf("tls.crt = %q err=%v, want %q", gotCrt, err, "CRT-DATA")
	}
	gotKey, err := os.ReadFile(filepath.Join(p.certDir, tlsKeyFileName))
	if err != nil || string(gotKey) != "KEY-DATA" {
		t.Fatalf("tls.key = %q err=%v, want %q", gotKey, err, "KEY-DATA")
	}

	// caBundle injected from ca.crt into every webhook.
	var whc admissionregistrationv1.MutatingWebhookConfiguration
	if err := p.client.Get(context.Background(), types.NamespacedName{Name: utils.WebhookConfigurationName}, &whc); err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	if got := string(whc.Webhooks[0].ClientConfig.CABundle); got != "CA-DATA" {
		t.Fatalf("caBundle = %q, want %q", got, "CA-DATA")
	}

	// caBundle also injected into the validating configuration.
	var vwhc admissionregistrationv1.ValidatingWebhookConfiguration
	if err := p.client.Get(context.Background(), types.NamespacedName{Name: utils.WebhookValidatingConfigurationName}, &vwhc); err != nil {
		t.Fatalf("get validating webhook config: %v", err)
	}
	if got := string(vwhc.Webhooks[0].ClientConfig.CABundle); got != "CA-DATA" {
		t.Fatalf("validating caBundle = %q, want %q", got, "CA-DATA")
	}

	// readiness signalled.
	select {
	case <-p.ready:
	default:
		t.Fatal("ready channel not closed after successful reconcile")
	}
}

func TestReconcileCertFallsBackToLeafWhenNoCA(t *testing.T) {
	p := newProvisioner(t, servingSecret("", "LEAF-CRT", "KEY"), webhookConfig(), validatingWebhookConfig())

	if err := p.reconcileCert(context.Background()); err != nil {
		t.Fatalf("reconcileCert: %v", err)
	}
	var whc admissionregistrationv1.MutatingWebhookConfiguration
	if err := p.client.Get(context.Background(), types.NamespacedName{Name: utils.WebhookConfigurationName}, &whc); err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	if got := string(whc.Webhooks[0].ClientConfig.CABundle); got != "LEAF-CRT" {
		t.Fatalf("caBundle = %q, want leaf cert %q", got, "LEAF-CRT")
	}
}

func TestReconcileCertErrorsWhenSecretMissing(t *testing.T) {
	p := newProvisioner(t, webhookConfig())
	if err := p.reconcileCert(context.Background()); err == nil {
		t.Fatal("expected error when serving cert secret is absent")
	}
	select {
	case <-p.ready:
		t.Fatal("ready channel closed despite failure")
	default:
	}
}

func TestReconcileCertIsIdempotent(t *testing.T) {
	p := newProvisioner(t, servingSecret("CA", "CRT", "KEY"), webhookConfig(), validatingWebhookConfig())
	for i := 0; i < 3; i++ {
		if err := p.reconcileCert(context.Background()); err != nil {
			t.Fatalf("reconcileCert iteration %d: %v", i, err)
		}
	}
}

func TestEnsureCertificateReferencesWebhookService(t *testing.T) {
	cert := utils.BuildServiceServingCertificate("ns", utils.WebhookServiceName, utils.WebhookServerCertSecret)
	spec, _ := cert.Object["spec"].(map[string]any)
	if spec["secretName"] != utils.WebhookServerCertSecret {
		t.Fatalf("secretName = %v, want %s", spec["secretName"], utils.WebhookServerCertSecret)
	}
	dnsNames, _ := spec["dnsNames"].([]any)
	want := utils.WebhookServiceName + ".ns.svc"
	found := false
	for _, d := range dnsNames {
		if d == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("dnsNames %v missing %s", dnsNames, want)
	}
}
