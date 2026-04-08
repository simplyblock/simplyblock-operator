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

type RestoreSourceSpec struct {
	BackupRef NamedReference `json:"backupRef"`
}

type RestoreTargetSpec struct {
	VolumeName string `json:"volumeName"`
}

// SimplyBlockRestoreSpec defines the desired state of SimplyBlockRestore.
type SimplyBlockRestoreSpec struct {
	ClusterName string            `json:"clusterName"`
	PoolName    string            `json:"poolName"`
	Source      RestoreSourceSpec `json:"source"`
	Target      RestoreTargetSpec `json:"target"`
}

// SimplyBlockRestoreStatus defines the observed state of SimplyBlockRestore.
type SimplyBlockRestoreStatus struct {
	Phase              string       `json:"phase,omitempty"`
	Message            string       `json:"message,omitempty"`
	BackupID           string       `json:"backupID,omitempty"`
	TargetVolumeID     string       `json:"targetVolumeID,omitempty"`
	TargetVolumeName   string       `json:"targetVolumeName,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	StartedAt          *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt        *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=".spec.source.backupRef.name"
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".spec.target.volumeName"

// SimplyBlockRestore is the Schema for the simplyblockrestores API.
type SimplyBlockRestore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SimplyBlockRestore
	// +required
	Spec SimplyBlockRestoreSpec `json:"spec"`

	// status defines the observed state of SimplyBlockRestore
	// +optional
	Status SimplyBlockRestoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimplyBlockRestoreList contains a list of SimplyBlockRestore.
type SimplyBlockRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimplyBlockRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimplyBlockRestore{}, &SimplyBlockRestoreList{})
}
