// The Remove action: draining a node before it leaves.
//
// Removing a storage node destroys it, and every logical volume whose data lives
// on it has to be somewhere else first. The drain is the part of the operation
// that makes that true, and the removal is the last step rather than the
// operation.
//
//	Validating ──► PreparingRemoval ──► MigratingDevices ──► MigratingVolumes ──► Verifying ──► Removing
//
// The control plane splits a removal into three steps, and this graph follows
// them. prepare-removal (PreparingRemoval, then MigratingDevices while its device
// rebuild runs) admits the node, marks it pending_removal, shuts it down, and
// rebuilds its devices onto its peers. verify-drained closes Verifying once the
// volumes have moved. The node DELETE is Removing.
//
// Validation runs before the trigger, and that ordering is the design. Nothing
// moves a node out of pending_removal except the removal itself, so triggering
// one whose drain cannot complete takes the node out of the cluster for good.
// Blocking first leaves the node fully operational while somebody decides what
// to do about the pinned claim.
//
// For the same reason no failure from PreparingRemoval onward resumes the node.
// The operation fails with the node left in the removal, and a later Remove
// drives it the rest of the way: every step here finds the node where an earlier
// attempt left it and carries on from there.
//
// The migration is fanned out as one move per volume through the mover of
// internal/volumemigration, which raises whichever kind the deployment runs: the
// cluster-scoped PersistentVolumeOps of §8.4, naming this operation in
// spec.creatorRef and carrying the managed-by label, or the registered
// VolumeMigration with a controller reference where that kind is still the one
// in use. Nothing in this file knows which, which is what the mover exists for.
//
// design-storagenode.md §8 is the specification.

package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// drainNodeLabel is what a migration this drain created carries, so that a List
// selects the fan-out of one node's drain and a watch maps a completion back to
// the operation that asked for it (§8.4).
const drainNodeLabel = "storage.simplyblock.io/drain-node"

// performRemoveStep runs one step of the drain.
func (r *StorageNodeOpsReconciler) performRemoveStep(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, current step,
) (bool, error) {
	clusterID, nodeID, err := r.target(ctx, ops)
	if err != nil {
		return false, err
	}

	// A node the control plane does not have is what this operation was for, so
	// every step of it is already done. The last step reads a 404 as success for
	// the same reason; the earlier ones did not, and a removal that found its
	// node missing at Suspending reported the 404 as a step that could not be
	// advanced and retried it for as long as the operator ran.
	//
	// The node can be gone before the step that would have removed it in more
	// than one way: an earlier attempt got that far and lost its response, the
	// add that was being undone never registered it, or somebody else removed it.
	// None of them is a failure of this operation.
	if gone, err := r.nodeGone(ctx, clusterID, nodeID); err != nil {
		return false, err
	} else if gone {
		return true, nil
	}

	switch current {
	case stepValidating:
		return r.drainValidate(ctx, ops, clusterID, nodeID)
	case stepPreparingRemoval:
		return r.drainPrepare(ctx, ops, clusterID, nodeID)
	case stepMigratingDevices:
		return r.drainMigrateDevices(ctx, ops, clusterID, nodeID)
	case stepMigratingVolumes:
		return r.drainMigrate(ctx, ops, clusterID, nodeID)
	case stepVerifying:
		return r.drainVerify(ctx, ops, clusterID, nodeID)
	case stepRemoving:
		return r.drainRemove(ctx, ops, clusterID, nodeID)
	default:
		return false, fatalf("step %s does not belong to the Remove action", current)
	}
}

// nodeGone reports whether the control plane has forgotten the node.
//
// It asks the control plane rather than the stream's cache, because the cache
// not having a node and the control plane not having one are different facts and
// only the second one ends a removal. A cache that has not synced reports every
// node missing, and treating that as "already removed" would finish a drain that
// never moved a volume.
func (r *StorageNodeOpsReconciler) nodeGone(
	ctx context.Context, clusterID, nodeID string,
) (bool, error) {
	_, found, err := r.API.StorageNode(ctx, clusterID, nodeID)
	if err != nil {
		return false, fmt.Errorf("read node %s: %w", nodeID, err)
	}
	return !found, nil
}

// drainValidate classifies the node's volumes and refuses to go on while any of
// them is pinned or unmanaged. It performs no side effect at all, which is what
// makes an abort here an Aborted directly rather than an unwind.
//
// It also writes status.drain.volumesTotal, once, at the end. That is the number
// every later step's progress is reported against, and fixing it here is what
// stops a pending count having to be kept in step with a total.
func (r *StorageNodeOpsReconciler) drainValidate(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, clusterID, nodeID string,
) (bool, error) {
	census, err := r.classify(ctx, ops, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	if census.Incomplete {
		// A claim that could not be read put a volume in the unmanaged bucket for
		// safety. Blocking on that would report a drain blocked by a volume that
		// is in fact accounted for, so the pass is retried instead.
		return false, fmt.Errorf(
			"a volume's claim could not be read; the classification is retried")
	}

	cluster := r.clusterLabel(ctx, ops)
	drainBlockedVolumesCount.WithLabelValues(cluster, blockedPinned).
		Set(float64(len(census.Pinned)))
	drainBlockedVolumesCount.WithLabelValues(cluster, blockedUnmanaged).
		Set(float64(len(census.Unmanaged)))

	if len(census.Pinned) > 0 {
		return false, blockedf(DrainBlocked,
			"blocked: %d pinned %s, remove the %s annotation from %s",
			len(census.Pinned), plural(len(census.Pinned), "volume", "volumes"),
			kube.AnnoSelectedStorageNode, strings.Join(census.Pinned, ", "))
	}
	if len(census.Unmanaged) > 0 {
		return false, blockedf(DrainBlocked,
			"blocked: %d unmanaged %s no PersistentVolume accounts for, remove %s by hand",
			len(census.Unmanaged), plural(len(census.Unmanaged), "volume", "volumes"),
			strings.Join(census.Unmanaged, ", "))
	}

	total := int32(len(census.Managed))
	err = r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageNodeOpsStatus) {
		status.Drain = &simplyblockv1alpha2.DrainStatus{VolumesTotal: total, VolumesMigrated: 0}
	})
	return err == nil, err
}

// drainPrepare triggers the removal on the control plane. It is skipped for a
// node the removal already has, which is what makes re-entering the step after a
// lost response harmless, and the step finishes when the control plane reports
// the node as the removal's rather than on the call.
//
// It is sent for a node that is already down too: the admission, the
// pending_removal stamp, and the device rebuild are all still needed.
func (r *StorageNodeOpsReconciler) drainPrepare(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, clusterID, nodeID string,
) (bool, error) {
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	if inRemoval(reading.Status) {
		return true, nil
	}
	// A shutdown somebody else started is waited for. Stamping pending_removal
	// under it loses the stamp to the shutdown's own final write, which is
	// offline, and with it the status that sends the rebuild to a peer.
	if reading.Status == nodeStatusInShutdown {
		return false, blockedf(RemovalWaiting,
			"node %s is shutting down; the removal is triggered once it has stopped",
			ops.Spec.NodeRef)
	}
	if err := r.API.PrepareRemoval(ctx, clusterID, nodeID); err != nil {
		// A 4xx is the admission refusing, which changed nothing on the node and
		// which retrying cannot change. A 503 is a node mid-transition and
		// anything else is transport, and both are asked again.
		var refusal *ControlPlaneError
		if errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 {
			return false, fatalf("the control plane refused to remove node %s: %v",
				ops.Spec.NodeRef, err)
		}
		return false, fmt.Errorf("trigger the removal of node %s: %w", ops.Spec.NodeRef, err)
	}
	return false, nil
}

// inRemoval reports a status the removal has put the node in. Nothing else moves
// a node into any of them.
func inRemoval(status string) bool {
	switch status {
	case nodeStatusPendingRemoval, nodeStatusMigratingDevices, nodeStatusMigratingLvols,
		nodeStatusInRemoval, nodeStatusRemovedFailed:
		return true
	default:
		return false
	}
}

// drainMigrateDevices waits for the device rebuild prepare-removal started, and
// completes when the control plane says it is done: done is the one field the
// progress carries that a caller must honor, and the counts are for reporting.
//
// Every pass that finds it unfinished re-sends prepare-removal. On a node already
// in the removal that only makes sure the rebuild is running, which is a no-op
// while it runs and what restarts one a control-plane restart lost.
//
// A rebuild that gave up holds rather than failing. Failing would undo nothing,
// and moving volumes on top of a device that was never rebuilt widens the
// exposure the removal exists to end.
func (r *StorageNodeOpsReconciler) drainMigrateDevices(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, clusterID, nodeID string,
) (bool, error) {
	progress, err := r.API.RemovalProgress(ctx, clusterID, nodeID)
	if err != nil {
		return false, fmt.Errorf("read the device rebuild of node %s: %w", ops.Spec.NodeRef, err)
	}
	if progress.Done {
		return true, nil
	}
	if err := r.API.PrepareRemoval(ctx, clusterID, nodeID); err != nil {
		return false, fmt.Errorf("keep the device rebuild of node %s running: %w",
			ops.Spec.NodeRef, err)
	}
	if progress.Failed > 0 {
		return false, blockedf(DeviceRebuildFailed,
			"the device rebuild of node %s gave up and is being restarted: %s",
			ops.Spec.NodeRef, progress.Message)
	}
	return false, nil
}

// drainMigrate moves every PV-managed volume to a peer, one migration object per
// volume, and completes when all of them have.
//
// Completed objects are deleted immediately, which is what keeps a hundred-volume
// drain from leaving a hundred objects behind. status.drain is the progress record
// rather than the objects' presence, which is why the counter is written before
// the delete rather than derived from a List (§8.4).
func (r *StorageNodeOpsReconciler) drainMigrate(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, clusterID, nodeID string,
) (bool, error) {
	log := logf.FromContext(ctx)

	migrations, err := r.migrationsOf(ctx, ops, nodeID)
	if err != nil {
		return false, err
	}

	// A failed migration is deleted and replaced against a fresh target, rather
	// than failing the drain: the volume is still on the node, and another peer
	// may take it.
	if retried, err := r.retryFailedMigrations(ctx, ops, migrations); err != nil {
		return false, err
	} else if retried > 0 {
		return false, nil
	}

	census, err := r.classify(ctx, ops, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	if census.Incomplete {
		return false, fmt.Errorf(
			"a volume's claim could not be read; the migration fan-out is retried")
	}

	// Every movable volume that has no migration gets one. That covers the first
	// pass, an object deleted out of band, and a volume that arrived on the node
	// after the count was taken.
	existing := make(map[string]struct{}, len(migrations))
	for i := range migrations {
		existing[migrations[i].Name] = struct{}{}
	}
	var missing []managedVolume
	for _, volume := range census.Managed {
		if _, ok := existing[migrationName(nodeID, volume.PVName)]; !ok {
			missing = append(missing, volume)
		}
	}

	if len(missing) > 0 {
		targets, err := r.peerTargets(ctx, clusterID, nodeID, missing)
		if err != nil {
			return false, err
		}
		for _, volume := range missing {
			if err := r.createMigration(ctx, ops, nodeID, volume, targets[volume.PVName]); err != nil {
				log.Error(err, "a volume's migration could not be created",
					"volume", volume.VolumeUUID, "persistentVolume", volume.PVName)
			}
		}
		return false, nil
	}

	// No movable volume is left and no migration is outstanding: everything that
	// was going to move has moved. The census is the authority rather than the
	// counter, because the counter is a record of what this operation did and the
	// census is what is actually on the node.
	if len(census.Managed) == 0 && len(migrations) == 0 {
		r.emit(ctx, ops, corev1.EventTypeNormal, DrainCompleted,
			"Every volume has been migrated off the node")
		return true, nil
	}

	completed, running := 0, 0
	for i := range migrations {
		if migrations[i].Phase == vmigration.MoveSucceeded {
			completed++
		} else {
			running++
		}
	}

	if running > 0 {
		return false, r.recordDrainProgress(ctx, ops, completed)
	}

	// Every migration finished. The counter is written before the objects go, so
	// a crash between the two leaves the progress recorded rather than lost.
	if err := r.recordDrainProgress(ctx, ops, completed); err != nil {
		return false, err
	}
	drainVolumesMigratedTotal.WithLabelValues(r.clusterLabel(ctx, ops)).Add(float64(completed))
	for i := range migrations {
		if err := r.mover().Delete(ctx, migrations[i]); err != nil {
			log.Error(err, "a completed migration could not be deleted",
				"migration", migrations[i].Name)
		}
	}
	return false, nil
}

// recordDrainProgress writes how many volumes have moved. The total stays as
// Validating fixed it: a drain that finds one more volume than it counted moves it
// too, and reporting eleven of ten is more honest than silently raising the total.
func (r *StorageNodeOpsReconciler) recordDrainProgress(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, completed int,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageNodeOpsStatus) {
		if status.Drain == nil {
			status.Drain = &simplyblockv1alpha2.DrainStatus{}
		}
		status.Drain.VolumesMigrated = int32(completed)
	})
}

// drainVerify deletes the system volumes the migration skipped and completes when
// the node reports no volumes at all.
//
// They are deleted rather than migrated because they are per-node benchmark
// artifacts: moving one to a peer would produce a benchmark volume measuring the
// wrong node. A delete the control plane refuses for a reason other than "already
// gone" fails the operation, because a volume that cannot be deleted and cannot be
// migrated is a volume the removal would destroy (§8.2).
func (r *StorageNodeOpsReconciler) drainVerify(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, clusterID, nodeID string,
) (bool, error) {
	census, err := r.classify(ctx, ops, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	if census.Incomplete {
		return false, fmt.Errorf(
			"a volume's claim could not be read; the verification is retried")
	}

	for _, volume := range census.System {
		if err := r.API.DeleteVolume(ctx, clusterID, volume.PoolUUID, volume.VolumeUUID); err != nil {
			return false, fatalf("system volume %s could not be deleted and the node still holds it: %v",
				volume.Name, err)
		}
	}

	remaining := len(census.Managed) + len(census.Pinned) + len(census.Unmanaged)
	if remaining > 0 {
		return false, blockedf(DrainBlocked,
			"%d %s still on the node after the migration; the removal is held",
			remaining, plural(remaining, "volume", "volumes"))
	}
	// The system volumes were deleted on this pass and the control plane's
	// deletion is asynchronous, so the node is not empty until a later pass says
	// so. Reporting unfinished is what makes the next pass re-read rather than
	// trust this one's arithmetic.
	if len(census.System) > 0 {
		return false, nil
	}

	// The census counts volumes and nothing else. The control plane's own check
	// counts snapshots too, and the DELETE that follows would destroy whatever
	// it still sees.
	verification, err := r.API.VerifyDrained(ctx, clusterID, nodeID)
	if err != nil {
		return false, fmt.Errorf("verify that node %s is drained: %w", ops.Spec.NodeRef, err)
	}
	if !verification.Drained {
		left := append(slices.Clone(verification.Lvols), verification.Snapshots...)
		return false, blockedf(DrainBlocked,
			"the control plane still sees %d %s and %d %s on the node (%s); the removal is held",
			len(verification.Lvols), plural(len(verification.Lvols), "volume", "volumes"),
			len(verification.Snapshots), plural(len(verification.Snapshots), "snapshot", "snapshots"),
			strings.Join(left, ", "))
	}
	return true, nil
}

// drainRemove deletes the backend node. A 404 is success, since a node the control
// plane no longer knows about is a node that has been removed.
func (r *StorageNodeOpsReconciler) drainRemove(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, clusterID, nodeID string,
) (bool, error) {
	if err := r.API.RemoveNode(ctx, clusterID, nodeID); err != nil {
		// The control plane refused the removal, which is its answer about what
		// the cluster can afford to lose. Retrying cannot change it, so the
		// operation fails and the node stays in the removal for a later Remove
		// (§8.3).
		return false, fatalf("the control plane refused to remove node %s: %v",
			ops.Spec.NodeRef, err)
	}
	return true, nil
}

// migrationsOf lists the fan-out of this drain.
func (r *StorageNodeOpsReconciler) migrationsOf(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, nodeID string,
) ([]vmigration.Move, error) {
	moves, err := r.mover().List(ctx, ops.Namespace, map[string]string{drainNodeLabel: nodeID})
	if err != nil {
		return nil, fmt.Errorf("list this drain's volume migrations: %w", err)
	}
	return moves, nil
}

// createMigration raises one volume's move, recording the operation that asked
// for it so that deleting the drain cascades to its fan-out.
//
// Which kind carries the move is the deployment's, and how the creator is
// recorded follows from it: a namespaced move takes a controller reference, and
// a cluster-scoped one cannot have one at all, so it names its creator in the
// spec and the cascade becomes this controller's own (design-storagenode.md
// §8.4, design-persistentvolumeops.md §11.1).
func (r *StorageNodeOpsReconciler) createMigration(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	nodeID string,
	volume managedVolume,
	target string,
) error {
	return r.mover().Start(ctx, vmigration.MoveRequest{
		Name:           migrationName(nodeID, volume.PVName),
		Namespace:      ops.Namespace,
		PVName:         volume.PVName,
		TargetNodeUUID: target,
		Labels:         map[string]string{drainNodeLabel: nodeID},
		Owner:          ops,
		OwnerKind:      "StorageNodeOps",
		Scheme:         r.Scheme,
	})
}

// mover is the fan-out's channel, defaulted so a reconciler built without one
// raises the kind this API group documents.
func (r *StorageNodeOpsReconciler) mover() vmigration.Mover {
	if r.Mover != nil {
		return r.Mover
	}
	return vmigration.NewMover(r.Client, r.Scheme, false)
}

// retryFailedMigrations deletes every migration that failed and reports how many,
// so the caller's next pass recreates them against a fresh round-robin target.
//
// Retrying rather than failing the drain is the design: the volume is still on the
// node, and the peer it could not reach is not the only peer.
func (r *StorageNodeOpsReconciler) retryFailedMigrations(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	migrations []vmigration.Move,
) (int, error) {
	retried := 0
	for i := range migrations {
		migration := migrations[i]
		if migration.Phase != vmigration.MoveFailed {
			continue
		}
		r.emit(ctx, ops, corev1.EventTypeWarning, MigrationRetried, fmt.Sprintf(
			"The migration of %s failed and is being retried against another peer: %s",
			migration.PVName, migration.Message))
		if err := r.mover().Delete(ctx, migration); err != nil {
			return retried, fmt.Errorf("delete the failed migration of %s: %w",
				migration.PVName, err)
		}
		retried++
	}
	return retried, nil
}

// cascadeMigrations aborts the fan-out of a drain that is being deleted and
// reports whether any of it is still running.
//
// Aborting before deleting is what makes the cascade's own deletes admissible and
// what stops a deleted operation leaving migrations running behind it (§8.4).
func (r *StorageNodeOpsReconciler) cascadeMigrations(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) (bool, error) {
	node, err := r.node(ctx, ops)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	migrations, err := r.migrationsOf(ctx, ops, node.Status.UUID)
	if err != nil {
		return false, err
	}
	pending := false
	for i := range migrations {
		migration := migrations[i]
		if !migration.Phase.Terminal() {
			pending = true
			continue
		}
		if err := r.mover().Delete(ctx, migration); err != nil {
			return true, fmt.Errorf("delete the migration of %s: %w", migration.PVName, err)
		}
	}
	return pending, nil
}

// migrationFormula names one volume's move. It is derived rather than generated
// so that the fan-out is idempotent: a pass that runs again finds the object it
// made rather than making a second.
//
// The formula is atlas-lib's rather than a local truncation, which is what keeps
// two long PersistentVolume names that share a prefix from colliding on one
// object name: the digest is part of the formula rather than something a caller
// remembers to append.
var migrationFormula = kube.Formula{Prefix: "drain-"}

func migrationName(nodeID, pvName string) string {
	return migrationFormula.Derive(nodeID, pvName).Value
}

// plural picks the noun for a count, so a message reads 1 pinned volume rather
// than 1 pinned volume(s).
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
