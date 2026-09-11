// What each step of a Restore actually does.
//
// Every perform here is idempotent and reports whether its step is finished, so
// a reconcile that arrives twice on one step converges rather than doubling its
// work. Where a step has a side effect that cannot be repeated, the guard is a
// status field written before it: status.restoredLvolID guards the restore
// request, and status.claimName guards the claim.
//
// The step that matters most is Validating, and the check that matters most in
// it is that the claim does not already exist. Restoring over a claim a workload
// is using would replace its data with the backup's, which is the single most
// destructive thing this band can do. The admission webhook refuses the ordinary
// mistake at creation and this refuses the interleaving, and both are needed:
// a claim can be created between the two.

package backup

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// The control plane's volume statuses a restore cares about, in its own
// spelling. restore_failed is what the tasks runner sets after exhausting every
// retry of the S3 transfer, and it is the only one of the two that is a verdict.
const (
	cpVolumeOnline        = utils.NodeStatusOnline
	cpVolumeRestoreFailed = "restore_failed"
)

// perform advances the current step, and reports whether it finished.
func (r *StorageBackupOpsReconciler) perform(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps, current step,
) (bool, error) {
	switch current {
	case stepValidating:
		return r.validate(ctx, ops)
	case stepRestoring:
		return r.startRestore(ctx, ops)
	case stepAwaitingVolume:
		return r.awaitVolume(ctx, ops)
	case stepBinding:
		return r.bind(ctx, ops)
	default:
		return false, fatalf("step %s belongs to no action this controller drives", current)
	}
}

// validate resolves every name the operation depends on and refuses the two
// conditions that make the rest of it impossible: a copy that does not exist,
// and a claim name that is already taken.
func (r *StorageBackupOpsReconciler) validate(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (bool, error) {
	restore := ops.Spec.Restore
	if restore == nil {
		return false, fatalf("action %s needs a spec.restore block and has none", ops.Spec.Action)
	}

	clusterID, err := r.clusterIDFor(ctx, ops)
	if err != nil {
		return false, fmt.Errorf("resolve cluster %s: %w", ops.Spec.ClusterRef, err)
	}

	var backup simplyblockv1alpha2.StorageBackup
	if err := r.Get(ctx,
		client.ObjectKey{Name: ops.Spec.BackupRef, Namespace: ops.Namespace}, &backup); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fatalf("StorageBackup %s does not exist", ops.Spec.BackupRef)
		}
		return false, err
	}
	switch backup.Status.Phase {
	case simplyblockv1alpha2.StorageBackupPhaseFailed:
		// A failed backup has no copy behind it, and it does not recover: it is
		// replaced by another backup rather than repaired.
		return false, fatalf("StorageBackup %s failed and has no copy to restore", backup.Name)
	case simplyblockv1alpha2.StorageBackupPhaseAvailable:
	default:
		// Still being written. Waiting is right rather than failing: the copy is
		// on its way to being restorable.
		return false, nil
	}

	poolUUID, err := utils.ResolvePoolUUID(ctx, r.Client, ops.Namespace, ops.Spec.ClusterRef, restore.TargetPool)
	if err != nil {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, ReasonPoolNotFound, ReasonPoolNotFound,
			"The target pool %s could not be resolved: %v", restore.TargetPool, err)
		return false, fatalf("the target pool %s is not a pool of cluster %s: %v",
			restore.TargetPool, ops.Spec.ClusterRef, err)
	}

	if err := r.refuseExistingClaim(ctx, ops, restore.ClaimName); err != nil {
		return false, err
	}

	return true, r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
		status.ClusterID = clusterID
		status.BackupID = backup.Spec.BackupID
		status.PoolUUID = poolUUID
		status.Message = fmt.Sprintf("Restoring backup %s into pool %s", backup.Spec.BackupID, restore.TargetPool)
	})
}

// refuseExistingClaim fails the operation when the claim it would produce is
// already there and is not this operation's own.
//
// A claim this operation already created is not a collision: a restart between
// creating the claim and recording the bind lands here a second time, and the
// label is what separates that from somebody else's claim of the same name.
func (r *StorageBackupOpsReconciler) refuseExistingClaim(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps, claimName string,
) error {
	var claim corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Name: claimName, Namespace: ops.Namespace}, &claim)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if claim.Labels[RestoredByLabel] == ops.Name {
		return nil
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, ReasonClaimExists, ReasonClaimExists,
		"Claim %s already exists and was not created by this operation, so the restore was refused",
		claimName)
	return fatalf("claim %s already exists; a restore never adopts a claim, because doing so would "+
		"replace a running workload's data with the backup's", claimName)
}

// startRestore asks the control plane for the copy back.
//
// status.restoredLvolID is what makes this idempotent, and it has to be,
// because a second request would produce a second volume nothing accounts for.
// The volume is named after the operation's UID rather than after anything a
// user chose, so two restores never collide in the pool and a name says which
// operation produced it.
func (r *StorageBackupOpsReconciler) startRestore(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (bool, error) {
	if ops.Status.RestoredLvolID != "" {
		return true, nil
	}

	lvolID, err := r.API.RestoreBackup(ctx, ops.Status.ClusterID, controlplane.RestoreBackupParams{
		BackupID: ops.Status.BackupID,
		LvolName: restoredVolumeName(ops),
		Pool:     ops.Spec.Restore.TargetPool,
	})
	if err != nil {
		return false, fmt.Errorf("ask the control plane to restore backup %s: %w", ops.Status.BackupID, err)
	}

	return true, r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
		status.RestoredLvolID = lvolID
		status.Message = "The control plane accepted the restore and is filling the volume"
	})
}

// awaitVolume waits for the restored volume to become readable.
//
// The predicate is that the volume reports online, and it holds for every state
// beyond that too, which is what a coalescing stream requires: a volume observed
// past the transition still satisfies a step waiting for it. The one status that
// ends the operation is the verdict the tasks runner writes after giving up on
// the transfer.
func (r *StorageBackupOpsReconciler) awaitVolume(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (bool, error) {
	volume, err := r.API.Volume(ctx, r.restoredHandle(ops))
	if err != nil {
		return false, fmt.Errorf("read the restored volume %s: %w", ops.Status.RestoredLvolID, err)
	}

	switch volume.Status {
	case cpVolumeRestoreFailed:
		return false, fatalf("the control plane gave up transferring backup %s into volume %s",
			ops.Status.BackupID, ops.Status.RestoredLvolID)
	case cpVolumeOnline:
		return true, nil
	default:
		return false, nil
	}
}

// bind produces the claim the restore exists for.
//
// It is the last step on purpose. Restoring posts the request, AwaitingVolume
// waits for the data to be there, and only then is a claim written, so a restore
// that fails or is aborted earlier leaves no claim to leak and a claim that does
// exist has a restored volume behind it.
func (r *StorageBackupOpsReconciler) bind(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (bool, error) {
	restore := ops.Spec.Restore

	// Write-ahead. The claim carries no owner reference back to this operation,
	// so a crash between creating it and recording it would otherwise restart
	// into a claim of the right name the step cannot tell from somebody else's,
	// and the refusal above would then block the operation from finishing its
	// own work.
	if ops.Status.ClaimName == "" {
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
			status.ClaimName = restore.ClaimName
			status.PersistentVolumeName = restoredVolumeName(ops)
			status.Message = fmt.Sprintf("Binding the restored volume to claim %s", restore.ClaimName)
		}); err != nil {
			return false, err
		}
	}

	backup, err := r.backupOf(ctx, ops)
	if err != nil {
		return false, err
	}
	if err := r.ensurePersistentVolume(ctx, ops, backup); err != nil {
		return false, err
	}
	if err := r.ensureClaim(ctx, ops, backup); err != nil {
		return false, err
	}

	var claim corev1.PersistentVolumeClaim
	if err := r.Get(ctx,
		client.ObjectKey{Name: restore.ClaimName, Namespace: ops.Namespace}, &claim); err != nil {
		return false, err
	}
	return claim.Status.Phase == corev1.ClaimBound, nil
}

// ensurePersistentVolume writes the volume the claim binds to, pre-bound to that
// claim so that no other claim can take it.
//
// An existing volume of the same name that names a different claim or a
// different logical volume is not this operation's, and adopting it would hand a
// workload storage it did not ask for.
func (r *StorageBackupOpsReconciler) ensurePersistentVolume(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageBackupOps,
	backup *simplyblockv1alpha2.StorageBackup,
) error {
	name := restoredVolumeName(ops)
	handle := r.restoredHandle(ops)

	var existing corev1.PersistentVolume
	err := r.Get(ctx, client.ObjectKey{Name: name}, &existing)
	if err == nil {
		if existing.Spec.CSI == nil || existing.Spec.CSI.VolumeHandle != string(handle) {
			return fatalf("PersistentVolume %s already exists and names another volume", name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	connection, err := r.API.Connection(ctx, handle)
	if err != nil {
		return fmt.Errorf("read how to reach the restored volume %s: %w", ops.Status.RestoredLvolID, err)
	}
	attributes, err := atlaskube.CSIVolumeAttributes(handle, connection)
	if err != nil {
		return fmt.Errorf("build the volume attributes for %s: %w", ops.Status.RestoredLvolID, err)
	}

	size := restoredSize(backup)
	volume := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{RestoredByLabel: ops.Name},
		},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: atlaskube.StorageClassNameFor(ops.Namespace, ops.Spec.ClusterRef, ops.Spec.Restore.TargetPool),
			Capacity:         corev1.ResourceList{corev1.ResourceStorage: size},
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			// Delete, matching what the driver provisions: a restored volume is
			// an ordinary volume from the moment it exists, and a claim deleted
			// later should not leave storage behind that nothing references.
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			ClaimRef: &corev1.ObjectReference{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Name:       ops.Spec.Restore.ClaimName,
				Namespace:  ops.Namespace,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           utils.CSIProvisioner,
					VolumeHandle:     string(handle),
					VolumeAttributes: attributes,
					// The filesystem the source volume carried, so the restored
					// one mounts the way it was backed up instead of taking the
					// driver's default. Empty for a backup whose source volume
					// is gone, and the driver falls back in that case.
					FSType: backup.Source().FSType,
				},
			},
		},
	}
	if err := r.Create(ctx, volume); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// ensureClaim writes the claim the restore produces.
//
// It carries no owner reference to the operation, deliberately: a restore's
// product is data somebody asked to have back, and tying the two would mean
// deleting the audit record deleted the recovered volume. The label is what
// records where it came from, and it is also what the refusal above reads.
func (r *StorageBackupOpsReconciler) ensureClaim(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageBackupOps,
	backup *simplyblockv1alpha2.StorageBackup,
) error {
	restore := ops.Spec.Restore

	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Name: restore.ClaimName, Namespace: ops.Namespace}, &existing)
	if err == nil {
		if existing.Labels[RestoredByLabel] != ops.Name {
			return fatalf("claim %s exists and was not created by this operation", restore.ClaimName)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	labels := map[string]string{RestoredByLabel: ops.Name}
	for key, value := range restore.ClaimLabels {
		labels[key] = value
	}

	storageClass := atlaskube.StorageClassNameFor(ops.Namespace, ops.Spec.ClusterRef, restore.TargetPool)
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        restore.ClaimName,
			Namespace:   ops.Namespace,
			Labels:      labels,
			Annotations: restore.ClaimAnnotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr.To(storageClass),
			VolumeName:       restoredVolumeName(ops),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: restoredSize(backup)},
			},
		},
	}
	if err := r.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// backupOf reads the operation's target, which the Binding step needs for the
// size and the filesystem the copy was taken with.
func (r *StorageBackupOpsReconciler) backupOf(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (*simplyblockv1alpha2.StorageBackup, error) {
	var backup simplyblockv1alpha2.StorageBackup
	if err := r.Get(ctx,
		client.ObjectKey{Name: ops.Spec.BackupRef, Namespace: ops.Namespace}, &backup); err != nil {
		return nil, err
	}
	return &backup, nil
}

// restoredSize is how big the restored claim is asked for, which is how big the
// copy is.
//
// The operation states no size, and that is the design rather than an omission:
// a restored volume is the size of the backup, and asking for a different one is
// either a truncation or a lie. A copy whose size the control plane never
// reported falls back to a gigabyte so that the claim is writable at all, and
// the volume behind it is whatever the control plane actually restored.
func restoredSize(backup *simplyblockv1alpha2.StorageBackup) resource.Quantity {
	if size := backup.Copy().Size; size != nil && *size > 0 {
		return *resource.NewQuantity(*size, resource.BinarySI)
	}
	return *resource.NewQuantity(1<<30, resource.BinarySI)
}

// restoredVolumeName and the restored logical volume's name are both built from
// the operation's UID, so that two restores never collide and a name in the pool
// says which operation produced it.
func restoredVolumeName(ops *simplyblockv1alpha2.StorageBackupOps) string {
	return "restore-" + string(ops.UID)
}

// restoredHandle addresses the volume the control plane created for this
// operation.
func (r *StorageBackupOpsReconciler) restoredHandle(
	ops *simplyblockv1alpha2.StorageBackupOps,
) lvol.VolumeHandle {
	return lvol.NewVolumeHandle(ops.Status.ClusterID, ops.Status.PoolUUID, ops.Status.RestoredLvolID)
}
