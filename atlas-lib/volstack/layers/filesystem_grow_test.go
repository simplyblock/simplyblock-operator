// What the filesystem layer owes an expand.
//
// The layer below has already taken the space by the time the runner reaches
// this one, so the only question here is whether the filesystem is told to use
// it. A volume that grew and a filesystem that did not is a pod still writing
// into the size it had yesterday.

package layers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/volstack"
)

// growable is a layer over a device already mounted, which is the state an
// expand arrives in.
func growable(t *testing.T, fs *fakeFS, fsType string) *Filesystem {
	t.Helper()
	fs.mountPoints[stagingPath] = true
	return newFSAsking(t, fs, fsType,
		blockdev.Reading{Content: blockdev.ContentFilesystem, Type: fsType}, nil)
}

// The layer implements the contract's Grower, which is what makes the runner
// walk it at all: a plan whose layers implement none is an expand that silently
// does nothing.
func TestFilesystemIsAGrower(t *testing.T) {
	var l any = NewFilesystem(FilesystemConfig{})
	if _, ok := l.(volstack.Grower); !ok {
		t.Fatal("the filesystem layer does not implement Grower, so an expand walks past it")
	}
}

// Each filesystem is grown by its own tool, pointed at what that tool resizes.
func TestFilesystemGrowRunsTheFilesystemsOwnTool(t *testing.T) {
	for _, tc := range []struct {
		fsType string
		want   []string
	}{
		{"ext4", []string{"resize2fs", "/dev/nvme0n1"}},
		{"xfs", []string{"xfs_growfs", stagingPath}},
	} {
		t.Run(tc.fsType, func(t *testing.T) {
			fs := newFakeFS()
			l := growable(t, fs, tc.fsType)

			art, err := l.Grow(context.Background(), belowArtifact())
			if err != nil {
				t.Fatalf("Grow: %v", err)
			}
			if art.Path != stagingPath {
				t.Errorf("a grown filesystem exposes %q, want the path it is mounted at", art.Path)
			}
			if len(fs.grown) != 1 || !slices.Equal(fs.grown[0], tc.want) {
				t.Fatalf("grew with %v, want %v", fs.grown, tc.want)
			}
		})
	}
}

// kubelet reissues NodeExpandVolume after one that already succeeded, so a
// filesystem already at its target is the ordinary case rather than an error.
// Both tools take the whole of what is now underneath them and succeed when that
// is where they already are.
func TestFilesystemGrowIsConvergent(t *testing.T) {
	fs := newFakeFS()
	l := growable(t, fs, "ext4")

	for i := range 2 {
		if _, err := l.Grow(context.Background(), belowArtifact()); err != nil {
			t.Fatalf("Grow %d: %v", i+1, err)
		}
	}
	if len(fs.grown) != 2 {
		t.Errorf("ran the resize %d times across two expands", len(fs.grown))
	}
}

// An expand arrives against a staged volume. A filesystem this host has not
// mounted is not this host's to resize, and XFS cannot be grown except through
// its mount at all.
func TestFilesystemGrowRefusesWhatIsNotMounted(t *testing.T) {
	fs := newFakeFS()
	l := newFSAsking(t, fs, "xfs",
		blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "xfs"}, nil)

	if _, err := l.Grow(context.Background(), belowArtifact()); err == nil {
		t.Fatal("Grow resized a filesystem that is not mounted")
	}
	if len(fs.grown) != 0 {
		t.Errorf("it ran %v anyway", fs.grown)
	}
}

// A filesystem nothing is known about is not grown by guessing at a tool name,
// which would run something arbitrary against a volume holding data.
func TestFilesystemGrowRefusesAFilesystemItCannotResize(t *testing.T) {
	fs := newFakeFS()
	l := growable(t, fs, "btrfs")

	_, err := l.Grow(context.Background(), belowArtifact())
	if err == nil {
		t.Fatal("Grow resized a filesystem it knows no resize tool for")
	}
	if !strings.Contains(err.Error(), "btrfs") {
		t.Errorf("the refusal does not name the filesystem: %v", err)
	}
	if len(fs.grown) != 0 {
		t.Errorf("it ran %v anyway", fs.grown)
	}
}

// A resize that failed is reported rather than swallowed: the volume grew and
// the filesystem did not, and a caller told otherwise would leave a pod writing
// into the size it had before.
func TestFilesystemGrowReportsAFailedResize(t *testing.T) {
	fs := newFakeFS()
	fs.growErr = errors.New("resize2fs: device or resource busy")
	l := growable(t, fs, "ext4")

	if _, err := l.Grow(context.Background(), belowArtifact()); err == nil {
		t.Fatal("Grow reported success for a resize that failed")
	}
}
