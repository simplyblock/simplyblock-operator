// A deployment's whole inventory, gathered in one call.
//
// This is the only place a caller has to know about to ask what there is to
// deploy on. The five readings come from five places — the CPU topology, the
// huge-page pools, the network interfaces, the block devices, and the
// Kubernetes API — and a discovery run wants all five before it writes a
// document, so collecting them separately would leave every caller writing the
// same five calls and the same partial-failure handling.
//
// Four of the five are one machine's and the fifth is the cluster's, and they
// are gathered together anyway because that is the shape of the answer: a
// ClusterDeploymentConfig states an environment and a set of workers with their
// devices, so a run that produced one half without the other has produced
// nothing reviewable.
//
// The disks are read by the blockdev package rather than here. What a block
// device is, who is using it, and whether it may be handed over is that
// package's subject and needs to open devices where the rest of this one only
// reads attributes. This file composes that reading in; it does not repeat it.
//
// ByNUMANode is the reason the per-node readings exist at all. A storage node
// is pinned to a socket, and it needs its cores, its memory, its NIC, and its
// disks on the same side of the interconnect; the join is what makes that
// answerable before anything is deployed.

package inventory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/discovery"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/pci"
)

const (
	// DefaultSysfsRoot is where the kernel's sysfs is mounted on a host that
	// did not move it.
	DefaultSysfsRoot = "/sys"

	// DefaultProcRoot is the conventional procfs mount point.
	DefaultProcRoot = "/proc"
)

// NUMANodeUnknown is the memory node of a device that sits on no bus, and of
// one whose bus does not say.
//
// It is blockdev's constant rather than a second one with the same value. Both
// packages report it on the same kind of field, and two definitions of one
// kernel sentinel is one definition too many.
const NUMANodeUnknown = blockdev.NUMANodeUnknown

// Config names the trees a reading is taken from, and the two seams the disk
// reading is taken through.
//
// A container inspecting its host mounts that host's /sys, /proc, and /dev
// somewhere of its own choosing, and the three roots are separate fields
// because nothing requires them to sit beside each other.
type Config struct {
	// SysfsRoot is the sysfs mount point, defaulting to DefaultSysfsRoot.
	SysfsRoot string

	// ProcRoot is the procfs mount point, defaulting to DefaultProcRoot.
	ProcRoot string

	// DevRoot is the device-node directory, defaulting to
	// blockdev.DefaultDevRoot. It decides the paths the disks are opened by.
	DevRoot string

	// MountinfoPath is the mount table the disk reading consults, defaulting to
	// this process's own. A collection running in a pod has to point it at the
	// host's, which is PID 1's: a pod has its own mount namespace, so its own
	// table lists none of the host's mounts and every mounted host disk would
	// be reported free. See blockdev.ScanConfig.MountinfoPath.
	MountinfoPath string

	// Prober reads what each block device carries. A nil Prober is the local
	// one, reading devices on this host with the page cache bypassed.
	Prober *blockdev.Prober

	// Exclusive asks the kernel whether it will hand a device over. A nil
	// Exclusive is blockdev.OpenExclusive, the real question against the real
	// kernel.
	Exclusive blockdev.ExclusiveOpener

	// Kubernetes is the cluster half of a collection's sources. The zero value
	// collects no environment, which is what a caller inspecting a machine
	// outside a cluster has.
	Kubernetes KubernetesSources
}

// KubernetesSources is what the environment is concluded from.
//
// Both fields are optional and are read independently: a caller with the nodes
// but no permission to list API groups still gets a conclusion, from the half
// it has. Both being empty is not an error either, and it is the difference
// between Environment.Distribution being empty and being DistributionVanilla.
type KubernetesSources struct {
	// Discovery lists the API groups the server registers, which is the
	// strongest evidence a distribution leaves.
	Discovery discovery.DiscoveryInterface

	// Nodes are the cluster's nodes. A caller inspecting a fleet has listed
	// them already, so they are passed in rather than listed again here.
	Nodes []corev1.Node
}

// sysfs, proc, and dev resolve the roots, so that the zero Config reads the
// live host.
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
		return blockdev.DefaultDevRoot
	}
	return c.DevRoot
}

// sysfsPath and procPath join a relative path onto the resolved root.
func (c Config) sysfsPath(elem ...string) string {
	return filepath.Join(append([]string{c.sysfs()}, elem...)...)
}

func (c Config) procPath(elem ...string) string {
	return filepath.Join(append([]string{c.proc()}, elem...)...)
}

// inspector is the disk reading this configuration describes, with the roots
// resolved once so that the block devices are read from the same trees as
// everything else.
func (c Config) inspector() blockdev.Inspector {
	return blockdev.Inspector{
		Config: blockdev.ScanConfig{
			SysfsRoot:     c.sysfs(),
			ProcRoot:      c.proc(),
			DevRoot:       c.dev(),
			MountinfoPath: c.MountinfoPath,
		},
		Prober:    c.Prober,
		Exclusive: c.Exclusive,
	}
}

// Inventory is one worker's resources, as one value.
//
// It is a snapshot with no handle back to the host, following the convention
// the NVMe package holds to: a stale inventory is re-collected rather than
// refreshed in place.
type Inventory struct {
	// CPU is the machine's processor count and topology, including which CPUs
	// each memory node owns.
	CPU CPU

	// Memory is how much the machine has and how much of it is available,
	// which huge pages alone do not say: a host may have 12 GiB reserved as
	// huge pages and 4 GiB of ordinary memory left, and a storage node needs
	// both.
	Memory Memory

	// HugePages is what has already been allocated, per size and per NUMA node.
	HugePages HugePages

	// Interfaces is every network interface the kernel presents, virtual ones
	// included: which of them is a data NIC is the caller's judgment, and a
	// reading that dropped the others would not let the caller make it.
	Interfaces []Interface

	// Devices is every block device the kernel presents, with the grounds on
	// which each one was refused as backend storage. Refused devices are here
	// too, for the same reason the virtual interfaces are: an administrator has
	// to be able to be told why the disk they expected is not a candidate.
	Devices []blockdev.Candidate

	// NVMeControllers is every NVMe controller on the machine's PCI bus, with
	// the driver that owns each.
	//
	// It is here because the disk reading cannot see all of them. SPDK takes a
	// controller by rebinding it from the kernel's NVMe driver to a
	// userspace-IO one, and from that moment the kernel presents no block
	// device for it: a worker with four such controllers reports no NVMe disks
	// at all. This is what says the disks are there and something else has
	// them.
	NVMeControllers []pci.Device

	// Environment is which Kubernetes distribution the cluster runs, and the
	// markers that said so.
	//
	// Its Distribution is empty when no Kubernetes sources were given, which is
	// not the same as DistributionVanilla: one means nothing was asked, and the
	// other means the cluster was read and carried no distinctive marker.
	Environment Environment
}

// AvailableDevices is the block devices that may be handed to a storage
// cluster.
//
// It is the shortcut for the common read, and it is deliberately not what
// Devices holds: a caller that needs to explain a refusal, or that means to
// override one with blockdev.Candidate.OnlyRejectedFor, reads Devices instead.
func (i Inventory) AvailableDevices() []blockdev.Candidate {
	var free []blockdev.Candidate
	for _, device := range i.Devices {
		if device.Available() {
			free = append(free, device)
		}
	}
	return free
}

// ControllersTakenByUserspace is the NVMe controllers a userspace driver owns,
// which are the disks this machine has and the kernel does not present.
//
// A discovery run that found no candidate devices should say whether this is
// empty: no disks and no controllers is a machine with no storage, and no disks
// with four controllers is a machine whose storage something else is already
// driving. They are different answers and only one of them is a surprise.
func (i Inventory) ControllersTakenByUserspace() []pci.Device {
	var taken []pci.Device
	for _, controller := range i.NVMeControllers {
		if controller.BoundToUserspace() {
			taken = append(taken, controller)
		}
	}
	return taken
}

// NUMANodeInventory is everything one memory node has.
type NUMANodeInventory struct {
	// Node is the memory node's id, or NUMANodeUnknown for the entry holding
	// what could not be placed on any node.
	Node int

	// CPUs is the node's share of the host's processors.
	CPUs NUMACPUs

	// Memory is the node's own memory, which is what a storage node pinned
	// here would draw on.
	Memory NUMAMemory

	// HugePages is the node's share of each huge-page pool, ascending by page
	// size. The page size is on the entry, so a caller reading one node's
	// memory does not have to hold the pool it came from.
	HugePages []NUMAHugePagesOfSize

	// Interfaces and Devices are the network interfaces and block devices
	// attached to this node.
	Interfaces []Interface
	Devices    []blockdev.Candidate

	// NVMeControllers is the NVMe controllers on this node, including the ones
	// no block device corresponds to because a userspace driver has them.
	NVMeControllers []pci.Device
}

// NUMAHugePagesOfSize is one node's share of one pool, carrying the page size
// the pool is for.
//
// NUMAHugePages does not carry the size, because it sits inside the pool that
// states it. A per-node rollup has no pool around it, so the size travels with
// the entry rather than being lost.
type NUMAHugePagesOfSize struct {
	NUMAHugePages

	// SizeBytes is the size of one page in the pool this entry came from.
	SizeBytes uint64
}

// AllocatedBytes is how much huge-page memory this entry accounts for.
func (n NUMAHugePagesOfSize) AllocatedBytes() uint64 { return n.Total * n.SizeBytes }

// FreeBytes is how much of it nothing has taken.
func (n NUMAHugePagesOfSize) FreeBytes() uint64 { return n.Free * n.SizeBytes }

// ByNUMANode is the inventory grouped by memory node, ascending, with one final
// entry for what belongs to no node.
//
// That last entry is a finding rather than a fixture: it appears only when
// something could not be placed, and it exists because a rollup that silently
// dropped the unplaceable would read as the whole inventory while missing part
// of it. A bridge and loopback hang off no bus, and so does a disk behind a
// controller whose bus does not report a node.
//
// The nodes come from the CPU reading, which is the one source that enumerates
// them, so a host whose CPU topology could not be read yields the unknown entry
// alone. That is the honest answer: nothing can be placed when there is nowhere
// to place it.
func (i Inventory) ByNUMANode() []NUMANodeInventory {
	byNode := make(map[int]*NUMANodeInventory, len(i.CPU.NUMANodes)+1)
	order := make([]int, 0, len(i.CPU.NUMANodes)+1)

	node := func(id int) *NUMANodeInventory {
		if existing, ok := byNode[id]; ok {
			return existing
		}
		byNode[id] = &NUMANodeInventory{Node: id}
		order = append(order, id)
		return byNode[id]
	}

	for _, cpus := range i.CPU.NUMANodes {
		node(cpus.Node).CPUs = cpus
	}

	for _, share := range i.Memory.NUMANodes {
		node(share.Node).Memory = share
	}

	for _, pool := range i.HugePages.Pools {
		for _, share := range pool.NUMANodes {
			entry := NUMAHugePagesOfSize{NUMAHugePages: share, SizeBytes: pool.SizeBytes}
			target := node(share.Node)
			target.HugePages = append(target.HugePages, entry)
		}
	}

	for _, iface := range i.Interfaces {
		target := node(iface.NUMANode)
		target.Interfaces = append(target.Interfaces, iface)
	}

	for _, device := range i.Devices {
		target := node(device.NUMANode)
		target.Devices = append(target.Devices, device)
	}

	for _, controller := range i.NVMeControllers {
		target := node(controller.NUMANode)
		target.NVMeControllers = append(target.NVMeControllers, controller)
	}

	// Ascending by node, with the unknown entry last: it is not a node, and a
	// caller walking the list expects the real ones first. Its id is negative,
	// so it has to be moved rather than sorted into place.
	slices.Sort(order)
	if len(order) > 0 && order[0] == NUMANodeUnknown {
		order = append(order[1:], NUMANodeUnknown)
	}

	nodes := make([]NUMANodeInventory, 0, len(order))
	for _, id := range order {
		entry := *byNode[id]
		slices.SortFunc(entry.HugePages, func(a, b NUMAHugePagesOfSize) int {
			return cmp.Compare(a.SizeBytes, b.SizeBytes)
		})
		nodes = append(nodes, entry)
	}
	return nodes
}

// Collect reads all five, and returns what it could read beside what it could
// not.
//
// The error joins every reader that failed, so a caller inspecting twenty
// workers can record the failure against the one worker and keep the rest of
// its inventory. The returned Inventory is populated for the readers that
// succeeded whatever the error says, which is why the error is not a reason to
// discard it.
//
// A device that could not be read is not one of those failures. The disk
// reading returns every device with the grounds it was refused on, and "its
// content could not be established" is one of those grounds: it belongs on the
// device rather than on the collection, because the other nineteen disks are
// still readable.
func Collect(ctx context.Context, cfg Config) (Inventory, error) {
	var inv Inventory
	var errs []error

	cpu, err := ReadCPU(cfg)
	if err != nil {
		errs = append(errs, fmt.Errorf("read the CPU topology: %w", err))
	}
	inv.CPU = cpu

	memory, err := ReadMemory(cfg)
	if err != nil {
		errs = append(errs, fmt.Errorf("read the memory: %w", err))
	}
	inv.Memory = memory

	pages, err := ReadHugePages(cfg)
	if err != nil {
		errs = append(errs, fmt.Errorf("read the huge pages: %w", err))
	}
	inv.HugePages = pages

	ifaces, err := ReadInterfaces(cfg)
	if err != nil {
		errs = append(errs, fmt.Errorf("read the network interfaces: %w", err))
	}
	inv.Interfaces = ifaces

	devices, err := cfg.inspector().Candidates(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("read the block devices: %w", err))
	}
	inv.Devices = devices

	controllers, err := pci.Scan(pci.Config{
		SysfsRoot: cfg.sysfs(),
		ProcRoot:  cfg.proc(),
		DevRoot:   cfg.dev(),
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("read the PCI controllers: %w", err))
	}
	inv.NVMeControllers = pci.NVMeControllers(controllers)

	if cfg.Kubernetes.Discovery != nil || len(cfg.Kubernetes.Nodes) > 0 {
		env, err := CollectEnvironment(ctx, cfg.Kubernetes.Discovery, cfg.Kubernetes.Nodes)
		if err != nil {
			errs = append(errs, fmt.Errorf("read the Kubernetes environment: %w", err))
		}
		inv.Environment = env
	}

	return inv, errors.Join(errs...)
}
