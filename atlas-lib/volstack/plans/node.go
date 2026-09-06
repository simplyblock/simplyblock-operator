// What a plan is built with: this node's identity, and an implementation of
// every seam the layers take.
//
// It is resolved once and shared by every plan a process builds, because these
// are properties of the host rather than of a volume: one connector, one view of
// sysfs, and one LVM. Keeping them here is also what keeps the shapes in
// plans.go free of construction detail, so that a row of the plan table reads as
// the list of layers it is.

package plans

import (
	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
)

// NodeConfig is what a node is built from. Every field is a seam the layers take
// as an interface, which is what lets a consumer's tests build real plans and
// run them against recorded doubles rather than a kernel.
type NodeConfig struct {
	// HostNQN and HostID are this node's identity, presented on every connect so
	// that an allowed-hosts pool authorizes the right host.
	HostNQN string
	HostID  string

	// Connector attaches and detaches the fabric, and Devices answers what is
	// attached.
	Connector nvmeof.Connector
	Devices   nvme.DeviceResolver

	// Manager runs the LVM commands the middle layers are made of.
	Manager *lvm.Manager

	// Content is the reading a formatting decision rests on. A device that could
	// not be read, or that carries something this driver did not put there, is
	// not an empty device, and this is what says so.
	Content layers.ContentReader

	// Filesystem formats and mounts. The CSI driver passes the Kubernetes mount
	// utilities, because atlas has no business depending on them.
	Filesystem layers.FilesystemOps

	// Resolve answers what the kernel says about a device path, and defaults to
	// blockdev.ResolveDevice. It is a seam only because the logical-volume layer
	// creates a device-mapper node and has to describe it upward, which a test
	// has no device-mapper for, so a caller running on a real node has nothing to
	// decide here.
	Resolve layers.DeviceResolver
}

// Node is everything the layers need from the host, resolved once so that every
// plan a process builds shares one connector and one view of sysfs.
type Node struct {
	cfg NodeConfig
}

// NewNode holds the given seams, filling in the one that has a single shipped
// implementation. It validates nothing: a missing seam is a programming error
// that surfaces at the first call through it, and refusing to build a plan here
// would only move that failure earlier without making it clearer.
func NewNode(cfg NodeConfig) *Node {
	if cfg.Resolve == nil {
		cfg.Resolve = blockdev.ResolveDevice
	}
	return &Node{cfg: cfg}
}

// fabric is the bottom layer of every plan: one namespace, attached.
func (n *Node) fabric(connection lvol.Connection) volstack.Layer {
	return layers.NewFabric(layers.FabricConfig{
		Connection: connection,
		Connector:  n.cfg.Connector,
		Devices:    n.cfg.Devices,
		HostNQN:    n.cfg.HostNQN,
		HostID:     n.cfg.HostID,
	})
}

// filesystem is the top layer of every plan that has one.
func (n *Node) filesystem(volume Volume) volstack.Layer {
	return layers.NewFilesystem(layers.FilesystemConfig{
		FsType:        volume.FsType,
		StagingPath:   volume.StagingPath,
		MountFlags:    volume.MountFlags,
		FormatOptions: volume.FormatOptions,
		Ops:           n.cfg.Filesystem,
		Content:       n.cfg.Content,
	})
}

// physicalVolume labels the device below as belonging to this volume's group.
// It is told the logical volume's name as well as the group's, because resolving
// a clone renames the group and leaves the volume inside it named after the
// source.
func (n *Node) physicalVolume(volume Volume) volstack.Layer {
	return layers.NewLVMPhysicalVolume(layers.LVMPhysicalVolumeConfig{
		VolumeGroup:   volume.VolumeGroup(),
		LogicalVolume: volume.LogicalVolume(),
		Manager:       n.cfg.Manager,
		Content:       n.cfg.Content,
	})
}

// volumeGroup is the layer between the physical volumes and the logical one.
func (n *Node) volumeGroup(volume Volume) volstack.Layer {
	return layers.NewLVMVolumeGroup(layers.LVMVolumeGroupConfig{
		VolumeGroup: volume.VolumeGroup(),
		Manager:     n.cfg.Manager,
	})
}

// logicalVolume is the one volume inside that group, whatever type the options
// say it is to be.
func (n *Node) logicalVolume(volume Volume, options LogicalVolumeOptions) volstack.Layer {
	return layers.NewLVMLogicalVolume(layers.LVMLogicalVolumeConfig{
		VolumeGroup:   volume.VolumeGroup(),
		LogicalVolume: volume.LogicalVolume(),
		PoolName:      options.PoolName,
		Definition:    options.Definition,
		Capability:    options.Capability,
		Manager:       n.cfg.Manager,
		Resolve:       n.cfg.Resolve,
	})
}
