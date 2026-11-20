// Tests for how the driver's agent chooses its dialer: plaintext only when it
// was configured with no CA bundle at all, and TLS, retried until the bundle is
// there, whenever it was given one.
package csilink

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// plaintextListener accepts on loopback and reports whether anything
// connected.
func plaintextListener(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
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
	return listener.Addr().String(), accepted
}

// The CA bundle is mounted from a Secret that can be populated after the plugin
// starts. A bundle that is not there yet is a reason to retry. Dialing
// plaintext instead would hand the token to whatever is on the path.
func TestAMissingCABundleIsNotAReasonToDialPlaintext(t *testing.T) {
	addr, accepted := plaintextListener(t)

	dial, err := dialer(Config{
		HubAddress: addr,
		CAFile:     filepath.Join(t.TempDir(), "ca.crt"),
	})
	if err != nil {
		t.Fatalf("dialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if conn, err := dial(ctx); err == nil {
		_ = conn.Close()
		t.Fatal("dialed without the CA bundle the agent was configured with")
	}
	select {
	case <-accepted:
		t.Fatal("the agent opened a plaintext connection while its CA bundle was missing")
	case <-time.After(200 * time.Millisecond):
	}
}

// A deployment with TLS off passes no CA bundle, and that alone is what dials
// plaintext.
func TestNoCABundleConfiguredDialsPlaintext(t *testing.T) {
	addr, accepted := plaintextListener(t)

	dial, err := dialer(Config{HubAddress: addr})
	if err != nil {
		t.Fatalf("dialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("the listener saw no connection")
	}
}

// A client certificate without its key, or the reverse, is refused when the
// agent starts rather than surfacing as handshakes that fail for no named
// reason.
func TestHalfAClientKeypairIsRefused(t *testing.T) {
	for _, cfg := range []Config{
		{HubAddress: "hub.test:9500", CAFile: "ca.crt", CertFile: "tls.crt"},
		{HubAddress: "hub.test:9500", CAFile: "ca.crt", KeyFile: "tls.key"},
	} {
		if _, err := dialer(cfg); err == nil {
			t.Errorf("dialer accepted %+v", cfg)
		}
	}
}
