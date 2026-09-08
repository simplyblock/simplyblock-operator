// Giving a device back to the kernel, or taking it away.
//
// This is the only file in the package that writes anything, and it is
// separated for that reason. Everything else answers questions; these two
// functions change which driver is driving a piece of hardware, and getting it
// wrong while SPDK is running takes a storage node's disks out from under it
// mid-I/O.
//
// So a rebind is guarded rather than trusted. [BindTo] refuses a device
// something is holding open, and the caller has to have looked: the guard reads
// the holders itself rather than taking a caller's word, because the caller
// that is wrong about this is exactly the one that would pass the wrong answer.
//
// Nothing in a discovery run calls either of these. A run inspects what is
// there and writes what it found; reclaiming a controller a previous deployment
// left bound is a decision somebody makes about a specific machine, after
// reading what the run reported.

package pci

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrDeviceHeld is returned by BindTo when a process is driving the device.
//
// It is a sentinel because it is the one failure a caller is expected to
// handle: everything else here is a broken sysfs or a missing driver, and this
// is that somebody is using it, and it has to be stopped first.
var ErrDeviceHeld = errors.New("pci: a process is holding the device")

// BindTo gives a device to a driver, taking it from whatever has it now.
//
// It refuses when anything holds the device's userspace character devices open,
// which is the check that separates a controller SPDK is running on from one a
// previous deployment left behind. The refusal names the holders, because the
// next question is always which process.
//
// An empty driver hands the device back to whatever the kernel would bind it
// to, by asking the bus to probe it again. That is the right way to return a
// controller to the NVMe driver: binding it by name would work today and would
// be a guess about a machine whose kernel may prefer something else.
func BindTo(cfg Config, addr, driver string) error {
	address, err := ParseAddress(addr)
	if err != nil {
		return err
	}

	devices, err := Scan(cfg)
	if err != nil {
		return err
	}
	device, found := deviceAt(devices, address)
	if !found {
		return fmt.Errorf("pci: no device at %s", address)
	}

	holders, err := HeldBy(cfg, device)
	if err != nil {
		// Not knowing is not permission. A caller that cannot see the host's
		// processes gets a refusal rather than a rebind on an unchecked
		// assumption.
		return fmt.Errorf("pci: %s could not be checked for holders, so it was left alone: %w",
			address, err)
	}
	if len(holders) > 0 {
		return fmt.Errorf("%w: %s is driven by %s", ErrDeviceHeld, address, describeHolders(holders))
	}

	if device.Driver != "" {
		if err := Unbind(cfg, address); err != nil {
			return err
		}
	}

	// A driver_override left behind would send the probe straight back to the
	// driver the device is being taken from.
	if err := writeAttr(cfg, filepath.Join("bus", "pci", "devices", address, "driver_override"),
		overrideFor(driver)); err != nil {
		return err
	}

	if driver == "" {
		return writeAttr(cfg, filepath.Join("bus", "pci", "drivers_probe"), address)
	}
	return writeAttr(cfg, filepath.Join("bus", "pci", "drivers", driver, "bind"), address)
}

// Unbind takes a device away from the driver that has it, leaving it with none.
//
// It does not check for holders. Unbinding is what a caller does deliberately
// to a device it has already decided about, and the check belongs where the
// decision is: BindTo calls this after refusing a held device.
func Unbind(cfg Config, addr string) error {
	address, err := ParseAddress(addr)
	if err != nil {
		return err
	}

	current, err := os.Readlink(filepath.Join(cfg.sysfs(), "bus", "pci", "devices", address, "driver"))
	if err != nil {
		// No driver to unbind from is the state unbinding is for reaching, so
		// arriving there early is success.
		return nil
	}
	return writeAttr(cfg,
		filepath.Join("bus", "pci", "drivers", filepath.Base(current), "unbind"), address)
}

// deviceAt finds one device of a scan by address.
func deviceAt(devices []Device, address string) (Device, bool) {
	for _, device := range devices {
		if device.Address == address {
			return device, true
		}
	}
	return Device{}, false
}

// describeHolders renders the holders for a refusal.
func describeHolders(holders []Holder) string {
	parts := make([]string, 0, len(holders))
	for _, holder := range holders {
		parts = append(parts, holder.String())
	}
	return strings.Join(parts, ", ")
}

// overrideFor is what goes into driver_override: the driver to force, or a
// newline to clear it.
//
// Clearing is written as a newline rather than an empty string because the
// kernel reads the attribute as a line, and an empty write is not a line.
func overrideFor(driver string) string {
	if driver == "" {
		return "\n"
	}
	return driver
}

// writeAttr writes one sysfs attribute, which is how every change to a binding
// is made.
func writeAttr(cfg Config, relative, value string) error {
	path := filepath.Join(cfg.sysfs(), relative)
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("pci: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	if _, err := file.WriteString(value); err != nil {
		return fmt.Errorf("pci: write %q to %s: %w", strings.TrimSpace(value), path, err)
	}
	return nil
}
