// The connection the atlas-lib client is handed.
//
// Two clients reach the same control plane from this process: the one in this
// package, which resolves TLS from the pod's environment and its mounted
// certificates, and the atlas-lib one the data-protection band writes through.
// Only the first of them was ever told how. The second was given the endpoint
// alone, so it verified this deployment's CA against the system trust store and
// presented nothing where a certificate was required.

package webapi

import (
	"os"
	"path/filepath"
	"testing"

	atlaskube "github.com/simplyblock/atlas/kube"
)

// TestTheAtlasClientIsGivenTheSameConnection covers the handover.
//
// The connection is the one that follows the ControlPlane per request, so the
// atlas-lib client reaches a control plane that serves TLS without having been
// built any differently from one that does not.
func TestTheAtlasClientIsGivenTheSameConnection(t *testing.T) {
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)
	nsPath, caPath, certPath, keyPath := writeNamespaceAndCertPair(t)
	pointTLSPaths(t, nsPath, caPath, certPath, keyPath)

	startup := NewClient()
	if startup.initErr != nil {
		t.Fatalf("the startup client: %v", startup.initErr)
	}

	// The assertion is on the Config the band is built from, not on the helper
	// that fills it in: a helper that returns the right transport and is not
	// wired into the Config is the same unverified connection.
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("a-token"), 0o600); err != nil {
		t.Fatalf("write the token: %v", err)
	}
	origTokenPath := atlaskube.ServiceAccountTokenPath
	t.Cleanup(func() { atlaskube.ServiceAccountTokenPath = origTokenPath })
	atlaskube.ServiceAccountTokenPath = tokenPath

	cfg, err := ControlPlaneConfig()
	if err != nil {
		t.Fatalf("ControlPlaneConfig: %v", err)
	}
	if cfg.Endpoint != "http://"+defaultHost {
		t.Errorf("the atlas-lib client is pointed at %q, want the default address the policy decides the scheme of",
			cfg.Endpoint)
	}

	carried := cfg.Transport
	if carried == nil {
		t.Fatal("the atlas-lib client is handed no transport, so it builds the default one")
	}
	if _, ok := carried.(*adaptiveTransport); !ok {
		t.Fatalf("the transport is a %T, want the one that follows the ControlPlane", carried)
	}
	if got := transportOf(startup); got == nil {
		t.Error("the startup client carries no transport to compare against")
	}
}

// A nil client is not a panic. ControlPlaneConfig builds one and reads it back
// in the same breath, so this is defense rather than a reachable path, but it is
// a nil dereference in the operator's startup if it ever becomes one.
func TestTransportOfNilIsNil(t *testing.T) {
	if transportOf(nil) != nil {
		t.Error("a nil client reported a transport")
	}
}
