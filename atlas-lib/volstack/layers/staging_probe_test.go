// A stage reads the device once, all the way through the runner.
//
// The filesystem layer observes the device to decide what may be done to it, and
// that observation is the expensive step in the whole staging path on exactly the
// degraded devices this stack has to be careful with. Reading twice is also two
// chances to get a different answer for one decision.

package layers

import (
	"context"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/volstack"
)

// constDevice is a bottom layer that exposes one device and reads nothing, so
// the only probes a test counts are the filesystem layer's.
type constDevice struct{}

func (constDevice) Name() string { return "below" }

func (constDevice) Observe(context.Context, volstack.Artifact) (volstack.State, volstack.Artifact, error) {
	return volstack.StateReady, belowArtifact(), nil
}

func (constDevice) Ensure(context.Context, volstack.Artifact) (volstack.Artifact, error) {
	return belowArtifact(), nil
}

func (constDevice) Release(context.Context, volstack.Artifact) error { return nil }
func (constDevice) Destroy(context.Context, volstack.Artifact) error { return nil }

func TestBringingAStackUpReadsTheDeviceOnce(t *testing.T) {
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
			fs := NewFilesystem(FilesystemConfig{
				FsType: "ext4", StagingPath: stagingPath, Ops: newFakeFS(), Content: reader,
			})

			runner := volstack.NewRunner(volstack.NewStore(t.TempDir()))
			if _, err := runner.Up(context.Background(), "cluster:pool:volume",
				volstack.Plan{constDevice{}, fs}); err != nil {
				t.Fatalf("Up: %v", err)
			}
			if reader.reads != 1 {
				t.Errorf("bringing the stack up read the device %d times, want 1", reader.reads)
			}
		})
	}
}
