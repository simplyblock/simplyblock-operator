// StorageNodeMetrics: how much of a storage node is in use, as the control
// plane's exporter last measured it.
//
// The kind exists for the reason the four readings beside it do, and it sits
// between two of them. A device's reading is one drive and a cluster's is the
// whole fleet; a node's is the machine, which is the unit placement is decided
// against and the unit a drain moves volumes off. StorageNode.status.resources
// .capacity carries the same pair, and it carries it with hysteresis: it is
// written only when the figure has moved by a percent of the node's own total,
// because a status rewritten on every sample wakes every watcher of the kind for
// a number nothing reconciles toward (design-crd-model.md §7.13). This is the
// same measurement without that damping, computed when a client asks and never
// persisted.
//
// The consequences are the same as the other four kinds' and are load-bearing
// rather than incidental:
//
//   - There is no spec and no status. The fields sit at the top level of the
//     object, because neither half of the split means anything for a reading
//     nobody wrote.
//   - An object exists only while a StorageNode does. There is no deletion and
//     no tombstone: a node that goes away stops being listed.
//   - A deployment with no reachable Prometheus serves no readings at all,
//     rather than readings of zero. A zero is the reading of an empty node and
//     not the absence of a reading.
//
// Which device or which volume is filling a node up is deliberately not here.
// A node reports its own totals, and the breakdown behind them is
// StorageDeviceMetrics and LogicalVolumeMetrics, which is what keeps the
// cardinality of the workload out of this list.
//
// design-storagenode.md §3.3 and §12 are the specification.

package v1alpha2

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageNodeCapacity is what one storage node holds. Every size is in bytes and
// is quoted as a resource.Quantity so that kubectl prints it the way it prints a
// PersistentVolumeClaim's capacity.
//
// It carries the same fields a device's reading does, because a node's total is
// the sum of its devices' and a reader comparing the two should not have to
// reconcile different shapes.
//
// +k8s:openapi-gen=true
type StorageNodeCapacity struct {
	// Total is the space the node's devices provide, as the exporter measured
	// it.
	Total resource.Quantity `json:"total"`
	// Used is the space they currently hold.
	Used resource.Quantity `json:"used"`
	// Free is the node's unallocated remainder as the control plane accounts for
	// it. It is reported rather than derived, so it need not equal Total minus
	// Used.
	Free resource.Quantity `json:"free"`
	// Provisioned is the space promised out of the node, which on a
	// thin-provisioned pool may exceed Total.
	Provisioned resource.Quantity `json:"provisioned"`
	// UtilizationPercent is the control plane's own utilization figure, from 0 to
	// 100. It is taken verbatim rather than recomputed from Used and Total, so
	// that it agrees with what the control plane's own interfaces report.
	UtilizationPercent int32 `json:"utilizationPercent"`
}

// +kubebuilder:object:root=true

// StorageNodeMetrics is one storage node's capacity reading.
//
// The object is named after the StorageNode it measures and lives in that
// object's namespace, so somebody who has the node's name needs to learn nothing
// else to ask for it, and ordinary namespaced RBAC confines a reader to the
// namespaces they already have. A backend node with no StorageNode object is
// therefore not listed: it has no name in this API and no namespace to be
// authorized against.
//
// +k8s:openapi-gen=true
type StorageNodeMetrics struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata. Name and namespace are the
	// StorageNode's. The creationTimestamp is the node object's rather than the
	// reading's.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Timestamp is when the control plane sampled these values, which is older
	// than the moment the request was served and may be considerably older if its
	// exporter has stopped being scraped. It is the zero time when the node has
	// never been sampled, so that "never measured" does not read as "measured in
	// 1970."
	Timestamp metav1.Time `json:"timestamp"`

	// NodeID is the control plane's identifier for the backend node. It is the
	// join key back to the control plane's own exporter and to its API, and it is
	// the field that changes when a slot is refilled by a replacement node while
	// the object's name stays where it was.
	NodeID string `json:"nodeID"`

	// StorageCluster is the name of the StorageCluster object the node belongs
	// to, so a reading says which cluster it is about without a second lookup.
	StorageCluster string `json:"storageCluster"`

	// WorkerNode is the Kubernetes worker the node runs on, which is what a
	// reader correlating a full node with a machine is actually looking for.
	WorkerNode string `json:"workerNode,omitempty"`

	// Capacity is the reading itself.
	Capacity StorageNodeCapacity `json:"capacity"`
}

// +kubebuilder:object:root=true

// StorageNodeMetricsList is a list of readings. It carries no continue token: the
// whole set is served from memory in one pass, so there is nothing to page
// through.
//
// +k8s:openapi-gen=true
type StorageNodeMetricsList struct {
	metav1.TypeMeta `json:",inline"`
	// The tag is omitempty rather than the omitzero the CRD kinds in this
	// repository use, because openapi-gen enforces the streaming-list convention
	// on a type it generates definitions for and that convention names omitempty.
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageNodeMetrics `json:"items"`
}
