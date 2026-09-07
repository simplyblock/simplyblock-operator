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

	xfs := FilesystemStrategyFor("xfs").FormatOptions(nil, FormatParameters{Geometry: striped})
	if !slices.Contains(xfs, "su=65536,sw=4") {
		t.Errorf("xfs was created without aligning to the stripes below it: %v", xfs)
	}

	// stride is one member's chunk counted in filesystem blocks and stripe_width
	// is one full trip across the members, so 65536/4096 = 16 and 16*4 = 64.
	ext := FilesystemStrategyFor("ext4").FormatOptions(nil, FormatParameters{Geometry: striped})
	if !slices.Contains(ext, "stride=16,stripe_width=64") {
		t.Errorf("ext4 was created without aligning to the stripes below it: %v", ext)
	}
}

// A chunk that is not a whole number of filesystem blocks describes a layout the
// device does not have, and rounding it would be worse than saying nothing.
func TestFilesystemStrategyDeclinesAChunkItCannotExpress(t *testing.T) {
	odd := volstack.Geometry{ChunkBytes: 100000, Stripes: 2}
	if got := FilesystemStrategyFor("ext4").FormatOptions(nil, FormatParameters{Geometry: odd}); len(got) != 0 {
		t.Errorf("ext4 rounded a chunk of %d bytes into %v", odd.ChunkBytes, got)
	}
}

// Each filesystem is grown by its own tool, pointed at whatever that tool
// resizes: ext resizes the device and XFS resizes the mount. Neither can be
// assembled from a name and a path by a caller that does not know which
// filesystem it is talking about.
func TestFilesystemStrategyGrowCommand(t *testing.T) {
	const dev, mnt = "/dev/nvme0n1", "/var/lib/kubelet/staging"

	if got := FilesystemStrategyFor("ext4").GrowCommand(dev, mnt); !slices.Equal(got, []string{"resize2fs", dev}) {
		t.Errorf("ext4 grows with %v, want resize2fs against the device", got)
	}
	if got := FilesystemStrategyFor("xfs").GrowCommand(dev, mnt); !slices.Equal(got, []string{"xfs_growfs", mnt}) {
		t.Errorf("xfs grows with %v, want xfs_growfs against the mount", got)
	}
	// Guessing at a tool name would run something arbitrary against a volume
	// holding data, so a filesystem nothing is known about reports that it cannot
	// be grown here.
	if got := FilesystemStrategyFor("btrfs").GrowCommand(dev, mnt); got != nil {
		t.Errorf("an unknown filesystem offered %v as a way to grow it", got)
	}
}

// A virtualized device reports no layout, and the hints computed for whatever is
// underneath it describe nothing once its blocks are relocated.
func TestFilesystemStrategyAlignsToNothingItCannotSee(t *testing.T) {
	for _, fsType := range []string{"xfs", "ext4", "btrfs"} {
		got := FilesystemStrategyFor(fsType).FormatOptions([]string{"-q"}, FormatParameters{})
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
	opts := FilesystemStrategyFor("xfs").FormatOptions([]string{"-K"},
		FormatParameters{Geometry: volstack.Geometry{ChunkBytes: 1 << 16, Stripes: 2}})
	if opts[0] != "-K" {
		t.Errorf("the volume's own format option is no longer first: %v", opts)
	}
	if n := strings.Count(strings.Join(opts, " "), "-K"); n != 1 {
		t.Errorf("the volume's own format option appears %d times: %v", n, opts)
	}
}

// TestExtReservesBlocksWhenTheVolumeAsks proves the ext family knows how the
// reservation is spelled, so that a consumer asks for the property and not for
// the flag. A volume that asks for none is left at the filesystem's own default,
// which is not the same as asking for zero.
func TestExtReservesBlocksWhenTheVolumeAsks(t *testing.T) {
	for _, fsType := range []string{"ext4", "ext3", "ext2"} {
		got := FilesystemStrategyFor(fsType).FormatOptions(nil, FormatParameters{
			ReservedBlocksPercent: "3",
		})
		if i := slices.Index(got, "-m"); i < 0 || i+1 >= len(got) || got[i+1] != "3" {
			t.Errorf("%s: got %v, want the reservation spelled as -m 3", fsType, got)
		}
	}
}

// TestExtReservesNothingWhenTheVolumeDoesNotAsk keeps mkfs at its own default
// rather than passing an empty value it would reject.
func TestExtReservesNothingWhenTheVolumeDoesNotAsk(t *testing.T) {
	got := FilesystemStrategyFor("ext4").FormatOptions([]string{"-q"}, FormatParameters{})
	if slices.Contains(got, "-m") {
		t.Errorf("got %v, want no reservation flag", got)
	}
	if !slices.Contains(got, "-q") {
		t.Errorf("got %v, want the volume's own options kept", got)
	}
}

// TestExtKeepsTheReservationBesideTheStripeAlignment proves the two
// contributions compose, since a striped ext volume asking for a reservation
// needs both and neither may replace the other.
func TestExtKeepsTheReservationBesideTheStripeAlignment(t *testing.T) {
	got := FilesystemStrategyFor("ext4").FormatOptions(nil, FormatParameters{
		Geometry:              volstack.Geometry{Stripes: 2, ChunkBytes: 64 << 10},
		ReservedBlocksPercent: "0",
	})
	if !slices.Contains(got, "-E") {
		t.Errorf("got %v, want the stripe alignment kept", got)
	}
	if i := slices.Index(got, "-m"); i < 0 || got[i+1] != "0" {
		t.Errorf("got %v, want the reservation kept", got)
	}
}

// TestOnlyExtSpellsAReservation is the negative half: -m is mke2fs's flag, and a
// filesystem that has no such notion must not be handed one.
func TestOnlyExtSpellsAReservation(t *testing.T) {
	for _, fsType := range []string{"xfs", "btrfs"} {
		got := FilesystemStrategyFor(fsType).FormatOptions(nil, FormatParameters{
			ReservedBlocksPercent: "3",
		})
		if slices.Contains(got, "-m") {
			t.Errorf("%s: got %v, want no reservation flag", fsType, got)
		}
	}
}

// TestNoFilesystemIsForcedPastItsOwnRefusal. mkfs has its own guard: it declines
// when it finds something where it is about to write, and both -F and -f exist
// to take that guard away. The layer formats only what it has positively read as
// blank, so the two checks agree in every ordinary case, and where they disagree
// the disagreement is the point. Overriding it turns a device this driver failed
// to understand into a destroyed volume.
func TestNoFilesystemIsForcedPastItsOwnRefusal(t *testing.T) {
	for _, fsType := range []string{"ext4", "ext3", "ext2", "xfs", "btrfs"} {
		got := FilesystemStrategyFor(fsType).FormatOptions(nil, FormatParameters{
			Geometry:              volstack.Geometry{Stripes: 2, ChunkBytes: 64 << 10},
			ReservedBlocksPercent: "0",
		})
		for _, force := range []string{"-F", "-f", "--force"} {
			if slices.Contains(got, force) {
				t.Errorf("%s is created with %s, which overrides mkfs's own refusal: %v",
					fsType, force, got)
			}
		}
	}
}
