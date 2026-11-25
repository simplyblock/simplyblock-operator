// Package devmapper manages single-segment dm-linear devices: the indirection
// between an NVMe-oF namespace and what a pod or a filesystem uses.
//
// A dm-linear device maps its whole range onto one underlying block device and
// writes nothing to it, so it can be put in front of a volume that already
// carries data. What it buys is a device whose identity does not change when
// the namespace behind it does: when a volume's namespace moves to another NVMe
// subsystem (consistency-group co-location), the new namespace shows up as a
// different block device, and Swap re-points the mapping at it -- suspend, load
// the new table, resume -- while the consumer keeps the device it opened. I/O
// issued during the suspend is queued by device-mapper, not failed.
package devmapper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Runner execs a command and returns its combined output. Tests substitute a
// recorder; production execs the host's dmsetup and blockdev.
type Runner func(ctx context.Context, args ...string) (string, error)

// ErrNotFound reports that the named mapping does not exist.
var ErrNotFound = errors.New("device-mapper mapping not found")

// Mapper creates, reads, re-points and removes dm-linear mappings.
type Mapper struct {
	run Runner
}

// New returns a Mapper over run; nil runs the host's commands.
func New(run Runner) *Mapper {
	if run == nil {
		run = runCommand
	}
	return &Mapper{run: run}
}

func runCommand(ctx context.Context, args ...string) (string, error) {
	//nolint:gosec // fixed set of binaries (dmsetup, blockdev), structured args
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	// No udev in the node plugin's container to complete the handshake.
	cmd.Env = append(os.Environ(), "DM_DISABLE_UDEV=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Path is the device node of mapping name.
func Path(name string) string { return "/dev/mapper/" + name }

// Target is what a mapping currently points at.
type Target struct {
	// Sectors is the mapped length in 512-byte sectors.
	Sectors uint64
	// Device is the underlying device as major:minor.
	Device string
}

// table renders the one-segment linear table for device.
func table(sectors uint64, device string) string {
	return fmt.Sprintf("0 %d linear %s 0", sectors, device)
}

// Sectors is device's size in 512-byte sectors.
func (m *Mapper) Sectors(ctx context.Context, device string) (uint64, error) {
	out, err := m.run(ctx, "blockdev", "--getsz", device)
	if err != nil {
		return 0, fmt.Errorf("size of %s: %w", device, err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size of %s: unexpected %q", device, strings.TrimSpace(out))
	}
	return n, nil
}

// Table reads mapping name's target, or ErrNotFound.
func (m *Mapper) Table(ctx context.Context, name string) (Target, error) {
	out, err := m.run(ctx, "dmsetup", "table", name)
	if err != nil {
		if strings.Contains(out, "No such device") || strings.Contains(err.Error(), "No such device") {
			return Target{}, ErrNotFound
		}
		return Target{}, fmt.Errorf("dmsetup table %s: %w", name, err)
	}
	return parseTable(name, out)
}

func parseTable(name, out string) (Target, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		return Target{}, fmt.Errorf("mapping %s has %d segments; a linear indirection has one", name, len(lines))
	}
	f := strings.Fields(lines[0])
	if len(f) != 5 || f[2] != "linear" || f[0] != "0" || f[4] != "0" {
		return Target{}, fmt.Errorf("mapping %s is not a whole-device linear mapping: %q", name, lines[0])
	}
	sectors, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return Target{}, fmt.Errorf("mapping %s: length %q: %w", name, f[1], err)
	}
	return Target{Sectors: sectors, Device: f[3]}, nil
}

// Create maps name onto device over sectors.
func (m *Mapper) Create(ctx context.Context, name, device string, sectors uint64) error {
	if _, err := m.run(ctx, "dmsetup", "create", name, "--table", table(sectors, device)); err != nil {
		return fmt.Errorf("dmsetup create %s: %w", name, err)
	}
	return nil
}

// Swap re-points name at device: suspend (in-flight I/O drains, new I/O is
// queued), load the new table, resume. A failed load resumes on the old table,
// so a swap never leaves the mapping suspended.
//
// The suspend does not freeze a filesystem on top, for the reason
// SuspendWithoutFreeze gives and one more: the heal swaps after total path
// loss, when the old table names a namespace that is gone. A freeze there has
// to write to that namespace. ext4's freeze then fails with EIO, and XFS's
// blocks indefinitely while it holds the superblock lock, which stalls every
// sync(2) on the node.
func (m *Mapper) Swap(ctx context.Context, name, device string, sectors uint64) error {
	if err := m.SuspendWithoutFreeze(ctx, name); err != nil {
		return err
	}
	if err := m.reload(ctx, name, table(sectors, device)); err != nil {
		if rerr := m.Resume(ctx, name); rerr != nil {
			return fmt.Errorf("dmsetup reload %s: %w; resume on the old table also failed: %v", name, err, rerr)
		}
		return fmt.Errorf("dmsetup reload %s: %w (resumed on the old table)", name, err)
	}
	return m.Resume(ctx, name)
}

// Suspend suspends mapping name: I/O in flight drains, new I/O is queued, and a
// filesystem mounted on it is frozen and flushed first, which is what dmsetup
// does by default (lockfs). The image underneath is then one a filesystem can
// mount after a crash, and stays that until Resume.
//
// Swap suspends through it, and so does a striped volume's snapshot, which
// holds the stripe's logical volume suspended so that every member is
// snapshotted at one point in its write stream (atlas-lib/freeze).
func (m *Mapper) Suspend(ctx context.Context, name string) error {
	if _, err := m.run(ctx, "dmsetup", "suspend", name); err != nil {
		return fmt.Errorf("dmsetup suspend %s: %w", name, err)
	}
	return nil
}

// Resume lets mapping name's queued and new I/O through again, and thaws a
// filesystem Suspend froze. Resuming a mapping that is not suspended is a
// no-op for device-mapper, which is what lets a caller resume when it is not
// sure a suspend took effect.
func (m *Mapper) Resume(ctx context.Context, name string) error {
	if _, err := m.run(ctx, "dmsetup", "resume", name); err != nil {
		return fmt.Errorf("dmsetup resume %s: %w", name, err)
	}
	return nil
}

// Remove deletes mapping name; a missing mapping is success.
func (m *Mapper) Remove(ctx context.Context, name string) error {
	out, err := m.run(ctx, "dmsetup", "remove", "--retry", name)
	if err != nil {
		if strings.Contains(out, "No such device") || strings.Contains(err.Error(), "No such device") {
			return nil
		}
		return fmt.Errorf("dmsetup remove %s: %w", name, err)
	}
	return nil
}

// Mapping is one device-mapper device as the startup scan sees it.
type Mapping struct {
	// Name is the mapping's name under /dev/mapper.
	Name string
	// Suspended reports that the mapping is holding its I/O.
	Suspended bool
}

// SuspendWithoutFreeze holds name's I/O without freezing a filesystem on top.
// In-flight requests complete on the current device before it returns and new
// ones queue, so no request is outstanding on a namespace that is about to
// leave its subsystem. --nolockfs skips the freeze Suspend does: moving a
// namespace changes a path, it is not a consistency point, and a freeze would
// flush dirty pages inside the window.
func (m *Mapper) SuspendWithoutFreeze(ctx context.Context, name string) error {
	if _, err := m.run(ctx, "dmsetup", "suspend", "--nolockfs", name); err != nil {
		return fmt.Errorf("dmsetup suspend %s: %w", name, err)
	}
	return nil
}

// ReloadResume points a suspended name at device and releases its I/O. A
// refused reload leaves the mapping suspended and returns the error: unlike
// Swap, the old table may name a namespace that has already gone, and the
// caller decides between retrying and resuming elsewhere.
func (m *Mapper) ReloadResume(ctx context.Context, name, device string, sectors uint64) error {
	if err := m.reload(ctx, name, table(sectors, device)); err != nil {
		return fmt.Errorf("dmsetup reload %s: %w (still suspended)", name, err)
	}
	return m.Resume(ctx, name)
}

// Suspended reports whether name is holding its I/O, or ErrNotFound.
func (m *Mapper) Suspended(ctx context.Context, name string) (bool, error) {
	out, err := m.run(ctx, "dmsetup", "info", "-c", "--noheadings", "-o", "name,suspended", name)
	if err != nil {
		if notFound(out, err) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("dmsetup info %s: %w", name, err)
	}
	for _, mp := range parseInfo(out) {
		if mp.Name == name {
			return mp.Suspended, nil
		}
	}
	return false, ErrNotFound
}

// List returns every mapping whose name starts with prefix.
func (m *Mapper) List(ctx context.Context, prefix string) ([]Mapping, error) {
	out, err := m.run(ctx, "dmsetup", "info", "-c", "--noheadings", "-o", "name,suspended")
	if err != nil {
		return nil, fmt.Errorf("dmsetup info: %w", err)
	}
	var found []Mapping
	for _, mp := range parseInfo(out) {
		if strings.HasPrefix(mp.Name, prefix) {
			found = append(found, mp)
		}
	}
	return found, nil
}

func parseInfo(out string) []Mapping {
	var found []Mapping
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, state, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || name == "" {
			continue
		}
		found = append(found, Mapping{Name: name, Suspended: state == "Suspended"})
	}
	return found
}

func notFound(out string, err error) bool {
	for _, s := range []string{out, err.Error()} {
		if strings.Contains(s, "No such device") || strings.Contains(s, "not found") {
			return true
		}
	}
	return false
}
