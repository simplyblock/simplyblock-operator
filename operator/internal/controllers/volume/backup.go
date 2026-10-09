// The Backup action of PersistentVolumeOps: snapshot one volume and ask the
// storage cluster to back the snapshot up. It has its own file because it shares
// only the kind's machinery with Migrate, and needs none of Migrate's target
// node, migration slot, or sibling locks.
//
// The snapshot is named from the operation's UID, and the backup is found through
// its snapshot, so a pass that died before recording either finds it again.

package volume

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// backupSnapshotPrefix starts the snapshot's name; the rest is the operation's UID.
	backupSnapshotPrefix = "pvops-backup-"

	// The control plane's backup statuses a Backup acts on. Any other status
	// means the copy is not finished.
	cpBackupCompleted = "completed"
	cpBackupFailed    = "failed"
)

// BackupClient is the control-plane surface the Backup action needs, so a test
// can drive the graph without an HTTP server.
type BackupClient interface {
	VolumeSnapshots(ctx context.Context, clusterID, poolID, volumeID string) ([]controlplane.Snapshot, error)
	CreateSnapshot(ctx context.Context, clusterID, poolID, volumeID, name string) (string, error)
	DeleteSnapshot(ctx context.Context, clusterID, poolID, snapshotID string) error
	CreateBackup(ctx context.Context, clusterID, snapshotID string) (string, error)
	ListBackups(ctx context.Context, clusterID string) ([]controlplane.Backup, error)
	BackupByID(ctx context.Context, clusterID, backupID string) (controlplane.Backup, error)
}

// backupSnapshotName is the name the operation's snapshot is requested under.
func backupSnapshotName(ops *simplyblockv1alpha2.PersistentVolumeOps) string {
	return backupSnapshotPrefix + string(ops.UID)
}

// reconcileBackup is Reconcile for a Backup, from the point where the
// finalizer is in place and the operation is not terminal.
func (r *PersistentVolumeOpsReconciler) reconcileBackup(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (ctrl.Result, error) {
	subject, err := r.resolveVolume(ctx, ops.Spec.PersistentVolumeName)
	switch {
	case errors.Is(err, errVolumeGone):
		// Nothing is left to back up; that is a stop, not a failure.
		return r.abandon(ctx, ops, err.Error())
	case err != nil:
		var fatal *terminalStepError
		if errors.As(err, &fatal) {
			r.event(ops, corev1.EventTypeWarning, ReasonClusterUnresolvable, "%s", fatal.Error())
			return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed, fatal.Error())
		}
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, err.Error())
	}

	lock, err := r.acquireBackupLock(ctx, ops, subject.pv)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !lock.acquired {
		if lock.holder == "" {
			// Another writer moved the volume between the read and the patch.
			return ctrl.Result{RequeueAfter: opsAdvance}, nil
		}
		r.event(ops, corev1.EventTypeNormal, ReasonOperationQueued,
			"Volume %s is held by operation %s; this one is waiting", lock.volume, lock.holder)
		return ctrl.Result{RequeueAfter: opsRetry}, r.hold(ctx, ops,
			fmt.Sprintf("waiting for operation %s to release volume %s", lock.holder, lock.volume))
	}

	return r.advanceBackup(ctx, ops, subject)
}

// acquireBackupLock locks the one volume the backup reads. A migration locks its
// whole subsystem, so it still waits for a backup of any member.
func (r *PersistentVolumeOpsReconciler) acquireBackupLock(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	pv *corev1.PersistentVolume,
) (lockOutcome, error) {
	if ops.Status.Step.State != "" {
		held, err := r.holdsNamedVolume(ctx, ops, pv)
		if err != nil || held {
			return lockOutcome{acquired: held}, err
		}
	}

	// The uncached reader, so a conflict means another writer.
	var fresh corev1.PersistentVolume
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: pv.Name}, &fresh); err != nil {
		return lockOutcome{}, fmt.Errorf("read volume %s to lock it: %w", pv.Name, err)
	}

	held := fresh.Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]
	if held == ops.Name {
		return lockOutcome{acquired: true}, nil
	}
	if held != "" {
		takeable, err := r.lockIsStale(ctx, held)
		if err != nil {
			return lockOutcome{}, err
		}
		if !takeable {
			return lockOutcome{holder: held, volume: fresh.Name}, nil
		}
	}

	taken, err := r.lockVolume(ctx, ops, &fresh)
	return lockOutcome{acquired: taken}, err
}

// advanceBackup runs the Backup machine forward by at most one step.
func (r *PersistentVolumeOpsReconciler) advanceBackup(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (ctrl.Result, error) {
	machine, err := graphs(0).FromSnapshot(ctx,
		statemachine.Action(ops.Spec.Action),
		statemachine.FromKube[step](ops.Status.Step))
	if err != nil {
		return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed,
			fmt.Sprintf("the operation cannot be resumed: %v", err))
	}
	defer machine.Close()

	if ops.Status.Step.State == "" {
		return r.enterInitialStep(ctx, ops, machine)
	}

	current := machine.CurrentState()

	// An abort the graph cannot honor is noted and the step still runs, so the
	// operation reaches its end instead of waiting on a pass that never comes.
	if ops.Spec.Abort && machine.CanAbort() {
		return r.unwindBackup(ctx, ops, current)
	}

	if machine.TimeoutReached() {
		r.event(ops, corev1.EventTypeWarning, ReasonStepDeadlineExceeded,
			"Step %s outlived its deadline", current)
		stepDeadlinesExceeded.WithLabelValues(
			subject.clusterUUID, string(ops.Spec.Action), string(current)).Inc()
		return r.fail(ctx, ops, subject, fmt.Sprintf("step %s outlived its deadline", current))
	}

	done, err := r.performBackup(ctx, ops, subject, current)
	if err != nil {
		var fatal *terminalStepError
		if errors.As(err, &fatal) {
			return r.fail(ctx, ops, subject, fatal.Error())
		}
		logf.FromContext(ctx).Error(err, "the step could not be advanced",
			"operation", ops.Name, "step", current)
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, err.Error())
	}
	if !done {
		message := fmt.Sprintf("waiting on %s", current)
		if ops.Spec.Abort {
			message += "; the abort arrived after the backup was requested and cannot be honored"
		}
		return r.waitOn(machine), r.note(ctx, ops, message)
	}

	r.observeStep(ops, subject, current)

	if machine.IsTerminal() {
		message := fmt.Sprintf("Backup %s of volume %s is complete",
			ops.Status.Backup.BackupID, ops.Spec.PersistentVolumeName)
		r.event(ops, corev1.EventTypeNormal, ReasonOperationSucceeded, "%s", message)
		return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded, message)
	}

	next := firstSuccessor(machine)
	// Write-ahead: record the step before entering it.
	if err := r.recordStep(ctx, ops, next, nil); err != nil {
		return ctrl.Result{}, err
	}
	if err := machine.TransitionTo(ctx, next); err != nil {
		return ctrl.Result{}, fmt.Errorf("enter step %s: %w", next, err)
	}
	snapshot := statemachine.ToKube(machine.Snapshot())
	return ctrl.Result{RequeueAfter: opsAdvance}, r.recordStep(ctx, ops, next, snapshot.Deadline)
}

// unwindBackup honors an abort the graph allows, from a step before the backup is requested.
func (r *PersistentVolumeOpsReconciler) unwindBackup(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	current step,
) (ctrl.Result, error) {
	if err := r.discardSnapshot(ctx, ops); err != nil {
		// Not terminal: ending the operation now would leak the snapshot.
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops,
			fmt.Sprintf("the abort is waiting on the snapshot being deleted: %v", err))
	}

	r.event(ops, corev1.EventTypeNormal, ReasonOperationAborted,
		"The operation was aborted at step %s and unwound", current)
	return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted,
		fmt.Sprintf("aborted at step %s", current))
}

// performBackup advances the current step, and reports whether it finished.
func (r *PersistentVolumeOpsReconciler) performBackup(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	subject *subject,
	current step,
) (bool, error) {
	switch current {
	case stepValidating:
		return r.validateBackup(ctx, ops, subject)
	case stepSnapshotting:
		return r.takeSnapshot(ctx, ops)
	case stepBackingUp:
		return r.requestBackup(ctx, ops)
	case stepAwaitingBackup:
		return r.awaitBackup(ctx, ops)
	default:
		return false, fatalf("step %s has no implementation for a Backup", current)
	}
}

// validateBackup refuses a cluster with no backup store and a consistency-group
// member, before anything is created, and records the UUIDs later steps use.
// A member is refused because a group is snapshotted as one unit.
func (r *PersistentVolumeOpsReconciler) validateBackup(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (bool, error) {
	if ops.Status.Backup != nil && ops.Status.Backup.SnapshotName != "" {
		return true, nil
	}

	if subject.cluster.Spec.Backup == nil {
		r.event(ops, corev1.EventTypeWarning, ReasonBackupStoreMissing,
			"Cluster %s has no backup store", subject.cluster.Name)
		return false, fatalf("cluster %s has no backup store configured, so there is nowhere to "+
			"back volume %s up to; set spec.backup on the StorageCluster", subject.cluster.Name,
			ops.Spec.PersistentVolumeName)
	}

	volume, err := r.API.Volume(ctx, lvol.VolumeHandle(subject.pv.Spec.CSI.VolumeHandle))
	if err != nil {
		return false, fmt.Errorf("read volume %s from the control plane: %w", ops.Spec.PersistentVolumeName, err)
	}
	if volume.ConsistencyGroup != "" {
		r.event(ops, corev1.EventTypeWarning, ReasonConsistencyGroupMember,
			"Volume %s belongs to consistency group %s", ops.Spec.PersistentVolumeName, volume.ConsistencyGroup)
		return false, fatalf("volume %s belongs to consistency group %s, which is snapshotted as one "+
			"unit; a backup of one member alone would not agree with the others",
			ops.Spec.PersistentVolumeName, volume.ConsistencyGroup)
	}

	poolUUID, err := r.poolUUIDOf(ctx, subject)
	if err != nil {
		return false, err
	}

	name := backupSnapshotName(ops)
	return true, r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Backup = &simplyblockv1alpha2.VolumeBackupStatus{
			ClusterUUID:  subject.clusterUUID,
			PoolUUID:     poolUUID,
			VolumeUUID:   subject.handle.VolumeID,
			SnapshotName: name,
		}
		status.Message = fmt.Sprintf("Backing up volume %s", ops.Spec.PersistentVolumeName)
	})
}

// poolUUIDOf is the volume's pool as a UUID. A handle written before the v2 API
// carries the pool's name instead.
func (r *PersistentVolumeOpsReconciler) poolUUIDOf(ctx context.Context, subject *subject) (string, error) {
	if lvol.IsCanonicalUUID(subject.handle.PoolRef) {
		return subject.handle.PoolRef, nil
	}
	uuid, err := utils.ResolvePoolUUID(ctx, r.Client,
		subject.cluster.Namespace, subject.cluster.Name, subject.handle.PoolRef)
	if err != nil {
		return "", fatalf("the volume's pool %q is not a pool of cluster %s: %v",
			subject.handle.PoolRef, subject.cluster.Name, err)
	}
	return uuid, nil
}

// takeSnapshot snapshots the volume once. It looks for the snapshot by name
// first, because a pass that died after the request left one nothing records.
func (r *PersistentVolumeOpsReconciler) takeSnapshot(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (bool, error) {
	backup := ops.Status.Backup
	if backup.SnapshotID != "" {
		return true, nil
	}

	adopted, err := r.snapshotByName(ctx, backup)
	if err != nil {
		return false, err
	}
	if adopted != "" {
		return true, r.recordSnapshot(ctx, ops, adopted,
			"A snapshot this operation had already taken was reused")
	}

	var snapshotID string
	claimed, err := r.once(ctx, ops, func() error {
		id, err := r.Backups.CreateSnapshot(ctx,
			backup.ClusterUUID, backup.PoolUUID, backup.VolumeUUID, backup.SnapshotName)
		if errors.Is(err, errs.ErrAlreadyExists) {
			// Taken between the lookup and the request; the next pass finds it.
			return nil
		}
		if err != nil {
			return fmt.Errorf("snapshot volume %s: %w", backup.VolumeUUID, err)
		}
		snapshotID = id
		return nil
	})
	if err != nil || !claimed || snapshotID == "" {
		return false, err
	}

	r.event(ops, corev1.EventTypeNormal, ReasonSnapshotTaken,
		"Snapshot %s of volume %s was taken", snapshotID, ops.Spec.PersistentVolumeName)
	return true, r.recordSnapshot(ctx, ops, snapshotID, "The snapshot was taken")
}

// snapshotByName finds the snapshot an earlier attempt took, and returns the
// empty string when there is none.
func (r *PersistentVolumeOpsReconciler) snapshotByName(
	ctx context.Context, backup *simplyblockv1alpha2.VolumeBackupStatus,
) (string, error) {
	snapshots, err := r.Backups.VolumeSnapshots(ctx, backup.ClusterUUID, backup.PoolUUID, backup.VolumeUUID)
	if err != nil {
		return "", fmt.Errorf("look for a snapshot already taken for this operation: %w", err)
	}
	for _, snapshot := range snapshots {
		if snapshot.Name == backup.SnapshotName {
			return snapshot.ID, nil
		}
	}
	return "", nil
}

func (r *PersistentVolumeOpsReconciler) recordSnapshot(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, snapshotID, message string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Backup.SnapshotID = snapshotID
		status.Message = message
	})
}

// requestBackup asks the control plane to back the snapshot up, once. An earlier
// attempt's backup is found through its snapshot. A 400 is a verdict, not a retry.
func (r *PersistentVolumeOpsReconciler) requestBackup(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (bool, error) {
	backup := ops.Status.Backup
	if backup.BackupID != "" {
		return true, nil
	}

	backups, err := r.Backups.ListBackups(ctx, backup.ClusterUUID)
	if err != nil {
		return false, fmt.Errorf("look for a backup already requested for this operation: %w", err)
	}
	for _, existing := range backups {
		if existing.SnapshotID == backup.SnapshotID {
			return true, r.recordBackup(ctx, ops, existing.ID,
				"A backup this operation had already requested was reused")
		}
	}

	var backupID string
	claimed, err := r.once(ctx, ops, func() error {
		id, err := r.Backups.CreateBackup(ctx, backup.ClusterUUID, backup.SnapshotID)
		if err != nil {
			var refused *controlplane.StatusError
			if errors.As(err, &refused) && refused.StatusCode == http.StatusBadRequest {
				return fatalf("the storage cluster refused the backup: %s", refused.Body)
			}
			return fmt.Errorf("ask the control plane to back up snapshot %s: %w", backup.SnapshotID, err)
		}
		backupID = id
		return nil
	})
	if err != nil || !claimed || backupID == "" {
		return false, err
	}

	r.event(ops, corev1.EventTypeNormal, ReasonBackupRequested,
		"The control plane accepted backup %s", backupID)
	return true, r.recordBackup(ctx, ops, backupID, "The control plane accepted the backup and is copying it")
}

func (r *PersistentVolumeOpsReconciler) recordBackup(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, backupID, message string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Backup.BackupID = backupID
		status.Message = message
	})
}

// awaitBackup waits for the control plane to report the backup complete. The
// request returns once the backup is accepted, and the transfer runs afterward.
func (r *PersistentVolumeOpsReconciler) awaitBackup(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (bool, error) {
	backup := ops.Status.Backup
	reported, err := r.Backups.BackupByID(ctx, backup.ClusterUUID, backup.BackupID)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backup.BackupID, err)
	}

	switch reported.Status {
	case cpBackupFailed:
		return false, fatalf("the control plane gave up on backup %s of volume %s",
			backup.BackupID, ops.Spec.PersistentVolumeName)
	case cpBackupCompleted:
		return true, nil
	default:
		return false, nil
	}
}

// discardSnapshot deletes the operation's snapshot when no backup was requested
// from it. The lookup by name covers a snapshot taken but never recorded.
func (r *PersistentVolumeOpsReconciler) discardSnapshot(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) error {
	backup := ops.Status.Backup
	if backup == nil || backup.BackupID != "" || (backup.SnapshotID == "" && backup.SnapshotName == "") {
		return nil
	}

	snapshotID := backup.SnapshotID
	if snapshotID == "" {
		found, err := r.snapshotByName(ctx, backup)
		if err != nil {
			return err
		}
		if found == "" {
			return nil // nothing was ever taken
		}
		snapshotID = found
	}

	if err := r.Backups.DeleteSnapshot(ctx, backup.ClusterUUID, backup.PoolUUID, snapshotID); err != nil {
		return fmt.Errorf("delete snapshot %s: %w", snapshotID, err)
	}
	return nil
}
