/*
Copyright (c) Arm Limited and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// whitebox test of some functions in initiator.go
package util

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestExecWithTimeoutPositive(t *testing.T) {
	elapsed, err := runExecWithTimeout([]string{"true"}, 10)
	if err != nil {
		t.Fatal("should succeed")
	}
	if elapsed > 3 {
		t.Fatal("timeout error")
	}
}

func TestExecWithTimeoutNegative(t *testing.T) {
	elapsed, err := runExecWithTimeout([]string{"false"}, 10)
	if err == nil {
		t.Fatal("should fail")
	}
	if elapsed > 3 {
		t.Fatal("timeout error")
	}
}

func TestExecWithTimeoutTimeout(t *testing.T) {
	elapsed, err := runExecWithTimeout([]string{"sleep", "10"}, 1)
	if err == nil {
		t.Fatal("should fail")
	}
	if elapsed > 3 {
		t.Fatal("timeout error")
	}
}

func runExecWithTimeout(cmdLine []string, timeout int) (int, error) {
	start := time.Now()
	err := execWithTimeout(context.Background(), cmdLine, timeout)
	elapsed := int(time.Since(start) / time.Second)
	return elapsed, err
}

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

	node, err := NewsimplyBlockClient(context.Background(), "test-cluster", "")
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

	node, err := NewsimplyBlockClient(context.Background(), "test-cluster", "")
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

	node, err := NewsimplyBlockClient(context.Background(), "test-cluster", "")
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

	node, err := NewsimplyBlockClient(context.Background(), "test-cluster", "")
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

	_, err := NewsimplyBlockClient(context.Background(), "test-cluster", "")
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

	node, err := NewsimplyBlockClient(context.Background(), "test-cluster", "")
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

	node, err := NewsimplyBlockClient(context.Background(), "test-cluster", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.API.Credential != testStaticSecret {
		t.Errorf("expected fallback to cluster_secret %q, got %q", testStaticSecret, node.API.Credential)
	}
}

// TestDHCHAPAuthArgsExtractsHostIdentityAndSecrets verifies that the host
// identity and DHCHAP/TLS flags are pulled out of the control-plane-supplied
// connect command line, that a --hostid matching the hostnqn's own UUID is
// synthesized (so it can never collide with the node's file-based default
// hostid, which is paired with a different hostnqn), and that unrelated
// flags (already covered by connectViaNVMe's own args) are ignored.
func TestDHCHAPAuthArgsExtractsHostIdentityAndSecrets(t *testing.T) {
	connect := "sudo nvme connect --reconnect-delay=2 --ctrl-loss-tmo=3600 " +
		"--transport=tcp --traddr=1.2.3.4 --trsvcid=4420 --nqn=nqn.test " +
		"--hostnqn=nqn.2014-08.io.simplyblock:uuid:node-uid " +
		"--dhchap-secret=DHHC-1:00:secret: --dhchap-ctrl-secret=DHHC-1:00:ctrlsecret: --tls"

	got := dhchapAuthArgs(connect)
	want := []string{
		"--hostnqn=nqn.2014-08.io.simplyblock:uuid:node-uid",
		"--dhchap-secret=DHHC-1:00:secret:",
		"--dhchap-ctrl-secret=DHHC-1:00:ctrlsecret:",
		"--tls",
		"--hostid=node-uid",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("dhchapAuthArgs(%q) = %v, want %v", connect, got, want)
	}
}

// TestDHCHAPAuthArgsNoAuthWhenUnneeded verifies that a connect command with no
// host identity or secrets (the common case for a pool without allowed_hosts)
// yields no extra flags.
func TestDHCHAPAuthArgsNoAuthWhenUnneeded(t *testing.T) {
	connect := "sudo nvme connect --transport=tcp --traddr=1.2.3.4 --trsvcid=4420 --nqn=nqn.test"
	if got := dhchapAuthArgs(connect); len(got) != 0 {
		t.Errorf("dhchapAuthArgs(%q) = %v, want empty", connect, got)
	}
}

// TestNodeHostNQNComputesAndCaches verifies NodeHostNQN derives the
// simplyblock-format host NQN from the Node's own UID, and that it's the
// same value on a second call even from a client that would now error (i.e.
// it's actually cached, not recomputed every time).
func TestNodeHostNQNComputesAndCaches(t *testing.T) {
	resetNodeHostNQNCache(t)

	const nodeName = "node-under-test"
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, UID: "node-uid-1234"},
	})

	got := NodeHostNQN(context.Background(), client, nodeName)
	want := "nqn.2014-08.io.simplyblock:uuid:node-uid-1234"
	if got != want {
		t.Fatalf("NodeHostNQN = %q, want %q", got, want)
	}

	// A client that would now error on any call proves the second call used
	// the cache instead of hitting the API again.
	erroringClient := fake.NewSimpleClientset()
	erroringClient.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("client should not be called again")
	})
	if got := NodeHostNQN(context.Background(), erroringClient, nodeName); got != want {
		t.Errorf("second NodeHostNQN call = %q, want cached %q", got, want)
	}
}

// TestNodeHostNQNRetriesAfterFailure verifies a failed lookup is not cached,
// so a later call with a working client still succeeds.
func TestNodeHostNQNRetriesAfterFailure(t *testing.T) {
	resetNodeHostNQNCache(t)

	const nodeName = "node-under-test"
	if got := NodeHostNQN(context.Background(), fake.NewSimpleClientset(), nodeName); got != "" {
		t.Fatalf("NodeHostNQN with no such node = %q, want empty", got)
	}

	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, UID: "node-uid-5678"},
	})
	const want = "nqn.2014-08.io.simplyblock:uuid:node-uid-5678"
	if got := NodeHostNQN(context.Background(), client, nodeName); got != want {
		t.Errorf("NodeHostNQN after a working client = %q, want %q", got, want)
	}
}

// resetNodeHostNQNCache clears NodeHostNQN's process-lifetime cache so tests
// don't leak state into each other.
func resetNodeHostNQNCache(t *testing.T) {
	t.Helper()
	nodeHostNQNMu.Lock()
	nodeHostNQNVal = ""
	nodeHostNQNMu.Unlock()
	t.Cleanup(func() {
		nodeHostNQNMu.Lock()
		nodeHostNQNVal = ""
		nodeHostNQNMu.Unlock()
	})
}
