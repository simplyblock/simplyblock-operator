// Package nfsexport wires the node's export assembler to the pieces the CSI
// driver already owns.
//
// Everything substantive is in atlas/export; what lives here is the join, which
// is the driver's own seams.
//
// The theme is that an export is the host's, not this container's: its kernel
// mounts the filesystem, its nfsd serves it, and its userspace has to be able to
// make it. So mkfs, mount, and exportfs run in the host's namespace with the
// host's tools (filesystem.go). What stays here is the driver's: the
// control-plane client, and the blkid probe that decides whether formatting
// would destroy something.
package nfsexport

import (
	"context"
	"errors"
	oexec "os/exec"

	"github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/nvme"
	csimount "github.com/simplyblock/csi-driver/internal/mount"
)

// ExportsDir is where nfsd reads drop-in export entries. It is a directory
// rather than /etc/exports itself so that one export's entry can be written and
// removed without rewriting a file other things also own.
const ExportsDir = "/etc/exports.d"

// NewAssembler returns the node's export assembler.
//
// Blankness is answered by the same blkid probe the driver already uses to
// decide whether staging a volume may format it. Reusing it matters: two
// different answers to "does this device carry a filesystem" on one node is how
// a device gets formatted twice, and the second time is the one that destroys
// data.
func NewAssembler(
	devices nvme.DeviceResolver, mounter *csimount.Mounter, hostNQN HostNQNFunc,
) (*export.Assembler, error) {
	attach := attacher{hostNQN: hostNQN}
	return export.New(export.Config{
		Devices: devices,
		// The host's tools, not this container's: the image is built on a
		// newer base than the hosts it runs on, so a filesystem made here
		// carries on-disk features the host kernel cannot mount
		// (filesystem.go).
		Filesystem: hostFilesystem{},
		// The MDS host attaches its own namespace, because nothing else does:
		// no CSI call targets the host serving an export (attach.go).
		Attach: attach.Attach,
		Detach: attach.Detach,
		Blank: func(ctx context.Context, devicePath string) (bool, error) {
			fsType, err := mounter.Probe(ctx, devicePath)
			if err != nil {
				return false, err
			}
			return fsType == "", nil
		},
		Run:        runner,
		ExportsDir: ExportsDir,
	})
}

// hostMountNamespace is the host's, reached through its init process. The node
// plugin sees it because the pod shares the host's PID namespace, which is what
// spec.pnfs turns on (operator/internal/controllers/driver/pnfs.go).
const hostMountNamespace = "/proc/1/ns/mnt"

// hostCommand rewrites a command to run in the host's mount namespace.
//
// exportfs is the userspace half of the host's nfsd: it writes
// /var/lib/nfs/etab and pokes /proc/fs/nfsd, and rpc.mountd reads the same
// files. Run in this container's own namespace it would edit a copy nothing
// serves from, so the export would be written, reported as published, and be
// invisible to every client -- the worst of the failures available here,
// because everything reports success.
//
// It also is not in this image, and deliberately so. nfs-utils on a host that
// serves exports is a prerequisite this design already states (§14.1, P0-10),
// and shipping a second copy in the container would mean a host running one
// version of exportfs against an etab written by another.
//
// The separator is load bearing: without it nsenter reads the command's own
// flags as its own.
func hostCommand(name string, args ...string) (string, []string) {
	return "nsenter", append([]string{"--mount=" + hostMountNamespace, "--", name}, args...)
}

// runner executes a command in the host's mount namespace and reports its
// output and exit code, which is the shape atlas/blockdev already defines.
func runner(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	name, args = hostCommand(name, args...)
	return exec(ctx, name, args...)
}

// exec runs an already-built command. It is separate from runner because
// filesystem.go builds its own, and double-wrapping in nsenter would enter the
// host namespace from inside the host namespace.
func exec(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	cmd := oexec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *oexec.ExitError
		if errors.As(err, &exitErr) {
			// A non-zero exit is the command's answer, not a failure to run it,
			// so it is reported as a code rather than as an error. The caller
			// decides what a given code means.
			return out, exitErr.ExitCode(), nil
		}
		return out, -1, err
	}
	return out, 0, nil
}
