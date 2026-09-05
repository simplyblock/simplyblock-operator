//go:build linux

// The local reader against a real block device.
//
// Nothing here runs unless a device is named, because the package's other tests
// deliberately need no device and this one needs a real one: the direct open,
// the block-size ioctls, and the read path are what a captured image cannot
// exercise, and they are also the part that only ever runs on a node.
//
//	SB_BLOCKDEV_DEVICE=/dev/loop0,/dev/loop1=Filesystem go test -run TestLocalDevice ./blockdev/

package blockdev

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestLocalDeviceReading(t *testing.T) {
	spec := os.Getenv("SB_BLOCKDEV_DEVICE")
	if spec == "" {
		t.Skip("set SB_BLOCKDEV_DEVICE to a comma-separated list of devices, optionally each =<expected Content>")
	}

	for _, entry := range strings.Split(spec, ",") {
		path, wantContent, _ := strings.Cut(strings.TrimSpace(entry), "=")
		if path == "" {
			continue
		}
		t.Run(path, func(t *testing.T) {
			dev, err := ResolveDevice(path)
			if err != nil {
				t.Fatalf("ResolveDevice(%s): %v", path, err)
			}
			t.Logf("resolved: size=%d logical=%d physical=%d major=%d minor=%d readonly=%v",
				dev.SizeBytes, dev.LogicalBlockSize, dev.PhysicalBlockSize, dev.Major, dev.Minor, dev.ReadOnly)

			if dev.SizeBytes == 0 {
				t.Fatal("resolved a size of zero")
			}
			if dev.LogicalBlockSize == 0 {
				t.Error("resolved no logical block size, so block-relative offsets fall back to 512")
			}

			got, err := NewProber().Read(context.Background(), dev)
			if err != nil {
				t.Fatalf("Read(%s): %v", path, err)
			}
			t.Logf("reading: %s type=%q detail=%s", got.Content, got.Type, got.Detail)

			if got.Content == ContentUnknown {
				t.Error("Read returned the zero Content without an error")
			}
			if wantContent != "" && got.Content.String() != wantContent {
				t.Errorf("Content = %s, want %s", got.Content, wantContent)
			}
		})
	}
}
