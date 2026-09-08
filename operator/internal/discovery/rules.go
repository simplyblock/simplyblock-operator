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

// ClassRule admits a device that can be named in the class the run is scanning.
//
// It is a rule rather than a precondition because the failure is worth
// reporting: an NVMe run against a worker whose disks are virtio finds devices
// it cannot name, and "no NVMe devices" is a more useful answer than an empty
// draft.
type ClassRule struct {
	Class DeviceClass
}

func (ClassRule) Name() string { return "device class" }

func (r ClassRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	if r.Class == ClassNVMe && device.Transport != string(blockdev.TransportNVMe) {
		return false, fmt.Sprintf("this run scans NVMe devices and the device is on %s",
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

// WholeDiskRule admits only a whole disk.
//
// The probe already refuses everything else, so this is belt and braces — but
// it is the one rule whose absence would be silent: a partition admitted by a
// waiver would be handed to a cluster as though it were a disk.
type WholeDiskRule struct{}

func (WholeDiskRule) Name() string { return "whole disk" }

func (WholeDiskRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	if device.Kind != string(blockdev.KindDisk) {
		return false, fmt.Sprintf("it is a %s rather than a whole disk", device.Kind)
	}
	return true, ""
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

func (r AllowDenyRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	address := r.Class.Address(device)

	for _, denied := range r.Deny {
		if strings.EqualFold(address, denied) {
			return false, fmt.Sprintf("%s is in the deny list", address)
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
	return false, fmt.Sprintf("%s is not in the allow list", address)
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
}

func (SizeRule) Name() string { return "size range" }

func (r SizeRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	if device.SizeBytes < r.Min {
		return false, fmt.Sprintf("it is %s and the range starts at %s",
			humanBytes(device.SizeBytes), humanBytes(r.Min))
	}
	if r.Max > 0 && device.SizeBytes > r.Max {
		return false, fmt.Sprintf("it is %s and the range ends at %s",
			humanBytes(device.SizeBytes), humanBytes(r.Max))
	}
	return true, ""
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
	// is full of disks that something else is already driving. Saying which
	// controllers and which driver is the difference between a reviewer
	// concluding the machine has no storage and knowing to reclaim it.
	if taken := report.ControllersTakenByUserspace(); len(taken) > 0 {
		return false, fmt.Sprintf(
			"it presents no usable block device, and %d of its NVMe controllers (%s) are "+
				"held by a userspace driver, so the kernel presents no disk for them",
			len(taken), describeControllers(taken))
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

// humanBytes renders a size the way an administrator writes one, so that a
// refusal quotes the same units the filter was written in.
func humanBytes(bytes uint64) string {
	switch {
	case bytes >= 1<<40:
		return fmt.Sprintf("%.4gT", float64(bytes)/float64(uint64(1)<<40))
	case bytes >= 1<<30:
		return fmt.Sprintf("%.4gG", float64(bytes)/float64(uint64(1)<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.4gM", float64(bytes)/float64(uint64(1)<<20))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}
