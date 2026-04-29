package tlsutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildStorageNodeAPIClientTrustsServingCert(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()

	caPath := writeCertPEM(t, server.Certificate())

	c, err := BuildStorageNodeAPIClient("default", caPath, "", "")
	if err != nil {
		t.Fatalf("BuildStorageNodeAPIClient returned error: %v", err)
	}

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T, want *http.Transport", c.Transport)
	}
	tr.TLSClientConfig.ServerName = "example.com"

	resp, err := c.Get(server.URL)
	if err != nil {
		t.Fatalf("client failed to dial trusted TLS server: %v", err)
	}
	_ = resp.Body.Close()
}

func TestBuildStorageNodeAPIClientRejectsBadCA(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write temp ca: %v", err)
	}
	if _, err := BuildStorageNodeAPIClient("default", caPath, "", ""); err == nil {
		t.Fatalf("expected error on garbage CA bundle")
	} else if !strings.Contains(err.Error(), "no usable certificates") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildStorageNodeAPIClientMissingCAFile(t *testing.T) {
	if _, err := BuildStorageNodeAPIClient("default", "/no/such/file", "", ""); err == nil {
		t.Fatalf("expected error on missing CA file")
	}
}

func TestBuildStorageNodeAPIClientShape(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	caPath := writeCertPEM(t, server.Certificate())

	c, err := BuildStorageNodeAPIClient("kube-storage", caPath, "", "")
	if err != nil {
		t.Fatalf("BuildStorageNodeAPIClient: %v", err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T, want *http.Transport", c.Transport)
	}
	if want := "simplyblock-storage-node-api.kube-storage.svc"; tr.TLSClientConfig.ServerName != want {
		t.Fatalf("ServerName = %q, want %q", tr.TLSClientConfig.ServerName, want)
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d, want %d", tr.TLSClientConfig.MinVersion, tls.VersionTLS12)
	}
	if tr.TLSClientConfig.RootCAs == nil {
		t.Fatalf("RootCAs not set")
	}
}

func TestBuildStorageNodeAPIClientMTLSShape(t *testing.T) {
	caPath, certPath, keyPath := writeSelfSignedCertPairPEM(t)

	c, err := BuildStorageNodeAPIClient("simplyblock", caPath, certPath, keyPath)
	if err != nil {
		t.Fatalf("BuildStorageNodeAPIClient: %v", err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T, want *http.Transport", c.Transport)
	}
	if len(tr.TLSClientConfig.Certificates) != 1 {
		t.Fatalf("Certificates len = %d, want 1", len(tr.TLSClientConfig.Certificates))
	}
}

func TestBuildWebAPIClientShape(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	caPath := writeCertPEM(t, server.Certificate())

	c, err := BuildWebAPIClient("simplyblock", caPath, "", "")
	if err != nil {
		t.Fatalf("BuildWebAPIClient: %v", err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T, want *http.Transport", c.Transport)
	}
	if want := "simplyblock-webappapi.simplyblock.svc"; tr.TLSClientConfig.ServerName != want {
		t.Fatalf("ServerName = %q, want %q", tr.TLSClientConfig.ServerName, want)
	}
}

func TestBuildWebAPIClientMTLSShape(t *testing.T) {
	caPath, certPath, keyPath := writeSelfSignedCertPairPEM(t)

	c, err := BuildWebAPIClient("simplyblock", caPath, certPath, keyPath)
	if err != nil {
		t.Fatalf("BuildWebAPIClient: %v", err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T, want *http.Transport", c.Transport)
	}
	if want := "simplyblock-webappapi.simplyblock.svc"; tr.TLSClientConfig.ServerName != want {
		t.Fatalf("ServerName = %q, want %q", tr.TLSClientConfig.ServerName, want)
	}
	if len(tr.TLSClientConfig.Certificates) != 1 {
		t.Fatalf("Certificates len = %d, want 1", len(tr.TLSClientConfig.Certificates))
	}
}

func TestBuildWebAPIClientMissingClientKeyPair(t *testing.T) {
	caPath, _, _ := writeSelfSignedCertPairPEM(t)
	if _, err := BuildWebAPIClient("simplyblock", caPath, "/no/such/cert", "/no/such/key"); err == nil {
		t.Fatalf("expected error when client certificate pair is missing")
	}
}

func TestDetectOperatorNamespace(t *testing.T) {
	orig := OperatorNamespacePath
	t.Cleanup(func() { OperatorNamespacePath = orig })

	dir := t.TempDir()
	path := filepath.Join(dir, "namespace")
	if err := os.WriteFile(path, []byte("simplyblock\n"), 0o600); err != nil {
		t.Fatalf("write ns: %v", err)
	}
	OperatorNamespacePath = path
	got, err := DetectOperatorNamespace()
	if err != nil {
		t.Fatalf("DetectOperatorNamespace: %v", err)
	}
	if got != "simplyblock" {
		t.Fatalf("namespace = %q, want %q", got, "simplyblock")
	}
}

func TestDetectOperatorNamespaceMissing(t *testing.T) {
	orig := OperatorNamespacePath
	t.Cleanup(func() { OperatorNamespacePath = orig })

	OperatorNamespacePath = "/no/such/path"
	if _, err := DetectOperatorNamespace(); err == nil {
		t.Fatalf("expected error when namespace file is missing")
	}
}

func TestDetectOperatorNamespaceEmpty(t *testing.T) {
	orig := OperatorNamespacePath
	t.Cleanup(func() { OperatorNamespacePath = orig })

	dir := t.TempDir()
	path := filepath.Join(dir, "namespace")
	if err := os.WriteFile(path, []byte("\n  \n"), 0o600); err != nil {
		t.Fatalf("write ns: %v", err)
	}
	OperatorNamespacePath = path
	if _, err := DetectOperatorNamespace(); err == nil {
		t.Fatalf("expected error when namespace file is empty")
	}
}

func writeCertPEM(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		t.Fatalf("write ca pem: %v", err)
	}
	return caPath
}

func writeSelfSignedCertPairPEM(t *testing.T) (string, string, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "simplyblock-client",
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
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	for path, data := range map[string][]byte{
		caPath:   certPEM,
		certPath: certPEM,
		keyPath:  keyPEM,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	return caPath, certPath, keyPath
}
