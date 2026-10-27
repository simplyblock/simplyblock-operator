// The LBA formats of every NVMe controller the kernel drives.
//
// Which block sizes a disk can be formatted with is a property of the
// controller, and a deployment chooses one before it formats anything: 4 KiB
// blocks on a disk that also offers 512 bytes, or a format with metadata for
// end-to-end protection. The answer comes from an Identify Namespace admin
// command, which the `nvme` package decodes. This file finds the controllers to
// send it to and the namespaces to ask about.

package inventory

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/internal/sysfs"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/pci"
)

// FormatReader reads the LBA formats of one namespace through the controller
// character device at controllerDevicePath. nvme.ReadNamespaceFormats is the
// real one, and nsid is nvme.NSIDAll for the controller's common formats.
type FormatReader func(controllerDevicePath string, nsid nvme.NamespaceID) (nvme.NamespaceFormats, error)

// NVMeControllerFormats is the LBA formats one kernel-driven NVMe controller
// offers.
type NVMeControllerFormats struct {
	// Address is the controller's PCI address, the key Inventory.NVMeControllers
	// is read by.
	Address string

	// Controller is the kernel's name for the controller, such as nvme0, and
	// DevicePath the character device the formats were read through.
	Controller nvme.ControllerID
	DevicePath string

	// Namespaces is the formats of each attached namespace, ascending by NSID.
	// A controller with no namespace attached has one entry for nvme.NSIDAll
	// instead: the formats a namespace created on it could take.
	Namespaces []nvme.NamespaceFormats
}

// nvmeControllerName matches a controller directory such as `nvme0` under a PCI
// device's `nvme` directory.
var nvmeControllerName = regexp.MustCompile("^nvme[0-9]+$")

// formats is the reader to use, defaulted.
func (c Config) formats() FormatReader {
	if c.NVMeFormats != nil {
		return c.NVMeFormats
	}
	return nvme.ReadNamespaceFormats
}

// ReadNVMeFormats reads the LBA formats of every controller in controllers
// that the kernel's NVMe driver owns.
//
// A controller on a userspace driver, or on none, is skipped: the kernel has no
// character device for it, and asking would mean taking it from whatever holds
// it. Reclaiming it first (Config.ReclaimControllers) is what brings it here.
//
// A controller whose formats could not be read is left out and its failure
// joined into the error, so the other controllers are still reported.
func ReadNVMeFormats(cfg Config, controllers []pci.Device) ([]NVMeControllerFormats, error) {
	read := cfg.formats()
	var found []NVMeControllerFormats
	var failed []error
	for _, controller := range controllers {
		if controller.Driver != pci.DriverNVMe {
			continue
		}
		formats, err := readControllerFormats(cfg, controller.Address, read)
		if err != nil {
			failed = append(failed, fmt.Errorf("controller %s: %w", controller.Address, err))
		}
		found = append(found, formats...)
	}
	return found, errors.Join(failed...)
}

// readControllerFormats reads the formats of every controller the kernel
// created for the PCI device at address, which is one for every NVMe device
// this was written against.
func readControllerFormats(cfg Config, address string, read FormatReader) ([]NVMeControllerFormats, error) {
	dir := cfg.sysfsPath("bus", "pci", "devices", address, "nvme")
	names, err := sysfs.List(dir)
	if err != nil {
		return nil, err
	}

	var found []NVMeControllerFormats
	var failed []error
	for _, name := range names {
		if !nvmeControllerName.MatchString(name) {
			continue
		}
		controller := NVMeControllerFormats{
			Address:    address,
			Controller: nvme.ControllerID(name),
			DevicePath: filepath.Join(cfg.dev(), name),
		}
		if err := controller.read(filepath.Join(dir, name), read); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", name, err))
			continue
		}
		found = append(found, controller)
	}
	return found, errors.Join(failed...)
}

// read fills Namespaces from the controller whose sysfs directory is dir.
//
// Only a live controller is asked. One that is resetting or reconnecting holds
// an admin command until it recovers, and a collection waiting on it would
// report its timeout rather than the state that caused it.
func (c *NVMeControllerFormats) read(dir string, read FormatReader) error {
	if state := sysfs.String(dir, "state"); state != "live" {
		return fmt.Errorf("state %q: %w", state, errs.ErrNotConnected)
	}

	nsids, err := nvme.ControllerNamespaceIDs(dir)
	if err != nil {
		return err
	}
	if len(nsids) == 0 {
		nsids = []nvme.NamespaceID{nvme.NSIDAll}
	}
	for _, nsid := range nsids {
		formats, err := read(c.DevicePath, nsid)
		if err != nil {
			return err
		}
		c.Namespaces = append(c.Namespaces, formats)
	}
	return nil
}
