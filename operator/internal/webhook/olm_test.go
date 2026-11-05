package webhook

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMountedCertWaiterClosesReadyOnceBothFilesExist(t *testing.T) {
	dir := t.TempDir()
	w := &mountedCertWaiter{certDir: dir, ready: make(chan struct{}), interval: 10 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()

	// Only the certificate: not ready yet.
	writeFile(t, filepath.Join(dir, "tls.crt"), "cert")
	select {
	case <-w.ready:
		t.Fatal("ready closed before the key was mounted")
	case <-time.After(50 * time.Millisecond):
	}

	writeFile(t, filepath.Join(dir, "tls.key"), "key")
	select {
	case <-w.ready:
	case <-ctx.Done():
		t.Fatal("ready not closed after both files were mounted")
	}
	if err := <-done; err != nil {
		t.Fatalf("Start returned %v", err)
	}
}

func TestMountedCertWaiterIgnoresEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "tls.crt"), "")
	writeFile(t, filepath.Join(dir, "tls.key"), "")
	if certFilesPresent(dir) {
		t.Fatal("empty files counted as a mounted certificate")
	}
}

func TestMountedCertWaiterStopsWithContext(t *testing.T) {
	w := &mountedCertWaiter{certDir: t.TempDir(), ready: make(chan struct{}), interval: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start returned %v", err)
	}
	select {
	case <-w.ready:
		t.Fatal("ready closed without a certificate")
	default:
	}
}

func TestMountedCertWaiterRunsOnEveryReplica(t *testing.T) {
	if (&mountedCertWaiter{}).NeedLeaderElection() {
		t.Fatal("the waiter must run on every replica, not only the leader")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
