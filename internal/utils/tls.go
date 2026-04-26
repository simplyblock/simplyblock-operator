package utils

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// StorageNodeAPICAPath is where the operator pod mounts the CA bundle that
// signs the storage-node API serving certificate.
const StorageNodeAPICAPath = "/etc/simplyblock/tls/ca.crt"

// BuildStorageNodeAPIClient returns an *http.Client that trusts the CA at
// caPath and pins ServerName to the storage-node API service DNS name in the
// given namespace. This lets the operator dial a pod/host IP directly while
// still passing hostname verification against the service-ca-issued cert,
// whose SAN is the service DNS name.
func BuildStorageNodeAPIClient(namespace, caPath string) (*http.Client, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %q: %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA bundle at %q contains no usable certificates", caPath)
	}

	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    pool,
				ServerName: fmt.Sprintf("simplyblock-storage-node-api.%s.svc", namespace),
			},
		},
	}, nil
}
