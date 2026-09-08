// blackbox test of util package
package node

import (
	"os"
	"testing"
)

func TestVolumeContext(t *testing.T) {

	dir, err := os.MkdirTemp("", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	volumeContext := map[string]string{
		"key1": "value1",
		"key2": "value2",
	}

	err = stashVolumeContext(volumeContext, dir)
	if err != nil {
		t.Fatalf("stashVolumeContext returned error: %v", err)
	}

	returnedContext, err := lookupVolumeContext(dir)
	if err != nil {
		t.Fatalf("lookupVolumeContext returned error: %v", err)
	}

	if volumeContext["key1"] != returnedContext["key1"] || volumeContext["key2"] != returnedContext["key2"] {
		t.Fatalf("lookupVolumeContext returned unexpected value: got %v, want %v", returnedContext, volumeContext)
	}

	err = cleanUpVolumeContext(dir)
	if err != nil {
		t.Fatalf("cleanUpVolumeContext returned error: %v", err)
	}

	_, err = os.Stat(dir + "/" + volumeContextFileName)
	if !os.IsNotExist(err) {
		t.Fatalf("cleanUpVolumeContext failed to cleanup volume context stash")
	}
}

// TestCleanUpVolumeContextIsIdempotent covers a second unstage of the same
// volume. The CSI spec lets kubelet retry NodeUnstageVolume, and a retry after
// a successful one finds the stash already gone; reporting that as an error
// fails the RPC forever and wedges the volume.
func TestCleanUpVolumeContextIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	if err := stashVolumeContext(map[string]string{"key": "value"}, dir); err != nil {
		t.Fatalf("stashVolumeContext: %v", err)
	}
	if err := cleanUpVolumeContext(dir); err != nil {
		t.Fatalf("first cleanUpVolumeContext: %v", err)
	}
	if err := cleanUpVolumeContext(dir); err != nil {
		t.Errorf("second cleanUpVolumeContext: %v, want nil — an unstage retry must not fail", err)
	}
}

// TestCleanUpVolumeContextOnNeverStagedPath covers the other order the same
// retry can arrive in: an unstage for a volume whose staging never wrote a
// stash at all.
func TestCleanUpVolumeContextOnNeverStagedPath(t *testing.T) {
	if err := cleanUpVolumeContext(t.TempDir()); err != nil {
		t.Errorf("cleanUpVolumeContext on a path with no stash: %v, want nil", err)
	}
}
