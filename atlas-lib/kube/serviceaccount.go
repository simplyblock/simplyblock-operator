package kube

import (
	"fmt"
	"os"
	"strings"
)

// In-cluster paths to the pod's projected service-account credentials. They are
// vars, not consts, so tests can point them at fixtures.
var (
	// ServiceAccountTokenPath is where the kubelet projects the pod's
	// service-account bearer token.
	ServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// ServiceAccountNamespacePath is where the kubelet projects the pod's
	// namespace.
	ServiceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// ServiceAccountToken reads the pod's service-account bearer token, trimmed of
// surrounding whitespace. It errors when the token file is unavailable (e.g.
// running outside a cluster) so callers can decide whether to proceed
// unauthenticated.
func ServiceAccountToken() (string, error) {
	return readTrimmedFile(ServiceAccountTokenPath)
}

// ServiceAccountNamespace reads the namespace of the pod's service account.
func ServiceAccountNamespace() (string, error) {
	return readTrimmedFile(ServiceAccountNamespacePath)
}

func readTrimmedFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}
