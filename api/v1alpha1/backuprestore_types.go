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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	RestorePhasePending    = "Pending"
	RestorePhaseInProgress = "InProgress"
	RestorePhasePVCBinding = "PVCBinding"
	RestorePhaseDone       = "Done"
	RestorePhaseFailed     = "Failed"

	// Cross-cluster phases: inserted between Pending and InProgress, and between InProgress and PVCBinding.
	RestorePhaseSwitchingSource      = "SwitchingSource"
	RestorePhaseSwitchingSourceLocal = "SwitchingSourceLocal"
)

// BackupRef identifies the StorageBackup to restore from, scoped to the same namespace.
type BackupRef struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Backup Name"
	// Name is the StorageBackup resource name.
	Name string `json:"name"`
}

// PVCTemplateMetadata describes the PVC metadata fields the controller honors.
type PVCTemplateMetadata struct {
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PVCTemplate describes the PVC the controller will create once the restore completes.
type PVCTemplate struct {
	// +optional
	Metadata PVCTemplateMetadata `json:"metadata,omitempty"`
	// Spec follows core PersistentVolumeClaimSpec.
	// spec.resources.requests.storage must be >= the backup size.
	Spec corev1.PersistentVolumeClaimSpec `json:"spec"`
}

// BackupRestoreSpec defines the desired state of BackupRestore.
type BackupRestoreSpec struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Cluster Name"
	// ClusterName is the target storage cluster name.
	ClusterName string `json:"clusterName"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Backup Ref"
	// BackupRef references the StorageBackup resource to restore from.
	BackupRef BackupRef `json:"backupRef"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Target Pool"
	// TargetPool overrides the pool to restore into.
	// Defaults to the source backup's pool.
	// +optional
	TargetPool string `json:"targetPool,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Target Node"
	// TargetNode is the UUID of the storage node to restore onto.
	// Defaults to the node that originally held the backup.
	// +optional
	TargetNode string `json:"targetNode,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="PVC Template"
	// PVCTemplate describes the PVC to create once the restore completes.
	PVCTemplate PVCTemplate `json:"pvcTemplate"`
}

// BackupRestoreStatus defines the observed state of BackupRestore.
type BackupRestoreStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Phase"
	// Phase is the high-level lifecycle shown in kubectl output.
	Phase string `json:"phase,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Message"
	// Message contains the latest reconciliation detail or error.
	Message string `json:"message,omitempty"`

	// ClusterUUID is the backend cluster UUID.
	ClusterUUID string `json:"clusterUUID,omitempty"`
	// BackupID is the backend backup UUID being restored.
	BackupID string `json:"backupID,omitempty"`
	// SourceLvolID is the original logical volume UUID that was backed up.
	SourceLvolID string `json:"sourceLvolID,omitempty"`

	// PoolName is the pool the restore was issued against.
	PoolName string `json:"poolName,omitempty"`
	// PoolUUID is the backend pool UUID.
	PoolUUID string `json:"poolUUID,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Restored LVOL ID"
	// RestoredLvolID is the UUID of the newly-created logical volume.
	RestoredLvolID string `json:"restoredLvolID,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Persistent Volume"
	// PVName is the name of the PersistentVolume created by the controller.
	PVName string `json:"pvName,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="PersistentVolumeClaim"
	// PVCName is the name of the PersistentVolumeClaim created from pvcTemplate.
	PVCName string `json:"pvcName,omitempty"`
	// PVCNamespace is the namespace of the created PVC.
	PVCNamespace string `json:"pvcNamespace,omitempty"`

	// SourceClusterUUID is the UUID of the cluster that originally created the backup.
	// Copied from the referenced StorageBackup's status.sourceClusterUUID.
	// When non-empty, the controller performs source-switch before and after the restore.
	SourceClusterUUID string `json:"sourceClusterUUID,omitempty"`

	// SourceSwitchedAt records when the target cluster was switched to read from the
	// source cluster's S3 bucket. Cleared once source-switch local completes.
	SourceSwitchedAt *metav1.Time `json:"sourceSwitchedAt,omitempty"`

	// StartedAt is when the backend restore task was accepted.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the PVC became bound.
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=br
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=".spec.backupRef.name"
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=".status.pvcName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +operator-sdk:csv:customresourcedefinitions:displayName="Backup Restore",resources={{PersistentVolume,v1,restored-volume},{PersistentVolumeClaim,v1,restored-claim}}

// BackupRestore is the Schema for the backuprestores API.
type BackupRestore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BackupRestore
	// +required
	Spec BackupRestoreSpec `json:"spec"`

	// status defines the observed state of BackupRestore
	// +optional
	Status BackupRestoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupRestoreList contains a list of BackupRestore.
type BackupRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BackupRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupRestore{}, &BackupRestoreList{})
}
