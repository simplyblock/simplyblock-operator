// The volume layer: one volume group holding one logical volume.
//
// It is the layer that both creates and activates, and the whole of its risk is
// telling those apart. A volume group present but not mapped on this host is a
// volume to reactivate; the same group read as absent is a volume to create, and
// creating over the first destroys it. So absence here means the device carries
// no volume group at all, established by asking the device, and never inferred
// from this volume's own group not being found.

package layers

import (
	"context"
	"errors"
	"fmt"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
)

// DeviceResolver answers what the kernel says about a device path, which
// blockdev.ResolveDevice implements. It is a seam because the layer creates a
// device-mapper node and has to describe it to the layer above, and a test has no
// device-mapper.
type DeviceResolver func(path string) (blockdev.Device, error)

// LVMVolumeConfig is what a volume layer is built with.
type LVMVolumeConfig struct {
	// VolumeGroup and LogicalVolume are derived from the volume's identity, so a
	// plan replayed on another host arrives at the same names.
	VolumeGroup   string
	LogicalVolume string

	// PoolName is lvcreate's <vg>/<pool> target, for a type that creates its
	// logical volume inside a pool. It is empty for a linear or striped volume,
	// which has no pool.
	PoolName string

	// Definition is what the logical volume is to be. It decides the arguments
	// lvcreate is given, and with them the geometry this layer reports upward, so
	// the two cannot disagree.
	Definition lvm.LogicalVolumeDefinition

	// Capability is what a node must carry for this volume's type to run there,
	// and is empty when any node will do.
	Capability volstack.Capability

	Manager *lvm.Manager
	Resolve DeviceResolver
}

// LVMVolume is one volume group holding one logical volume.
type LVMVolume struct {
	cfg LVMVolumeConfig
}

// NewLVMVolume returns the volume layer for one volume.
func NewLVMVolume(cfg LVMVolumeConfig) *LVMVolume {
	if cfg.Resolve == nil {
		cfg.Resolve = blockdev.ResolveDevice
	}
	return &LVMVolume{cfg: cfg}
}

// Name is what the record calls this layer.
func (l *LVMVolume) Name() string { return "lvmVolume" }

// group is this volume's volume group.
func (l *LVMVolume) group() lvm.VolumeGroup { return lvm.VolumeGroup{Name: l.cfg.VolumeGroup} }

// volume is this volume's logical volume.
func (l *LVMVolume) volume() lvm.LogicalVolume {
	return lvm.LogicalVolume{VolumeGroup: l.group(), Name: l.cfg.LogicalVolume}
}

// path is where the logical volume is mapped once it is active.
func (l *LVMVolume) path() string { return "/dev/" + l.cfg.VolumeGroup + "/" + l.cfg.LogicalVolume }

// Observe reports how much of this layer is on the device below.
//
// The four states are four different things to do, and the first two are the
// ones that matter: Absent creates, Inactive reactivates, and confusing them
// destroys a volume. Partial is the interrupted create, whose volume group
// activates successfully while producing no usable device, and Ready is complete
// and mapped.
func (l *LVMVolume) Observe(
	ctx context.Context, below volstack.Artifact,
) (volstack.State, volstack.Artifact, error) {
	if len(below.Devices) == 0 {
		return volstack.StateAbsent, volstack.Artifact{}, errors.New(
			"lvmVolume: the layer below exposes no devices to build a volume group on")
	}

	// Read the identity off the device rather than looking this volume's group up
	// by name. A name lookup answers "not found" for a device carrying somebody
	// else's group, and a caller acting on that runs vgcreate over their data.
	pv := lvm.PhysicalVolume{DevicePath: below.Devices[0].Path}
	group, err := l.cfg.Manager.VolumeGroup(ctx, pv)
	if err != nil {
		return volstack.StateAbsent, volstack.Artifact{}, fmt.Errorf(
			"lvmVolume: read the volume group on %s: %w", pv.DevicePath, err)
	}

	switch group.Name {
	case "":
		return volstack.StateAbsent, volstack.Artifact{}, nil
	case l.cfg.VolumeGroup:
		return l.inspect(ctx)
	default:
		// The layer below re-identifies a clone, and by the time a bring-up reaches
		// here it has. Meeting one anyway means the plan is wrong or the walk is a
		// read-only one over a stack that was never brought up, and neither is a
		// thing to converge: an error strands a teardown, where creating over it
		// would destroy the clone.
		return volstack.StateAbsent, volstack.Artifact{}, fmt.Errorf(
			"lvmVolume: %s carries volume group %s rather than %s, which lvmPV resolves and this layer may not",
			pv.DevicePath, group.Name, l.cfg.VolumeGroup)
	}
}

// inspect answers how complete this volume's own group is.
func (l *LVMVolume) inspect(ctx context.Context) (volstack.State, volstack.Artifact, error) {
	has, err := l.cfg.Manager.HasLogicalVolume(ctx, l.volume())
	if err != nil {
		return volstack.StateAbsent, volstack.Artifact{}, fmt.Errorf(
			"lvmVolume: list the volumes in %s: %w", l.cfg.VolumeGroup, err)
	}
	if !has {
		// The group exists and holds nothing. It activates successfully and
		// produces no usable device, so every stage would otherwise reactivate an
		// empty group forever.
		return volstack.StatePartial, volstack.Artifact{}, nil
	}

	active, err := l.cfg.Manager.LogicalVolumeActive(ctx, l.volume())
	if err != nil {
		return volstack.StateAbsent, volstack.Artifact{}, fmt.Errorf(
			"lvmVolume: read the state of %s/%s: %w", l.cfg.VolumeGroup, l.cfg.LogicalVolume, err)
	}
	if !active {
		// Complete, and not mapped here. It exposes no device until it is, which is
		// what the layer above waits for.
		return volstack.StateInactive, volstack.Artifact{}, nil
	}

	own, err := l.artifact()
	if err != nil {
		return volstack.StateAbsent, volstack.Artifact{}, err
	}
	return volstack.StateReady, own, nil
}

// artifact is the mapped logical volume, described for the layer above.
func (l *LVMVolume) artifact() (volstack.Artifact, error) {
	dev, err := l.cfg.Resolve(l.path())
	if err != nil {
		return volstack.Artifact{}, fmt.Errorf("lvmVolume: resolve %s: %w", l.path(), err)
	}
	return volstack.Artifact{Devices: []blockdev.Device{dev}, Geometry: l.geometry()}, nil
}

// geometry is the stripe layout the layer above may align to, read from the same
// definition lvcreate was built from so that the two cannot disagree. A linear
// volume and a virtualized one both report the zero value, which is the correct
// answer for a device whose blocks are not laid out in stripes at all.
func (l *LVMVolume) geometry() volstack.Geometry {
	if l.cfg.Definition.Stripes < 2 {
		return volstack.Geometry{}
	}
	return volstack.Geometry{
		ChunkBytes: l.cfg.Definition.StripeChunkBytes,
		Stripes:    l.cfg.Definition.Stripes,
	}
}

// Ensure brings the volume to Ready from wherever Observe found it, creating only
// from Absent.
func (l *LVMVolume) Ensure(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	state, own, err := l.Observe(ctx, below)
	if err != nil {
		return volstack.Artifact{}, err
	}

	switch state {
	case volstack.StateReady:
		return own, nil

	case volstack.StateAbsent:
		pvs := make([]lvm.PhysicalVolume, 0, len(below.Devices))
		for _, dev := range below.Devices {
			pvs = append(pvs, lvm.PhysicalVolume{DevicePath: dev.Path})
		}
		if _, err := l.cfg.Manager.CreateVolumeGroup(ctx, l.group(), pvs...); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmVolume: %w", err)
		}
		if err := l.create(ctx); err != nil {
			return volstack.Artifact{}, err
		}

	case volstack.StatePartial:
		// The group is already there, so this completes the create rather than
		// starting one: a second vgcreate here would be over the first.
		if err := l.create(ctx); err != nil {
			return volstack.Artifact{}, err
		}

	case volstack.StateInactive:
		if err := l.cfg.Manager.ActivateVolumeGroup(ctx, l.group()); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmVolume: %w", err)
		}

	case volstack.StateForeign:
		// Observe reports the foreign case as an error rather than a state, so this
		// is unreachable, and saying so beats a silent fallthrough into a create.
		return volstack.Artifact{}, errors.New(
			"lvmVolume: a foreign volume group reached Ensure, which Observe refuses to report")
	}

	return l.artifact()
}

// create makes the logical volume, and is what both a fresh create and an
// interrupted one end in.
func (l *LVMVolume) create(ctx context.Context) error {
	if _, err := l.cfg.Manager.CreateLogicalVolume(
		ctx, l.group(), l.cfg.PoolName, l.cfg.LogicalVolume, l.cfg.Definition); err != nil {
		return fmt.Errorf("lvmVolume: %w", err)
	}
	return nil
}

// Release unmaps the volume on this host and keeps every byte of it. It is what
// an unstage calls, and an unstage fires on an ordinary pod restart.
//
// The force path is not exceptional. When the backing device is gone, LVM can no
// longer read the metadata a clean deactivation needs, and the device-mapper
// nodes stay live with nothing able to clear them; removing them directly is what
// keeps a dead stack from stranding the one below it.
func (l *LVMVolume) Release(ctx context.Context, _ volstack.Artifact) error {
	if err := l.cfg.Manager.DeactivateVolumeGroup(ctx, l.group()); err == nil {
		return nil
	}
	if err := l.cfg.Manager.RemoveOrphanedDMNodes(ctx, l.group()); err != nil {
		return fmt.Errorf("lvmVolume: deactivate %s, including its force path: %w", l.cfg.VolumeGroup, err)
	}
	return nil
}

// Destroy removes the volume and the data in it, the logical volume first and
// then the group that held it. Only a deletion path calls it.
func (l *LVMVolume) Destroy(ctx context.Context, _ volstack.Artifact) error {
	if err := l.cfg.Manager.RemoveLogicalVolume(ctx, l.volume()); err != nil {
		return fmt.Errorf("lvmVolume: %w", err)
	}
	if err := l.cfg.Manager.RemoveVolumeGroup(ctx, l.group()); err != nil {
		return fmt.Errorf("lvmVolume: %w", err)
	}
	return nil
}

// Grow takes the space the members below gained.
//
// Each member is resized first, because the volume group has no new space to give
// until its physical volumes report the larger device, and then the logical volume
// takes it. Convergent throughout: kubelet reissues NodeExpandVolume after one
// that already succeeded, and a volume already at its target is what that retry
// finds.
func (l *LVMVolume) Grow(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	for _, dev := range below.Devices {
		if err := l.cfg.Manager.ExpandPhysicalVolume(ctx, lvm.PhysicalVolume{DevicePath: dev.Path}); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmVolume: %w", err)
		}
	}
	if err := l.cfg.Manager.ExpandLogicalVolume(ctx, l.target()); err != nil {
		return volstack.Artifact{}, fmt.Errorf("lvmVolume: %w", err)
	}

	// A pooled type sizes its logical volume independently of the pool's physical
	// size, so the volume has to be told to match what the pool just became.
	if l.cfg.PoolName != "" {
		pool := lvm.LogicalVolume{VolumeGroup: l.group(), Name: l.cfg.PoolName}
		size, err := l.cfg.Manager.LogicalVolumeSize(ctx, pool)
		if err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmVolume: %w", err)
		}
		if err := l.cfg.Manager.ExtendLogicalVolumeToSize(ctx, l.volume(), size); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmVolume: %w", err)
		}
	}
	return l.artifact()
}

// target is what the physical space is extended into: the pool when there is one,
// since that is what holds the extents, and the logical volume itself otherwise.
func (l *LVMVolume) target() lvm.LogicalVolume {
	if l.cfg.PoolName != "" {
		return lvm.LogicalVolume{VolumeGroup: l.group(), Name: l.cfg.PoolName}
	}
	return l.volume()
}

// NodeCapability is what a node must carry for this volume's type to run there.
// A type needing a kernel module the node does not have fails as a mount error on
// the wrong node otherwise, discovered instead of reported.
func (l *LVMVolume) NodeCapability() volstack.Capability { return l.cfg.Capability }

// PinsToNode reports false: this layer's durable state is the LVM metadata, which
// lives on the device and travels with it. Nothing of it stays on the host.
func (l *LVMVolume) PinsToNode() bool { return false }

// LVMVolumeParams is what the record carries for this layer.
type LVMVolumeParams struct {
	PoolName         string `json:"poolName,omitempty"`
	Deduplication    bool   `json:"deduplication,omitempty"`
	Compression      bool   `json:"compression,omitempty"`
	Stripes          int    `json:"stripes,omitempty"`
	StripeChunkBytes int64  `json:"stripeChunkBytes,omitempty"`
}

// Params is what a later process needs in order to rebuild this layer: what the
// volume was made to be. The names are derived from the volume's identity and the
// devices come from the layer below, so what is left is the definition, and it is
// recorded rather than re-derived from a StorageClass that may have been edited
// since the volume was created.
func (l *LVMVolume) Params() any {
	return LVMVolumeParams{
		PoolName:         l.cfg.PoolName,
		Deduplication:    l.cfg.Definition.Deduplication,
		Compression:      l.cfg.Definition.Compression,
		Stripes:          l.cfg.Definition.Stripes,
		StripeChunkBytes: l.cfg.Definition.StripeChunkBytes,
	}
}
