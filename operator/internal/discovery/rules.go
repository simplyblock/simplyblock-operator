// The two rule seams: which devices may reach a draft, and which workers take
// part.
//
// A rule refuses with a reason rather than returning a boolean, because every
// refusal here is something an administrator may disagree with. The disk they
// expected is missing from the draft, and the run has to be able to say it was
// the size range, or the deny list, or that the kernel would not hand it over.
//
// The rules are separate from the probe's own rejections and sit after them. A
// probe decides whether a device is free at all, which is a fact about the
// machine; a rule decides whether a free device is one this deployment wants,
// which is the run's own configuration.

package discovery

import (
	"fmt"
	"strings"

	"github.com/simplyblock/atlas/blockdev"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// DeviceRule decides whether one reported device may go into a draft.
//
// It is given the whole report as well as the device, because a rule may need
// to know something about the machine the device is in: today none does, and
// the first one that needs the worker's other disks or its NUMA layout should
// not have to change the interface to get them.
type DeviceRule interface {
	// Name identifies the rule in a refusal, so a reader knows which rule
	// declined a device.
	Name() string

	// Admit reports whether the device may be used, and why not when it may
	// not. The reason is a sentence fragment completing a declined-because
	// sentence.
	Admit(report nodeprobe.Report, device nodeprobe.Device) (bool, string)
}

// PreFilter marks a device rule that decides whether a device was ever a
// candidate. Refusing a loopback device for not being a whole disk is true and
// says nothing about why a run found no storage; refusing a disk because
// something else is using it is the answer.
//
// It is an optional interface rather than a method on DeviceRule because a rule
// that does not say is the common case and should not have to.
type PreFilter interface {
	PreFilter() bool
}

// WorkerRule decides whether a worker takes part in the deployment.
type WorkerRule interface {
	Name() string

	// Admit is given the worker's report and the devices that survived the
	// device rules, because whether a worker is worth including is mostly a
	// question about what is left of it.
	Admit(report nodeprobe.Report, admitted []nodeprobe.Device) (bool, string)
}

// Refusal is one rule declining one thing, kept so that the run can report it.
type Refusal struct {
	// Worker is the node the refusal is about.
	Worker string

	// Device is the device declined, empty when a whole worker was.
	Device string

	// Rule is the rule that declined it, and Reason is why.
	Rule   string
	Reason string

	// PreFilter says the rule answers whether the thing was ever a candidate,
	// rather than why a candidate was not taken. A machine presents dozens of
	// loopback and network block devices and one disk somebody cares about, and
	// a report that treats the two alike buries the second under the first.
	PreFilter bool
}

// String renders a refusal for an event or a status message.
func (r Refusal) String() string {
	if r.Device == "" {
		return fmt.Sprintf("%s: declined by %s because %s", r.Worker, r.Rule, r.Reason)
	}
	return fmt.Sprintf("%s/%s: declined by %s because %s", r.Worker, r.Device, r.Rule, r.Reason)
}

// DeviceClass is which of the two classes of backend storage a run is scanning.
//
// A cluster is built out of one class, so this selects rather than filters: a
// run scans NVMe or it scans logical block devices, and the draft it writes
// names devices in that class's own vocabulary.
type DeviceClass string

const (
	// ClassNVMe names devices by PCI address, which is what a NodeGroup's NVMe
	// device selection carries.
	ClassNVMe DeviceClass = "nvme"

	// ClassBlock names devices by path, which is what a NodeGroup's block
	// selection carries.
	ClassBlock DeviceClass = "block"
)

// ClassOf reads which class a run is scanning out of its filter. Absent, or a
// filter that does not ask for block devices, is NVMe: that is what every
// deployment before the logical block-device class existed was built out of, so
// it is what a run that says nothing keeps reporting.
func ClassOf(filter *simplyblockv1alpha2.DeviceFilter) DeviceClass {
	if filter != nil && filter.EnableLogicalBlockDevices != nil && *filter.EnableLogicalBlockDevices {
		return ClassBlock
	}
	return ClassNVMe
}

// Address is how the draft names a device of this class: its PCI address for
// NVMe, its path for a logical block device. It is empty when the device cannot
// be named in the class at all, which is what AdmitClass refuses on.
func (c DeviceClass) Address(device nodeprobe.Device) string {
	if c == ClassBlock {
		return device.Path
	}
	return device.PCIAddress
}

// AvailableRule admits a device the probe found free.
//
// EnablePartitionedDevices is the one waiver, and it is narrow on purpose: a
// device whose only refusal is its partition table is admitted, and a device
// that is also mounted, held, or unreadable is not. That is what
// blockdev.Candidate.OnlyRejectedFor guarantees on the probe's side and what
// this rule spends here.
type AvailableRule struct {
	// AllowPartitioned waives a partition table, for the administrator who
	// knows the table is stale.
	AllowPartitioned bool
}

func (AvailableRule) Name() string { return "available" }

func (r AvailableRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	if device.Available {
		return true, ""
	}
	if r.AllowPartitioned && device.OnlyRejectedFor(string(blockdev.ReasonPartitioned)) {
		return true, ""
	}

	reasons := make([]string, 0, len(device.Rejections))
	for _, rejection := range device.Rejections {
		reasons = append(reasons, rejection.Reason)
	}
	if len(reasons) == 0 {
		return false, "the probe did not report it as available and gave no reason"
	}
	return false, "the probe refused it: " + strings.Join(reasons, ", ")
}

// ClassRule admits a device that can be named in the class the run is scanning,
// and refuses one the class cannot take.
//
// It is a rule rather than a precondition because the failure is worth
// reporting: an NVMe run against a worker whose disks are virtio finds devices
// it cannot name, and "no NVMe devices" is a more useful answer than an empty
// draft.
//
// The two sides are not symmetric, because the classes are not two disjoint
// sets of hardware. The NVMe class is the narrow one: SPDK binds a controller
// through vfio-pci, so only a device on the NVMe bus and carrying a PCI address
// qualifies, and a virtio disk is refused. The block class is the wide one: it
// reaches a device through the kernel, where a local NVMe namespace at
// /dev/nvme0n1 is a block device like any other, so it is admitted. One machine
// can therefore be deployed either way, and which way is the administrator's
// statement through EnableLogicalBlockDevices rather than something the bus
// decides for them.
//
// A fabric namespace is the one bus the block class still refuses. It is a
// volume something else exported rather than a disk the machine has, so it
// belongs to neither class, and refusing it here keeps a block run's answer
// about what the device is rather than about what is holding it.
type ClassRule struct {
	Class DeviceClass
}

func (ClassRule) Name() string { return "device class" }

// PreFilter: a device of another class is not one this run was scanning for, so
// saying so explains nothing about the storage the fleet has.
func (ClassRule) PreFilter() bool { return true }

func (r ClassRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	if r.Class == ClassNVMe && device.Transport != string(blockdev.TransportNVMe) {
		return false, fmt.Sprintf("this run scans NVMe devices and the device is on %s",
			transportOrNone(device.Transport))
	}
	if r.Class == ClassBlock && device.Transport == string(blockdev.TransportNVMeFabric) {
		return false, fmt.Sprintf("this run scans logical block devices and the device is on %s, "+
			"which is a volume something else exported rather than a disk this machine has",
			transportOrNone(device.Transport))
	}
	if address := r.Class.Address(device); address == "" {
		return false, fmt.Sprintf("it has no %s to name it by", addressKind(r.Class))
	}
	return true, ""
}

// transportOrNone names a transport for a message, including the empty one.
func transportOrNone(transport string) string {
	if transport == "" {
		return "no bus this scan could name"
	}
	return transport
}

// addressKind is what a device of a class is named by, for a refusal.
func addressKind(class DeviceClass) string {
	if class == ClassBlock {
		return "device path"
	}
	return "PCI address"
}

// WholeDiskRule admits a whole disk, and a partition where the class can take
// one.
//
// Which class can take one follows from how the device is reached. SPDK binds an
// NVMe controller through vfio-pci and is handed the whole device, so a
// partition of one was never something a run could propose: admitting it would
// hand a cluster a partition as though it were a disk, which is the silent
// failure this rule exists for. A logical block device is reached through the
// kernel, where a partition is an ordinary block device and the backend takes
// one — the journal share is documented for the case where the smallest device
// selected is a partition.
//
// Nothing else is admitted in either class. A loopback device and a
// device-mapper node are no more candidates for the block class than for NVMe,
// so the relaxation is the partition and not the rule.
type WholeDiskRule struct {
	// Class is what the run is scanning, which decides whether a partition is a
	// device this deployment could be handed at all.
	Class DeviceClass
}

func (WholeDiskRule) Name() string { return "whole disk" }

// PreFilter: a device this class could never have taken is not an answer to
// why a fleet proposed no storage, so refusing it explains nothing.
func (WholeDiskRule) PreFilter() bool { return true }

func (r WholeDiskRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	if device.Kind == string(blockdev.KindDisk) {
		return true, ""
	}
	if r.Class == ClassBlock && device.Kind == string(blockdev.KindPartition) {
		return true, ""
	}
	return false, fmt.Sprintf("it is a %s rather than a whole disk", device.Kind)
}

// AllowDenyRule admits a device whose address is in the allow list, when there
// is one, and not in the deny list.
//
// One rule covers both classes because the shape is identical and only the
// address differs; which lists it was given is the caller's business.
type AllowDenyRule struct {
	Class DeviceClass
	Allow []string
	Deny  []string
}

func (AllowDenyRule) Name() string { return "allow and deny lists" }

// The reason names the list and not the address. Which device was refused is
// already on the refusal, and putting it in the sentence too made every such
// refusal a different sentence, so a hundred disks declined by one list read as
// a hundred separate findings rather than one list and a number.

func (r AllowDenyRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	address := r.Class.Address(device)

	for _, denied := range r.Deny {
		if strings.EqualFold(address, denied) {
			return false, "it is in the deny list"
		}
	}
	if len(r.Allow) == 0 {
		return true, ""
	}
	for _, allowed := range r.Allow {
		if strings.EqualFold(address, allowed) {
			return true, ""
		}
	}
	return false, "it is not in the allow list"
}

// ModelRule admits a device whose model string contains the wanted text.
//
// A substring and not an exact match, because a model string is padded,
// versioned, and vendor-formatted: "SAMSUNG MZQL23T8HCLS-00A07" is what sysfs
// reports and "MZQL2" is what an administrator would write.
type ModelRule struct {
	Model string
}

func (ModelRule) Name() string { return "model" }

func (r ModelRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	if r.Model == "" {
		return true, ""
	}
	if strings.Contains(strings.ToLower(device.Model), strings.ToLower(r.Model)) {
		return true, ""
	}
	return false, fmt.Sprintf("its model %q does not contain %q", device.Model, r.Model)
}

// SizeRule admits a device whose size falls inside a range.
type SizeRule struct {
	// Min and Max are inclusive bounds in bytes. A zero Max is no upper bound.
	Min, Max uint64

	// Spec is the range as the filter wrote it, which is what a refusal quotes.
	//
	// The bound is not re-rendered from the parsed number, because that is a
	// different string: a filter naming 1920G would be quoted back as 1.875T,
	// and a reviewer comparing the refusal against what they wrote would be
	// comparing two spellings of one number. What they can act on is what they
	// typed.
	Spec string
}

func (SizeRule) Name() string { return "size range" }

func (r SizeRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	if device.SizeBytes < r.Min {
		return false, fmt.Sprintf("it is %s and the range %s starts above it",
			humanBytes(device.SizeBytes), r.describeRange(r.Min))
	}
	if r.Max > 0 && device.SizeBytes > r.Max {
		return false, fmt.Sprintf("it is %s and the range %s ends below it",
			humanBytes(device.SizeBytes), r.describeRange(r.Max))
	}
	return true, ""
}

// describeRange is the range as the filter wrote it, and the bound rendered
// when the rule was built by hand rather than from a filter.
func (r SizeRule) describeRange(bound uint64) string {
	if r.Spec != "" {
		return r.Spec
	}
	return humanBytes(bound)
}

// WorkerHasDevices admits a worker that has at least one device left.
//
// It is the only worker rule, and it is the one that cannot be omitted: a
// worker in a draft with no devices expands into a storage node with nothing to
// store on.
type WorkerHasDevices struct{}

func (WorkerHasDevices) Name() string { return "has devices" }

func (WorkerHasDevices) Admit(report nodeprobe.Report, admitted []nodeprobe.Device) (bool, string) {
	if len(admitted) > 0 {
		return true, ""
	}

	// A worker whose disks are on a userspace driver has no block devices at
	// all, so "no device survived the rules" is true and useless: the machine
	// is full of disks that something else has. Saying which controllers and
	// which driver is the difference between a reviewer concluding the machine
	// has no storage and knowing what is on it.
	if bound := report.ControllersBoundToUserspace(); len(bound) > 0 {
		// Whether anything is driving them is the half that decides what to do
		// next, and the two answers ask for opposite things. Nothing holding
		// them means the binding is a leftover and the disks can be taken back.
		// Something holding them means the machine is serving whatever that is,
		// which need not be this product: a userspace binding is also how a
		// disk is passed through to a guest.
		var busy, unchecked []nodeprobe.Controller
		for _, controller := range bound {
			switch {
			case controller.Held():
				busy = append(busy, controller)
			case !controller.Free():
				unchecked = append(unchecked, controller)
			}
		}
		if len(busy) == 0 && len(unchecked) > 0 {
			return false, fmt.Sprintf(
				"it presents no usable block device, and whether anything is driving %d of its "+
					"NVMe controllers (%s) could not be established, so they are not offered",
				len(unchecked), describeControllers(unchecked))
		}
		if len(busy) > 0 {
			return false, fmt.Sprintf(
				"it presents no usable block device, and %d of its NVMe controllers (%s) are "+
					"bound to a userspace driver and in use, so something is driving its disks",
				len(busy), describeControllers(busy))
		}
		return false, fmt.Sprintf(
			"it presents no usable block device, and %d of its NVMe controllers (%s) are "+
				"bound to a userspace driver and nothing is using them, so the disks are "+
				"there to be reclaimed",
			len(bound), describeControllers(bound))
	}
	return false, "no device of it survived the device rules"
}

// describeControllers names the controllers and the driver holding them, which
// is what a reviewer needs to decide whether to reclaim them.
func describeControllers(controllers []nodeprobe.Controller) string {
	parts := make([]string, 0, len(controllers))
	for _, controller := range controllers {
		parts = append(parts, fmt.Sprintf("%s on %s", controller.Address, controller.Driver))
	}
	return strings.Join(parts, ", ")
}

// WorkerWasReadable admits a worker whose probe read everything it went for.
//
// It is off by default. A worker whose CPU tree could not be read still has
// disks worth reviewing, so refusing it would lose more than it protects — but
// a fleet that wants only fully readable machines in its draft can say so.
type WorkerWasReadable struct{}

func (WorkerWasReadable) Name() string { return "fully readable" }

func (WorkerWasReadable) Admit(report nodeprobe.Report, _ []nodeprobe.Device) (bool, string) {
	if len(report.Unreadable) == 0 {
		return true, ""
	}
	return false, fmt.Sprintf("the probe could not read %d of its readings: %s",
		len(report.Unreadable), strings.Join(report.Unreadable, "; "))
}

// humanBytes renders a size the way an administrator writes one.
//
// Two properties, and the old rendering had neither. A whole number of units is
// written as that whole number, so the sizes a fleet is actually built out of
// read as 3T rather than as 3.001T and parse back to the byte they came from.
// Anything else is truncated rather than rounded, because a size that reads as
// more than the device holds is one that says the disk is bigger than it is:
// the old rendering printed a byte under a tebibyte as 1024G, which is not a
// approximation of the value, it is above it.
func humanBytes(bytes uint64) string {
	for _, unit := range []struct {
		size   uint64
		suffix string
	}{
		{uint64(1) << 40, "T"},
		{uint64(1) << 30, "G"},
		{uint64(1) << 20, "M"},
	} {
		if bytes < unit.size {
			continue
		}
		if bytes%unit.size == 0 {
			return fmt.Sprintf("%d%s", bytes/unit.size, unit.suffix)
		}
		// Truncated to two decimals, which is the precision a disk is sold in
		// and never more than the device holds. A trailing zero is dropped, so
		// a size and a half reads as 1.5T rather than 1.50T.
		hundredths := (bytes % unit.size) * 100 / unit.size
		decimals := fmt.Sprintf("%02d", hundredths)
		decimals = strings.TrimRight(decimals, "0")
		if decimals == "" {
			return fmt.Sprintf("%d%s", bytes/unit.size, unit.suffix)
		}
		return fmt.Sprintf("%d.%s%s", bytes/unit.size, decimals, unit.suffix)
	}
	return fmt.Sprintf("%dB", bytes)
}
