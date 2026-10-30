// StorageDevice is one storage backend device belonging to one storage node,
// expressed as a Kubernetes resource. Objects are discovered rather than
// declared: the operator creates them from what the control plane reports on the
// per-node device stream, and a user never writes one. The kind is the bottom of
// the ownership spine.
//
// The kind is declared here rather than in v1alpha1 because it is new: nothing
// ever shipped a v1alpha1 spelling of it, so there is no older representation to
// convert from and it starts at the group's current version. That is why this
// file carries no conversion and why the type implements no hub interface.
//
// The type follows design-storagedevice.md Appendix A. Where it departs from the
// appendix, the reason is that the control plane does not report the field, and
// each departure is commented at the field.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageDeviceShortIDLength is how much of the control-plane device UUID goes
// into an object's name: the first hyphen-delimited group, which is short enough
// to keep the name readable and wide enough that two devices on one node cannot
// collide in practice.
const StorageDeviceShortIDLength = 8

// StorageDeviceName returns the object name mirroring one control-plane device:
// the owning StorageNode's name and the device's short id. Naming a device after
// its node keeps the two sorted together in a listing, which is what somebody
// scanning for a node's devices actually does.
func StorageDeviceName(nodeName, deviceID string) string {
	short := deviceID
	if len(short) > StorageDeviceShortIDLength {
		short = short[:StorageDeviceShortIDLength]
	}
	return nodeName + "-" + short
}

// The labels every StorageDevice object carries. They duplicate what is already
// reachable by following the object's owner, and they are what make the kind
// usable in an incident: somebody holding a failed drive knows which machine
// they pulled it from and nothing else, so `kubectl get sd -l
// storage.simplyblock.io/worker=worker-3` has to be the question they can ask.
//
// The keys are the ones the rest of the API group already uses for the same
// three things, so a selector written for one kind selects the same scope on
// this one.
const (
	// DeviceLabelCluster is the StorageCluster the device's node belongs to,
	// named by its Kubernetes object rather than by the backend id in
	// status.clusterID.
	DeviceLabelCluster = "storage.simplyblock.io/cluster"
	// DeviceLabelNode is the owning StorageNode's object name. It is also the
	// first half of the object's own name, and a label as well because a name
	// prefix is not something a selector can match.
	DeviceLabelNode = "storage.simplyblock.io/node"
	// DeviceLabelWorker is the Kubernetes node the device is physically in,
	// copied from the StorageNode's own label of the same name.
	DeviceLabelWorker = "storage.simplyblock.io/worker"
)

// StorageDevicePhase is the operator's own view of a device. Degraded and Failed
// are deliberately distinct: a degraded device is serving and should not be,
// while a failed one is not serving and the cluster is running with less
// redundancy than it thinks until it is replaced.
// +kubebuilder:validation:Enum=Online;Degraded;Unknown;Removed;Failed;Migrated
type StorageDevicePhase string

const (
	StorageDevicePhaseOnline   StorageDevicePhase = "Online"
	StorageDevicePhaseDegraded StorageDevicePhase = "Degraded"
	// StorageDevicePhaseUnknown is a device whose node cannot be reached, so its
	// state is not observable rather than bad. A terminal phase is not
	// overwritten by it.
	StorageDevicePhaseUnknown StorageDevicePhase = "Unknown"
	StorageDevicePhaseRemoved StorageDevicePhase = "Removed"
	StorageDevicePhaseFailed  StorageDevicePhase = "Failed"
	// StorageDevicePhaseMigrated is a device taken out of service whose data the
	// control plane has rebuilt onto the node's peers, so the cluster carries no
	// less redundancy for it. It is where the devices of a removed node end up,
	// and like Failed and Removed it is terminal.
	StorageDevicePhaseMigrated StorageDevicePhase = "Migrated"
)

// StorageDeviceRole is what the device carries. It is decided when the node is
// created, by spec.storageNodes.enableJournalDevice, and it is what makes one
// device's failure worse than another's.
// +kubebuilder:validation:Enum=Storage;Journal
type StorageDeviceRole string

const (
	StorageDeviceRoleStorage StorageDeviceRole = "Storage"
	StorageDeviceRoleJournal StorageDeviceRole = "Journal"
)

// DeviceCapacity is how big the device is. It carries no used size and no
// sample time, because a physical device cannot be resized: the number is a
// property of the hardware, it is written when the device is discovered, and
// nothing rewrites it. A device reporting a different size is a different
// device, which arrives as its own object.
//
// What the device holds is the number that moves, and it is served from
// metrics.simplyblock.io as StorageDeviceMetrics rather than published here. A
// reading that changes continuously is not desired state, and keeping it in a
// status would write etcd on every sample and wake every watcher of the kind
// for it.
type DeviceCapacity struct {
	// TotalBytes is the device's usable size, as the control plane reports it
	// with the device.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`
}

// DeviceHardware identifies the part. These are what somebody walking into a
// datacenter with a failed drive needs, and none of them is recoverable from the
// control plane's device id alone. Every field is optional, because which ones a
// device has depends on how it is attached.
//
// The design's devicePath is absent: the control plane reports no host path for
// a device, only the NVMe controller it hangs off, so the field would never be
// populated. NVMeController carries what is actually reported.
type DeviceHardware struct {
	// PCIAddress is the device's address on the host ("0000:5e:00.0"), where it
	// has one. A logical block device may not: the address can belong to the
	// controller it hangs off rather than to the device.
	// +optional
	PCIAddress string `json:"pciAddress,omitempty"`

	// SerialNumber is what is printed on the drive.
	// +optional
	SerialNumber string `json:"serialNumber,omitempty"`

	// Model is the manufacturer's model string.
	// +optional
	Model string `json:"model,omitempty"`

	// NVMeController is the controller the device is served through, in the
	// control plane's spelling.
	// +optional
	NVMeController string `json:"nvmeController,omitempty"`
}

// DevicePerformanceVerdict is how the device performs against the other devices
// of its cluster that carry the same role.
// +kubebuilder:validation:Enum=Ok;Slow;Unknown
type DevicePerformanceVerdict string

const (
	// DevicePerformanceOk is a device serving its share of the cluster's I/O at a
	// latency in line with the rest of the fleet.
	DevicePerformanceOk DevicePerformanceVerdict = "Ok"
	// DevicePerformanceSlow is a device whose latency stands out from the fleet
	// by more than its load explains. Every write that lands on a slow device
	// waits for it, so one slow device sets the latency of the volumes it
	// carries.
	DevicePerformanceSlow DevicePerformanceVerdict = "Slow"
	// DevicePerformanceUnknown is a device that could not be compared: too
	// little I/O to measure, too few devices to compare against, or no metrics.
	DevicePerformanceUnknown DevicePerformanceVerdict = "Unknown"
)

// DeviceSMARTVerdict is what the device's own health log says about it.
// +kubebuilder:validation:Enum=Ok;Warning;Failing;Unsupported;Unknown
type DeviceSMARTVerdict string

const (
	// DeviceSMARTOk is a device whose health log reports nothing wrong.
	DeviceSMARTOk DeviceSMARTVerdict = "Ok"
	// DeviceSMARTWarning is a device that is serving and is wearing out or
	// running hot: worth planning a replacement for, not yet worth failing.
	DeviceSMARTWarning DeviceSMARTVerdict = "Warning"
	// DeviceSMARTFailing is a device whose health log reports that its
	// reliability is degraded, that it has gone read-only, or that its spare
	// capacity is exhausted. It is a replacement candidate now.
	DeviceSMARTFailing DeviceSMARTVerdict = "Failing"
	// DeviceSMARTUnsupported is a device that exposes no health log, such as a
	// virtual disk inside a virtual machine.
	DeviceSMARTUnsupported DeviceSMARTVerdict = "Unsupported"
	// DeviceSMARTUnknown is a device whose health log could not be read on the
	// last attempt.
	DeviceSMARTUnknown DeviceSMARTVerdict = "Unknown"
)

// DevicePerformance is the device's performance measured against its fleet:
// the devices of the same cluster with the same role. Each device is expected to
// carry I/O in proportion to its weight, which is its capacity, because that is
// how the cluster places data.
type DevicePerformance struct {
	// Verdict is the operator's judgment. It changes only after the condition
	// has held for several consecutive assessments, so a single burst does not
	// flip it.
	// +optional
	Verdict DevicePerformanceVerdict `json:"verdict,omitempty"`

	// Reason is the measurement behind the verdict, in one sentence.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Since is when the verdict last changed.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`

	// AssessedAt is when the device was last assessed.
	// +optional
	AssessedAt *metav1.Time `json:"assessedAt,omitempty"`

	// LatencyMicroseconds is the device's mean latency per I/O over the
	// assessment window.
	// +optional
	LatencyMicroseconds *int64 `json:"latencyMicroseconds,omitempty"`

	// FleetLatencyMicroseconds is the median latency per I/O of the fleet the
	// device is compared against.
	// +optional
	FleetLatencyMicroseconds *int64 `json:"fleetLatencyMicroseconds,omitempty"`

	// LatencyRatioPercent is the device's latency as a percentage of the fleet
	// median: 100 is exactly the median.
	// +optional
	LatencyRatioPercent *int32 `json:"latencyRatioPercent,omitempty"`

	// IOPSRatioPercent is the I/O the device serves as a percentage of the share
	// its weight entitles it to: 100 is exactly its share. A device far above
	// its share is expected to be slower, and one far below it while slower is
	// one that is not keeping up.
	// +optional
	IOPSRatioPercent *int32 `json:"iopsRatioPercent,omitempty"`
}

// DeviceSMART is the device's own health log, as last read, and the verdict
// drawn from it. Which fields are present depends on the device: an NVMe drive
// reports wear and spare capacity, and a SATA or SAS drive reports its overall
// self-assessment and its sector counts.
type DeviceSMART struct {
	// Verdict is the operator's judgment of the health log.
	// +optional
	Verdict DeviceSMARTVerdict `json:"verdict,omitempty"`

	// Reason names what in the health log decided the verdict.
	// +optional
	Reason string `json:"reason,omitempty"`

	// CheckedAt is when the health log was last read, successfully or not.
	// +optional
	CheckedAt *metav1.Time `json:"checkedAt,omitempty"`

	// Source is where the health log came from: `spdk` for a device the
	// storage engine drives directly, `nvme-cli` or `smartctl` for a device the
	// host kernel drives.
	// +optional
	Source string `json:"source,omitempty"`

	// CriticalWarning is the NVMe critical-warning bit field. Zero is healthy.
	// +optional
	CriticalWarning *int32 `json:"criticalWarning,omitempty"`

	// TemperatureCelsius is the device's composite temperature.
	// +optional
	TemperatureCelsius *int32 `json:"temperatureCelsius,omitempty"`

	// AvailableSparePercent is the spare capacity left, from 0 to 100.
	// +optional
	AvailableSparePercent *int32 `json:"availableSparePercent,omitempty"`

	// AvailableSpareThresholdPercent is the spare capacity below which the
	// device itself reports a critical warning.
	// +optional
	AvailableSpareThresholdPercent *int32 `json:"availableSpareThresholdPercent,omitempty"`

	// PercentageUsed is the device's own estimate of how much of its rated life
	// is used. It can exceed 100.
	// +optional
	PercentageUsed *int32 `json:"percentageUsed,omitempty"`

	// MediaErrors is how many unrecovered data-integrity errors the device has
	// reported over its life.
	// +optional
	MediaErrors *int64 `json:"mediaErrors,omitempty"`

	// PowerOnHours is how long the device has been powered.
	// +optional
	PowerOnHours *int64 `json:"powerOnHours,omitempty"`

	// UnsafeShutdowns is how often the device lost power without a clean
	// shutdown.
	// +optional
	UnsafeShutdowns *int64 `json:"unsafeShutdowns,omitempty"`

	// SMARTPassed is a SATA or SAS device's overall self-assessment. It is
	// absent on NVMe, which reports CriticalWarning instead.
	// +optional
	SMARTPassed *bool `json:"smartPassed,omitempty"`

	// ReallocatedSectors is how many sectors a SATA device has remapped.
	// +optional
	ReallocatedSectors *int64 `json:"reallocatedSectors,omitempty"`

	// PendingSectors is how many sectors a SATA device is waiting to remap.
	// +optional
	PendingSectors *int64 `json:"pendingSectors,omitempty"`
}

// DeviceHealth is what the operator has observed about how well the device
// works, as distinct from the state the control plane reports for it.
type DeviceHealth struct {
	// Performance is the device measured against its fleet.
	// +optional
	Performance *DevicePerformance `json:"performance,omitempty"`

	// SMART is the device's own health log. It is read on a schedule, and a
	// StorageDeviceOps with action CheckHealth reads it immediately.
	// +optional
	SMART *DeviceSMART `json:"smart,omitempty"`
}

// StorageDeviceSpec identifies the device this object reports on, and carries
// nothing else. Everything a user could change about a device is on the node, so
// a spec field here would be a second place to set the same thing.
type StorageDeviceSpec struct {
	// NodeRef names the StorageNode this device belongs to. The node owns this
	// object by controller reference, so deleting the node deletes its devices.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	NodeRef string `json:"nodeRef"`

	// DeviceID is the control plane's identifier for the device. With NodeRef it
	// is the whole of this object's identity.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	DeviceID string `json:"deviceID"`
}

// StorageDeviceStatus is everything observed about the device.
type StorageDeviceStatus struct {
	// Phase is the operator's own view of the device.
	// +optional
	Phase StorageDevicePhase `json:"phase,omitempty"`

	// DeviceStatus is the control plane's own string, in the control plane's
	// spelling, which is why it carries no Enum here.
	// +optional
	DeviceStatus string `json:"deviceStatus,omitempty"`

	// Role is what the device carries.
	// +optional
	Role StorageDeviceRole `json:"role,omitempty"`

	// Capacity is how big the device is. What it holds is served as
	// StorageDeviceMetrics instead.
	// +optional
	Capacity *DeviceCapacity `json:"capacity,omitempty"`

	// Hardware identifies the part.
	// +optional
	Hardware *DeviceHardware `json:"hardware,omitempty"`

	// ClusterID and NodeID are the backend ids of the stream this device was
	// last observed on. They are recorded because an object outliving its device
	// has to say which scope's absence would be authoritative before the mirror
	// may delete it; the object's own spec names Kubernetes objects rather than
	// backend ones, so it cannot answer that on its own.
	// +optional
	ClusterID string `json:"clusterID,omitempty"`
	// +optional
	NodeID string `json:"nodeID,omitempty"`

	// ActiveOpsRef names the StorageDeviceOps currently allowed to act on this
	// device, and is empty when none is. It is the operation lock every entity
	// of this group carries, and until StorageDeviceOps exists nothing takes it,
	// so the field is present and always empty. Declaring it now is what lets a
	// reader tell "no operation is running" from "this kind cannot say," and it
	// keeps the lock out of the change that introduces the operations.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// Health is how well the device works: its performance against the fleet
	// and its own health log. It is assessed by the operator rather than
	// reported by the control plane, and a device that is slow or failing
	// raises an alert on the control plane's alerts endpoint as well.
	// +optional
	Health *DeviceHealth `json:"health,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the device moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from. On this kind it moves at most once, since every spec field is fixed
	// when the object is created.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sd
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.nodeRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=".status.role"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.deviceStatus"
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=".status.capacity.totalBytes"
// +kubebuilder:printcolumn:name="Performance",type=string,JSONPath=".status.health.performance.verdict"
// +kubebuilder:printcolumn:name="SMART",type=string,JSONPath=".status.health.smart.verdict"
// +kubebuilder:printcolumn:name="PCI",type=string,JSONPath=".status.hardware.pciAddress",priority=1
// +kubebuilder:printcolumn:name="Serial",type=string,JSONPath=".status.hardware.serialNumber",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +operator-sdk:csv:customresourcedefinitions:displayName="Storage Device"

// StorageDevice is one storage backend device belonging to one storage node. It
// is the bottom of the ownership spine and the narrowest thing an operation can
// target.
//
// Objects are created by the operator from what the control plane reports and
// are never written by a user: the device's existence follows from the node
// having it, and which devices a node uses is decided in the node's own spec.
type StorageDevice struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec identifies the mirrored control-plane device
	// +required
	Spec StorageDeviceSpec `json:"spec"`

	// status defines the observed state of the device
	// +optional
	Status StorageDeviceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StorageDeviceList contains a list of StorageDevice.
type StorageDeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StorageDevice `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageDevice{}, &StorageDeviceList{})
}
