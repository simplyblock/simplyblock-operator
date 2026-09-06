// What each filesystem contributes, asked of it directly.
//
// The layer's own cases go through Ensure and Heal and prove the contributions
// reach mkfs and mount. These prove what the contributions are, which is the
// half that used to be an `if` naming one filesystem and saying nothing about
// the others.

package layers

import (
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/volstack"
)

func TestFilesystemStrategyForKnowsItsFilesystem(t *testing.T) {
	for _, fsType := range []string{"ext4", "ext3", "ext2", "xfs", "btrfs", ""} {
		if got := FilesystemStrategyFor(fsType).Name(); got != fsType {
			t.Errorf("the strategy for %q calls itself %q", fsType, got)
		}
	}
}

// A layout to align to reaches the filesystems that can use one, and nothing
// invents one for a filesystem that cannot.
func TestFilesystemStrategyStripeAlignment(t *testing.T) {
	striped := volstack.Geometry{ChunkBytes: 64 << 10, Stripes: 4}

	xfs := FilesystemStrategyFor("xfs").FormatOptions(nil, striped)
	if !slices.Contains(xfs, "su=65536,sw=4") {
		t.Errorf("xfs was created without aligning to the stripes below it: %v", xfs)
	}

	// Not a defect, and not silence either: the ext family has stride and
	// stripe_width and does not pass them yet, which leaves it misaligned rather
	// than wrong. The case is here so that changes when somebody adds them.
	if got := FilesystemStrategyFor("ext4").FormatOptions(nil, striped); len(got) != 0 {
		t.Errorf("ext4 now contributes format options; this case is what said it did not: %v", got)
	}
}

// A virtualized device reports no layout, and the hints computed for whatever is
// underneath it describe nothing once its blocks are relocated.
func TestFilesystemStrategyAlignsToNothingItCannotSee(t *testing.T) {
	for _, fsType := range []string{"xfs", "ext4", "btrfs"} {
		got := FilesystemStrategyFor(fsType).FormatOptions([]string{"-q"}, volstack.Geometry{})
		if !slices.Equal(got, []string{"-q"}) {
			t.Errorf("%s added %v to a device that reports no geometry", fsType, got)
		}
	}
}

// XFS refuses to mount two filesystems carrying the same UUID, and a volume and
// its clone do, so without this only one of them can be mounted on a node.
func TestFilesystemStrategyMountFlags(t *testing.T) {
	xfs := FilesystemStrategyFor("xfs").MountFlags([]string{"ro"})
	if !slices.Contains(xfs, "nouuid") {
		t.Errorf("xfs mounts without nouuid, so a clone cannot be staged beside its source: %v", xfs)
	}
	if !slices.Contains(xfs, "ro") {
		t.Errorf("xfs dropped the flag the volume asked for: %v", xfs)
	}

	for _, fsType := range []string{"ext4", "btrfs"} {
		got := FilesystemStrategyFor(fsType).MountFlags([]string{"ro"})
		if !slices.Equal(got, []string{"ro"}) {
			t.Errorf("%s added %v to the flags the volume asked for", fsType, got)
		}
	}
}

// The volume's own options and flags survive, whatever the filesystem adds.
func TestFilesystemStrategyKeepsWhatTheVolumeAsked(t *testing.T) {
	opts := FilesystemStrategyFor("xfs").FormatOptions(
		[]string{"-K"}, volstack.Geometry{ChunkBytes: 1 << 16, Stripes: 2})
	if opts[0] != "-K" {
		t.Errorf("the volume's own format option is no longer first: %v", opts)
	}
	if n := strings.Count(strings.Join(opts, " "), "-K"); n != 1 {
		t.Errorf("the volume's own format option appears %d times: %v", n, opts)
	}
}
