// Package nfsexport wires the node's export assembler to the pieces the CSI
// driver already owns.
//
// Everything substantive is in atlas: assembling an export is atlas/export, and
// carrying the call is atlas/export/exportrpc. What lives here is only the
// join -- the driver's mounter satisfying the filesystem interface, its blkid
// probe answering whether a device is blank, and its exec runner running
// exportfs -- because those are the driver's own seams and atlas has no
// business knowing them.
package nfsexport

import (
	"context"
	"errors"
	"os/exec"

	"github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/nvme"
	csimount "github.com/simplyblock/csi-driver/internal/mount"
)

// ExportsDir is where nfsd reads drop-in export entries. It is a directory
// rather than /etc/exports itself so that one export's entry can be written and
// removed without rewriting a file other things also own.
const ExportsDir = "/etc/exports.d"

// filesystem adapts the driver's mounter to what atlas/export needs. The
// signatures differ only by a context, which the mounter predates.
type filesystem struct {
	mounter *csimount.Mounter
}

func (f filesystem) Format(ctx context.Context, device, fsType string, options []string) error {
	return f.mounter.Format(ctx, device, fsType, options)
}

func (f filesystem) Mount(_ context.Context, source, target, fsType string, options []string) error {
	return f.mounter.Mount(source, target, fsType, options)
}

func (f filesystem) Unmount(_ context.Context, target string) error {
	return f.mounter.Unmount(target)
}

func (f filesystem) IsMountPoint(_ context.Context, path string) (bool, error) {
	return f.mounter.IsMounted(path)
}

// NewAssembler returns the node's export assembler.
//
// Blankness is answered by the same blkid probe the driver already uses to
// decide whether staging a volume may format it. Reusing it matters: two
// different answers to "does this device carry a filesystem" on one node is how
// a device gets formatted twice, and the second time is the one that destroys
// data.
func NewAssembler(devices nvme.DeviceResolver, mounter *csimount.Mounter) (*export.Assembler, error) {
	return export.New(export.Config{
		Devices:    devices,
		Filesystem: filesystem{mounter: mounter},
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

// runner executes a command and reports its output and exit code, which is the
// shape atlas/blockdev already defines for this.
func runner(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
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
