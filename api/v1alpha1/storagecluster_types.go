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

// NodeRecycleSpec configures the node-recycle action behaviour.
type NodeRecycleSpec struct {
	// RefreshSNodeAPI restarts the storage-node DaemonSet pod on each node
	// before shutting it down, ensuring the latest image is running.
	RefreshSNodeAPI bool `json:"refreshSNodeAPI,omitempty"`
}

// NodeRecycleStatus tracks in-progress state for the node-recycle action.
// All fields are persisted in CR status so the reconciler can resume after a requeue.
type NodeRecycleStatus struct {
	// PendingNodes is the ordered list of node UUIDs still to be recycled.
	PendingNodes []string `json:"pendingNodes,omitempty"`
	// ProcessedNodes is the list of node UUIDs already recycled.
	ProcessedNodes []string `json:"processedNodes,omitempty"`
	// NodePhase is the current step for the node being recycled:
	// "snode-refresh" | "snode-refresh-wait" | "shutting-down" | "restarting" | "rebalancing"
	NodePhase string `json:"nodePhase,omitempty"`
	// PhaseTriggered indicates the API call for the current NodePhase was already sent.
	PhaseTriggered bool `json:"phaseTriggered,omitempty"`
}

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type BackupCredentialsSecretRef struct {
	// Name is the name of the Secret in the same namespace as the cluster CR.
	Name string `json:"name"`
}

type BackupSpec struct {
	LocalEndpoint string `json:"localEndpoint,omitempty"`
	// +optional
	SnapshotBackups *bool `json:"snapshotBackups,omitempty"`
	// +optional
	WithCompression *bool `json:"withCompression,omitempty"`
	// +optional
	SecondaryTarget *int32 `json:"secondaryTarget,omitempty"`
	// +optional
	LocalTesting *bool `json:"localTesting,omitempty"`
	// CredentialsSecretRef points to the Secret holding access_key_id and secret_access_key.
	CredentialsSecretRef BackupCredentialsSecretRef `json:"credentialsSecretRef"`
}

// StorageClusterSpec defines the desired state of StorageCluster
type StorageClusterSpec struct {
	// MgmtIfname is the management network interface name used for cluster communication.
	// FIXME: Unused for now
	MgmtIfname string `json:"mgmtIfname,omitempty"`
	// EnableNodeAffinity enables node-affinity placement for storage components.
	EnableNodeAffinity *bool `json:"enableNodeAffinity,omitempty"`
	// StripeSpec configures erasure-coding data/parity chunk counts.
	StripeSpec *StripeSpec `json:"stripe,omitempty"`
	// HAType defines the backend high-availability mode.
	HAType string `json:"haType,omitempty"`
	// ClusterName is the user-facing cluster identifier.
	ClusterName string `json:"clusterName"`
	// +kubebuilder:validation:Enum=activate;expand;shutdown;start;restart;node-recycle
	// Action triggers a cluster-level action.
	Action string `json:"action,omitempty"`
	// NodeRecycle configures the node-recycle action.
	NodeRecycle *NodeRecycleSpec `json:"nodeRecycle,omitempty"`

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
	// FabricType defines the storage fabric type.
	FabricType string `json:"fabricType,omitempty"`
	// ClientDataIfname defines the client data network interface.
	ClientDataIfname string `json:"clientDataIfname,omitempty"`
	// MaxFaultTolerance defines the maximum tolerated concurrent faults.
	MaxFaultTolerance *int32 `json:"maxFaultTolerance,omitempty"`
	// NvmfBasePort defines the base NVMf service port.
	NvmfBasePort *int32 `json:"nvmfBasePort,omitempty"`
	// RpcBasePort defines the base RPC service port.
	RpcBasePort *int32 `json:"rpcBasePort,omitempty"`
	// SnodeApiPort defines the storage-node API port.
	SnodeApiPort *int32 `json:"snodeApiPort,omitempty"`

	// QoSClasses defines backend QosSpec class configuration.
	// FIXME: Unused for now
	QoSClasses string `json:"qosClasses,omitempty"`
	// WarningThresholdSpec defines warning-level capacity thresholds.
	WarningThresholdSpec *CapacityThresholdSpec `json:"warningThreshold,omitempty"`
	// CriticalThresholdSpec defines critical-level capacity thresholds.
	CriticalThresholdSpec *CapacityThresholdSpec `json:"criticalThreshold,omitempty"`
	// ClientQpairCount defines client-side queue-pair count.
	// FIXME: Unused for now
	ClientQpairCount *int32 `json:"clientQpairCount,omitempty"`
	// IncludeEventLog controls whether event logs are included in responses/exports.
	// FIXME: Unused for now
	IncludeEventLog *bool `json:"includeEventLog,omitempty"`
	// EventLogEntries limits the number of event-log entries returned/retained.
	// FIXME: Unused for now
	EventLogEntries *int32 `json:"eventLogEntries,omitempty"`
	// Backup specifies the specification for backup to S3 configuration
	Backup *BackupSpec `json:"backup,omitempty"`
}

// StorageClusterStatus defines the observed state of StorageCluster.
type StorageClusterStatus struct {
	// UUID is the backend cluster UUID.
	UUID string `json:"uuid,omitempty"`
	// ClusterName is the resolved backend cluster name.
	ClusterName string `json:"clusterName,omitempty"`
	// MgmtNodes is the number of management nodes.
	// FIXME: Unused for now (API update required?)
	MgmtNodes *int32 `json:"mgmtNodes,omitempty"`
	// StorageNodes is the number of storage nodes.
	// FIXME: Unused for now (API update required?)
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
	// FIXME: Unused for now (API update required?)
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
	// Created is the backend creation timestamp.
	// FIXME: Unused for now (API update required?)
	Created *metav1.Time `json:"created,omitempty"`
	// Configured indicates whether initial cluster setup completed.
	Configured bool `json:"configured,omitempty"`
	// ActionStatus tracks the most recent action execution state.
	ActionStatus *ActionStatus `json:"actionStatus,omitempty"`
	// NodeRecycleStatus tracks in-progress state for the node-recycle action.
	NodeRecycleStatus *NodeRecycleStatus `json:"nodeRecycleStatus,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.status",description="Backend-reported cluster lifecycle status"
// +kubebuilder:printcolumn:name="UUID",type="string",JSONPath=".status.uuid",description="Backend cluster UUID"
// +kubebuilder:printcolumn:name="Configured",type="boolean",JSONPath=".status.configured",description="Whether initial cluster setup has completed"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// StorageCluster is the Schema for the storageclusters API
type StorageCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StorageCluster
	// +required
	Spec StorageClusterSpec `json:"spec"`

	// status defines the observed state of StorageCluster
	// +optional
	Status StorageClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StorageClusterList contains a list of StorageCluster
type StorageClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StorageCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageCluster{}, &StorageClusterList{})
}
