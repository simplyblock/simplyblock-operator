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

// Volume-level replication phases. Each phase maps to a specific idempotent
// action so that re-reconciliation after a crash or error resumes from the
// correct step rather than re-calling already-completed API operations.
const (
	// VolPhaseWaitingForTargetReplication is the initial phase after the
	// failover triggers replicate_lvol on the target cluster. The controller
	// waits for the task to complete before proceeding.
	VolPhaseWaitingForTargetReplication = "WaitingForTargetReplication"

	// VolPhaseTriggeringTargetReplication means the replicate_lvol API call
	// has been dispatched to the target cluster and has not yet completed.
	VolPhaseTriggeringTargetReplication = "TriggeringTargetReplication"

	// VolPhaseReplicatingToSource means the failback replication from target
	// back to the source cluster is in progress.
	VolPhaseReplicatingToSource = "ReplicatingToSource"

	// VolPhaseWaitingForTargetDeletion means the source-side lvol has been
	// restored and the controller is waiting for the target-side snapshot to
	// be cleaned up.
	VolPhaseWaitingForTargetDeletion = "WaitingForTargetDeletion"

	// VolPhaseCompleted means all replication steps finished successfully for
	// this volume.
	VolPhaseCompleted = "Completed"

	// VolPhaseFailed means an unrecoverable error occurred for this volume.
	VolPhaseFailed = "Failed"

	// VolPhasePending is the default phase for volumes that have not started
	// replication yet.
	VolPhasePending = "Pending"

	// VolPhaseRunning means normal periodic replication is active.
	VolPhaseRunning = "Running"

	// VolPhasePaused means replication is explicitly paused.
	VolPhasePaused = "Paused"
)

// Condition type constants used in SnapshotReplicationStatus.Conditions.
const (
	// ConditionTypeReady indicates the overall replication is operational.
	ConditionTypeReady = "Ready"
	// ConditionTypeConfigured indicates addreplication has completed successfully.
	ConditionTypeConfigured = "Configured"
	// ConditionTypeFailback indicates a failback operation is in progress or completed.
	ConditionTypeFailback = "Failback"
)

// SnapshotReplicationSpec defines the desired state of SnapshotReplication
type SnapshotReplicationSpec struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Source Cluster"
	// Source cluster for the snapshots
	SourceCluster string `json:"sourceCluster"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Target Cluster"
	// Target cluster for replication
	TargetCluster string `json:"targetCluster"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Target Pool"
	// Target cluster pool for replication
	TargetPool string `json:"targetPool"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Source Pool"
	// required for failback to a fresh source cluster
	SourcePool string `json:"sourcePool,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Timeout"
	// snapshot replication timeout
	Timeout *int32 `json:"timeout,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Interval"
	// snapshot replication interval in seconds (default: 300sec)
	Interval *int32 `json:"interval,omitempty"`
	// +kubebuilder:validation:Enum=failback
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Action"
	Action string `json:"action,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Include Volume IDs"
	// Optional: only these volumes are included in failback.
	// If empty, all volumes are candidates unless excluded below.
	IncludeVolumeIDs []string `json:"includeVolumeIDs,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Exclude Volume IDs"
	// Optional: volumes to exclude from failback.
	ExcludeVolumeIDs []string `json:"excludeVolumeIDs,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Volume IDs"
	// Optional: list of volumes to replicate. Empty means all volumes
	VolumeIDs []string `json:"volumeIDs,omitempty"`
}

// SnapshotReplicationStatus defines the observed state of SnapshotReplication.
type SnapshotReplicationStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Configured"
	Configured bool `json:"configured,omitempty"`

	// The metadata.generation value for which failback was last processed.
	ObservedFailbackGeneration int64 `json:"observedFailbackGeneration,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Volumes"
	// Per-volume replication status
	Volumes []VolumeReplicationStatus `json:"volumes,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Conditions"
	// Conditions provides human-readable status conditions for kubectl get output.
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VolumeReplicationStatus tracks the replication state of an individual volume
type VolumeReplicationStatus struct {
	// Volume ID
	VolumeID string `json:"volumeID"`

	// Phase is the current replication phase for this volume.
	// +kubebuilder:validation:Enum=Pending;Running;TriggeringTargetReplication;WaitingForTargetReplication;ReplicatingToSource;WaitingForTargetDeletion;Completed;Failed;Paused
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
// +kubebuilder:printcolumn:name="Configured",type="boolean",JSONPath=".status.configured"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +operator-sdk:csv:customresourcedefinitions:displayName="Snapshot Replication"

// SnapshotReplication is the Schema for the snapshotreplications API
type SnapshotReplication struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SnapshotReplication
	// +required
	Spec SnapshotReplicationSpec `json:"spec"`

	// status defines the observed state of SnapshotReplication
	// +optional
	Status SnapshotReplicationStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SnapshotReplicationList contains a list of SnapshotReplication
type SnapshotReplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SnapshotReplication `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SnapshotReplication{}, &SnapshotReplicationList{})
}
