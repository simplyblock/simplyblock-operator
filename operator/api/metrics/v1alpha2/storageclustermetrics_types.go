// StorageClusterMetrics: how much of a storage cluster is in use, as the
// control plane's exporter last measured it.
//
// The kind exists for the reason the three readings beside it do. A cluster's
// layout is desired state and lives in its spec; how full it is moves with
// every write to every volume in it, and putting that in the custom resource
// would charge one etcd write and one wake-up of every watcher of the kind for
// a number nothing reconciles toward. So it is computed from Prometheus when a
// client asks and never persisted.
//
// What it answers that the three narrower kinds cannot is the whole-cluster
// question. A pool's reading is bounded by the capacity that pool was carved
// out with, and a device's by one device; neither says whether the cluster
// underneath them is about to run out. That number is the one a capacity plan
// is made against, and until now it existed only in Grafana.
//
// The consequences are the same as the other three kinds' and are load-bearing
// rather than incidental:
//
//   - There is no spec and no status. The fields sit at the top level of the
//     object, because neither half of the split means anything for a reading
//     nobody wrote.
//   - An object exists only while a StorageCluster does. There is no deletion
//     and no tombstone: a cluster that goes away stops being listed.
//   - A deployment with no reachable Prometheus serves no readings at all,
//     rather than readings of zero. A zero is the reading of an empty cluster
//     and not the absence of a reading.
//
// Which pool or which volume is filling a cluster up is deliberately not here.
// A cluster reports its own totals, and the breakdown behind them is
// StoragePoolMetrics and LogicalVolumeMetrics, which is what keeps the
// cardinality of the workload out of this list.

package v1alpha2

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageClusterCapacity is what one cluster holds and what it has promised.
// Every size is in bytes and is quoted as a resource.Quantity so that kubectl
// prints it the way it prints a PersistentVolumeClaim's capacity.
//
// +k8s:openapi-gen=true
type StorageClusterCapacity struct {
	// Total is the raw capacity the cluster's devices add up to, after the
	// erasure-coding layout has taken its share.
	Total resource.Quantity `json:"total"`
	// Used is the space the cluster's volumes actually occupy, after thin
	// provisioning, compression, and deduplication.
	Used resource.Quantity `json:"used"`
	// Free is the cluster's unallocated remainder as the control plane accounts
	// for it. It is reported rather than derived, so it need not equal Total
	// minus Used.
	Free resource.Quantity `json:"free"`
	// Provisioned is the sum of what every volume in the cluster was promised,
	// which on a thin-provisioned cluster legitimately exceeds Used and may
	// exceed Total. Against Total it is the over-commitment signal.
	Provisioned resource.Quantity `json:"provisioned"`
	// UtilizationPercent is the control plane's own utilization figure, from 0
	// to 100. It is taken verbatim rather than recomputed from Used and Total,
	// so that it agrees with what the control plane's own interfaces report.
	UtilizationPercent int32 `json:"utilizationPercent"`
}

// +kubebuilder:object:root=true

// StorageClusterMetrics is one storage cluster's capacity reading.
//
// The object is named after the StorageCluster it measures and lives in that
// object's namespace, so an administrator who has the cluster's name needs to
// learn nothing else to ask for it, and ordinary namespaced RBAC confines a
// reader to the namespaces they already have. A backend cluster with no
// StorageCluster object is therefore not listed: it has no name in this API and
// no namespace to be authorized against.
//
// +k8s:openapi-gen=true
type StorageClusterMetrics struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata. Name and namespace are the
	// StorageCluster's. The creationTimestamp is the cluster object's rather
	// than the reading's.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Timestamp is when the control plane sampled these values, which is older
	// than the moment the request was served. It is the zero time on a
	// deployment whose exporter reports no sample date.
	Timestamp metav1.Time `json:"timestamp"`

	// ClusterID is the control plane's identifier for the cluster. It is the
	// join key back to the control plane's own exporter and to its API.
	ClusterID string `json:"clusterID"`

	// ErasureCodingScheme is the active layout, rendered as ndcs, an x, and
	// npcs. It is carried because it is what the raw and the usable figures
	// differ by, and reading a total without it invites the wrong plan.
	// +optional
	ErasureCodingScheme string `json:"erasureCodingScheme,omitempty"`

	// Capacity is the reading itself.
	Capacity StorageClusterCapacity `json:"capacity"`
}

// +kubebuilder:object:root=true

// StorageClusterMetricsList is a list of readings. It carries no continue
// token: the whole set is served from memory in one pass, so there is nothing
// to page through.
//
// +k8s:openapi-gen=true
type StorageClusterMetricsList struct {
	metav1.TypeMeta `json:",inline"`
	// The tag is omitempty rather than the omitzero the CRD kinds in this
	// repository use, because openapi-gen enforces the streaming-list
	// convention on a type it generates definitions for and that convention
	// names omitempty.
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageClusterMetrics `json:"items"`
}
