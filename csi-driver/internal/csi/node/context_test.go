// blackbox test of util package
package node

import (
	"os"
	"testing"
)

func TestVolumeContext(t *testing.T) {
	volumeContextFileName := "volumeContext.json"

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
