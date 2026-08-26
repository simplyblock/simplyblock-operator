package lvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes a single LVM/device-mapper command and returns its combined
// output, which is where these tools put the reason for a failure, as well as
// (for pvs/lvs) the fields callers scrape from stdout mixed with any
// WARNING: lines lvm2 writes to stderr.
//
// A field rather than a hardcoded exec.Command so callers can test the
// identity logic in this package without lvm2 or a kernel present.
type Runner func(ctx context.Context, args ...string) (string, error)

// runCommand is the default Runner: it execs args[0] with the rest as
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

// deviceScope returns LVM's --devices argument pair, scoping a command to
// exactly the given devices (comma-joined, LVM's own syntax for the flag) and
// bypassing its default system-wide device scan entirely. See the package
// doc comment for why every command in this package is scoped this way.
//
// Returns nil for zero devices rather than an empty --devices value, which
// LVM would reject: a caller that no longer has a device path to scope to
// (a teardown addressing a VG by name after its backing device is already
// gone) runs unscoped by passing no devices, not zero-scoped.
//
// Unexported: it only builds one argument fragment for Run, and every named
// method in this package goes through Run rather than assembling --devices
// itself.
func deviceScope(devices ...string) []string {
	if len(devices) == 0 {
		return nil
	}
	return []string{"--devices", strings.Join(devices, ",")}
}

// Manager assembles and inspects an LVM stack on top of simplyblock volumes:
// creating and removing physical volumes, volume groups, and logical volumes
// (including a VDO-backed one, see vdo.go), activating and growing them, and
// answering content-based identity questions about them, every operation
// scoped to a fixed device set. `Run` remains available as an escape hatch
// for a command this type doesn't wrap yet, but the named methods across this
// package's files are what a caller should reach for first: assembling a
// stack by hand-building `Run` argument lists is exactly the duplication this
// package exists to prevent.
type Manager struct {
	run Runner
}

// NewManager returns a Manager that runs real LVM/dm-vdo commands.
func NewManager() *Manager {
	return &Manager{run: runCommand}
}

// NewManagerWithRunner is NewManager with the command execution supplied by
// the caller (see Runner). A nil run means the real one NewManager uses.
func NewManagerWithRunner(run Runner) *Manager {
	if run == nil {
		run = runCommand
	}
	return &Manager{run: run}
}

// Run executes an LVM/dm-vdo command scoped to devices, inserting the
// --devices flag immediately after args[0] (the binary). Prefer a named
// method when one exists: this is for a command that doesn't have one yet.
func (m *Manager) Run(ctx context.Context, devices []string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("lvm: Run requires a command name")
	}
	full := append([]string{args[0]}, deviceScope(devices...)...)
	full = append(full, args[1:]...)
	return m.run(ctx, full...)
}

// firstRealLine returns the first non-empty, non-WARNING: line of out.
// Runner implementations typically merge stdout+stderr, and pvs/lvs can print
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
