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
	"slices"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
)

// DeviceResolver answers what the kernel says about a device path, which
// blockdev.ResolveDevice implements. It is a seam because the layer creates a
// device-mapper node and has to describe it to the layer above, and a test has no
// device-mapper.
type DeviceResolver func(path string) (blockdev.Device, error)

// LVMLogicalVolumeConfig is what a volume layer is built with.
type LVMLogicalVolumeConfig struct {
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

// LVMLogicalVolume is the one volume inside its group. The group itself, and
// which members it is made of, belong to the layer below.
type LVMLogicalVolume struct {
	cfg LVMLogicalVolumeConfig
}

// NewLVMVolume returns the volume layer for one volume.
func NewLVMLogicalVolume(cfg LVMLogicalVolumeConfig) *LVMLogicalVolume {
	if cfg.Resolve == nil {
		cfg.Resolve = blockdev.ResolveDevice
	}
	return &LVMLogicalVolume{cfg: cfg}
}

// Name is what the record calls this layer.
func (l *LVMLogicalVolume) Name() string { return "lvmLogicalVolume" }

// group is this volume's volume group.
func (l *LVMLogicalVolume) group() lvm.VolumeGroup { return lvm.VolumeGroup{Name: l.cfg.VolumeGroup} }

// volume is this volume's logical volume.
func (l *LVMLogicalVolume) volume() lvm.LogicalVolume {
	return lvm.LogicalVolume{VolumeGroup: l.group(), Name: l.cfg.LogicalVolume}
}

// path is where the logical volume is mapped once it is active.
func (l *LVMLogicalVolume) path() string {
	return "/dev/" + l.cfg.VolumeGroup + "/" + l.cfg.LogicalVolume
}

// Observe reports how much of this layer is on the device below.
//
// The four states are four different things to do, and the first two are the
// ones that matter: Absent creates, Inactive reactivates, and confusing them
// destroys a volume. Partial is a group of ours without this volume in it, which
// activates successfully while producing no usable device: an interrupted create
// when the group is empty or holds only the pool a pooled create makes first,
// and somebody's data otherwise, which Ensure tells apart. Ready is complete and
// mapped.
func (l *LVMLogicalVolume) Observe(
	ctx context.Context, below volstack.Artifact,
) (volstack.State, volstack.Artifact, error) {
	if len(below.Devices) == 0 {
		// Total path loss, not an interrupted bring-up: there is no device left
		// to read a group off at all, which is a different question from a probe
		// that failed on a device that is there. Release is unconditionally a
		// no-op for this layer regardless of what Observe reports, so answering
		// Absent here costs nothing, and it is what keeps a Down walk from being
		// aborted here rather than reaching the group layer below, which has
		// real work to do and answers this same situation independently
		// (lvmVolumeGroup.Observe).
		return volstack.StateAbsent, volstack.Artifact{}, nil
	}

	// Read the identity off the device rather than looking this volume's group up
	// by name. A name lookup answers "not found" for a device carrying somebody
	// else's group, and a caller acting on that runs vgcreate over their data.
	pv := lvm.PhysicalVolume{DevicePath: below.Devices[0].Path}
	group, err := l.cfg.Manager.VolumeGroup(ctx, pv)
	if err != nil {
		return volstack.StateAbsent, volstack.Artifact{}, fmt.Errorf(
			"lvmLogicalVolume: read the volume group on %s: %w", pv.DevicePath, err)
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
			"lvmLogicalVolume: %s carries volume group %s rather than %s, which lvmPhysicalVolume resolves and this layer may not",
			pv.DevicePath, group.Name, l.cfg.VolumeGroup)
	}
}

// inspect answers how complete this volume's own group is.
func (l *LVMLogicalVolume) inspect(ctx context.Context) (volstack.State, volstack.Artifact, error) {
	has, err := l.cfg.Manager.HasLogicalVolume(ctx, l.volume())
	if err != nil {
		return volstack.StateAbsent, volstack.Artifact{}, fmt.Errorf(
			"lvmLogicalVolume: list the volumes in %s: %w", l.cfg.VolumeGroup, err)
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
			"lvmLogicalVolume: read the state of %s/%s: %w", l.cfg.VolumeGroup, l.cfg.LogicalVolume, err)
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
func (l *LVMLogicalVolume) artifact() (volstack.Artifact, error) {
	dev, err := l.cfg.Resolve(l.path())
	if err != nil {
		return volstack.Artifact{}, fmt.Errorf("lvmLogicalVolume: resolve %s: %w", l.path(), err)
	}
	return volstack.Artifact{Devices: []blockdev.Device{dev}, Geometry: l.geometry()}, nil
}

// geometry is the stripe layout the layer above may align to, read from the same
// definition lvcreate was built from so that the two cannot disagree. A linear
// volume and a virtualized one both report the zero value, which is the correct
// answer for a device whose blocks are not laid out in stripes at all.
func (l *LVMLogicalVolume) geometry() volstack.Geometry {
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
func (l *LVMLogicalVolume) Ensure(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	state, own, err := l.Observe(ctx, below)
	if err != nil {
		return volstack.Artifact{}, err
	}

	switch state {
	case volstack.StateReady:
		// Complete and mapped. The one thing to do is clear a marker a create
		// left behind, which is what a failed vgchange --deltag after a
		// successful lvcreate leaves.
		if err := l.clearStaleMarker(ctx); err != nil {
			return volstack.Artifact{}, err
		}
		return own, nil

	case volstack.StateAbsent:
		// A group with nothing in it, made by the layer below and never filled.
		if err := l.create(ctx); err != nil {
			return volstack.Artifact{}, err
		}

	case volstack.StatePartial:
		// A group that exists and does not hold this volume. What else it holds
		// decides whether a create may run, and for a pooled type an interrupted
		// create is what it usually holds.
		if err := l.completeInterruptedCreate(ctx); err != nil {
			return volstack.Artifact{}, err
		}

	case volstack.StateInactive:
		// The layer below maps the group and everything in it, so reaching here
		// means the volume alone was taken down. Mapping it again is convergent
		// and costs nothing when it is already mapped.
		if err := l.cfg.Manager.ActivateVolumeGroup(ctx, l.group()); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmLogicalVolume: %w", err)
		}
		if err := l.clearStaleMarker(ctx); err != nil {
			return volstack.Artifact{}, err
		}

	case volstack.StateForeign:
		// Observe reports the foreign case as an error rather than a state, so this
		// is unreachable, and saying so beats a silent fallthrough into a create.
		return volstack.Artifact{}, errors.New(
			"lvmLogicalVolume: a foreign volume group reached Ensure, which Observe refuses to report")
	}

	return l.artifact()
}

// creatingMarker is the tag the group carries while lvcreate runs.
//
// lvcreate is one command and, for a pooled type, several LVM commits: the pool
// as a plain volume, then formatted, then converted, and the volume inside it
// last. A node that dies between the first and the last leaves the pool behind
// under the name the retry needs, and nothing on the device says whether that
// pool is this layer's unfinished work or somebody's volume. The marker says so.
// It goes on before lvcreate and comes off after, in the group's own metadata,
// so it survives the reboot, follows the volume to another node, and is copied
// into a clone taken from a source that was mid-create, which carries no data
// either.
//
// Spelled out on disk, so it is a contract rather than a name: the tests write
// the same string, and a recovery on another build reads it.
const creatingMarker = "simplyblock.creating"

// create makes the logical volume, and is what both a fresh create and an
// interrupted one end in. The marker brackets lvcreate, which is the only
// command here that a crash can leave half done.
func (l *LVMLogicalVolume) create(ctx context.Context) error {
	if err := l.cfg.Manager.AddVolumeGroupTag(ctx, l.group(), creatingMarker); err != nil {
		return fmt.Errorf("lvmLogicalVolume: %w", err)
	}
	if _, err := l.cfg.Manager.CreateLogicalVolume(
		ctx, l.group(), l.cfg.PoolName, l.cfg.LogicalVolume, l.cfg.Definition); err != nil {
		return fmt.Errorf("lvmLogicalVolume: %w", err)
	}
	if err := l.cfg.Manager.RemoveVolumeGroupTag(ctx, l.group(), creatingMarker); err != nil {
		return fmt.Errorf("lvmLogicalVolume: %w", err)
	}
	return nil
}

// clearStaleMarker takes the marker off a group whose volume exists.
//
// create removes it after lvcreate, and that removal can fail with the volume
// already made. The group is complete then, and the marker says nothing true
// about it: it is not an interrupted create, and a clone of it must not read the
// marker as permission to remove anything. So every bring-up that finds the
// volume takes the marker off on the way through, which costs one vgs read and,
// almost always, nothing else.
func (l *LVMLogicalVolume) clearStaleMarker(ctx context.Context) error {
	tags, err := l.cfg.Manager.VolumeGroupTags(ctx, l.group())
	if err != nil {
		return fmt.Errorf("lvmLogicalVolume: %w", err)
	}
	if !slices.Contains(tags, creatingMarker) {
		return nil
	}
	if err := l.cfg.Manager.RemoveVolumeGroupTag(ctx, l.group(), creatingMarker); err != nil {
		return fmt.Errorf("lvmLogicalVolume: clear the stale marker: %w", err)
	}
	return nil
}

// completeInterruptedCreate finishes a create that a group without this volume
// is the remains of, when that is what the group is.
//
// Three readings, and only two of them are a create to complete. A group with
// nothing in it is the plain interrupted create, where the layer below finished
// and this one never ran, and lvcreate is simply run. A group holding this
// volume's pool and nothing else, under the marker create put there, is a
// pooled create that died between LVM's commits: no volume ever existed inside
// that pool, so nothing can have written into it, and the marker says it is this
// layer's own work. The pool is removed and lvcreate is run again. Any other
// content is refused: a pool without the marker was not made by a create of
// ours that this layer knows how to finish, and a group holding any volume that
// is not ours holds somebody's data, whether or not the marker is there. The
// refusal names what it found, because the alternative is a stage retried
// forever with LVM's own message and nothing to say why.
func (l *LVMLogicalVolume) completeInterruptedCreate(ctx context.Context) error {
	volumes, err := l.cfg.Manager.ListLogicalVolumes(ctx, l.group())
	if err != nil {
		return fmt.Errorf("lvmLogicalVolume: list the volumes in %s: %w", l.cfg.VolumeGroup, err)
	}
	if len(volumes) == 0 {
		return l.create(ctx)
	}

	names := make([]string, 0, len(volumes))
	for _, v := range volumes {
		names = append(names, v.Name)
	}
	onlyThePool := l.cfg.PoolName != "" && len(volumes) == 1 && volumes[0].Name == l.cfg.PoolName
	if !onlyThePool {
		return fmt.Errorf(
			"lvmLogicalVolume: %s holds %v and not %s, which is not an interrupted create this layer can finish; nothing was removed",
			l.cfg.VolumeGroup, names, l.cfg.LogicalVolume)
	}

	tags, err := l.cfg.Manager.VolumeGroupTags(ctx, l.group())
	if err != nil {
		return fmt.Errorf("lvmLogicalVolume: %w", err)
	}
	if !slices.Contains(tags, creatingMarker) {
		return fmt.Errorf(
			"lvmLogicalVolume: %s holds the pool %s and no volume, and carries no %s marker, so the pool is not known to be this layer's interrupted work; nothing was removed",
			l.cfg.VolumeGroup, l.cfg.PoolName, creatingMarker)
	}

	pool := lvm.LogicalVolume{VolumeGroup: l.group(), Name: l.cfg.PoolName}
	if err := l.cfg.Manager.RemoveLogicalVolume(ctx, pool); err != nil {
		return fmt.Errorf("lvmLogicalVolume: remove the orphaned pool: %w", err)
	}
	return l.create(ctx)
}

// Release does nothing, because what holds a logical volume on a host is its
// group being mapped there, and the group is the layer below. An unstage walks
// down through both, so the hold is given up either way, and giving it up twice
// would mean this layer deactivating everything else in the group along with its
// own volume.
func (l *LVMLogicalVolume) Release(context.Context, volstack.Artifact) error { return nil }

// Destroy removes the volume and the data in it. The group that held it goes
// with the layer below, which a teardown reaches next. Only a deletion path
// calls either.
func (l *LVMLogicalVolume) Destroy(ctx context.Context, _ volstack.Artifact) error {
	if err := l.cfg.Manager.RemoveLogicalVolume(ctx, l.volume()); err != nil {
		return fmt.Errorf("lvmLogicalVolume: %w", err)
	}
	return nil
}

// Grow takes the space the group below now has.
//
// Only takes it. Making the space is the group's, whether that meant resizing the
// members it already had or accepting new ones, and by the time a walk reaches
// here the free extents are there to be claimed. Convergent: kubelet reissues
// NodeExpandVolume after one that already succeeded, and a volume already at its
// target is what that retry finds.
func (l *LVMLogicalVolume) Grow(ctx context.Context, _ volstack.Artifact) (volstack.Artifact, error) {
	if err := l.cfg.Manager.ExpandLogicalVolume(ctx, l.target()); err != nil {
		return volstack.Artifact{}, fmt.Errorf("lvmLogicalVolume: %w", err)
	}

	// A pooled type sizes its logical volume independently of the pool's physical
	// size, so the volume has to be told to match what the pool just became.
	if l.cfg.PoolName != "" {
		pool := lvm.LogicalVolume{VolumeGroup: l.group(), Name: l.cfg.PoolName}
		size, err := l.cfg.Manager.LogicalVolumeSize(ctx, pool)
		if err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmLogicalVolume: %w", err)
		}
		if err := l.cfg.Manager.ExtendLogicalVolumeToSize(ctx, l.volume(), size); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmLogicalVolume: %w", err)
		}
	}
	return l.artifact()
}

// target is what the physical space is extended into: the pool when there is one,
// since that is what holds the extents, and the logical volume itself otherwise.
func (l *LVMLogicalVolume) target() lvm.LogicalVolume {
	if l.cfg.PoolName != "" {
		return lvm.LogicalVolume{VolumeGroup: l.group(), Name: l.cfg.PoolName}
	}
	return l.volume()
}

// NodeCapability is what a node must carry for this volume's type to run there.
// A type needing a kernel module the node does not have fails as a mount error on
// the wrong node otherwise, discovered instead of reported.
func (l *LVMLogicalVolume) NodeCapability() volstack.Capability { return l.cfg.Capability }

// PinsToNode reports false: this layer's durable state is the LVM metadata, which
// lives on the device and travels with it. Nothing of it stays on the host.
func (l *LVMLogicalVolume) PinsToNode() bool { return false }

// LVMLogicalVolumeParams is what the record carries for this layer.
type LVMLogicalVolumeParams struct {
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
func (l *LVMLogicalVolume) Params() any {
	return LVMLogicalVolumeParams{
		PoolName:         l.cfg.PoolName,
		Deduplication:    l.cfg.Definition.Deduplication,
		Compression:      l.cfg.Definition.Compression,
		Stripes:          l.cfg.Definition.Stripes,
		StripeChunkBytes: l.cfg.Definition.StripeChunkBytes,
	}
}
