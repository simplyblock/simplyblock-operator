// Package nfsexport wires atlas/export to the pieces the CSI driver owns.
//
// Everything runs in this container. The mount reaches the host through the
// bidirectional propagation on the export root, and exportfs reaches the host's
// nfsd through the state directories mounted in. Neither needs the host's PID
// namespace, and the plugin does not have it.
package nfsexport

import (
	"context"
	"errors"
	oexec "os/exec"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/export"
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

	// Where nfs-utils keeps the export table. rpc.mountd on the host reads the
	// same file, so it is mounted in: against this container's own copy the
	// export would report success and be invisible to every client.
	NFSStateDir = "/var/lib/nfs"

	// stackRecordDir is where an export's stack record is kept, which is the
	// host directory the node plugin already records into. The handles cannot
	// collide: an export's is prefixed (export.Spec.StackHandle).
	stackRecordDir = "/var/run/simplyblock/stacks"
)

// NewAssembler returns the node's export assembler.
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

// run is what atlas/export calls exportfs with, in atlas/blockdev's shape.
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
