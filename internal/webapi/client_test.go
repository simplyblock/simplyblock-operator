package webapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/tlsutil"
)

func TestNewClientDefaultsToHTTP(t *testing.T) {
	t.Setenv("SB_TLS_SERVE", "")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")

	c := NewClient()
	if c.BaseURL != "http://simplyblock-webappapi:5000" {
		t.Fatalf("BaseURL = %q, want http://simplyblock-webappapi:5000", c.BaseURL)
	}
	if c.HttpClient == nil {
		t.Fatalf("HttpClient unset")
	}
	if c.initErr != nil {
		t.Fatalf("unexpected initErr: %v", c.initErr)
	}
}

func TestNewClientTLSEnabledFlipsScheme(t *testing.T) {
	t.Setenv("SB_TLS_SERVE", "1")
	t.Setenv("SB_TLS_CONNECT", "")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)

	c := NewClient()
	if !strings.HasPrefix(c.BaseURL, "https://") {
		t.Fatalf("BaseURL = %q, want https:// scheme when TLS enabled", c.BaseURL)
	}
	// CA bundle won't exist in unit tests — initErr should be set so callers
	// surface a real error rather than silently using a misconfigured client.
	if c.initErr == nil {
		t.Fatalf("expected initErr when CA bundle is unavailable in unit tests")
	}
}

func TestNewClientInitErrSurfacesFromDo(t *testing.T) {
	t.Setenv("SB_TLS_SERVE", "1")
	t.Setenv("SB_TLS_CONNECT", "")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)

	c := NewClient()
	if c.initErr == nil {
		t.Skip("CA bundle unexpectedly present in test environment")
	}

	_, _, err := c.Do(context.Background(), "secret", http.MethodGet, "/api/v2/anything", nil)
	if err == nil {
		t.Fatalf("expected init error from Do")
	}
	if !strings.Contains(err.Error(), "webapi client init") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewClientExplicitURLBypassesEnv(t *testing.T) {
	t.Setenv("SB_TLS_SERVE", "1")
	t.Setenv("SB_TLS_CONNECT", "")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "https://override.example/")
	resetTLSClientCacheForTest(t)

	c := NewClient("http://explicit.example:1234")
	if c.BaseURL != "http://explicit.example:1234" {
		t.Fatalf("explicit baseURL not honored: %q", c.BaseURL)
	}
}

func TestNewClientTLSMutualEnabledUsesClientCertificate(t *testing.T) {
	t.Setenv("SB_TLS_SERVE", "1")
	t.Setenv("SB_TLS_CONNECT", "authenticated")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)

	origNamespacePath := tlsutil.OperatorNamespacePath
	origCAPath := tlsutil.ServiceCABundlePath
	origCertPath := tlsutil.ServiceClientCertificatePath
	origKeyPath := tlsutil.ServiceClientKeyPath
	t.Cleanup(func() {
		tlsutil.OperatorNamespacePath = origNamespacePath
		tlsutil.ServiceCABundlePath = origCAPath
		tlsutil.ServiceClientCertificatePath = origCertPath
		tlsutil.ServiceClientKeyPath = origKeyPath
	})

	nsPath, caPath, certPath, keyPath := writeNamespaceAndCertPair(t)
	tlsutil.OperatorNamespacePath = nsPath
	tlsutil.ServiceCABundlePath = caPath
	tlsutil.ServiceClientCertificatePath = certPath
	tlsutil.ServiceClientKeyPath = keyPath

	c := NewClient()
	if c.initErr != nil {
		t.Fatalf("unexpected initErr: %v", c.initErr)
	}
	if !strings.HasPrefix(c.BaseURL, "https://") {
		t.Fatalf("BaseURL = %q, want https:// scheme when TLS enabled", c.BaseURL)
	}

	tr, ok := c.HttpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T, want *http.Transport", c.HttpClient.Transport)
	}
	if len(tr.TLSClientConfig.Certificates) != 1 {
		t.Fatalf("Certificates len = %d, want 1", len(tr.TLSClientConfig.Certificates))
	}
}

// resetTLSClientCacheForTest forces the next NewClient call to rebuild its
// cached TLS client so test order doesn't leak state.
func resetTLSClientCacheForTest(t *testing.T) {
	t.Helper()
	tlsClientCacheMu.Lock()
	defer tlsClientCacheMu.Unlock()
	tlsClient = nil
	tlsClientErr = nil
	resetTLSClientOnce()
}

func writeNamespaceAndCertPair(t *testing.T) (string, string, string, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "simplyblock-operator-client",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	for path, data := range map[string][]byte{
		nsPath:   []byte("simplyblock\n"),
		caPath:   certPEM,
		certPath: certPEM,
		keyPath:  keyPEM,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	return nsPath, caPath, certPath, keyPath
}
