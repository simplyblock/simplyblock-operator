// What one worker reported about itself, as the JSON both sides of the probe
// agree on.
//
// The probe binary writes this and the discovery step reads it, so it is a wire
// format and not a view: renaming a field breaks a probe pod already running
// with an older image, which is what Version is for.
//
// It is a purpose-built type rather than atlas/inventory marshaled directly,
// for two reasons. The first is that inventory would not survive the trip:
// blockdev.Usage carries a ProbeErr of type error, and an error marshals to {},
// so the reason a device could not be checked would vanish on the way — exactly
// the silent loss the content reading exists to prevent. The second is that
// this is the document's vocabulary rather than the kernel's: what a
// ClusterDeploymentConfig names is a PCI address, a size, and whether a device
// is free, and a reviewer reading a report should see those and not a sysfs
// tree.
//
// The environment is deliberately absent. Which distribution a cluster runs is
// a fact about the cluster, so the discovery step concludes it once from the
// API rather than having twenty probe pods each list every node to reach the
// same answer.

package nodeprobe

import (
	"encoding/json"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReportVersion is the schema version of the JSON below.
//
// A reader refuses a version it does not know rather than guessing at it,
// because a probe pod outlives the operator that created it across an upgrade:
// the image is pinned in the Job, and a Job already running keeps the image it
// started with.
const ReportVersion = 1

// Report is one worker's inventory as the probe found it.
type Report struct {
	// Version is ReportVersion as of the probe that wrote this.
	Version int `json:"version"`

	// Node is the name of the Kubernetes node this was collected on, which is
	// the name a ClusterDeploymentConfig names the worker by.
	Node string `json:"node"`

	// ProbedAt is when the collection ran.
	ProbedAt metav1.Time `json:"probedAt"`

	// CPU is the machine's processor inventory.
	CPU CPU `json:"cpu"`

	// Memory is how much the machine has and how much of it is available,
	// which the huge-page reading alone does not say.
	Memory Memory `json:"memory"`

	// HugePages is what is already set aside, one entry per page size.
	HugePages []HugePagePool `json:"hugePages,omitempty"`

	// Interfaces is every network interface, virtual ones included: which is a
	// data NIC depends on the deployment's addressing, so the report carries
	// the evidence rather than the choice.
	Interfaces []Interface `json:"interfaces,omitempty"`

	// Devices is every block device, refused ones included and each with the
	// grounds it was refused on.
	Devices []Device `json:"devices,omitempty"`

	// NVMeControllers is every NVMe controller on the machine's PCI bus, with
	// the driver that owns each.
	//
	// It is here because Devices cannot see all of them. A controller a
	// userspace driver has been given has no block device at all, so a worker
	// with four of them reports no NVMe disks: this is what tells a reviewer
	// the disks exist and something else is driving them, rather than leaving
	// them to conclude the machine has none.
	NVMeControllers []Controller `json:"nvmeControllers,omitempty"`

	// Unreadable is what the probe could not read, one sentence each. It is
	// separate from a device's own rejections: this is the machine refusing to
	// answer, where a rejection is an answer.
	//
	// A report with entries here is still a report. A worker whose CPU tree
	// could not be read still has disks worth reviewing, and a probe that
	// returned nothing would hide them.
	Unreadable []string `json:"unreadable,omitempty"`
}

// CPU is what the probe found out about the machine's processors.
//
// The affinity count atlas/inventory also reads is not here. It is a fact about
// the probe's own cgroup rather than about the worker, and a reviewer sizing a
// storage node against it would size it against the probe pod's limits.
type CPU struct {
	// OnlineCPUs is the logical CPUs work can be placed on, which is the number
	// an SPDK core mask is drawn from.
	OnlineCPUs int `json:"onlineCPUs"`

	// OfflineCPUs is how many the kernel knows about and is not using. A worker
	// with any is one worth asking about.
	OfflineCPUs int `json:"offlineCPUs,omitempty"`

	// PhysicalCores counts a hyperthreaded core once.
	PhysicalCores int `json:"physicalCores"`

	// Sockets is the number of physical packages.
	Sockets int `json:"sockets"`

	// ThreadsPerCore is 1 without simultaneous multithreading and 2 with it on
	// almost every host.
	ThreadsPerCore int `json:"threadsPerCore"`

	// HyperThreading reports whether simultaneous multithreading is active.
	HyperThreading bool `json:"hyperThreading"`

	// NUMANodes says which of the online CPUs each memory node owns, ascending.
	NUMANodes []NUMACPUs `json:"numaNodes,omitempty"`
}

// NUMACPUs is one memory node's share of the processors.
type NUMACPUs struct {
	Node          int   `json:"node"`
	OnlineCPUs    []int `json:"onlineCPUs,omitempty"`
	PhysicalCores int   `json:"physicalCores"`
}

// Memory is the machine's memory as the kernel reports it, in bytes.
type Memory struct {
	// TotalBytes is all usable RAM.
	TotalBytes uint64 `json:"totalBytes"`

	// FreeBytes is memory nothing holds, and AvailableBytes is what a new
	// process could actually get: on a busy host the second is far larger,
	// because most of what is not free is page cache the kernel reclaims on
	// demand. Size against available.
	FreeBytes      uint64 `json:"freeBytes"`
	AvailableBytes uint64 `json:"availableBytes"`

	// HugePagesBytes is memory reserved as huge pages, which is out of the
	// general pool whether or not the pages are in use.
	HugePagesBytes uint64 `json:"hugePagesBytes,omitempty"`

	// SwapTotalBytes and SwapFreeBytes describe the swap the host has. A
	// storage node host with swap in use is already oversubscribed.
	SwapTotalBytes uint64 `json:"swapTotalBytes,omitempty"`
	SwapFreeBytes  uint64 `json:"swapFreeBytes,omitempty"`

	// NUMANodes is the same per memory node, ascending.
	NUMANodes []NUMAMemory `json:"numaNodes,omitempty"`
}

// NUMAMemory is one memory node's share.
type NUMAMemory struct {
	Node       int    `json:"node"`
	TotalBytes uint64 `json:"totalBytes"`
	FreeBytes  uint64 `json:"freeBytes"`
}

// HugePagePool is one page size's allocation.
type HugePagePool struct {
	// SizeBytes is the size of one page, 2 MiB or 1 GiB on x86.
	SizeBytes uint64 `json:"sizeBytes"`

	// Total and Free are pages rather than bytes, matching what the kernel
	// exports. AllocatedBytes does the multiplication.
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`

	// NUMANodes is the same allocation per memory node, ascending. A host with
	// the right total and the wrong distribution starts SPDK on a node that
	// then cannot get memory.
	NUMANodes []NUMAHugePages `json:"numaNodes,omitempty"`
}

// AllocatedBytes is how much memory this pool holds.
func (p HugePagePool) AllocatedBytes() uint64 { return p.Total * p.SizeBytes }

// NUMAHugePages is one memory node's share of one pool.
type NUMAHugePages struct {
	Node  int    `json:"node"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// Interface is one network interface.
type Interface struct {
	Name       string `json:"name"`
	MACAddress string `json:"macAddress,omitempty"`

	// SpeedMbps is the negotiated link speed, and zero means unknown rather
	// than slow: a link that is down has no speed and neither does a virtual
	// device.
	SpeedMbps int `json:"speedMbps,omitempty"`

	MTU   int    `json:"mtu,omitempty"`
	State string `json:"state,omitempty"`

	// Driver and PCIAddress are empty for a device with no hardware behind it.
	Driver     string `json:"driver,omitempty"`
	PCIAddress string `json:"pciAddress,omitempty"`

	// NUMANode is the memory node the interface hangs off, or NUMANodeUnknown.
	NUMANode int `json:"numaNode"`

	Virtual  bool `json:"virtual,omitempty"`
	Loopback bool `json:"loopback,omitempty"`
}

// Device is one block device and whether it may be handed to a storage cluster.
type Device struct {
	// Name is the kernel name and Path is the /dev path. A logical
	// block-device deployment names a device by its path.
	Name string `json:"name"`
	Path string `json:"path"`

	// PCIAddress is the slot, in the form an NVMe deployment names a device by.
	PCIAddress string `json:"pciAddress,omitempty"`

	SizeBytes uint64 `json:"sizeBytes"`

	// Kind is what the device is and Transport is the bus it sits on, both in
	// the spelling atlas/blockdev uses.
	Kind      string `json:"kind"`
	Transport string `json:"transport,omitempty"`

	Vendor     string `json:"vendor,omitempty"`
	Model      string `json:"model,omitempty"`
	Serial     string `json:"serial,omitempty"`
	Rotational bool   `json:"rotational,omitempty"`

	// NUMANode is the memory node the device hangs off, or NUMANodeUnknown.
	NUMANode int `json:"numaNode"`

	// Available reports whether the device may be handed over: a whole disk, on
	// a bus the scan recognized, that nothing is using and that positively
	// reads as holding nothing.
	Available bool `json:"available"`

	// Content and ContentType are what the device was read to carry, and are
	// empty for a device that was refused before anything was opened.
	Content     string `json:"content,omitempty"`
	ContentType string `json:"contentType,omitempty"`

	// Rejections is every ground the device was refused on, and is empty when
	// Available. Both are written, rather than one being derived from the
	// other, so that a reader on an older schema still gets the verdict.
	Rejections []Rejection `json:"rejections,omitempty"`
}

// Controller is one NVMe controller on the PCI bus.
type Controller struct {
	// Address is the slot, in the form a deployment config names an NVMe
	// device by.
	Address string `json:"address"`

	// Driver is what owns it: the kernel's own driver for a controller whose
	// namespaces it presents, uio_pci_generic or vfio-pci for one a userspace
	// driver has, and empty for one nothing owns.
	Driver string `json:"driver,omitempty"`

	// Vendor and Product are the raw PCI identifiers, which is what sysfs has:
	// resolving them to names needs a database the probe does not carry.
	Vendor  string `json:"vendor,omitempty"`
	Product string `json:"product,omitempty"`

	// NUMANode is the memory node it hangs off, or NUMANodeUnknown.
	NUMANode int `json:"numaNode"`

	// TakenByUserspace reports whether a userspace-IO driver owns it, which on
	// this product's hosts means SPDK has it or something left it taken.
	TakenByUserspace bool `json:"takenByUserspace,omitempty"`
}

// ControllersTakenByUserspace is the controllers no block device corresponds
// to, which is the answer to why a worker full of disks reported none.
func (r Report) ControllersTakenByUserspace() []Controller {
	var taken []Controller
	for _, controller := range r.NVMeControllers {
		if controller.TakenByUserspace {
			taken = append(taken, controller)
		}
	}
	return taken
}

// Rejection is one ground with the evidence for it.
type Rejection struct {
	// Reason is the ground, in the spelling blockdev.Reason uses.
	Reason string `json:"reason"`

	// Detail is the finding in words: which mountpoint, which holder, which
	// signature at which offset.
	Detail string `json:"detail,omitempty"`
}

// RejectedFor reports whether reason is among the grounds.
func (d Device) RejectedFor(reason string) bool {
	return slices.ContainsFunc(d.Rejections, func(r Rejection) bool { return r.Reason == reason })
}

// OnlyRejectedFor reports whether the device was refused and every ground is
// among those given.
//
// It mirrors blockdev.Candidate.OnlyRejectedFor so that an override survives
// the trip through JSON. A caller accepting a device whose partition table is
// stale asks OnlyRejectedFor("Partitioned"), and a device that is also mounted
// answers false, so the override cannot widen into one that takes a disk in
// use.
func (d Device) OnlyRejectedFor(reasons ...string) bool {
	if d.Available || len(d.Rejections) == 0 {
		return false
	}
	for _, rejection := range d.Rejections {
		if !slices.Contains(reasons, rejection.Reason) {
			return false
		}
	}
	return true
}

// AvailableDevices is the devices that may be handed to a storage cluster.
func (r Report) AvailableDevices() []Device {
	var free []Device
	for _, device := range r.Devices {
		if device.Available {
			free = append(free, device)
		}
	}
	return free
}

// HugePageBytes is how much huge-page memory the worker holds across every size.
func (r Report) HugePageBytes() uint64 {
	var total uint64
	for _, pool := range r.HugePages {
		total += pool.AllocatedBytes()
	}
	return total
}

// Encode renders the report as the bytes that go into a ConfigMap.
//
// It is indented because the value a human reads is a kubectl get -o YAML of a
// ConfigMap, and a single line of minified JSON in that output is unreadable.
// The size difference does not matter against the 1 MiB an object may hold.
func Encode(report Report) ([]byte, error) {
	report.Version = ReportVersion
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the report for node %s: %w", report.Node, err)
	}
	return out, nil
}

// Decode parses a report and refuses a version it does not know.
//
// Refusing is right rather than cautious. A Job keeps the image it started
// with, so an operator upgraded mid-run reads reports from the previous probe,
// and a field that changed meaning between the two would be read as the current
// one. A refused report is a probe to re-run; a misread one is a device list
// nobody can trust.
func Decode(data []byte) (Report, error) {
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, fmt.Errorf("parse the probe report: %w", err)
	}
	if report.Version != ReportVersion {
		return Report{}, fmt.Errorf(
			"the probe report is version %d and this operator reads version %d, so the probe has to be re-run",
			report.Version, ReportVersion)
	}
	if report.Node == "" {
		return Report{}, fmt.Errorf("the probe report names no node, so nothing can be attributed to it")
	}
	return report, nil
}
