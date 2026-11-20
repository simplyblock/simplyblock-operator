//go:build linux

// The Identify ioctl against a controller device that never answers.
//
// A FIFO with no writer stands in for the device: its open blocks in the kernel
// the way an admin command does against a controller that left the live state
// after it was chosen, and nothing in Go can interrupt either.

package nvme

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// An Identify against a wedged controller used to hold its caller until the
// kernel tore the controller down, and the caller can be the attach or the
// reconnect loop serving every volume on the node.
//
// Regression: 2026-10-04-unbounded-nvme-io.
func TestIdentifyGivesUpOnAControllerThatNeverAnswers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nvme0")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := identifyControllerMNAN(p)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("identifyControllerMNAN on a stuck device = %v, want an error wrapping context.DeadlineExceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("identifyControllerMNAN was still blocked after 3s")
	}
}
