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

// ReplicationSlotState is the lifecycle state of a ReplicationSlot.
type ReplicationSlotState string

const (
	ReplicationSlotStateAttaching      ReplicationSlotState = "attaching"
	ReplicationSlotStateReplicating    ReplicationSlotState = "replicating"
	ReplicationSlotStateCutoverPending ReplicationSlotState = "cutover_pending"
	ReplicationSlotStateCutoverDone    ReplicationSlotState = "cutover_done"
	ReplicationSlotStateFailedOver     ReplicationSlotState = "failed_over"
	ReplicationSlotStateDetaching      ReplicationSlotState = "detaching"
	ReplicationSlotStateError          ReplicationSlotState = "error"
)

// ReplicationSlotDirection indicates which side of the replication relationship
// this cluster holds for a given slot.
type ReplicationSlotDirection string

const (
	ReplicationSlotDirectionSource ReplicationSlotDirection = "source"
	ReplicationSlotDirectionTarget ReplicationSlotDirection = "target"
)

// ReplicationSlotSpec defines the identity of a per-volume replication slot.
type ReplicationSlotSpec struct {
	// PolicyRef is the name of the ReplicationPolicy governing this slot. Immutable.
	// +kubebuilder:validation:Required
	PolicyRef string `json:"policyRef"`

	// PVCRef is the name of the PVC being replicated. Immutable.
	// +kubebuilder:validation:Required
	PVCRef string `json:"pvcRef"`

	// VolumeID is the backend lvol UUID of the source volume. Immutable.
	// Format: "<clusterUUID>:<poolUUID>:<volumeUUID>"
	// +kubebuilder:validation:Required
	VolumeID string `json:"volumeID"`
}

// ReplicationSlotStatus holds the observed state of a ReplicationSlot.
type ReplicationSlotStatus struct {
	// State is the current replication state for this slot.
	// +kubebuilder:validation:Enum=attaching;replicating;cutover_pending;cutover_done;failed_over;detaching;error
	// +optional
	State string `json:"state,omitempty"`

	// Direction is which side of the replication relationship this cluster holds.
	// +kubebuilder:validation:Enum=source;target
	// +optional
	Direction string `json:"direction,omitempty"`

	// SourceLvolID is the UUID of the source volume on the source cluster.
	// +optional
	SourceLvolID string `json:"sourceLvolID,omitempty"`

	// TargetLvolID is the UUID of the replicated volume on the target cluster.
	// +optional
	TargetLvolID string `json:"targetLvolID,omitempty"`

	// TargetNQN is the NVMe NQN on the target cluster (populated after failover).
	// +optional
	TargetNQN string `json:"targetNQN,omitempty"`

	// LastReplicatedAt is the timestamp of the last successful replication snapshot.
	// +optional
	LastReplicatedAt *metav1.Time `json:"lastReplicatedAt,omitempty"`

	// Message provides a human-readable description of the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions holds standard Kubernetes condition types.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=relslot
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=".spec.policyRef"
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=".spec.pvcRef"
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=".status.state"
// +kubebuilder:printcolumn:name="Direction",type=string,JSONPath=".status.direction"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ReplicationSlot tracks the live replication state for a single PVC.
// One ReplicationSlot is created per PVC by the PVCAnnotationWatcher controller when
// a PVC references a ReplicationPolicy via annotation. It is owned by its PVC, so
// deleting the PVC cascades deletion and triggers a backend detach via the slot finalizer.
// The ReplicationSlot reconciler drives all backend calls: attach, monitor, cutover,
// failover, and detach.
type ReplicationSlot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReplicationSlotSpec   `json:"spec,omitempty"`
	Status ReplicationSlotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReplicationSlotList contains a list of ReplicationSlot.
type ReplicationSlotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReplicationSlot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReplicationSlot{}, &ReplicationSlotList{})
}
