// Package volstack is a volume's node-side stack expressed as ordered layers.
//
// A node service assembles the same few things in different orders: a fabric
// connection, sometimes a volume manager, sometimes a filesystem, sometimes an
// export. Written inline, each combination is a branch, and the branches
// multiply with every feature that adds one. Written as layers, a combination is
// a list, and bringing a volume up is walking it.
//
// The contract is four verbs, and two of them are separate because conflating
// them destroys data. Release gives up this host's hold and keeps the data.
// Destroy removes the durable object. NodeUnstageVolume calls only the first,
// because it fires whenever no pod on this node needs the volume mounted, which
// includes an ordinary pod restart.
//
// Specified by operator/docs/designs/design-node-volume-stack.md.
package volstack

import (
	"context"

	"github.com/simplyblock/atlas/blockdev"
)

// Layer is one reversible transform in a volume's node-side stack: it takes what
// the layer below exposes and exposes something for the layer above. Every
// method is safe to re-enter, because NodeStageVolume is retried, a heal re-runs
// a live stack, and a teardown may resume after a crash.
type Layer interface {
	// Name identifies the layer in logs and in the stack record. It is stable
	// across releases: a teardown after an upgrade replays a record an earlier
	// version wrote.
	Name() string

	// Observe reports what of this layer is present on the host, and what it
	// currently exposes, without changing anything. Ensure, Release, and Destroy
	// all dispatch on what it found rather than re-deriving the same facts.
	//
	// The Artifact is the zero value at StateAbsent and is what the layer
	// exposes in every other state, StatePartial included: a fabric device that
	// is present and cannot serve is still the device a release has to detach.
	//
	// Returning it is what gives the runner a read-only way to walk a stack.
	// Down, Heal, and Grow all pass through layers they are not acting on, and
	// deriving those layers' output by calling Ensure would converge them: a
	// teardown taking that route reconnects a fabric before detaching it, and on
	// a volume whose paths are already gone, which is when an unstage arrives,
	// the reconnect waits for a device that is not coming back.
	Observe(ctx context.Context, below Artifact) (State, Artifact, error)

	// Ensure converges the layer and returns what the layer above consumes.
	Ensure(ctx context.Context, below Artifact) (Artifact, error)

	// Release drops this host's hold on the layer and keeps its data. It is the
	// only verb NodeUnstageVolume calls, and it has to succeed when the layer
	// below is already gone, which is the normal case after total path loss.
	Release(ctx context.Context, below Artifact) error

	// Destroy removes the layer's durable object. Only a deletion path calls it,
	// never an unstage.
	Destroy(ctx context.Context, below Artifact) error
}

// State is what Observe found. The distinctions matter because Ensure's response
// to each is different, and two of them are the difference between reactivating
// a volume and reformatting it.
type State int

const (
	// StateAbsent means nothing of this layer exists. Ensure creates it, which
	// is the only circumstance under which a layer may format anything.
	StateAbsent State = iota

	// StatePartial means an interrupted Ensure left an incomplete object. An LVM
	// volume group whose logical volume was never created reports zero logical
	// volumes and activates successfully while producing no usable device, so
	// the group existing is not the same question as the layer being ready.
	StatePartial

	// StateForeign means the object exists but carries another volume's
	// identity. A byte-level clone copies its source's LVM metadata, so the
	// clone's device claims the source's volume group until vgimportclone
	// renames it.
	StateForeign

	// StateInactive means the object is complete but not currently mapped on
	// this host. It is what Release leaves behind and what a node reboot leaves
	// behind, and Ensure reactivates rather than recreating.
	StateInactive

	// StateReady means present, complete, and usable.
	StateReady
)

// String names the state for a log line, an event, and a test failure.
func (s State) String() string {
	switch s {
	case StateAbsent:
		return "Absent"
	case StatePartial:
		return "Partial"
	case StateForeign:
		return "Foreign"
	case StateInactive:
		return "Inactive"
	case StateReady:
		return "Ready"
	default:
		return "State(?)"
	}
}

// Artifact is what one layer hands to the layer above it. It carries what a
// higher layer can act on and nothing about how the layer below produced it.
type Artifact struct {
	// Devices are the block devices this layer exposes, in a defined order. A
	// fan-in layer exposes several; every other layer exposes one.
	Devices []blockdev.Device

	// Path is the filesystem path this layer mounted, empty until a layer mounts
	// one.
	Path string

	// Geometry is the stripe layout of Devices, for a layer above that aligns to
	// it.
	Geometry Geometry
}

// Device is the single device an artifact exposes, and reports whether there was
// one. A layer that consumes one device asks for it here rather than indexing,
// so a plan that put a fan-in layer where a single device belongs fails with
// something a reader can act on.
func (a Artifact) Device() (blockdev.Device, bool) {
	if len(a.Devices) != 1 {
		return blockdev.Device{}, false
	}
	return a.Devices[0], true
}

// Geometry is a stripe layout: the per-stripe chunk size and the number of
// stripes data is spread across. The zero value means unknown, which is the
// correct answer for a device whose blocks are virtualized.
type Geometry struct {
	ChunkBytes int64
	Stripes    int
}

// Known reports whether this geometry describes a layout a layer above can align
// to. A virtualized device reports the zero value, and a filesystem over one
// passes no stripe alignment because there is nothing real to align to.
func (g Geometry) Known() bool { return g.ChunkBytes > 0 && g.Stripes > 0 }

// Healer is implemented by a layer whose object can go bad under a live stack
// and be repaired in place. Heal never recreates: the data already exists.
type Healer interface {
	// Healthy is a read. It reports whether this layer is currently serving.
	Healthy(ctx context.Context, own Artifact) (bool, error)

	// Heal repairs the layer against the layer below, which may itself have just
	// been healed.
	Heal(ctx context.Context, below, own Artifact) error
}

// Grower is implemented by a layer that has to be enlarged when the volume
// behind it grows. Grow is convergent: a layer already at its target size
// succeeds without doing anything, because kubelet reissues NodeExpandVolume
// after it has already succeeded.
type Grower interface {
	Grow(ctx context.Context, below Artifact) (Artifact, error)
}

// Capability is a label a node must carry for a layer to run on it.
type Capability string

// NodeRequirements is implemented by a layer that constrains where the volume
// may be staged: it needs something from the node, or its durable state stays
// there. The volume carrying it can then be staged only on a node that can run
// the layer, and only on one node at a time.
//
// Unlike Healer and Grower this is a declaration rather than an action, which is
// why it is a noun: the runner interrogates it instead of calling it.
type NodeRequirements interface {
	// NodeCapability is the label a node must carry, or the zero value when any
	// node will do.
	NodeCapability() Capability

	// PinsToNode reports whether this layer's durable state lives on the host.
	PinsToNode() bool
}

// Recorder is implemented by a layer that was constructed with parameters a
// later process needs in order to rebuild it.
//
// The stack record holds the plan and what each layer was built with, and this
// is how a layer contributes the second half. The value is opaque to the runner:
// the layer that declared it is the only thing that parses it, which is what
// lets a new layer ship without the record format changing.
//
// A layer whose identity is fully determined by the volume handle implements
// nothing here, and its entry carries no parameters: the physical-volume layer
// is the worked example, since a pvcreate needs only the device below it.
//
// It returns no error because a layer returning what it was built with cannot
// fail at it. Encoding can, and that belongs to the runner, which is the thing
// that owns the record's format.
type Recorder interface {
	Params() any
}

// Plan is a volume's stack, bottom layer first. Up walks it forward and Down
// walks it back, which is most of why the order is recorded rather than
// re-derived from a StorageClass that may have been edited since.
type Plan []Layer

// Names is the plan's layer names in order, for a log line and for the record.
func (p Plan) Names() []string {
	names := make([]string, 0, len(p))
	for _, l := range p {
		names = append(names, l.Name())
	}
	return names
}
