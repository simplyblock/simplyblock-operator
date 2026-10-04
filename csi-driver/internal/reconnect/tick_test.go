// One tick of the reconnect monitor when its path repair cannot run.
//
// A fake `nvme` first on PATH fails every query, which is what a wedged or
// missing nvme-cli does to the repair half of a tick. The device under test is
// recorded under a name no kernel gives a namespace, so the real sysfs scan never
// lists it on any host and it reads as removed.

package reconnect

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/csi-driver/internal/initiator"
	sbkube "github.com/simplyblock/csi-driver/internal/kubernetes"
)

// A tick whose path repair fails still has to report the volumes whose device
// the kernel removed. Returning early on the repair's error is what hid every
// removal while nvme-cli was failing, and the guardian never heard of them.
//
// Regression: 2026-10-04-unbounded-nvme-io — the Copilot review of #628 asked for
// the ordering to be pinned.
func TestTickReportsGoneDevicesWhenPathRepairFails(t *testing.T) {
	dir := t.TempDir()
	script := []byte("#!/bin/sh\nexit 1\n")
	if err := os.WriteFile(filepath.Join(dir, "nvme"), script, 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const (
		device = "/dev/nvme-gone-test0n1"
		lvolID = "edb5ab15-4417-46f0-b99a-3610034e6917"
	)
	initiator.MarkDevicePresent(device, lvolID)
	t.Cleanup(func() { initiator.ForgetDevice(device) })

	var broken []string
	err := reconnectSubsystems(
		func(id string) { broken = append(broken, id) },
		sbkube.NewManager(fake.NewSimpleClientset()),
		"csi.simplyblock.io", "worker-3",
	)
	if err == nil {
		t.Fatal("reconnectSubsystems returned no error although every nvme-cli query failed")
	}
	if !slices.Contains(broken, lvolID) {
		t.Fatalf("a tick whose path repair failed reported %v broken, want %s", broken, lvolID)
	}
}
