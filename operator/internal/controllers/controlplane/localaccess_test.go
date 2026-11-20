// What the operator reaches its own control plane with.
//
// The endpoint is half of it. A control plane serving TLS presents a certificate
// signed by the deployment's own CA, which is in no system trust store, so a
// probe given the address and nothing else fails the handshake rather than the
// request -- and reports it as the control plane not being ready, which is a
// sentence about the wrong component.

package controlplane

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/tlsutil"
)

// mountedCA writes what a mutual-TLS operator pod mounts -- the CA bundle, the
// client certificate, and its key, all three out of one Secret -- and points
// tlsutil at them for the duration of the test.
func mountedCA(t *testing.T) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "simplyblock-certificate-authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("sign the CA: %v", err)
	}

	dir := t.TempDir()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	for name, content := range map[string][]byte{
		"ca.crt": certPEM, "tls.crt": certPEM, "tls.key": keyPEM,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	originalCA := tlsutil.ServiceCABundlePath
	originalCert := tlsutil.ServiceClientCertificatePath
	originalKey := tlsutil.ServiceClientKeyPath
	t.Cleanup(func() {
		tlsutil.ServiceCABundlePath = originalCA
		tlsutil.ServiceClientCertificatePath = originalCert
		tlsutil.ServiceClientKeyPath = originalKey
	})
	tlsutil.ServiceCABundlePath = filepath.Join(dir, "ca.crt")
	tlsutil.ServiceClientCertificatePath = filepath.Join(dir, "tls.crt")
	tlsutil.ServiceClientKeyPath = filepath.Join(dir, "tls.key")
}

// TestTheProbeOfATLSControlPlaneCarriesTheCA is the defect.
//
// Regression: 2026-09-21-the-local-probe-verified-against-the-system-store — the
// install learned to serve TLS and localEndpoint learned to say so, and the
// probe kept the client it had, which was none. Every readiness read failed with
// "x509: certificate signed by unknown authority" and the ControlPlane reported
// AwaitingDependency forever, while the management API beside it was serving and
// answering every other caller in the operator.
func TestTheProbeOfATLSControlPlaneCarriesTheCA(t *testing.T) {
	mountedCA(t)

	access, err := localAccess(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))
	if err != nil {
		t.Fatalf("localAccess: %v", err)
	}
	if access.client == nil {
		t.Fatal("the probe of a TLS control plane verifies against the system trust store, " +
			"which does not hold this deployment's CA")
	}
}

// A plaintext install needs none of it, and must not fail for want of a CA file
// that its pod does not mount.
func TestThePlaintextProbeNeedsNoCA(t *testing.T) {
	original := tlsutil.ServiceCABundlePath
	t.Cleanup(func() { tlsutil.ServiceCABundlePath = original })
	tlsutil.ServiceCABundlePath = filepath.Join(t.TempDir(), "absent")

	access, err := localAccess(aLocalControlPlane(
		simplyblockv1alpha2.ControlPlaneTLS{EnableTLS: ptr.To(false)}))
	if err != nil {
		t.Fatalf("a plaintext control plane could not be reached: %v", err)
	}
	if access.client != nil {
		t.Error("a plaintext probe carries a TLS client")
	}
}

// The address follows the install either way, because it is published as
// status.endpoint and every control-plane call in the operator resolves it.
func TestTheLocalAddressFollowsTheInstall(t *testing.T) {
	mountedCA(t)

	secure, err := localAccess(aLocalControlPlane(simplyblockv1alpha2.ControlPlaneTLS{}))
	if err != nil {
		t.Fatalf("localAccess: %v", err)
	}
	if got := secure.endpoint[:8]; got != "https://" {
		t.Errorf("a TLS control plane is reached over %q", secure.endpoint)
	}

	plain, err := localAccess(aLocalControlPlane(
		simplyblockv1alpha2.ControlPlaneTLS{EnableTLS: ptr.To(false)}))
	if err != nil {
		t.Fatalf("localAccess: %v", err)
	}
	if got := plain.endpoint[:7]; got != "http://" {
		t.Errorf("a plaintext control plane is reached at %q", plain.endpoint)
	}
}
