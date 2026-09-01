// LogicalVolumeMetrics: the capacity a simplyblock logical volume actually
// occupies, as the control plane last reported it.
//
// The type lives in an aggregated API group rather than in a CRD because of what
// the numbers are. Thin-provisioned usage moves continuously, and every place
// Kubernetes offers to store it charges for the movement: a PersistentVolume
// annotation rewritten on each sample wakes every PV watcher in the cluster, and
// a custom resource per volume writes the same churn to etcd. A sample is not
// desired state and nothing reconciles toward it, so it is computed from the
// control-plane cache when a client asks and never persisted at all. That is the
// same trade metrics.k8s.io makes for PodMetrics.
//
// Two consequences follow from that and are load-bearing rather than
// incidental:
//
//   - There is no spec and no status. The fields sit at the top level of the
//     object, as they do on PodMetrics, because neither half of the spec/status
//     split means anything for a reading nobody wrote.
//   - An object exists only while the control plane reports the volume. There is
//     no deletion, no finalizer, and no tombstone; a volume that goes away stops
//     being listed.

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LogicalVolumeCapacity is the space a volume occupies on the cluster it is
// provisioned from. Every size is in bytes and is quoted as a resource.Quantity
// so that kubectl prints it the way it prints a PersistentVolumeClaim's
// capacity.
//
// Provisioned against Used is the distinction the type exists for: a volume is
// thin-provisioned, so the size a claim asked for and the space the cluster has
// actually given it are different numbers, and only the first of them is visible
// anywhere in the Kubernetes API today.
//
// +k8s:openapi-gen=true
type LogicalVolumeCapacity struct {
	// Provisioned is the volume's nominal size: what the PersistentVolumeClaim
	// asked for, and the number the volume reports to the initiator.
	Provisioned resource.Quantity `json:"provisioned"`
	// Used is the space the volume actually occupies after thin provisioning,
	// compression, and deduplication. It is the field a PersistentVolume cannot
	// express.
	Used resource.Quantity `json:"used"`
	// Free is the volume's unallocated remainder, as the control plane accounts
	// for it. It is reported rather than derived, so it need not equal
	// Provisioned minus Used.
	Free resource.Quantity `json:"free"`
	// Total is the capacity the volume is charged against, which for a clone or
	// a snapshot chain is larger than the volume alone.
	Total resource.Quantity `json:"total"`
	// UtilizationPercent is the control plane's own utilization figure, from 0 to
	// 100. It is taken verbatim rather than recomputed from Used and Provisioned,
	// because the control plane accounts for shared blocks in a clone chain and a
	// naive ratio does not.
	UtilizationPercent int32 `json:"utilizationPercent"`
}

// +kubebuilder:object:root=true

// LogicalVolumeMetrics is one logical volume's capacity reading.
//
// The object is named after the PersistentVolumeClaim it backs and lives in that
// claim's namespace, so a user who knows their claim's name needs to learn
// nothing else to ask for it, and ordinary namespaced RBAC confines a tenant to
// their own volumes. A logical volume with no bound claim is therefore not
// listed: it has no name in this API and no namespace to be authorized against.
//
// +k8s:openapi-gen=true
type LogicalVolumeMetrics struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata. Name and namespace are the backing
	// PersistentVolumeClaim's; creationTimestamp is the claim's, not the
	// reading's.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Timestamp is when the control plane sampled these values, which is older
	// than the moment the request was served and may be considerably older if
	// the update stream has been disconnected.
	Timestamp metav1.Time `json:"timestamp"`

	// VolumeHandle is the CSI volume handle, "clusterID:poolID:volumeID." It is
	// the join key back to the PersistentVolume and to the control plane.
	VolumeHandle string `json:"volumeHandle"`

	// PersistentVolume is the name of the cluster-scoped PersistentVolume bound
	// to this object's claim.
	PersistentVolume string `json:"persistentVolume"`

	// PoolName is the backend storage pool the volume was provisioned from.
	PoolName string `json:"poolName"`

	// Capacity is the reading itself.
	Capacity LogicalVolumeCapacity `json:"capacity"`
}

// +kubebuilder:object:root=true

// LogicalVolumeMetricsList is a list of readings. It carries no continue token:
// the whole set is served from memory in one pass, so there is nothing to page
// through.
//
// +k8s:openapi-gen=true
type LogicalVolumeMetricsList struct {
	metav1.TypeMeta `json:",inline"`
	// The tag is omitempty rather than the omitzero the CRD kinds in this
	// repository use. openapi-gen enforces the streaming-list convention on a
	// type it generates definitions for, and that convention names omitempty; a
	// CRD is never checked against it, which is why the two differ.
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LogicalVolumeMetrics `json:"items"`
}
