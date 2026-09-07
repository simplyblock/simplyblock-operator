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

// ReplicationPolicySpec defines the desired replication schedule and retention.
type ReplicationPolicySpec struct {
	// PairRef is the name of the ReplicationPair that defines the source and target clusters.
	// Multiple ReplicationPolicies may reference the same pair with different schedules.
	// +kubebuilder:validation:Required
	PairRef string `json:"pairRef"`

	// Mode controls replication semantics.
	// failover: target is a DR standby; volumes are read-only on the target.
	// migration: planned online cutover to the target cluster.
	// +kubebuilder:validation:Enum=failover;migration
	// +kubebuilder:default=failover
	// +optional
	Mode string `json:"mode,omitempty"`

	// Interval is how often a replication snapshot is taken (e.g., "5m" or "1h").
	// +kubebuilder:default="5m"
	// +optional
	Interval string `json:"interval,omitempty"`

	// SnapshotRetention is the minimum number of snapshots to retain on the target.
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:default=3
	// +optional
	SnapshotRetention int32 `json:"snapshotRetention,omitempty"`

	// EnableConsistencyGroup groups the policy's volumes into ONE crash-consistent
	// unit: replication snapshots are taken as atomically frozen generations
	// across every member, and the members fail over together. Members must share
	// one storage node (the group pins placement on the first attached volume and
	// rejects members created elsewhere). Decided at creation — the backend
	// creates the group record together with the policy — hence immutable.
	// +k8s:immutable
	// +optional
	EnableConsistencyGroup *bool `json:"enableConsistencyGroup,omitempty"`
}

// ReplicationPolicyStatus holds the observed state of a ReplicationPolicy.
type ReplicationPolicyStatus struct {
	// Ready is true when the backend ReplicationPolicy has been created.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// BackendPolicyID is the UUID of the backend ReplicationPolicy resource.
	// +optional
	BackendPolicyID string `json:"backendPolicyID,omitempty"`

	// SlotCount is the number of ReplicationSlot CRs currently managed by this policy.
	// +optional
	SlotCount int32 `json:"slotCount,omitempty"`

	// ActiveOpsRef is the name of the currently running ReplicationOps CR.
	// Empty when no operation is in progress.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// GroupNodeUUID is the storage node the policy's consistency group is
	// pinned to, set by the group's first attached member: every further
	// member volume must be created on this node. Empty for a policy without
	// a consistency group, or before the first member joins.
	// +optional
	GroupNodeUUID string `json:"groupNodeUUID,omitempty"`

	// GroupLvsName is the logical volume store the consistency group is
	// pinned to, alongside GroupNodeUUID.
	// +optional
	GroupLvsName string `json:"groupLvsName,omitempty"`

	// GroupLastSeq is the newest completed consistency-group generation
	// (each generation is one crash-consistent snapshot set across every
	// member).
	// +optional
	GroupLastSeq int64 `json:"groupLastSeq,omitempty"`

	// Conditions holds standard Kubernetes condition types.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=repl
// +kubebuilder:printcolumn:name="Pair",type=string,JSONPath=".spec.pairRef"
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=".spec.interval"
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Slots",type=integer,JSONPath=".status.slotCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ReplicationPolicy defines the replication schedule and retention for volumes replicated
// between the clusters defined by a ReplicationPair.
// A StorageClass or PVC references a policy via the storage.simplyblock.io/replication-policy
// annotation. The operator automatically creates one ReplicationSlot per bound PVC.
// Deletion is blocked while any ReplicationSlots reference this policy.
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
