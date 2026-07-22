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

// StorageNodeOpsPhase is the lifecycle phase of a StorageNodeOps.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type StorageNodeOpsPhase string

const (
	StorageNodeOpsPhasePending   StorageNodeOpsPhase = "Pending"
	StorageNodeOpsPhaseRunning   StorageNodeOpsPhase = "Running"
	StorageNodeOpsPhaseSucceeded StorageNodeOpsPhase = "Succeeded"
	StorageNodeOpsPhaseFailed    StorageNodeOpsPhase = "Failed"
)

// StorageNodeOpsSubPhase is the active drain sub-phase when action=remove.
// +kubebuilder:validation:Enum=Validating;Suspending;Migrating;Verifying;Removing
type StorageNodeOpsSubPhase string

const (
	StorageNodeOpsSubPhaseValidating StorageNodeOpsSubPhase = "Validating"
	StorageNodeOpsSubPhaseSuspending StorageNodeOpsSubPhase = "Suspending"
	StorageNodeOpsSubPhaseMigrating  StorageNodeOpsSubPhase = "Migrating"
	StorageNodeOpsSubPhaseVerifying  StorageNodeOpsSubPhase = "Verifying"
	StorageNodeOpsSubPhaseRemoving   StorageNodeOpsSubPhase = "Removing"
)

// DrainOpsSpec configures the drain workflow for action=remove.
type DrainOpsSpec struct {
	// SystemVolumeFilterRegex is a Go regular expression matched against backend
	// volume names. Matching volumes are treated as system volumes: excluded from
	// drain migration and deleted inline during the Verifying phase.
	// Defaults to "^sb-fio-baseline-.*".
	// +optional
	SystemVolumeFilterRegex *string `json:"systemVolumeFilterRegex,omitempty"`
}

// StorageNodeOpsSpec defines the desired state of a StorageNodeOps.
type StorageNodeOpsSpec struct {
	// StorageNodeRef is the name of the target StorageNode. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	StorageNodeRef string `json:"storageNodeRef"`

	// Action is the operation to perform. Immutable.
	// +kubebuilder:validation:Enum=shutdown;restart;suspend;resume;remove;migrate
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action string `json:"action"`

	// TargetWorkerNode is the Kubernetes worker hostname the storage node is
	// migrated onto. Required (and only used) when action=migrate. The source
	// node is drained and removed exactly as for action=remove, then the owning
	// StorageNodeSet is re-pointed from the current worker to this one so a fresh
	// storage node is provisioned on the target host. Immutable.
	// +optional
	// +k8s:immutable
	TargetWorkerNode string `json:"targetWorkerNode,omitempty"`

	// Force enables forced execution where the backend supports it.
	// +optional
	Force *bool `json:"force,omitempty"`

	// ReattachVolume reattaches volumes during the node restart.
	// Applicable when action=restart or action=migrate.
	// +optional
	ReattachVolume *bool `json:"reattachVolume,omitempty"`

	// Drain configures the drain workflow. Only applicable when action=remove.
	// +optional
	Drain *DrainOpsSpec `json:"drain,omitempty"`
}

// StorageNodeOpsStatus holds the observed state of a StorageNodeOps.
type StorageNodeOpsStatus struct {
	// Phase is the high-level lifecycle phase.
	// +optional
	Phase StorageNodeOpsPhase `json:"phase,omitempty"`

	// SubPhase tracks the active drain step when action=remove and phase=Running.
	// +optional
	SubPhase StorageNodeOpsSubPhase `json:"subPhase,omitempty"`

	// Message is a human-readable description of the current state or failure reason.
	// +optional
	Message string `json:"message,omitempty"`

	// VolumesMigrated is the count of volumes successfully migrated (drain only).
	// +optional
	VolumesMigrated int `json:"volumesMigrated,omitempty"`

	// VolumesPending is the count of volumes awaiting migration (drain only).
	// +optional
	VolumesPending int `json:"volumesPending,omitempty"`

	// Triggered indicates the backend action POST has been sent (used during
	// Suspending to avoid duplicate POSTs across reconcile iterations).
	// +optional
	Triggered bool `json:"triggered,omitempty"`

	// StartedAt is when the operation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation finished (successfully or not).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=snops
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.storageNodeRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="SubPhase",type=string,JSONPath=".status.subPhase"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageNodeOps is a one-shot operational CR targeting a single StorageNode.
// Analogous to a Kubernetes Job — it drives an action (shutdown, restart, suspend,
// resume, remove/drain) to completion and records the result. Only one
// StorageNodeOps can be active per StorageNode at a time.
type StorageNodeOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageNodeOpsSpec   `json:"spec,omitempty"`
	Status StorageNodeOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageNodeOpsList contains a list of StorageNodeOps.
type StorageNodeOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageNodeOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageNodeOps{}, &StorageNodeOpsList{})
}
