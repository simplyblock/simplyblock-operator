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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	atlaskube "github.com/simplyblock/atlas/kube"

	"github.com/simplyblock/simplyblock-operator/internal/tlsutil"
)

// TestTheAtlasClientIsGivenTheSameConnection covers the handover.
func TestTheAtlasClientIsGivenTheSameConnection(t *testing.T) {
	t.Setenv("SB_TLS_SERVE", "1")
	t.Setenv("SB_TLS_CONNECT", "authenticated")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)

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

	nsPath, caPath, certPath, keyPath := writeNamespaceAndCertPair(t)
	tlsutil.OperatorNamespacePath = nsPath
	tlsutil.ServiceCABundlePath = caPath
	tlsutil.ServiceClientCertificatePath = certPath
	tlsutil.ServiceClientKeyPath = keyPath

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
	if !strings.HasPrefix(cfg.Endpoint, "https://") {
		t.Errorf("the atlas-lib client is pointed at %q", cfg.Endpoint)
	}

	carried := cfg.Transport
	if carried == nil {
		t.Fatal("the atlas-lib client is handed no transport, so it builds the default one")
	}
	if got := transportOf(startup); got != carried {
		t.Error("the Config carries a different connection than the startup client's")
	}

	transport, ok := carried.(*http.Transport)
	if !ok {
		t.Fatalf("the transport is a %T", carried)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Error("the transport carries no CA, so this deployment's certificate cannot be verified")
	}
	if len(transport.TLSClientConfig.Certificates) != 1 {
		t.Errorf("the transport presents %d client certificates, want the pod's one",
			len(transport.TLSClientConfig.Certificates))
	}
}

// A plaintext deployment hands over nothing, which is what the atlas-lib client
// takes to mean its own default and is what every existing caller is.
func TestAPlaintextDeploymentHandsOverNoTransport(t *testing.T) {
	t.Setenv("SB_TLS_SERVE", "")
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	resetTLSClientCacheForTest(t)

	if carried := transportOf(NewClient()); carried != nil {
		t.Errorf("a plaintext deployment handed over a %T", carried)
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
