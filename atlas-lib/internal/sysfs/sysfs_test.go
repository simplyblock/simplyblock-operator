// The attribute readers against an attribute the kernel never answers.
//
// A FIFO with no writer stands in for the stuck attribute: opening it blocks in
// the kernel exactly as a read of a controller attribute does while that
// controller is wedged in error recovery, and no context reaches either.

package sysfs

import (
	"context"
	"errors"
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

// A read that never returns used to stall its caller for as long as the kernel
// held it, and the caller was the CSI driver's single reconnect loop, so one
// wedged controller stopped every volume on the node from being reported broken.
//
// Regression: 2026-10-04-unbounded-nvme-io — on lblk_outage_matrix_k8s-20261003-080237
// the reconnect monitor on worker-3 logged its last line at 08:56:59 and never
// reported the removal of nvme2n2 at 08:58:45.
func TestReadAttrGivesUpOnAnAttributeThatNeverAnswers(t *testing.T) {
	p := hangingPath(t)

	err := returnsWithin(t, 3*time.Second, "ReadAttr", func() error {
		_, err := ReadAttr(p)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadAttr on a stuck attribute = %v, want an error wrapping context.DeadlineExceeded", err)
	}
}
