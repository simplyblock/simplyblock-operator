// Tests that the driver issues the client certificates its plugins mount for
// mutual TLS through cert-manager, which only the chart issued before.

package driver

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func issuedSecrets(t *testing.T, d *simplyblockv1alpha2.SimplyblockDriver) map[string]bool {
	t.Helper()
	r := &SimplyblockDriverReconciler{Scheme: reconcilerScheme(t), Snapshots: servingCluster}
	out := map[string]bool{}
	for _, obj := range desiredSet(t, r, d) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || u.GetKind() != "Certificate" {
			continue
		}
		s, _, _ := unstructured.NestedString(u.Object, "spec", "secretName")
		out[s] = true
	}
	return out
}

func TestMutualTLSThroughCertManagerIssuesBothClientCertificates(t *testing.T) {
	d := testDriver("simplyblock")
	d.Spec.TLS = simplyblockv1alpha2.DriverTLS{
		EnableTLS: ptr.To(true), EnableMutualTLS: ptr.To(true),
		Provider: simplyblockv1alpha2.DriverTLSProviderCertManager,
	}
	n := names(d)
	got := issuedSecrets(t, d)
	for _, want := range []string{n.controllerClientSecret, n.nodeClientSecret} {
		if !got[want] {
			t.Errorf("no Certificate issues %s, which the plugins mount", want)
		}
	}
}

func TestNoClientCertificatesWithoutMutualTLSOrCertManager(t *testing.T) {
	for name, tls := range map[string]simplyblockv1alpha2.DriverTLS{
		"TLS off":   {},
		"one-way":   {EnableTLS: ptr.To(true), EnableMutualTLS: ptr.To(false), Provider: simplyblockv1alpha2.DriverTLSProviderCertManager},
		"OpenShift": {EnableTLS: ptr.To(true), EnableMutualTLS: ptr.To(true), Provider: simplyblockv1alpha2.DriverTLSProviderOpenShift},
	} {
		t.Run(name, func(t *testing.T) {
			d := testDriver("simplyblock")
			d.Spec.TLS = tls
			if got := issuedSecrets(t, d); len(got) != 0 {
				t.Errorf("issued %v", got)
			}
		})
	}
}
