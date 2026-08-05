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

// StorageClusterOpsPhase is the lifecycle phase of a StorageClusterOps.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type StorageClusterOpsPhase string

const (
	StorageClusterOpsPhasePending   StorageClusterOpsPhase = "Pending"
	StorageClusterOpsPhaseRunning   StorageClusterOpsPhase = "Running"
	StorageClusterOpsPhaseSucceeded StorageClusterOpsPhase = "Succeeded"
	StorageClusterOpsPhaseFailed    StorageClusterOpsPhase = "Failed"
)

// StorageClusterOpsSpec defines the desired state of a StorageClusterOps.
type StorageClusterOpsSpec struct {
	// ClusterRef is the name of the target SimplyblocksStorageCluster. Immutable.
	// +kubebuilder:validation:Required
	ClusterRef string `json:"clusterRef"`

	// Action is the operation to perform. Immutable.
	// +kubebuilder:validation:Enum=activate;expand;shutdown;restart;node-recycle
	// +kubebuilder:validation:Required
	Action string `json:"action"`

	// NodeUUID is the target node UUID, required only when action=node-recycle.
	// +optional
	NodeUUID string `json:"nodeUUID,omitempty"`
}

// StorageClusterOpsStatus holds the observed state of a StorageClusterOps.
type StorageClusterOpsStatus struct {
	// Phase is the high-level lifecycle phase.
	// +optional
	Phase StorageClusterOpsPhase `json:"phase,omitempty"`

	// Triggered indicates the backend POST has been sent for this operation.
	// Guards against duplicate backend calls on retry.
	// +optional
	Triggered bool `json:"triggered,omitempty"`

	// Message is a human-readable description of the current state or failure reason.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when the operation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation finished (successfully or not).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=scops
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageClusterOps is a one-shot operational CR targeting a single SimplyblocksStorageCluster.
// Analogous to a Kubernetes Job — it drives a cluster-level operation (activate, expand,
// shutdown, restart, node-recycle) to completion and records the result. Only one
// StorageClusterOps can be active per cluster at a time.
type StorageClusterOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageClusterOpsSpec   `json:"spec,omitempty"`
	Status StorageClusterOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageClusterOpsList contains a list of StorageClusterOps.
type StorageClusterOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageClusterOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageClusterOps{}, &StorageClusterOpsList{})
}
