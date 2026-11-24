// The leave a JoinConsistencyGroup runs first when the volume is a member of
// another group, as it is when its claim was relabeled from one group to
// another: the volume is split out of the NVMe-oF subsystem it shares with that
// group's members, through the same swap a join moves a group with, and is then
// detached from the group. Only then is the join planned.
//
// The order is what keeps the volume consistent with one group at every
// moment. A split rolled back inside its window leaves the volume in its old
// subsystem, and the operation fails with the volume still a member of its old
// group. A detach happens only once the volume no longer shares a subsystem
// with that group's members, so no group is ever left with a subsystem holding
// a volume that is not its member.
//
// design-cg-live-subsystem-flip.md §4 is the specification.

package volume

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// leaveRequestSuffix keeps the leave's request ID apart from the join's, which
// the control plane would otherwise answer with the move of another group.
const leaveRequestSuffix = "-leave"

// leaving reports a volume still to be split out of the group it leaves, which
// is when the swap steps move its namespace on that group.
func leaving(join simplyblockv1alpha2.JoinConsistencyGroupStatus) bool {
	return join.Leave != nil && !join.Leave.Split
}

// moveGroupOf is the group every subsystem-move call addresses: the group the
// volume leaves while it is being split out of it, and the group it joins
// after.
func moveGroupOf(join simplyblockv1alpha2.JoinConsistencyGroupStatus) string {
	if leaving(join) {
		return join.Leave.GroupUUID
	}
	return join.GroupUUID
}

// selectLeave has the control plane reserve the fresh subsystem the volume is
// split into, then locks what the split touches. A volume that shares its
// subsystem with nobody has nothing to split out of.
func (r *PersistentVolumeOpsReconciler) selectLeave(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	old := joinOf(ops).Leave.GroupUUID
	move, err := r.Join.CreateSubsystemLeave(ctx, subject.clusterUUID, old,
		subject.handle.VolumeID, string(ops.UID)+leaveRequestSuffix)
	switch {
	case errors.Is(err, controlplane.ErrSubsystemLimitReached):
		return "", false, joinFatalf(ReasonSubsystemLimitReached,
			"the volume stays a member of consistency group %s: its node cannot hold the fresh subsystem "+
				"the volume needs to leave the one it shares with that group's members: %v", old, err)
	case errors.Is(err, controlplane.ErrColocationRefused):
		return "", false, joinFatalf(ReasonConsistencyGroupColocationRefused,
			"the volume stays a member of consistency group %s: the control plane cannot move it out of "+
				"the subsystem it shares with that group's members: %v", old, err)
	case errors.Is(err, controlplane.ErrMoveConflict):
		return "", false, waitingf("consistency group %s is in another move or migration: %v", old, err)
	case err != nil:
		return "", false, err
	}
	if move.Empty() {
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			joinStatus(status).Leave.Split = true
		}); err != nil {
			return "", false, err
		}
		return stepLeaving, true, nil
	}
	if err := r.recordMove(ctx, ops, move); err != nil {
		return "", false, err
	}
	return r.lockAndFindHosts(ctx, ops, subject)
}

// joinLeave runs the leave a step at a time: the swap that splits the volume
// out of its old group's subsystem, the record that it is split, the detach,
// and then the plan of the join.
func (r *PersistentVolumeOpsReconciler) joinLeave(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	join := joinOf(ops)
	old := join.Leave.GroupUUID
	if !join.Leave.Split {
		if join.MoveID == "" {
			return stepSelectingSubsystem, true, nil
		}
		// Back from a swap that committed. The locks are given back before the
		// split is recorded, so a recorded split never holds a lock: the Migrate
		// a join may create next takes the volume's lock itself.
		if err := r.releaseLock(ctx, ops); err != nil {
			return "", false, err
		}
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			j := joinStatus(status)
			j.Leave.Split = true
			j.Leave.SplitNQN = j.TargetNQN
			j.MoveID, j.TargetNQN, j.TargetIsNew, j.NamespacesAt = "", "", false, ""
			j.Volumes, j.Hosts, j.SuspendDeadline = nil, nil, nil
		}); err != nil {
			return "", false, err
		}
		message := fmt.Sprintf("The volume left the subsystem it shared with consistency group %s's members, into %s",
			old, joinOf(ops).Leave.SplitNQN)
		r.event(ops, corev1.EventTypeNormal, ReasonConsistencyGroupSplit, "%s", message)
		r.claimEvent(ctx, subject.pv, corev1.EventTypeNormal, ReasonConsistencyGroupSplit, "%s", message)
	}

	if !joinOf(ops).Leave.Detached {
		if err := r.Join.DetachConsistencyGroupMember(ctx, subject.clusterUUID, old, subject.handle.VolumeID); err != nil {
			return "", false, fmt.Errorf("detach the volume from consistency group %s: %w", old, err)
		}
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			joinStatus(status).Leave.Detached = true
		}); err != nil {
			return "", false, err
		}
		message := fmt.Sprintf("The volume left consistency group %s; the generations that contain it stay restorable", old)
		r.event(ops, corev1.EventTypeNormal, ReasonConsistencyGroupLeft, "%s", message)
		r.claimEvent(ctx, subject.pv, corev1.EventTypeNormal, ReasonConsistencyGroupLeft, "%s", message)
	}
	return r.planJoin(ctx, ops, subject)
}
