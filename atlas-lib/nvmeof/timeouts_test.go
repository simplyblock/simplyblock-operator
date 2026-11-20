// The kernel and nvme-cli calls this package makes, against a kernel or a
// process that never answers.
//
// A FIFO with no writer stands in for a device node whose open blocks, and a
// fake nvme-cli that leaves a background process holding its output pipe stands
// in for an nvme-cli the kill cannot reap: in both, the call outlives every
// context it was given.

package nvmeof

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// hangingPath returns a path whose open blocks until something writes to it,
// which nothing in these tests ever does.
func hangingPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stuck")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	return p
}

// returnsWithin runs fn and fails the test if it has not returned after limit,
// leaving the blocked goroutine behind rather than hanging the suite with it.
func returnsWithin(t *testing.T, limit time.Duration, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("%s was still blocked after %s", what, limit)
		return nil
	}
}

// Opening a namespace whose controllers are all in error recovery can block,
// and the readiness check that opens it runs inside the attach and the reconnect
// loop, which must keep going for every other volume.
//
// Regression: 2026-10-04-unbounded-nvme-io.
func TestOpenDeviceGivesUpOnADeviceThatNeverOpens(t *testing.T) {
	p := hangingPath(t)

	err := returnsWithin(t, 3*time.Second, "openDevice", func() error {
		return openDevice(p)
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("openDevice on a stuck device = %v, want an error wrapping context.DeadlineExceeded", err)
	}
}

// fakeNVMe puts an `nvme` executable running script first on PATH.
func fakeNVMe(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nvme"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// An nvme-cli call has to return once its context is done, even when the
// process cannot be reaped. Killing it on the deadline is not enough: the call
// waits for the process and its output, and an nvme-cli stuck in the kernel
// delivers neither.
//
// Regression: 2026-10-04-unbounded-nvme-io.
func TestRunCommandReturnsWhenTheProcessDoesNot(t *testing.T) {
	fakeNVMe(t, "sleep 30 &\necho started")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := returnsWithin(t, 4*time.Second, "runCommand", func() error {
		_, err := runCommand(ctx, "list")
		return err
	})
	if err == nil {
		t.Fatal("runCommand returned no error for a process that never released its output")
	}
}
