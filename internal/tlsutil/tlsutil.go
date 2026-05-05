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

// ServiceClientCertPath / ServiceClientKeyPath are where the operator pod
// mounts its own keypair for presenting as a client certificate to
// services that require mutual TLS (i.e. SB_TLS_CLIENT_AUTH=required on the
// server side / SB_TLS_CONNECT=authenticated on the client side).
const (
	ServiceClientCertPath = "/etc/simplyblock/tls/tls.crt"
	ServiceClientKeyPath  = "/etc/simplyblock/tls/tls.key"
)

// OperatorNamespacePath holds the path the in-pod service-account namespace
// is mounted at. Overridable for tests.
var OperatorNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// ClientOptions tunes the TLS client built by the helpers in this package.
type ClientOptions struct {
	// CABundlePath is the path to the PEM bundle that signs the server cert.
	CABundlePath string
	// Mutual enables mutual TLS by loading a client keypair on every
	// handshake. When true, ClientCertPath / ClientKeyPath must point at a
	// usable PEM-encoded cert/key pair.
	Mutual         bool
	ClientCertPath string
	ClientKeyPath  string
}

// BuildStorageNodeAPIClient returns an *http.Client that trusts the CA at
// caPath and pins ServerName to the storage-node API service DNS name in the
// given namespace. This lets the operator dial a pod/host IP directly while
// still passing hostname verification against the service-ca-issued cert,
// whose SAN is the service DNS name. Timeout is short (3s) since this is used
// for reachability probes.
func BuildStorageNodeAPIClient(namespace string, opts ClientOptions) (*http.Client, error) {
	c, err := buildServiceAPIClient("simplyblock-storage-node-api", namespace, opts)
	if err != nil {
		return nil, err
	}
	c.Timeout = 3 * time.Second
	return c, nil
}

// BuildWebAPIClient returns an *http.Client that trusts the CA at caPath and
// pins ServerName to the simplyblock-webappapi service DNS name. Timeout
// matches the previous default for the cluster control-plane API client.
func BuildWebAPIClient(namespace string, opts ClientOptions) (*http.Client, error) {
	c, err := buildServiceAPIClient("simplyblock-webappapi", namespace, opts)
	if err != nil {
		return nil, err
	}
	c.Timeout = 30 * time.Second
	return c, nil
}

func buildServiceAPIClient(serviceName, namespace string, opts ClientOptions) (*http.Client, error) {
	caPEM, err := os.ReadFile(opts.CABundlePath)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %q: %w", opts.CABundlePath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA bundle at %q contains no usable certificates", opts.CABundlePath)
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: fmt.Sprintf("%s.%s.svc", serviceName, namespace),
	}

	if opts.Mutual {
		// Probe once at construction time so a misconfigured pod fails fast
		// instead of erroring on the first request.
		if _, err := tls.LoadX509KeyPair(opts.ClientCertPath, opts.ClientKeyPath); err != nil {
			return nil, fmt.Errorf("load client keypair (%q, %q): %w", opts.ClientCertPath, opts.ClientKeyPath, err)
		}
		certPath := opts.ClientCertPath
		keyPath := opts.ClientKeyPath
		// Re-read on every handshake so cert-manager rotations take effect
		// without requiring a pod restart.
		tlsConfig.GetClientCertificate = func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certPath, keyPath)
			if err != nil {
				return nil, fmt.Errorf("reload client keypair: %w", err)
			}
			return &cert, nil
		}
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
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
