//go:build linux

// The plans this suite brings up, built from the implementations that ship.
//
// Every layer takes its side effects as an interface, which is what lets the
// unit tests run without a kernel. The value of running here is the opposite
// one: nothing is substituted. The prober opens the device with O_DIRECT and
// asks the kernel for its block size, the connector is nvme-cli, the resolver
// walks the node's own sysfs, and LVM is LVM. A test that swapped any of those
// for a stand-in would be exercising the stand-in, since those are the parts
// that only a real kernel can be wrong about.

package onnode

import (
	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
)

// Target is the namespace the host-side driver published for this run, handed
// over rather than discovered: which nvmet subsystem exists is the driver's
// doing, and a suite that went looking for one could find a neighbor's.
type Target struct {
	NQN     string
	Address string
	Port    int
	NSID    uint32
}

// Volume is the identity a plan derives its names from. It stands in for the
// volume handle the CSI driver would carry.
type Volume struct {
	UUID        string
	StagingPath string
	FsType      string
}

// VolumeGroup is the group name a volume's LVM layers use. Derived from the
// volume's own UUID and nothing host-specific, so a plan replayed on another
// host arrives at the same name.
func (v Volume) VolumeGroup() string { return "vol-" + v.UUID }

// LogicalVolume is the name of the one logical volume inside that group.
func (v Volume) LogicalVolume() string { return "lv-" + v.UUID }

// node is everything the layers need from the host, resolved once so that every
// plan in a run shares one connector and one view of sysfs.
type node struct {
	hostNQN   string
	hostID    string
	subsys    nvme.SubsystemResolver
	devices   nvme.DeviceResolver
	connector nvmeof.Connector
	manager   *lvm.Manager
	content   *blockdev.Prober
	ops       layers.FilesystemOps
}

// newNode wires the shipped implementations together.
func newNode(hostNQN, hostID string) *node {
	cfg := nvme.SysfsConfig{}
	subs := nvme.NewSysfsSubsystemResolver(cfg)
	return &node{
		hostNQN:   hostNQN,
		hostID:    hostID,
		subsys:    subs,
		devices:   nvme.NewSysfsDeviceResolver(cfg),
		connector: nvmeof.NewCLIConnector(subs),
		manager:   lvm.NewManager(),
		content:   blockdev.NewProber(),
		ops:       shellFilesystem{},
	}
}

// fabric is the bottom layer of every plan: the volume's namespace, attached.
func (n *node) fabric(t Target) volstack.Layer {
	return layers.NewFabric(layers.FabricConfig{
		Connection: lvol.Connection{
			NQN:  t.NQN,
			NSID: t.NSID,
			Endpoints: []lvol.Endpoint{{
				Transport: "tcp",
				Address:   t.Address,
				Port:      t.Port,
			}},
		},
		Connector: n.connector,
		Devices:   n.devices,
		HostNQN:   n.hostNQN,
		HostID:    n.hostID,
	})
}

// filesystem is the top layer of every plan that has one.
func (n *node) filesystem(v Volume) volstack.Layer {
	return layers.NewFilesystem(layers.FilesystemConfig{
		FsType:      v.FsType,
		StagingPath: v.StagingPath,
		Ops:         n.ops,
		Content:     n.content,
	})
}

// RawBlock is `fabric`, and nothing above it. Raw block mode is the plain plan
// with its top layer absent rather than a flag inside a stage function, and
// asserting that shape here is what keeps it that way.
func (n *node) RawBlock(t Target) volstack.Plan {
	return volstack.Plan{n.fabric(t)}
}

// Plain is `fabric` → `filesystem`, the RWO plan the node service performs
// today and the one Phase 1 has to match call for call.
func (n *node) Plain(t Target, v Volume) volstack.Plan {
	return volstack.Plan{n.fabric(t), n.filesystem(v)}
}

// LVM is `fabric` → `lvmPV` → `lvmVolume` → `filesystem`, the shape a volume
// with client-side dedup or compression takes. definition decides what the
// logical volume is, so the linear and the VDO plans differ in it and in
// nothing else.
func (n *node) LVM(t Target, v Volume, definition lvm.LogicalVolumeDefinition, pool string) volstack.Plan {
	return volstack.Plan{
		n.fabric(t),
		n.physicalVolume(v),
		n.logicalVolume(v, definition, pool),
		n.filesystem(v),
	}
}

// Striped is `members(n)` → `lvmPV` → `lvmVolume(striped)` → `filesystem`, the
// striped export's plan without the export on top. It is the only plan whose
// bottom is not a single layer, which is the whole reason the composite exists.
func (n *node) Striped(targets []Target, v Volume, definition lvm.LogicalVolumeDefinition) volstack.Plan {
	members := make(volstack.Plan, 0, len(targets))
	for _, t := range targets {
		members = append(members, n.fabric(t))
	}
	return volstack.Plan{
		layers.NewMembers(members),
		n.physicalVolume(v),
		n.logicalVolume(v, definition, ""),
		n.filesystem(v),
	}
}

func (n *node) physicalVolume(v Volume) volstack.Layer {
	return layers.NewLVMPhysicalVolume(layers.LVMPhysicalVolumeConfig{
		VolumeGroup:   v.VolumeGroup(),
		LogicalVolume: v.LogicalVolume(),
		Manager:       n.manager,
		Content:       n.content,
	})
}

func (n *node) logicalVolume(v Volume, definition lvm.LogicalVolumeDefinition, pool string) volstack.Layer {
	return layers.NewLVMVolume(layers.LVMVolumeConfig{
		VolumeGroup:   v.VolumeGroup(),
		LogicalVolume: v.LogicalVolume(),
		PoolName:      pool,
		Definition:    definition,
		Manager:       n.manager,
		Resolve:       blockdev.ResolveDevice,
	})
}
