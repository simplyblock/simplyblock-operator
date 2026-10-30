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
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/statemachine"
)

// HistoricalRecordAnnotation marks an operation that records work already done
// rather than work to perform.
//
// It exists for one caller: the upgrade's absorption of a finished BackupRestore
// into this kind. That object is an audit record of a restore that ran under
// the old kind, and it is born into a world where everything the admission
// checks look for is already true: the claim exists, because the restore it
// records produced it, and the backup it names may have been pruned years ago.
// Checking a record of the past against the present rejects every object the
// absorption exists to preserve.
//
// Two things read it, and both have to, or the marker would be worse than
// nothing. The validator skips the reference checks, because the references
// describe what was rather than what will be. The controller never advances the
// operation, because running a restore that already ran would create a second
// volume and try to bind a claim that exists.
//
// A user can set it, and what that buys them is an inert object with references
// nothing resolves, a false line in an audit log that somebody able to create
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
// +kubebuilder:validation:Enum=Validating;Restoring;AwaitingVolume;AwaitingVolumes;Binding;Completed
type StorageBackupOpsStep string

const (
	// StorageBackupOpsStepValidating belongs to every action.
	StorageBackupOpsStepValidating StorageBackupOpsStep = "Validating"

	// The Restore action's remaining steps. Restoring is where the control plane
	// is asked for the copy back, so it is the last step from which an abort
	// undoes nothing: everything after it has a logical volume behind it.
	//
	// A single-backup restore waits on its one volume at AwaitingVolume; a group
	// restore drives N member recoveries at Restoring and waits for all of them at
	// AwaitingVolumes before Binding creates one claim per member. The step the
	// graph takes is chosen by whether spec.groupBackupID is set.
	StorageBackupOpsStepRestoring       StorageBackupOpsStep = "Restoring"
	StorageBackupOpsStepAwaitingVolume  StorageBackupOpsStep = "AwaitingVolume"
	StorageBackupOpsStepAwaitingVolumes StorageBackupOpsStep = "AwaitingVolumes"
	StorageBackupOpsStepBinding         StorageBackupOpsStep = "Binding"
	// StorageBackupOpsStepCompleted is the terminal step of an operation that succeeded.
	StorageBackupOpsStepCompleted StorageBackupOpsStep = "Completed"
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
	// ClaimName is the PersistentVolumeClaim to create, in this operation's
	// namespace. It must not already exist: a restore that adopted an existing
	// claim would replace a running workload's data with the backup's.
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
// +kubebuilder:validation:XValidation:rule="(has(self.backupRef) ? 1 : 0) + (has(self.groupBackupID) ? 1 : 0) + (has(self.groupBackupRef) ? 1 : 0) == 1",message="set exactly one of backupRef, groupBackupRef and groupBackupID: an operation acts on a single backup or on one whole group backup"
type StorageBackupOpsSpec struct {
	// ClusterRef names the StorageCluster the operation runs against. Its
	// namespace defaults to this operation's. The cluster's StoragePool, which
	// spec.restore.targetPool names, is read from the cluster's namespace.
	//
	// A cluster in another namespace needs the requesting user to be allowed to
	// read it there.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef NamespacedDnsReference `json:"clusterRef"`

	// omitzero, not omitempty, on BackupRef: a struct is never "empty" to
	// encoding/json, so omitempty serialized an unset BackupRef as {"name": ""}
	// on every write the controller made to the object (its finalizer update,
	// first of all). The API server then refused the operation it had just
	// admitted: the CEL rule saw both backupRef and groupBackupID set, and name
	// failed its minimum length. Every group restore died on its first
	// reconcile (live 2026-10-20). Kept out of the doc comment so the CRD
	// description stays the field's, not the bug's.

	// BackupRef names the single StorageBackup this operation acts on. Its
	// namespace defaults to this operation's. A backup in another namespace needs
	// the requesting user to be allowed to read it there. Set exactly one of this
	// and GroupBackupID: an operation acts on one backup or on a whole group
	// backup.
	// +k8s:immutable
	// +optional
	BackupRef NamespacedReference `json:"backupRef,omitzero"`

	// GroupBackupID restores a whole consistency-group backup instead of the
	// single backup named by BackupRef, producing one PersistentVolumeClaim per
	// member labeled into one restored consistency group. Its value is the
	// control-plane group backup id carried by the storage.simplyblock.io/group-backup-id
	// label on the member StorageBackups, which are resolved beside the cluster,
	// so a requester in another namespace needs to be allowed to list
	// StorageBackups there. It is the path for a group backup no request object
	// stands for: one a StorageBackupPolicy scheduled, or one imported. Set
	// exactly one of this, GroupBackupRef, and BackupRef.
	// +kubebuilder:validation:MaxLength=253
	// +k8s:immutable
	// +optional
	GroupBackupID string `json:"groupBackupID,omitempty"`

	// GroupBackupRef restores the whole group backup a StorageGroupBackup
	// produced. Its namespace defaults to this operation's, and a
	// StorageGroupBackup in another namespace needs the requesting user to be
	// allowed to read it there, the same rule as BackupRef. The member copies are
	// recorded beside the StorageGroupBackup, so restoring a group taken in an
	// application's namespace needs no access to any other namespace's backups.
	// Set exactly one of this, GroupBackupID, and BackupRef.
	// +k8s:immutable
	// +optional
	GroupBackupRef NamespacedReference `json:"groupBackupRef,omitzero"`

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

// RestoredGroupMember is one volume a group restore produced: the logical volume
// the control plane created, the source member it came from, and the claim it is
// bound to once Binding produces one.
type RestoredGroupMember struct {
	// LvolID is the restored logical volume's backend id.
	// +kubebuilder:validation:Required
	LvolID string `json:"lvolID"`

	// MemberName is the source member's volume name, which the restored claim's
	// name is formed from (claimName-memberName) and which tells the members apart.
	// +optional
	MemberName string `json:"memberName,omitempty"`

	// ClaimName is the PersistentVolumeClaim this member was bound to, written by
	// Binding. Empty until the claim is produced.
	// +optional
	ClaimName string `json:"claimName,omitempty"`
}

// BackupsNamespace is the namespace the operation's StorageBackups live in:
// the StorageCluster's, since the mirror records every copy of a cluster's
// store beside the cluster (StorageBackupReconciler.recordOf), not beside the
// operations that restore them. An operation created in an application's
// namespace with a clusterRef into the operator's namespace resolves its group
// members here; listing them in the operation's own namespace found none and
// the admission webhook refused every such restore (live 2026-10-20).
func (ops *StorageBackupOps) BackupsNamespace() string {
	return ops.Spec.ClusterRef.In(ops.Namespace).Namespace
}

// IsGroupRestore reports an operation on a whole group backup, named by id or
// by the StorageGroupBackup that produced it, rather than on one StorageBackup. It is
// decided by the spec alone, so every step branches the same way from the
// operation's first pass.
func (ops *StorageBackupOps) IsGroupRestore() bool {
	return ops.Spec.GroupBackupID != "" || ops.Spec.GroupBackupRef.Name != ""
}

// GroupBackupKey is the StorageGroupBackup a groupBackupRef names.
func (ops *StorageBackupOps) GroupBackupKey() types.NamespacedName {
	return ops.Spec.GroupBackupRef.In(ops.Namespace)
}

// TargetGroupBackupID is the group backup a group restore restores: the id the
// spec names, or the one Validating read from the StorageGroupBackup and
// recorded in status.backupID.
func (ops *StorageBackupOps) TargetGroupBackupID() string {
	if ops.Spec.GroupBackupID != "" {
		return ops.Spec.GroupBackupID
	}
	return ops.Status.BackupID
}

// StorageBackupOpsStatus is the observed state of one backup operation.
type StorageBackupOpsStatus struct {
	// Phase is the operation's own progress.
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Phase"
	// +optional
	Phase StorageBackupOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine. It is
	// persisted before the side effect that step performs, so a step reported
	// here is a step that started.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','Restoring','AwaitingVolume','AwaitingVolumes','Binding','Completed']",message="unknown step"
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
	// binding. For a group restore it holds the first member's volume; the full
	// set is tracked per member as the group drive binds them.
	// +optional
	RestoredLvolID string `json:"restoredLvolID,omitempty"`

	// RestoredGroupID is the consistency group a group restore formed, written
	// once the control-plane group restore returns, so the operation says which
	// group it produced. Empty for a single-backup restore.
	// +optional
	RestoredGroupID string `json:"restoredGroupID,omitempty"`

	// RestoredGroupMembers is one entry per restored member volume of a group
	// restore, written when the control plane accepts the restore and carrying the
	// claim each is bound to as Binding produces them. It is how a restarted
	// AwaitingVolumes or Binding step knows which volumes to wait on and which
	// claims to produce. Empty for a single-backup restore.
	// +optional
	// +listType=map
	// +listMapKey=lvolID
	RestoredGroupMembers []RestoredGroupMember `json:"restoredGroupMembers,omitempty"`

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
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=".spec.backupRef.name"
// +kubebuilder:printcolumn:name="GroupBackup",type=string,JSONPath=".spec.groupBackupID",priority=1
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
