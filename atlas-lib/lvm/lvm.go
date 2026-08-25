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
// complete it — confirmed live ("device not cleared, Aborting. Failed to wipe
// start of new LV" from a real lvcreate without this set). Respects ctx's own
// deadline rather than imposing one of its own; a caller that wants one bounds
// ctx itself.
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

// DeviceScope returns LVM's --devices argument pair, scoping a command to
// exactly the given devices (comma-joined, LVM's own syntax for the flag) and
// bypassing its default system-wide device scan entirely. See the package
// doc comment for why every command in this package is scoped this way.
//
// Returns nil for zero devices rather than an empty --devices value, which
// LVM would reject: a caller that no longer has a device path to scope to
// (a teardown addressing a VG by name after its backing device is already
// gone) runs unscoped by passing no devices, not zero-scoped.
func DeviceScope(devices ...string) []string {
	if len(devices) == 0 {
		return nil
	}
	return []string{"--devices", strings.Join(devices, ",")}
}

// Inspector answers content-based identity questions about devices' LVM
// state, and runs arbitrary LVM/dm commands scoped to a fixed device set, for
// callers assembling their own LVM stack on top of simplyblock volumes (VDO,
// a striped volume group across several members) who need the same
// device-scoping and content-based identity checks this package exists for.
type Inspector struct {
	run Runner
}

// NewInspector returns an Inspector that runs real LVM/dm-vdo commands.
func NewInspector() *Inspector {
	return &Inspector{run: runCommand}
}

// NewInspectorWithRunner is NewInspector with the command execution supplied
// by the caller — see Runner. A nil run means the real one NewInspector uses.
func NewInspectorWithRunner(run Runner) *Inspector {
	if run == nil {
		run = runCommand
	}
	return &Inspector{run: run}
}

// Run executes an LVM/dm-vdo command scoped to devices, inserting the
// --devices flag immediately after args[0] (the binary): pvcreate, vgcreate,
// lvcreate, vgchange, lvextend, vgremove, and every other LVM/dm-vdo
// invocation an assembled stack needs beyond the identity questions below.
func (i *Inspector) Run(ctx context.Context, devices []string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("lvm: Run requires a command name")
	}
	full := append([]string{args[0]}, DeviceScope(devices...)...)
	full = append(full, args[1:]...)
	return i.run(ctx, full...)
}

// VolumeGroup returns the name of the volume group devicePath's on-disk PV
// signature currently belongs to, or "" if devicePath carries no LVM
// signature at all — a genuinely blank device, or the probe itself failing,
// are both treated the same way by callers: nothing to resolve.
//
// Content-based (pvs on devicePath), not a name-based `vgs <name>` lookup:
// confirmed live that `vgs --devices devicePath vgname` can report success
// for a VG name that was never actually created on that device, on a host
// whose LVM devices file restricts default visibility to unrelated devices.
func (i *Inspector) VolumeGroup(ctx context.Context, devicePath string) (string, error) {
	out, err := i.Run(ctx, []string{devicePath}, "pvs", "--noheadings", "-o", "vg_name", devicePath)
	if err != nil {
		return "", nil //nolint:nilerr // probe failure reads as "nothing to resolve," same as a blank device
	}
	// Runner implementations typically merge stdout+stderr, and pvs can print
	// WARNING: lines ahead of the actual field value (e.g., duplicate-PV
	// warnings on a byte-level clone) — take the first non-empty,
	// non-warning line rather than trusting the whole trimmed blob, which
	// would otherwise pollute both an identity comparison and any log
	// message built from it.
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "WARNING:") {
			continue
		}
		return line, nil
	}
	return "", nil
}

// HasLogicalVolume reports whether vg, scoped to devices, already contains a
// logical volume named lvName. Distinguishes a fully assembled stack from one
// left orphaned by an interrupted create (pvcreate/vgcreate completed, the
// final lvcreate did not) — such a VG reports zero LVs, and `vgchange -ay`
// against it "succeeds" while producing no mountable device at all.
func (i *Inspector) HasLogicalVolume(ctx context.Context, devices []string, vg, lvName string) (bool, error) {
	out, err := i.Run(ctx, devices, "lvs", "--noheadings", "-o", "lv_name", vg)
	if err != nil {
		return false, nil //nolint:nilerr // unreadable VG reads as "nothing found," same as a genuinely empty one
	}
	for name := range strings.FieldsSeq(out) {
		if name == lvName {
			return true, nil
		}
	}
	return false, nil
}

// Rescan refreshes LVM's cached view of devices (pvscan --cache), scoped to
// exactly these devices so a volume's other redundant local device nodes are
// never registered into LVM's cache alongside the ones being inspected.
func (i *Inspector) Rescan(ctx context.Context, devices []string) error {
	_, err := i.Run(ctx, devices, "pvscan", "--cache")
	return err
}

// EscapeDMName escapes name the way device-mapper flattens a compound name
// (doubling every literal "-"), so a caller matching dmsetup's own output
// (e.g., "<vg>-<lv>") against a known VG or LV name compares correctly.
// Confirmed live: matching against an unescaped name found nothing in
// `dmsetup ls` output, leaving an orphaned stack stuck with nothing left to
// clean it up.
func EscapeDMName(name string) string {
	return strings.ReplaceAll(name, "-", "--")
}

// RemoveOrphanedDMNodes clears any live device-mapper nodes whose name starts
// with namePrefix (escaped internally via EscapeDMName), for when the backing
// device is gone and the higher-level removal (vgremove, etc.) can no longer
// read the metadata it needs to deactivate cleanly. Retries across a few
// passes so removing a dependent node unblocks what it was blocking, rather
// than hardcoding the dependency chain.
func (i *Inspector) RemoveOrphanedDMNodes(ctx context.Context, namePrefix string) error {
	out, err := i.run(ctx, "dmsetup", "ls")
	if err != nil {
		return fmt.Errorf("dmsetup ls: %w", err)
	}

	escaped := EscapeDMName(namePrefix)

	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "No devices found" {
			continue
		}
		name := strings.Fields(line)[0]
		if strings.HasPrefix(name, escaped+"-") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}

	var lastErr error
	for pass := 0; pass < 3 && len(names) > 0; pass++ {
		var remaining []string
		for _, name := range names {
			if _, err := i.run(ctx, "dmsetup", "remove", name); err != nil {
				remaining = append(remaining, name)
				lastErr = err
			}
		}
		names = remaining
	}
	if len(names) > 0 {
		return fmt.Errorf("failed to remove orphaned dm nodes %v: %w", names, lastErr)
	}
	return nil
}
