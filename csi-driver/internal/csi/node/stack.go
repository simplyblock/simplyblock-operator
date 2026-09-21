// The volume stack behind the node RPCs: the host seams a plan is built with,
// the runner that walks it, and the directory the records live in.
//
// What a node RPC does to a volume is a walk of an ordered list of layers, and
// this is where the list gets something to walk with. The seams are resolved
// once per process, because they are properties of the host rather than of a
// volume: one connector, one view of sysfs, one content reader, and one
// mounter. Only the host identity a connect presents is per-volume, since the
// control plane decides per volume whether it authorizes a named host at all.
//
// Specified by operator/docs/designs/design-node-volume-stack.md §7.5, which is
// the table saying which RPC calls which verb.

package node

import (
	"context"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/plans"

	"github.com/simplyblock/csi-driver/internal/mount"
)

// stackRecordDir is where a volume's stack record is kept. It is a host
// directory mounted into the node plugin rather than container-local storage: a
// plugin restart is an ordinary event, and the record is the only thing that
// tells the restarted process what the previous one built.
const stackRecordDir = "/var/run/simplyblock/stacks"

// stackRunner is the volume stack's runner as the node RPCs use it.
//
// It is an interface so a test can assert which verb an RPC chose without a
// fabric under it, which is the property most of these RPCs are about: an
// unstage that reached for Destroy would remove a volume the control plane
// still owns, and no fake device can make that claim provable.
type stackRunner interface {
	// Up brings a plan up and returns what the topmost layer exposes.
	Up(ctx context.Context, handle string, plan volstack.Plan) (volstack.Artifact, error)
	// Down releases a plan, top to bottom, and removes its record.
	Down(ctx context.Context, handle string, plan volstack.Plan) error
	// Heal repairs the layers that report themselves unhealthy, and creates
	// nothing.
	Heal(ctx context.Context, handle string, plan volstack.Plan) error
	// Grow enlarges the layers that can grow.
	Grow(ctx context.Context, plan volstack.Plan) error
	// Destroy removes the plan's durable objects. Only a deletion path calls it.
	Destroy(ctx context.Context, handle string, plan volstack.Plan) error
	// Observe reads a live stack without converging any of it.
	Observe(ctx context.Context, plan volstack.Plan) (volstack.Artifact, error)
}

// stack is the volume stack as one node plugin holds it.
type stack struct {
	// seams are everything a plan is built with except the host identity, which
	// is resolved per volume.
	seams plans.NodeConfig

	// store is the record directory, kept alongside the runner because the
	// teardown path reads a record the runner only writes.
	store *volstack.Store

	runner stackRunner
}

// newStack resolves the host seams and the record directory.
//
// It performs no I/O: every seam here is a constructor that reads nothing until
// it is called, which is what lets the node service be built in a test.
func newStack(mounter *mount.Mounter, recordDir string) *stack {
	sysfs := nvme.SysfsConfig{}
	store := volstack.NewStore(recordDir)

	return &stack{
		seams: plans.NodeConfig{
			Connector:  nvmeof.NewCLIConnector(nvme.NewSysfsSubsystemResolver(sysfs)),
			Devices:    nvme.NewSysfsDeviceResolver(sysfs),
			Content:    blockdev.NewProber(),
			Filesystem: mounter.FilesystemOps(),
		},
		store:  store,
		runner: volstack.NewRunner(store),
	}
}

// priorFormatFunc answers what a volume is recorded as carrying, from a record
// kept away from the device itself. The filesystem layer consults it only when
// the device reads blank, which is the reading that means either "empty" or
// "could not be read" and cannot be told apart on its own.
type priorFormatFunc func(ctx context.Context, volume plans.Volume) (string, error)

// node is the plan builder for one volume, carrying the host identity that
// volume's connect presents.
//
// The identity is per volume rather than per node because the control plane
// decides it: an access-controlled pool answers with a connect line naming the
// host it authorized, and a plain pool answers with none. Presenting a host NQN
// the control plane did not name would be worse than useless, because the
// kernel keeps one host id per host NQN and refuses every later connect on the
// node that pairs them differently.
//
// priorFormat is bound here too, since it is the one seam that needs a volume
// to answer: what a volume is recorded as carrying is read from its claim.
func (s *stack) node(hostNQN string, priorFormat priorFormatFunc) *plans.Node {
	cfg := s.seams
	cfg.HostNQN = hostNQN
	cfg.PriorFormat = priorFormat
	// NewNode derives the host id from the NQN, which the kernel keeps 1:1 with
	// it.
	return plans.NewNode(cfg)
}
