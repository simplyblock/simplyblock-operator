package utils

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	TLSProviderOpenShift           = "openshift"
	TLSProviderCertManager         = "cert-manager"
	OpenShiftServingCertAnnotation = "service.beta.openshift.io/serving-cert-secret-name"
	CertManagerClusterIssuerName   = "simplyblock-certificate-authority-issuer"
)

// NormalizeTLSProvider is the provider in the spelling sbcli's settings accept,
// which is lowercase: the chart passes it that way, while the CRD enums spell
// the same provider OpenShift. Empty is OpenShift.
func NormalizeTLSProvider(provider string) string {
	if provider == "" {
		return TLSProviderOpenShift
	}
	return strings.ToLower(provider)
}

func TLSProviderSupported(provider string) bool {
	switch NormalizeTLSProvider(provider) {
	case TLSProviderOpenShift, TLSProviderCertManager:
		return true
	default:
		return false
	}
}

func IsOpenShiftTLSProvider(provider string) bool {
	return NormalizeTLSProvider(provider) == TLSProviderOpenShift
}

func IsCertManagerTLSProvider(provider string) bool {
	return NormalizeTLSProvider(provider) == TLSProviderCertManager
}

func ServingCertServiceAnnotations(tlsEnabled bool, tlsProvider, secretName string) map[string]string {
	if !tlsEnabled || !IsOpenShiftTLSProvider(tlsProvider) {
		return nil
	}
	return map[string]string{
		OpenShiftServingCertAnnotation: secretName,
	}
}

func BuildServiceServingCertificate(namespace, serviceName, secretName string) *unstructured.Unstructured {
	names := ServiceCertificateNames(serviceName, namespace)
	dnsNames := make([]any, len(names))
	for i, name := range names {
		dnsNames[i] = name
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cert-manager.io/v1",
			"kind":       "Certificate",
			"metadata": map[string]any{
				"name":      serviceName,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"commonName": serviceName,
				"secretName": secretName,
				"issuerRef": map[string]any{
					"kind": "ClusterIssuer",
					"name": CertManagerClusterIssuerName,
				},
				"dnsNames": dnsNames,
			},
		},
	}
}
