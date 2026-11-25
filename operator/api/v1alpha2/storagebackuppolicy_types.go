// StorageBackupPolicy: which claims a cluster backs up, how often, and how many
// copies it keeps.
//
// The kind is declared here rather than in v1alpha1 because it is new. The
// registered BackupPolicy is a different kind under a different name, and no
// conversion webhook is invoked across kinds, so this one is born at v1alpha2
// and the upgrade copies each old object into a new one
// (design-api-upgrade.md §7.1, design-storagebackup.md §13). That is why this
// file carries no conversion and why the type implements no hub interface.
//
// The type follows design-storagebackup.md Appendix A.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageBackupPolicyPhase is where the operator has got to with this policy.
// +kubebuilder:validation:Enum=Pending;Active;Failed
type StorageBackupPolicyPhase string

const (
	StorageBackupPolicyPhasePending StorageBackupPolicyPhase = "Pending"
	StorageBackupPolicyPhaseActive  StorageBackupPolicyPhase = "Active"
	StorageBackupPolicyPhaseFailed  StorageBackupPolicyPhase = "Failed"
)

// StorageBackupPolicySpec schedules and retains the backups of the claims it
// selects, or of one consistency group it names. The operator reconciles it into
// the control plane and reports what the control plane did; it does not run the
// schedule itself, because a backup schedule that stopped when the operator was
// down would be one nobody could rely on.
// +kubebuilder:validation:XValidation:rule="!(has(self.claimSelector) && has(self.consistencyGroupName))",message="set at most one of claimSelector and consistencyGroupName: a policy backs up either independent claims or one whole consistency group"
type StorageBackupPolicySpec struct {
	// ClusterRef names the StorageCluster whose backup target this policy writes
	// to.
	//
	// Bounded at what a StorageCluster name may be, since a longer value names
	// nothing that can exist.
	// +kubebuilder:validation:MaxLength=63
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Cluster Ref"
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// ClaimSelector selects the PersistentVolumeClaims this policy backs up. An
	// absent selector selects nothing, which is deliberate: the cost of backing
	// up too much is silent and recurring, and the cost of backing up too little
	// is an error somebody sees. The controller reports an inert policy with a
	// SelectorEmpty event rather than refusing it.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Claim Selector"
	// +optional
	ClaimSelector *metav1.LabelSelector `json:"claimSelector,omitempty"`

	// ConsistencyGroupName names the consistency group this policy backs up as
	// one unit, resolved in ClusterRef's cluster. When set, the policy takes one
	// crash-consistent generation of the group per scheduled interval and backs
	// up every member together, rather than backing up selected claims
	// independently. The group is named rather than selected by label because the
	// membership label's key is constant and only its value, the group name,
	// varies. Set at most one of this and ClaimSelector.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Consistency Group Name"
	// +kubebuilder:validation:MaxLength=63
	// +optional
	ConsistencyGroupName string `json:"consistencyGroupName,omitempty"`

	// Schedule is the tiered schedule the control plane runs the policy on, as a
	// space-separated list of interval,keep_count pairs ("15m,4 60m,11 24h,7"):
	// four quarter-hourly backups, eleven hourly, and seven daily. Intervals
	// must be strictly increasing, and the supported units are m, h, d, and w.
	//
	// It is also the policy's whole statement of retention. The control plane
	// keeps as many backups as the tiers' keep counts add up to, for as long as
	// the furthest tier reaches back, and status.retention shows both numbers.
	// There is no separate version or age cap to set, so the object cannot
	// promise one thing in the schedule and another beside it.
	//
	// Immutable, and that is a property of the control plane rather than a
	// choice. The control plane offers no call that applies a changed schedule:
	// it creates, deletes, attaches, and detaches a policy and nothing else. A
	// mutable field the operator cannot reconcile would leave the declaration
	// and the backups actually being taken permanently disagreeing, with the
	// object still reporting Active, so the schedule is fixed at creation until
	// that call exists. Changing one means replacing the policy.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Schedule"
	// +kubebuilder:validation:Pattern=`^(\d+[mhdw],\d+)( +\d+[mhdw],\d+)*$`
	// +k8s:immutable
	// +optional
	Schedule string `json:"schedule,omitempty"`
}

// BackupRetention is what the schedule keeps, as the operator read it off the
// tiers and asked the control plane to enforce.
type BackupRetention struct {
	// MaxVersions is how many backups of one claim the control plane keeps: the
	// schedule's keep counts added up. Zero means no limit by count.
	// +optional
	MaxVersions int32 `json:"maxVersions,omitempty"`

	// MaxAge is how long the control plane keeps a backup ("7d," "36h"): the
	// reach of the schedule's furthest tier. Empty means no limit by age.
	// +optional
	MaxAge string `json:"maxAge,omitempty"`
}

// AttachedClaim is one claim the policy currently covers, in Kubernetes terms
// rather than in the control plane's.
type AttachedClaim struct {
	// Name is the PersistentVolumeClaim's name in this namespace.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// PersistentVolumeName is the volume behind it.
	// +optional
	PersistentVolumeName string `json:"persistentVolumeName,omitempty"`

	// LvolID is the logical volume the control plane attached the policy to. It
	// is what a detach addresses, and it is recorded per claim so that a claim
	// rebound onto a new volume is detached from the old one rather than
	// silently left attached.
	// +optional
	LvolID string `json:"lvolID,omitempty"`

	// ClusterID is the backend cluster the claim's volume is in, whose policy
	// it is attached to.
	// +optional
	ClusterID string `json:"clusterID,omitempty"`

	// AttachedAt is when the claim started matching the selector.
	// +optional
	AttachedAt *metav1.Time `json:"attachedAt,omitempty"`
}

// BackupPolicyMemberStatus is the policy in one member cluster of a
// supercluster.
type BackupPolicyMemberStatus struct {
	// Cluster is the member StorageCluster.
	Cluster string `json:"cluster"`

	// ClusterID is the member's backend cluster.
	// +optional
	ClusterID string `json:"clusterID,omitempty"`

	// PolicyID is the control plane's identifier for the policy in that
	// member.
	// +optional
	PolicyID string `json:"policyID,omitempty"`
}

// StorageBackupPolicyStatus is the observed state of the policy.
type StorageBackupPolicyStatus struct {
	// Phase is the operator's own view of this policy.
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Phase"
	// +optional
	Phase StorageBackupPolicyPhase `json:"phase,omitempty"`

	// ClusterID is the backend cluster the policy was created in.
	// +optional
	ClusterID string `json:"clusterID,omitempty"`

	// PolicyID is the control plane's identifier for the policy.
	// +optional
	PolicyID string `json:"policyID,omitempty"`

	// Members is the policy in each member, when spec.clusterRef names a
	// supercluster: one control-plane policy per member, each covering the
	// claims whose volumes are in that member. ClusterID and PolicyID stay
	// empty for such a policy.
	// +optional
	// +listType=map
	// +listMapKey=cluster
	Members []BackupPolicyMemberStatus `json:"members,omitempty"`

	// AttachedClaims are the claims the policy currently covers. Detaching one
	// stops new backups being taken and deletes none of the existing ones.
	// +optional
	AttachedClaims []AttachedClaim `json:"attachedClaims,omitempty"`

	// LastBackupAt is when the control plane last completed a backup under this
	// policy, and it is what an age alert is computed from.
	// +optional
	LastBackupAt *metav1.Time `json:"lastBackupAt,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the policy moves, and never a log.
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Message"
	// +optional
	Message string `json:"message,omitempty"`

	// Retention is what spec.schedule keeps, as derived from its tiers and
	// handed to the control plane: the version count and the age the pruning
	// honors. Absent while the policy has no schedule.
	// +optional
	Retention *BackupRetention `json:"retention,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbp
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=".spec.schedule"
// +kubebuilder:printcolumn:name="Claims",type=integer,JSONPath=".status.attachedClaims.length()"
// +kubebuilder:printcolumn:name="LastBackup",type=date,JSONPath=".status.lastBackupAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +operator-sdk:csv:customresourcedefinitions:displayName="Storage Backup Policy",resources={{PersistentVolumeClaim,v1,covered-claim}}

// StorageBackupPolicy schedules and retains the backups of the claims it
// selects. It is the only thing that decides a backup is taken; the copies
// themselves are the control plane's, are pruned by its retention, and reach
// Kubernetes as StorageBackup objects the operator discovers.
type StorageBackupPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StorageBackupPolicy
	// +required
	Spec StorageBackupPolicySpec `json:"spec"`

	// status defines the observed state of StorageBackupPolicy
	// +optional
	Status StorageBackupPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StorageBackupPolicyList contains a list of StorageBackupPolicy.
type StorageBackupPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StorageBackupPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageBackupPolicy{}, &StorageBackupPolicyList{})
}
