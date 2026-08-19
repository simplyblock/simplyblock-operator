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

// ReplicationPairState is the lifecycle state of a ReplicationPair.
type ReplicationPairState string

const (
	ReplicationPairStateAttaching      ReplicationPairState = "attaching"
	ReplicationPairStateReplicating    ReplicationPairState = "replicating"
	ReplicationPairStateCutoverPending ReplicationPairState = "cutover_pending"
	ReplicationPairStateCutoverDone    ReplicationPairState = "cutover_done"
	ReplicationPairStateFailedOver     ReplicationPairState = "failed_over"
	ReplicationPairStateDetaching      ReplicationPairState = "detaching"
	ReplicationPairStateError          ReplicationPairState = "error"
)

// ReplicationPairDirection indicates which side of the replication relationship
// this cluster holds.
type ReplicationPairDirection string

const (
	ReplicationPairDirectionSource ReplicationPairDirection = "source"
	ReplicationPairDirectionTarget ReplicationPairDirection = "target"
)

// ReplicationPairSpec defines the immutable identity of a replication relationship.
type ReplicationPairSpec struct {
	// PolicyRef is the name of the ReplicationPolicy that owns this pair. Immutable.
	// +kubebuilder:validation:Required
	PolicyRef string `json:"policyRef"`

	// PVCRef is the name of the PVC being replicated. Immutable.
	// +kubebuilder:validation:Required
	PVCRef string `json:"pvcRef"`

	// VolumeID is the backend lvol UUID of the source volume. Immutable.
	// +kubebuilder:validation:Required
	VolumeID string `json:"volumeID"`
}

// ReplicationPairStatus holds the observed state of a ReplicationPair.
type ReplicationPairStatus struct {
	// State is the current replication state for this pair.
	// +kubebuilder:validation:Enum=attaching;replicating;cutover_pending;cutover_done;failed_over;detaching;error
	// +optional
	State string `json:"state,omitempty"`

	// Direction is which side of the replication relationship this cluster holds.
	// +kubebuilder:validation:Enum=source;target
	// +optional
	Direction string `json:"direction,omitempty"`

	// SourceLvolID is the UUID of the source volume on Cluster A.
	// +optional
	SourceLvolID string `json:"sourceLvolID,omitempty"`

	// TargetLvolID is the UUID of the replicated volume on Cluster B.
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
// +kubebuilder:resource:scope=Namespaced,shortName=relpair
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=".spec.policyRef"
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=".spec.pvcRef"
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=".status.state"
// +kubebuilder:printcolumn:name="Direction",type=string,JSONPath=".status.direction"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ReplicationPair tracks the live replication relationship between a source PVC
// and its replicated counterpart on a target cluster. One ReplicationPair is
// created per PVC by the PVCAnnotationWatcher controller and owned by that PVC,
// so deleting the PVC triggers garbage collection of the pair. The ReplicationPair
// reconciler drives all backend calls: attach, monitor, cutover, failover, and detach.
type ReplicationPair struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReplicationPairSpec   `json:"spec,omitempty"`
	Status ReplicationPairStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReplicationPairList contains a list of ReplicationPair.
type ReplicationPairList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReplicationPair `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReplicationPair{}, &ReplicationPairList{})
}
