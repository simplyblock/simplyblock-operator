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
// selects. The operator reconciles it into the control plane and reports what
// the control plane did; it does not run the schedule itself, because a backup
// schedule that stopped when the operator was down would be one nobody could
// rely on.
type StorageBackupPolicySpec struct {
	// ClusterRef names the StorageCluster whose backup target this policy writes
	// to.
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

	// Schedule is the tiered schedule the control plane runs the policy on, as a
	// space-separated list of interval,keep_count pairs ("15m,4 60m,11 24h,7").
	// Intervals must be strictly increasing, and the supported units are m, h,
	// d, and w.
	//
	// Immutable, and that is a property of the control plane rather than a
	// choice. design-storagebackup.md §10 lists a PUT that applies a changed
	// schedule, and the v2 API offers no such endpoint: it creates, deletes,
	// attaches, and detaches a policy and nothing else. A mutable field the
	// operator cannot reconcile would leave the declaration and the backups
	// actually being taken permanently disagreeing, with the object still
	// reporting Active, so the schedule is fixed at creation until the endpoint
	// exists. Changing one means replacing the policy.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Schedule"
	// +kubebuilder:validation:Pattern=`^(\d+[mhdw],\d+)( +\d+[mhdw],\d+)*$`
	// +k8s:immutable
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// MaxVersions is how many backups of one claim to keep. Zero means no limit
	// by count. Immutable, for the reason Schedule is.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Max Versions"
	// +kubebuilder:validation:Minimum=0
	// +k8s:immutable
	// +optional
	MaxVersions *int32 `json:"maxVersions,omitempty"`

	// MaxAge is how long to keep a backup ("30d," "720h"). Empty means no limit
	// by age. Retention is enforced by the control plane, not here. Immutable,
	// for the reason Schedule is.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Max Age"
	// +kubebuilder:validation:Pattern=`^[1-9]\d*[mhdw]$`
	// +k8s:immutable
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

	// AttachedAt is when the claim started matching the selector.
	// +optional
	AttachedAt *metav1.Time `json:"attachedAt,omitempty"`
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
