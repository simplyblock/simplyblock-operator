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

const (
	BackupPhasePending    = "Pending"
	BackupPhaseInProgress = "InProgress"
	BackupPhaseDone       = "Done"
	BackupPhaseFailed     = "Failed"
	BackupPhaseMerging    = "Merging"
	BackupPhaseDeleting   = "Deleting"
)

type PersistentVolumeClaimRef struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="PVC Name"
	// Name is the PVC name.
	Name string `json:"name"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="PVC Namespace"
	// Namespace overrides the backup resource namespace for the PVC lookup.
	Namespace string `json:"namespace,omitempty"`
}

// StorageBackupSpec defines the desired state of StorageBackup.
type StorageBackupSpec struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Cluster Name"
	// ClusterName is the target storage cluster name.
	ClusterName string `json:"clusterName"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="PVC Ref"
	// PVCRef identifies the PVC whose backing Simplyblock volume should be snapshotted and backed up.
	// Not required when SourceClusterUUID is set (imported backup).
	// +optional
	PVCRef *PersistentVolumeClaimRef `json:"pvcRef,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Snapshot Name"
	// SnapshotName optionally overrides the internally-created snapshot name.
	// +optional
	SnapshotName string `json:"snapshotName,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Source Cluster UUID"
	// SourceClusterUUID, when non-empty, marks this StorageBackup as imported from another cluster.
	// The StorageBackup controller will not create snapshots or backups for imported resources.
	// Set by the BackupImport controller; do not set manually.
	// +optional
	SourceClusterUUID string `json:"sourceClusterUUID,omitempty"`
}

// StorageBackupStatus defines the observed state of StorageBackup.
type StorageBackupStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Phase"
	// Phase is the high-level backup lifecycle shown in kubectl output.
	Phase string `json:"phase,omitempty"`
	// APIStatus is the raw status returned by the backup API.
	APIStatus string `json:"apiStatus,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Message"
	// Message contains the latest reconciliation detail or error.
	Message string `json:"message,omitempty"`

	// ClusterUUID is the backend cluster UUID.
	ClusterUUID string `json:"clusterUUID,omitempty"`
	// PVCNamespace is the resolved PVC namespace.
	PVCNamespace string `json:"pvcNamespace,omitempty"`
	// PVName is the bound PV name.
	PVName string `json:"pvName,omitempty"`
	// PoolName is the Simplyblock pool name derived from the CSI volume handle.
	PoolName string `json:"poolName,omitempty"`
	// PoolUUID is the backend pool UUID.
	PoolUUID string `json:"poolUUID,omitempty"`
	// LvolID is the Simplyblock volume UUID.
	LvolID string `json:"lvolID,omitempty"`
	// LvolName is the backend logical volume name.
	LvolName string `json:"lvolName,omitempty"`

	// SnapshotID is the internally-created snapshot UUID used for the backup request.
	SnapshotID string `json:"snapshotID,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Snapshot Name"
	// SnapshotName is the snapshot name used for the backup request.
	SnapshotName string `json:"snapshotName,omitempty"`

	// SourceClusterUUID is set for imported backups; identifies the cluster that originally
	// created the backup. When non-empty and different from the restore target cluster UUID,
	// BackupRestore will automatically perform source-switch operations around the restore.
	SourceClusterUUID string `json:"sourceClusterUUID,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Backup ID"
	// BackupID is the backend backup UUID.
	BackupID string `json:"backupID,omitempty"`
	// S3ID is the backend S3 object identifier.
	S3ID int64 `json:"s3ID,omitempty"`
	// NodeID is the source storage node UUID.
	NodeID string `json:"nodeID,omitempty"`
	// PrevBackupID links the previous backup in the chain.
	PrevBackupID string `json:"prevBackupID,omitempty"`
	// Size is the backup size in bytes.
	Size int64 `json:"size,omitempty"`
	// AllowedHosts contains the allowed host metadata returned by the backup API.
	AllowedHosts []map[string]string `json:"allowedHosts,omitempty"`
	// CreatedAt is when the backup was created.
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`
	// CompletedAt is when the backup completed.
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=".spec.pvcRef.name"
// +kubebuilder:printcolumn:name="BackupID",type=string,JSONPath=".status.backupID"
// +kubebuilder:printcolumn:name="Snapshot",type=string,JSONPath=".status.snapshotName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +operator-sdk:csv:customresourcedefinitions:displayName="Storage Backup",resources={{PersistentVolume,v1,source-volume},{PersistentVolumeClaim,v1,source-claim}}

// StorageBackup is the Schema for the storagebackups API.
type StorageBackup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StorageBackup
	// +required
	Spec StorageBackupSpec `json:"spec"`

	// status defines the observed state of StorageBackup
	// +optional
	Status StorageBackupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StorageBackupList contains a list of StorageBackup.
type StorageBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StorageBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageBackup{}, &StorageBackupList{})
}
