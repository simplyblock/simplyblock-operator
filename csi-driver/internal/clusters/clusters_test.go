package clusters

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/lvol"
)

// writeTempFile creates a temp file with the given content and registers cleanup.
func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "spdkcsi-test-*")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	_ = f.Close()
	return f.Name()
}

const testStaticSecret = "static-secret"

const testSecretJSON = `{"clusters":[{"cluster_id":"test-cluster","cluster_endpoint":"http://localhost","cluster_secret":"static-secret"}]}` //nolint:lll // unwrappable string/log/signature

const testSecretNoCredJSON = `{"clusters":[{"cluster_id":"test-cluster","cluster_endpoint":"http://localhost","cluster_secret":""}]}` //nolint:lll // unwrappable string/log/signature

// TestCredentialAPITokenUsed verifies that when SPDKCSI_API_TOKEN_PATH points to a
// file containing a valid token, that token is used as the credential instead
// of the cluster_secret from the secret file.
func TestCredentialAPITokenUsed(t *testing.T) {
	secretFile := writeTempFile(t, testSecretJSON)
	tokenFile := writeTempFile(t, "sa-jwt-token")
	t.Setenv("SPDKCSI_SECRET", secretFile)
	t.Setenv("SPDKCSI_API_TOKEN_PATH", tokenFile)

	node, err := Client(context.Background(), "test-cluster", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.API.Credential != "sa-jwt-token" {
		t.Errorf("expected API token %q as credential, got %q", "sa-jwt-token", node.API.Credential)
	}
}

// TestCredentialClusterSecretFallback verifies that when SPDKCSI_API_TOKEN_PATH is
// not set, the cluster_secret from the secret file is used unchanged.
func TestCredentialClusterSecretFallback(t *testing.T) {
	secretFile := writeTempFile(t, testSecretJSON)
	t.Setenv("SPDKCSI_SECRET", secretFile)
	t.Setenv("SPDKCSI_API_TOKEN_PATH", "")

	node, err := Client(context.Background(), "test-cluster", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.API.Credential != testStaticSecret {
		t.Errorf("expected cluster_secret %q, got %q", testStaticSecret, node.API.Credential)
	}
}

// TestCredentialAPITokenWhitespaceTrimmed verifies that leading/trailing
// whitespace in the API token file is stripped before use.
func TestCredentialAPITokenWhitespaceTrimmed(t *testing.T) {
	secretFile := writeTempFile(t, testSecretJSON)
	tokenFile := writeTempFile(t, " tok \n")
	t.Setenv("SPDKCSI_SECRET", secretFile)
	t.Setenv("SPDKCSI_API_TOKEN_PATH", tokenFile)

	node, err := Client(context.Background(), "test-cluster", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.API.Credential != "tok" {
		t.Errorf("expected trimmed token %q, got %q", "tok", node.API.Credential)
	}
}

// TestCredentialAPITokenWithEmptyClusterSecret verifies that API token auth
// succeeds even when cluster_secret is empty in the secret file.
func TestCredentialAPITokenWithEmptyClusterSecret(t *testing.T) {
	secretFile := writeTempFile(t, testSecretNoCredJSON)
	tokenFile := writeTempFile(t, "sa-jwt-token")
	t.Setenv("SPDKCSI_SECRET", secretFile)
	t.Setenv("SPDKCSI_API_TOKEN_PATH", tokenFile)

	node, err := Client(context.Background(), "test-cluster", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.API.Credential != "sa-jwt-token" {
		t.Errorf("expected API token %q, got %q", "sa-jwt-token", node.API.Credential)
	}
}

// TestCredentialBothMissingReturnsError verifies that when SPDKCSI_API_TOKEN_PATH is
// unset and cluster_secret is empty, NewsimplyBlockClient returns an error.
func TestCredentialBothMissingReturnsError(t *testing.T) {
	secretFile := writeTempFile(t, testSecretNoCredJSON)
	t.Setenv("SPDKCSI_SECRET", secretFile)
	t.Setenv("SPDKCSI_API_TOKEN_PATH", "")

	_, err := Client(context.Background(), "test-cluster", "")
	if err == nil {
		t.Fatal("expected error when both cluster_secret and API token are missing, got nil")
	}
	const want = "no cluster_secret and no API token available"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err.Error(), want)
	}
}

// TestCredentialAPITokenFileUnreadableFallsBackToClusterSecret verifies that when
// SPDKCSI_API_TOKEN_PATH points to a nonexistent file, the driver falls back to
// cluster_secret rather than failing silently or crashing.
func TestCredentialAPITokenFileUnreadableFallsBackToClusterSecret(t *testing.T) {
	secretFile := writeTempFile(t, testSecretJSON)
	t.Setenv("SPDKCSI_SECRET", secretFile)
	t.Setenv("SPDKCSI_API_TOKEN_PATH", "/nonexistent/path/to/token")

	node, err := Client(context.Background(), "test-cluster", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.API.Credential != testStaticSecret {
		t.Errorf("expected fallback to cluster_secret %q, got %q", testStaticSecret, node.API.Credential)
	}
}

// TestCredentialAPITokenFileEmptyFallsBackToClusterSecret verifies that when
// SPDKCSI_API_TOKEN_PATH points to a file that is empty (or whitespace-only),
// the driver falls back to cluster_secret.
func TestCredentialAPITokenFileEmptyFallsBackToClusterSecret(t *testing.T) {
	secretFile := writeTempFile(t, testSecretJSON)
	tokenFile := writeTempFile(t, "   \n")
	t.Setenv("SPDKCSI_SECRET", secretFile)
	t.Setenv("SPDKCSI_API_TOKEN_PATH", tokenFile)

	node, err := Client(context.Background(), "test-cluster", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.API.Credential != testStaticSecret {
		t.Errorf("expected fallback to cluster_secret %q, got %q", testStaticSecret, node.API.Credential)
	}
}

// TestReplicationClientReachesTLSControlPlane pins that the replication client
// honors SB_TLS_CONNECT, so the delete-time replica-chain cleanup can reach a
// control plane served over TLS.
//
// Regression: 2026-10-07-repl-client-ignores-tls. deleteRetiredReplicaChain,
// added with the csi-addons work in #548, builds its client via
// ReplicationClient, which set no TLS transport and did not upgrade the endpoint
// scheme. Against a TLS control plane the handshake never completed, so the
// relationship lookup failed and every DeleteVolume failed with it, plain RWO
// volumes included.
func TestReplicationClientReachesTLSControlPlane(t *testing.T) {
	var reached bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	caFile := writeTempFile(t, string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: srv.Certificate().Raw,
	})))
	secret := writeTempFile(t,
		`{"clusters":[{"cluster_id":"test-cluster","cluster_endpoint":"`+
			srv.URL+`","cluster_secret":"s"}]}`)

	t.Setenv("SPDKCSI_SECRET", secret)
	t.Setenv("SPDKCSI_API_TOKEN_PATH", "")
	t.Setenv("SB_TLS_CONNECT", "anonymous")
	t.Setenv("SB_TLS_CERTIFICATE_AUTHORITY", caFile)

	client, err := ReplicationClient(context.Background(), "test-cluster")
	if err != nil {
		t.Fatalf("ReplicationClient: %v", err)
	}
	// The response itself does not matter: a 404 maps to not-found. What the bug
	// broke is reaching the server at all over TLS. The handle must be
	// well-formed (clusterID:poolID:volumeID, each a UUID), because the client
	// validates it before dialing.
	handle := lvol.VolumeHandle(
		"11111111-1111-1111-1111-111111111111:" +
			"22222222-2222-2222-2222-222222222222:" +
			"33333333-3333-3333-3333-333333333333")
	_, _ = client.GetVolumeReplicationRelationship(context.Background(), handle)

	if !reached {
		t.Fatal("replication client never reached the TLS control plane: its transport was not TLS-configured")
	}
}
