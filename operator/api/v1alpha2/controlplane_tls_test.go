// What the TLS block means when it is not there.
//
// The decision these answer is reached from objects that never went past
// admission: a reconciler builds a LocalControlPlane in a test, a client reads
// one through a cache, an older object predates the field. Defaulting is the API
// server's and applies to none of those, so the zero value has to be the closed
// one on its own rather than because something filled it in.

package v1alpha2

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"
)

// An object that says nothing about TLS serves it, which is the whole point of
// spelling the toggles as the disable.
func TestAControlPlaneThatSaysNothingServesTLS(t *testing.T) {
	var local LocalControlPlane
	if !local.ServesTLS() {
		t.Error("a control plane with no TLS block came up in plaintext")
	}
	if !local.RequiresClientCertificate() {
		t.Error("a control plane with no TLS block admitted anonymous callers")
	}
	if got := local.TLSProvider(); got != ControlPlaneTLSCertManager {
		t.Errorf("the issuer defaulted to %q", got)
	}
}

// A nil spec is the same answer. It is reachable: source.local is a pointer, and
// a managed deployment leaves it unset.
func TestANilLocalControlPlaneStillReadsAsClosed(t *testing.T) {
	var local *LocalControlPlane
	if !local.ServesTLS() || !local.RequiresClientCertificate() {
		t.Error("a nil local control plane read as plaintext")
	}
	if got := local.TLSProvider(); got != ControlPlaneTLSCertManager {
		t.Errorf("the issuer of a nil spec is %q", got)
	}
}

// Each toggle is honored on its own, and the narrower one leaves the serving up.
func TestEachToggleIsHonored(t *testing.T) {
	anonymous := LocalControlPlane{TLS: ControlPlaneTLS{EnableMutualTLS: ptr.To(false)}}
	if !anonymous.ServesTLS() {
		t.Error("dropping the client certificate also dropped the serving")
	}
	if anonymous.RequiresClientCertificate() {
		t.Error("a client certificate is still required after it was disabled")
	}
}

// Mutual TLS without serving TLS is not a state to represent, so disabling the
// serving disables it rather than leaving the two to contradict each other.
func TestPlaintextNeverAsksForAClientCertificate(t *testing.T) {
	plaintext := LocalControlPlane{TLS: ControlPlaneTLS{EnableTLS: ptr.To(false)}}
	if plaintext.ServesTLS() {
		t.Fatal("the serving was disabled and the control plane still serves TLS")
	}
	if plaintext.RequiresClientCertificate() {
		t.Error("a plaintext control plane asks its callers for a certificate")
	}
}

// An explicitly named issuer is not overwritten by the default.
func TestANamedIssuerIsKept(t *testing.T) {
	openshift := LocalControlPlane{TLS: ControlPlaneTLS{Provider: ControlPlaneTLSOpenShift}}
	if got := openshift.TLSProvider(); got != ControlPlaneTLSOpenShift {
		t.Errorf("the named issuer became %q", got)
	}
}
