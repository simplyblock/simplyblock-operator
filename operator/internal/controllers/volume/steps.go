// What each step of a migration does on the pass it is entered, and what makes
// it finished.
//
// The three steps are the three things a safe migration is made of. The
// migration is created and every host that consumes the subsystem is checked
// against the target it is about to be served from. The copy runs. The paths
// the creation published are accounted for. Skipping the first leaves a host
// pointing at a node that has nothing for it at cutover; skipping the last
// leaves connections nothing tracks, which poisons the data path and blocks
// every later migration of the volume.
//
// One rule runs through all of it: a step finishes on the state the control
// plane reports, never on the return of the call that started it. A five-second
// read timeout on a request that took slightly longer once made the operator
// retry a transfer that had already committed, and the retry copied nothing
// while the source was unfrozen.
//
// design-persistentvolumeops.md §5 and §7 are the specification.

package volume

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Control-plane migration states, in the control plane's own spelling. They are
// not an enum in the client, because the client reports what it was told rather
// than deciding which values exist.
const (
	migrationPhasePreCreated = "pre_created"

	migrationStatusDone     = "done"
	migrationStatusFailed   = "failed"
	migrationStatusCanceled = "canceled"
)

// perform advances the current step and reports whether it has finished.
func (r *PersistentVolumeOpsReconciler) perform(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	subject *subject,
	current step,
) (bool, error) {
	switch current {
	case stepValidating:
		return r.validate(ctx, ops, subject)
	case stepMigrating:
		return r.copy(ctx, ops, subject)
	case stepVerifying:
		return r.verify(ctx, ops, subject)
	default:
		return false, fatalf("step %s has no implementation", current)
	}
}

// validate creates the migration and proves that every host consuming the
// subsystem can reach the target on the paths it published.
//
// It is one step rather than two because the paths only exist once the
// migration does: the control plane publishes them with the creation and with
// nothing else, so a migration created and then forgotten is a migration whose
// paths nothing can name.
func (r *PersistentVolumeOpsReconciler) validate(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	subject *subject,
) (bool, error) {
	if ops.Status.Migration == nil || ops.Status.Migration.MigrationUUID == "" {
		return false, r.createMigration(ctx, ops, subject)
	}

	nodes, err := r.consumingNodes(ctx, ops, subject)
	switch {
	case isConsumerNotReady(err):
		// A consumer that exists and is not Running yet is waited for rather
		// than skipped: a pod that stages against the source mid-migration is
		// stranded at cutover exactly like an established one. The step's
		// deadline is what bounds the wait.
		r.event(ops, corev1.EventTypeNormal, ReasonWaitingForConsumer, "%s", err.Error())
		return false, nil
	case err != nil:
		return false, err
	case len(nodes) == 0:
		// No volume of the subsystem has a consumer, so there are no host paths
		// to check anywhere.
		r.event(ops, corev1.EventTypeNormal, ReasonValidationSkipped,
			"No consumer for any volume of subsystem %s", ops.Status.Migration.SubsystemNQN)
		return true, nil
	}

	started, err := r.startValidationJobs(ctx, ops, subject, nodes)
	if err != nil {
		return false, err
	}
	if started > 0 {
		// The Jobs are watched, so the next pass arrives when one finishes
		// rather than on a timer.
		return false, nil
	}
	return r.validationJobsPassed(ctx, ops, subject)
}

// createMigration asks the control plane for the migration and records
// everything the later steps address it by.
//
// A control plane that is busy is not a failure. It refuses a migration while a
// cluster-wide realignment runs, which ends on its own, so the operation waits
// in its step rather than failing — and the step's deadline is what stops it
// waiting forever.
func (r *PersistentVolumeOpsReconciler) createMigration(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	subject *subject,
) error {
	volume, err := r.API.Volume(ctx, subject.handle.Handle())
	if err != nil {
		return fmt.Errorf("read volume %s: %w", subject.handle.VolumeID, err)
	}
	if volume.NQN == "" {
		return fatalf("volume %s publishes under no subsystem, so its migration cannot be addressed",
			subject.handle.VolumeID)
	}

	migration, err := r.API.CreateMigration(ctx, subject.clusterUUID, volume.NQN, subject.targetUUID)
	if err != nil {
		// The request may have taken effect despite the error: a create can
		// take longer than the client's timeout and allocates on the way, so
		// failing here would abandon a half-created migration. Retrying is what
		// lets a later pass find and cancel it.
		return fmt.Errorf("create the migration of subsystem %s: %w", volume.NQN, err)
	}
	if migration.ID == "" {
		return fatalf("the control plane created a migration with no identifier")
	}
	if migration.SourceNodeID != "" && migration.SourceNodeID == subject.targetUUID {
		r.event(ops, corev1.EventTypeWarning, ReasonTargetNodeIsSource,
			"Volume %s is already on node %s", ops.Spec.PersistentVolumeName, subject.targetNodeName())
		// Cancel rather than continue: the migration exists on the backend and
		// leaving it would block the next one.
		if cancelErr := r.API.CancelMigration(ctx, subject.clusterUUID, volume.NQN, migration.ID); cancelErr != nil {
			return fmt.Errorf("cancel a migration to the node the volume is already on: %w", cancelErr)
		}
		return fatalf("volume %s is already on node %s",
			ops.Spec.PersistentVolumeName, subject.targetNodeName())
	}

	if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Migration = &simplyblockv1alpha2.MigrationStatus{
			MigrationUUID:  migration.ID,
			ClusterUUID:    subject.clusterUUID,
			PoolUUID:       subject.handle.PoolRef,
			VolumeUUID:     subject.handle.VolumeID,
			SubsystemNQN:   volume.NQN,
			SourceNodeUUID: migration.SourceNodeID,
			TargetNodeUUID: subject.targetUUID,
			MemberCount:    ptr.To(int32(migration.MemberCount)),
			Connections:    connectionsOf(migration),
		}
		status.DeferredSince = nil
	}); err != nil {
		return err
	}

	r.event(ops, corev1.EventTypeNormal, ReasonMigrationCreated,
		"Migration %s created for subsystem %s (%d volume(s)) to node %s",
		migration.ID, volume.NQN, migration.MemberCount, subject.targetNodeName())
	return nil
}

// connectionsOf records the paths the migration published, as they will be
// connected rather than as the control plane answered.
//
// The controller-loss timeout is overridden here rather than where the Job is
// built, so that what status.connections shows is the connect that will
// actually be made. Every volume path in this system is established with the
// CSI driver's timeout, and a migration target path becomes the volume's data
// path at cutover: connecting it with the hour the control plane answers would
// leave one volume's paths on two different timeouts depending on which of them
// last moved.
func connectionsOf(migration controlplane.Migration) []simplyblockv1alpha2.MigrationConnection {
	out := make([]simplyblockv1alpha2.MigrationConnection, 0, len(migration.Paths))
	for _, path := range migration.Paths {
		out = append(out, simplyblockv1alpha2.MigrationConnection{
			NQN:                      migration.TargetNQN,
			Address:                  path.Address,
			Port:                     ptr.To(int32(path.Port)),
			Transport:                path.Transport,
			NrIOQueues:               ptr.To(int32(path.NrIOQueues)),
			ReconnectDelaySeconds:    ptr.To(int32(path.ReconnectDelaySec)),
			CtrlLossTimeoutSeconds:   ptr.To(int32(migrationCtrlLossTimeout)),
			FastIOFailTimeoutSeconds: ptr.To(int32(ptr.From(path.FastIOFailTMOSec, 0))),
			KeepAliveTimeoutSeconds:  ptr.To(int32(path.KeepAliveTMOSec)),
		})
	}
	return out
}

// copy continues the migration, which is what starts the data copy, and waits
// for the control plane to report it finished.
//
// Continue is not idempotent: it accepts a migration in pre_created and rejects
// any later call. So the phase is read first, and a migration that has already
// advanced is left alone — a pass that continued the copy and then crashed
// before recording it must not have its copy canceled by the pass that
// follows.
func (r *PersistentVolumeOpsReconciler) copy(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	subject *subject,
) (bool, error) {
	migration := ops.Status.Migration
	if migration == nil || migration.MigrationUUID == "" {
		return false, fatalf("the operation reached the copy with no migration recorded")
	}

	current, err := r.API.GetMigration(ctx,
		migration.ClusterUUID, migration.SubsystemNQN, migration.MigrationUUID)
	if err != nil {
		return false, fmt.Errorf("read migration %s: %w", migration.MigrationUUID, err)
	}

	switch current.Status {
	case migrationStatusDone:
		return true, nil
	case migrationStatusFailed:
		return false, fatalf("the migration failed: %s", current.ErrorMessage)
	case migrationStatusCanceled:
		return false, fatalf("the migration was canceled outside this operation")
	}

	if current.Phase == migrationPhasePreCreated && migration.ContinuedAt == nil {
		// Recorded before it is issued, which makes the request at-most-once.
		// A repeated continue is the shape that has lost writes here, so a
		// crash between this write and the call leaves the copy unstarted and
		// the step to time out rather than leaving a transfer to be retried.
		now := metav1.Now()
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			status.Migration.ContinuedAt = &now
		}); err != nil {
			return false, err
		}
		if err := r.API.ContinueMigration(ctx,
			migration.ClusterUUID, migration.SubsystemNQN, migration.MigrationUUID); err != nil {
			// Whether the copy started is what the next read says, not what
			// this error says. A call that timed out after the transfer
			// committed reports a failure the migration itself contradicts.
			return false, fmt.Errorf("continue migration %s: %w", migration.MigrationUUID, err)
		}
		r.event(ops, corev1.EventTypeNormal, ReasonMigrationStarted,
			"Migration %s started: volume %s to node %s",
			migration.MigrationUUID, ops.Spec.PersistentVolumeName, subject.targetNodeName())
	}

	// The source node is only reported once the migration is running, and a
	// failure that says where the volume was is worth more than one that says
	// only where it was going.
	if current.SourceNodeID != "" && current.SourceNodeID != migration.SourceNodeUUID {
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			status.Migration.SourceNodeUUID = current.SourceNodeID
			if current.MemberCount > 0 {
				status.Migration.MemberCount = ptr.To(int32(current.MemberCount))
			}
		}); err != nil {
			return false, err
		}
	}
	return false, nil
}

// verify takes the validation Jobs down and clears the husks a migration leaves
// on the hosts that took part in it.
//
// The paths themselves are not released. By this point the copy has finished
// and the target is where the volume is served from, so the paths the creation
// published are the data path: releasing them here is the outage the whole
// cleanup exists to avoid. What is cleared is the controller that carries
// nothing at all — the state a path lost mid-validation settles into, which
// blocks the subsystem's next migration just as surely as a live leak would.
//
// An operation that never cut over is a different case, and it does release:
// see discardMigration.
func (r *PersistentVolumeOpsReconciler) verify(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	subject *subject,
) (bool, error) {
	if ops.Status.Migration == nil || len(ops.Status.Migration.ValidationJobs) == 0 {
		// Nothing was validated, so no host connected a path on this
		// operation's account and there is nothing to account for.
		return true, nil
	}

	if err := r.deleteValidationJobs(ctx, ops); err != nil {
		return false, err
	}

	done, err := r.reapOnEveryValidatedNode(ctx, ops, subject)
	if err != nil || !done {
		return false, err
	}

	// The record of the Jobs goes once they are gone and their hosts are
	// clear, so that a restart does not start the cleanup over.
	return true, r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Migration.ValidationJobs = nil
		status.Migration.Connections = nil
	})
}

// discardMigration takes back everything the operation created: the backend
// migration, and the target paths every consuming host connected on its
// account.
//
// It runs on the abort, the failure, and the deletion paths, which is to say
// exactly where the migration did not cut over. That is the precondition the
// release has: after a cutover the target paths are the volume's data path, and
// releasing them then is the outage the check exists to prevent.
//
// Every recorded node is asked rather than only the ones that passed. Release
// is idempotent and declines to touch a path that is serving, so asking a node
// that already released costs one Job and reports nothing; guessing which nodes
// still hold paths would mean trusting a Job's success to mean "connected,"
// which it does not — a Job killed mid-run leaves paths with no record at all.
func (r *PersistentVolumeOpsReconciler) discardMigration(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) error {
	migration := ops.Status.Migration
	if migration == nil || migration.MigrationUUID == "" {
		// Nothing was ever created, which is the state being asked for.
		return nil
	}

	if err := r.API.CancelMigration(ctx,
		migration.ClusterUUID, migration.SubsystemNQN, migration.MigrationUUID); err != nil {
		return fmt.Errorf("cancel migration %s: %w", migration.MigrationUUID, err)
	}

	if err := r.deleteValidationJobs(ctx, ops); err != nil {
		return err
	}
	if err := r.releaseOnEveryValidatedNode(ctx, ops, subject); err != nil {
		return err
	}

	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Migration.ValidationJobs = nil
		status.Migration.Connections = nil
	})
}

// isConsumerNotReady reports the one waiting condition the consumer lookup
// distinguishes from a genuine failure.
func isConsumerNotReady(err error) bool {
	return err != nil && strings.Contains(err.Error(), consumerNotRunning)
}

// stepStarted is when the operation entered its current step, derived from the
// step's deadline and the budget it was set from. It is what the per-step
// histogram measures against, and it is derived rather than recorded because a
// second timestamp in status would be a second thing to keep true.
func stepStarted(ops *simplyblockv1alpha2.PersistentVolumeOps, current step) (time.Time, bool) {
	deadline, bounded := ops.Status.Step.KubeDeadline()
	if !bounded {
		return time.Time{}, false
	}
	budget, known := stepBudgets[current]
	if !known {
		budget = copyDeadline(memberCount(ops))
	}
	return deadline.Add(-budget), true
}

// expectedMembers is the member count a validation has to cover, which is the
// subsystem's members and, always, the volume this operation names: a listing
// that raced a change must not drop the one volume the operation is about.
func expectedMembers(members []lvol.Volume, volumeUUID string) map[string]struct{} {
	out := make(map[string]struct{}, len(members)+1)
	for _, member := range members {
		if handle, ok := lvol.ParseHandle(member.ID); ok {
			out[handle.VolumeID] = struct{}{}
		}
	}
	out[volumeUUID] = struct{}{}
	return out
}
