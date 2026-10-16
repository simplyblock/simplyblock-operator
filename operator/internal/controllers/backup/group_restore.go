// The Restore action's group path: restoring a whole consistency-group backup
// into a new group of claims (design-consistency-group-backup.md §6, §7.3).
//
// It is the same four-step graph the single-backup restore walks, branched where
// the two differ. Validating resolves the cluster and pool; Restoring asks the
// control plane to restore every member as one unit and records the volumes it
// produced; AwaitingVolumes waits for all of them to come online; and Binding
// creates one claim per member, named <claimName>-<member> and labeled into the
// restored consistency group, so the restored claims form a group the way their
// originals did. The member volumes and claims carry the operation's UID in their
// names, so a restart finds its own work rather than asking again.

package backup

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// consistencyGroupLabel marks a restored claim as a member of the consistency
// group a group restore forms, so the restored claims are a group the way their
// originals were (design §7.3). Its value is the restored group's name.
const consistencyGroupLabel = "storage.simplyblock.io/consistency-group"

// listGroupMembers resolves the member StorageBackups of this group backup, the
// StorageBackups carrying the group-backup-id label.
func (r *StorageBackupOpsReconciler) listGroupMembers(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) ([]simplyblockv1alpha2.StorageBackup, error) {
	var members simplyblockv1alpha2.StorageBackupList
	if err := r.List(ctx, &members,
		client.InNamespace(ops.Namespace),
		client.MatchingLabels{simplyblockv1alpha2.BackupLabelGroupBackupID: ops.Spec.GroupBackupID},
	); err != nil {
		return nil, err
	}
	return members.Items, nil
}

// acquireGroupLock takes the lock on every member of the group, so two group
// restores of one generation serialize (§7.3). It is all-or-nothing: if any
// member is held by another operation, this one waits and takes none.
func (r *StorageBackupOpsReconciler) acquireGroupLock(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (bool, error) {
	members, err := r.listGroupMembers(ctx, ops)
	if err != nil {
		return false, err
	}
	if len(members) == 0 {
		_, err := r.finish(ctx, ops, simplyblockv1alpha2.StorageBackupOpsPhaseAborted,
			fmt.Sprintf("group backup %s has no members", ops.Spec.GroupBackupID))
		return false, err
	}

	holder := lockHolder(ops)
	for i := range members {
		if held := members[i].Status.ActiveOpsRef; held != "" && held != holder {
			r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
				ReasonOperationQueued, ReasonOperationQueued,
				"Group backup member %s is held by operation %s; this one is waiting", members[i].Name, held)
			return false, r.hold(ctx, ops, fmt.Sprintf(
				"waiting for operation %s to release group backup member %s", held, members[i].Name))
		}
	}
	for i := range members {
		if members[i].Status.ActiveOpsRef == holder {
			continue
		}
		patch := client.MergeFromWithOptions(members[i].DeepCopy(), client.MergeFromWithOptimisticLock{})
		members[i].Status.ActiveOpsRef = holder
		if err := r.Status().Patch(ctx, &members[i], patch); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, fmt.Errorf("acquire the lock on group backup member %s: %w", members[i].Name, err)
		}
	}

	if ops.Status.Phase == "" || ops.Status.Phase == simplyblockv1alpha2.StorageBackupOpsPhasePending {
		now := metav1.Now()
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
			status.Phase = simplyblockv1alpha2.StorageBackupOpsPhaseRunning
			status.StartedAt = &now
			status.Message = "The operation holds the group backup and is running"
		}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// releaseGroupLock clears the lock on every member it still holds.
func (r *StorageBackupOpsReconciler) releaseGroupLock(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) error {
	members, err := r.listGroupMembers(ctx, ops)
	if err != nil {
		return err
	}
	holder := lockHolder(ops)
	for i := range members {
		if members[i].Status.ActiveOpsRef != holder {
			continue
		}
		patch := client.MergeFromWithOptions(members[i].DeepCopy(), client.MergeFromWithOptimisticLock{})
		members[i].Status.ActiveOpsRef = ""
		if err := r.Status().Patch(ctx, &members[i], patch); err != nil && !apierrors.IsConflict(err) {
			return fmt.Errorf("release the lock on group backup member %s: %w", members[i].Name, err)
		}
	}
	return nil
}

// validateGroup resolves the cluster and the target pool for a group restore. The
// members were validated at admission, and the per-member copies are read by the
// control plane rather than one at a time here, so there is no single backup to
// resolve.
func (r *StorageBackupOpsReconciler) validateGroup(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageBackupOps,
	clusterID string,
	restore *simplyblockv1alpha2.RestoreSpec,
) (bool, error) {
	poolUUID, err := utils.ResolvePoolUUID(ctx, r.Client,
		clusterKey(ops).Namespace, ops.Spec.ClusterRef.Name, restore.TargetPool)
	if err != nil {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, ReasonPoolNotFound, ReasonPoolNotFound,
			"The target pool %s could not be resolved: %v", restore.TargetPool, err)
		return false, fatalf("the target pool %s is not a pool of cluster %s: %v",
			restore.TargetPool, ops.Spec.ClusterRef.Name, err)
	}
	return true, r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
		status.ClusterID = clusterID
		status.PoolUUID = poolUUID
		status.Message = fmt.Sprintf("Restoring group backup %s into pool %s",
			ops.Spec.GroupBackupID, restore.TargetPool)
	})
}

// startGroupRestore asks the control plane to restore the whole group backup into
// a new consistency group, and records the volumes it produced.
//
// Idempotent through the recorded members: once they are written the step is
// done, so a re-entry does not ask again. The control-plane call is guarded by
// the step's claim so two passes cannot both issue it. A pass that issued the
// call and died before recording the members would, on retry, be answered that
// the group already exists; recovering that case by resolving the group by name
// is a follow-up (the control plane refuses a duplicate name rather than creating
// a second group, so the failure is loud rather than lossy).
func (r *StorageBackupOpsReconciler) startGroupRestore(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (bool, error) {
	if len(ops.Status.RestoredGroupMembers) > 0 {
		return true, nil
	}

	var groupID string
	var members []controlplane.RestoredGroupMember
	claimed, err := r.once(ctx, ops, func() error {
		var e error
		groupID, members, e = r.API.RestoreGroupBackup(ctx, ops.Status.ClusterID,
			controlplane.RestoreGroupBackupParams{
				GroupBackupID: ops.Spec.GroupBackupID,
				IntoGroupName: restoredGroupName(ops),
				Pool:          ops.Spec.Restore.TargetPool,
			})
		if e != nil {
			return fmt.Errorf("ask the control plane to restore group backup %s: %w",
				ops.Spec.GroupBackupID, e)
		}
		return nil
	})
	if err != nil || !claimed {
		return false, err
	}

	recorded := make([]simplyblockv1alpha2.RestoredGroupMember, len(members))
	for i, m := range members {
		recorded[i] = simplyblockv1alpha2.RestoredGroupMember{LvolID: m.LvolID, MemberName: m.MemberName}
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
		ReasonGroupRestoreStarted, ReasonGroupRestoreStarted,
		"The control plane accepted the restore of group backup %s and is filling %d volume(s)",
		ops.Spec.GroupBackupID, len(recorded))
	return true, r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
		status.RestoredGroupID = groupID
		status.RestoredGroupMembers = recorded
		status.Message = fmt.Sprintf(
			"The control plane accepted the group restore and is filling %d volume(s)", len(recorded))
	})
}

// awaitGroupVolumes waits for every restored member volume to come online. A
// member whose transfer the control plane gave up on fails the whole operation,
// the group's all-or-nothing rule applied to the restore.
func (r *StorageBackupOpsReconciler) awaitGroupVolumes(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (bool, error) {
	for _, m := range ops.Status.RestoredGroupMembers {
		handle := lvol.NewVolumeHandle(ops.Status.ClusterID, ops.Status.PoolUUID, m.LvolID)
		volume, err := r.API.Volume(ctx, handle)
		if err != nil {
			return false, fmt.Errorf("read the restored volume %s: %w", m.LvolID, err)
		}
		switch volume.Status {
		case cpVolumeRestoreFailed:
			return false, fatalf(
				"the control plane gave up transferring a member of group backup %s into volume %s",
				ops.Spec.GroupBackupID, m.LvolID)
		case cpVolumeOnline:
			continue
		default:
			return false, nil // not every member is online yet
		}
	}
	return true, nil
}

// bindGroup produces one claim per restored member, named <claimName>-<member>
// and labeled into the restored consistency group. It is the last step, so a
// group restore that fails or aborts earlier leaves no claims to leak, and a
// claim that exists has a restored volume behind it.
func (r *StorageBackupOpsReconciler) bindGroup(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (bool, error) {
	// Each member's own StorageBackup carries its size and the filesystem the copy
	// was taken with, resolved once by the group-backup-id label and keyed by the
	// source member name the restore reports.
	backups, err := r.memberBackupsByName(ctx, ops)
	if err != nil {
		return false, err
	}

	allBound := true
	for i := range ops.Status.RestoredGroupMembers {
		m := ops.Status.RestoredGroupMembers[i]
		claimName := restoredMemberClaimName(ops, m.MemberName)

		backup, ok := backups[m.MemberName]
		if !ok {
			return false, fatalf(
				"no StorageBackup for restored member %s of group backup %s",
				m.MemberName, ops.Spec.GroupBackupID)
		}

		// Write-ahead: the claim name is recorded before the claim is created, so a
		// crash between the two restarts into a record that names the claim rather
		// than one that does not.
		if m.ClaimName == "" {
			if err := r.recordMemberClaim(ctx, ops, m.LvolID, claimName); err != nil {
				return false, err
			}
		}

		if err := r.ensureMemberVolume(ctx, ops, m.LvolID, claimName, backup); err != nil {
			return false, err
		}
		if err := r.ensureMemberClaim(ctx, ops, m, claimName, backup); err != nil {
			return false, err
		}

		var claim corev1.PersistentVolumeClaim
		if err := r.Get(ctx,
			client.ObjectKey{Name: claimName, Namespace: ops.Namespace}, &claim); err != nil {
			return false, err
		}
		if claim.Status.Phase != corev1.ClaimBound {
			allBound = false
		}
	}
	return allBound, nil
}

// memberBackupsByName resolves the member StorageBackups of this group backup,
// keyed by the source member name a restored member reports. The members are the
// StorageBackups carrying the group-backup-id label, the same selector the
// admission webhook resolved the group against.
func (r *StorageBackupOpsReconciler) memberBackupsByName(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (map[string]*simplyblockv1alpha2.StorageBackup, error) {
	var members simplyblockv1alpha2.StorageBackupList
	if err := r.List(ctx, &members,
		client.InNamespace(ops.Namespace),
		client.MatchingLabels{simplyblockv1alpha2.BackupLabelGroupBackupID: ops.Spec.GroupBackupID},
	); err != nil {
		return nil, err
	}
	byName := make(map[string]*simplyblockv1alpha2.StorageBackup, len(members.Items))
	for i := range members.Items {
		byName[members.Items[i].Source().LvolName] = &members.Items[i]
	}
	return byName, nil
}

// recordMemberClaim writes down the claim one restored member is bound to.
func (r *StorageBackupOpsReconciler) recordMemberClaim(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps, lvolID, claimName string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
		for i := range status.RestoredGroupMembers {
			if status.RestoredGroupMembers[i].LvolID == lvolID {
				status.RestoredGroupMembers[i].ClaimName = claimName
			}
		}
		status.Message = fmt.Sprintf("Binding the restored group into %d claim(s)",
			len(status.RestoredGroupMembers))
	})
}

// ensureMemberVolume writes the PersistentVolume one restored member binds to,
// pre-bound to that member's claim so no other claim can take it. The ownership
// checks are the single-restore path's, applied per member.
func (r *StorageBackupOpsReconciler) ensureMemberVolume(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageBackupOps,
	lvolID, claimName string,
	backup *simplyblockv1alpha2.StorageBackup,
) error {
	name := restoredMemberVolumeName(ops, lvolID)
	handle := lvol.NewVolumeHandle(ops.Status.ClusterID, ops.Status.PoolUUID, lvolID)

	var existing corev1.PersistentVolume
	err := r.Get(ctx, client.ObjectKey{Name: name}, &existing)
	if err == nil {
		switch {
		case existing.Spec.CSI == nil || existing.Spec.CSI.VolumeHandle != string(handle):
			return fatalf("PersistentVolume %s already exists and names another volume", name)
		case existing.Labels[RestoredByLabel] != ops.Name:
			return fatalf("PersistentVolume %s already exists and was not created by this operation", name)
		case existing.Spec.ClaimRef == nil ||
			existing.Spec.ClaimRef.Name != claimName ||
			existing.Spec.ClaimRef.Namespace != ops.Namespace:
			return fatalf("PersistentVolume %s already exists and is bound to another claim", name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	connection, err := r.API.Connection(ctx, handle)
	if err != nil {
		return fmt.Errorf("read how to reach the restored volume %s: %w", lvolID, err)
	}
	attributes, err := atlaskube.CSIVolumeAttributes(handle, connection)
	if err != nil {
		return fmt.Errorf("build the volume attributes for %s: %w", lvolID, err)
	}

	storageClass, err := r.restoreStorageClassName(ctx, ops)
	if err != nil {
		return err
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{RestoredByLabel: ops.Name},
		},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName:              storageClass,
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: restoredSize(backup)},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			ClaimRef: &corev1.ObjectReference{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Name:       claimName,
				Namespace:  ops.Namespace,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           utils.CSIProvisioner,
					VolumeHandle:     string(handle),
					VolumeAttributes: attributes,
					FSType:           backup.Source().FSType,
				},
			},
		},
	}
	if err := r.Create(ctx, pv); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// ensureMemberClaim creates one restored member's claim, labeled into the
// restored consistency group so the claims form a group.
func (r *StorageBackupOpsReconciler) ensureMemberClaim(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageBackupOps,
	member simplyblockv1alpha2.RestoredGroupMember,
	claimName string,
	backup *simplyblockv1alpha2.StorageBackup,
) error {
	if adopted, err := r.memberClaimIsOurs(ctx, ops, claimName); err != nil || adopted {
		return err
	}

	labels := map[string]string{}
	for key, value := range ops.Spec.Restore.ClaimLabels {
		labels[key] = value
	}
	labels[RestoredByLabel] = ops.Name
	labels[consistencyGroupLabel] = restoredGroupName(ops)

	storageClass, err := r.restoreStorageClassName(ctx, ops)
	if err != nil {
		return err
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        claimName,
			Namespace:   ops.Namespace,
			Labels:      labels,
			Annotations: ops.Spec.Restore.ClaimAnnotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr.To(storageClass),
			VolumeName:       restoredMemberVolumeName(ops, member.LvolID),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: restoredSize(backup)},
			},
		},
	}
	if err := r.Create(ctx, claim); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		adopted, err := r.memberClaimIsOurs(ctx, ops, claimName)
		if err != nil {
			return err
		}
		if !adopted {
			r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, ReasonClaimExists, ReasonClaimExists,
				"Claim %s was created by something else while the group restore was binding, so the restore was refused",
				claimName)
			return fatalf("claim %s was created by something else while this operation was binding it", claimName)
		}
	}
	return nil
}

// memberClaimIsOurs reports whether a claim of this name exists and was created
// by this operation, the per-member form of claimIsOurs.
func (r *StorageBackupOpsReconciler) memberClaimIsOurs(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps, claimName string,
) (bool, error) {
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Name: claimName, Namespace: ops.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if existing.Labels[RestoredByLabel] != ops.Name {
		return false, fatalf("claim %s exists and was not created by this operation", claimName)
	}
	return true, nil
}

// discardRestoredGroup deletes the volumes a group restore produced, for an abort
// at Restoring. Idempotent: a volume already gone is the state asked for.
func (r *StorageBackupOpsReconciler) discardRestoredGroup(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) error {
	for _, m := range ops.Status.RestoredGroupMembers {
		handle := lvol.NewVolumeHandle(ops.Status.ClusterID, ops.Status.PoolUUID, m.LvolID)
		if err := r.API.DeleteVolume(ctx, handle); err != nil {
			return fmt.Errorf("discard the restored volume %s: %w", m.LvolID, err)
		}
	}
	return nil
}

// restoredGroupName is the consistency group a group restore forms, named after
// the base claim name so the restored group reads as what it holds.
func restoredGroupName(ops *simplyblockv1alpha2.StorageBackupOps) string {
	return ops.Spec.Restore.ClaimName
}

// restoredMemberClaimName is <claimName>-<member>, the name of one restored
// member's claim.
func restoredMemberClaimName(ops *simplyblockv1alpha2.StorageBackupOps, memberName string) string {
	return ops.Spec.Restore.ClaimName + "-" + memberName
}

// restoredMemberVolumeName carries the operation's UID and the member's volume
// id, so two restores never collide and a name says which operation and member
// produced it.
func restoredMemberVolumeName(ops *simplyblockv1alpha2.StorageBackupOps, lvolID string) string {
	return "restore-" + string(ops.UID) + "-" + lvolID
}
