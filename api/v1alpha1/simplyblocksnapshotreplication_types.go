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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SimplyBlockSnapshotReplicationSpec defines the desired state of SimplyBlockSnapshotReplication
type SimplyBlockSnapshotReplicationSpec struct {
	// Source cluster for the snapshots
	SourceCluster string `json:"sourceCluster"`

	// Target cluster for replication
	TargetCluster string `json:"targetCluster"`

	// Target cluster pool for replication
	TargetPool string `json:"targetPool"`

	// snapshot replication timeout
	Timeout *int32 `json:"timeout,omitempty"`

	// Optional: list of volumes to replicate. Empty means all volumes
	VolumeIDs []string `json:"volumeIDs,omitempty"`
}

// SimplyBlockSnapshotReplicationStatus defines the observed state of SimplyBlockSnapshotReplication.
type SimplyBlockSnapshotReplicationStatus struct {
	Configured bool `json:"configured,omitempty"`
	// Per-volume replication status
	Volumes []VolumeReplicationStatus `json:"volumes,omitempty"`
}

// VolumeReplicationStatus tracks the replication state of an individual volume
type VolumeReplicationStatus struct {
	// Volume ID
	VolumeID string `json:"volumeID"`

	// Current phase for this volume
	// +kubebuilder:validation:Enum=Pending;Running;Completed;Failed;Paused
	Phase string `json:"phase,omitempty"`

	// Last snapshot ID replicated for this volume
	LastSnapshotID string `json:"lastSnapshotID,omitempty"`

	// Timestamp of the last successful replication for this volume
	LastReplicationTime *metav1.Time `json:"lastReplicationTime,omitempty"`

	// Number of snapshots successfully replicated
	ReplicatedCount *int32 `json:"replicatedCount,omitempty"`

	// Optional: list of errors encountered for this volume
	Errors []ReplicationError `json:"errors,omitempty"`
}

// ReplicationError stores timestamped error messages
type ReplicationError struct {
	Timestamp metav1.Time `json:"timestamp"`
	Message   string      `json:"message"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SimplyBlockSnapshotReplication is the Schema for the simplyblocksnapshotreplications API
type SimplyBlockSnapshotReplication struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SimplyBlockSnapshotReplication
	// +required
	Spec SimplyBlockSnapshotReplicationSpec `json:"spec"`

	// status defines the observed state of SimplyBlockSnapshotReplication
	// +optional
	Status SimplyBlockSnapshotReplicationStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimplyBlockSnapshotReplicationList contains a list of SimplyBlockSnapshotReplication
type SimplyBlockSnapshotReplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimplyBlockSnapshotReplication `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimplyBlockSnapshotReplication{}, &SimplyBlockSnapshotReplicationList{})
}
