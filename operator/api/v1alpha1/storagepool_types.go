/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// StoragePoolQoSThroughputSpec defines throughput QosSpec limits in MiB/s.
type StoragePoolQoSThroughputSpec struct {
	// Read is the read throughput limit for the pool.
	Read *int32 `json:"read,omitempty"`
	// ReadWrite is the combined read/write throughput limit for the pool.
	ReadWrite *int32 `json:"readWrite,omitempty"`
	// Write is the write throughput limit for the pool.
	Write *int32 `json:"write,omitempty"`
}

// StoragePoolQoSSpec defines pool QosSpec limits.
type StoragePoolQoSSpec struct {
	// IOPS is the IOPS limit for the pool.
	IOPS *int32 `json:"iops,omitempty"`
	// Throughput contains throughput limits for the pool.
	Throughput *StoragePoolQoSThroughputSpec `json:"throughput,omitempty"`
}

// StoragePoolQoSThroughputStatus defines observed throughput QosSpec values in MiB/s.
type StoragePoolQoSThroughputStatus struct {
	// Read is the observed/configured read throughput value.
	Read *int32 `json:"read,omitempty"`
	// ReadWrite is the observed/configured combined read/write throughput value.
	ReadWrite *int32 `json:"readWrite,omitempty"`
	// Write is the observed/configured write throughput value.
	Write *int32 `json:"write,omitempty"`
}

// StoragePoolQoSStatus defines observed pool QosSpec values.
type StoragePoolQoSStatus struct {
	// Host is the backend host handling pool QosSpec enforcement.
	Host string `json:"host,omitempty"`
	// IOPS is the observed/configured IOPS value.
	IOPS *int32 `json:"iops,omitempty"`
	// Throughput contains observed/configured throughput values.
	Throughput *StoragePoolQoSThroughputStatus `json:"throughput,omitempty"`
}

// StorageClassParameters defines the default StorageClass parameter values for volumes in this pool.
// These are passed as-is to the CSI driver when the StorageClass is created.
// cluster_id and pool_name are always set automatically and cannot be overridden here.
//
// IMPORTANT: StorageClass Parameters are immutable in the Kubernetes API, so this whole field
// is immutable once set (see StoragePoolSpec.StorageClassParameters) — there's no supported way
// to change a pool's StorageClass defaults after the pool is created. Create a new StoragePool
// instead.
type StorageClassParameters struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Read/Write IOPS"
	// QosRwIops sets the read/write IOPS limit (0 = unlimited).
	// +kubebuilder:default="0"
	QosRwIops string `json:"qosRwIops,omitempty"`
	// QosRwMbytes sets the read/write throughput limit in MB/s (0 = unlimited).
	// +kubebuilder:default="0"
	QosRwMbytes string `json:"qosRwMbytes,omitempty"`
	// QosRMbytes sets the read throughput limit in MB/s (0 = unlimited).
	// +kubebuilder:default="0"
	QosRMbytes string `json:"qosRMbytes,omitempty"`
	// QosWMbytes sets the write throughput limit in MB/s (0 = unlimited).
	// +kubebuilder:default="0"
	QosWMbytes string `json:"qosWMbytes,omitempty"`
	// Compression enables compression for logical volumes.
	// +kubebuilder:default="False"
	Compression string `json:"compression,omitempty"`
	// ClientCompression enables client-side (VDO) compression for logical volumes in this
	// pool. Distinct from Compression (server-side). Independent of ClientDeduplication --
	// either, both, or neither may be set. Changing this on a Pool whose StorageClass
	// already exists has no effect (see issue #401) -- it only takes effect for pools
	// whose StorageClass does not exist yet.
	// +kubebuilder:default=false
	ClientCompression *bool `json:"clientCompression,omitempty"`
	// ClientDeduplication enables client-side (VDO) deduplication for logical volumes in
	// this pool. Carries a significant, measured, fixed RAM cost per volume independent of
	// ClientCompression -- intended to be opt-in on specific pools where duplicate data is
	// actually expected, not enabled by default. Same StorageClass-immutability caveat as
	// ClientCompression applies.
	// +kubebuilder:default=false
	ClientDeduplication *bool `json:"clientDeduplication,omitempty"`
	// Encryption enables encryption for logical volumes.
	// +kubebuilder:default=false
	Encryption *bool `json:"encryption,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Replicate By Default"
	// Replicate enables replication for logical volumes.
	// +kubebuilder:default=false
	Replicate *bool `json:"replicate,omitempty"`
	// LvolPriorityClass sets the logical volume priority class.
	// +kubebuilder:default="0"
	LvolPriorityClass string `json:"lvolPriorityClass,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Fabric"
	// Fabric is the transport fabric (e.g. tcp).
	// +kubebuilder:default=tcp
	Fabric string `json:"fabric,omitempty"`
	// MaxNamespacePerSubsys limits namespaces per NVMf subsystem.
	// +kubebuilder:default="1"
	MaxNamespacePerSubsys string `json:"maxNamespacePerSubsys,omitempty"`
	// Tune2fsReservedBlocks sets the ext4 reserved-blocks percentage. Left unset, the node
	// plugin skips tune2fs entirely and mkfs.ext4's own default reserve applies, matching a
	// StorageClass that omits tune2fs_reserved_blocks. A default of "0" here would not be a
	// no-op: it actively runs `tune2fs -m 0` on every volume, since the node plugin only skips
	// the call when the parameter is empty (see stageVolume in the CSI driver), not when it's
	// "0".
	Tune2fsReservedBlocks string `json:"tune2fsReservedBlocks,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Filesystem"
	// Filesystem is the filesystem used to format logical volumes of this pool.
	// +kubebuilder:validation:Enum=ext4;xfs
	// +kubebuilder:default=xfs
	Filesystem string `json:"filesystem,omitempty"`
}

// StoragePoolSpec defines the desired state of StoragePool
type StoragePoolSpec struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Cluster Name"
	// ClusterName is the target storage cluster name.
	// +k8s:immutable
	ClusterName string `json:"clusterName"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Status"
	// Status is an optional desired-status hint for backend workflows.
	// FIXME: Unused for now
	Status string `json:"status,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Capacity Limit"
	// CapacityLimit is the maximum aggregate capacity that can be allocated from this pool.
	// This maps to sbctl pool add --pool-max. Use sizes like 20M, 20G, or 0 for unlimited.
	CapacityLimit string `json:"capacityLimit,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Logical Volume Maximum Size"
	// LogicalVolumeMaxSize is the maximum size allowed for any single logical volume
	// created in this pool. This maps to sbctl pool add --lvol-max. Use sizes like
	// 20M, 20G, or 0 for unlimited.
	LogicalVolumeMaxSize string `json:"logicalVolumeMaxSize,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="DHCHAP"
	// DHCHAP enables DH-HMAC-CHAP key generation for the pool. Authentication is only
	// enforced when allowedNodes is non-empty. Also controls whether the StoragePool's StorageClass
	// gets an allowedTopologies restriction, which — like StorageClass Parameters — is
	// immutable in the Kubernetes API, hence this field is immutable too.
	// +kubebuilder:default=false
	// +k8s:immutable
	DHCHAP bool `json:"dhchap,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Allowed Nodes"
	// AllowedNodes is the list of Kubernetes worker node names allowed to access volumes
	// in this pool. The operator resolves each node name to a deterministic NQN derived
	// from the node's UID: nqn.2014-08.io.simplyblock:uuid:<node-uid>.
	// The CSI node uses the same formula so no manual NQN management is required.
	AllowedNodes []string `json:"allowedNodes,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="QoS"
	// QosSpec defines QosSpec limits for the pool.
	QosSpec *StoragePoolQoSSpec `json:"qos,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Action"
	// Action triggers an imperative pool operation.
	// FIXME: Unused for now
	Action string `json:"action,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Storage Class Parameters"
	// StorageClassParameters sets default StorageClass parameter values for volumes in this pool.
	// Immutable: the underlying StorageClass's Parameters/AllowedTopologies cannot be patched in
	// the Kubernetes API once created, so there is no supported way to change these after the
	// fact. Create a new StoragePool to provision volumes with different settings.
	// +kubebuilder:default={}
	// +k8s:immutable
	StorageClassParameters *StorageClassParameters `json:"storageClassParameters,omitempty"`
}

// StoragePoolStatus defines the observed state of StoragePool.
type StoragePoolStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Storage Pool UUID"
	// UUID is the backend pool UUID.
	UUID string `json:"uuid,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Status"
	// Status is the backend lifecycle status.
	Status string `json:"status,omitempty"`
	// QoS contains observed/configured QoS values.
	QoS *StoragePoolQoSStatus `json:"qos,omitempty"`
	// AllowedNodes lists the Kubernetes node names currently registered on the backend.
	AllowedNodes []string `json:"allowedNodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.status",description="Backend pool status"
// +kubebuilder:printcolumn:name="UUID",type="string",JSONPath=".status.uuid",description="Backend pool UUID"
// +kubebuilder:printcolumn:name="Capacity",type="string",JSONPath=".spec.capacityLimit",description="Configured capacity limit"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +operator-sdk:csv:customresourcedefinitions:displayName="Storage Pool"

// StoragePool is the Schema for the storagepools API
type StoragePool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StoragePool
	// +required
	Spec StoragePoolSpec `json:"spec"`

	// status defines the observed state of StoragePool
	// +optional
	Status StoragePoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StoragePoolList contains a list of StoragePool
type StoragePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StoragePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StoragePool{}, &StoragePoolList{})
}
