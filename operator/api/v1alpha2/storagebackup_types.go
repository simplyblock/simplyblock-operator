// StorageBackup: one point-in-time copy of one volume, as the operator found it
// in a cluster's store.
//
// The kind is an observation rather than a request, which is the whole of why
// the spec is two fields. The cluster names an S3 location, the operator mirrors
// what that location holds, and one object appears per backup it reports. What
// the backup is of, how big it is, and where it came from are all things that
// were already true when the object was created, so they live in status
// (design-storagebackup.md §5.1).
//
// The status is in three groups, because the registered shape had twenty-two
// flat fields and no way to tell which of them described the copy and which
// described the volume it was taken from: status.backup is the copy, status.source
// is what the volume was when the copy was taken, and the rest is the operator's
// own view (design-storagebackup.md §5.2).
//
// The type follows design-storagebackup.md Appendix B. Three fields are here
// that the appendix does not list, and each is commented where it is declared.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/kube"
)

// The labels every StorageBackup object carries. They are how a backup is found
// by the thing somebody actually knows: the claim whose data is in it, or the
// cluster whose store holds it. The claim is not reachable by following an owner
// reference, because a backup outlives the claim it was taken from, so a label
// is the only way to ask the question.
//
// The keys are the ones the rest of the API group already uses for the same
// things, so a selector written for another kind selects the same scope here.
const (
	// BackupLabelCluster is the StorageCluster whose store the backup was found
	// in, named by its Kubernetes object rather than by the backend id in
	// status.clusterID.
	BackupLabelCluster = "storage.simplyblock.io/cluster"
	// BackupLabelClaim is the PersistentVolumeClaim the copy was taken from,
	// where the operator could resolve one. A backup whose claim is gone carries
	// no such label, which is the honest answer rather than a stale name.
	BackupLabelClaim = "storage.simplyblock.io/claim"
	// BackupLabelPolicy is the StorageBackupPolicy that scheduled the copy,
	// where the control plane reports one.
	BackupLabelPolicy = "storage.simplyblock.io/backup-policy"
)

// storageBackupNames derives a StorageBackup object's name from the identifier
// the store holds the backup under.
//
// A backup id is a UUID in every store this product has seen, and a UUID is
// already a legal object name, so the formula is a pass-through for it. It is a
// formula rather than a cast because the id is the store's rather than this
// operator's: §5.1 makes it the object's identity, and an identity that the API
// server refuses would leave a backup with no object at all. The digest a
// truncation adds is what keeps two long ids from landing on one name.
var storageBackupNames = kube.Formula{Kind: kube.ObjectName}

// StorageBackupName returns the object name mirroring one backup in a cluster's
// store. The same backup is the same object however many clusters have the
// location configured, which is why the name is the store's identifier and
// carries nothing about the cluster that reported it.
func StorageBackupName(backupID string) string {
	return storageBackupNames.Derive(backupID).Value
}

// StorageBackupPhase is where the operator has got to with this backup.
// Available is the terminal success rather than Succeeded, because a backup is
// not an operation: what matters afterward is that the copy can be restored, not
// that the copying finished.
// +kubebuilder:validation:Enum=Pending;Creating;Available;Failed
type StorageBackupPhase string

const (
	StorageBackupPhasePending   StorageBackupPhase = "Pending"
	StorageBackupPhaseCreating  StorageBackupPhase = "Creating"
	StorageBackupPhaseAvailable StorageBackupPhase = "Available"
	StorageBackupPhaseFailed    StorageBackupPhase = "Failed"
)

// BackupSource is what the volume was when the copy was taken. It is written
// once and never updated: the pool a volume was in when it was backed up is a
// fact about the backup, and rewriting it when the volume moves would destroy
// the only record of where the data came from. A restore reads it to know what
// it is restoring.
type BackupSource struct {
	// ClaimName and ClaimNamespace are the PersistentVolumeClaim the copy was
	// taken from. They are the pair a user recognizes, and the reason the claim
	// rather than the volume is the printed column.
	// +optional
	ClaimName string `json:"claimName,omitempty"`
	// +optional
	ClaimNamespace string `json:"claimNamespace,omitempty"`

	// PersistentVolumeName is the PV that was copied.
	// +optional
	PersistentVolumeName string `json:"persistentVolumeName,omitempty"`

	// PoolName and PoolUUID are the StoragePool the volume was in. A restore
	// names its own target pool rather than defaulting to this one, because a
	// backup discovered from a shared store may name a pool this cluster does
	// not have.
	// +optional
	PoolName string `json:"poolName,omitempty"`
	// +optional
	PoolUUID string `json:"poolUUID,omitempty"`

	// LvolID and LvolName identify the logical volume that was copied.
	// +optional
	LvolID string `json:"lvolID,omitempty"`
	// +optional
	LvolName string `json:"lvolName,omitempty"`

	// FSType is the filesystem the volume was formatted with, which a restore
	// needs in order to produce a mountable claim.
	// +optional
	FSType string `json:"fsType,omitempty"`

	// SnapshotID and SnapshotName identify the snapshot the backup was taken
	// from.
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`
	// +optional
	SnapshotName string `json:"snapshotName,omitempty"`

	// NodeID is the storage node the copy was read from. The appendix does not
	// list it and §1 counts it among the twenty-two, and it belongs to the
	// source rather than to the copy: it says which machine held the data, which
	// is what somebody correlating a backup against a node failure needs.
	// +optional
	NodeID string `json:"nodeID,omitempty"`

	// ClusterUUID is the cluster the volume lived in, which differs from the
	// backup's own cluster when another cluster wrote it.
	// +optional
	ClusterUUID string `json:"clusterUUID,omitempty"`
}

// BackupCopy is the copy itself: what it is, what it cost, and what it depends
// on.
type BackupCopy struct {
	// BackupID is the control plane's identifier for the copy.
	// +optional
	BackupID string `json:"backupID,omitempty"`

	// S3ID is the object behind the copy in the store. The appendix does not
	// list it and §5.2 names it, and it is what somebody reconciling a bill
	// against a bucket listing matches on.
	// +optional
	S3ID int64 `json:"s3ID,omitempty"`

	// Size is the copy's size in bytes, which is what a bill tracks.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Size *int64 `json:"size,omitempty"`

	// PreviousBackupID names the backup this one is incremental against, where
	// the control plane reports one. It makes the chain legible, which matters
	// because deleting a backup another depends on is not obviously safe.
	// +optional
	PreviousBackupID string `json:"previousBackupID,omitempty"`

	// StartedAt and CompletedAt bound how long the copy took. They are the
	// backup's own reported interval rather than transitions the operator
	// observed, because the stream coalesces and a small backup can be complete
	// before the operator ever saw it start.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// StorageBackupSpec is the identity of one backup the operator found in a
// cluster's store, and nothing else. The object is created by the operator and
// by nobody else (§5.1), so there is no request here to carry.
type StorageBackupSpec struct {
	// ClusterRef names the StorageCluster whose store this backup was found in.
	// With BackupID it is the whole of this object's identity.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Cluster Ref"
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// BackupID is the identifier the store holds the backup under, and what a
	// restore addresses. It is the store's identifier rather than a name this
	// operator assigns, so the same backup is the same object however many
	// clusters have the location configured.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Backup ID"
	// +kubebuilder:validation:Required
	// +k8s:immutable
	BackupID string `json:"backupID"`
}

// StorageBackupStatus is the observed state of one backup, in three groups: the
// copy, what the volume was, and the operator's own view.
type StorageBackupStatus struct {
	// Phase is the operator's own view of this backup.
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Phase"
	// +optional
	Phase StorageBackupPhase `json:"phase,omitempty"`

	// APIStatus is the control plane's own lifecycle string, in the control
	// plane's spelling, which is why it carries no Enum here. It is what keeps
	// the states the four-value phase folds together — merging, deleting —
	// legible.
	// +optional
	APIStatus string `json:"apiStatus,omitempty"`

	// ClusterID is the backend cluster whose stream this backup was last
	// observed on. The appendix does not list it, and the mirror cannot work
	// without it: an object outliving its backup has to say which scope's
	// silence is authoritative before it may be deleted, and spec.clusterRef
	// names a Kubernetes object rather than a backend one. It is the same field,
	// for the same reason, that StorageDevice carries
	// (design-storagedevice.md §5.1).
	// +optional
	ClusterID string `json:"clusterID,omitempty"`

	// Backup is the copy itself.
	// +optional
	Backup *BackupCopy `json:"backup,omitempty"`

	// Source is what the volume was when the copy was taken.
	// +optional
	Source *BackupSource `json:"source,omitempty"`

	// ActiveOpsRef names the StorageBackupOps currently allowed to act on this
	// backup. Empty when none is running.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the backup moves, and never a log.
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Message"
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one. On this kind it
	// moves at most once, since every spec field is fixed when the object is
	// created.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Copy returns the status.backup group, or a zero-valued one when the backup
// has not been observed yet.
//
// Both groups are pointers so that absence is spelled as absence rather than as
// an object of empty fields, and that makes every reader of one field write a
// nil check. These two accessors are where the check lives instead, which is
// the same shape StorageNodeOpsSpec.MigrateParams takes for its optional
// per-action block: a zero group reads as "nothing is known," which is what an
// absent group means.
func (b *StorageBackup) Copy() BackupCopy {
	if b.Status.Backup == nil {
		return BackupCopy{}
	}
	return *b.Status.Backup
}

// Source returns the status.source group, or a zero-valued one when the backup
// has not been observed yet.
func (b *StorageBackup) Source() BackupSource {
	if b.Status.Source == nil {
		return BackupSource{}
	}
	return *b.Status.Source
}

// v1alpha2 is the storage version in the manifests this repository ships, which
// are the ones a fresh install applies. A cluster installed today stores this
// shape from the first write and never converts anything, so the conversion
// webhook is inert there and is not deployed.
//
// An upgrade of an existing cluster is the other path, and it does not take this
// value. The upgrade tool applies these same CRDs with storage held at v1alpha1,
// because a server-side apply overwrites the live storage version and moving it
// before the conversion webhook is serving breaks every write. It flips to
// v1alpha2 with the storage rewrite once the migration has run
// (design-api-upgrade.md §24, design-property-renames.md §3.8).
// +kubebuilder:storageversion
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sb
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=".status.source.claimName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=".status.backup.size"
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=".status.source.poolName",priority=1
// +kubebuilder:printcolumn:name="BackupID",type=string,JSONPath=".status.backup.backupID",priority=1
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=".status.backup.completedAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +operator-sdk:csv:customresourcedefinitions:displayName="Storage Backup",resources={{PersistentVolume,v1,source-volume},{PersistentVolumeClaim,v1,source-claim}}

// StorageBackup is one point-in-time copy of one volume that exists in a
// cluster's store.
//
// Objects are created by the operator from what the store holds and are never
// written by a user: creating one by hand would claim a backup exists that the
// store does not hold, and deleting one would hide a backup that is still there
// and still restorable. Deleting the object never deletes the copy.
type StorageBackup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec identifies the backup this object reports on
	// +required
	Spec StorageBackupSpec `json:"spec"`

	// status defines the observed state of StorageBackup
	// +optional
	Status StorageBackupStatus `json:"status,omitzero"`
}

// Hub marks this version as the conversion hub for StorageBackup.
func (*StorageBackup) Hub() {}

// +kubebuilder:object:root=true

// StorageBackupList contains a list of StorageBackup.
type StorageBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StorageBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageBackup{}, &StorageBackupList{})
}
