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

// ReplicationOpsPhase is the lifecycle phase of a ReplicationOps.
type ReplicationOpsPhase string

const (
	ReplicationOpsPhasePending   ReplicationOpsPhase = "Pending"
	ReplicationOpsPhaseRunning   ReplicationOpsPhase = "Running"
	ReplicationOpsPhaseSucceeded ReplicationOpsPhase = "Succeeded"
	ReplicationOpsPhaseFailed    ReplicationOpsPhase = "Failed"
)

// ReplicationOpsResultStatus is the per-volume outcome of a ReplicationOps.
type ReplicationOpsResultStatus string

const (
	ReplicationOpsResultSucceeded ReplicationOpsResultStatus = "succeeded"
	ReplicationOpsResultSkipped   ReplicationOpsResultStatus = "skipped"
	ReplicationOpsResultFailed    ReplicationOpsResultStatus = "failed"
)

// ReplicationOpsResult holds the outcome for a single volume in a ReplicationOps.
type ReplicationOpsResult struct {
	// SlotRef is the name of the ReplicationSlot CR.
	SlotRef string `json:"slotRef"`

	// Status is the outcome for this volume.
	// +kubebuilder:validation:Enum=succeeded;skipped;failed
	Status string `json:"status"`

	// Detail is an optional human-readable note (error message or skip reason).
	// +optional
	Detail string `json:"detail,omitempty"`

	// TargetLvolID is the UUID of the volume on the target cluster (failover only).
	// +optional
	TargetLvolID string `json:"targetLvolID,omitempty"`
}

// ReplicationOpsSpec defines the desired state of a ReplicationOps.
type ReplicationOpsSpec struct {
	// Action is the operation to perform. Immutable.
	// +kubebuilder:validation:Enum=failover;failback
	// +kubebuilder:validation:Required
	Action string `json:"action"`

	// Scope controls which volumes are affected. Immutable.
	// target: all volumes whose ReplicationSlot references the named policy's target.
	// policy: all volumes managed by the named ReplicationPolicy CR.
	// volume: a single ReplicationSlot (unplanned per-volume failover).
	// +kubebuilder:validation:Enum=target;policy;volume
	// +kubebuilder:validation:Required
	Scope string `json:"scope"`

	// Ref is the name of the resource identified by Scope:
	// a ReplicationPolicy name for scope=policy or scope=target,
	// or a ReplicationSlot name for scope=volume. Immutable.
	// +kubebuilder:validation:Required
	Ref string `json:"ref"`

	// SourceClusterID is used for failback only. Omit to recover to the original source.
	// +optional
	SourceClusterID string `json:"sourceClusterID,omitempty"`
}

// ReplicationOpsStatus holds the observed state of a ReplicationOps.
type ReplicationOpsStatus struct {
	// Phase is the current lifecycle phase of this operation.
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Subphase describes what the operation is currently doing within the phase
	// (e.g. "TriggeringFailover", "UpdatingSlotStatuses", "ReleasingLock").
	// +optional
	Subphase string `json:"subphase,omitempty"`

	// Message is a human-readable description of the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when the operation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation finished (successfully or not).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Results holds a per-volume summary of the operation outcome.
	// +optional
	Results []ReplicationOpsResult `json:"results,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=replops
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=".spec.scope"
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=".spec.ref"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Subphase",type=string,JSONPath=".status.subphase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ReplicationOps is a one-shot user-driven CR for imperative replication operations:
// failover (planned or unplanned) and failback. The operator drives the backend calls
// to completion and records per-volume outcomes in status.results. Only one ReplicationOps
// may be active per ReplicationPolicy at a time, enforced via ReplicationPolicy.status.activeOpsRef.
type ReplicationOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReplicationOpsSpec   `json:"spec,omitempty"`
	Status ReplicationOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReplicationOpsList contains a list of ReplicationOps.
type ReplicationOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReplicationOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReplicationOps{}, &ReplicationOpsList{})
}
