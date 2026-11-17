// Exercises the detectable half of mountcheck: a bucket directory that
// shares its root's device, the drift this package exists to catch. The
// healthy case -- a bucket directory that is a distinct mount -- needs an
// actual mount(2) (CAP_SYS_ADMIN), which a unit test does not have; that
// side is covered by the gateway's e2e suite against a real cluster
// instead.

package mountcheck

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnmountedReportsABucketDirStillOnTheRootsOwnDevice(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bucket-a"), 0o755); err != nil {
		t.Fatalf("creating bucket-a: %v", err)
	}

	got, err := Unmounted(root)
	if err != nil {
		t.Fatalf("Unmounted: %v", err)
	}
	if len(got) != 1 || got[0] != "bucket-a" {
		t.Errorf("Unmounted = %v, want [bucket-a]: it never left the root's own device", got)
	}
}

func TestUnmountedIgnoresNonDirectoryEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "not-a-bucket"), []byte("x"), 0o644); err != nil {
		t.Fatalf("creating a plain file: %v", err)
	}

	got, err := Unmounted(root)
	if err != nil {
		t.Fatalf("Unmounted: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Unmounted = %v, want none: the only entry is not a directory", got)
	}
}

func TestUnmountedOfAnEmptyRootIsEmpty(t *testing.T) {
	got, err := Unmounted(t.TempDir())
	if err != nil {
		t.Fatalf("Unmounted: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Unmounted = %v, want none", got)
	}
}

func TestUnmountedErrorsWhenTheRootDoesNotExist(t *testing.T) {
	if _, err := Unmounted(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("Unmounted = nil error, want one: the root does not exist")
	}
}

// Regression: 2026-11-13-mountcheck-vgwlocks-false-positive -- versitygw's
// posix backend creates `.vgwlocks` directly under the gateway root at
// startup (backend/posix/objlock.go), before any bucket is ever delivered.
// Unmounted treated it like any other subdirectory sharing the root's
// device, so a gateway with zero buckets failed its own readiness probe
// forever: confirmed live on cluster91, ObjectStore/simplyblock-s3 stuck
// Degraded with the gateway pod's /readyz returning 503 indefinitely. No
// valid S3 bucket name can start with a dot (buckets.go's CreateBucket
// enforces DNS-1123), so a hidden entry is never a bucket to report.
func TestUnmountedIgnoresHiddenBackendDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".vgwlocks"), 0o755); err != nil {
		t.Fatalf("creating .vgwlocks: %v", err)
	}

	got, err := Unmounted(root)
	if err != nil {
		t.Fatalf("Unmounted: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Unmounted = %v, want none: .vgwlocks is versitygw's own bookkeeping, not a bucket", got)
	}
}
