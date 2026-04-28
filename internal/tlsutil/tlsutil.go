// Package tlsutil holds TLS plumbing shared by other internal packages. It is
// a leaf package — keep it free of imports from utils or webapi to avoid
// creating an import cycle.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ServiceCABundlePath is where the operator pod mounts the CA bundle that
// signs the in-cluster simplyblock service serving certificates.
const ServiceCABundlePath = "/etc/simplyblock/tls/ca.crt"

// OperatorNamespacePath holds the path the in-pod service-account namespace
// is mounted at. Overridable for tests.
var OperatorNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// BuildStorageNodeAPIClient returns an *http.Client that trusts the CA at
// caPath and pins ServerName to the storage-node API service DNS name in the
// given namespace. This lets the operator dial a pod/host IP directly while
// still passing hostname verification against the service-ca-issued cert,
// whose SAN is the service DNS name. Timeout is short (3s) since this is used
// for reachability probes.
func BuildStorageNodeAPIClient(namespace, caPath string) (*http.Client, error) {
	c, err := buildServiceAPIClient("simplyblock-storage-node-api", namespace, caPath)
	if err != nil {
		return nil, err
	}
	c.Timeout = 3 * time.Second
	return c, nil
}

// BuildWebAPIClient returns an *http.Client that trusts the CA at caPath and
// pins ServerName to the simplyblock-webappapi service DNS name. Timeout
// matches the previous default for the cluster control-plane API client.
func BuildWebAPIClient(namespace, caPath string) (*http.Client, error) {
	c, err := buildServiceAPIClient("simplyblock-webappapi", namespace, caPath)
	if err != nil {
		return nil, err
	}
	c.Timeout = 30 * time.Second
	return c, nil
}

func buildServiceAPIClient(serviceName, namespace, caPath string) (*http.Client, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %q: %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA bundle at %q contains no usable certificates", caPath)
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    pool,
				ServerName: fmt.Sprintf("%s.%s.svc", serviceName, namespace),
			},
		},
	}, nil
}

// DetectOperatorNamespace reads the namespace this pod is running in from the
// projected service-account namespace file.
func DetectOperatorNamespace() (string, error) {
	b, err := os.ReadFile(OperatorNamespacePath)
	if err != nil {
		return "", fmt.Errorf("read operator namespace from %q: %w", OperatorNamespacePath, err)
	}
	ns := strings.TrimSpace(string(b))
	if ns == "" {
		return "", fmt.Errorf("operator namespace file %q is empty", OperatorNamespacePath)
	}
	return ns, nil
}
