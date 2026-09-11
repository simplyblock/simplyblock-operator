// StoragePoolMetrics: how much of a storage pool is in use, as the control
// plane's exporter last measured it.
//
// The kind exists for the reason the two readings beside it do. A pool's
// ceilings are desired state and live in its spec; what the pool has actually
// allocated moves with every volume written to it, and putting that in a custom
// resource would charge one etcd write and one wake-up of every watcher of the
// kind for a number nothing reconciles toward. So it is computed from Prometheus
// when a client asks and never persisted.
//
// What it answers that the pool object cannot is the tenancy question:
// spec.limits.capacity is what a pool may allocate, and Provisioned against it
// is whether the tenant is about to run out. Used beside Provisioned is the
// second half, because a pool is thin-provisioned and the space promised out of
// it is not the space it occupies.
//
// The consequences are the same as the other two kinds' and are load-bearing
// rather than incidental:
//
//   - There is no spec and no status. The fields sit at the top level of the
//     object, because neither half of the split means anything for a reading
//     nobody wrote.
//   - An object exists only while a StoragePool does. There is no deletion and
//     no tombstone: a pool that goes away stops being listed.
//   - A deployment with no reachable Prometheus serves no readings at all,
//     rather than readings of zero. A zero is the reading of an empty pool and
//     not the absence of a reading.
//
// Which volume in a pool is the one filling it up is deliberately not here. A
// pool reports its own totals, and the per-volume breakdown behind them is
// LogicalVolumeMetrics against each volume's claim, which is what lets a tenant
// read their own volumes without being granted anything on the pool and keeps
// the cardinality of the workload out of this list.
//
// design-storagepool.md §9.2 is the specification.

package v1alpha2

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StoragePoolCapacity is what one pool holds and what it has promised. Every
// size is in bytes and is quoted as a resource.Quantity so that kubectl prints
// it the way it prints a PersistentVolumeClaim's capacity.
//
// +k8s:openapi-gen=true
type StoragePoolCapacity struct {
	// Total is the capacity the pool is charged against.
	Total resource.Quantity `json:"total"`
	// Used is the space the pool's volumes actually occupy, after thin
	// provisioning, compression, and deduplication.
	Used resource.Quantity `json:"used"`
	// Free is the pool's unallocated remainder as the control plane accounts for
	// it. It is reported rather than derived, so it need not equal Total minus
	// Used.
	Free resource.Quantity `json:"free"`
	// Provisioned is the sum of what the pool's volumes were promised, which on
	// a thin-provisioned pool legitimately exceeds Used and may exceed Total.
	// Against the pool's own spec.limits.capacity it is the tenancy signal.
	Provisioned resource.Quantity `json:"provisioned"`
	// UtilizationPercent is the control plane's own utilization figure, from 0
	// to 100. It is taken verbatim rather than recomputed from Used and Total,
	// so that it agrees with what the control plane's own interfaces report.
	UtilizationPercent int32 `json:"utilizationPercent"`
}

// +kubebuilder:object:root=true

// StoragePoolMetrics is one storage pool's capacity reading.
//
// The object is named after the StoragePool it measures and lives in that
// object's namespace, so an administrator who has the pool's name needs to learn
// nothing else to ask for it, and ordinary namespaced RBAC confines a reader to
// the namespaces they already have. A control-plane pool with no StoragePool
// object is therefore not listed: it has no name in this API and no namespace to
// be authorized against.
//
// +k8s:openapi-gen=true
type StoragePoolMetrics struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata. Name and namespace are the
	// StoragePool's. The creationTimestamp is the pool object's rather than the
	// reading's.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Timestamp is when the control plane sampled these values, which is older
	// than the moment the request was served. The control plane exports no
	// sample date for a pool, unlike a volume or a device, so it is the zero
	// time on every reading this API serves today rather than a date that is
	// sometimes absent.
	Timestamp metav1.Time `json:"timestamp"`

	// PoolID is the control plane's identifier for the pool. It is the join key
	// back to the control plane's own exporter and to its API.
	PoolID string `json:"poolID"`

	// ClusterID is the control plane's identifier for the cluster the pool is
	// carved out of, so a reading says which cluster it is about without a
	// second lookup.
	ClusterID string `json:"clusterID"`

	// Capacity is the reading itself.
	Capacity StoragePoolCapacity `json:"capacity"`
}

// +kubebuilder:object:root=true

// StoragePoolMetricsList is a list of readings. It carries no continue token:
// the whole set is served from memory in one pass, so there is nothing to page
// through.
//
// +k8s:openapi-gen=true
type StoragePoolMetricsList struct {
	metav1.TypeMeta `json:",inline"`
	// The tag is omitempty rather than the omitzero the CRD kinds in this
	// repository use, because openapi-gen enforces the streaming-list convention
	// on a type it generates definitions for and that convention names
	// omitempty.
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StoragePoolMetrics `json:"items"`
}
