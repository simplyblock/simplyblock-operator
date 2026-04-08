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

type NamedReference struct {
	Name string `json:"name"`
}

type BackupSourceSpec struct {
	VolumeRef NamedReference `json:"volumeRef"`
}

type BackupSnapshotSpec struct {
	Name   string `json:"name,omitempty"`
	Retain bool   `json:"retain,omitempty"`
}

// SimplyBlockBackupSpec defines the desired state of SimplyBlockBackup.
type SimplyBlockBackupSpec struct {
	ClusterName string             `json:"clusterName"`
	PoolName    string             `json:"poolName"`
	Source      BackupSourceSpec   `json:"source"`
	Snapshot    BackupSnapshotSpec `json:"snapshot,omitempty"`
}

// SimplyBlockBackupStatus defines the observed state of SimplyBlockBackup.
type SimplyBlockBackupStatus struct {
	Phase              string       `json:"phase,omitempty"`
	Message            string       `json:"message,omitempty"`
	SourceVolumeID     string       `json:"sourceVolumeID,omitempty"`
	SourceVolumeName   string       `json:"sourceVolumeName,omitempty"`
	SnapshotID         string       `json:"snapshotID,omitempty"`
	SnapshotName       string       `json:"snapshotName,omitempty"`
	SnapshotDeleted    bool         `json:"snapshotDeleted,omitempty"`
	BackupID           string       `json:"backupID,omitempty"`
	S3ID               *int64       `json:"s3ID,omitempty"`
	PreviousBackupID   string       `json:"previousBackupID,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	CreatedAt          *metav1.Time `json:"createdAt,omitempty"`
	CompletedAt        *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=".spec.source.volumeRef.name"
// +kubebuilder:printcolumn:name="BackupID",type=string,JSONPath=".status.backupID"
// +kubebuilder:printcolumn:name="Snapshot",type=string,JSONPath=".status.snapshotName"

// SimplyBlockBackup is the Schema for the simplyblockbackups API.
type SimplyBlockBackup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SimplyBlockBackup
	// +required
	Spec SimplyBlockBackupSpec `json:"spec"`

	// status defines the observed state of SimplyBlockBackup
	// +optional
	Status SimplyBlockBackupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimplyBlockBackupList contains a list of SimplyBlockBackup.
type SimplyBlockBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimplyBlockBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimplyBlockBackup{}, &SimplyBlockBackupList{})
}
