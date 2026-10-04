// Naming the namespace behind a path.

package nvme

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A namespace is found by the number the kernel knows it as, which is what a
// path resolves to and what no alias changes. The same device is
// /dev/nvme0n1, /dev/disk/by-id/nvme-uuid.…, and whatever else udev linked to
// it, and all three stat to one number.
func TestByDeviceNumberNamesTheNamespace(t *testing.T) {
	r := NewSysfsDeviceResolver(SysfsConfig{SysRoot: vm17Fixture(t), DevRoot: "/dev"})

	device, err := r.ByDeviceNumber(context.Background(), "259:1")
	if err != nil {
		t.Fatalf("ByDeviceNumber: %v", err)
	}
	if device.Namespace.ID != 1 {
		t.Errorf("namespace = %d, want 1", device.Namespace.ID)
	}
	if !strings.Contains(device.Subsystem.NQN, "792e184c") {
		t.Errorf("subsystem = %q, want the one the namespace belongs to", device.Subsystem.NQN)
	}
}

// A number no namespace carries is not found, rather than answered with
// whichever namespace was scanned first.
func TestByDeviceNumberIsNotFoundForAnUnknownNumber(t *testing.T) {
	r := NewSysfsDeviceResolver(SysfsConfig{SysRoot: vm17Fixture(t), DevRoot: "/dev"})

	if _, err := r.ByDeviceNumber(context.Background(), "7:3"); err == nil {
		t.Fatal("a device number nothing carries was resolved to a namespace")
	}
}

// The number of the filesystem a path is on, which for a staging path is the
// device mounted there.
func TestDeviceNumberAtReadsThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}

	number, err := DeviceNumberAt(path)
	if err != nil {
		t.Fatalf("DeviceNumberAt: %v", err)
	}
	if !regexp.MustCompile(`^\d+:\d+$`).MatchString(number) {
		t.Errorf("device number = %q, want major:minor", number)
	}
}

// A path that is not there is an error rather than a zero number, because a
// zero would be a device number like any other to the lookup above.
func TestDeviceNumberAtFailsForAMissingPath(t *testing.T) {
	if _, err := DeviceNumberAt(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a path that does not exist reported a device number")
	}
}
