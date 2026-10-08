// Package nfsexport wires atlas/nfsexport to the pieces the CSI driver owns.
//
// Its assembler runs in the metadata server's guest (cmd/mds-agent), the one
// host that mounts an export's filesystem and publishes it through its own
// nfsd. A client node uses only the attach half (Attach, HostNQN), to connect
// the namespace the guest made the filesystem on.
package nfsexport

import (
	"context"
	"errors"
	oexec "os/exec"

	"github.com/simplyblock/atlas/blockdev"
	export "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/plans"

	csimount "github.com/simplyblock/csi-driver/internal/mount"
)

const (
	// ExportsDir is where nfsd reads drop-in entries. A directory rather than
	// /etc/exports, so one entry can be written without rewriting a shared file.
	ExportsDir = "/etc/exports.d"

	// Where nfs-utils keeps the export table and nfsdcld its client recovery
	// database. In the guest it is the state disk, which outlives a restart.
	NFSStateDir = "/var/lib/nfs"

	// stackRecordDir is where an export's stack record is kept. The handles
	// are prefixed (export.Spec.StackHandle), so they cannot collide with a
	// block volume's.
	stackRecordDir = "/var/run/simplyblock/stacks"
)

// NewAssembler returns the metadata server's export assembler.
func NewAssembler(
	devices nvme.DeviceResolver, mounter *csimount.Mounter, hostNQN HostNQNFunc,
) (*export.Assembler, error) {
	store := volstack.NewStore(stackRecordDir)
	build := planner{
		seams: plans.NodeConfig{
			Connector: nvmeof.NewCLIConnector(nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{})),
			Devices:   devices,
			// The same probe the block path formats against: two answers to
			// "does this device carry a filesystem" on one node is how a device
			// is formatted twice, and the second time destroys data.
			Content:    blockdev.NewProber(),
			Filesystem: mounter.FilesystemOps(),
		},
		hostNQN: hostNQN,
		store:   store,
	}
	return export.New(export.Config{
		Plan:       build.Plan,
		Stack:      volstack.NewRunner(store),
		Run:        run,
		ExportsDir: ExportsDir,
	})
}

// run is what atlas/nfsexport calls exportfs with, in atlas/blockdev's shape.
func run(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	cmd := oexec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *oexec.ExitError
		if errors.As(err, &exitErr) {
			// The command's answer, not a failure to run it.
			return out, exitErr.ExitCode(), nil
		}
		return out, -1, err
	}
	return out, 0, nil
}
