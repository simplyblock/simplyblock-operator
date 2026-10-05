// The nvme-cli queries the reconnect monitor runs every tick, against an
// nvme-cli that does not answer in time.
//
// A fake `nvme` first on PATH plays the stuck process: one that sleeps past its
// budget, and one that leaves a background process holding its output pipe, which
// no kill on the context's deadline can end.

package initiator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeNVMe puts an `nvme` executable running script first on PATH.
func fakeNVMe(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	body := []byte("#!/bin/sh\n" + script + "\n")
	if err := os.WriteFile(filepath.Join(dir, "nvme"), body, 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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

// The reconnect monitor is a single goroutine serving every volume on the node,
// and a device listing that never returned stopped it for good: on worker-3 it
// logged its last line at 08:56:59 and never reported a namespace the kernel
// removed at 08:58:45, so that volume was never marked broken.
//
// Regression: 2026-10-04-unbounded-nvme-io.
func TestNVMeDevicesReturnsWhenNVMeCLIDoesNot(t *testing.T) {
	fakeNVMe(t, `sleep 30 &
echo '{"Devices":[]}'`)

	err := returnsWithin(t, 4*time.Second, "NVMeDevices", func() error {
		_, err := NVMeDevices(context.Background())
		return err
	})
	if err == nil {
		t.Fatal("NVMeDevices returned no error for an nvme-cli that never released its output")
	}
}

// A device listing reads kernel state and returns in milliseconds, so one still
// running after a couple of seconds is stuck, and waiting the old ten seconds for
// it held up every other volume's reconnect on every tick.
//
// Regression: 2026-10-04-unbounded-nvme-io.
func TestSubsystemsForDeviceGivesUpOnASlowQuery(t *testing.T) {
	fakeNVMe(t, "exec sleep 30")

	err := returnsWithin(t, 4*time.Second, "SubsystemsForDevice", func() error {
		_, err := SubsystemsForDevice(context.Background(), "/dev/nvme0n1")
		return err
	})
	if err == nil {
		t.Fatal("SubsystemsForDevice returned no error for a query that never finished")
	}
}
