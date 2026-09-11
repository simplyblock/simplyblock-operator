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

// FormatParameters is what a format is decided from besides the options the
// volume passed through verbatim.
//
// It is a struct rather than an argument list because what a filesystem can be
// asked for grows: a reservation is spelled one way by the ext family and not at
// all by XFS, and the next such property should reach the strategies without
// every one of them changing shape.
type FormatParameters struct {
	// Geometry is the stripe layout underneath, and the zero value when there is
	// none, which is what a virtualized device reports.
	Geometry volstack.Geometry

	// ReservedBlocksPercent is how much of the filesystem is held back for
	// privileged processes, as the volume asked for it. Empty leaves the
	// filesystem at its own default, which is not what asking for zero means.
	ReservedBlocksPercent string
}

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
	// needs, or was asked, to be created with on the device below.
	FormatOptions(options []string, params FormatParameters) []string

	// MountFlags are the flags the volume asked for plus any this filesystem
	// requires in order to mount at all.
	MountFlags(flags []string) []string

	// GrowCommand extends the filesystem onto the device it now sits on, or nil
	// when this filesystem cannot be grown in place.
	//
	// Both the tool and what it is pointed at differ: ext resizes the device and
	// XFS resizes the mount, so a caller cannot compose the command from a name
	// and a path without knowing which filesystem it is talking about. That is
	// why it is asked for rather than assembled.
	//
	// Growing only. XFS cannot shrink at all, ext shrinks only while unmounted,
	// and a volume is never shrunk beneath a running pod.
	GrowCommand(device, mountpoint string) []string
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

func (e extStrategy) Name() string {
	return e.fsType
}

// extBlockBytes is the block size mkfs picks for a volume of any size worth
// striping, and the unit stride and stripe_width are counted in. A filesystem
// made with a different one would want different numbers, and nothing here can
// ask what the size will be before the filesystem exists.
const extBlockBytes = 4096

// FormatOptions align the filesystem to the stripes underneath it. stride is one
// member's chunk counted in filesystem blocks, and stripe_width is one full trip
// across the members, which is what lets the allocator spread writes instead of
// landing them all on the same one.
//
// Only when the chunk is a whole number of blocks. It is in practice, and
// rounding it would describe a layout the device does not have, which is worse
// than describing none.
func (e extStrategy) FormatOptions(options []string, params FormatParameters) []string {
	options = reserveBlocks(options, params.ReservedBlocksPercent)

	geometry := params.Geometry
	if !geometry.Known() || geometry.ChunkBytes%extBlockBytes != 0 {
		return options
	}
	stride := geometry.ChunkBytes / extBlockBytes
	return append(options, "-E", fmt.Sprintf("stride=%d,stripe_width=%d",
		stride, stride*int64(geometry.Stripes)))
}

// reserveBlocks holds part of the filesystem back for privileged processes,
// which the ext family spells as mke2fs's -m and no other filesystem spells at
// all. It is set when the filesystem is created rather than tuned afterward,
// because the two produce the same filesystem and only one of them is a second
// command that can fail on its own after the volume is already formatted.
//
// A volume that asked for nothing is left at mke2fs's default, since asking for
// no reservation is a different thing from not asking.
func reserveBlocks(options []string, percent string) []string {
	if percent == "" {
		return options
	}
	return append(options, "-m", percent)
}

// GrowCommand resizes the device, which resize2fs does whether the filesystem is
// mounted or not.
func (e extStrategy) GrowCommand(device, _ string) []string {
	return []string{"resize2fs", device}
}

// MountFlags adds nothing: ext mounts a volume and its clone side by side
// without complaint, since it does not refuse a filesystem whose UUID it has
// already seen.
func (e extStrategy) MountFlags(flags []string) []string {
	return flags
}

// xfsStrategy is XFS.
type xfsStrategy struct{}

func (xfsStrategy) Name() string {
	return "xfs"
}

// FormatOptions align the filesystem to the stripes underneath it, so a write
// that fills one chunk lands on one member rather than across two.
//
// Only when there is a layout to align to. A virtualized device reports none,
// and the hints computed for the backend underneath it describe nothing once its
// blocks are relocated, so passing them there would be misleading rather than
// merely useless.
func (xfsStrategy) FormatOptions(options []string, params FormatParameters) []string {
	geometry := params.Geometry
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

// GrowCommand resizes the mount rather than the device, because XFS grows only
// while mounted and is told which filesystem by the path it is mounted at.
func (xfsStrategy) GrowCommand(_, mountpoint string) []string {
	return []string{"xfs_growfs", mountpoint}
}

// plainStrategy is a filesystem this package knows no specifics about, which
// contributes nothing to either question rather than being turned away.
type plainStrategy struct{ fsType string }

func (p plainStrategy) Name() string {
	return p.fsType
}

func (p plainStrategy) FormatOptions(options []string, _ FormatParameters) []string {
	return options
}

func (p plainStrategy) MountFlags(flags []string) []string {
	return flags
}

// GrowCommand is nil. Nothing here knows how to grow a filesystem it knows
// nothing else about, and guessing at a tool name would run something arbitrary
// against a volume holding data.
func (p plainStrategy) GrowCommand(_, _ string) []string {
	return nil
}
