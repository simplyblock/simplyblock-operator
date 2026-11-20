// Tests for the two parts of the hub with behavior worth pinning: the
// certificate reloader, which decides when a keypair on disk has been rotated
// (getting that wrong means a long-lived listener serves an expired certificate
// until the operator happens to restart), and which ServiceAccount may link as
// which kind of peer.
package csilink

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"maps"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/simplyblock/atlas/link"
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

// Each kind is open to its own ServiceAccount only. The metadata server pod's
// kind is the one export calls are relayed through, so a node or controller
// token registering as it would receive export calls it never assembled.
func TestEachPeerKindIsOpenOnlyToItsOwnServiceAccount(t *testing.T) {
	accounts := serviceAccounts(Config{
		Namespace:                "simplyblock",
		NodeServiceAccount:       "simplyblock-csi-node-sa",
		ControllerServiceAccount: "simplyblock-csi-controller-sa",
		MDSServiceAccount:        "simplyblock-csi-mds-sa",
	})
	want := map[link.PeerKind][]string{
		link.PeerKindNode:       {"simplyblock/simplyblock-csi-node-sa"},
		link.PeerKindController: {"simplyblock/simplyblock-csi-controller-sa"},
		link.PeerKindMDS:        {"simplyblock/simplyblock-csi-mds-sa"},
	}
	if !maps.EqualFunc(accounts, want, slices.Equal) {
		t.Errorf("serviceAccounts = %v, want %v", accounts, want)
	}
}

// Without a metadata server ServiceAccount the kind is closed: the map stays
// non-empty, which makes every kind it does not name a refusal.
func TestMDSKindIsClosedWithoutItsServiceAccount(t *testing.T) {
	accounts := serviceAccounts(Config{
		Namespace:                "simplyblock",
		NodeServiceAccount:       "simplyblock-csi-node-sa",
		ControllerServiceAccount: "simplyblock-csi-controller-sa",
	})
	if _, open := accounts[link.PeerKindMDS]; open {
		t.Errorf("MDS kind open without a ServiceAccount: %v", accounts)
	}
}

// linkCA signs the hub's serving certificate and the peers' client
// certificates in the listener tests.
type linkCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newLinkCA(t *testing.T) *linkCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "csi-link-test-ca"},
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
	return &linkCA{cert: cert, key: key}
}

func (ca *linkCA) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// issue signs a leaf for name and returns it as PEM, certificate then key.
func (ca *linkCA) issue(t *testing.T, name string, usage x509.ExtKeyUsage) ([]byte, []byte) {
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// handshake dials the listener once as a peer verifying "hub.test" against
// roots, presenting client when it is set, and returns the error the hub's
// side of the handshake ended with. Under TLS 1.3 the client finishes first,
// so only the hub's side says whether the peer was let in.
func handshake(t *testing.T, listener net.Listener, roots *x509.CertPool, client *tls.Certificate) error {
	t.Helper()
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		serverErr <- conn.(*tls.Conn).Handshake()
	}()

	cfg := &tls.Config{RootCAs: roots, ServerName: "hub.test", MinVersion: tls.VersionTLS13}
	if client != nil {
		cfg.Certificates = []tls.Certificate{*client}
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp",
		listener.Addr().String(), cfg)
	if err == nil {
		// Reading makes the client wait for the hub's verdict on its
		// certificate rather than closing before the hub has given one.
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, _ = conn.Read(make([]byte, 1))
		_ = conn.Close()
	}

	select {
	case err := <-serverErr:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the hub saw no handshake")
		return nil
	}
}

// The serving certificate is issued in the cluster and mounted from an optional
// Secret, so the operator starts before it exists. The hub has to come up
// anyway and begin serving TLS once it lands. Plaintext in the meantime is not
// an option: the peers would hand it their tokens.
func TestHubStartsBeforeItsCertificateExists(t *testing.T) {
	ca := newLinkCA(t)
	dir := t.TempDir()
	cfg := Config{
		BindAddress: "127.0.0.1:0",
		CertFile:    filepath.Join(dir, "tls.crt"),
		KeyFile:     filepath.Join(dir, "tls.key"),
	}

	listener, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen before the certificate exists: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	if err := handshake(t, listener, roots, nil); err == nil {
		t.Fatal("the hub completed a handshake with no certificate to serve")
	}

	certPEM, keyPEM := ca.issue(t, "hub.test", x509.ExtKeyUsageServerAuth)
	writeFile(t, cfg.CertFile, certPEM)
	writeFile(t, cfg.KeyFile, keyPEM)
	if err := handshake(t, listener, roots, nil); err != nil {
		t.Fatalf("handshake once the certificate arrived: %v", err)
	}
}

// Under mutual TLS the hub admits a peer only with a client certificate its CA
// issued to one of the CSI identities. The token still says which pod and node
// the peer is, and the certificate says the connection comes from a component
// the install issued one to.
func TestHubRequiresAClientCertificateFromItsCA(t *testing.T) {
	ca := newLinkCA(t)
	dir := t.TempDir()
	cfg := Config{
		BindAddress:  "127.0.0.1:0",
		CertFile:     filepath.Join(dir, "tls.crt"),
		KeyFile:      filepath.Join(dir, "tls.key"),
		ClientCAFile: filepath.Join(dir, "ca.crt"),
		ClientNames:  []string{"simplyblock-csi-node", "simplyblock-csi-controller"},
	}
	certPEM, keyPEM := ca.issue(t, "hub.test", x509.ExtKeyUsageServerAuth)
	writeFile(t, cfg.CertFile, certPEM)
	writeFile(t, cfg.KeyFile, keyPEM)
	writeFile(t, cfg.ClientCAFile, ca.pem())

	listener, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)

	clientCert := func(ca *linkCA, name string) *tls.Certificate {
		certPEM, keyPEM := ca.issue(t, name, x509.ExtKeyUsageClientAuth)
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatalf("client keypair: %v", err)
		}
		return &pair
	}

	if err := handshake(t, listener, roots, clientCert(ca, "simplyblock-csi-node")); err != nil {
		t.Errorf("a CSI node certificate from the hub's CA was refused: %v", err)
	}
	if err := handshake(t, listener, roots, nil); err == nil {
		t.Error("a peer with no client certificate was admitted")
	}
	if err := handshake(t, listener, roots, clientCert(ca, "simplyblock-prometheus")); err == nil {
		t.Error("a client certificate issued to another component was admitted")
	}
	if err := handshake(t, listener, roots, clientCert(newLinkCA(t), "simplyblock-csi-node")); err == nil {
		t.Error("a client certificate from another CA was admitted")
	}
}
