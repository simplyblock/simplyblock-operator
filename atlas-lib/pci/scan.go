// What the PCI bus has on it, and who owns each device.
//
// Everything here is read from sysfs, and the one non-obvious reading is the
// class. A PCI class is three bytes — class, subclass, and programming
// interface — and NVMe is 0x010802: mass storage, NVM, NVMe. Matching the first
// two bytes and not the third is what makes the reading survive a controller
// that reports a different programming interface, and matching all three would
// be a filter nobody meant.

package pci

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/simplyblock/atlas/internal/sysfs"
)

const (
	// DefaultSysfsRoot is where the kernel's sysfs is mounted on a host that
	// did not move it.
	DefaultSysfsRoot = "/sys"

	// DefaultProcRoot is the conventional procfs mount point.
	DefaultProcRoot = "/proc"

	// DefaultDevRoot is the conventional device-node directory.
	DefaultDevRoot = "/dev"
)

const (
	// DriverNVMe is the kernel driver that presents a controller's namespaces
	// as block devices.
	DriverNVMe = "nvme"

	// DriverUIOGeneric and DriverVFIO are the userspace-IO drivers SPDK binds a
	// controller to when it takes it. A device on either has no block device.
	DriverUIOGeneric = "uio_pci_generic"
	DriverVFIO       = "vfio-pci"
)

// NUMANodeUnknown is the memory node of a device whose bus does not say, which
// is what a virtual machine usually reports.
const NUMANodeUnknown = -1

// classNVMePrefix is the class and subclass of an NVMe controller: mass storage
// (0x01), NVM (0x08). The third byte is the programming interface, which is
// 0x02 for NVMe and is deliberately not matched.
const classNVMePrefix = "0x0108"

// Config names the trees a scan is taken from.
type Config struct {
	// SysfsRoot is the sysfs mount point, defaulting to DefaultSysfsRoot.
	SysfsRoot string

	// ProcRoot is the procfs mount point, defaulting to DefaultProcRoot. Only
	// HeldBy reads it.
	ProcRoot string

	// DevRoot is the device-node directory, defaulting to DefaultDevRoot. It
	// decides what a UIO device's path says.
	DevRoot string
}

func (c Config) sysfs() string {
	if c.SysfsRoot == "" {
		return DefaultSysfsRoot
	}
	return c.SysfsRoot
}

func (c Config) proc() string {
	if c.ProcRoot == "" {
		return DefaultProcRoot
	}
	return c.ProcRoot
}

func (c Config) dev() string {
	if c.DevRoot == "" {
		return DefaultDevRoot
	}
	return c.DevRoot
}

// Device is one PCI device as sysfs presents it.
type Device struct {
	// Address is the domain:bus:device.function the kernel names it by, which
	// is the form a deployment config names an NVMe device by.
	Address string

	// Class is the raw three-byte class code, such as 0x010802.
	Class string

	// Vendor and Product are the raw identifiers, such as 0x1b36 and 0x0010.
	// They are the identifiers rather than names because sysfs has no names:
	// resolving them needs a PCI ID database this library does not ship.
	Vendor, Product string

	// Driver is the driver bound to the device, and is empty when none is.
	// Empty is a real state and not a failure: a device rebound away from one
	// driver and not to another sits there with no driver at all.
	Driver string

	// NUMANode is the memory node the device hangs off, or NUMANodeUnknown.
	NUMANode int

	// UIODevices is the character devices a userspace-IO driver exposes for
	// this device, by path. It is empty for a device on a kernel driver.
	//
	// The mapping is read from the device's own uio directory rather than by
	// numbering: the uio index does not follow the PCI order, and on the fleet
	// this was written against uio0 was the controller in slot 05.0 while uio2
	// was the one in slot 02.0.
	UIODevices []string
}

// IsNVMe reports whether the device is an NVMe controller.
func (d Device) IsNVMe() bool { return strings.HasPrefix(d.Class, classNVMePrefix) }

// BoundToUserspace reports whether a userspace-IO driver owns the device, which
// on this product's hosts means SPDK has taken it or something left it taken.
func (d Device) BoundToUserspace() bool {
	return d.Driver == DriverUIOGeneric || d.Driver == DriverVFIO
}

// HasKernelDriver reports whether any driver owns it at all.
func (d Device) HasKernelDriver() bool { return d.Driver != "" }

// String renders the device for a log line or an event.
func (d Device) String() string {
	driver := d.Driver
	if driver == "" {
		driver = "no driver"
	}
	return fmt.Sprintf("%s class %s vendor %s:%s on %s", d.Address, d.Class, d.Vendor, d.Product, driver)
}

// address matches the domain:bus:device.function form the kernel names its PCI
// directories with.
var address = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`)

// Scan reports every PCI device on the machine, ordered by address so that two
// scans of one host are comparable.
//
// A host with no PCI bus at all, which is what a captured tree without one
// looks like, has no devices and is not an error.
func Scan(cfg Config) ([]Device, error) {
	base := filepath.Join(cfg.sysfs(), "bus", "pci", "devices")
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("pci: list %s: %w", base, err)
	}

	devices := make([]Device, 0, len(entries))
	for _, entry := range entries {
		if !address.MatchString(entry.Name()) {
			continue
		}
		devices = append(devices, readDevice(cfg, filepath.Join(base, entry.Name()), entry.Name()))
	}

	slices.SortFunc(devices, func(a, b Device) int { return cmp.Compare(a.Address, b.Address) })
	return devices, nil
}

// NVMeControllers is the NVMe devices of a scan, which is the question this
// package is usually asked.
func NVMeControllers(devices []Device) []Device {
	var nvme []Device
	for _, device := range devices {
		if device.IsNVMe() {
			nvme = append(nvme, device)
		}
	}
	return nvme
}

// readDevice reads one device's attributes.
//
// Nothing here fails. Every attribute is missing on some device somewhere, and
// a device reported with what was readable is worth more than a scan that
// failed over a field it did not need.
func readDevice(cfg Config, dir, addr string) Device {
	device := Device{
		Address:  addr,
		Class:    sysfs.String(dir, "class"),
		Vendor:   sysfs.String(dir, "vendor"),
		Product:  sysfs.String(dir, "device"),
		NUMANode: sysfs.Int(NUMANodeUnknown, dir, "numa_node"),
	}
	if driver, err := os.Readlink(filepath.Join(dir, "driver")); err == nil {
		device.Driver = filepath.Base(driver)
	}
	device.UIODevices = uioDevicesOf(cfg, dir)
	return device
}

// uioDevicesOf lists the character devices a userspace-IO driver exposes for
// one PCI device.
func uioDevicesOf(cfg Config, dir string) []string {
	names, err := sysfs.List(filepath.Join(dir, "uio"))
	if err != nil || len(names) == 0 {
		return nil
	}
	slices.Sort(names)

	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, filepath.Join(cfg.dev(), name))
	}
	return paths
}

// ParseAddress checks that a string is a PCI address the kernel would name a
// directory with, so that a caller handed one from a config can refuse it
// before writing it anywhere.
func ParseAddress(addr string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(addr))
	if !address.MatchString(normalized) {
		return "", fmt.Errorf(
			"pci: %q is not a PCI address; the form is domain:bus:device.function, as in 0000:5e:00.0",
			addr)
	}
	return normalized, nil
}
