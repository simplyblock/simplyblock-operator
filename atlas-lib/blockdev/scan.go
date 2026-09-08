// Which block devices a host has, and what each one is.
//
// The rest of this package resolves a path it was handed. This file enumerates,
// which is a different problem: a scan has to decide what it is even allowed to
// look at. /sys/class/block holds the unclaimed NVMe SSD a discovery run is
// looking for and, in the same directory and indistinguishable by name alone,
// the loop device backing a container image, the device-mapper node of the root
// volume group, and the partitions of the boot disk.
//
// So the scan classifies rather than filters. It reports every entry with what
// it is — a disk or a partition, on which bus, in which slot — and leaves the
// decision to candidate.go, because a device that is excluded silently is a
// device nobody can be told about. Filtering by path pattern, which is what a
// caller without this had to do, excluded /dev/loop2 and then found the same
// volume again at /dev/disk/by-diskseq/15.
//
// Everything here is read from sysfs, including the sizes and the block sizes
// that ResolveDevice takes ioctls for. That is not an optimization: a scan that
// opened every device to size it would touch devices it has no business
// touching, and it makes the whole reading exercisable against a captured tree.

package blockdev

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/simplyblock/atlas/internal/sysfs"
)

const (
	// DefaultSysfsRoot is where the kernel's sysfs is mounted on a host that
	// did not move it.
	DefaultSysfsRoot = "/sys"

	// DefaultDevRoot is the conventional device-node directory.
	DefaultDevRoot = "/dev"

	// DefaultProcRoot is the conventional procfs mount point.
	DefaultProcRoot = "/proc"
)

// NUMANodeUnknown is the NUMA node of a device on no bus, and of one whose bus
// does not say. It matches the kernel's own -1 sentinel.
const NUMANodeUnknown = -1

// sectorSize is the unit sysfs counts a device's size in, whatever the device's
// own logical block size is. Reading it as the logical block size is the one
// arithmetic error this file can make, and it makes a 4Kn device look eight
// times larger than it is.
const sectorSize = 512

// ScanConfig names the trees a scan is taken from.
//
// A container inspecting its host mounts that host's /sys, /dev, and /proc
// somewhere of its own choosing, and the three roots are separate fields
// because nothing requires them to sit beside each other.
type ScanConfig struct {
	// SysfsRoot is the sysfs mount point, defaulting to DefaultSysfsRoot.
	SysfsRoot string

	// DevRoot is the device-node directory the scan builds paths in,
	// defaulting to DefaultDevRoot. Nothing is opened during a scan, so this
	// only decides what Device.Path says.
	DevRoot string

	// ProcRoot is the procfs mount point, defaulting to DefaultProcRoot. The
	// scan does not read it; usage.go does.
	ProcRoot string

	// MountinfoPath is the mount table to read, defaulting to this process's
	// own at <ProcRoot>/self/mountinfo.
	//
	// It is a field because the default is wrong in a container, and wrong in
	// the direction that loses data. A process in a pod has its own mount
	// namespace, so its own mountinfo does not list the host's mounts at all: a
	// scan running there reads an empty table, concludes that nothing is
	// mounted, and reports the disk carrying the host's root filesystem as
	// free. The host's table is PID 1's, which such a caller points this at.
	MountinfoPath string
}

func (c ScanConfig) sysfs() string {
	if c.SysfsRoot == "" {
		return DefaultSysfsRoot
	}
	return c.SysfsRoot
}

func (c ScanConfig) dev() string {
	if c.DevRoot == "" {
		return DefaultDevRoot
	}
	return c.DevRoot
}

func (c ScanConfig) proc() string {
	if c.ProcRoot == "" {
		return DefaultProcRoot
	}
	return c.ProcRoot
}

// mountinfo resolves which mount table to read.
func (c ScanConfig) mountinfo() string {
	if c.MountinfoPath == "" {
		return filepath.Join(c.proc(), "self", "mountinfo")
	}
	return c.MountinfoPath
}

// Kind is what a block device is, which decides whether handing it to a storage
// cluster is even a question.
type Kind string

const (
	// KindDisk is a whole disk: the only kind a cluster is built out of.
	KindDisk Kind = "Disk"

	// KindPartition is a slice of one. A partition is not backend storage, and
	// a disk that has any is a disk somebody has already divided up.
	KindPartition Kind = "Partition"

	// KindDeviceMapper is a device-mapper node: an LVM logical volume, a
	// multipath map, a crypt target.
	KindDeviceMapper Kind = "DeviceMapper"

	// KindMDRaid is a software-RAID array.
	KindMDRaid Kind = "MDRaid"

	// KindLoop is a file presented as a block device, which is what backs a
	// container image and a test image and never backend storage.
	KindLoop Kind = "Loop"

	// KindMemory is a RAM disk or a zram device.
	KindMemory Kind = "Memory"

	// KindNetwork is a block device backed by something on the network: an
	// nbd, a Ceph RBD, a DRBD replica.
	//
	// It earns a kind of its own because a stock Rocky worker carries sixteen
	// unconfigured nbd devices, and calling those disks put sixteen entries
	// claiming to be local storage into the report of every worker in a fleet.
	KindNetwork Kind = "Network"

	// KindOther is a device none of the above recognized. It is not a disk, and
	// it is reported rather than dropped so that a caller can say what it saw.
	KindOther Kind = "Other"
)

// Transport is the bus a device sits on.
//
// It is here because a path pattern cannot answer it, and because the two
// classes of backend storage a cluster is built out of are distinguished by
// exactly this: an NVMe deployment names devices by PCI address, and a logical
// block-device deployment names them by path.
type Transport string

const (
	// TransportNVMe is an NVMe namespace behind a PCIe controller: a disk in a
	// slot in this machine.
	TransportNVMe Transport = "NVMe"

	// TransportNVMeFabric is an NVMe namespace reached over a fabric, which on
	// this product's hosts is a simplyblock volume attached to the node.
	//
	// It is a separate transport from TransportNVMe because the two are
	// indistinguishable by name — both are nvmeXnY in class/block — and
	// confusing them is the worst mistake a discovery run can make: a fabric
	// namespace looks exactly like an unclaimed local disk, and handing one to
	// a cluster as backend storage would give a volume's own bytes away as free
	// space. candidate.go rejects it for that reason and no other.
	TransportNVMeFabric Transport = "NVMeFabric"

	// TransportSATA is a disk behind an ATA port.
	TransportSATA Transport = "SATA"

	// TransportSAS is a disk behind a SAS expander or an HBA.
	TransportSAS Transport = "SAS"

	// TransportSCSI is a SCSI disk whose bus the tree does not narrow further.
	TransportSCSI Transport = "SCSI"

	// TransportVirtio is a paravirtualized disk, which is what a virtual worker
	// has.
	TransportVirtio Transport = "Virtio"

	// TransportUSB is a disk on a USB bus.
	TransportUSB Transport = "USB"

	// TransportMMC is an SD or eMMC card.
	TransportMMC Transport = "MMC"

	// TransportUnknown is a device on no bus the tree names, which every
	// virtual device is.
	TransportUnknown Transport = ""
)

// Disk is one entry of class/block: the Device the rest of this package works
// with, plus what the scan concluded about it.
//
// It embeds Device rather than restating its fields, so that a candidate can be
// handed straight to Prober.Read. Like Device it is an immutable snapshot: a
// stale scan is retaken rather than refreshed.
type Disk struct {
	Device

	// Kind is what the device is.
	Kind Kind

	// Transport is the bus it sits on.
	Transport Transport

	// Vendor, Model, and Serial are the identity strings the device's bus
	// exports, with the padding sysfs writes them with trimmed off. NVMe
	// exports a model and a serial and no vendor; SCSI exports a vendor and a
	// model and puts the serial somewhere this does not read.
	Vendor, Model, Serial string

	// Rotational reports whether the kernel considers the device a spinning
	// disk.
	Rotational bool

	// Removable reports whether the device can be taken out of the machine.
	Removable bool

	// Virtual reports whether the device is backed by no hardware.
	Virtual bool

	// PCIAddress is the slot the device sits in, in the
	// domain:bus:device.function form a deployment config names an NVMe device
	// by. It is empty for a device on no PCI bus.
	PCIAddress string

	// NUMANode is the memory node the device's bus is attached to, or
	// NUMANodeUnknown.
	NUMANode int

	// Partitions is the kernel names of the partitions on this device,
	// ascending. A disk with any is a disk something has already divided up,
	// whether or not those partitions carry anything.
	Partitions []string

	// Holders is the kernel names of the devices stacked directly on top of
	// this one: the device-mapper node of a volume group that took it, the
	// software-RAID array it is a member of. A device with a holder is a device
	// in use, and the holder is the readable reason.
	Holders []string
}

// Scan reports every block device the host presents, ordered by kernel name so
// that two scans of one host are comparable.
//
// A tree with no class/block has no block devices, which is not an error: it is
// what a captured tree that did not include them looks like.
func Scan(cfg ScanConfig) ([]Disk, error) {
	base := filepath.Join(cfg.sysfs(), "class", "block")
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("blockdev: list %s: %w", base, err)
	}

	disks := make([]Disk, 0, len(entries))
	for _, entry := range entries {
		disk, err := scanOne(cfg, filepath.Join(base, entry.Name()), entry.Name())
		if err != nil {
			return nil, err
		}
		disks = append(disks, disk)
	}

	slices.SortFunc(disks, func(a, b Disk) int { return cmp.Compare(a.Name, b.Name) })
	return disks, nil
}

// scanOne reads one entry of class/block.
//
// The device numbers are the one attribute whose absence is a failure. Every
// other attribute is missing on some device somewhere — a size of zero on an
// unbacked loop device, no queue directory on a device with no request queue,
// no identity on a virtual one — and a device reported with what was readable
// is worth more than a scan that failed over the fields that were not. The
// numbers are different: they are the identity a mount is matched against, and
// a device without them cannot be told apart from another.
func scanOne(cfg ScanConfig, dir, name string) (Disk, error) {
	major, minor, err := readDevNumbers(dir)
	if err != nil {
		return Disk{}, err
	}

	disk := Disk{
		Device: Device{
			Path:              filepath.Join(cfg.dev(), name),
			Name:              name,
			Major:             major,
			Minor:             minor,
			LogicalBlockSize:  sysfs.Uint32(dir, "queue", "logical_block_size"),
			PhysicalBlockSize: sysfs.Uint32(dir, "queue", "physical_block_size"),
			SizeBytes:         sysfs.Uint64(dir, "size") * sectorSize,
			ReadOnly:          sysfs.Bool(dir, "ro"),
		},
		Removable:  sysfs.Bool(dir, "removable"),
		Rotational: sysfs.Bool(dir, "queue", "rotational"),
		NUMANode:   NUMANodeUnknown,
		Kind:       classify(dir, name),
		Partitions: partitionsOf(dir, name),
		Holders:    childNames(filepath.Join(dir, "holders")),
	}

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// The class entry is a symlink by construction, so failing to resolve
		// it means the device went away between the listing and this read.
		// What was read stays reported: the alternative is losing the rest of
		// the host over one disk that was removed.
		return disk, nil
	}
	disk.Virtual = sysfs.IsVirtual(resolved)
	disk.PCIAddress = sysfs.PCIAddressOf(resolved)
	disk.NUMANode = numaNodeOf(resolved)
	if transport, ok := nvmeSubsystemTransport(resolved); ok {
		disk.Transport = transport
	} else {
		disk.Transport = transportOf(resolved)
	}

	device, err := filepath.EvalSymlinks(filepath.Join(dir, "device"))
	if err != nil {
		return disk, nil
	}
	disk.Vendor = strings.TrimSpace(sysfs.String(device, "vendor"))
	disk.Model = strings.TrimSpace(sysfs.String(device, "model"))
	disk.Serial = strings.TrimSpace(sysfs.String(device, "serial"))
	return disk, nil
}

// readDevNumbers reads the dev attribute, which the kernel writes as the
// major number, a colon, and the minor number.
func readDevNumbers(dir string) (major, minor uint32, err error) {
	raw, err := sysfs.ReadAttr(dir, "dev")
	if err != nil {
		return 0, 0, fmt.Errorf("blockdev: read %s/dev: %w", dir, err)
	}
	majorText, minorText, ok := strings.Cut(raw, ":")
	if !ok {
		return 0, 0, fmt.Errorf("blockdev: %s/dev holds %q, which is not major:minor", dir, raw)
	}
	maj, err := strconv.ParseUint(majorText, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("blockdev: %s/dev holds %q: %w", dir, raw, err)
	}
	min, err := strconv.ParseUint(minorText, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("blockdev: %s/dev holds %q: %w", dir, raw, err)
	}
	return uint32(maj), uint32(min), nil
}

// classify decides what a device is, in the order the evidence is conclusive.
//
// The partition attribute comes first because it is the only positive marker a
// partition carries, and a partition of an NVMe namespace would otherwise be
// classified by everything its disk is. The name prefixes come last, and only
// where the subdirectory that would settle it is absent: an unbacked loop
// device has no loop directory, and its name is then the only evidence there
// is.
func classify(dir, name string) Kind {
	if fileExists(filepath.Join(dir, "partition")) {
		return KindPartition
	}
	switch {
	case isDir(filepath.Join(dir, "dm")):
		return KindDeviceMapper
	case isDir(filepath.Join(dir, "md")):
		return KindMDRaid
	case isDir(filepath.Join(dir, "loop")), strings.HasPrefix(name, "loop"):
		return KindLoop
	case strings.HasPrefix(name, "zram"), strings.HasPrefix(name, "ram"):
		return KindMemory
	case strings.HasPrefix(name, "nbd"), strings.HasPrefix(name, "rbd"),
		strings.HasPrefix(name, "drbd"):
		return KindNetwork
	case strings.HasPrefix(name, "dm-"):
		return KindDeviceMapper
	case strings.HasPrefix(name, "md"):
		return KindMDRaid
	}
	return KindDisk
}

// transportOf names the bus from the resolved device path.
//
// The path is the evidence because there is no transport attribute: the kernel
// says what a device is by where it puts it, and the enclosing subsystem
// directories are that statement. The order matters, because a SAS disk's path
// crosses a SCSI host and an ATA disk's crosses one too, so the specific
// markers are checked before the general one.
func transportOf(resolved string) Transport {
	var scsi bool
	for _, segment := range deviceSegments(resolved) {
		switch {
		case segment == "nvme":
			return TransportNVMe
		case numbered(segment, "virtio"):
			return TransportVirtio
		case segment == "mmc_host", numbered(segment, "mmc"):
			return TransportMMC
		case numbered(segment, "usb"):
			return TransportUSB
		case numbered(segment, "ata"):
			return TransportSATA
		case strings.HasPrefix(segment, "end_device-"), strings.HasPrefix(segment, "sas_"),
			strings.HasPrefix(segment, "expander-"):
			return TransportSAS
		case numbered(segment, "host"):
			scsi = true
		}
	}
	if scsi {
		return TransportSCSI
	}
	return TransportUnknown
}

// deviceSegments is the part of a resolved sysfs path below the device tree.
//
// Everything above it is the caller's mount point and says nothing about any
// bus. Skipping it is not tidiness: a probe pod mounts the host's sysfs at
// /host/sys, which put a segment called "host" in front of every device on the
// machine, and that read as a SCSI host directory. On a real worker it made
// every device-mapper node, loop device, and network block device a SCSI one.
func deviceSegments(resolved string) []string {
	segments := strings.Split(filepath.Clean(resolved), string(filepath.Separator))
	for i, segment := range segments {
		if segment == "devices" {
			return segments[i+1:]
		}
	}
	return nil
}

// numbered reports whether a segment is a prefix followed by an instance
// number, which is how the kernel names a bus: host0 is a SCSI host and ata1 is
// an ATA port, where host and ata alone are neither.
func numbered(segment, prefix string) bool {
	rest, ok := strings.CutPrefix(segment, prefix)
	if !ok || rest == "" {
		return false
	}
	for i := range len(rest) {
		if rest[i] < '0' || rest[i] > '9' {
			return false
		}
	}
	return true
}

// nvmeSubsystemTransport answers for a namespace the kernel presents through an
// NVMe subsystem rather than through a controller, and reports whether the path
// was one of those at all.
//
// The kernel puts the head device of a multipath namespace under
// devices/virtual/nvme-subsystem, and it does that for a dual-ported PCIe disk
// and for a fabric namespace alike. The path therefore cannot tell them apart,
// and the name cannot either: both are nvmeXnY. What can is the transport of
// the controllers that reach the namespace, which the subsystem directory links
// to. A controller reporting PCIe is a disk in this machine, and any other
// transport (TCP, RDMA, or Fibre Channel) is somewhere else's.
//
// A subsystem with no readable controller is reported as a fabric namespace,
// because that is the answer that refuses the device: a namespace whose origin
// could not be established is not one to hand to a cluster.
func nvmeSubsystemTransport(resolved string) (Transport, bool) {
	subsystem := filepath.Dir(filepath.Clean(resolved))
	if !strings.HasPrefix(filepath.Base(subsystem), "nvme-subsys") {
		return TransportUnknown, false
	}

	names, err := sysfs.List(subsystem)
	if err != nil {
		return TransportNVMeFabric, true
	}
	for _, name := range names {
		if !controllerName.MatchString(name) {
			continue
		}
		if sysfs.String(subsystem, name, "transport") == "pcie" {
			return TransportNVMe, true
		}
	}
	return TransportNVMeFabric, true
}

// controllerName matches the controller entries of a subsystem directory,
// nvme0 and nvme12, and not the namespaces (nvme0n1), the per-controller legs
// (nvme0c0n1), or the generic character devices (ng0n1) that sit beside them.
var controllerName = regexp.MustCompile(`^nvme[0-9]+$`)

// partitionsOf lists the partition children of a device, which sysfs nests
// inside the device's own directory.
//
// A child is a partition when it carries the partition attribute, and the name
// prefix is not the test: an NVMe namespace's partitions are nvme0n1p1 and a
// SCSI disk's are sda1, and both sit beside directories that are neither.
func partitionsOf(dir, name string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var parts []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == name {
			continue
		}
		if fileExists(filepath.Join(dir, entry.Name(), "partition")) {
			parts = append(parts, entry.Name())
		}
	}
	slices.Sort(parts)
	return parts
}

// childNames lists a directory's entries by name, ascending, which is the form
// the holders and slaves directories carry their answer in: the entry names are
// the devices, and what they link to adds nothing.
func childNames(dir string) []string {
	names, err := sysfs.List(dir)
	if err != nil || len(names) == 0 {
		return nil
	}
	slices.Sort(names)
	return names
}

// numaNodeOf reads the memory node of a device, climbing from it toward the bus
// until something says.
//
// It takes the resolved class path rather than the device link, because a
// partition has no device link: the kernel nests a partition inside its disk's
// directory and exports no device of its own for it. A partition sits in the
// same slot, on the same bus, and therefore on the same memory node as the disk
// that carries it, and the resolved path is what still says so. Reading the
// device link instead reported every partition on no node at all.
//
// A SCSI disk's own directory carries no numa_node either; the HBA it hangs off
// does, and that is the node a caller pinning work to a socket needs. The climb
// stops at the PCI device, because past that lies the host bridge, whose node
// is not the device's.
func numaNodeOf(device string) int {
	pci := sysfs.PCIAddressOf(device)
	for dir := filepath.Clean(device); ; {
		if node := sysfs.Int(NUMANodeUnknown, dir, "numa_node"); node != NUMANodeUnknown {
			return node
		}
		if pci != "" && filepath.Base(dir) == pci {
			return NUMANodeUnknown
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return NUMANodeUnknown
		}
		dir = parent
	}
}

// isDir and fileExists are the two presence questions the classifier asks. A
// directory that is there is a fact about the device; a file that is there is
// another.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
