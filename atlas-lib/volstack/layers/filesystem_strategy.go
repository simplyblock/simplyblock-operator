// What one filesystem needs that the others do not.
//
// The filesystem layer is one layer whatever is on the device: it decides when a
// format is permitted, refuses a volume the plan did not ask for, mounts, clears
// a mount that will not come down, and heals. None of that differs between ext4
// and XFS. What differs is small, specific, and easy to leave out by accident,
// which is the reason it is gathered here rather than left as conditionals in
// the layer: a filesystem that contributes nothing to a question says so by
// returning nothing, where an `if` naming the other filesystem says it by
// omission and reads the same as never having considered it.

package layers

import (
	"fmt"

	"github.com/simplyblock/atlas/volstack"
)

// FilesystemLayerStrategy is the per-filesystem half of the filesystem layer.
//
// Chosen from the filesystem the plan asks for, which is also the only one the
// layer will act on: a device carrying another is refused rather than
// reconciled, so a strategy never has to consider a filesystem that is not its
// own.
type FilesystemLayerStrategy interface {
	// Name is the filesystem this strategy is for, spelled as mkfs and mount
	// spell it.
	Name() string

	// FormatOptions are the volume's own options plus whatever this filesystem
	// needs in order to be created well on the device below. geometry describes
	// the stripe layout underneath and is the zero value when there is none, which
	// is what a virtualized device reports.
	FormatOptions(options []string, geometry volstack.Geometry) []string

	// MountFlags are the flags the volume asked for plus any this filesystem
	// requires in order to mount at all.
	MountFlags(flags []string) []string
}

// FilesystemStrategyFor is the strategy for a filesystem, and a strategy that
// adds nothing for one this package knows no specifics about.
//
// An unknown filesystem is not refused here. Whether a volume may ask for one is
// the plan's question, and mkfs and mount answer it soon enough; what this
// decides is only whether anything has to be added on its behalf.
func FilesystemStrategyFor(fsType string) FilesystemLayerStrategy {
	switch fsType {
	case "ext4", "ext3", "ext2":
		return extStrategy{fsType: fsType}
	case "xfs":
		return xfsStrategy{}
	default:
		return plainStrategy{fsType: fsType}
	}
}

// extStrategy is the ext family.
type extStrategy struct{ fsType string }

func (e extStrategy) Name() string { return e.fsType }

// FormatOptions adds nothing yet. An ext filesystem on a striped volume wants
// stride and stripe_width, and not passing them leaves it misaligned rather than
// wrong, so it is a gap rather than a defect.
func (e extStrategy) FormatOptions(options []string, _ volstack.Geometry) []string {
	return options
}

// MountFlags adds nothing: ext mounts a volume and its clone side by side
// without complaint, since it does not refuse a filesystem whose UUID it has
// already seen.
func (e extStrategy) MountFlags(flags []string) []string { return flags }

// xfsStrategy is XFS.
type xfsStrategy struct{}

func (xfsStrategy) Name() string { return "xfs" }

// FormatOptions align the filesystem to the stripes underneath it, so a write
// that fills one chunk lands on one member rather than across two.
//
// Only when there is a layout to align to. A virtualized device reports none,
// and the hints computed for the backend underneath it describe nothing once its
// blocks are relocated, so passing them there would be misleading rather than
// merely useless.
func (xfsStrategy) FormatOptions(options []string, geometry volstack.Geometry) []string {
	if !geometry.Known() {
		return options
	}
	return append(options,
		"-d", fmt.Sprintf("su=%d,sw=%d", geometry.ChunkBytes, geometry.Stripes),
		"-l", fmt.Sprintf("su=%d", geometry.ChunkBytes))
}

// MountFlags add nouuid, because XFS refuses to mount two filesystems carrying
// the same UUID and a volume and its clone or restored snapshot do. Without it
// only one of the two can be mounted on a node.
func (xfsStrategy) MountFlags(flags []string) []string {
	return append(flags, "nouuid")
}

// plainStrategy is a filesystem this package knows no specifics about, which
// contributes nothing to either question rather than being turned away.
type plainStrategy struct{ fsType string }

func (p plainStrategy) Name() string { return p.fsType }

func (p plainStrategy) FormatOptions(options []string, _ volstack.Geometry) []string { return options }

func (p plainStrategy) MountFlags(flags []string) []string { return flags }
