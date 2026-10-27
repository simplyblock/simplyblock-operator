// Tests for the file-backed TLS dialer: that it reads its CA bundle and client
// keypair on every attempt, presents the keypair to a hub that requires one,
// and never falls back to plaintext when a file it was told to use is missing.
package link

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testCA signs the hub's and the peers' certificates in these tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "link-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue signs a leaf for name with the given usage, returning it as a
// tls.Certificate and as PEM files' contents.
func (ca *testCA) issue(t *testing.T, name string, usage x509.ExtKeyUsage) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("issue certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load issued keypair: %v", err)
	}
	return pair, certPEM, keyPEM
}

// tlsHub serves TLS for "hub.test" on loopback and reports each handshake's
// outcome as the server saw it. Under TLS 1.3 a client finishes its handshake
// before the server has judged the client certificate, so the server's view is
// the one that says whether a peer was let in.
func tlsHub(t *testing.T, ca *testCA, requireClientCert bool) (string, <-chan error) {
	t.Helper()
	serving, _, _ := ca.issue(t, "hub.test", x509.ExtKeyUsageServerAuth)
	cfg := &tls.Config{Certificates: []tls.Certificate{serving}, MinVersion: tls.VersionTLS13}
	if requireClientCert {
		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	handshakes := make(chan error, 16)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				handshakes <- conn.(*tls.Conn).Handshake()
			}()
		}
	}()
	return listener.Addr().String(), handshakes
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func dialOnce(t *testing.T, dial Dialer) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func serverSaw(t *testing.T, handshakes <-chan error) error {
	t.Helper()
	select {
	case err := <-handshakes:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the hub saw no handshake")
		return nil
	}
}

// On a fresh install the peer starts before its CA bundle is published, so the
// dialer has to read the bundle per attempt rather than once.
func TestTLSFileDialerPicksUpACABundleWrittenAfterIt(t *testing.T) {
	ca := newTestCA(t)
	addr, _ := tlsHub(t, ca, false)
	caFile := filepath.Join(t.TempDir(), "ca.crt")

	dial, err := TLSFileDialer(addr, TLSFiles{CAFile: caFile, ServerName: "hub.test"})
	if err != nil {
		t.Fatalf("TLSFileDialer: %v", err)
	}
	if err := dialOnce(t, dial); err == nil {
		t.Fatal("dialed without a CA bundle on disk")
	}

	writeFile(t, caFile, ca.pem)
	if err := dialOnce(t, dial); err != nil {
		t.Fatalf("dial after the bundle arrived: %v", err)
	}
}

// A missing bundle is a reason to retry, never a reason to send the bearer
// token in the clear: the dialer must not open a plaintext connection.
func TestTLSFileDialerNeverFallsBackToPlaintext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	dial, err := TLSFileDialer(listener.Addr().String(),
		TLSFiles{CAFile: filepath.Join(t.TempDir(), "absent.crt")})
	if err != nil {
		t.Fatalf("TLSFileDialer: %v", err)
	}
	if err := dialOnce(t, dial); err == nil {
		t.Fatal("dial succeeded without a CA bundle")
	}
	select {
	case <-accepted:
		t.Fatal("the dialer opened a connection with no CA bundle to verify the hub")
	case <-time.After(200 * time.Millisecond):
	}
}

// Under mutual TLS the peer presents the client certificate it was issued, and
// a hub requiring one admits it.
func TestTLSFileDialerPresentsItsClientCertificate(t *testing.T) {
	ca := newTestCA(t)
	addr, handshakes := tlsHub(t, ca, true)
	dir := t.TempDir()
	_, certPEM, keyPEM := ca.issue(t, "simplyblock-csi-node", x509.ExtKeyUsageClientAuth)
	files := TLSFiles{
		CAFile:     filepath.Join(dir, "ca.crt"),
		CertFile:   filepath.Join(dir, "tls.crt"),
		KeyFile:    filepath.Join(dir, "tls.key"),
		ServerName: "hub.test",
	}
	writeFile(t, files.CAFile, ca.pem)
	writeFile(t, files.CertFile, certPEM)
	writeFile(t, files.KeyFile, keyPEM)

	dial, err := TLSFileDialer(addr, files)
	if err != nil {
		t.Fatalf("TLSFileDialer: %v", err)
	}
	if err := dialOnce(t, dial); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := serverSaw(t, handshakes); err != nil {
		t.Fatalf("the hub refused the client certificate: %v", err)
	}
}

// The negative of the above: without a keypair the same hub turns the peer
// away, which is what makes the previous test mean anything.
func TestTLSFileDialerWithoutAKeypairIsRefusedByAHubRequiringOne(t *testing.T) {
	ca := newTestCA(t)
	addr, handshakes := tlsHub(t, ca, true)
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	writeFile(t, caFile, ca.pem)

	dial, err := TLSFileDialer(addr, TLSFiles{CAFile: caFile, ServerName: "hub.test"})
	if err != nil {
		t.Fatalf("TLSFileDialer: %v", err)
	}
	_ = dialOnce(t, dial)
	if err := serverSaw(t, handshakes); err == nil {
		t.Fatal("a hub requiring a client certificate admitted a peer that presented none")
	}
}

// Half a keypair is a configuration error, caught when the dialer is built
// rather than as handshakes that fail for a reason the log does not name.
func TestTLSFileDialerRefusesHalfAKeypair(t *testing.T) {
	for _, files := range []TLSFiles{
		{CAFile: "ca.crt", CertFile: "tls.crt"},
		{CAFile: "ca.crt", KeyFile: "tls.key"},
	} {
		if _, err := TLSFileDialer("hub.test:9500", files); err == nil {
			t.Errorf("TLSFileDialer accepted %+v", files)
		}
	}
}

// Plaintext is a separate dialer the caller asks for by name, so an empty CA
// file is a configuration error here rather than a quiet downgrade.
func TestTLSFileDialerNeedsACABundleAndAnAddress(t *testing.T) {
	if _, err := TLSFileDialer("hub.test:9500", TLSFiles{}); err == nil {
		t.Error("TLSFileDialer accepted no CA bundle")
	}
	if _, err := TLSFileDialer("", TLSFiles{CAFile: "ca.crt"}); err == nil {
		t.Error("TLSFileDialer accepted no address")
	}
}
