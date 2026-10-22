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
	// ReplicationOpsResultPending marks a volume whose migration is still in
	// progress. Only a Running operation reports it.
	ReplicationOpsResultPending ReplicationOpsResultStatus = "pending"
)

// ReplicationOpsResult holds the outcome for a single volume in a ReplicationOps.
type ReplicationOpsResult struct {
	// SlotRef is the name of the ReplicationSlot CR, as "<namespace>/<name>"
	// when it lives outside this operation's namespace. It is empty for a
	// PersistentVolumeClaim that a cluster-scoped migration did not enroll.
	SlotRef string `json:"slotRef"`

	// PVCRef is the name of the PersistentVolumeClaim, when the operation
	// resolved one, as "<namespace>/<name>" when it lives outside this
	// operation's namespace.
	// +optional
	PVCRef string `json:"pvcRef,omitempty"`

	// Status is the outcome for this volume. A migration reports pending
	// while the volume is still replicating or cutting over.
	// +kubebuilder:validation:Enum=succeeded;skipped;failed;pending
	Status string `json:"status"`

	// Detail is an optional human-readable note (error message or skip reason).
	// +optional
	Detail string `json:"detail,omitempty"`

	// TargetLvolID is the UUID of the volume on the target cluster (failover only).
	// +optional
	TargetLvolID string `json:"targetLvolID,omitempty"`

	// State is the migration progress of this volume as the storage control
	// plane reports it: replicating (initial copy or catch-up), queued
	// (caught up, waiting for its turn to cut over), converging (shrinking
	// the final delta), cutting_over (I/O frozen, paths switching), done,
	// failed, canceled, or skipped.
	// +optional
	State string `json:"state,omitempty"`

	// TargetNodeID is the storage node on the target cluster that receives
	// this volume.
	// +optional
	TargetNodeID string `json:"targetNodeID,omitempty"`

	// BytesRemaining is the data still to be copied for this volume.
	// +optional
	BytesRemaining int64 `json:"bytesRemaining,omitempty"`

	// ETASeconds is the forecast time, in seconds, until this volume has
	// cut over, counting the volumes queued ahead of it.
	// +optional
	ETASeconds int64 `json:"etaSeconds,omitempty"`
}

// ReplicationOpsForecast is the storage control plane's estimate of how long
// a migration takes. It is recomputed on every progress poll, because client
// writes during the copy add to the data still to transfer.
type ReplicationOpsForecast struct {
	// BytesRemaining is the data still to be copied across all volumes.
	// +optional
	BytesRemaining int64 `json:"bytesRemaining,omitempty"`

	// ETASeconds is the forecast time, in seconds, until the last volume has
	// cut over.
	// +optional
	ETASeconds int64 `json:"etaSeconds,omitempty"`

	// CompletesAt is the forecast completion time.
	// +optional
	CompletesAt *metav1.Time `json:"completesAt,omitempty"`

	// ThroughputBytesPerSec is the transfer rate the forecast assumes.
	// +optional
	ThroughputBytesPerSec int64 `json:"throughputBytesPerSec,omitempty"`

	// ThroughputMeasured is true when the rate was measured from completed
	// transfers, and false when none has completed yet and a default rate
	// was assumed.
	// +optional
	ThroughputMeasured bool `json:"throughputMeasured,omitempty"`

	// Converging is false when some source lvstore's volumes are written at
	// least about as fast as it replicates them: their lag never closes, no
	// cutover can start, and ETASeconds and CompletesAt are left unset. Unset
	// when the storage control plane does not report the verdict.
	// +optional
	Converging *bool `json:"converging,omitempty"`

	// DirtyBytesPerSec is the rate the clients write to the volumes still
	// being copied, which replication has to outrun.
	// +optional
	DirtyBytesPerSec int64 `json:"dirtyBytesPerSec,omitempty"`

	// NotConvergingLVS names the source lvstores whose volumes are written
	// faster than they replicate.
	// +optional
	NotConvergingLVS []string `json:"notConvergingLVS,omitempty"`
}

// ReplicationOpsSpec defines the desired state of a ReplicationOps.
type ReplicationOpsSpec struct {
	// Action is the operation to perform. Immutable.
	// failover: unplanned. Promotes the target clone; the source may be down.
	// failback: restores the source as primary after a prior failover.
	// migration: planned online cutover to the target cluster. Both clusters stay up.
	//            Every affected volume goes into one migration plan, which copies
	//            the data, then cuts the volumes over one at a time per source
	//            lvstore so each I/O freeze stays short. The operation runs until
	//            every volume has cut over, and reports progress and a forecast.
	//            Slot state progression: replicating → cutover_pending → cutover_done.
	// +kubebuilder:validation:Enum=failover;failback;migration
	// +kubebuilder:validation:Required
	Action string `json:"action"`

	// Scope controls which volumes are affected. Immutable.
	// target: all volumes across every policy that uses the named ReplicationPair.
	// policy: all volumes managed by the named ReplicationPolicy CR.
	// volume: a single ReplicationSlot (planned or unplanned per-volume operation).
	// cluster: every PersistentVolumeClaim, in any namespace, whose volume lives
	//          on the named source storage cluster (migration only). Claims that
	//          are not replicated yet are enrolled under spec.policyRef.
	// +kubebuilder:validation:Enum=target;policy;volume;cluster
	// +kubebuilder:validation:Required
	Scope string `json:"scope"`

	// Ref is the name of the resource identified by Scope:
	// a ReplicationPair name for scope=target,
	// a ReplicationPolicy name for scope=policy,
	// a ReplicationSlot name for scope=volume,
	// or a StorageCluster name (or the cluster's UUID) for scope=cluster. Immutable.
	// +kubebuilder:validation:Required
	Ref string `json:"ref"`

	// SourceClusterID is used for failback only. Omit to recover to the original source.
	// +optional
	SourceClusterID string `json:"sourceClusterID,omitempty"`

	// DeleteSource instructs the backend to delete the old volume after a
	// successful cutover. For migration it deletes the original source volume;
	// for failback it deletes the failed-over volume on the target cluster.
	// +optional
	DeleteSource bool `json:"deleteSource,omitempty"`

	// PolicyRef names the ReplicationPolicy, in mode migration, that a
	// cluster-scoped migration enrolls not-yet-replicated claims under. It is
	// required for scope=cluster and not allowed otherwise. A claim that
	// already follows a different policy is reported as skipped, never
	// re-enrolled.
	// +optional
	// +k8s:immutable
	PolicyRef string `json:"policyRef,omitempty"`

	// DryRun asks for the migration plan and its forecast only: which
	// volumes would move, to which target nodes, how much data that is, and
	// how long it is expected to take. Nothing is enrolled, replicated, or
	// cut over, and the operation succeeds once the forecast is in status.
	// Allowed for action=migration only.
	// +optional
	// +k8s:immutable
	DryRun bool `json:"dryRun,omitempty"`
}

// ReplicationOpsStatus holds the observed state of a ReplicationOps.
type ReplicationOpsStatus struct {
	// Phase is the current lifecycle phase of this operation.
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Subphase describes what the operation is currently doing within the phase
	// (e.g., "TriggeringFailover", "UpdatingSlotStatuses", "ReleasingLock").
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

	// Results holds a per-volume summary of the operation outcome. A
	// migration updates it while it runs.
	// +optional
	Results []ReplicationOpsResult `json:"results,omitempty"`

	// PlanID is the storage control plane's migration plan that a migration
	// operation drives. It is set once the plan is created and never replaced.
	// +optional
	PlanID string `json:"planID,omitempty"`

	// Forecast is the latest estimate of how much data a migration still has
	// to copy and when it completes.
	// +optional
	Forecast *ReplicationOpsForecast `json:"forecast,omitempty"`

	// Warnings are the risks the storage control plane found when it planned
	// the migration and did not refuse it for, such as too little free
	// capacity on the source for the snapshots the copy pins.
	// +optional
	Warnings []string `json:"warnings,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=replops
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=".spec.scope"
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=".spec.ref"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Subphase",type=string,JSONPath=".status.subphase"
// +kubebuilder:printcolumn:name="ETA",type=integer,JSONPath=".status.forecast.etaSeconds",priority=1
// +kubebuilder:printcolumn:name="Converging",type=boolean,JSONPath=".status.forecast.converging",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ReplicationOps is a one-shot user-driven CR for imperative replication operations:
// failover (planned or unplanned), failback, and online migration to the target
// cluster. The operator drives the backend calls
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
