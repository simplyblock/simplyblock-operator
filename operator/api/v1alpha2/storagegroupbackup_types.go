// StorageGroupBackup: one backup of a whole consistency group, taken on demand.
//
// A StorageBackup is one copy of one volume. A group backup is N copies of one
// crash-consistent generation, committed as one unit, so it is its own kind: a
// restore names it in StorageBackupOps.spec.groupBackupRef, and the mirror reads
// it to record the member copies beside it rather than beside the cluster
// (design-consistency-group-backup.md §7.5, Appendix E).

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BackupLabelGroupBackup is the StorageGroupBackup a member copy belongs to, on
// a member StorageBackup recorded beside it. Absent on the members of a group
// backup no StorageGroupBackup owns.
const BackupLabelGroupBackup = "storage.simplyblock.io/group-backup"

// GroupBackupConditionAvailable is True once every member copy has completed.
const GroupBackupConditionAvailable = "Available"

// StorageGroupBackupPhase is where a group backup taken on demand has got to.
// Available is the terminal success, as on StorageBackup: what matters is that
// the group can be restored, not that the copying finished.
// +kubebuilder:validation:Enum=Pending;Creating;Available;Failed
type StorageGroupBackupPhase string

const (
	StorageGroupBackupPhasePending   StorageGroupBackupPhase = "Pending"
	StorageGroupBackupPhaseCreating  StorageGroupBackupPhase = "Creating"
	StorageGroupBackupPhaseAvailable StorageGroupBackupPhase = "Available"
	StorageGroupBackupPhaseFailed    StorageGroupBackupPhase = "Failed"
)

// GroupBackupSource names the consistency group to back up.
type GroupBackupSource struct {
	// ConsistencyGroupName is the consistency group to back up as one unit: the
	// group the claims in this object's namespace labeled
	// storage.simplyblock.io/consistency-group with this value belong to. The
	// cluster is the one that provisioned them. A group that does not exist yet
	// holds this object in Pending until it does.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ConsistencyGroupName string `json:"consistencyGroupName"`
}

// StorageGroupBackupSpec asks for one group backup.
type StorageGroupBackupSpec struct {
	// Source is the consistency group to back up.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Source GroupBackupSource `json:"source"`
}

// GroupBackupMember is one member copy of the group backup.
type GroupBackupMember struct {
	// BackupID is the control plane's identifier for the member copy.
	// +kubebuilder:validation:Required
	BackupID string `json:"backupID"`

	// BackupName is the StorageBackup recording the copy, in this object's
	// namespace. Empty until the record has been placed here.
	// +optional
	BackupName string `json:"backupName,omitempty"`

	// ClaimName is the member claim the copy was taken from.
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// LvolName is the member volume the copy was taken from.
	// +optional
	LvolName string `json:"lvolName,omitempty"`

	// Phase is the member copy's own phase.
	// +optional
	Phase StorageBackupPhase `json:"phase,omitempty"`
}

// StorageGroupBackupStatus is the observed state of one group backup.
type StorageGroupBackupStatus struct {
	// Phase is where the group backup has got to.
	// +optional
	Phase StorageGroupBackupPhase `json:"phase,omitempty"`

	// Message is the reason the phase is what it is, in one sentence.
	// +optional
	Message string `json:"message,omitempty"`

	// ClusterID is the backend cluster the group backup was taken on.
	// +optional
	ClusterID string `json:"clusterID,omitempty"`

	// GroupBackupID is the control plane's identifier for the group backup. A
	// StorageBackupOps restores it by naming this object in spec.groupBackupRef.
	// +optional
	GroupBackupID string `json:"groupBackupID,omitempty"`

	// Generation is the group sequence of the generation that was backed up.
	// +optional
	Generation int64 `json:"generation,omitempty"`

	// MemberCount is how many member copies must complete before the group
	// backup is Available.
	// +optional
	MemberCount int32 `json:"memberCount,omitempty"`

	// Members is one entry per member copy.
	// +listType=map
	// +listMapKey=backupID
	// +optional
	Members []GroupBackupMember `json:"members,omitempty"`

	// Size is the sum of the member copies' sizes in bytes.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Size *int64 `json:"size,omitempty"`

	// CompletedAt is when the last member copy completed.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Conditions carries Available, which a restore waiting on this object reads.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:storageversion
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sgb
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=".spec.source.consistencyGroupName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Members",type=integer,JSONPath=".status.memberCount"
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=".status.generation"
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=".status.size"
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=".status.completedAt"
// +kubebuilder:printcolumn:name="GroupBackupID",type=string,JSONPath=".status.groupBackupID",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageGroupBackup is one backup of a whole consistency group, taken on
// demand: one crash-consistent generation, every member backed up, committed as
// one unit. The member copies are recorded as StorageBackups beside it.
// Deleting it deletes no copy and no record.
type StorageGroupBackup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec names the consistency group to back up
	// +required
	Spec StorageGroupBackupSpec `json:"spec"`

	// status defines the observed state of StorageGroupBackup
	// +optional
	Status StorageGroupBackupStatus `json:"status,omitzero"`
}

// GroupBackupLabelValue returns the bare id of the group backup this object
// produced, the form the group-backup-id label and the indexes key on. Empty
// until the control plane has accepted the group backup.
func (g *StorageGroupBackup) GroupBackupLabelValue() string {
	return GroupBackupLabelValue(g.Status.GroupBackupID)
}

// +kubebuilder:object:root=true

// StorageGroupBackupList contains a list of StorageGroupBackup.
type StorageGroupBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StorageGroupBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageGroupBackup{}, &StorageGroupBackupList{})
}
