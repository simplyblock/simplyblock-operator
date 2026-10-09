//go:build linux

// The by-id link lookup the legacy-identity case mounts from, tested on a
// temporary directory: it needs no node, unlike the rest of this suite.

package onnode

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression: 2026-10-09-onnode-udev-temporary-link (#697, run 37897452947). udev
// creates a link under `.#name` and renames it into place, so the temporary
// name alone must not count as the device's link, and the stable one must.
func TestStableByIDLinkWaitsOutUdevsTemporaryName(t *testing.T) {
	dir := t.TempDir()
	device := filepath.Join(t.TempDir(), "nvme5n1")
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".#nvme-simplyblock_vol", "nvme-simplyblock_vol-part1"} {
		if err := os.Symlink(device, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	if link, ok := stableByIDLink(dir, device); ok {
		t.Fatalf("stableByIDLink = %q before udev renamed its temporary link into place", link)
	}

	stable := filepath.Join(dir, "nvme-simplyblock_vol")
	if err := os.Symlink(device, stable); err != nil {
		t.Fatal(err)
	}
	if link, ok := stableByIDLink(dir, device); !ok || link != stable {
		t.Errorf("stableByIDLink = %q, %v; want %q", link, ok, stable)
	}
}
