// Package plans is the catalog of volume-stack shapes: one constructor per row
// of the volume-stack design's plan table, each returning the layer list that
// kind of volume is brought up from.
//
// It is a package of its own because a plan is assembled out of the layer
// implementations while Plan itself is a volstack type, and layers already
// imports volstack, so the compositions have to sit above both. What it buys is
// that the shape of a stack is decided once: a caller names a kind and describes
// the volume, and which layers that means, in which order, is not a decision the
// call site repeats. The alternative is every consumer composing its own list,
// which is how the node service and its tests come to disagree about what a
// staged volume looks like.
//
// Nothing here reaches the host. Building a plan resolves no device, runs no
// command, and reads no sysfs, which is what lets a consumer unit-test the
// selection it makes without a kernel under it. Selection is the consumer's:
// deciding which kind a volume is belongs to whatever holds its provisioning
// parameters, and this package only knows what each kind is made of.
//
// This file holds the shapes themselves and the naming rule they derive from a
// volume. node.go holds what they are built with.
package plans

import (
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
)

// The prefixes the LVM names carry. They exist to keep the group and the volume
// inside it apart, since LVM would otherwise be asked for <vg>/<vg>, and to mark
// both as this driver's when a human reads `lvs` on a node.
const (
	volumeGroupPrefix   = "vol-"
	logicalVolumePrefix = "lv-"
)

// Volume is the volume as one node presents it: the identity its LVM names
// derive from, and what the filesystem layer needs in order to serve it.
type Volume struct {
	// UUID identifies the volume. It is the volume's own and never the node's,
	// because the names below are derived from it: a plan replayed on another
	// host, or by a teardown that has only the record, has to arrive at the same
	// ones.
	UUID string

	// StagingPath is where the filesystem is mounted.
	StagingPath string

	// FsType is the filesystem this volume is. It decides what a blank device is
	// formatted as, and it is also the only filesystem that will be mounted: a
	// device carrying another is refused.
	FsType string

	// MountFlags are the flags the volume asked for, ahead of the ones the
	// filesystem layer derives from the filesystem itself.
	MountFlags []string

	// FormatOptions are passed to mkfs ahead of anything derived from the
	// geometry underneath.
	FormatOptions []string

	// ReservedBlocksPercent is how much of the filesystem is held back for
	// privileged processes, for a filesystem that has such a notion. Empty leaves
	// it at that filesystem's own default, which is not what asking for zero
	// means. How it is spelled is the filesystem's business, not the caller's.
	ReservedBlocksPercent string
}

// VolumeGroup is the name of the group this volume's LVM layers use.
func (v Volume) VolumeGroup() string { return VolumeGroupName(v.UUID) }

// LogicalVolume is the name of the one logical volume inside that group.
func (v Volume) LogicalVolume() string { return LogicalVolumeName(v.UUID) }

// VolumeGroupName is the group-naming rule, exported for a caller that has a
// volume's identity and no plan: a teardown working from a stack record, or a
// sweep looking for what this driver left on a node, has to name the group the
// way the plan that created it did, character for character.
func VolumeGroupName(uuid string) string { return volumeGroupPrefix + uuid }

// LogicalVolumeName is the same rule for the volume inside the group.
func LogicalVolumeName(uuid string) string { return logicalVolumePrefix + uuid }

// LogicalVolumeOptions is what the LVM rows differ in. The linear, VDO, and
// striped plans use one lvmLogicalVolume layer with different contents here,
// which is why striping and deduplication are parameters and not each a plan of
// their own.
type LogicalVolumeOptions struct {
	// Definition is what the logical volume is to be. It decides the arguments
	// lvcreate is given, and with them the geometry the layer reports upward.
	Definition lvm.LogicalVolumeDefinition

	// PoolName is lvcreate's <vg>/<pool> target, for a type that creates its
	// volume inside a pool. It is empty for a linear or striped volume, which has
	// no pool.
	PoolName string

	// Capability is what a node must carry for this volume's type to run there,
	// and is empty when any node will do. A VDO volume is the case that has one,
	// since a node without the kernel module cannot stage it at all.
	Capability volstack.Capability
}

// RawBlock is `fabric`, and nothing above it. Raw block mode is the plain plan
// with its top layer absent rather than a flag inside a stage function, which is
// what keeps a block volume from sharing a code path with a formatting one.
func (n *Node) RawBlock(connection lvol.Connection) volstack.Plan {
	return volstack.Plan{n.fabric(connection)}
}

// Plain is `fabric` → `filesystem`, the plan a single-namespace volume with a
// filesystem is staged from.
func (n *Node) Plain(connection lvol.Connection, volume Volume) volstack.Plan {
	return volstack.Plan{n.fabric(connection), n.filesystem(volume)}
}

// LVM is `fabric` → `lvmPhysicalVolume` → `lvmVolumeGroup` → `lvmLogicalVolume`
// → `filesystem`, the shape a volume with client-side deduplication or
// compression takes. What the logical volume is to be lives in options, so this
// row and the striped one differ in that and in their bottom layer alone.
func (n *Node) LVM(
	connection lvol.Connection, volume Volume, options LogicalVolumeOptions,
) volstack.Plan {
	return append(volstack.Plan{n.fabric(connection)}, n.lvmStack(volume, options)...)
}

// Striped is `members(n)` → `lvmPhysicalVolume` → `lvmVolumeGroup` →
// `lvmLogicalVolume` → `filesystem`: several namespaces where every other plan
// has one. It is the only plan whose bottom is not a single layer, which is the
// whole reason the composite exists, and the order of the connections is
// contract rather than convenience, since a stripe assembled over the same
// members in another order is a different device.
func (n *Node) Striped(
	connections []lvol.Connection, volume Volume, options LogicalVolumeOptions,
) volstack.Plan {
	members := make(volstack.Plan, 0, len(connections))
	for _, connection := range connections {
		members = append(members, n.fabric(connection))
	}
	return append(volstack.Plan{layers.NewMembers(members)}, n.lvmStack(volume, options)...)
}

// lvmStack is the four layers the LVM rows share above their bottom one. The
// group is a layer between the physical volumes and the logical one, rather than
// part of either, because a striped volume gains capacity by taking on members
// rather than by growing the ones it has.
func (n *Node) lvmStack(volume Volume, options LogicalVolumeOptions) volstack.Plan {
	return volstack.Plan{
		n.physicalVolume(volume),
		n.volumeGroup(volume),
		n.logicalVolume(volume, options),
		n.filesystem(volume),
	}
}
