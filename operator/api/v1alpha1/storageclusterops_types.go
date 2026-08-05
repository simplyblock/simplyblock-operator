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

// NodeRecycleStatus tracks in-progress state for the node-recycle action.
// All fields are persisted in the StorageClusterOps status so the reconciler
// can resume after a requeue or operator restart.
type NodeRecycleStatus struct {
	// PendingNodes is the ordered list of node UUIDs still to be recycled.
	PendingNodes []string `json:"pendingNodes,omitempty"`
	// ProcessedNodes is the list of node UUIDs already recycled.
	ProcessedNodes []string `json:"processedNodes,omitempty"`
	// NodePhase is the current step for the node being recycled:
	// "snode-refresh" | "snode-refresh-wait" | "shutting-down" | "restarting" | "rebalancing"
	NodePhase string `json:"nodePhase,omitempty"`
	// PhaseTriggered indicates the API call for the current NodePhase was already sent.
	PhaseTriggered bool `json:"phaseTriggered,omitempty"`
}

// NodeRecycleSpec configures the node-recycle action behaviour.
type NodeRecycleSpec struct {
	// RefreshSNodeAPI restarts the storage-node DaemonSet pod on each node
	// after the backend node is shut down and before it is restarted, ensuring
	// the latest image is running before the node comes back online.
	// +optional
	RefreshSNodeAPI bool `json:"refreshSNodeAPI,omitempty"`
}

// StorageClusterOpsSpec defines the desired state of a StorageClusterOps.
type StorageClusterOpsSpec struct {
	// ClusterRef is the name of the target SimplyblocksStorageCluster. Immutable.
	// +kubebuilder:validation:Required
	ClusterRef string `json:"clusterRef"`

	// Action is the operation to perform. Immutable.
	// +kubebuilder:validation:Enum=activate;expand;shutdown;start;restart;node-recycle
	// +kubebuilder:validation:Required
	Action string `json:"action"`

	// NodeRecycle configures behaviour specific to the node-recycle action.
	// Ignored for all other actions.
	// +optional
	NodeRecycle *NodeRecycleSpec `json:"nodeRecycle,omitempty"`
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

	// NodeRecycleStatus tracks per-node progress for the node-recycle action.
	// Nil for all other actions.
	// +optional
	NodeRecycleStatus *NodeRecycleStatus `json:"nodeRecycleStatus,omitempty"`
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
