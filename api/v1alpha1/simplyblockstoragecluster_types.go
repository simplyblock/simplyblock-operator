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

type CapacityThresholdSpec struct {
	// Capacity defines the absolute capacity threshold value.
	Capacity *int32 `json:"capacity,omitempty"`
	// ProvisionedCapacity defines the provisioned-capacity threshold value.
	ProvisionedCapacity *int32 `json:"provisionedCapacity,omitempty"`
}

type StripeSpec struct {
	// DataChunks defines the number of data chunks in the erasure-coding layout.
	DataChunks *int32 `json:"dataChunks,omitempty"`
	// ParityChunks defines the number of parity chunks in the erasure-coding layout.
	ParityChunks *int32 `json:"parityChunks,omitempty"`
}

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SimplyBlockStorageClusterSpec defines the desired state of SimplyBlockStorageCluster
type SimplyBlockStorageClusterSpec struct {
	// MgmtIfname is the management network interface name used for cluster communication.
	MgmtIfname string `json:"mgmtIfname,omitempty"`
	// EnableNodeAffinity enables node-affinity placement for storage components.
	EnableNodeAffinity *bool `json:"enableNodeAffinity,omitempty"`
	// StripeSpec configures erasure-coding data/parity chunk counts.
	StripeSpec *StripeSpec `json:"stripeSpec,omitempty"`
	// HAType defines the backend high-availability mode.
	HAType string `json:"haType,omitempty"`
	// ClusterName is the user-facing cluster identifier.
	ClusterName string `json:"clusterName"`
	// +kubebuilder:validation:Enum=activate;expand
	// Action triggers a cluster-level action.
	Action string `json:"action,omitempty"`

	// IsSingleNode enables single-node cluster mode.
	IsSingleNode *bool `json:"isSingleNode,omitempty"`
	// StrictNodeAntiAffinity enforces strict anti-affinity between storage nodes.
	StrictNodeAntiAffinity *bool `json:"strictNodeAntiAffinity,omitempty"`
	// QpairCount defines the NVMe queue-pair count used by the cluster.
	QpairCount *int32 `json:"qpairCount,omitempty"`
	// BlockSize defines the logical block size in bytes.
	BlockSize *int32 `json:"blockSize,omitempty"`
	// PageSizeInBlocks defines page size expressed in blocks.
	PageSizeInBlocks *int32 `json:"pageSizeInBlocks,omitempty"`
	// MaxQueueSize defines the maximum backend queue size.
	MaxQueueSize *int32 `json:"maxQueueSize,omitempty"`
	// InflightIOThreshold defines the inflight I/O threshold.
	InflightIOThreshold *int32 `json:"inflightIOThreshold,omitempty"`
	// Fabric defines the storage fabric type.
	Fabric string `json:"fabric,omitempty"`
	// ClientDataNic defines the client data network interface.
	ClientDataNic string `json:"clientDataNic,omitempty"`
	// MaxFaultTolerance defines the maximum tolerated concurrent faults.
	MaxFaultTolerance *int32 `json:"maxFaultTolerance,omitempty"`
	// NvmfBasePort defines the base NVMf service port.
	NvmfBasePort *int32 `json:"nvmfBasePort,omitempty"`
	// RpcBasePort defines the base RPC service port.
	RpcBasePort *int32 `json:"rpcBasePort,omitempty"`
	// SnodeApiPort defines the storage-node API port.
	SnodeApiPort *int32 `json:"snodeApiPort,omitempty"`

	// QoSClasses defines backend QosSpec class configuration.
	QoSClasses string `json:"qosClasses,omitempty"`
	// WarningThresholdSpec defines warning-level capacity thresholds.
	WarningThresholdSpec *CapacityThresholdSpec `json:"warningThresholdSpec,omitempty"`
	// CriticalThresholdSpec defines critical-level capacity thresholds.
	CriticalThresholdSpec *CapacityThresholdSpec `json:"criticalThresholdSpec,omitempty"`
	// ClientQpairCount defines client-side queue-pair count.
	// FIXME: Unused for now (API update required?)
	ClientQpairCount *int32 `json:"clientQpairCount,omitempty"`
	// IncludeEventLog controls whether event logs are included in responses/exports.
	IncludeEventLog *bool `json:"includeEventLog,omitempty"`
	// EventLogEntries limits the number of event-log entries returned/retained.
	EventLogEntries *int32 `json:"eventLogEntries,omitempty"`
}

// SimplyBlockStorageClusterStatus defines the observed state of SimplyBlockStorageCluster.
type SimplyBlockStorageClusterStatus struct {
	// UUID is the backend cluster UUID.
	UUID string `json:"uuid,omitempty"`
	// ClusterName is the resolved backend cluster name.
	ClusterName string `json:"clusterName,omitempty"`
	// MgmtNodes is the number of management nodes.
	MgmtNodes *int32 `json:"mgmtNodes,omitempty"`
	// StorageNodes is the number of storage nodes.
	StorageNodes *int32 `json:"storageNodes,omitempty"`
	// NQN is the cluster NVM subsystem qualified name.
	NQN string `json:"nqn,omitempty"`
	// Status is the backend-reported lifecycle status.
	Status string `json:"status,omitempty"`
	// Rebalancing indicates whether cluster rebalancing is currently active.
	Rebalancing *bool `json:"rebalancing,omitempty"`
	// ErasureCodingScheme is the active erasure-coding layout, for example "2x1".
	ErasureCodingScheme string `json:"erasureCodingScheme,omitempty"`
	// SecretName is the Kubernetes Secret containing cluster credentials.
	SecretName string `json:"secretName,omitempty"`
	// LastUpdated is the last backend update timestamp.
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
	// Created is the backend creation timestamp.
	Created *metav1.Time `json:"created,omitempty"`
	// Configured indicates whether initial cluster setup completed.
	Configured bool `json:"configured,omitempty"`
	// ActionStatus tracks the most recent action execution state.
	ActionStatus *ActionStatus `json:"actionStatus,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SimplyBlockStorageCluster is the Schema for the simplyblockstorageclusters API
type SimplyBlockStorageCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SimplyBlockStorageCluster
	// +required
	Spec SimplyBlockStorageClusterSpec `json:"spec"`

	// status defines the observed state of SimplyBlockStorageCluster
	// +optional
	Status SimplyBlockStorageClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimplyBlockStorageClusterList contains a list of SimplyBlockStorageCluster
type SimplyBlockStorageClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimplyBlockStorageCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimplyBlockStorageCluster{}, &SimplyBlockStorageClusterList{})
}
