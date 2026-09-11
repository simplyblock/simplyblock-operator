// Conversion of StorageBackup between this version and the v1alpha2 hub.
//
// The kind changes shape rather than a field name, so this is longer than the
// conversions next door. Three things move
// (design-storagebackup.md §5.1, §5.2, and §13):
//
//   - spec.clusterName becomes spec.clusterRef, matching every other reference
//     in the group.
//   - The store's identifier moves into the spec. The hub's identity is the
//     cluster and the backup id, and this version only ever held the id in
//     status, so the conversion reads it from there and writes it back there.
//   - The twenty-two flat status fields regroup. status.backup is the copy and
//     status.source is what the volume was when the copy was taken, and each
//     group is left absent rather than empty when nothing has observed the
//     backup yet.
//
// Four fields have no counterpart in the other version, and each is stashed in
// an annotation rather than dropped, which is what design-api-upgrade.md §6.2
// requires of any conversion: an object is read and written back by clients of
// both versions, and a field with nowhere to go is truncated on the round trip
// unless something carries it. The stash and unstash helpers in
// storagepool_conversion.go are the mechanism, shared with the kind that needed
// them first.
//
// Two of them are the hub's and are stashed going down. status.activeOpsRef is
// the lock a StorageBackupOps holds on its target, and this version has no field
// for it: while an upgrade keeps storage at v1alpha1, every v1alpha2 write is
// converted down to be stored, so a lock that did not survive that would leave
// two restores of one backup each believing they held it.
// status.observedGeneration is what says whether a status is current, and a
// reader cannot tell a lost one from a stale one.
//
// Two are this version's and are stashed going up, and both are requests rather
// than observations, which is the kind's whole change: the hub has no requests.
// spec.snapshotName asked for a snapshot name and status.snapshotName records
// the one that was used, so status.source.snapshotName carries the fact while
// the stash carries the request. spec.sourceClusterUUID is the same shape of
// thing against status.sourceClusterUUID.
//
// Stashing both halves of each pair is not redundant, and the reason is worth
// stating because the first version of this conversion got it wrong: what an
// object asked for and what happened are different facts, and an object where
// only one of them is set is a state this version can express and the hub cannot.
// Recovering the request from the observation would invent a request nobody made.
//
// status.allowedHosts is the fourth, and it is neither: connection metadata that
// is neither the copy nor the source, so §5.2's regrouping has nowhere to put it.

package v1alpha1

import (
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// storageBackupPhaseToHub maps this version's six phases onto the hub's four.
// It is not a bijection, which is why the inverse below is written out rather
// than derived: the control plane's merging and deleting states are lifecycle
// detail the hub keeps in status.apiStatus, and folding them into Creating says
// the copy is not stably restorable without claiming it failed.
// The annotations this kind stashes into, keyed by the JSON path each value came
// from so that one on a live object says what it is standing in for.
const (
	annoBackupActiveOpsRef       = "storage.simplyblock.io/conversion-status.activeOpsRef"
	annoBackupObservedGeneration = "storage.simplyblock.io/conversion-status.observedGeneration"
	annoBackupAllowedHosts       = "storage.simplyblock.io/conversion-status.allowedHosts"
	annoBackupSnapshotRequest    = "storage.simplyblock.io/conversion-spec.snapshotName"
	annoBackupSourceRequest      = "storage.simplyblock.io/conversion-spec.sourceClusterUUID"
)

var storageBackupPhaseToHub = map[string]string{
	BackupPhasePending:    string(v1alpha2.StorageBackupPhasePending),
	BackupPhaseInProgress: string(v1alpha2.StorageBackupPhaseCreating),
	BackupPhaseDone:       string(v1alpha2.StorageBackupPhaseAvailable),
	BackupPhaseFailed:     string(v1alpha2.StorageBackupPhaseFailed),
	BackupPhaseMerging:    string(v1alpha2.StorageBackupPhaseCreating),
	BackupPhaseDeleting:   string(v1alpha2.StorageBackupPhaseCreating),
}

// storageBackupPhaseFromHub is the four-value inverse. Creating maps back to
// InProgress and not to either of the two states that also fold into it,
// because InProgress is the one this version's controllers wrote.
var storageBackupPhaseFromHub = map[string]string{
	string(v1alpha2.StorageBackupPhasePending):   BackupPhasePending,
	string(v1alpha2.StorageBackupPhaseCreating):  BackupPhaseInProgress,
	string(v1alpha2.StorageBackupPhaseAvailable): BackupPhaseDone,
	string(v1alpha2.StorageBackupPhaseFailed):    BackupPhaseFailed,
}

// ConvertTo converts this StorageBackup to the v1alpha2 hub.
func (src *StorageBackup) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.StorageBackup)

	// Deep-copied rather than assigned, because both halves of the stash mutate
	// the annotation map and a shared one would write through to the object the
	// API server handed in.
	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()

	dst.Spec = v1alpha2.StorageBackupSpec{
		ClusterRef: src.Spec.ClusterName,
		BackupID:   src.Status.BackupID,
	}

	dst.Status = v1alpha2.StorageBackupStatus{
		Phase: v1alpha2.StorageBackupPhase(
			mapOrPassThrough(storageBackupPhaseToHub, src.Status.Phase),
		),
		APIStatus: src.Status.APIStatus,
		ClusterID: src.Status.ClusterUUID,
		Backup:    src.backupCopy(),
		Source:    src.backupSource(),
		Message:   src.Status.Message,
		// Taken back out of the stash this object was stored with, and removed
		// from it: the field is where the value lives, and leaving the
		// annotation behind would state the same fact twice.
	}
	if err := unstash(&dst.ObjectMeta, annoBackupActiveOpsRef, &dst.Status.ActiveOpsRef); err != nil {
		return err
	}
	if err := unstash(&dst.ObjectMeta, annoBackupObservedGeneration, &dst.Status.ObservedGeneration); err != nil {
		return err
	}

	// Put this version's own unrepresentable fields where the trip back down can
	// find them.
	for _, stashed := range []struct {
		key   string
		value any
	}{
		{annoBackupAllowedHosts, src.Status.AllowedHosts},
		{annoBackupSnapshotRequest, src.Spec.SnapshotName},
		{annoBackupSourceRequest, src.Spec.SourceClusterUUID},
	} {
		if err := stash(&dst.ObjectMeta, stashed.key, stashed.value); err != nil {
			return err
		}
	}

	return nil
}

// backupCopy is the status.backup group, or nil when nothing in it is set. An
// empty group would be an object of nothing that every reader has to check
// past, so absence is spelled as absence.
func (src *StorageBackup) backupCopy() *v1alpha2.BackupCopy {
	copied := v1alpha2.BackupCopy{
		BackupID:         src.Status.BackupID,
		S3ID:             src.Status.S3ID,
		PreviousBackupID: src.Status.PrevBackupID,
		StartedAt:        src.Status.CreatedAt,
		CompletedAt:      src.Status.CompletedAt,
	}
	// Zero is a size the control plane reports for a backup that has not
	// transferred anything yet, and the hub states the field as a pointer so
	// that it is distinguishable from unset.
	if src.Status.Size != 0 {
		copied.Size = ptr.To(src.Status.Size)
	}
	if copied == (v1alpha2.BackupCopy{}) {
		return nil
	}
	return &copied
}

// backupSource is the status.source group, or nil when nothing in it is set.
//
// Two of its fields have two possible origins in this version, and in both
// cases status wins over spec: status records what happened and spec records
// what was asked for, and the group is the record of what happened.
func (src *StorageBackup) backupSource() *v1alpha2.BackupSource {
	source := v1alpha2.BackupSource{
		ClaimNamespace:       src.Status.PVCNamespace,
		PersistentVolumeName: src.Status.PVName,
		PoolName:             src.Status.PoolName,
		PoolUUID:             src.Status.PoolUUID,
		LvolID:               src.Status.LvolID,
		LvolName:             src.Status.LvolName,
		FSType:               src.Status.FSType,
		SnapshotID:           src.Status.SnapshotID,
		SnapshotName:         firstNonEmpty(src.Status.SnapshotName, src.Spec.SnapshotName),
		NodeID:               src.Status.NodeID,
		ClusterUUID:          firstNonEmpty(src.Status.SourceClusterUUID, src.Spec.SourceClusterUUID),
	}
	if src.Spec.PVCRef != nil {
		source.ClaimName = src.Spec.PVCRef.Name
		source.ClaimNamespace = firstNonEmpty(source.ClaimNamespace, src.Spec.PVCRef.Namespace)
	}

	if source == (v1alpha2.BackupSource{}) {
		return nil
	}
	return &source
}

// ConvertFrom converts the v1alpha2 hub into this StorageBackup.
func (dst *StorageBackup) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.StorageBackup)

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()

	dst.Spec = StorageBackupSpec{ClusterName: src.Spec.ClusterRef}

	dst.Status = StorageBackupStatus{
		Phase:       mapOrPassThrough(storageBackupPhaseFromHub, string(src.Status.Phase)),
		APIStatus:   src.Status.APIStatus,
		Message:     src.Status.Message,
		ClusterUUID: src.Status.ClusterID,
		BackupID:    src.Spec.BackupID,
	}

	// This version's own fields, taken back out of the stash the hub carried
	// them in. Each is removed as it is restored: the field is where the value
	// lives, and leaving the annotation behind would state the same fact twice.
	for _, restored := range []struct {
		key    string
		target any
	}{
		{annoBackupAllowedHosts, &dst.Status.AllowedHosts},
		{annoBackupSnapshotRequest, &dst.Spec.SnapshotName},
		{annoBackupSourceRequest, &dst.Spec.SourceClusterUUID},
	} {
		if err := unstash(&dst.ObjectMeta, restored.key, restored.target); err != nil {
			return err
		}
	}

	// The hub's own fields, put where the trip back up can find them.
	if err := stash(&dst.ObjectMeta, annoBackupActiveOpsRef, src.Status.ActiveOpsRef); err != nil {
		return err
	}
	if err := stash(&dst.ObjectMeta, annoBackupObservedGeneration, src.Status.ObservedGeneration); err != nil {
		return err
	}

	if backup := src.Status.Backup; backup != nil {
		// spec.backupID and status.backup.backupID are the same identifier by
		// construction, and the spec is the one that is required, so it is the
		// one read above. This keeps the group's copy honest where an older
		// object carries only one of them.
		dst.Status.BackupID = firstNonEmpty(src.Spec.BackupID, backup.BackupID)
		dst.Status.S3ID = backup.S3ID
		dst.Status.PrevBackupID = backup.PreviousBackupID
		dst.Status.Size = ptr.FromOrZero(backup.Size)
		dst.Status.CreatedAt = backup.StartedAt
		dst.Status.CompletedAt = backup.CompletedAt
	}

	if source := src.Status.Source; source != nil {
		dst.Status.PVCNamespace = source.ClaimNamespace
		dst.Status.PVName = source.PersistentVolumeName
		dst.Status.PoolName = source.PoolName
		dst.Status.PoolUUID = source.PoolUUID
		dst.Status.LvolID = source.LvolID
		dst.Status.LvolName = source.LvolName
		dst.Status.FSType = source.FSType
		dst.Status.SnapshotID = source.SnapshotID
		dst.Status.SnapshotName = source.SnapshotName
		dst.Status.NodeID = source.NodeID
		dst.Status.SourceClusterUUID = source.ClusterUUID

		// The claim is the one thing the hub keeps in status that this version
		// keeps in spec, and rebuilding the reference is what makes the trip
		// down and back up return the object that was written. A source with a
		// namespace and no name is a backup whose claim is gone, and it gets no
		// reference rather than one naming nothing.
		if source.ClaimName != "" {
			dst.Spec.PVCRef = &PersistentVolumeClaimRef{
				Name:      source.ClaimName,
				Namespace: source.ClaimNamespace,
			}
		}
	}

	return nil
}

// firstNonEmpty is the first of its arguments that is set, or the empty string.
// It exists for the fields this version records in two places, where the
// conversion has to state which one wins rather than picking whichever it
// happened to read last.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
