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
	BackupImportPhasePending   = "Pending"
	BackupImportPhaseExporting = "Exporting"
	BackupImportPhaseImporting = "Importing"
	BackupImportPhaseDone      = "Done"
	BackupImportPhaseFailed    = "Failed"
)

// BackupImportSpec defines the desired state of BackupImport.
type BackupImportSpec struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Source Cluster Name"
	// SourceClusterName is the StorageCluster CR name of the cluster that owns the backup.
	SourceClusterName string `json:"sourceClusterName"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Source Backup ID"
	// SourceBackupID is the UUID of the backup on the source cluster to import.
	SourceBackupID string `json:"sourceBackupID"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Target Cluster Name"
	// TargetClusterName is the StorageCluster CR name of the cluster to import into.
	TargetClusterName string `json:"targetClusterName"`
}

// BackupImportStatus defines the observed state of BackupImport.
type BackupImportStatus struct {
	// Phase is the high-level lifecycle shown in kubectl output.
	Phase string `json:"phase,omitempty"`
	// Message contains the latest reconciliation detail or error.
	Message string `json:"message,omitempty"`

	// SourceClusterUUID is the resolved UUID of the source cluster.
	SourceClusterUUID string `json:"sourceClusterUUID,omitempty"`
	// TargetClusterUUID is the resolved UUID of the target cluster.
	TargetClusterUUID string `json:"targetClusterUUID,omitempty"`

	// ImportedBackupID is the backup UUID after successful import into the target cluster.
	ImportedBackupID string `json:"importedBackupID,omitempty"`

	// StorageBackupRef is the name of the StorageBackup CR created in the target namespace
	// after a successful import. This CR can be referenced directly in a BackupRestore.
	StorageBackupRef string `json:"storageBackupRef,omitempty"`

	// CompletedAt is when the import completed.
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=bi
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=".spec.sourceClusterName"
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".spec.targetClusterName"
// +kubebuilder:printcolumn:name="BackupRef",type=string,JSONPath=".status.storageBackupRef"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// BackupImport imports a completed backup from a source cluster into a target cluster,
// creating a StorageBackup CR that can be referenced by a BackupRestore.
type BackupImport struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BackupImport
	// +required
	Spec BackupImportSpec `json:"spec"`

	// status defines the observed state of BackupImport
	// +optional
	Status BackupImportStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupImportList contains a list of BackupImport.
type BackupImportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BackupImport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupImport{}, &BackupImportList{})
}
