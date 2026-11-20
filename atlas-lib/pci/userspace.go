// Whether anything is actually driving a device a userspace driver owns.
//
// Bound and in use are different states, and the difference decides whether a
// controller can be taken back. A controller something is running on is bound
// and held; one a previous deployment left behind is bound and idle. Handing
// the first back to the kernel takes its disks out from under whatever is
// driving them, and handing the second back costs nothing.
//
// What holds it is not assumed to be this product. A userspace binding is also
// how a hypervisor passes a disk through to a guest and how a DPDK application
// takes a device, and a machine that is doing either looks from sysfs exactly
// like one holding leftovers. That is the case this file exists to tell apart,
// and it is why the answer is about whether anything holds the device rather
// than about whether the holder is recognized.
//
// sysfs will not answer it. The uio driver exports name, version, and event,
// and none of them changes while a process holds the character device: this was
// measured rather than assumed, by opening /dev/uio0 on a live worker and
// diffing what sysfs exported across the open. There was no difference.
//
// So the only evidence is a process holding the device open, which means
// walking every process's descriptors. That needs the host's PID namespace: a
// caller in a pod without it sees its own processes and concludes, wrongly,
// that nothing holds anything.

package pci

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Holder is one process holding a device open.
type Holder struct {
	// PID is the process, in the PID namespace the procfs being read belongs
	// to.
	PID int

	// Command is the process's name, which is what makes a holder identifiable
	// in a message: "spdk_tgt holds it" is actionable where a bare number is
	// not.
	Command string

	// Device is the character device it holds.
	Device string
}

// String renders a holder for a message.
func (h Holder) String() string {
	return fmt.Sprintf("pid %d (%s) holds %s", h.PID, h.Command, h.Device)
}

// HeldBy reports which processes hold this device's character devices open.
//
// An empty result from a procfs that is not the host's is not evidence of
// anything, which is why this returns the holders rather than a boolean: a
// caller that cannot see the host's processes should not be handed a
// confident answer that nothing holds it.
func HeldBy(cfg Config, device Device) ([]Holder, error) {
	if len(device.UIODevices) == 0 {
		return nil, nil
	}
	return holdersOf(cfg, device.UIODevices)
}

// CheckHolders fills in InUse for every device given, and returns what it could
// not determine alongside the devices it could.
//
// The failures are returned as well as left on the device, because the two
// answers are not the same. A device nothing holds and a device that could not
// be checked are a set InUse and a nil one, and only the first is safe to
// reclaim: a caller reading the field gets the distinction whether or not it
// reads the error. The devices come back either way, since a machine whose
// process table could not be read still has controllers worth reporting.
func CheckHolders(cfg Config, devices []Device) ([]Device, error) {
	out := make([]Device, 0, len(devices))
	var errs []error

	for _, device := range devices {
		if !device.BoundToUserspace() {
			// The kernel is driving it, so its namespaces are block devices and
			// nothing about the process table changes that.
			out = append(out, device)
			continue
		}

		holders, err := HeldBy(cfg, device)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"pci: %s could not be checked for holders, so whether it is free is unknown: %w",
				device.Address, err))
			out = append(out, device)
			continue
		}
		held := len(holders) > 0
		device.InUse = &held
		out = append(out, device)
	}
	return out, errors.Join(errs...)
}

// holdersOf walks the process table for anything holding one of the paths.
func holdersOf(cfg Config, paths []string) ([]Holder, error) {
	proc := cfg.proc()
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil, fmt.Errorf("pci: read %s: %w", proc, err)
	}

	wanted := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		wanted[path] = struct{}{}
	}

	var holders []Holder
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		fds, err := os.ReadDir(filepath.Join(proc, entry.Name(), "fd"))
		if err != nil {
			// A process that exited between the listing and this read, or one
			// this process may not look at. Neither is a reason to abandon the
			// walk, and both are why the answer is best-effort.
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(proc, entry.Name(), "fd", fd.Name()))
			if err != nil {
				continue
			}
			if _, held := wanted[target]; !held {
				continue
			}
			holders = append(holders, Holder{
				PID:     pid,
				Command: commandOf(proc, entry.Name()),
				Device:  target,
			})
		}
	}

	slices.SortFunc(holders, func(a, b Holder) int {
		if a.PID != b.PID {
			return a.PID - b.PID
		}
		return strings.Compare(a.Device, b.Device)
	})
	return holders, nil
}

// commandOf reads a process's name, which is what a message quotes.
func commandOf(proc, pid string) string {
	raw, err := os.ReadFile(filepath.Join(proc, pid, "comm"))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(raw))
}
