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

	"github.com/simplyblock/atlas/pci"
)

// ReportVersion is the schema version of the JSON below.
//
// A reader refuses a version it does not know rather than guessing at it,
// because a probe pod outlives the operator that created it across an upgrade:
// the image is pinned in the Job, and a Job already running keeps the image it
// started with.
//
// Version 8 added StablePath, and it is a bump rather than an additive field
// because absence means something. A version-7 report decodes with StablePath
// empty on every device, which a reader takes as "udev published no link" and
// falls back to the kernel path for — so an old probe's report, read by a new
// operator, drafts exactly the enumeration-order names this version exists to
// replace, and says nothing about having done so. Refusing it re-runs the probe,
// which is cheap and correct.
//
// Version 9 carries each interface address with its prefix length, and is a bump
// for the same reason. A version-8 address names no network, so a reader
// comparing the networks of two workers would find nothing to compare and pass
// a fleet whose links cannot reach each other.
//
// Version 10 carries the kernel's release and capabilities, and is a bump
// because a version-9 report decodes with every kernel fact empty. A reader
// that warns about a worker whose native NVMe multipath is not enabled would
// find nothing to warn about, and pass a worker that cannot mount a volume
// over more than one path.
//
// Version 11 carries the NetworkAttachmentDefinitions the run's namespace
// holds, and is a bump because a version-10 report decodes with none. A run
// asked to attach its storage nodes through a definition would read that as a
// worker in a cluster without Multus and plan it onto host networking.
const ReportVersion = 11

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

	// HostOS is the operating system the worker boots and the architecture it
	// runs on.
	//
	// It is the machine's own answer and not the cluster's, which is why it is
	// here where the environment is not: the distribution a node runs decides
	// whether a kernel module has to be installed before NVMe-oF works and
	// which package manager would install it, and two workers of one cluster
	// can differ.
	HostOS HostOS `json:"hostOS"`

	// NVMeControllers is every NVMe controller on the machine's PCI bus, with
	// the driver that owns each.
	//
	// It is here because Devices cannot see all of them. A controller a
	// userspace driver has been given has no block device at all, so a worker
	// with four of them reports no NVMe disks: this is what tells a reviewer
	// the disks exist and something else is driving them, rather than leaving
	// them to conclude the machine has none.
	NVMeControllers []Controller `json:"nvmeControllers,omitempty"`

	// Reclaimed is the controllers this probe handed back to the kernel, as
	// they were before it did.
	//
	// The probe takes a controller a dead deployment left on a userspace driver
	// so that the disks behind it can be read at all, and leaves it on the
	// kernel driver afterward, because the path a draft records has to still
	// exist when the node is added. These are therefore the disks in Devices
	// that are there only because the probe changed the machine, and a reader
	// comparing a worker against what it looked like an hour ago is owed the
	// list.
	Reclaimed []Controller `json:"reclaimed,omitempty"`

	// NetworkAttachments is the Multus NetworkAttachmentDefinitions the run's
	// namespace holds, read by this probe through the API.
	//
	// It is in a worker's report although the definitions are the cluster's,
	// because what each one builds on is the worker's: a definition whose master
	// interface this machine does not have cannot attach a pod on it, and
	// Interfaces above is the evidence that is checked against. It is absent for
	// a probe that was given no cluster to ask, which is not a cluster without
	// the kind.
	NetworkAttachments *NetworkAttachments `json:"networkAttachments,omitempty"`

	// Unreadable is what the probe could not read, one sentence each. It is
	// separate from a device's own rejections: this is the machine refusing to
	// answer, where a rejection is an answer.
	//
	// A report with entries here is still a report. A worker whose CPU tree
	// could not be read still has disks worth reviewing, and a probe that
	// returned nothing would hide them.
	Unreadable []string `json:"unreadable,omitempty"`
}

// HostOS is the worker's operating system as the probe read it.
//
// Every field is empty on a probe that could not read the host's os-release,
// and Unreadable then carries the reason. An empty distro is not a host with
// no distribution. It is a reading that was not taken.
type HostOS struct {
	// Distro is the os-release ID, such as `ubuntu` or `rocky`.
	Distro string `json:"distro,omitempty"`

	// Family is the packaging tradition the distro belongs to: `Debian`,
	// `RedHat`, `SUSE`, `Alpine`, or `Arch`. It is empty for a host with no package manager and
	// for one this product does not place.
	Family string `json:"family,omitempty"`

	// Version is the os-release VERSION_ID, such as 22.04.
	Version string `json:"version,omitempty"`

	// PrettyName is the distribution's own description, which is what a
	// reviewer recognizes fastest.
	PrettyName string `json:"prettyName,omitempty"`

	// Architecture is uname's machine field: `x86_64`, `aarch64`. It says whether
	// the images this product ships would run on the worker at all.
	Architecture string `json:"architecture,omitempty"`

	// Kernel is what the kernel the worker boots offers.
	Kernel Kernel `json:"kernel"`
}

// Kernel is the worker's kernel and the capabilities it offers, each with the
// evidence it was concluded from.
type Kernel struct {
	// Release is what `uname -r` prints, such as
	// `6.1.155-176.282.amzn2023.x86_64`. It is empty when it could not be read,
	// and Unreadable then says why.
	Release string `json:"release,omitempty"`

	// Version is the upstream version the release opens with, and is absent
	// when the release does not open with one.
	Version *KernelVersion `json:"version,omitempty"`

	// NVMeMultipath is `Enabled`, `Disabled`, `NotBuilt`, or `Unknown`. A worker
	// on which it is not Enabled can host a storage node and cannot mount a
	// volume over more than one path: each path is a disk of its own.
	NVMeMultipath Fact `json:"nvmeMultipath"`

	// KVM, TUN, and PNFSBlock are `Available`, `Loadable`, `Absent`, or
	// `Unknown`: /dev/kvm, /dev/net/tun, and the pNFS block layout driver.
	KVM       Fact `json:"kvm"`
	TUN       Fact `json:"tun"`
	PNFSBlock Fact `json:"pnfsBlock"`
}

// KernelVersion is the upstream kernel version, `6.1.155` as three numbers. A
// distribution backports fixes without changing it, so it places a kernel in
// upstream's history and does not say which fixes the kernel carries.
type KernelVersion struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
	Patch int `json:"patch"`
}

// Fact is one conclusion about the kernel and the evidence for it.
type Fact struct {
	State string `json:"state,omitempty"`

	// Detail names the source that answered and what it said.
	Detail string `json:"detail,omitempty"`
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

	Virtual bool `json:"virtual,omitempty"`

	// Peered reports whether the interface is one end of a pair, which is what
	// a pod's link into the host is. It is read from iflink and is the one
	// reading that tells such a link from the machine's own virtual devices:
	// sysfs describes them identically otherwise.
	Peered   bool `json:"peered,omitempty"`
	Loopback bool `json:"loopback,omitempty"`

	// Bridge reports whether the interface is a software bridge, which a
	// cluster's own CNI leaves on every worker. It is separate from Virtual
	// because the two answer different questions: Virtual says the interface is
	// backed by no hardware, and Bridge says it is carrying somebody else's
	// traffic.
	Bridge bool `json:"bridge,omitempty"`

	// Kind is what sort of device the interface is, in the spelling
	// inventory.LinkKind uses: `physical`, `loopback`, `bridge`, `bond`, `team`,
	// `vlan`, `vxlan`, `macvlan`, `ipvlan`, or `virtual` for a device the kernel
	// does not identify.
	//
	// It is the field the management-interface rule turns on. Every software
	// interface a worker has is virtual, so without the kind a bond and a tagged
	// VLAN, which is where most fleets put their management address, read the
	// same as a veth to a pod.
	Kind string `json:"kind,omitempty"`

	// Lower is what this interface is built on, ascending by name: the members
	// of a bond or a bridge, or the single parent of a VLAN or a macvlan.
	//
	// It is how a draft reaches the hardware under an aggregate. A bond reports
	// no slot, no driver, and no memory node of its own, so a reader that has
	// to know where a bonded management network lands looks its members up in
	// the same report.
	Lower []string `json:"lower,omitempty"`

	// Upper is what is built on this interface, ascending by name. It is the
	// direction that answers for a NIC holding no address: on a host whose
	// management network is tagged, the address is on a VLAN above it.
	Upper []string `json:"upper,omitempty"`

	// VLAN is the tag of a VLAN interface and VXLAN the network identifier of
	// a VXLAN one. Each is absent for every other kind, and for an interface
	// whose identifier the probe could not read over netlink.
	VLAN  *VLAN  `json:"vlan,omitempty"`
	VXLAN *VXLAN `json:"vxlan,omitempty"`

	// Addresses are the IP addresses the interface holds, in CIDR notation such
	// as 10.10.0.1/24. They are what makes a management interface identifiable:
	// the one a draft names is the one carrying the address the cluster already
	// reaches the machine on. The prefix length is what says which network the
	// address is on, which is how two workers' links are told to be on one
	// network or on two.
	Addresses []string `json:"addresses,omitempty"`
}

// VLAN is the tag a VLAN interface puts on its frames.
type VLAN struct {
	// ID is the VLAN identifier, 0 to 4094.
	ID int `json:"id"`

	// Protocol is `802.1Q` or `802.1ad`.
	Protocol string `json:"protocol,omitempty"`
}

// VXLAN is what identifies the overlay a VXLAN interface belongs to.
type VXLAN struct {
	// VNI is the VXLAN network identifier, 0 to 16777215.
	VNI int `json:"vni"`
}

// NetworkAttachments is what the run's namespace offers.
type NetworkAttachments struct {
	// State is `Served`, `NotServed` for a cluster that does not register the
	// kind, or `Unread` for a reading that failed, in the spelling
	// inventory.NetworkAttachmentsState uses.
	State string `json:"state"`

	// Detail is why the reading failed.
	Detail string `json:"detail,omitempty"`

	// Namespace is the namespace that was listed.
	Namespace string `json:"namespace,omitempty"`

	// Definitions are the namespace's definitions, ascending by name.
	Definitions []NetworkAttachment `json:"definitions,omitempty"`
}

// NetworkAttachment is one NetworkAttachmentDefinition and what its CNI
// configuration says about the host.
type NetworkAttachment struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`

	// Type is the CNI plugin that creates the interface: ipvlan, macvlan,
	// host-device, bridge, or sriov.
	Type string `json:"type,omitempty"`

	// HostInterface is the host interface the plugin builds on, and PCIAddress
	// the slot a host-device names instead. Both are empty for a plugin whose
	// device comes from ResourceName.
	HostInterface string `json:"hostInterface,omitempty"`
	PCIAddress    string `json:"pciAddress,omitempty"`

	// Mode is the ipvlan or macvlan mode, and IPAM the address plugin.
	Mode string `json:"mode,omitempty"`
	IPAM string `json:"ipam,omitempty"`

	// ResourceName is the device plugin resource the definition asks the
	// scheduler for.
	ResourceName string `json:"resourceName,omitempty"`

	// Problem says why nothing could be read from the configuration.
	Problem string `json:"problem,omitempty"`
}

// NetworkAttachment returns the definition of that name, when the report
// carries one.
func (r Report) NetworkAttachment(name string) (NetworkAttachment, bool) {
	if r.NetworkAttachments == nil {
		return NetworkAttachment{}, false
	}
	for _, definition := range r.NetworkAttachments.Definitions {
		if definition.Name == name {
			return definition, true
		}
	}
	return NetworkAttachment{}, false
}

// Device is one block device and whether it may be handed to a storage cluster.
type Device struct {
	// Name is the kernel name and Path is the /dev path. A logical
	// block-device deployment names a device by its path.
	Name string `json:"name"`
	Path string `json:"path"`

	// StablePath is the persistent /dev/disk name udev publishes for the
	// device, and is what a logical block-device deployment names it by.
	//
	// Path is not. It is a position in this boot's enumeration order, so a
	// machine that comes back with its controllers probed in another order
	// hands /dev/sdb to a different disk, and a deployment that recorded it
	// would select that other disk on the next configure. This is read off the
	// device itself (its WWN, or the identifier in its own partition table),
	// and outlives the reboot.
	//
	// Empty for a device udev published no link for, which is an answer rather
	// than an omission: such a device has no persistent name, and a reader has
	// to be able to tell that apart from one this probe did not look up.
	StablePath string `json:"stablePath,omitempty"`

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

	// AtomicWriteUnitMaxBytes is the largest write this device completes whole
	// across a power failure, and AtomicWriteUnitMinBytes the smallest it makes
	// that promise about. They are what says whether a cluster may run checksum
	// validation on a device whose logical block size is under the data plane's
	// minimum, which is otherwise a claim somebody has to make by hand.
	//
	// Absent rather than zero when the kernel does not publish them, which it
	// did not before 6.11. The distinction is the point: a device that says it
	// guarantees nothing and a kernel that was never able to ask are different
	// findings, and only the first is evidence.
	AtomicWriteUnitMaxBytes *uint32 `json:"atomicWriteUnitMaxBytes,omitempty"`
	AtomicWriteUnitMinBytes *uint32 `json:"atomicWriteUnitMinBytes,omitempty"`

	// NUMANode is the memory node the device hangs off, or NUMANodeUnknown.
	NUMANode int `json:"numaNode"`

	// SubsystemNQN is the NVMe Qualified Name of the subsystem the namespace
	// belongs to, and is empty for a device on any other bus.
	//
	// It is what tells a volume this product exported from a disk the fleet
	// owns. Both are namespaces, both are presented as disks, and the transport
	// separates them only by inference: what an NQN says is which cluster and
	// which logical volume the bytes belong to, which is the answer a draft
	// needs before it proposes handing them to a cluster.
	SubsystemNQN string `json:"subsystemNQN,omitempty"`

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
	//
	// All four states matter and none of them is derivable from the others,
	// which is why the driver is reported rather than a flag saying whether it
	// is a userspace one. uio_pci_generic and vfio-pci in particular differ in
	// what they suggest about who bound it: the second is also how a disk is
	// passed through to a guest.
	Driver string `json:"driver,omitempty"`

	// Vendor and Product are the raw PCI identifiers, which is what sysfs has:
	// resolving them to names needs a database the probe does not carry.
	Vendor  string `json:"vendor,omitempty"`
	Product string `json:"product,omitempty"`

	// NUMANode is the memory node it hangs off, or NUMANodeUnknown.
	NUMANode int `json:"numaNode"`

	// InUse reports whether anything holds the controller open, and is absent
	// for a controller the probe could not check.
	//
	// It is the question Driver cannot answer and the one that decides whether
	// a controller can be reclaimed: a userspace binding nothing is driving is
	// a leftover, and the same binding with a process behind it is a disk in
	// service, which may belong to a hypervisor guest or another product rather
	// than to this one.
	//
	// The third state is why this is a pointer. A controller nothing holds and
	// a controller whose process table could not be read are different answers,
	// and only the first is one a draft may act on: reading them as one value
	// is how a disk something is driving gets proposed as free. Read it through
	// Held and Free, which are the two questions a reader has and neither of
	// which is the negation of the other.
	InUse *bool `json:"inUse,omitempty"`

	// LBAFormats is the block sizes each of the controller's namespaces can be
	// formatted with, and the one each is formatted with. It is empty for a
	// controller the kernel does not drive, which cannot be asked, and for one
	// that did not answer, whose failure is in Unreadable.
	//
	// It is additive within version 10: no reader acts on it, so a report that
	// lacks it is one that says nothing about formats, not one that says
	// something wrong.
	LBAFormats []NamespaceFormats `json:"lbaFormats,omitempty"`
}

// NamespaceFormats is the LBA formats one namespace of a controller supports.
type NamespaceFormats struct {
	// NSID is the namespace the formats were read from. It is absent when
	// Common is set.
	NSID uint32 `json:"nsid,omitempty"`

	// Common reports that the controller had no namespace attached and these
	// are the formats a namespace created on it could take.
	Common bool `json:"common,omitempty"`

	// Formats is every format the controller supports, ascending by index.
	Formats []LBAFormat `json:"formats"`

	// Current is the index of the format the namespace is formatted with, and
	// is absent for Common.
	Current *int `json:"current,omitempty"`

	// ExtendedMetadata reports that the metadata is transferred at the end of
	// each block rather than in a separate buffer.
	ExtendedMetadata bool `json:"extendedMetadata,omitempty"`
}

// LBAFormat is one block size and metadata size combination.
type LBAFormat struct {
	// Index is the number a Format NVM command takes.
	Index int `json:"index"`

	// DataSize is one logical block, in bytes, and MetadataSize the metadata
	// carried with it.
	DataSize     uint32 `json:"dataSize"`
	MetadataSize uint16 `json:"metadataSize"`

	// RelativePerformance is the controller's ranking: best, better, good, or
	// degraded.
	RelativePerformance string `json:"relativePerformance"`
}

// Held reports whether something is known to hold the controller open.
func (c Controller) Held() bool { return c.InUse != nil && *c.InUse }

// Free reports whether the probe checked the controller and found nothing
// holding it, which is the only state a draft may claim it in.
func (c Controller) Free() bool { return c.InUse != nil && !*c.InUse }

// BoundToUserspace reports whether a userspace-IO driver owns the controller.
//
// It reads the driver and says nothing about who bound it or whether anything
// is still driving it. InUse answers the second, and nothing answers the first,
// because a binding carries no record of what made it.
// HasDriver reports whether anything at all is bound to the controller.
//
// The state it distinguishes is the one a failed claim leaves: taking an NVMe
// controller for a userspace driver unbinds it from the kernel first, so a run
// that stops in between leaves a controller with no driver, no namespaces and no
// block device. It is neither the kernel's nor a userspace driver's, and asking
// who holds it is asking about a character device no driver has created.
func (c Controller) HasDriver() bool { return c.Driver != "" }

func (c Controller) BoundToUserspace() bool {
	return c.Driver == pci.DriverUIOGeneric || c.Driver == pci.DriverVFIO
}

// ControllersBoundToUserspace is the controllers no block device corresponds
// to, which is the answer to why a worker full of disks reported none.
//
// The classification lives here rather than at every call site because which
// drivers are userspace drivers is something this product knows and a string
// comparison spread across consumers would drift.
func (r Report) ControllersBoundToUserspace() []Controller {
	var taken []Controller
	for _, controller := range r.NVMeControllers {
		if controller.BoundToUserspace() {
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
