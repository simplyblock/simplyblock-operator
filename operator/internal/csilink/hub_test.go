// Tests for the hub's certificate reloader, the part of this package with
// behaviour worth pinning: it decides when a keypair on disk has been rotated,
// and getting that decision wrong means a long-lived listener serves an expired
// certificate until the operator happens to restart.
package csilink

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeKeyPair writes a fresh self-signed keypair over the two paths and
// returns the certificate's serial, which is what tells one generation from the
// next.
func writeKeyPair(t *testing.T, certFile, keyFile string, serial int64) *big.Int {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "csi-link-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	if err := os.WriteFile(certFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyFile,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return tmpl.SerialNumber
}

// serialOf reads the serial back out of what the reloader chose to serve.
func serialOf(t *testing.T, cert *tls.Certificate) *big.Int {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse served certificate: %v", err)
	}
	return leaf.SerialNumber
}

func TestCertReloaderReloadsWhenOnlyTheKeysMtimeMoved(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	first := writeKeyPair(t, certFile, keyFile, 1)
	r := &certReloader{certFile: certFile, keyFile: keyFile}

	served, err := r.load()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if got := serialOf(t, served); got.Cmp(first) != 0 {
		t.Fatalf("first load served serial %v, want %v", got, first)
	}
	certMtimeAtFirstLoad := statModTime(t, certFile)

	// Rotate the pair, then put the certificate's mtime back where it was, so
	// only the key's mtime moved. A reloader watching the certificate alone
	// cannot see this and goes on serving the superseded keypair.
	second := writeKeyPair(t, certFile, keyFile, 2)
	if err := os.Chtimes(certFile, certMtimeAtFirstLoad, certMtimeAtFirstLoad); err != nil {
		t.Fatalf("pin certificate mtime: %v", err)
	}
	if got := statModTime(t, certFile); !got.Equal(certMtimeAtFirstLoad) {
		t.Fatalf("certificate mtime is %v, want it pinned at %v", got, certMtimeAtFirstLoad)
	}

	// Defeat the stat interval, which is not what is under test here.
	r.lastLoad = time.Now().Add(-2 * reloadInterval)

	served, err = r.load()
	if err != nil {
		t.Fatalf("load after rotation: %v", err)
	}
	if got := serialOf(t, served); got.Cmp(second) != 0 {
		t.Errorf("after rotating the key, served serial %v, want %v; "+
			"the reloader is deciding staleness from the certificate alone", got, second)
	}
}

func TestCertReloaderKeepsServingWhenAFileGoesMissing(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	first := writeKeyPair(t, certFile, keyFile, 1)
	r := &certReloader{certFile: certFile, keyFile: keyFile}
	if _, err := r.load(); err != nil {
		t.Fatalf("first load: %v", err)
	}

	// Half a keypair is not something to serve, but it is also no reason to
	// start failing handshakes: a rotation caught mid-write is transient.
	if err := os.Remove(keyFile); err != nil {
		t.Fatalf("remove key: %v", err)
	}
	r.lastLoad = time.Now().Add(-2 * reloadInterval)

	served, err := r.load()
	if err != nil {
		t.Fatalf("load with the key missing: %v", err)
	}
	if got := serialOf(t, served); got.Cmp(first) != 0 {
		t.Errorf("served serial %v, want the previously loaded %v", got, first)
	}
}

func TestCertReloaderFailsWhenItNeverLoadedAnything(t *testing.T) {
	dir := t.TempDir()
	r := &certReloader{
		certFile: filepath.Join(dir, "absent.crt"),
		keyFile:  filepath.Join(dir, "absent.key"),
	}
	if _, err := r.load(); err == nil {
		t.Error("load with no files and nothing cached returned no error; " +
			"a listener must not come up believing it has a certificate")
	}
}

func statModTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.ModTime()
}
