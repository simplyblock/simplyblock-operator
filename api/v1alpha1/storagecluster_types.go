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
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Data Chunks"
	// DataChunks defines the number of data chunks in the erasure-coding layout.
	DataChunks *int32 `json:"dataChunks,omitempty"`
	// ParityChunks defines the number of parity chunks in the erasure-coding layout.
	ParityChunks *int32 `json:"parityChunks,omitempty"`
}

// NodeRecycleSpec configures the node-recycle action behaviour.
type NodeRecycleSpec struct {
	// RefreshSNodeAPI restarts the storage-node DaemonSet pod on each node
	// after the backend node is shut down and before it is restarted, ensuring
	// the latest image is running before the node comes back online.
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
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Backup Credentials Secret"
	// Name is the name of the Secret in the same namespace as the cluster CR.
	Name string `json:"name"`
}

// HashicorpVaultSettings configures the HashiCorp Vault endpoint the cluster uses to store keys.
type HashicorpVaultSettings struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Vault Base URL"
	// BaseURL is the HashiCorp Vault endpoint (e.g. https://vault.example.com:8200).
	BaseURL string `json:"baseURL,omitempty"`
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
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Enable Node Affinity"
	// EnableNodeAffinity enables node-affinity placement for storage components.
	EnableNodeAffinity *bool `json:"enableNodeAffinity,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Stripe"
	// StripeSpec configures erasure-coding data/parity chunk counts.
	StripeSpec *StripeSpec `json:"stripe,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="HA Type"
	// HAType defines the backend high-availability mode.
	HAType string `json:"haType,omitempty"`
	// +kubebuilder:validation:Enum=activate;expand;shutdown;start;restart;node-recycle
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Action"
	// Action triggers a cluster-level action.
	Action string `json:"action,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Node Recycle"
	// NodeRecycle configures the node-recycle action.
	NodeRecycle *NodeRecycleSpec `json:"nodeRecycle,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Single Node"
	// IsSingleNode enables single-node cluster mode.
	IsSingleNode *bool `json:"isSingleNode,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Strict Node Anti-Affinity"
	// StrictNodeAntiAffinity enforces strict anti-affinity between storage nodes.
	StrictNodeAntiAffinity *bool `json:"strictNodeAntiAffinity,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Queue Pair Count"
	// QpairCount defines the NVMe queue-pair count used by the cluster.
	QpairCount *int32 `json:"qpairCount,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Block Size"
	// BlockSize defines the logical block size in bytes.
	BlockSize *int32 `json:"blockSize,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Page Size In Blocks"
	// PageSizeInBlocks defines page size expressed in blocks.
	PageSizeInBlocks *int32 `json:"pageSizeInBlocks,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Max Queue Size"
	// MaxQueueSize defines the maximum backend queue size.
	MaxQueueSize *int32 `json:"maxQueueSize,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Inflight IO Threshold"
	// InflightIOThreshold defines the inflight I/O threshold.
	InflightIOThreshold *int32 `json:"inflightIOThreshold,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Fabric Type"
	// FabricType defines the storage fabric type.
	FabricType string `json:"fabricType,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Client Data Interface"
	// ClientDataIfname defines the client data network interface.
	ClientDataIfname string `json:"clientDataIfname,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Max Fault Tolerance"
	// MaxFaultTolerance defines the maximum tolerated concurrent faults.
	MaxFaultTolerance *int32 `json:"maxFaultTolerance,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="NVMf Base Port"
	// NvmfBasePort defines the base NVMf service port.
	NvmfBasePort *int32 `json:"nvmfBasePort,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="RPC Base Port"
	// RpcBasePort defines the base RPC service port.
	RpcBasePort *int32 `json:"rpcBasePort,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Storage Node API Port"
	// SnodeApiPort defines the storage-node API port.
	SnodeApiPort *int32 `json:"snodeApiPort,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Warning Threshold"
	// WarningThresholdSpec defines warning-level capacity thresholds.
	WarningThresholdSpec *CapacityThresholdSpec `json:"warningThreshold,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Critical Threshold"
	// CriticalThresholdSpec defines critical-level capacity thresholds.
	CriticalThresholdSpec *CapacityThresholdSpec `json:"criticalThreshold,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Client Queue Pair Count"
	// ClientQpairCount defines client-side queue-pair count.
	ClientQpairCount *int32 `json:"clientQpairCount,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Backup"
	// Backup specifies the specification for backup to S3 configuration
	Backup *BackupSpec `json:"backup,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="HashiCorp Vault Settings"
	// HashicorpVaultSettings configures the Vault endpoint used by the cluster for key storage.
	HashicorpVaultSettings *HashicorpVaultSettings `json:"hashicorpVaultSettings,omitempty"`
}

// StorageClusterStatus defines the observed state of StorageCluster.
type StorageClusterStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Cluster UUID"
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
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Status"
	// Status is the backend-reported lifecycle status.
	Status string `json:"status,omitempty"`
	// Rebalancing indicates whether cluster rebalancing is currently active.
	Rebalancing *bool `json:"rebalancing,omitempty"`
	// ErasureCodingScheme is the active erasure-coding layout, for example "2x1".
	ErasureCodingScheme string `json:"erasureCodingScheme,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Cluster Secret"
	// SecretName is the Kubernetes Secret containing cluster credentials.
	SecretName string `json:"secretName,omitempty"`
	// LastUpdated is the last backend update timestamp.
	// FIXME: Unused for now (API update required?)
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
	// Created is the backend creation timestamp.
	// FIXME: Unused for now (API update required?)
	Created *metav1.Time `json:"created,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Configured"
	// Configured indicates whether initial cluster setup completed.
	Configured bool `json:"configured,omitempty"`
	// MaxFaultTolerance is the backend-reported maximum number of nodes that can
	// be simultaneously offline (failed, drained, or restarted) without violating
	// the cluster's redundancy guarantees.
	MaxFaultTolerance *int32 `json:"maxFaultTolerance,omitempty"`
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
// +operator-sdk:csv:customresourcedefinitions:displayName="Storage Cluster",resources={{Secret,v1,simplyblock-cluster-credentials}}

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
