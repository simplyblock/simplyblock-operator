package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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

	c, err := BuildStorageNodeAPIClient("default", ClientOptions{CABundlePath: caPath})
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
	if _, err := BuildStorageNodeAPIClient("default", ClientOptions{CABundlePath: caPath}); err == nil {
		t.Fatalf("expected error on garbage CA bundle")
	} else if !strings.Contains(err.Error(), "no usable certificates") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildStorageNodeAPIClientMissingCAFile(t *testing.T) {
	if _, err := BuildStorageNodeAPIClient("default", ClientOptions{CABundlePath: "/no/such/file"}); err == nil {
		t.Fatalf("expected error on missing CA file")
	}
}

func TestBuildStorageNodeAPIClientShape(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	caPath := writeCertPEM(t, server.Certificate())

	c, err := BuildStorageNodeAPIClient("kube-storage", ClientOptions{CABundlePath: caPath})
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
	if tr.TLSClientConfig.GetClientCertificate != nil {
		t.Fatalf("GetClientCertificate set without Mutual=true")
	}
}

func TestBuildWebAPIClientShape(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	caPath := writeCertPEM(t, server.Certificate())

	c, err := BuildWebAPIClient("simplyblock", ClientOptions{CABundlePath: caPath})
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

func TestBuildClientMutualMissingKeypair(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	caPath := writeCertPEM(t, server.Certificate())

	_, err := BuildWebAPIClient("simplyblock", ClientOptions{
		CABundlePath:   caPath,
		Mutual:         true,
		ClientCertPath: "/no/such/cert",
		ClientKeyPath:  "/no/such/key",
	})
	if err == nil {
		t.Fatalf("expected error when client keypair is missing")
	}
	if !strings.Contains(err.Error(), "load client keypair") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildClientMutualPresentsCert(t *testing.T) {
	// Server requires client auth and verifies against its own CA pool.
	clientCA, clientCert, clientKey := generateClientCert(t)

	serverCertPool := x509.NewCertPool()
	serverCertPool.AddCert(clientCA)

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 {
			t.Errorf("server saw no client cert")
		}
		w.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  serverCertPool,
	}
	server.StartTLS()
	defer server.Close()

	caPath := writeCertPEM(t, server.Certificate())
	certPath := writeFile(t, "tls.crt", clientCert)
	keyPath := writeFile(t, "tls.key", clientKey)

	c, err := BuildWebAPIClient("default", ClientOptions{
		CABundlePath:   caPath,
		Mutual:         true,
		ClientCertPath: certPath,
		ClientKeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("BuildWebAPIClient: %v", err)
	}
	tr := c.Transport.(*http.Transport)
	tr.TLSClientConfig.ServerName = "example.com"

	resp, err := c.Get(server.URL)
	if err != nil {
		t.Fatalf("mutual TLS request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
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

// generateClientCert returns a self-signed CA certificate plus a leaf cert+key
// that is signed by it and intended to be used as a TLS client certificate.
// PEM-encoded bytes are returned for the leaf so they can be written to disk
// and loaded with tls.LoadX509KeyPair. The CA *x509.Certificate is returned so
// the caller can stuff it into a CertPool for server-side verification.
func generateClientCert(t *testing.T) (*x509.Certificate, []byte, []byte) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca cert: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return caCert, certPEM, keyPEM
}

func writeFile(t *testing.T, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}
