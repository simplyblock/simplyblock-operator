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

// ReplicationPolicySpec defines the desired replication configuration.
type ReplicationPolicySpec struct {
	// ClusterName is the name of the local StorageCluster this policy belongs to.
	// +kubebuilder:validation:Required
	ClusterName string `json:"clusterName"`

	// Target is the name or UUID of the remote cluster to replicate to.
	// Immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="target is immutable"
	Target string `json:"target"`

	// Interval is how often a replication snapshot is taken (e.g. "5m", "1h").
	// +kubebuilder:default="5m"
	// +optional
	Interval string `json:"interval,omitempty"`

	// Mode controls replication semantics.
	// failover: target is a DR standby; volumes are read-only on the target.
	// migration: planned online cutover to the target cluster.
	// +kubebuilder:validation:Enum=failover;migration
	// +kubebuilder:default=failover
	// +optional
	Mode string `json:"mode,omitempty"`

	// KeepReplicated is the minimum number of snapshots to retain on the target.
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:default=3
	// +optional
	KeepReplicated int32 `json:"keepReplicated,omitempty"`
}

// ReplicationPolicyStatus holds the observed state of a ReplicationPolicy.
type ReplicationPolicyStatus struct {
	// Ready is true when the backend ReplicationTarget and ReplicationPolicy exist.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// BackendTargetID is the UUID of the backend ReplicationTarget resource.
	// +optional
	BackendTargetID string `json:"backendTargetID,omitempty"`

	// BackendPolicyID is the UUID of the backend ReplicationPolicy resource.
	// +optional
	BackendPolicyID string `json:"backendPolicyID,omitempty"`

	// PairCount is the number of ReplicationPair CRs currently managed by this policy.
	// +optional
	PairCount int32 `json:"pairCount,omitempty"`

	// ActiveOpsRef is the name of the currently running ReplicationOps CR.
	// Empty when no operation is in progress.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// Conditions holds standard Kubernetes condition types.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=repl
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".spec.target"
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=".spec.interval"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Pairs",type=integer,JSONPath=".status.pairCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ReplicationPolicy defines the desired replication configuration for a set of PVCs.
// A StorageClass or PVC may reference a ReplicationPolicy by name via the
// replication.simplyblock.io/policy annotation. The operator ensures the
// corresponding backend ReplicationTarget and ReplicationPolicy exist, and
// manages one ReplicationPair CR per PVC that uses this policy.
type ReplicationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReplicationPolicySpec   `json:"spec,omitempty"`
	Status ReplicationPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReplicationPolicyList contains a list of ReplicationPolicy.
type ReplicationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReplicationPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReplicationPolicy{}, &ReplicationPolicyList{})
}
