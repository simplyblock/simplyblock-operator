// A stage reads the device once.
//
// On a degraded device a probe is the expensive operation in the whole staging
// path, and the reading is what the format decision rests on, so reading twice
// is both slower and two chances to get a different answer.

package layers

import (
	"context"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
)

type countingReader struct {
	reading blockdev.Reading
	reads   int
}

func (c *countingReader) Read(context.Context, blockdev.Device) (blockdev.Reading, error) {
	c.reads++
	return c.reading, nil
}

func TestEnsureReadsTheDeviceOnce(t *testing.T) {
	cases := []struct {
		name    string
		reading blockdev.Reading
	}{
		{"a blank device, which is formatted", blockdev.Reading{Content: blockdev.ContentBlank}},
		{"an existing filesystem, which is mounted",
			blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &countingReader{reading: tc.reading}
			l := NewFilesystem(FilesystemConfig{
				FsType: "ext4", StagingPath: stagingPath, Ops: newFakeFS(), Content: reader,
			})

			if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if reader.reads != 1 {
				t.Errorf("Ensure read the device %d times, want 1", reader.reads)
			}
		})
	}
}
