// The Remove action: draining a node before it leaves.
//
// Removing a storage node destroys it, and every logical volume whose data lives
// on it has to be somewhere else first. The drain is the part of the operation
// that makes that true, and the removal is the last step rather than the
// operation.
//
//	Validating ──► ShuttingDown ──► MigratingDevices ──► MigratingVolumes
//	           ──► Verifying ──► Removing ──► AwaitingRemoval
//
// The control plane carries out three of these: prepare-removal (ShuttingDown and
// MigratingDevices) admits the node, shuts it down, and rebuilds its devices onto
// the peers; the operator then moves the volumes; verify-drained closes the volume
// half, and the node DELETE (Removing and AwaitingRemoval) takes the node apart.
//
// Validation runs before prepare-removal, and that ordering is the design. From
// prepare-removal on there is no way back, so a drain that cannot complete is held
// while the node is still fully operational and somebody decides what to do about
// the pinned claim. Nothing in the action resumes the node: Validating changes
// nothing, and every later step leaves the node to the control plane.
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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// drainNodeLabel is what a migration this drain created carries, so that a List
// selects the fan-out of one node's drain and a watch maps a completion back to
// the operation that asked for it (§8.4).
const drainNodeLabel = "storage.simplyblock.io/drain-node"

// maxPrepareAttempts is how many times MigratingDevices sends prepare-removal
// again for a node stuck in pending_removal before the operation fails.
const maxPrepareAttempts = 3

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
	// node missing at an earlier step reported the 404 as a step that could not be
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
	case stepShuttingDown:
		return r.drainShutDown(ctx, ops, clusterID, nodeID)
	case stepMigratingDevices:
		return r.drainMigrateDevices(ctx, ops, machine, clusterID, nodeID)
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

// drainShutDown takes the node down before anything is moved off it, through the
// node's own shutdown, and finishes once the control plane has accepted it.
//
// The shutdown is the node action rather than the one prepare-removal runs. The
// node action answers at once and shuts the node down in the background, while
// prepare-removal ran it inside its own request, blocked for longer than the
// client waits, and on a slow SPDK kill reported the shutdown failed although
// SPDK was gone, leaving the node pending_removal with no rebuild. With the node
// already down, prepare-removal has no shutdown of its own to run.
//
// Only a node still running is shut down: one already offline is not shut down
// again, one in_shutdown is under a shutdown already, and one in a removal status
// is past this step. A 409 is a shutdown the control plane cannot run yet, and
// the step holds on it (shutdownDeferral). Any other 4xx is a refusal that
// changed nothing, with the node still serving, so the operation fails. A
// timeout or a 5xx is retried, and the next pass reads the node.
//
// The shutdown is graceful rather than forced, because the conditions a 409
// names are the ones that keep two nodes from being down at once: a peer
// restarting or shutting down, a migration or restart task, a live restart
// claim. Forcing past them would take this node down beside another one before
// the removal's admission has judged what the cluster can afford to lose.
func (r *StorageNodeOpsReconciler) drainShutDown(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, clusterID, nodeID string,
) (bool, error) {
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	if reading.Status != nodeStatusOnline && reading.Status != nodeStatusSuspended {
		return true, nil
	}

	claimed, err := r.once(ctx, ops, func() error {
		if err := r.API.ShutdownNode(ctx, clusterID, nodeID); err != nil {
			if deferral := shutdownDeferral(ops, err); deferral != nil {
				return deferral
			}
			if refused(err) {
				return fatalf("the control plane refused to shut node %s down for its removal: %v",
					ops.Spec.NodeRef, err)
			}
			return fmt.Errorf("shut node %s down: %w", ops.Spec.NodeRef, err)
		}
		return nil
	})
	return claimed && err == nil, err
}

// drainMigrateDevices sends prepare-removal once the node is down, waits for the
// control plane to rebuild the node's devices onto its peers, and finishes when
// the control plane reports the rebuild done.
//
// A node still online, suspended, or in_shutdown is waited on with nothing sent,
// because the shutdown ShuttingDown asked for has not landed. One still running a
// whole shutdown budget after that is one whose shutdown the control plane
// dropped, and the operation fails with the node untouched.
//
// For an offline node, prepare-removal is the control plane's admission: a 4xx
// is a refusal, and the operation fails with the node left offline, because
// nothing in a removal brings a node back. From then on prepare-removal is sent
// again on every pass, which the control plane treats as a no-op while the
// rebuild runs and as a restart of it when it stopped, as it does across a
// control-plane restart. A rebuild the control plane gave up on fails the
// operation, and each change in the node's or its devices' statuses moves the
// deadline out (recordRemovalProgress).
//
// A node still pending_removal is one whose shutdown has not finished, or
// failed: prepare-removal marks the node before it shuts it down and leaves it
// there when the shutdown fails. Nothing is sent while the shutdown may still be
// running. It is sent again (retryPrepare) once the control plane reports the
// step failed, or once nothing has moved for longer than a shutdown takes.
func (r *StorageNodeOpsReconciler) drainMigrateDevices(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	machine *statemachine.Machine[step],
	clusterID, nodeID string,
) (bool, error) {
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	progress, err := r.API.RemovalProgress(ctx, clusterID, nodeID)
	if err != nil {
		return false, fmt.Errorf("read the device rebuild of node %s: %w", ops.Spec.NodeRef, err)
	}

	switch reading.Status {
	case nodeStatusOnline, nodeStatusSuspended:
		if recorded := ops.Status.Removal; recorded != nil && recorded.LastProgressTime != nil &&
			recorded.NodeStatus == reading.Status &&
			time.Since(recorded.LastProgressTime.Time) >= shuttingDownDeadline {
			return false, fatalf("node %s is still %s %s after its shutdown was accepted; the "+
				"control plane dropped the shutdown, and nothing has been changed",
				ops.Spec.NodeRef, reading.Status, shuttingDownDeadline)
		}
		return false, r.recordRemovalProgress(ctx, ops, machine, reading.Status)
	case nodeStatusInShutdown:
		return false, r.recordRemovalProgress(ctx, ops, machine, reading.Status)
	case nodeStatusOffline:
		if _, err := r.once(ctx, ops, func() error {
			if err := r.API.PrepareRemoval(ctx, clusterID, nodeID); err != nil {
				if deferral := r.removalDeferral(ops, clusterID, err); deferral != nil {
					return deferral
				}
				if refused(err) {
					return fatalf("the control plane refused to remove node %s: %v; the node is "+
						"left offline, and a Restart operation brings it back", ops.Spec.NodeRef, err)
				}
				return fmt.Errorf("prepare the removal of node %s: %w", ops.Spec.NodeRef, err)
			}
			return nil
		}); err != nil {
			return false, err
		}
		return false, r.recordRemovalProgress(ctx, ops, machine, reading.Status)
	case nodeStatusPendingRemoval:
		if retry, err := r.retryPrepare(ctx, ops, clusterID, nodeID, progress); err != nil || retry {
			return false, err
		}
		return false, r.recordRemovalProgress(ctx, ops, machine, reading.Status)
	}

	// Under a claim, so it is sent at most once a lease rather than by every pass
	// that read the step from a cache.
	if _, err := r.once(ctx, ops, func() error {
		if err := r.API.PrepareRemoval(ctx, clusterID, nodeID); err != nil {
			return fmt.Errorf("keep the device rebuild of node %s running: %w",
				ops.Spec.NodeRef, err)
		}
		return nil
	}); err != nil {
		return false, err
	}
	if progress.Failed > 0 {
		return false, fatalf("the control plane gave up rebuilding the devices of node %s: %s",
			ops.Spec.NodeRef, progress.Message)
	}
	if progress.Done {
		return true, nil
	}
	return false, r.recordRemovalProgress(ctx, ops, machine, reading.Status)
}

// prepareRetryAfterFailure is how long a prepare-removal sent again has to
// answer before a failure the control plane still reports is taken as its own.
const prepareRetryAfterFailure = time.Minute

// retryPrepare sends prepare-removal again for a node stuck in pending_removal,
// and reports whether it did.
//
// A failure the control plane reports is retried a minute after the last
// attempt, which gives a retry time to clear the failure it is retrying. A node
// whose status has not moved for longer than a shutdown takes, with no failure
// reported, is retried too: the control plane can fail the shutdown without
// recording it. The first case knows the shutdown has finished; the second waits
// the shutdown's budget so that no retry is sent under a shutdown still running.
// After maxPrepareAttempts the operation fails with the control plane's message.
func (r *StorageNodeOpsReconciler) retryPrepare(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	clusterID, nodeID string,
	progress RemovalProgress,
) (bool, error) {
	recorded := ops.Status.Removal
	if recorded == nil || recorded.LastProgressTime == nil {
		return false, nil
	}
	lastActivity := recorded.LastProgressTime.Time
	if last := recorded.LastPrepareTime; last != nil && last.After(lastActivity) {
		lastActivity = last.Time
	}
	failed := progress.Failed > 0 && (recorded.LastPrepareTime == nil ||
		time.Since(recorded.LastPrepareTime.Time) >= prepareRetryAfterFailure)
	stalled := time.Since(lastActivity) >= shuttingDownDeadline
	if !failed && !stalled {
		return false, nil
	}

	if recorded.PrepareAttempts >= maxPrepareAttempts {
		reason := progress.Message
		if reason == "" {
			reason = fmt.Sprintf("the node stayed %s", nodeStatusPendingRemoval)
		}
		return false, fatalf("the control plane did not shut node %s down for its removal after "+
			"%d attempts: %s", ops.Spec.NodeRef, recorded.PrepareAttempts+1, reason)
	}

	// The attempt is counted in the claim's own patch, before the call, so a pass
	// that dies between the two cannot send unbounded retries, and a pass that
	// read the step from a cache loses the claim and neither counts nor sends.
	now := metav1.Now()
	claimed, err := r.once(ctx, ops, func() error {
		if err := r.API.PrepareRemoval(ctx, clusterID, nodeID); err != nil && refused(err) {
			return fatalf("the control plane refused to remove node %s: %v", ops.Spec.NodeRef, err)
		}
		return nil
	}, func() {
		ops.Status.Removal.PrepareAttempts++
		ops.Status.Removal.LastPrepareTime = &now
	})
	return claimed, err
}

// passingRefusals are the admission's refusals that pass by themselves, in the
// control plane's own wording, lowercased: a cluster still rebalancing, a task
// still active on the node, and peers that are down, which come back.
var passingRefusals = []string{
	"wait for rebalancing",
	"is rebalancing",
	"active task(s) on the node",
	"not-online node",
	"is not online",
	"risk budget already committed",
	"peer node(s) not online",
}

// unexplainedRemovalRefusal is what the node DELETE answers a refused removal with
// when the control plane does not say why.
const unexplainedRemovalRefusal = "failed to remove storage node"

// removalDeferral reads a 400 the control plane answered prepare-removal or the
// node DELETE with, and returns the hold to report when the refusal passes by
// itself, or nil when it is final or not a refusal at all.
//
// Both calls run the control plane's removal admission, and some of its
// refusals describe a moment rather than the cluster: a rebalance still
// running, an active task on the node, a peer that is down. The node is already
// shut down by then, so failing the operation on one of them leaves it offline
// for a condition that clears minutes later. The operation holds instead, the
// claim's lease paces the next attempt, and the step's deadline bounds the
// wait.
//
// A refusal that names its reason is judged by the reason. One that names none,
// which is how the node DELETE answers on a control plane that does not report
// it, is judged by the cluster: a cluster that is not active or is rebalancing
// is one the admission refuses for that, and a settled one is refusing for a
// reason no wait changes.
func (r *StorageNodeOpsReconciler) removalDeferral(
	ops *simplyblockv1alpha2.StorageNodeOps, clusterID string, err error,
) error {
	var answer *ControlPlaneError
	if !errors.As(err, &answer) || answer.Status != http.StatusBadRequest {
		return nil
	}
	reason := refusalReason(answer.Body)
	lowered := strings.ToLower(reason)
	for _, passing := range passingRefusals {
		if strings.Contains(lowered, passing) {
			return blockedf(RemovalDeferred,
				"the control plane deferred the removal of node %s: %s; it is asked again",
				ops.Spec.NodeRef, reason)
		}
	}
	if lowered != "" && lowered != unexplainedRemovalRefusal {
		return nil
	}
	if busy := r.clusterBusy(clusterID); busy != "" {
		return blockedf(RemovalDeferred,
			"the control plane refused the removal of node %s while the cluster %s; it is asked "+
				"again once the cluster settles", ops.Spec.NodeRef, busy)
	}
	return nil
}

// refusalReason is the reason a control-plane error body gives: the detail of a
// JSON body, and the body itself when it has none.
func refusalReason(body string) string {
	var answer struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err == nil && answer.Detail != "" {
		return answer.Detail
	}
	return strings.TrimSpace(body)
}

// clusterBusy says what keeps the cluster from settling, from the cluster
// stream, and is empty when the cluster is active and not rebalancing or when
// the stream has not reported it. An unreported cluster is not read as a busy
// one: the hold exists for a cluster known to be busy, not for one unknown.
func (r *StorageNodeOpsReconciler) clusterBusy(clusterID string) string {
	if r.Clusters == nil || !r.Clusters.SyncedRoot() {
		return ""
	}
	reading, ok := r.Clusters.Lookup(clusterID)
	switch {
	case !ok:
		return ""
	case reading.Status != utils.ClusterStatusActive:
		return "is " + reading.Status
	case reading.Rebalancing:
		return "is rebalancing"
	}
	return ""
}

// shutdownDeferral reads the answer to the removal's shutdown and returns the
// hold to report when the control plane answered 409, which is how it says a
// graceful shutdown's precondition is not met yet, and nil otherwise. Every
// such precondition clears by itself, the node is still serving while the step
// waits, and the claim's lease sends the shutdown again at most once a minute.
func shutdownDeferral(ops *simplyblockv1alpha2.StorageNodeOps, err error) error {
	var answer *ControlPlaneError
	if !errors.As(err, &answer) || answer.Status != http.StatusConflict {
		return nil
	}
	return blockedf(RemovalDeferred,
		"the control plane deferred the shutdown of node %s for its removal: %s; it is asked again",
		ops.Spec.NodeRef, refusalReason(answer.Body))
}

// refused reports whether the control plane answered with a 4xx, which is its
// own decision and not something a retry changes. A timeout or a 5xx says nothing
// about whether the call took effect.
func refused(err error) bool {
	var answer *ControlPlaneError
	return errors.As(err, &answer) && answer.Status >= 400 && answer.Status < 500
}

// drainMigrate moves every PV-managed volume to a peer, one migration object per
// NVMe-oF subsystem, and completes when all of them have.
//
// Completed objects are deleted immediately, which is what keeps a hundred-volume
// drain from leaving a hundred objects behind. status.drain is the progress record
// rather than the objects' presence, which is why the counter is written before
// the delete rather than derived from a List (§8.4).
//
// Failed objects are kept until the step ends, because they are the memory of
// where each subsystem could not go (retry.go). A failed move is replaced
// rather than failing the drain: the volume is still on the node, and the peer
// it could not reach is not the only peer.
func (r *StorageNodeOpsReconciler) drainMigrate(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, clusterID, nodeID string,
) (bool, error) {
	log := logf.FromContext(ctx)

	migrations, err := r.migrationsOf(ctx, ops, nodeID)
	if err != nil {
		return false, err
	}

	// An aborted move was called off rather than failed, so it is re-issued
	// and blames nobody.
	if reissued, err := r.reissueAbortedMigrations(ctx, ops, migrations); err != nil {
		return false, err
	} else if reissued > 0 {
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

	var live, failed []vmigration.Move
	taken := make(map[string]struct{}, len(migrations))
	for _, migration := range migrations {
		taken[migration.Name] = struct{}{}
		if migration.Phase == vmigration.MoveFailed {
			failed = append(failed, migration)
		} else {
			live = append(live, migration)
		}
	}

	// No movable volume is left and no migration is outstanding: everything that
	// was going to move has moved. The census is the authority rather than the
	// counter, because the counter is a record of what this operation did and the
	// census is what is actually on the node. The failed moves have nothing left
	// to remember, so they go with the step.
	if len(census.Managed) == 0 && len(live) == 0 {
		for _, migration := range failed {
			if err := r.mover().Delete(ctx, migration); err != nil {
				return false, fmt.Errorf("delete the failed migration %s: %w", migration.Name, err)
			}
		}
		r.emit(ctx, ops, corev1.EventTypeNormal, DrainCompleted,
			"Every volume has been migrated off the node")
		return true, nil
	}

	// Every subsystem with a movable volume and no live migration gets one. That
	// covers the first pass, a failed move, an object deleted out of band, and a
	// volume that arrived on the node after the count was taken.
	//
	// A move carries its whole subsystem, so a subsystem is covered by a live
	// move named after any of its volumes. The control plane moves the members'
	// records to the target one at a time during a cutover, so the volume a
	// move is named by can have left the node while a sibling is still reported
	// on it; the subsystem is what says the sibling is already being moved.
	covered := make(map[string]struct{}, len(live))
	coveredVolumes := make(map[string]struct{}, len(live))
	for _, migration := range live {
		covered[subsystemKey(census.subsystemOf(migration.PVName), migration.PVName)] = struct{}{}
		coveredVolumes[migration.PVName] = struct{}{}
	}
	var missing []managedVolume
	for _, move := range subsystemMoves(census.Managed) {
		if _, ok := covered[subsystemKey(move.NQN, move.PVName)]; ok {
			continue
		}
		if _, ok := coveredVolumes[move.PVName]; ok {
			continue
		}
		missing = append(missing, move)
	}

	if len(missing) > 0 {
		histories := failureHistories(census, failed)
		ruledOut := make(map[string][]string, len(missing))
		for _, move := range missing {
			if history := histories[subsystemKey(move.NQN, move.PVName)]; history != nil {
				ruledOut[move.PVName] = history.ruledOut
			}
		}
		targets, err := r.peerTargets(ctx, clusterID, nodeID, missing, ruledOut)
		if err != nil {
			return false, err
		}
		for _, move := range missing {
			attempt := 0
			history := histories[subsystemKey(move.NQN, move.PVName)]
			if history != nil {
				attempt = history.attempts
			}
			name := retryName(nodeID, move.PVName, attempt, taken)
			target := targets[move.PVName]
			if err := r.createMigration(ctx, ops, name, nodeID, move, target); err != nil {
				log.Error(err, "a volume's migration could not be created",
					"volume", move.VolumeUUID, "persistentVolume", move.PVName)
				continue
			}
			if history != nil {
				r.emit(ctx, ops, corev1.EventTypeWarning, MigrationRetried, fmt.Sprintf(
					"The migration of %s failed %d %s, last on node %s (%s); it is retried against node %s",
					move.PVName, history.attempts, plural(history.attempts, "time", "times"),
					history.last.TargetNodeUUID, history.last.Message, target))
			}
		}
		return false, nil
	}

	// Progress is counted in volumes. A move carries every volume of its
	// subsystem, and one that has not reported how many carried one.
	completed, running := 0, 0
	for _, migration := range live {
		if migration.Phase == vmigration.MoveSucceeded {
			completed += max(migration.Members, 1)
		} else {
			running++
		}
	}

	if running > 0 {
		return false, r.recordDrainProgress(ctx, ops, completed)
	}

	// Every live migration finished. The counter is written before the objects
	// go, so a crash between the two leaves the progress recorded rather than
	// lost.
	if err := r.recordDrainProgress(ctx, ops, completed); err != nil {
		return false, err
	}
	drainVolumesMigratedTotal.WithLabelValues(r.clusterLabel(ctx, ops)).Add(float64(completed))
	for _, migration := range live {
		if err := r.mover().Delete(ctx, migration); err != nil {
			log.Error(err, "a completed migration could not be deleted",
				"migration", migration.Name)
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
// the node reports no volumes at all and the control plane's verify-drained
// agrees, which also covers snapshots.
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
	if len(census.System) > 0 {
		return false, nil
	}

	// The census walks the pools for volumes and sees no snapshots, and the
	// node DELETE refuses a node that still holds either. verify-drained is the
	// control plane's own answer over both, so it is the one that closes the
	// step.
	verification, err := r.API.VerifyDrained(ctx, clusterID, nodeID)
	if err != nil {
		return false, fmt.Errorf("verify that node %s is drained: %w", ops.Spec.NodeRef, err)
	}
	if !verification.Drained {
		left := append(slices.Clone(verification.Lvols), verification.Snapshots...)
		return false, blockedf(DrainBlocked,
			"the control plane still sees %d %s on the node (%s); the removal is held",
			len(left), plural(len(left), "volume or snapshot", "volumes or snapshots"),
			strings.Join(left, ", "))
	}
	return true, nil
}

// drainRemove deletes the backend node. A 404 is success, since a node the control
// plane no longer knows about is a node that has been removed.
//
// The DELETE can take longer to answer than the client waits, so a pass after a
// lost answer reads the node first. in_removal, removed, and removed_failed say
// the teardown the DELETE starts has begun, so the DELETE is not sent again. The
// preparation statuses (pending_removal through migrating_lvols) say nothing
// about the DELETE: prepare-removal leaves the node migrating_lvols before it is
// ever sent. The DELETE is sent for those, which the control plane answers with
// the running removal when one is already in flight. A node still shutting down
// is waited on.
func (r *StorageNodeOpsReconciler) drainRemove(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, clusterID, nodeID string,
) (bool, error) {
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	switch reading.Status {
	case nodeStatusInRemoval, nodeStatusRemoved, nodeStatusRemovedFailed:
		// The teardown has begun, and how it ends is AwaitingRemoval's to read.
		return true, nil
	case nodeStatusInShutdown:
		return false, nil
	}

	claimed, err := r.once(ctx, ops, func() error {
		if err := r.API.RemoveNode(ctx, clusterID, nodeID); err != nil {
			if deferral := r.removalDeferral(ops, clusterID, err); deferral != nil {
				return deferral
			}
			// Only an answer is a refusal. A 4xx is the control plane's own
			// admission saying what the cluster can afford to lose. Retrying
			// cannot change it, so the operation fails, and the node is left to
			// the control plane. A timeout or a 5xx says nothing about the
			// removal, which may well be under way, so the step is retried and
			// the next pass reads the node.
			if refused(err) {
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
// machine's deadline a whole budget out from now.
//
// The record and the machine's position go in one patch, so progress is never
// persisted without the deadline it extended: a pass that lost a second patch
// would keep the new progress with the old deadline, and the next pass, seeing
// nothing new, would extend nothing. The deadline itself is the machine's
// (Machine.Extend); this only persists where the machine now is, keeping the
// claim the step carries (snapshotOf).
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
	machine.Extend(stepBudgets[machine.CurrentState()])
	staged := &simplyblockv1alpha2.RemovalStatus{
		NodeStatus:       nodeStatus,
		Devices:          statuses,
		LastProgressTime: &now,
	}
	if recorded := ops.Status.Removal; recorded != nil {
		staged.PrepareAttempts = recorded.PrepareAttempts
		staged.LastPrepareTime = recorded.LastPrepareTime
	}
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageNodeOpsStatus) {
		status.Removal = staged
		status.Step = snapshotOf(status.Step, machine)
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
	name, nodeID string,
	volume managedVolume,
	target string,
) error {
	return r.mover().Start(ctx, vmigration.MoveRequest{
		Name:           name,
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

// reissueAbortedMigrations deletes every migration that was aborted and reports
// how many, so the caller's next pass raises each again.
//
// An abort is a decision rather than a verdict on the target: somebody set
// spec.abort, or the move was called off from elsewhere. So the move is deleted
// rather than kept as a failure, and its target is not ruled out.
func (r *StorageNodeOpsReconciler) reissueAbortedMigrations(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	migrations []vmigration.Move,
) (int, error) {
	reissued := 0
	for _, migration := range migrations {
		if migration.Phase != vmigration.MoveAborted {
			continue
		}
		r.emit(ctx, ops, corev1.EventTypeNormal, MigrationRetried, fmt.Sprintf(
			"The migration of %s was aborted and is re-issued; node %s is not ruled out for it",
			migration.PVName, migration.TargetNodeUUID))
		if err := r.mover().Delete(ctx, migration); err != nil {
			return reissued, fmt.Errorf("delete the aborted migration of %s: %w",
				migration.PVName, err)
		}
		reissued++
	}
	return reissued, nil
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
