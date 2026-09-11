// StoragePool in the shape design-storagepool.md specifies: one tenancy unit
// within a StorageCluster, with its own capacity limit and QoS ceilings.
//
// The kind shipped under v1alpha1 and this version supersedes it, so the spoke
// beside the older type converts between the two. What moves (§11):
//
//   - spec.clusterName becomes spec.clusterRef, matching every other reference
//     in the group.
//   - spec.capacityLimit, spec.logicalVolumeMaxSize, and spec.qos.* regroup under
//     spec.limits, which is what the pool as a whole is held to.
//   - spec.storageClassParameters.* regroups under spec.volumeDefaults, which is
//     what each volume in the pool gets. The two groups now use the same units,
//     so a reader can compare a pool's ceiling against a volume's.
//   - spec.dhchap becomes spec.volumeDefaults.enableDHCHAP, and encryption and
//     replicate take the same enable form.
//   - spec.action and spec.status are removed. Both were marked unused and
//     neither ever had an effect; the operations they gestured at are
//     StoragePoolOps.
//
// The grouping is the point of the rework. The registered spec had the pool's
// IOPS ceiling at spec.qos.iops and a volume's default IOPS at
// spec.storageClassParameters.qosRwIops — one an integer, the other a string,
// one enforced against the pool and the other against each volume, both called
// QoS. Naming the groups for what they limit is what makes that legible.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StoragePoolPhase is where the operator has got to with this pool.
// +kubebuilder:validation:Enum=Pending;Ready;Deleting
type StoragePoolPhase string

const (
	// StoragePoolPhasePending is a pool that has no backend UUID yet, whether
	// because its cluster is not finished or because the create has not landed.
	StoragePoolPhasePending StoragePoolPhase = "Pending"
	// StoragePoolPhaseReady is a pool the control plane has created and that
	// volumes can be provisioned from.
	StoragePoolPhaseReady StoragePoolPhase = "Ready"
	// StoragePoolPhaseDeleting is a pool being torn down, which may be held for
	// a long time behind an assigned class or a bound volume.
	StoragePoolPhaseDeleting StoragePoolPhase = "Deleting"
)

// ThroughputLimits are throughput ceilings in megabytes per second. The unit is
// the field's rather than the value's, which is why the class keys these reach
// spell it out: a parameter map has no type to carry it.
type ThroughputLimits struct {
	// Read is the read-only ceiling, written as max_read_mbytes_per_sec.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Read *int32 `json:"read,omitempty"`

	// Write is the write-only ceiling, written as max_write_mbytes_per_sec.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Write *int32 `json:"write,omitempty"`

	// ReadWrite is the ceiling on both directions together, written as
	// max_mbytes_per_sec. It is not an access mode, which is the confusion the
	// older class key qos_rw_mbytes invited.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ReadWrite *int32 `json:"readWrite,omitempty"`
}

// PoolLimits are the ceilings the pool as a whole is held to. They are the
// pool's budget rather than a volume's: a volume's own defaults are in
// StoragePoolSpec.VolumeDefaults, and the two use the same units so that a
// reader can compare them.
type PoolLimits struct {
	// Capacity is the total capacity the pool may allocate, written the way an
	// administrator writes one: `10T`, `500G`. Empty is unlimited.
	// +optional
	Capacity string `json:"capacity,omitempty"`

	// MaxVolumeSize is the largest single logical volume the pool will create.
	// Empty is unlimited.
	// +optional
	MaxVolumeSize string `json:"maxVolumeSize,omitempty"`

	// IOPS is the pool-wide ceiling on operations per second, both directions
	// together. Zero is unlimited, which is the control plane's own convention
	// for these values.
	// +kubebuilder:validation:Minimum=0
	// +optional
	IOPS *int32 `json:"iops,omitempty"`

	// Throughput is the pool-wide throughput ceiling.
	// +optional
	Throughput *ThroughputLimits `json:"throughput,omitempty"`
}

// VolumeDefaults are the defaults every volume in the pool is created with. They
// are what a StorageClass assigned to this pool is expected to carry in its
// parameters, which is how they reach the CSI driver, and the whole block is
// immutable once set for that reason: StorageClass.parameters is immutable in
// the Kubernetes API, so a pool whose defaults changed would have a class the
// operator cannot update and a spec that no longer describes it.
type VolumeDefaults struct {
	// IOPS is each volume's ceiling on operations per second, both directions
	// together. Zero is unlimited. It is written into the class as max_iops.
	// +kubebuilder:validation:Minimum=0
	// +optional
	IOPS *int32 `json:"iops,omitempty"`

	// Throughput is each volume's throughput ceiling, in megabytes per second.
	// +optional
	Throughput *ThroughputLimits `json:"throughput,omitempty"`

	// Filesystem is what a volume is formatted with. The values are the kernel's
	// names for filesystems, which is why they are not recased: this group did
	// not invent the words.
	// +kubebuilder:validation:Enum=ext4;xfs
	// +kubebuilder:default=xfs
	// +optional
	Filesystem string `json:"filesystem,omitempty"`

	// EnableCompression compresses logical volumes.
	// +optional
	EnableCompression *bool `json:"enableCompression,omitempty"`

	// EnableEncryption encrypts logical volumes, using the key store the cluster
	// names in its own spec.
	// +optional
	EnableEncryption *bool `json:"enableEncryption,omitempty"`

	// EnableReplication replicates logical volumes.
	// +optional
	EnableReplication *bool `json:"enableReplication,omitempty"`

	// EnableDHCHAP authenticates NVMe-oF connections to this pool's volumes.
	// Authentication is only enforced when AllowedNodes is non-empty, because
	// the generated class gets its node selector from that list and a class's
	// parameters cannot be edited afterward.
	// +optional
	EnableDHCHAP *bool `json:"enableDHCHAP,omitempty"`

	// PriorityClass is the logical-volume priority class the control plane
	// places with.
	// +optional
	PriorityClass string `json:"priorityClass,omitempty"`

	// Fabric is the storage fabric a volume is served over, defaulting to the
	// cluster's.
	// +optional
	Fabric string `json:"fabric,omitempty"`

	// MaxNamespacesPerSubsystem caps how many namespaces share one NVMe-oF
	// subsystem.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxNamespacesPerSubsystem *int32 `json:"maxNamespacesPerSubsystem,omitempty"`

	// Tune2fsReservedBlocks is the reserved-block percentage passed to tune2fs
	// on an ext4 volume. Empty means the filesystem's own default, which is not
	// the same as `0`: the node plugin skips the call entirely when this is
	// empty, and runs `tune2fs -m 0` when it is `0`.
	// +optional
	Tune2fsReservedBlocks string `json:"tune2fsReservedBlocks,omitempty"`
}

// StoragePoolSpec is the desired state of one tenancy unit within a cluster.
type StoragePoolSpec struct {
	// ClusterRef names the StorageCluster this pool is carved out of, in this
	// pool's own namespace. The cluster owns this object by controller
	// reference, so deleting the cluster deletes its pools, held behind each
	// pool's own finalizer while classes are assigned or volumes are bound.
	//
	// Immutable from creation: which cluster a pool is in is its identity.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// AllowedNodes restricts which storage nodes may host this pool's volumes.
	// Empty means every node in the cluster. Narrowing it stops new volumes
	// landing on the removed nodes and leaves the existing ones where they are.
	//
	// The list is left exactly as authored: a name that no longer resolves is
	// dropped from Status.AllowedNodes rather than pruned from here, so a node
	// removed for maintenance and added back under the same name returns to the
	// pools that named it without anybody re-authoring them.
	// +optional
	// +listType=set
	AllowedNodes []string `json:"allowedNodes,omitempty"`

	// Limits are the ceilings the pool as a whole is held to. Mutable: raising a
	// pool's capacity is an ordinary operation the control plane supports, and it
	// does not touch any StorageClass.
	// +optional
	Limits *PoolLimits `json:"limits,omitempty"`

	// VolumeDefaults are what every volume in the pool is created with.
	// Immutable once set, because StorageClass.parameters is immutable in the
	// Kubernetes API: a pool whose defaults changed would have a class the
	// operator cannot update. Changing them means creating a new pool.
	// +optional
	// +k8s:immutable
	VolumeDefaults *VolumeDefaults `json:"volumeDefaults,omitempty"`
}

// PoolLimitsStatus is what the control plane reports the pool's ceilings
// actually are, which is not necessarily what Limits asked for.
type PoolLimitsStatus struct {
	// Host is the backend host enforcing the pool's QoS.
	// +optional
	Host string `json:"host,omitempty"`

	// IOPS is the combined ceiling the control plane is enforcing.
	// +optional
	IOPS *int32 `json:"iops,omitempty"`

	// Throughput is the throughput ceiling the control plane is enforcing.
	// +optional
	Throughput *ThroughputLimits `json:"throughput,omitempty"`
}

// StoragePoolStatus is the observed state of one pool.
type StoragePoolStatus struct {
	// Phase is the operator's own view of this pool.
	// +optional
	Phase StoragePoolPhase `json:"phase,omitempty"`

	// UUID is the backend pool UUID. Empty means the pool has not been created,
	// and it is the field the controller branches on.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// Status is the lifecycle the control plane reports, in the control plane's
	// own spelling, which is why it carries no Enum here.
	// +optional
	Status string `json:"status,omitempty"`

	// StorageClassNames are the classes assigned to this pool, found by the
	// three storage.simplyblock.io labels a class carries. Empty means no class
	// draws from this pool yet, which is a valid state and what a freshly
	// created cluster's default pool has before anybody writes one. Publishing
	// it is what makes the assignment readable from the pool, and what a
	// deletion is held on.
	// +optional
	// +listType=set
	StorageClassNames []string `json:"storageClassNames,omitempty"`

	// DefaultStorageClassName is the class the operator wrote for the default
	// pool, set on that pool only. It records that the class was created, so a
	// class missing while this is set was deleted deliberately and is not
	// written again.
	// +optional
	DefaultStorageClassName string `json:"defaultStorageClassName,omitempty"`

	// Limits is what the control plane reports the ceilings to be.
	// +optional
	Limits *PoolLimitsStatus `json:"limits,omitempty"`

	// AllowedNodes is Spec.AllowedNodes resolved against the StorageNodes that
	// exist, which is what the control plane is sent. An empty list here is not
	// the same as an absent Spec.AllowedNodes: absent means every node, and
	// empty after resolution means the pool can place nothing.
	// +optional
	// +listType=set
	AllowedNodes []string `json:"allowedNodes,omitempty"`

	// ActiveOpsRef names the StoragePoolOps currently allowed to act on this
	// pool. Empty when none is running.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the pool moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// v1alpha2 is the storage version in the manifests this repository ships. A
// fresh install stores this shape from the first write and converts nothing; an
// upgrade of an existing cluster applies the same CRD with storage held at
// v1alpha1 and flips it with the storage rewrite once the conversion webhook is
// serving.
// +kubebuilder:storageversion
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sp
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Capacity",type=string,JSONPath=".spec.limits.capacity"
// +kubebuilder:printcolumn:name="Classes",type=string,JSONPath=".status.storageClassNames"
// +kubebuilder:printcolumn:name="UUID",type=string,JSONPath=".status.uuid",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StoragePool carves a StorageCluster into a unit with its own capacity limit
// and QoS ceilings, and it is what a StorageClass is assigned to. It is
// therefore the join between the storage administrator's world and the
// application developer's, a join held together by three labels, an opaque
// parameter map, and a finalizer rather than by the API.
type StoragePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StoragePoolSpec   `json:"spec,omitempty"`
	Status StoragePoolStatus `json:"status,omitempty"`
}

// Hub marks this version as the conversion hub for StoragePool.
func (*StoragePool) Hub() {}

// +kubebuilder:object:root=true

// StoragePoolList contains a list of StoragePool.
type StoragePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StoragePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StoragePool{}, &StoragePoolList{})
}
