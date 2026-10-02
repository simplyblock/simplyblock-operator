// The Remove action: draining a node before it leaves.
//
// Removing a storage node destroys it, and every logical volume whose data lives
// on it has to be somewhere else first. The drain is the part of the operation
// that makes that true, and the removal is the last step rather than the
// operation.
//
//	Validating ──► Suspending ──► MigratingVolumes ──► Verifying ──► Removing ──► AwaitingRemoval
//
// Validation runs before the suspend, and that ordering is the design. A suspended
// node accepts no new volume placement, so suspending one whose drain cannot
// complete takes capacity out of the cluster and leaves it out for as long as the
// blocker goes unnoticed. Blocking first leaves the node fully operational while
// somebody decides what to do about the pinned claim.
//
// Every terminal outcome from Suspending onward resumes the node first, which is
// the unwind the graph's abort edges are declared against. It lives in the
// reconciler rather than here because a failure of any kind owes it, not only a
// failure of a step in this file.
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
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// drainNodeLabel is what a migration this drain created carries, so that a List
// selects the fan-out of one node's drain and a watch maps a completion back to
// the operation that asked for it (§8.4).
const drainNodeLabel = "storage.simplyblock.io/drain-node"

// performRemoveStep runs one step of the drain.
func (r *StorageNodeOpsReconciler) performRemoveStep(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, machine *statemachine.Machine[step],
) (bool, error) {
	current := machine.CurrentState()
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
	case stepSuspending:
		return r.drainSuspend(ctx, ops, clusterID, nodeID)
	case stepMigratingVolumes:
		return r.drainMigrate(ctx, ops, clusterID, nodeID)
	case stepVerifying:
		return r.drainVerify(ctx, ops, clusterID, nodeID)
	case stepRemoving:
		return r.drainRemove(ctx, ops, clusterID, nodeID)
	case stepAwaitingRemoval:
		return r.drainAwaitRemoval(ctx, ops, machine, clusterID, nodeID)
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

// drainSuspend takes the node out of service so that no new volume is placed on
// it while its own are being moved. The call is skipped when the node is already
// suspended or past it, which is what makes re-entering the step harmless.
func (r *StorageNodeOpsReconciler) drainSuspend(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, clusterID, nodeID string,
) (bool, error) {
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	if reading.Status == nodeStatusSuspended {
		return true, nil
	}
	// A node already offline is past the state a suspend would produce: it is
	// serving nothing, which is what the suspend exists to achieve.
	if reading.Status == nodeStatusOffline {
		return true, nil
	}
	_, err = r.once(ctx, ops, func() error {
		if err := r.API.Suspend(ctx, clusterID, nodeID); err != nil {
			return fmt.Errorf("suspend node %s: %w", ops.Spec.NodeRef, err)
		}
		return nil
	})
	return false, err
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

	// The deletions are one claimed call. The control plane deletes
	// asynchronously, so the passes that follow still list the volumes, and
	// deleting a volume already being deleted is a refusal this step reads as
	// fatal.
	if len(census.System) > 0 {
		_, err := r.once(ctx, ops, func() error {
			for _, volume := range census.System {
				if err := r.API.DeleteVolume(ctx, clusterID, volume.PoolUUID, volume.VolumeUUID); err != nil {
					return fatalf("system volume %s could not be deleted and the node still holds it: %v",
						volume.Name, err)
				}
			}
			return nil
		})
		if err != nil {
			return false, err
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
	return len(census.System) == 0, nil
}

// drainRemove deletes the backend node. A 404 is success, since a node the control
// plane no longer knows about is a node that has been removed.
//
// The DELETE shuts the node down before it answers, and that can take longer
// than the client waits. A pass after a lost answer therefore reads the node
// first: a removal status is the removal accepted, and a node still shutting
// down is waited on, because a second DELETE against either is not one to send.
func (r *StorageNodeOpsReconciler) drainRemove(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, clusterID, nodeID string,
) (bool, error) {
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	switch reading.Status {
	case nodeStatusPendingRemoval, nodeStatusMigratingDevices, nodeStatusMigratingLvols,
		nodeStatusInRemoval, nodeStatusRemoved, nodeStatusRemovedFailed:
		// The removal was accepted, and how it ends is AwaitingRemoval's to read.
		return true, nil
	case nodeStatusInShutdown:
		return false, nil
	}

	claimed, err := r.once(ctx, ops, func() error {
		if err := r.API.RemoveNode(ctx, clusterID, nodeID); err != nil {
			// Only an answer is a refusal. A 4xx is the control plane's own
			// admission saying what the cluster can afford to lose. Retrying
			// cannot change it, so the operation fails, and the node is left
			// suspended for a Resume operation to bring back (see unwinds). A
			// timeout or a 5xx says nothing about the removal, which may well be
			// under way, so the step is retried and the next pass reads the node.
			var answer *ControlPlaneError
			if errors.As(err, &answer) && answer.Status >= 400 && answer.Status < 500 {
				return fatalf("the control plane refused to remove node %s: %v",
					ops.Spec.NodeRef, err)
			}
			return fmt.Errorf("remove node %s: %w", ops.Spec.NodeRef, err)
		}
		return nil
	})
	// A claimed call that failed is not a finished step.
	return claimed && err == nil, err
}

// drainAwaitRemoval waits for the control plane to finish the removal it
// accepted, which moves the node's devices and volumes onto its peers first and
// takes as long as that data does.
//
// removed is the outcome, and a node the control plane no longer reports reaches
// the same answer through the check every step makes first. removed_failed is the
// control plane giving up, and it is terminal on that side, so the operation
// fails rather than waiting out its deadline for a status that will not come.
//
// Anything else is the removal still running, and each pass records how far it
// has got. A change is progress and moves the deadline out (recordRemovalProgress).
func (r *StorageNodeOpsReconciler) drainAwaitRemoval(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	machine *statemachine.Machine[step],
	clusterID, nodeID string,
) (bool, error) {
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	switch reading.Status {
	case nodeStatusRemoved:
		return true, nil
	case nodeStatusRemovedFailed:
		return false, fatalf("the control plane gave up removing node %s and reports it %s",
			ops.Spec.NodeRef, reading.Status)
	default:
		return false, r.recordRemovalProgress(ctx, ops, machine, reading.Status)
	}
}

// recordRemovalProgress writes the node's status and its devices' statuses into
// status.removal and, when either changed since the last pass, extends the
// machine's deadline a whole budget out from now. The reconciler persists the
// machine's snapshot when the pass ends, as it does for a transition.
//
// The deadline is a bound on a removal that stopped moving rather than on one
// that takes long. migrating_devices is a single node status for a rebuild that
// can run for hours, and what changes during it is the devices, one at a time as
// each one's data lands on the peers. So both count, and a removal is failed only
// once a whole budget passes with neither changing.
//
// The devices are read from the StorageDevice objects this operator mirrors
// rather than asked of the control plane, so the wait costs no request beyond the
// node read every step already makes.
func (r *StorageNodeOpsReconciler) recordRemovalProgress(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	machine *statemachine.Machine[step],
	nodeStatus string,
) error {
	var devices simplyblockv1alpha2.StorageDeviceList
	if err := r.List(ctx, &devices, client.InNamespace(ops.Namespace),
		client.MatchingLabels{simplyblockv1alpha2.DeviceLabelNode: ops.Spec.NodeRef}); err != nil {
		return fmt.Errorf("list the devices of node %s: %w", ops.Spec.NodeRef, err)
	}
	var statuses map[string]string
	for i := range devices.Items {
		if statuses == nil {
			statuses = make(map[string]string, len(devices.Items))
		}
		statuses[devices.Items[i].Name] = devices.Items[i].Status.DeviceStatus
	}

	if recorded := ops.Status.Removal; recorded != nil &&
		recorded.NodeStatus == nodeStatus && maps.Equal(recorded.Devices, statuses) {
		return nil
	}

	now := metav1.Now()
	machine.Extend(awaitingRemovalDeadline)
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageNodeOpsStatus) {
		status.Removal = &simplyblockv1alpha2.RemovalStatus{
			NodeStatus:       nodeStatus,
			Devices:          statuses,
			LastProgressTime: &now,
		}
	})
}

// removalProgress is the waiting message of AwaitingRemoval: the node's status,
// and how many of its devices the removal has finished with.
func removalProgress(removal *simplyblockv1alpha2.RemovalStatus) string {
	settled := 0
	for _, status := range removal.Devices {
		if status == cpDeviceFailedAndMigrated || status == cpDeviceRemoved {
			settled++
		}
	}
	return fmt.Sprintf("the control plane reports the node %s, %d of %d %s migrated off it",
		removal.NodeStatus, settled, len(removal.Devices),
		plural(len(removal.Devices), "device", "devices"))
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

// abortMigrations is the abort path's half of the cascade: a drain called off
// mid-flight leaves nothing moving behind it.
func (r *StorageNodeOpsReconciler) abortMigrations(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, current step,
) {
	if current != stepMigratingVolumes && current != stepVerifying {
		return
	}
	if _, err := r.cascadeMigrations(ctx, ops); err != nil {
		logf.FromContext(ctx).Error(err, "the drain's migrations could not all be stopped",
			"operation", ops.Name)
	}
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
