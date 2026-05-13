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

// PoolQoSThroughputSpec defines throughput QosSpec limits in MiB/s.
type PoolQoSThroughputSpec struct {
	// Read is the read throughput limit for the pool.
	Read *int32 `json:"read,omitempty"`
	// ReadWrite is the combined read/write throughput limit for the pool.
	ReadWrite *int32 `json:"readWrite,omitempty"`
	// Write is the write throughput limit for the pool.
	Write *int32 `json:"write,omitempty"`
}

// PoolQoSSpec defines pool QosSpec limits.
type PoolQoSSpec struct {
	// IOPS is the IOPS limit for the pool.
	IOPS *int32 `json:"iops,omitempty"`
	// Throughput contains throughput limits for the pool.
	Throughput *PoolQoSThroughputSpec `json:"throughput,omitempty"`
}

// PoolQoSThroughputStatus defines observed throughput QosSpec values in MiB/s.
type PoolQoSThroughputStatus struct {
	// Read is the observed/configured read throughput value.
	Read *int32 `json:"read,omitempty"`
	// ReadWrite is the observed/configured combined read/write throughput value.
	ReadWrite *int32 `json:"readWrite,omitempty"`
	// Write is the observed/configured write throughput value.
	Write *int32 `json:"write,omitempty"`
}

// PoolQoSStatus defines observed pool QosSpec values.
type PoolQoSStatus struct {
	// Host is the backend host handling pool QosSpec enforcement.
	Host string `json:"host,omitempty"`
	// IOPS is the observed/configured IOPS value.
	IOPS *int32 `json:"iops,omitempty"`
	// Throughput contains observed/configured throughput values.
	Throughput *PoolQoSThroughputStatus `json:"throughput,omitempty"`
}

// StorageClassParameters defines the default StorageClass parameter values for volumes in this pool.
// These are passed as-is to the CSI driver when the StorageClass is created.
// cluster_id and pool_name are always set automatically and cannot be overridden here.
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
	// Encryption enables encryption for logical volumes.
	// +kubebuilder:default=false
	Encryption *bool `json:"encryption,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Replicate By Default"
	// Replicate enables replication for logical volumes.
	// +kubebuilder:default=false
	Replicate *bool `json:"replicate,omitempty"`
	// NumDataChunks is the number of data chunks (distr_ndcs).
	// +kubebuilder:default="1"
	NumDataChunks string `json:"numDataChunks,omitempty"`
	// NumParityChunks is the number of parity chunks (distr_npcs).
	// +kubebuilder:default="1"
	NumParityChunks string `json:"numParityChunks,omitempty"`
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
	// Tune2fsReservedBlocks sets the ext4 reserved-blocks percentage.
	// +kubebuilder:default="0"
	Tune2fsReservedBlocks string `json:"tune2fsReservedBlocks,omitempty"`
}

// PoolSpec defines the desired state of Pool
type PoolSpec struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Cluster Name"
	// ClusterName is the target storage cluster name.
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
	// enforced when allowedNodes is non-empty
	// +kubebuilder:default=false
	DHCHAP bool `json:"dhchap,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Allowed Nodes"
	// AllowedNodes is the list of Kubernetes worker node names allowed to access volumes
	// in this pool. The operator resolves each node name to a deterministic NQN derived
	// from the node's UID: nqn.2014-08.io.simplyblock:uuid:<node-uid>.
	// The CSI node uses the same formula so no manual NQN management is required.
	AllowedNodes []string `json:"allowedNodes,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="QoS"
	// QosSpec defines QosSpec limits for the pool.
	QosSpec *PoolQoSSpec `json:"qos,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Action"
	// Action triggers an imperative pool operation.
	// FIXME: Unused for now
	Action string `json:"action,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Storage Class Parameters"
	// StorageClassParameters sets default StorageClass parameter values for volumes in this pool.
	// +kubebuilder:default={}
	StorageClassParameters *StorageClassParameters `json:"storageClassParameters,omitempty"`
}

// PoolStatus defines the observed state of Pool.
type PoolStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Pool UUID"
	// UUID is the backend pool UUID.
	UUID string `json:"uuid,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Status"
	// Status is the backend lifecycle status.
	Status string `json:"status,omitempty"`
	// QoS contains observed/configured QoS values.
	QoS *PoolQoSStatus `json:"qos,omitempty"`
	// AllowedNodes lists the Kubernetes node names currently registered on the backend.
	AllowedNodes []string `json:"allowedNodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +operator-sdk:csv:customresourcedefinitions:displayName="Pool"

// Pool is the Schema for the pools API
type Pool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Pool
	// +required
	Spec PoolSpec `json:"spec"`

	// status defines the observed state of Pool
	// +optional
	Status PoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PoolList contains a list of Pool
type PoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Pool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Pool{}, &PoolList{})
}
