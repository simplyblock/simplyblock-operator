// StorageDeviceMetrics: how much of a storage device is in use, as the control
// plane's exporter last measured it.
//
// The kind exists because a StorageDevice cannot carry the number. A device is
// physical, so its size is immutable and belongs in the object's status, while
// what it holds moves continuously: writing that to a custom resource would put
// one etcd write and one wake-up of every watcher of the kind behind every
// sample, for a reading nothing reconciles toward. So it is computed from
// Prometheus when a client asks and never persisted, which is the trade
// metrics.k8s.io makes for PodMetrics and the one LogicalVolumeMetrics next door
// already makes for volumes.
//
// The consequences are the same as that kind's and are load-bearing rather than
// incidental:
//
//   - There is no spec and no status. The fields sit at the top level of the
//     object, because neither half of the split means anything for a reading
//     nobody wrote.
//   - An object exists only while a StorageDevice does. There is no deletion and
//     no tombstone: a device that goes away stops being listed.
//   - A deployment with no reachable Prometheus serves no readings at all,
//     rather than readings of zero. A zero is the reading of an empty device and
//     not the absence of a reading.
//
// design-storagedevice.md §4.2 and §8 are the specification.

package v1alpha2

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageDeviceCapacity is what one device holds. Every size is in bytes and is
// quoted as a resource.Quantity so that kubectl prints it the way it prints a
// PersistentVolumeClaim's capacity.
//
// Used against Total is the pair the type exists for: cluster capacity is the
// sum of its devices, and a cluster at seventy per cent with one device at
// ninety-eight is a cluster about to have a problem that its own thresholds
// cannot see.
//
// +k8s:openapi-gen=true
type StorageDeviceCapacity struct {
	// Total is the space the device can hold, as the exporter measured it. It is
	// the same number the device object's status.capacity carries, reported
	// again here so that a reading is complete without a second lookup.
	Total resource.Quantity `json:"total"`
	// Used is the space the device currently holds.
	Used resource.Quantity `json:"used"`
	// Free is the device's unallocated remainder as the control plane accounts
	// for it. It is reported rather than derived, so it need not equal Total
	// minus Used.
	Free resource.Quantity `json:"free"`
	// Provisioned is the space promised out of the device, which on a
	// thin-provisioned pool may exceed Total.
	Provisioned resource.Quantity `json:"provisioned"`
	// UtilizationPercent is the control plane's own utilization figure, from 0
	// to 100. It is taken verbatim rather than recomputed from Used and Total,
	// so that it agrees with what the control plane's own interfaces report.
	UtilizationPercent int32 `json:"utilizationPercent"`
}

// +kubebuilder:object:root=true

// StorageDeviceMetrics is one storage device's capacity reading.
//
// The object is named after the StorageDevice it measures and lives in that
// object's namespace, so somebody who has the device's name needs to learn
// nothing else to ask for it, and ordinary namespaced RBAC confines a reader to
// the namespaces they already have. A device with no StorageDevice object is
// therefore not listed: it has no name in this API and no namespace to be
// authorized against.
//
// +k8s:openapi-gen=true
type StorageDeviceMetrics struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata. Name and namespace are the
	// StorageDevice's. The creationTimestamp is the device object's rather
	// than the reading's.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Timestamp is when the control plane sampled these values, which is older
	// than the moment the request was served and may be considerably older if
	// its exporter has stopped being scraped. It is the zero time when the
	// device has never been sampled, so that "never measured" does not read as
	// "measured in 1970."
	Timestamp metav1.Time `json:"timestamp"`

	// DeviceID is the control plane's identifier for the device. It is the join
	// key back to the control plane's own exporter and to its API.
	DeviceID string `json:"deviceID"`

	// StorageNode is the name of the StorageNode object the device belongs to,
	// so a reading says which machine it is about without a second lookup.
	StorageNode string `json:"storageNode"`

	// Capacity is the reading itself.
	Capacity StorageDeviceCapacity `json:"capacity"`
}

// +kubebuilder:object:root=true

// StorageDeviceMetricsList is a list of readings. It carries no continue token:
// the whole set is served from memory in one pass, so there is nothing to page
// through.
//
// +k8s:openapi-gen=true
type StorageDeviceMetricsList struct {
	metav1.TypeMeta `json:",inline"`
	// The tag is omitempty rather than the omitzero the CRD kinds in this
	// repository use, because openapi-gen enforces the streaming-list convention
	// on a type it generates definitions for and that convention names
	// omitempty.
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageDeviceMetrics `json:"items"`
}
