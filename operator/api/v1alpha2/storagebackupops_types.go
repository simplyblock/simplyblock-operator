// StorageBackupOps: one operation performed against one StorageBackup, which
// today is a restore.
//
// The kind is declared here rather than in v1alpha1 because it is new. It
// absorbs the registered BackupRestore, and an absorption is a different kind
// under a different name rather than a field change, so no conversion webhook
// is invoked across the two: this one is born at v1alpha2 and the upgrade
// constructs a StorageBackupOps from each BackupRestore it finds
// (design-api-upgrade.md §7.1, design-storagebackup.md §13). That is why this
// file carries no conversion and why the type implements no hub interface.
//
// It keeps spec.action although it has one action, for the reason every
// single-action Ops kind in the group does: the shape does not have to change
// the day it gains a second.
//
// The type follows design-storagebackup.md Appendix C.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// HistoricalRecordAnnotation marks an operation that records work already done
// rather than work to perform.
//
// It exists for one caller: the upgrade's absorption of a finished BackupRestore
// into this kind (design-storagebackup.md §13). That object is an audit record
// of a restore that ran under the old kind, and it is born into a world where
// everything the admission checks look for is already true — the claim exists,
// because the restore it records produced it, and the backup it names may have
// been pruned years ago. Checking a record of the past against the present
// rejects every object the absorption exists to preserve.
//
// Two things read it, and both have to, or the marker would be worse than
// nothing. The validator skips the reference checks, because the references
// describe what was rather than what will be. The controller never advances the
// operation, because running a restore that already ran would create a second
// volume and try to bind a claim that exists.
//
// A user can set it, and what that buys them is an inert object with references
// nothing resolves — a false line in an audit log, which somebody able to create
// this kind can write in a dozen other ways. It buys them no action, which is
// the property that matters: the controller refuses to run a marked operation
// whoever wrote it.
const HistoricalRecordAnnotation = "storage.simplyblock.io/historical-record"

// IsHistoricalRecord reports an operation that records work already done.
func IsHistoricalRecord(ops *StorageBackupOps) bool {
	return ops.Annotations[HistoricalRecordAnnotation] == "true"
}

// StorageBackupOpsAction is the operation a StorageBackupOps performs. Restore
// acts on a StorageBackup, which is every backup in the cluster's store.
// +kubebuilder:validation:Enum=Restore
type StorageBackupOpsAction string

const (
	StorageBackupOpsActionRestore StorageBackupOpsAction = "Restore"
)

// StorageBackupOpsPhase is the operation's own progress. It is the same small
// set every Ops kind in the group carries: Pending before the operation holds
// its target's lock, Running while it works, and three terminal values.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StorageBackupOpsPhase string

const (
	StorageBackupOpsPhasePending   StorageBackupOpsPhase = "Pending"
	StorageBackupOpsPhaseRunning   StorageBackupOpsPhase = "Running"
	StorageBackupOpsPhaseSucceeded StorageBackupOpsPhase = "Succeeded"
	StorageBackupOpsPhaseFailed    StorageBackupOpsPhase = "Failed"
	StorageBackupOpsPhaseAborted   StorageBackupOpsPhase = "Aborted"
)

// StorageBackupOpsStep is one step of a running backup operation. The enum stays
// flat as actions are added, because which steps belong to which action is
// declared by the graph rather than by this type.
// +kubebuilder:validation:Enum=Validating;Restoring;AwaitingVolume;Binding
type StorageBackupOpsStep string

const (
	// StorageBackupOpsStepValidating belongs to every action.
	StorageBackupOpsStepValidating StorageBackupOpsStep = "Validating"

	// The Restore action's three remaining steps. Restoring is where the control
	// plane is asked for the copy back, so it is the last step from which an
	// abort undoes nothing: everything after it has a logical volume behind it.
	StorageBackupOpsStepRestoring      StorageBackupOpsStep = "Restoring"
	StorageBackupOpsStepAwaitingVolume StorageBackupOpsStep = "AwaitingVolume"
	StorageBackupOpsStepBinding        StorageBackupOpsStep = "Binding"
)

// RestoreSpec parameterizes the Restore action.
//
// It carries no size, no access mode, and no volume mode, which the registered
// BackupRestore took as a whole PersistentVolumeClaim template. All three are
// facts about the backup rather than choices: a restored volume is the size of
// the copy, and asking for a different one is either a truncation or a lie. The
// controller reads the size off status.backup.size and mounts the filesystem
// status.source.fsType records.
type RestoreSpec struct {
	// ClaimName is the PersistentVolumeClaim to create. It must not already
	// exist: a restore that adopted an existing claim would replace a running
	// workload's data with the backup's.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Claim Name"
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClaimName string `json:"claimName"`

	// TargetPool is the StoragePool to restore into. It is required rather than
	// defaulted: a backup found in the store may have been written by another
	// cluster, so status.source.poolName names a pool this cluster need not
	// have, and which pool a volume lands in is a tenancy and QoS decision
	// nobody should make by omission.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Target Pool"
	// +kubebuilder:validation:Required
	// +k8s:immutable
	TargetPool string `json:"targetPool"`

	// ClaimLabels and ClaimAnnotations are applied to the created claim, so that
	// a restored volume can be selected by a policy or an application the same
	// way its original was.
	//
	// Immutable with the rest of the block. The claim is written at the last
	// step, so a value edited while the operation waited for its volume would
	// produce a claim built from inputs the admitted and audited operation never
	// carried, which is the audit record disagreeing with what happened.
	// +k8s:immutable
	// +optional
	ClaimLabels map[string]string `json:"claimLabels,omitempty"`
	// +k8s:immutable
	// +optional
	ClaimAnnotations map[string]string `json:"claimAnnotations,omitempty"`
}

// StorageBackupOpsSpec is one operation to perform against a backup.
type StorageBackupOpsSpec struct {
	// ClusterRef names the StorageCluster the operation runs against.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Cluster Ref"
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// BackupRef names the StorageBackup this operation acts on, in this
	// namespace. Required, since Restore is the only action and every backup in
	// the store has an object.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Backup Ref"
	// +kubebuilder:validation:Required
	// +k8s:immutable
	BackupRef string `json:"backupRef"`

	// Action is the operation to perform.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Action"
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action StorageBackupOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind. It is
	// the one field of this spec that may be edited after the object is created,
	// because it is the one thing about an operation that can legitimately be
	// decided after it started.
	//
	// A restore cannot be aborted once it has created a logical volume, and the
	// action's graph declares that rather than this field: an abort arriving
	// later is reported as an illegal transition while the operation runs on,
	// rather than leaving the work half undone.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Restore parameterizes action Restore.
	// +optional
	Restore *RestoreSpec `json:"restore,omitempty"`
}

// StorageBackupOpsStatus is the observed state of one backup operation.
type StorageBackupOpsStatus struct {
	// Phase is the operation's own progress.
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Phase"
	// +optional
	Phase StorageBackupOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine. It is
	// persisted before the side effect that step performs. The closed set is a
	// CEL rule rather than an Enum marker because a marker cannot reach a field
	// whose type is declared in another module.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','Restoring','AwaitingVolume','Binding']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// ClusterID is the backend cluster the operation ran against.
	// +optional
	ClusterID string `json:"clusterID,omitempty"`

	// BackupID is the copy the restore read from, recorded so the operation says
	// what it restored after the object list has moved on.
	// +optional
	BackupID string `json:"backupID,omitempty"`

	// RestoredLvolID is the logical volume the control plane created. It is
	// written before the claim, so a restarted Binding step knows what it is
	// binding.
	// +optional
	RestoredLvolID string `json:"restoredLvolID,omitempty"`

	// PoolUUID is the backend identifier of spec.restore.targetPool, resolved
	// once at Validating.
	// +optional
	PoolUUID string `json:"poolUUID,omitempty"`

	// PersistentVolumeName is the PV the operation wrote for the restored
	// volume.
	// +optional
	PersistentVolumeName string `json:"persistentVolumeName,omitempty"`

	// ClaimName is the PersistentVolumeClaim a Restore produced. It is written
	// before the claim is created rather than after, because the claim carries
	// no owner reference back: a restarted Binding step recognizes its own work
	// by this name together with the storage.simplyblock.io/restored-by label on
	// the claim, and refuses a claim of the right name that carries neither.
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Claim Name"
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Message"
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from. On this kind it advances at most twice, and the second advance is
	// precisely the signal that spec.abort has been observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StartedAt is when the operation acquired its target's lock.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when it reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbops
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=".spec.backupRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=".status.claimName",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +operator-sdk:csv:customresourcedefinitions:displayName="Storage Backup Operation",resources={{PersistentVolume,v1,restored-volume},{PersistentVolumeClaim,v1,restored-claim}}

// StorageBackupOps is a single operation performed against one StorageBackup. It
// runs to a terminal phase and stays afterward as the audit record of what was
// restored, into which pool, and how it ended.
//
// The claim a restore produces is not owned by the operation and outlives it:
// nobody restores a backup in order to keep a StorageBackupOps, so deleting the
// audit record must not delete the recovered volume.
type StorageBackupOps struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the operation to perform
	// +required
	Spec StorageBackupOpsSpec `json:"spec"`

	// status defines the observed state of StorageBackupOps
	// +optional
	Status StorageBackupOpsStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StorageBackupOpsList contains a list of StorageBackupOps.
type StorageBackupOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StorageBackupOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageBackupOps{}, &StorageBackupOpsList{})
}
