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
	StorageNodeOperationPhasePending   = "Pending"
	StorageNodeOperationPhaseRunning   = "Running"
	StorageNodeOperationPhaseCompleted = "Completed"
	StorageNodeOperationPhaseFailed    = "Failed"
)

// StorageNodeOperationSpec defines a one-shot imperative operation on a storage node.
type StorageNodeOperationSpec struct {
	// StorageNodeRef is the name of the StorageNode CR this operation targets.
	StorageNodeRef string `json:"storageNodeRef"`
	// NodeUUID is the backend storage node UUID to operate on.
	NodeUUID string `json:"nodeUUID"`
	// +kubebuilder:validation:Enum=restart;shutdown;suspend;resume;remove
	// Action is the operation to perform.
	Action string `json:"action"`
	// WorkerNode is a migration target worker for restart operations.
	// When set, the SPDK stack is restarted on this new worker node instead of in-place.
	WorkerNode string `json:"workerNode,omitempty"`
	// ReattachVolume reattaches volumes during restart where supported by the backend.
	ReattachVolume *bool `json:"reattachVolume,omitempty"`
	// Force enables forced action execution where supported.
	Force *bool `json:"force,omitempty"`
}

// StorageNodeOperationStatus reports the observed state of a StorageNodeOperation.
type StorageNodeOperationStatus struct {
	// +kubebuilder:validation:Enum=Pending;Running;Completed;Failed
	// Phase is the current execution phase.
	Phase string `json:"phase,omitempty"`
	// Message is a human-readable status detail or error.
	Message string `json:"message,omitempty"`
	// StartedAt is when the operation began execution.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the operation finished (Completed or Failed).
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// Triggered indicates whether the underlying backend API call has been made.
	Triggered bool `json:"triggered,omitempty"`
	// ObservedGeneration is the resource generation observed by this status.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="has(self.spec.nodeUUID) && self.spec.nodeUUID != \"\"",message="nodeUUID is required"
// +operator-sdk:csv:customresourcedefinitions:displayName="Storage Node Operation"
// StorageNodeOperation is the Schema for the storagenodeoperations API.
// It represents a single imperative lifecycle operation (restart, shutdown, suspend, resume, remove)
// on a storage node managed by a StorageNode CR.
type StorageNodeOperation struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired operation
	// +required
	Spec StorageNodeOperationSpec `json:"spec"`

	// status defines the observed state of the operation
	// +optional
	Status StorageNodeOperationStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StorageNodeOperationList contains a list of StorageNodeOperation.
type StorageNodeOperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StorageNodeOperation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageNodeOperation{}, &StorageNodeOperationList{})
}
