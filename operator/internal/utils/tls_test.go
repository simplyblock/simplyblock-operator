// Tests for the TLS provider helpers in tls.go: how a provider string from the
// chart or from a CR is recognized, whatever its spelling.

package utils

import "testing"

// Regression: 2026-10-19-tls-provider-case. The chart passes SB_TLS_PROVIDER in
// lowercase, the CRD enums spell the same provider OpenShift, and sbcli accepts
// only lowercase. TLSProviderSupported folded case while the predicates and
// NormalizeTLSProvider did not, so SB_TLS_PROVIDER=OpenShift passed the startup
// check and then matched neither provider.
func TestTLSProviderIsRecognizedInAnyCase(t *testing.T) {
	for _, provider := range []string{"openshift", "OpenShift", "OPENSHIFT"} {
		if !TLSProviderSupported(provider) {
			t.Errorf("TLSProviderSupported(%q) = false", provider)
		}
		if !IsOpenShiftTLSProvider(provider) {
			t.Errorf("IsOpenShiftTLSProvider(%q) = false", provider)
		}
		if got := NormalizeTLSProvider(provider); got != TLSProviderOpenShift {
			t.Errorf("NormalizeTLSProvider(%q) = %q, want %q", provider, got, TLSProviderOpenShift)
		}
	}
	for _, provider := range []string{"cert-manager", "Cert-Manager"} {
		if !IsCertManagerTLSProvider(provider) {
			t.Errorf("IsCertManagerTLSProvider(%q) = false", provider)
		}
		if got := NormalizeTLSProvider(provider); got != TLSProviderCertManager {
			t.Errorf("NormalizeTLSProvider(%q) = %q, want %q", provider, got, TLSProviderCertManager)
		}
	}
	if TLSProviderSupported("vault") {
		t.Error(`TLSProviderSupported("vault") = true`)
	}
}

// The value the storage-node pods receive is the spelling sbcli's settings
// accept, which is lowercase only.
func TestTheStorageNodeReceivesTheProviderInLowercase(t *testing.T) {
	for _, provider := range []string{"", "OpenShift", "openshift"} {
		if got := NormalizeTLSProvider(provider); got != "openshift" {
			t.Errorf("NormalizeTLSProvider(%q) = %q, want openshift", provider, got)
		}
	}
}
