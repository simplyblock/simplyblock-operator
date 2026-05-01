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
	BackupPolicyPhaseActive  = "Active"
	BackupPolicyPhasePending = "Pending"
	BackupPolicyPhaseFailed  = "Failed"
)

// BackupPolicySpec defines the desired state of BackupPolicy.
//
// +kubebuilder:validation:XValidation:rule="self.clusterName == oldSelf.clusterName",message="clusterName is immutable"
// +kubebuilder:validation:XValidation:rule="self.maxVersions == oldSelf.maxVersions",message="maxVersions is immutable"
// +kubebuilder:validation:XValidation:rule="self.maxAge == oldSelf.maxAge",message="maxAge is immutable"
// +kubebuilder:validation:XValidation:rule="self.schedule == oldSelf.schedule",message="schedule is immutable"
type BackupPolicySpec struct {
	// ClusterName is the target storage cluster name.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Cluster Name"
	ClusterName string `json:"clusterName"`

	// MaxVersions is the maximum number of completed backup versions to retain.
	// When exceeded, the oldest backup is merged into the second-oldest.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Max Versions"
	MaxVersions int `json:"maxVersions,omitempty"`

	// MaxAge is the maximum age of backups to retain (e.g. "7d", "12h", "30m").
	// Backups older than this are merged. Accepts m, h, d, w suffixes.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Max Age"
	MaxAge string `json:"maxAge,omitempty"`

	// Schedule defines the tiered backup schedule as a space-separated list of
	// interval,keep_count pairs (e.g. "15m,4 60m,11 24h,7").
	// Intervals must be strictly increasing. Supported units: m, h, d, w.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Schedule"
	Schedule string `json:"schedule,omitempty"`
}

// AttachedLvol records a single PVC-to-lvol attachment managed by this policy.
type AttachedLvol struct {
	// PVCName is the name of the PVC.
	PVCName string `json:"pvcName"`
	// PVCNamespace is the namespace of the PVC.
	PVCNamespace string `json:"pvcNamespace"`
	// LvolID is the Simplyblock logical volume UUID that this policy is attached to.
	LvolID string `json:"lvolID"`
}

// BackupPolicyStatus defines the observed state of BackupPolicy.
type BackupPolicyStatus struct {
	// Phase is the high-level lifecycle state of the policy.
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Phase"
	Phase string `json:"phase,omitempty"`
	// Message contains the latest reconciliation detail or error.
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Message"
	Message string `json:"message,omitempty"`

	// ClusterUUID is the resolved backend cluster UUID.
	ClusterUUID string `json:"clusterUUID,omitempty"`
	// PolicyID is the UUID assigned to this policy by the Simplyblock backend.
	PolicyID string `json:"policyID,omitempty"`

	// AttachedLvols lists the PVCs (and their lvol IDs) currently attached to
	// this policy in the Simplyblock backend. The controller uses this to detect
	// and reconcile annotation additions and removals.
	AttachedLvols []AttachedLvol `json:"attachedLvols,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterName"
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=".spec.schedule"
// +kubebuilder:printcolumn:name="MaxVersions",type=integer,JSONPath=".spec.maxVersions"
// +kubebuilder:printcolumn:name="MaxAge",type=string,JSONPath=".spec.maxAge"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// BackupPolicy is the Schema for the backuppolicies API.
//
// A BackupPolicy defines retention and scheduling parameters for Simplyblock
// backups. To apply a policy to a PVC, annotate the PVC with:
//
//	simplybk/backup-policy: <BackupPolicy-name>
//
// The BackupPolicy must be in the same namespace as the annotated PVC.
// The controller attaches and detaches the policy in the Simplyblock backend
// whenever the annotation is added or removed.
type BackupPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BackupPolicy
	// +required
	Spec BackupPolicySpec `json:"spec"`

	// status defines the observed state of BackupPolicy
	// +optional
	Status BackupPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupPolicyList contains a list of BackupPolicy.
type BackupPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BackupPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupPolicy{}, &BackupPolicyList{})
}
