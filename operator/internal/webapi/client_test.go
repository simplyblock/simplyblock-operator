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

// The environment no longer says anything about TLS. A deployment that still
// sets the retired variables gets the same client as one that does not.
func TestNewClientIgnoresTheRetiredTLSVariables(t *testing.T) {
	t.Setenv("SB_TLS_SERVE", "1")
	t.Setenv("SB_TLS_CONNECT", "authenticated")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)

	c := NewClient()
	if c.BaseURL != "http://simplyblock-webappapi:5000" {
		t.Fatalf("BaseURL = %q, want the plain default whatever the environment says", c.BaseURL)
	}
	if c.initErr != nil {
		t.Fatalf("unexpected initErr: %v", c.initErr)
	}
	if c.HttpClient == nil {
		t.Fatal("HttpClient unset")
	}
}

// An HTTPS address is a request for TLS, and the CA is read when the client is
// built, so a pod without one reports it rather than dialing unverified.
func TestNewClientForAnHTTPSAddressNeedsTheCA(t *testing.T) {
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)

	c := NewClient("https://simplyblock-webappapi.simplyblock.svc.cluster.local:5000")
	// The CA bundle won't exist in unit tests, so initErr is how the caller
	// finds out rather than the client silently dropping back to plaintext.
	if c.initErr == nil {
		t.Fatalf("expected initErr when CA bundle is unavailable in unit tests")
	}

	_, _, err := c.Do(context.Background(), http.MethodGet, "/api/v2/anything", nil)
	if err == nil || !strings.Contains(err.Error(), "webapi client init") {
		t.Fatalf("unexpected error from Do: %v", err)
	}
}

func TestNewClientExplicitURLBypassesEnv(t *testing.T) {
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "https://override.example/")
	resetTLSClientCacheForTest(t)

	c := NewClient("http://explicit.example:1234")
	if c.BaseURL != "http://explicit.example:1234" {
		t.Fatalf("explicit baseURL not honored: %q", c.BaseURL)
	}
}

// The client certificate is presented where the pod mounts one. Mutual TLS is
// decided by the ControlPlane, and the chart mounts the pair exactly when it asks
// for it, so the files are the pod's evidence of what to present.
func TestTheTLSClientPresentsTheMountedCertificate(t *testing.T) {
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)
	nsPath, caPath, certPath, keyPath := writeNamespaceAndCertPair(t)
	pointTLSPaths(t, nsPath, caPath, certPath, keyPath)

	c := NewClient("https://simplyblock-webappapi.simplyblock.svc.cluster.local:5000")
	if c.initErr != nil {
		t.Fatalf("unexpected initErr: %v", c.initErr)
	}
	tr, ok := c.HttpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T, want *http.Transport", c.HttpClient.Transport)
	}
	if len(tr.TLSClientConfig.Certificates) != 1 {
		t.Fatalf("Certificates len = %d, want 1", len(tr.TLSClientConfig.Certificates))
	}
}

// Without the pair the connection is TLS and anonymous, which is a
// deployment that serves TLS without asking for client certificates.
func TestTheTLSClientPresentsNothingWhereNoneIsMounted(t *testing.T) {
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)
	nsPath, caPath, certPath, keyPath := writeNamespaceAndCertPair(t)
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	pointTLSPaths(t, nsPath, caPath, certPath, keyPath)

	c := NewClient("https://simplyblock-webappapi.simplyblock.svc.cluster.local:5000")
	if c.initErr != nil {
		t.Fatalf("unexpected initErr: %v", c.initErr)
	}
	tr := c.HttpClient.Transport.(*http.Transport)
	if len(tr.TLSClientConfig.Certificates) != 0 {
		t.Fatalf("presented %d certificates with none mounted", len(tr.TLSClientConfig.Certificates))
	}
}

// A failed build is not remembered. The material arrives with the pod's
// volumes, and an operator that asked before it was there would otherwise stay
// unable to reach a control plane that has been answering for hours.
func TestAFailedTLSBuildIsRetried(t *testing.T) {
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)
	nsPath, caPath, certPath, keyPath := writeNamespaceAndCertPair(t)
	pointTLSPaths(t, nsPath, "/does/not/exist.crt", certPath, keyPath)

	if _, err := cachedTLSClient(); err == nil {
		t.Fatal("expected an error with no CA bundle")
	}

	tlsutil.ServiceCABundlePath = caPath
	if _, err := cachedTLSClient(); err != nil {
		t.Fatalf("the build was not retried once the CA appeared: %v", err)
	}
}

// pointTLSPaths aims the mounted-material paths at a test's files.
func pointTLSPaths(t *testing.T, ns, ca, cert, key string) {
	t.Helper()
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
	tlsutil.OperatorNamespacePath = ns
	tlsutil.ServiceCABundlePath = ca
	tlsutil.ServiceClientCertificatePath = cert
	tlsutil.ServiceClientKeyPath = key
}

// resetTLSClientCacheForTest forces the next NewClient call to rebuild its
// cached TLS client so test order doesn't leak state.
func resetTLSClientCacheForTest(t *testing.T) {
	t.Helper()
	tlsClientCacheMu.Lock()
	defer tlsClientCacheMu.Unlock()
	tlsClient = nil
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

func TestNewStreamClientHasNoTimeout(t *testing.T) {
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://example:1234")

	sc, err := NewStreamClient()
	if err != nil {
		t.Fatalf("NewStreamClient: %v", err)
	}
	if sc.BaseURL != "http://example:1234" {
		t.Errorf("BaseURL = %q, want http://example:1234", sc.BaseURL)
	}
	if sc.Client == nil {
		t.Fatal("Client unset")
	}
	// A fixed timeout would sever a long-lived stream.
	if sc.Client.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0 (no timeout for streams)", sc.Client.Timeout)
	}
}
