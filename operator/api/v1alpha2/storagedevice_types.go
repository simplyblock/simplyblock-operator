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
// +kubebuilder:validation:Enum=Online;Degraded;Unknown;Removed;Failed
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
