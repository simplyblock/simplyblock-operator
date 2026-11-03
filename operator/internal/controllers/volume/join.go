// The JoinConsistencyGroup action: a volume whose claim gained the
// consistency-group label is migrated onto its group's node, added to the
// group, and brought into the group's NVMe-oF subsystem while its consumers
// keep running.
//
// This file carries the flow and the steps up to the swap: the group, the plan,
// the child migration, the join, and the selection of the subsystem the group
// ends up in. The swap itself, where every consuming host holds its I/O while
// the namespaces change subsystem, is join_swap.go.
//
// The action runs its own pass rather than the Migrate one, because it takes
// neither the cluster's migration slot nor the named volume's lock up front:
// the child migration takes both itself, and the swap locks every volume it
// moves, which is only known once the control plane has chosen the target.
//
// design-cg-live-subsystem-flip.md §4, §5.5, §7, and §8 are the specification.

package volume

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/statemachine"
	"github.com/simplyblock/atlas/volstack/swaprpc"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The action's steps. Migrating, AttachingPaths, and Committing are the
// kind's existing names, reused because they mean the same thing here.
const (
	stepPlanning           = simplyblockv1alpha2.PersistentVolumeOpsStepPlanning
	stepJoinMigrating      = stepMigrating
	stepJoining            = simplyblockv1alpha2.PersistentVolumeOpsStepJoining
	stepSelectingSubsystem = simplyblockv1alpha2.PersistentVolumeOpsStepSelectingSubsystem
	stepPreparingTarget    = simplyblockv1alpha2.PersistentVolumeOpsStepPreparingTarget
	stepJoinAttachingPaths = stepAttachingPaths
	stepSuspending         = simplyblockv1alpha2.PersistentVolumeOpsStepSuspending
	stepMovingNamespaces   = simplyblockv1alpha2.PersistentVolumeOpsStepMovingNamespaces
	stepJoinResuming       = simplyblockv1alpha2.PersistentVolumeOpsStepResuming
	stepJoinCommitting     = stepCommitting
	stepReleasingPaths     = simplyblockv1alpha2.PersistentVolumeOpsStepReleasingPaths
	stepLeaving            = simplyblockv1alpha2.PersistentVolumeOpsStepLeaving
)

const (
	// joinInsideWindow is how soon a step inside the suspend window looks
	// again. The window is seconds long and every host's I/O is held for its
	// length, so a step waiting in it is not left waiting on opsRetry.
	joinInsideWindow = 500 * time.Millisecond

	// defaultSuspendTimeout is spec.joinConsistencyGroup.suspendTimeoutSeconds
	// when the admission default did not apply, as on an object a test builds.
	defaultSuspendTimeout = 10 * time.Second

	// childMigrationSuffix names the Migrate operation a join creates.
	childMigrationSuffix = "-migrate"
)

// JoinClient is the control-plane surface a join needs.
//
// It is separate from MigrationClient because only this action calls it, and
// a test of a migration has no business faking a join.
type JoinClient interface {
	ConsistencyGroupNamed(ctx context.Context, clusterID, name string) (controlplane.ConsistencyGroup, bool, error)
	PlanConsistencyGroupJoin(ctx context.Context, clusterID, groupID, lvolID string) (controlplane.JoinPlan, error)
	JoinConsistencyGroupMember(ctx context.Context, clusterID, groupID, lvolID string) error
	CreateSubsystemMove(ctx context.Context, clusterID, groupID, lvolID, requestID string) (controlplane.SubsystemMove, error)
	CreateSubsystemLeave(ctx context.Context, clusterID, groupID, lvolID, requestID string) (controlplane.SubsystemMove, error)
	DetachConsistencyGroupMember(ctx context.Context, clusterID, groupID, lvolID string) error
	GetSubsystemMove(ctx context.Context, clusterID, groupID, moveID string) (controlplane.SubsystemMove, error)
	PrepareSubsystemMove(ctx context.Context, clusterID, groupID, moveID, hostNQN string) (controlplane.PreparedMove, error)
	StartSubsystemMove(ctx context.Context, clusterID, groupID, moveID string, deadline time.Time) (controlplane.SubsystemMove, error)
	CommitSubsystemMove(ctx context.Context, clusterID, groupID, moveID string) error
	RollbackSubsystemMove(ctx context.Context, clusterID, groupID, moveID string) error
}

// SwapNode is one CSI node's swap service, as swaprpc.Client serves it.
type SwapNode interface {
	Describe(ctx context.Context, handles []string) ([]swaprpc.VolumeState, error)
	Prepare(ctx context.Context, handles []string, targetNQN string, paths []nvmeof.Target) ([]swaprpc.VolumeState, error)
	Suspend(ctx context.Context, handles []string, deadline time.Time) ([]swaprpc.VolumeState, error)
	Resume(ctx context.Context, handles []string, nqn string) ([]swaprpc.VolumeState, error)
	Release(ctx context.Context, nqn string) (bool, error)
}

// SwapPeers finds the swap service of the CSI node on a Kubernetes node, and
// the instance of the plugin process serving it. A different instance on a
// later call is a plugin that restarted in between.
type SwapPeers interface {
	Node(name string) (SwapNode, string, error)
}

var (
	// ErrNoSwapPeer is a node whose CSI plugin is not linked to the operator.
	ErrNoSwapPeer = errors.New("no CSI node plugin is linked on the node")

	// ErrNoSwapCapability is a linked plugin that does not serve the swap,
	// which is a plugin older than the swap service.
	ErrNoSwapCapability = errors.New("the node's CSI plugin does not serve the volume swap")
)

// joinEnded is a step that ended the operation by itself: an empty move, or a
// colocation the control plane will not make. Both succeed.
type joinEnded struct {
	message    string
	colocated  bool
	eventType  string
	reason     string
	eventProse string
}

func (e *joinEnded) Error() string { return e.message }

// rollbackNeeded is a step that cannot move the group forward and has
// something on the hosts or in the control plane to take back.
type rollbackNeeded struct{ reason string }

func (e *rollbackNeeded) Error() string { return e.reason }

func rollbackf(format string, args ...any) error {
	return &rollbackNeeded{reason: fmt.Sprintf(format, args...)}
}

// joinFatal is a terminal failure that owes an event of its own reason.
type joinFatal struct {
	reason  string
	message string
}

func (e *joinFatal) Error() string { return e.message }

func joinFatalf(reason, format string, args ...any) error {
	return &joinFatal{reason: reason, message: fmt.Sprintf(format, args...)}
}

// now is the reconciler's clock.
func (r *PersistentVolumeOpsReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// joinStatus is the action's status block, created on first use.
func joinStatus(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) *simplyblockv1alpha2.JoinConsistencyGroupStatus {
	if status.JoinConsistencyGroup == nil {
		status.JoinConsistencyGroup = &simplyblockv1alpha2.JoinConsistencyGroupStatus{}
	}
	return status.JoinConsistencyGroup
}

// joinOf is the action's status block as read, never nil.
func joinOf(ops *simplyblockv1alpha2.PersistentVolumeOps) simplyblockv1alpha2.JoinConsistencyGroupStatus {
	if ops.Status.JoinConsistencyGroup == nil {
		return simplyblockv1alpha2.JoinConsistencyGroupStatus{}
	}
	return *ops.Status.JoinConsistencyGroup
}

// suspendTimeout is how long a host may hold I/O.
func suspendTimeout(ops *simplyblockv1alpha2.PersistentVolumeOps) time.Duration {
	if spec := ops.Spec.JoinConsistencyGroup; spec != nil && spec.SuspendTimeoutSeconds != nil {
		return time.Duration(*spec.SuspendTimeoutSeconds) * time.Second
	}
	return defaultSuspendTimeout
}

// reconcileJoin is the action's pass. The group is resolved before the first
// step, so a label naming a group that does not exist yet holds the operation
// Pending rather than starting it.
func (r *PersistentVolumeOpsReconciler) reconcileJoin(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (ctrl.Result, error) {
	if r.Join == nil {
		r.event(ops, corev1.EventTypeWarning, ReasonOperationFailed,
			"The operator has no control-plane client for consistency-group joins")
		return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed,
			"the operator has no control-plane client for consistency-group joins")
	}

	if ops.Status.Step.State == "" {
		name := ops.Spec.JoinConsistencyGroup.GroupName
		group, found, err := r.Join.ConsistencyGroupNamed(ctx, subject.clusterUUID, name)
		if err != nil {
			return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, err.Error())
		}
		if !found {
			r.event(ops, corev1.EventTypeNormal, ReasonConsistencyGroupPending,
				"No consistency group %s exists in cluster %s yet; the join waits for it", name, subject.clusterUUID)
			r.claimEvent(ctx, subject.pv, corev1.EventTypeNormal, ReasonConsistencyGroupPending,
				"No consistency group %s exists yet; the join waits for it", name)
			return ctrl.Result{RequeueAfter: opsRetry}, r.hold(ctx, ops,
				fmt.Sprintf("waiting for consistency group %s to exist", name))
		}
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			joinStatus(status).GroupUUID = group.ID
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.advanceJoin(ctx, ops, subject)
}

// advanceJoin runs the action's machine forward by at most one step.
func (r *PersistentVolumeOpsReconciler) advanceJoin(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (ctrl.Result, error) {
	machine, err := graphs(0).FromSnapshot(ctx, actionJoin, statemachine.FromKube[step](ops.Status.Step))
	if err != nil {
		return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed,
			fmt.Sprintf("the operation cannot be resumed: %v", err))
	}
	defer machine.Close()

	if ops.Status.Step.State == "" {
		return r.enterInitialStep(ctx, ops, machine)
	}
	current := machine.CurrentState()
	rollingBack := joinOf(ops).RollbackReason != ""

	if ops.Spec.Abort && machine.CanAbort() && !rollingBack {
		return r.unwindJoin(ctx, ops, subject, current)
	}

	if machine.TimeoutReached() && !rollingBack {
		switch current {
		case stepJoinCommitting:
			// The namespaces are in the target and every host has resumed on
			// it, so the commit is retried for as long as it takes: failing
			// here would leave the records naming the source.
		case stepReleasingPaths:
			// A host left unconfirmed keeps an idle path to the subsystem it
			// left, which its own teardown removes.
			if leaving(joinOf(ops)) {
				return r.enterJoinStep(ctx, ops, machine, stepLeaving)
			}
			return r.finishJoin(ctx, ops, subject,
				"some hosts were not confirmed to have released the subsystem they left")
		default:
			r.event(ops, corev1.EventTypeWarning, ReasonStepDeadlineExceeded, "Step %s outlived its deadline", current)
			stepDeadlinesExceeded.WithLabelValues(subject.clusterUUID, string(ops.Spec.Action), string(current)).Inc()
			if joinOf(ops).MoveID != "" {
				return r.beginRollback(ctx, ops, subject, machine,
					fmt.Sprintf("step %s outlived its deadline", current))
			}
			return r.failJoin(ctx, ops, subject, ReasonStepDeadlineExceeded,
				fmt.Sprintf("step %s outlived its deadline", current))
		}
	}

	next, done, err := r.performJoin(ctx, ops, subject, current)
	var (
		waiting  *stepWaiting
		ended    *joinEnded
		rollback *rollbackNeeded
		fatal    *joinFatal
	)
	switch {
	case errors.As(err, &waiting):
		return r.waitInJoin(machine, current), r.note(ctx, ops, fmt.Sprintf("waiting on %s: %s", current, waiting.message))
	case errors.As(err, &ended):
		return r.endJoin(ctx, ops, subject, ended)
	case errors.As(err, &rollback):
		return r.beginRollback(ctx, ops, subject, machine, rollback.reason)
	case errors.As(err, &fatal):
		return r.failJoin(ctx, ops, subject, fatal.reason, fatal.message)
	case err != nil:
		logf.FromContext(ctx).Error(err, "the step could not be advanced", "operation", ops.Name, "step", current)
		return r.waitInJoin(machine, current), r.note(ctx, ops, err.Error())
	}
	if !done {
		return r.waitInJoin(machine, current), nil
	}

	r.observeStep(ops, subject, current)
	if next == "" {
		return r.finishJoin(ctx, ops, subject, "")
	}
	return r.enterJoinStep(ctx, ops, machine, next)
}

// enterJoinStep records the next step before entering it, then enters it.
func (r *PersistentVolumeOpsReconciler) enterJoinStep(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
	machine *statemachine.Machine[step], next step,
) (ctrl.Result, error) {
	if next == machine.CurrentState() {
		return ctrl.Result{RequeueAfter: joinInsideWindow}, nil
	}
	if err := r.recordStep(ctx, ops, next, nil); err != nil {
		return ctrl.Result{}, err
	}
	if err := machine.TransitionTo(ctx, next); err != nil {
		return ctrl.Result{}, fmt.Errorf("enter step %s: %w", next, err)
	}
	snapshot := statemachine.ToKube(machine.Snapshot())
	return ctrl.Result{RequeueAfter: joinInsideWindow}, r.recordStep(ctx, ops, next, snapshot.Deadline)
}

// waitInJoin requeues a waiting step, sooner inside the suspend window.
func (r *PersistentVolumeOpsReconciler) waitInJoin(machine *statemachine.Machine[step], current step) ctrl.Result {
	switch current {
	case stepSuspending, stepMovingNamespaces, stepJoinResuming:
		return ctrl.Result{RequeueAfter: joinInsideWindow}
	}
	return r.waitOn(machine)
}

// performJoin runs one step and says which step follows it. An empty next
// step with done set is the end of the operation.
func (r *PersistentVolumeOpsReconciler) performJoin(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, current step,
) (step, bool, error) {
	switch current {
	case stepPlanning:
		return r.joinPlan(ctx, ops, subject)
	case stepLeaving:
		return r.joinLeave(ctx, ops, subject)
	case stepJoinMigrating:
		return r.joinMigrate(ctx, ops, subject)
	case stepJoining:
		return r.joinMember(ctx, ops, subject)
	case stepSelectingSubsystem:
		return r.joinSelect(ctx, ops, subject)
	case stepPreparingTarget:
		return r.joinPrepare(ctx, ops, subject)
	case stepJoinAttachingPaths:
		return r.joinAttach(ctx, ops, subject)
	case stepSuspending:
		return r.joinSuspend(ctx, ops)
	case stepMovingNamespaces:
		return r.joinMove(ctx, ops, subject)
	case stepJoinResuming:
		return r.joinResume(ctx, ops, subject)
	case stepJoinCommitting:
		return r.joinCommit(ctx, ops, subject)
	case stepReleasingPaths:
		return r.joinRelease(ctx, ops)
	}
	return "", false, fatalf("step %s is not a step of a join", current)
}

// joinPlan sends a volume that is a member of another group to leave it first,
// and plans the join of any other.
func (r *PersistentVolumeOpsReconciler) joinPlan(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	if joinOf(ops).Leave == nil {
		volume, err := r.API.Volume(ctx, subject.handle.Handle())
		if err != nil {
			return "", false, fmt.Errorf("read the consistency group of volume %s: %w", subject.handle.VolumeID, err)
		}
		if volume.GroupID != "" && volume.GroupID != joinOf(ops).GroupUUID {
			// Written ahead of the step: every later pass routes the swap to
			// the group the volume leaves by this record.
			if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
				joinStatus(status).Leave = &simplyblockv1alpha2.ConsistencyGroupLeaveStatus{GroupUUID: volume.GroupID}
			}); err != nil {
				return "", false, err
			}
			return stepLeaving, true, nil
		}
	}
	return r.planJoin(ctx, ops, subject)
}

// planJoin asks the control plane what the join takes, and refuses one that can
// never finish before anything moves.
func (r *PersistentVolumeOpsReconciler) planJoin(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	plan, err := r.Join.PlanConsistencyGroupJoin(ctx, subject.clusterUUID, joinOf(ops).GroupUUID, subject.handle.VolumeID)
	if errors.Is(err, controlplane.ErrMembershipRefused) {
		return "", false, joinFatalf(ReasonConsistencyGroupJoinRefused,
			"consistency group %s refused the volume: %v", ops.Spec.JoinConsistencyGroup.GroupName, err)
	}
	if err != nil {
		return "", false, err
	}
	if co := plan.Colocation; co != nil && strings.HasPrefix(co.Refusal, "SubsystemLimitReached") {
		return "", false, joinFatalf(ReasonSubsystemLimitReached,
			"the group's node cannot hold the subsystems the join needs, so nothing was moved: %s", co.Refusal)
	}
	if !plan.Has(controlplane.JoinStepJoin) {
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			joinStatus(status).Joined = true
		}); err != nil {
			return "", false, err
		}
	}
	if plan.Has(controlplane.JoinStepMigrate) {
		return stepJoinMigrating, true, nil
	}
	return stepJoining, true, nil
}

// childName is the name of the Migrate operation a join creates. A join named
// near the length limit takes a hashed suffix instead, so the name stays valid
// and still identifies its creator.
func childName(ops *simplyblockv1alpha2.PersistentVolumeOps) string {
	name := ops.Name + childMigrationSuffix
	if len(name) <= 253 {
		return name
	}
	sum := sha256.Sum256([]byte(ops.Name))
	return ops.Name[:253-len(childMigrationSuffix)-11] + "-" + hex.EncodeToString(sum[:])[:10] + childMigrationSuffix
}

// joinMigrate brings the volume onto the group's node through a Migrate
// operation of its own, and waits for it.
func (r *PersistentVolumeOpsReconciler) joinMigrate(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	name := joinOf(ops).MigrationName
	if name == "" {
		// Write-ahead: the child's name is recorded before it is created, so
		// a crash between the two finds it by name rather than creating a
		// second.
		name = childName(ops)
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			joinStatus(status).MigrationName = name
		}); err != nil {
			return "", false, err
		}
	}

	var child simplyblockv1alpha2.PersistentVolumeOps
	err := r.Get(ctx, types.NamespacedName{Name: name}, &child)
	if apierrors.IsNotFound(err) {
		ref, err := r.joinTargetNode(ctx, ops, subject)
		if err != nil {
			return "", false, err
		}
		child = simplyblockv1alpha2.PersistentVolumeOps{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: ops.Labels},
			Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
				PersistentVolumeName: ops.Spec.PersistentVolumeName,
				Action:               simplyblockv1alpha2.PersistentVolumeOpsActionMigrate,
				Migrate:              &simplyblockv1alpha2.MigrateVolumeSpec{TargetNodeRef: ref},
				CreatorRef: &simplyblockv1alpha2.CreatorReference{
					Kind: "PersistentVolumeOps", Name: ops.Name, UID: ops.UID,
				},
			},
		}
		if err := r.Create(ctx, &child); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", false, fmt.Errorf("create the migration %s onto the group's node: %w", name, err)
		}
		return "", false, waitingf("migration %s was created", name)
	}
	if err != nil {
		return "", false, fmt.Errorf("read the migration %s: %w", name, err)
	}

	switch child.Status.Phase {
	case simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded:
		return stepJoining, true, nil
	case simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed, simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted:
		return "", false, joinFatalf(ReasonConsistencyGroupMigrationFailed,
			"the migration %s onto the group's node ended %s: %s", name, child.Status.Phase, child.Status.Message)
	}
	return "", false, waitingf("migration %s is %s", name, phaseOrPending(child.Status.Phase))
}

func phaseOrPending(phase simplyblockv1alpha2.PersistentVolumeOpsPhase) simplyblockv1alpha2.PersistentVolumeOpsPhase {
	if phase == "" {
		return simplyblockv1alpha2.PersistentVolumeOpsPhasePending
	}
	return phase
}

// joinTargetNode is the StorageNode the plan names as the group's node.
func (r *PersistentVolumeOpsReconciler) joinTargetNode(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (simplyblockv1alpha2.StorageNodeReference, error) {
	plan, err := r.Join.PlanConsistencyGroupJoin(ctx, subject.clusterUUID, joinOf(ops).GroupUUID, subject.handle.VolumeID)
	if err != nil {
		return simplyblockv1alpha2.StorageNodeReference{}, err
	}
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(subject.namespace())); err != nil {
		return simplyblockv1alpha2.StorageNodeReference{}, fmt.Errorf("list the storage nodes: %w", err)
	}
	for _, node := range nodes.Items {
		if node.Status.UUID == plan.TargetNodeID && plan.TargetNodeID != "" {
			return simplyblockv1alpha2.StorageNodeReference{Namespace: node.Namespace, Name: node.Name}, nil
		}
	}
	return simplyblockv1alpha2.StorageNodeReference{}, fmt.Errorf(
		"no StorageNode in %s reports the group's node %s", subject.namespace(), plan.TargetNodeID)
}

// joinMember adds the volume to the group, once.
func (r *PersistentVolumeOpsReconciler) joinMember(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	if !joinOf(ops).Joined {
		err := r.Join.JoinConsistencyGroupMember(ctx, subject.clusterUUID, joinOf(ops).GroupUUID, subject.handle.VolumeID)
		if errors.Is(err, controlplane.ErrMembershipRefused) {
			return "", false, joinFatalf(ReasonConsistencyGroupJoinRefused,
				"consistency group %s refused the volume: %v", ops.Spec.JoinConsistencyGroup.GroupName, err)
		}
		if err != nil {
			return "", false, err
		}
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			joinStatus(status).Joined = true
		}); err != nil {
			return "", false, err
		}
	}
	return stepSelectingSubsystem, true, nil
}

// joinSelect has the control plane choose and reserve the subsystem the group
// ends up in, then locks every volume the move touches and records the hosts
// that consume them.
func (r *PersistentVolumeOpsReconciler) joinSelect(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	if joinOf(ops).MoveID == "" && leaving(joinOf(ops)) {
		return r.selectLeave(ctx, ops, subject)
	}
	if joinOf(ops).MoveID == "" {
		move, err := r.Join.CreateSubsystemMove(ctx, subject.clusterUUID, joinOf(ops).GroupUUID,
			subject.handle.VolumeID, string(ops.UID))
		switch {
		case errors.Is(err, controlplane.ErrSubsystemLimitReached):
			return "", false, joinFatalf(ReasonSubsystemLimitReached,
				"the volume joined the group, and the group's node cannot hold the fresh subsystem the group needs: %v", err)
		case errors.Is(err, controlplane.ErrColocationRefused):
			return "", false, &joinEnded{
				message:    fmt.Sprintf("the volume joined the group and keeps its own subsystem: %v", err),
				eventType:  corev1.EventTypeWarning,
				reason:     ReasonConsistencyGroupColocationRefused,
				eventProse: fmt.Sprintf("The volume joined the group and keeps its own subsystem: %v", err),
			}
		case errors.Is(err, controlplane.ErrMoveConflict):
			return "", false, waitingf("the group is in another move or migration: %v", err)
		case err != nil:
			return "", false, err
		}
		if move.Empty() {
			return "", false, &joinEnded{
				message:    "the volume is in the group's subsystem; nothing was moved",
				colocated:  true,
				eventType:  corev1.EventTypeNormal,
				reason:     ReasonConsistencyGroupColocated,
				eventProse: "The volume is in its consistency group's subsystem",
			}
		}
		if err := r.recordMove(ctx, ops, move); err != nil {
			return "", false, err
		}
	}
	return r.lockAndFindHosts(ctx, ops, subject)
}

// recordMove writes a created move into status.
func (r *PersistentVolumeOpsReconciler) recordMove(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, move controlplane.SubsystemMove,
) error {
	volumes, err := r.movingVolumes(ctx, move)
	if err != nil {
		return err
	}
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		join := joinStatus(status)
		join.MoveID = move.ID
		join.TargetNQN = move.TargetNQN
		join.TargetIsNew = move.TargetIsNew
		join.Volumes = volumes
	})
}

// lockAndFindHosts locks every volume the recorded move touches and records the
// hosts consuming them.
func (r *PersistentVolumeOpsReconciler) lockAndFindHosts(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	if acquired, err := r.lockMove(ctx, ops, subject); err != nil || !acquired {
		return "", false, err
	}

	if joinOf(ops).Hosts == nil {
		hosts, err := r.consumingHosts(ctx, joinOf(ops).Volumes)
		if err != nil {
			return "", false, err
		}
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			joinStatus(status).Hosts = hosts
			status.DeferredSince = nil
		}); err != nil {
			return "", false, err
		}
	}
	return stepPreparingTarget, true, nil
}

// movingVolumes is the move's volumes as status records them, each with the
// PersistentVolume fronting it when there is one.
func (r *PersistentVolumeOpsReconciler) movingVolumes(
	ctx context.Context, move controlplane.SubsystemMove,
) ([]simplyblockv1alpha2.MovingVolume, error) {
	ids := map[string]struct{}{}
	for _, v := range move.Volumes {
		ids[v.LvolID] = struct{}{}
	}
	pvs, err := r.volumesFronting(ctx, ids)
	if err != nil {
		return nil, err
	}
	pvOf := map[string]string{}
	for _, pv := range pvs {
		if id, ok := volumeIDOf(pv); ok {
			pvOf[id] = pv.Name
		}
	}
	out := make([]simplyblockv1alpha2.MovingVolume, 0, len(move.Volumes))
	for _, v := range move.Volumes {
		out = append(out, simplyblockv1alpha2.MovingVolume{
			VolumeUUID:           v.LvolID,
			PersistentVolumeName: pvOf[v.LvolID],
			SourceNQN:            v.SourceNQN,
			TargetNSID:           int32(v.TargetNSID),
		})
	}
	return out, nil
}

// lockMove takes the lock on every volume of every subsystem the move touches:
// each source subsystem and, when it already exists, the target. All of them
// or none, in name order, as acquireLock does for one subsystem.
//
// A volume another operation holds leaves this one holding nothing, with the
// move it reserved still reserved: the reservation is what keeps the slots,
// and the lock is what keeps a sibling's migration from moving a subsystem
// under the swap.
func (r *PersistentVolumeOpsReconciler) lockMove(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (bool, error) {
	join := joinOf(ops)
	nqns := map[string]struct{}{}
	for _, v := range join.Volumes {
		nqns[v.SourceNQN] = struct{}{}
	}
	if !join.TargetIsNew {
		nqns[join.TargetNQN] = struct{}{}
	}
	ids := map[string]struct{}{}
	for _, v := range join.Volumes {
		ids[v.VolumeUUID] = struct{}{}
	}
	for nqn := range nqns {
		if nqn == "" {
			continue
		}
		members, err := r.API.SubsystemVolumes(ctx, subject.clusterUUID, nqn)
		if err != nil {
			return false, fmt.Errorf("list the volumes of subsystem %s: %w", nqn, err)
		}
		for _, m := range members {
			if id, ok := lvolIDOf(m.ID); ok {
				ids[id] = struct{}{}
			}
		}
	}
	volumes, err := r.volumesFronting(ctx, ids)
	if err != nil {
		return false, err
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })

	for _, volume := range volumes {
		held := volume.Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]
		if held == ops.Name {
			continue
		}
		if held != "" {
			takeable, err := r.lockIsStale(ctx, held)
			if err != nil {
				return false, errors.Join(err, r.releaseLock(ctx, ops))
			}
			if !takeable {
				return false, r.deferSwap(ctx, ops, subject, held, volume.Name)
			}
		}
		taken, err := r.lockVolume(ctx, ops, volume)
		if err != nil || !taken {
			return false, errors.Join(err, r.releaseLock(ctx, ops))
		}
	}
	return true, nil
}

// deferSwap gives back every lock this pass took and reports the wait.
func (r *PersistentVolumeOpsReconciler) deferSwap(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, holder, volume string,
) error {
	if err := r.releaseLock(ctx, ops); err != nil {
		return err
	}
	r.event(ops, corev1.EventTypeNormal, ReasonSubsystemSwapDeferred,
		"Volume %s, which the subsystem move touches, is held by operation %s; the swap waits", volume, holder)
	r.claimEvent(ctx, subject.pv, corev1.EventTypeNormal, ReasonSubsystemSwapDeferred,
		"The move into the group's subsystem waits for operation %s, which holds volume %s", holder, volume)
	if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		if status.DeferredSince == nil {
			now := metav1.Now()
			status.DeferredSince = &now
		}
	}); err != nil {
		return err
	}
	return waitingf("operation %s holds volume %s", holder, volume)
}

// consumingHosts is every node a moving volume is attached to, with the moving
// volumes it consumes, sorted. A volume with no PersistentVolume or no
// attachment has no consumer to hold.
func (r *PersistentVolumeOpsReconciler) consumingHosts(
	ctx context.Context, volumes []simplyblockv1alpha2.MovingVolume,
) ([]simplyblockv1alpha2.SwapHost, error) {
	moving := map[string]bool{}
	for _, v := range volumes {
		if v.PersistentVolumeName != "" {
			moving[v.PersistentVolumeName] = true
		}
	}
	var attachments storagev1.VolumeAttachmentList
	if err := r.Reader.List(ctx, &attachments); err != nil {
		return nil, fmt.Errorf("list the volume attachments: %w", err)
	}
	byNode := map[string][]string{}
	for _, va := range attachments.Items {
		pv := va.Spec.Source.PersistentVolumeName
		if pv == nil || !moving[*pv] || va.Spec.NodeName == "" || !va.DeletionTimestamp.IsZero() {
			continue
		}
		if !slices.Contains(byNode[va.Spec.NodeName], *pv) {
			byNode[va.Spec.NodeName] = append(byNode[va.Spec.NodeName], *pv)
		}
	}
	hosts := make([]simplyblockv1alpha2.SwapHost, 0, len(byNode))
	for node, pvs := range byNode {
		sort.Strings(pvs)
		hosts = append(hosts, simplyblockv1alpha2.SwapHost{Node: node, PersistentVolumes: pvs})
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Node < hosts[j].Node })
	return hosts, nil
}

// joinPrepare creates the target on every node of the group when it is new.
func (r *PersistentVolumeOpsReconciler) joinPrepare(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	join := joinOf(ops)
	if _, err := r.Join.PrepareSubsystemMove(ctx, subject.clusterUUID, moveGroupOf(join), join.MoveID, ""); err != nil {
		if errors.Is(err, errs.ErrAlreadyExists) || errors.Is(err, errs.ErrNotFound) {
			return "", false, rollbackf("the control plane refused to prepare the target: %v", err)
		}
		return "", false, err
	}
	if len(join.Hosts) == 0 {
		return stepMovingNamespaces, true, nil
	}
	return stepJoinAttachingPaths, true, nil
}

// joinCommit records every moved volume in its new subsystem.
func (r *PersistentVolumeOpsReconciler) joinCommit(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (step, bool, error) {
	join := joinOf(ops)
	if err := r.Join.CommitSubsystemMove(ctx, subject.clusterUUID, moveGroupOf(join), join.MoveID); err != nil {
		return "", false, err
	}
	if len(join.Hosts) == 0 {
		return afterSwap(join), true, nil
	}
	return stepReleasingPaths, true, nil
}

// afterSwap is the step after a swap that settled: the rest of the leave when
// the swap split the volume out of the group it leaves, and the end of the
// operation otherwise.
func afterSwap(join simplyblockv1alpha2.JoinConsistencyGroupStatus) step {
	if join.RollbackReason == "" && leaving(join) {
		return stepLeaving
	}
	return ""
}

// endJoin ends an operation a step finished early.
func (r *PersistentVolumeOpsReconciler) endJoin(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, ended *joinEnded,
) (ctrl.Result, error) {
	r.event(ops, ended.eventType, ended.reason, "%s", ended.eventProse)
	r.claimEvent(ctx, subject.pv, ended.eventType, ended.reason, "%s", ended.eventProse)
	return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded, ended.message)
}

// finishJoin ends an operation whose last step completed: Succeeded when the
// namespaces are in the target, Failed with the rollback's reason when they
// were taken back.
func (r *PersistentVolumeOpsReconciler) finishJoin(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, note string,
) (ctrl.Result, error) {
	join := joinOf(ops)
	if join.RollbackReason != "" {
		cgSubsystemMoves.WithLabelValues(subject.clusterUUID, "rolled_back").Inc()
		message := "the move into the group's subsystem was rolled back: " + join.RollbackReason
		r.event(ops, corev1.EventTypeWarning, ReasonOperationFailed, "%s", message)
		return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed, message)
	}

	cgSubsystemMoves.WithLabelValues(subject.clusterUUID, "succeeded").Inc()
	cgSubsystemMoveNamespaces.WithLabelValues(subject.clusterUUID).Add(float64(len(join.Volumes)))
	// The move names what it moved, not what the target already held, so the
	// message counts namespaces moved rather than members present.
	target := "existing"
	if join.TargetIsNew {
		target = "new"
	}
	message := fmt.Sprintf("%d namespace(s) moved into the group's %s subsystem %s", len(join.Volumes), target, join.TargetNQN)
	if note != "" {
		message += "; " + note
	}
	r.event(ops, corev1.EventTypeNormal, ReasonConsistencyGroupColocated, "%s", message)
	r.claimEvent(ctx, subject.pv, corev1.EventTypeNormal, ReasonConsistencyGroupColocated,
		"The volume is in consistency group %s's subsystem %s", ops.Spec.JoinConsistencyGroup.GroupName, join.TargetNQN)
	return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded, message)
}

// failJoin ends an operation that failed before anything was moved or
// reserved, aborting the child migration it created.
func (r *PersistentVolumeOpsReconciler) failJoin(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, reason, message string,
) (ctrl.Result, error) {
	if join := joinOf(ops); join.MoveID != "" {
		if err := r.Join.RollbackSubsystemMove(ctx, subject.clusterUUID, moveGroupOf(join), join.MoveID); err != nil {
			return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops,
				fmt.Sprintf("%s; releasing the reserved move: %v", message, err))
		}
	}
	if err := r.abortChild(ctx, ops); err != nil {
		logf.FromContext(ctx).Error(err, "the child migration could not be aborted", "operation", ops.Name)
	}
	r.event(ops, corev1.EventTypeWarning, reason, "%s", message)
	r.claimEvent(ctx, subject.pv, corev1.EventTypeWarning, reason, "%s", message)
	return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed, message)
}

// abortChild asks the child migration to stop, and reports whether it has.
func (r *PersistentVolumeOpsReconciler) abortChild(ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps) error {
	_, err := r.childStopped(ctx, ops)
	return err
}

// childStopped asks the child migration, if there is one, to abort, and reports
// whether it is terminal.
func (r *PersistentVolumeOpsReconciler) childStopped(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (bool, error) {
	name := joinOf(ops).MigrationName
	if name == "" {
		return true, nil
	}
	var child simplyblockv1alpha2.PersistentVolumeOps
	err := r.Get(ctx, types.NamespacedName{Name: name}, &child)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the migration %s: %w", name, err)
	}
	if terminal(child.Status.Phase) {
		return true, nil
	}
	if !child.Spec.Abort {
		patch := client.MergeFrom(child.DeepCopy())
		child.Spec.Abort = true
		if err := r.Patch(ctx, &child, patch); err != nil {
			return false, fmt.Errorf("abort the migration %s: %w", name, err)
		}
	}
	return false, nil
}

// unwindJoin honors an abort from a step that has nothing on the hosts' data
// path yet: the child migration is stopped, the reserved move is released, and
// prepared paths are disconnected.
func (r *PersistentVolumeOpsReconciler) unwindJoin(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, current step,
) (ctrl.Result, error) {
	stopped, err := r.childStopped(ctx, ops)
	if err != nil {
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, err.Error())
	}
	if !stopped {
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, "the abort waits for the migration to stop")
	}
	if join := joinOf(ops); join.MoveID != "" {
		if err := r.Join.RollbackSubsystemMove(ctx, subject.clusterUUID, moveGroupOf(join), join.MoveID); err != nil {
			return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops,
				fmt.Sprintf("the abort waits for the reserved move to be released: %v", err))
		}
		r.releasePrepared(ctx, ops)
	}
	r.event(ops, corev1.EventTypeNormal, ReasonOperationAborted, "The operation was aborted at step %s and unwound", current)
	return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted, fmt.Sprintf("aborted at step %s", current))
}

// releasePrepared disconnects the target from every host that was given its
// paths. Best effort: a host that cannot be reached keeps an idle controller,
// which its own teardown removes, and holding the operation open for it would
// hold the volumes' locks for nothing.
func (r *PersistentVolumeOpsReconciler) releasePrepared(ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps) {
	join := joinOf(ops)
	for _, host := range join.Hosts {
		if !host.Prepared {
			continue
		}
		node, _, err := r.reach(host.Node)
		if err == nil {
			_, err = node.Release(ctx, join.TargetNQN)
		}
		if err != nil {
			logf.FromContext(ctx).Error(err, "the target could not be released", "node", host.Node)
		}
	}
}

// teardownJoin finishes or takes back a join deleted while it ran: a move whose
// namespaces are in the target is committed, any other is rolled back, and
// every host it held is resumed on the subsystem the namespaces are in.
func (r *PersistentVolumeOpsReconciler) teardownJoin(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (ctrl.Result, error) {
	if _, err := r.childStopped(ctx, ops); err != nil {
		logf.FromContext(ctx).Error(err, "the child migration could not be aborted", "operation", ops.Name)
	}
	join := joinOf(ops)
	if join.MoveID != "" && r.Join != nil {
		cluster, err := r.clusterOfVolume(ctx, ops)
		if err != nil {
			return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, err.Error())
		}
		toTarget := join.NamespacesAt == simplyblockv1alpha2.SubsystemSideTarget && join.RollbackReason == ""
		if toTarget {
			err = r.Join.CommitSubsystemMove(ctx, cluster, moveGroupOf(join), join.MoveID)
		} else {
			err = r.Join.RollbackSubsystemMove(ctx, cluster, moveGroupOf(join), join.MoveID)
		}
		if err != nil {
			r.event(ops, corev1.EventTypeWarning, ReasonCleanupBlocked,
				"The delete is held until the subsystem move is settled: %v", err)
			return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops,
				fmt.Sprintf("the delete is held until the subsystem move is settled: %v", err))
		}
		r.resumeEveryHost(ctx, ops, toTarget)
		if !toTarget {
			r.releasePrepared(ctx, ops)
		}
	}
	if err := r.releaseLock(ctx, ops); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(ops, opsFinalizer)
	return ctrl.Result{}, r.Update(ctx, ops)
}

// clusterOfVolume is the volume's cluster, read from its handle, for a pass
// that has no resolved subject.
func (r *PersistentVolumeOpsReconciler) clusterOfVolume(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (string, error) {
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.PersistentVolumeName}, &pv); err != nil {
		return "", fmt.Errorf("read volume %s for its cluster: %w", ops.Spec.PersistentVolumeName, err)
	}
	handle, err := r.addressOf(ctx, &pv)
	if err != nil {
		return "", err
	}
	return handle.ClusterID, nil
}

// claimEvent records an event on the claim of the volume, which is the object
// the user labeled and watches. A volume with no claim records nothing.
func (r *PersistentVolumeOpsReconciler) claimEvent(
	_ context.Context, pv *corev1.PersistentVolume, eventType, reason, format string, args ...any,
) {
	if r.Recorder == nil || pv == nil || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Name == "" {
		return
	}
	ref := pv.Spec.ClaimRef
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: ref.Name, Namespace: ref.Namespace, UID: ref.UID,
	}}
	r.Recorder.Eventf(claim, nil, eventType, reason, reason, format, args...)
}

// claimOf names a volume's claim as namespace/name, or the volume's own name
// when it has none.
func claimOf(pv *corev1.PersistentVolume) string {
	if pv == nil {
		return ""
	}
	if ref := pv.Spec.ClaimRef; ref != nil && ref.Name != "" {
		return ref.Namespace + "/" + ref.Name
	}
	return pv.Name
}
