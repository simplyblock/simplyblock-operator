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

// ReplicationPairSpec defines the source and target clusters for a replication relationship.
type ReplicationPairSpec struct {
	// SourceCluster is the name of the local StorageCluster (the replication source).
	// +kubebuilder:validation:Required
	SourceCluster string `json:"sourceCluster"`

	// TargetCluster is the name or UUID of the remote cluster (the replication target).
	// Immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetCluster is immutable"
	TargetCluster string `json:"targetCluster"`
}

// ReplicationPairStatus holds the observed state of a ReplicationPair.
type ReplicationPairStatus struct {
	// Ready is true when the backend ReplicationTarget has been created and is available.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// BackendTargetID is the UUID of the backend ReplicationTarget resource.
	// +optional
	BackendTargetID string `json:"backendTargetID,omitempty"`

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
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=".spec.sourceCluster"
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".spec.targetCluster"
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ReplicationPair defines the source and target clusters for a replication relationship.
// It is reusable configuration — multiple ReplicationPolicies may reference the same pair
// to replicate volumes between the same two clusters with different schedules or retention.
// The operator ensures the corresponding backend ReplicationTarget exists and stores its ID
// in status.backendTargetID for use by ReplicationPolicy resources.
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
