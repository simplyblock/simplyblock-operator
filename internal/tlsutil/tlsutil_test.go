package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildStorageNodeAPIClientTrustsServingCert(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()

	caPath := writeCertPEM(t, server.Certificate())

	c, err := BuildStorageNodeAPIClient("default", caPath)
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
	if _, err := BuildStorageNodeAPIClient("default", caPath); err == nil {
		t.Fatalf("expected error on garbage CA bundle")
	} else if !strings.Contains(err.Error(), "no usable certificates") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildStorageNodeAPIClientMissingCAFile(t *testing.T) {
	if _, err := BuildStorageNodeAPIClient("default", "/no/such/file"); err == nil {
		t.Fatalf("expected error on missing CA file")
	}
}

func TestBuildStorageNodeAPIClientShape(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	caPath := writeCertPEM(t, server.Certificate())

	c, err := BuildStorageNodeAPIClient("kube-storage", caPath)
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

func TestBuildWebAPIClientShape(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	caPath := writeCertPEM(t, server.Certificate())

	c, err := BuildWebAPIClient("simplyblock", caPath)
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
