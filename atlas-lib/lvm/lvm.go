package lvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runner executes a single LVM/device-mapper command and returns its combined
// output, which is where these tools put the reason for a failure, as well as
// (for pvs/lvs) the fields callers scrape from stdout mixed with any
// WARNING: lines lvm2 writes to stderr.
//
// A field rather than a hardcoded exec.Command so this package's own tests
// can run the identity logic without lvm2 or a kernel present. Unexported:
// nothing outside this package injects one today, and NewManagerWithRunner
// stays usable from outside regardless, since Go allows passing a matching
// func literal to a named func-type parameter whether or not the type name
// itself is exported.
type runner func(ctx context.Context, args ...string) (string, error)

// runCommand is the default runner: it execs args[0] with the rest as
// arguments, and disables udev's device-node handshake (DM_DISABLE_UDEV)
// since a container running these tools typically has no udev daemon to
// complete it. Confirmed live: "device not cleared, Aborting. Failed to wipe
// start of new LV" from a real lvcreate without this set. Respects ctx's own
// deadline rather than imposing one of its own, so a caller that wants one
// bounds ctx itself.
func runCommand(ctx context.Context, args ...string) (string, error) {
	//nolint:gosec // fixed set of LVM/dm binaries, structured args
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "DM_DISABLE_UDEV=1")
	output, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return string(output), fmt.Errorf("timed out running %v", args)
	}
	if err != nil {
		return string(output), fmt.Errorf("%v: %w: %s", args, err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// PhysicalVolume identifies an LVM physical volume by the device path it was
// created on. A plain value type, not a handle: it carries no reference back
// to a Manager, so it stays valid to pass to any Manager instance.
type PhysicalVolume struct {
	DevicePath string
}

// VolumeGroup identifies an LVM volume group by name.
type VolumeGroup struct {
	Name string
}

// LogicalVolume identifies a logical volume within a VolumeGroup: the VDO
// pool a create/grow targets, the VDO logical volume itself, or a plain LV.
// Carrying its VolumeGroup rather than just a bare name is what makes a
// caller's VG/LV pairing a type the compiler checks instead of two strings
// that happen to travel together correctly today.
type LogicalVolume struct {
	VolumeGroup VolumeGroup
	Name        string
}

// devicePaths extracts the device path each of pvs identifies, in order, for
// the handful of calls that still need a []string to build an argument list
// or scope a command with deviceScope.
func devicePaths(pvs []PhysicalVolume) []string {
	paths := make([]string, len(pvs))
	for i, pv := range pvs {
		paths[i] = pv.DevicePath
	}
	return paths
}

// deviceScope returns LVM's --devices argument pair, scoping a command to
// exactly the given devices (comma-joined, LVM's own syntax for the flag) and
// bypassing its default system-wide device scan entirely. See the package doc
// comment for which commands need that and why.
//
// Returns nil for zero devices rather than an empty --devices value, which LVM
// would reject, so a command with no device to scope to simply runs unscoped.
func deviceScope(devices ...string) []string {
	if len(devices) == 0 {
		return nil
	}
	return []string{"--devices", strings.Join(devices, ",")}
}

// Manager assembles and inspects an LVM stack on top of simplyblock volumes:
// creating and removing physical volumes, volume groups, and logical volumes
// (including a VDO-backed one, see the vdo subpackage), activating and growing
// them, and answering content-based identity questions about them.
//
// Device scoping is this type's business, not its caller's. A method that
// names a device scopes itself to that device, while a method that addresses a
// volume group or logical volume by name runs unscoped, which is unambiguous
// for the reasons the package doc comment gives. No method asks a caller
// which devices LVM may look at, because no caller is in a position to answer.
//
// `Run` remains available as an escape hatch for a command this type does not
// wrap yet, but the named methods across this package's files are what a caller
// should reach for first: assembling a stack by hand-building `Run` argument
// lists is exactly the duplication this package exists to prevent.
type Manager struct {
	run runner
}

// NewManager returns a Manager that runs real LVM/dm-vdo commands.
func NewManager() *Manager {
	return &Manager{run: runCommand}
}

// NewManagerWithRunner is NewManager with the command execution supplied by
// the caller: a func(ctx, args...) (string, error), for a caller that wants
// to test the identity logic in this package without lvm2 or a kernel
// present. A nil run means the real one NewManager uses.
func NewManagerWithRunner(run runner) *Manager {
	if run == nil {
		run = runCommand
	}
	return &Manager{run: run}
}

// Run executes an LVM/dm-vdo command unscoped, the escape hatch for a command
// this package does not wrap yet. Prefer a named method when one exists.
//
// A command that has to be scoped to a device belongs in this package as a
// named method rather than here, because the scope follows from which device
// the command names, and that is knowledge this package holds and its callers
// do not. See the package doc comment.
func (m *Manager) Run(ctx context.Context, args ...string) (string, error) {
	return m.exec(ctx, nil, args...)
}

// exec runs an LVM/dm-vdo command scoped to devices, inserting the --devices
// flag immediately after args[0] (the binary) and omitting it entirely when
// devices is empty. Every method in this package goes through here, passing
// the devices its own operands imply.
func (m *Manager) exec(ctx context.Context, devices []string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("lvm: a command name is required")
	}
	full := append([]string{args[0]}, deviceScope(devices...)...)
	full = append(full, args[1:]...)
	return m.run(ctx, full...)
}

// firstRealLine returns the first non-empty, non-WARNING: line of out.
// A runner typically merges stdout+stderr, and pvs/lvs can print
// WARNING: lines ahead of the actual field value (e.g., duplicate-PV warnings
// on a byte-level clone). Trusting the whole trimmed blob would pollute both
// an identity comparison and any log message built from it.
func firstRealLine(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "WARNING:") {
			continue
		}
		return line
	}
	return ""
}
