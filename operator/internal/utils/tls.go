package utils

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	TLSProviderOpenShift           = "OpenShift"
	TLSProviderCertManager         = "cert-manager"
	OpenShiftServingCertAnnotation = "service.beta.openshift.io/serving-cert-secret-name"
	CertManagerClusterIssuerName   = "simplyblock-certificate-authority-issuer"
)

func NormalizeTLSProvider(provider string) string {
	if provider == "" {
		return TLSProviderOpenShift
	}
	return provider
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
	dnsNames := []any{
		serviceName,
		fmt.Sprintf("%s.%s", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace),
		fmt.Sprintf("*.%s.%s.svc.cluster.local", serviceName, namespace),
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
