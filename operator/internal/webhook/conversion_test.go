// Tests for the conversion webhook's wiring: that every converted kind's CRD is
// named for CA-bundle injection, and that the cert-manager provisioner actually
// injects into it.
//
// The second is the one worth having. A conversion webhook the API server does
// not trust does not degrade its kind, it makes it unreadable, and the mutating
// and validating configurations the provisioner already patched give no coverage
// of the CRD path at all.

package webhook

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/ptr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func convertedCRD(name string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ClientConfig: &apiextensionsv1.WebhookClientConfig{},
				},
			},
		},
	}
}

// The Go list and the manifest list have to name the same CRDs. A kind in the
// manifest list but not in Go is a CRD whose conversion strategy is Webhook with
// nothing serving it, which makes the kind unreadable; a kind in Go but not in
// the manifest list is a conversion that is never reached, so the API server
// hands v1alpha1 data to a v1alpha2 reader with the unknown fields pruned.
func TestConvertedKindsMatchTheManifestList(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "converted-kinds.txt"))
	if err != nil {
		t.Fatalf("read converted-kinds.txt: %v", err)
	}

	var fromManifest []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fromManifest = append(fromManifest, line)
	}

	fromGo := append([]string(nil), ConvertedKindCRDNames()...)
	sort.Strings(fromGo)
	sort.Strings(fromManifest)

	if !slices.Equal(fromGo, fromManifest) {
		t.Errorf("converted kinds disagree:\n  conversion.go:        %v\n  converted-kinds.txt:  %v",
			fromGo, fromManifest)
	}
}

// The manager's ClusterRole may write exactly the CRDs that declare a converted
// kind, and no others.
//
// Patching an arbitrary CRD is close to cluster-admin in effect, so the write
// grant carries a resourceNames list. That list is a third copy of the same set,
// beside conversion.go and converted-kinds.txt, and it is generated from a
// kubebuilder marker rather than from either of them — so nothing but this test
// notices when a kind is added to the migration and the grant is not widened with
// it. The failure that would follow is the operator silently unable to inject the
// CA into the new kind's CRD, which makes that kind unreadable.
func TestManagerRoleNamesOnlyTheConvertedCRDs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatalf("read role.yaml: %v", err)
	}

	var role struct {
		Rules []struct {
			APIGroups     []string `json:"apiGroups"`
			Resources     []string `json:"resources"`
			ResourceNames []string `json:"resourceNames"`
			Verbs         []string `json:"verbs"`
		} `json:"rules"`
	}
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatalf("parse role.yaml: %v", err)
	}

	writeVerbs := map[string]bool{"create": true, "delete": true, "deletecollection": true,
		"patch": true, "update": true}

	var named []string
	for _, rule := range role.Rules {
		if !slices.Contains(rule.APIGroups, "apiextensions.k8s.io") ||
			!slices.Contains(rule.Resources, "customresourcedefinitions") {
			continue
		}
		writes := false
		for _, verb := range rule.Verbs {
			if writeVerbs[verb] {
				writes = true
			}
		}
		if !writes {
			continue
		}
		if len(rule.ResourceNames) == 0 {
			t.Fatalf("a rule writes every CustomResourceDefinition cluster-wide (verbs %v); "+
				"the write grant must name the converted CRDs", rule.Verbs)
		}
		named = append(named, rule.ResourceNames...)
	}

	if len(named) == 0 {
		t.Fatal("no rule writes any CustomResourceDefinition, so the CA can never be injected")
	}

	want := append([]string(nil), ConvertedKindCRDNames()...)
	sort.Strings(want)
	sort.Strings(named)

	if !slices.Equal(named, want) {
		t.Errorf("the role's writable CRDs disagree with the converted kinds:\n"+
			"  role.yaml:      %v\n  conversion.go:  %v", named, want)
	}
}

func TestConvertedKindCRDNamesIsNotEmpty(t *testing.T) {
	names := ConvertedKindCRDNames()
	if len(names) == 0 {
		t.Fatal("no converted kinds are named, so no CRD would ever be trusted")
	}
	for _, name := range names {
		if name == "" {
			t.Error("a converted kind has an empty CRD name")
		}
	}
}

func TestReconcileCertInjectsCAIntoConvertedCRDs(t *testing.T) {
	crdName := ConvertedKindCRDNames()[0]

	p := newProvisioner(t,
		servingSecret(testCAData, testCertData, testKeyData),
		webhookConfig(),
		validatingWebhookConfig(),
		convertedCRD(crdName),
	)

	if err := p.reconcileCert(context.Background()); err != nil {
		t.Fatalf("reconcileCert: %v", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := p.client.Get(context.Background(), types.NamespacedName{Name: crdName}, &crd); err != nil {
		t.Fatalf("get crd: %v", err)
	}
	if crd.Spec.Conversion == nil || crd.Spec.Conversion.Webhook == nil ||
		crd.Spec.Conversion.Webhook.ClientConfig == nil {
		t.Fatalf("crd %s lost its conversion client config", crdName)
	}
	if got := string(crd.Spec.Conversion.Webhook.ClientConfig.CABundle); got != testCAData {
		t.Errorf("conversion caBundle = %q, want %q", got, testCAData)
	}
}

// The namespace baked into the shipped manifest is the default install's. An
// operator running anywhere else has to correct it, because it is the only party
// that knows which namespace it is in, and a service reference naming the wrong
// namespace makes every read of the kind fail.
func TestReconcileCertCorrectsTheConversionServiceNamespace(t *testing.T) {
	crdName := ConvertedKindCRDNames()[0]

	// The manifest names the namespace of the default install. This operator runs
	// in the one newProvisioner puts it in, which is a different one.
	crd := convertedCRD(crdName)
	crd.Spec.Conversion.Webhook.ClientConfig.Service = &apiextensionsv1.ServiceReference{
		Namespace: "some-other-namespace",
		Name:      utils.ConversionWebhookServiceName,
		Path:      ptr.To("/convert"),
	}

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(crd).Build()
	r := &conversionServiceReconciler{client: c, apiReader: c, namespace: "tenant-a"}

	if err := r.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}

	var got apiextensionsv1.CustomResourceDefinition
	if err := c.Get(context.Background(), types.NamespacedName{Name: crdName}, &got); err != nil {
		t.Fatalf("get crd: %v", err)
	}
	svc := got.Spec.Conversion.Webhook.ClientConfig.Service
	if svc == nil {
		t.Fatal("conversion service reference was removed")
	}
	if svc.Namespace != "tenant-a" {
		t.Errorf("conversion service namespace = %q, want %q", svc.Namespace, "tenant-a")
	}
	if svc.Name != utils.ConversionWebhookServiceName {
		t.Errorf("conversion service name = %q, want %q", svc.Name, utils.ConversionWebhookServiceName)
	}
}

// A CRD that appears after the operator started must still be corrected.
//
// The operator and its CRDs are applied by separate steps and in either order, so
// an operator that gives up after one pass leaves the conversion webhook pointing
// at the namespace the manifest shipped with. That is not a degraded state: the
// API server cannot reach the webhook, so every read of the converted kinds fails
// for as long as the operator keeps running. The same pass has to keep running
// for a second reason — re-applying the CRDs, as an upgrade does, puts the
// shipped namespace back and the correction has to be made again.
func TestConversionServiceReferenceCorrectsACRDThatAppearsLater(t *testing.T) {
	crdName := ConvertedKindCRDNames()[0]

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	r := &conversionServiceReconciler{
		client:    c,
		apiReader: c,
		namespace: "tenant-a",
		interval:  10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// The CRD lands only after the first pass has already found nothing.
	crd := convertedCRD(crdName)
	crd.Spec.Conversion.Webhook.ClientConfig.Service = &apiextensionsv1.ServiceReference{
		Namespace: "some-other-namespace",
		Name:      utils.ConversionWebhookServiceName,
		Path:      ptr.To("/convert"),
	}
	time.Sleep(30 * time.Millisecond)
	if err := c.Create(ctx, crd); err != nil {
		t.Fatalf("create crd: %v", err)
	}

	var corrected bool
	for range 100 {
		var got apiextensionsv1.CustomResourceDefinition
		if err := c.Get(ctx, types.NamespacedName{Name: crdName}, &got); err == nil {
			if svc := got.Spec.Conversion.Webhook.ClientConfig.Service; svc != nil &&
				svc.Namespace == "tenant-a" {
				corrected = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !corrected {
		t.Error("a CRD created after start was never corrected, so its kind stays unreadable")
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start returned %v, want nil on context cancellation", err)
	}
}

// The correction has to tolerate a CRD that is absent or carries no conversion
// strategy, for the same reason the CA injection does: the operator and its CRDs
// are applied by separate steps.
func TestConversionServiceReferenceToleratesAMissingCRD(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	r := &conversionServiceReconciler{client: c, apiReader: c, namespace: "tenant-a"}

	if err := r.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcileOnce with no CRD present: %v", err)
	}
}

// A CRD that has not yet been patched with the conversion strategy must not
// make the provisioner fail: the operator and its CRDs are applied by separate
// steps, so the two orders both have to work.
func TestReconcileCertToleratesACRDWithoutConversion(t *testing.T) {
	crdName := ConvertedKindCRDNames()[0]

	crd := convertedCRD(crdName)
	crd.Spec.Conversion = nil

	p := newProvisioner(t,
		servingSecret(testCAData, testCertData, testKeyData),
		webhookConfig(),
		validatingWebhookConfig(),
		crd,
	)

	if err := p.reconcileCert(context.Background()); err != nil {
		t.Fatalf("reconcileCert on a CRD with no conversion strategy: %v", err)
	}
}

// A CRD that is missing entirely must not fail the provisioner either, or the
// operator cannot start before its CRDs are applied.
func TestReconcileCertToleratesAMissingCRD(t *testing.T) {
	p := newProvisioner(t,
		servingSecret(testCAData, testCertData, testKeyData),
		webhookConfig(),
		validatingWebhookConfig(),
	)

	if err := p.reconcileCert(context.Background()); err != nil {
		t.Fatalf("reconcileCert with no CRD present: %v", err)
	}
}
