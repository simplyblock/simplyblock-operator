// volumegroupsnapshotops_controller.go reconciles a VolumeGroupSnapshotOps
// (design-consistency-groups.md §7.4): the Restore action creates one
// PersistentVolumeClaim per member snapshot of the target VolumeGroupSnapshot's
// generation and waits for every claim to bind. The restore composes the
// per-member dataSource path. There is no new CSI verb and no backend call.
package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	volumegroupsnapshotv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumegroupsnapshot/v1beta1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

const (
	// restoreOpsLabel marks a claim as created by a named VolumeGroupSnapshotOps,
	// which is what distinguishes an idempotent re-create from a name collision
	// with a claim the operation does not own.
	restoreOpsLabel = "storage.simplyblock.io/volume-group-snapshot-ops"
	// consistencyGroupLabel is the membership label a restored claim carries when
	// restore.consistencyGroup asks the clones to form a new group (design §7.2).
	consistencyGroupLabel = "storage.simplyblock.io/consistency-group"

	// groupRestoreTargetRequeueInterval paces re-checks of a target that exists
	// but is not yet ReadyToUse.
	groupRestoreTargetRequeueInterval = 15 * time.Second
	// groupRestoreBindRequeueInterval paces the wait for restored claims to
	// bind. The claim watch wakes the reconcile earlier on a bind event.
	groupRestoreBindRequeueInterval = 10 * time.Second

	eventReasonGroupRestoreBlocked  = "RestoreBlocked"
	eventReasonGroupRestoreComplete = "RestoreComplete"
)

// VolumeGroupSnapshotOpsReconciler drives a VolumeGroupSnapshotOps to
// completion.
type VolumeGroupSnapshotOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumegroupsnapshotops,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumegroupsnapshotops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=groupsnapshot.storage.k8s.io,resources=volumegroupsnapshots,verbs=get;list;watch
// +kubebuilder:rbac:groups=groupsnapshot.storage.k8s.io,resources=volumegroupsnapshotcontents,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create

// step aliases keep the graph literal readable.
type restoreStep = simplyblockv1alpha1.VolumeGroupSnapshotOpsStep

const (
	stepValidating     = simplyblockv1alpha1.VolumeGroupSnapshotOpsStepValidating
	stepCreatingClaims = simplyblockv1alpha1.VolumeGroupSnapshotOpsStepCreatingClaims
	stepWaitingForBind = simplyblockv1alpha1.VolumeGroupSnapshotOpsStepWaitingForBind
)

// stepGraphs declares one state graph per action (design-crd-model.md §3.1):
// the graph, not the step enum, is what ties a step to its action.
func stepGraphs() statemachine.MultiConfig[restoreStep] {
	return statemachine.MultiConfig[restoreStep]{
		statemachine.Action(simplyblockv1alpha1.VolumeGroupSnapshotOpsActionRestore): {
			Initial: stepValidating,
			States: map[restoreStep]statemachine.StateDef[restoreStep]{
				stepValidating:     {To: []restoreStep{stepCreatingClaims}},
				stepCreatingClaims: {To: []restoreStep{stepWaitingForBind}},
				// Completion is a phase flip, not a step: the phase turns
				// terminal while the machine rests here.
				stepWaitingForBind: {},
			},
		},
	}
}

func (r *VolumeGroupSnapshotOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ops := &simplyblockv1alpha1.VolumeGroupSnapshotOps{}
	if err := r.Get(ctx, req.NamespacedName, ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A terminal operation re-reconciles to nothing.
	switch ops.Status.Phase {
	case simplyblockv1alpha1.VolumeGroupSnapshotOpsPhaseSucceeded,
		simplyblockv1alpha1.VolumeGroupSnapshotOpsPhaseFailed:
		return ctrl.Result{}, nil
	}

	graph, known := stepGraphs()[statemachine.Action(ops.Spec.Action)]
	if !known {
		// Admission's enum rejects unknown actions. An older CRD may not carry it.
		return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf("unknown action %q", ops.Spec.Action))
	}
	snap := statemachine.Snapshot[restoreStep]{State: ops.Status.Step.State}
	if ops.Status.Step.Deadline != nil {
		snap.Deadline = ops.Status.Step.Deadline.Time
	}
	machine, err := statemachine.NewFromSnapshot(ctx, graph, snap)
	if err != nil {
		// An unrecognized step: a downgrade, or a hand-edited resource.
		return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf("unrecognized step %q", ops.Status.Step.State))
	}
	defer machine.Close()

	result, err := r.advance(ctx, ops, machine)
	if err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

// advance performs at most one step transition per pass, so every position the
// operation can be interrupted in is readable from status.
func (r *VolumeGroupSnapshotOpsReconciler) advance(
	ctx context.Context,
	ops *simplyblockv1alpha1.VolumeGroupSnapshotOps,
	machine *statemachine.Machine[restoreStep],
) (ctrl.Result, error) {
	switch machine.CurrentState() {
	case stepValidating:
		return r.validate(ctx, ops, machine)
	case stepCreatingClaims:
		return r.createClaims(ctx, ops, machine)
	case stepWaitingForBind:
		return r.waitForBind(ctx, ops)
	}
	return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf("unhandled step %q", machine.CurrentState()))
}

// validate resolves the target, waits for its readiness, enumerates the member
// snapshots, applies the incomplete-generation gate, and records the durable
// claim plan in status before any claim is created (write ahead of the side
// effect).
func (r *VolumeGroupSnapshotOpsReconciler) validate(
	ctx context.Context,
	ops *simplyblockv1alpha1.VolumeGroupSnapshotOps,
	machine *statemachine.Machine[restoreStep],
) (ctrl.Result, error) {
	vgs := &volumegroupsnapshotv1beta1.VolumeGroupSnapshot{}
	err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.VolumeGroupSnapshotRef, Namespace: ops.Namespace}, vgs)
	if kerrors.IsNotFound(err) {
		// Admission resolved the reference, so the target was deleted after it.
		return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf(
			"VolumeGroupSnapshot %q no longer exists", ops.Spec.VolumeGroupSnapshotRef))
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if vgs.Status == nil || vgs.Status.ReadyToUse == nil || !*vgs.Status.ReadyToUse {
		if err := r.block(ctx, ops, fmt.Sprintf(
			"VolumeGroupSnapshot %q is not ReadyToUse yet", vgs.Name)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: groupRestoreTargetRequeueInterval}, nil
	}

	members, err := r.memberSnapshots(ctx, ops.Namespace, vgs.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	expected := r.expectedMembers(ctx, vgs, len(members))
	if len(members) < expected && !restoreSpec(ops).EnablePartialRestore {
		return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf(
			"generation incomplete: %d of %d member snapshots present; "+
				"set restore.enablePartialRestore to restore what remains", len(members), expected))
	}

	prefix := restoreSpec(ops).NamePrefix
	if prefix == "" {
		prefix = ops.Name
	}
	plan := make([]simplyblockv1alpha1.RestoredMemberStatus, 0, len(members))
	for _, member := range members {
		source := ""
		if member.Spec.Source.PersistentVolumeClaimName != nil {
			source = *member.Spec.Source.PersistentVolumeClaimName
		}
		if source == "" {
			return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf(
				"member snapshot %q names no source PersistentVolumeClaim", member.Name))
		}
		plan = append(plan, simplyblockv1alpha1.RestoredMemberStatus{
			VolumeSnapshotName:        member.Name,
			PersistentVolumeClaimName: prefix + "-" + source,
		})
	}

	if err := machine.TransitionTo(ctx, stepCreatingClaims); err != nil {
		return ctrl.Result{}, err
	}
	err = r.writeStatus(ctx, ops, func(status *simplyblockv1alpha1.VolumeGroupSnapshotOpsStatus) {
		status.Phase = simplyblockv1alpha1.VolumeGroupSnapshotOpsPhaseRunning
		status.Message = fmt.Sprintf("restoring %d member snapshot(s)", len(plan))
		status.MembersExpected = expected
		status.Members = plan
		if status.StartedAt == nil {
			status.StartedAt = &metav1.Time{Time: time.Now()}
		}
		setStep(status, machine)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// createClaims creates the planned claims. Creation is idempotent through the
// ownership label: a claim that exists and carries this operation's label was
// created by an earlier pass. One without it is a name collision and fails the
// operation, leaving claims already created in place (design §7.4).
func (r *VolumeGroupSnapshotOpsReconciler) createClaims(
	ctx context.Context,
	ops *simplyblockv1alpha1.VolumeGroupSnapshotOps,
	machine *statemachine.Machine[restoreStep],
) (ctrl.Result, error) {
	for _, member := range ops.Status.Members {
		existing := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, types.NamespacedName{Name: member.PersistentVolumeClaimName, Namespace: ops.Namespace}, existing)
		if err == nil {
			if existing.Labels[restoreOpsLabel] != ops.Name {
				return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf(
					"claim %q already exists and was not created by this restore", member.PersistentVolumeClaimName))
			}
			continue
		}
		if !kerrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		claim, buildErr := r.buildClaim(ctx, ops, member)
		if buildErr != nil {
			return ctrl.Result{}, r.fail(ctx, ops, buildErr.Error())
		}
		if err := r.Create(ctx, claim); err != nil && !kerrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
	}

	if err := machine.TransitionTo(ctx, stepWaitingForBind); err != nil {
		return ctrl.Result{}, err
	}
	err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha1.VolumeGroupSnapshotOpsStatus) {
		status.Message = fmt.Sprintf("waiting for %d claim(s) to bind", len(ops.Status.Members))
		setStep(status, machine)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// waitForBind counts bound claims and turns the phase terminal once every
// planned claim is bound. The claim watch wakes the reconcile on bind events,
// and the requeue is the fallback.
func (r *VolumeGroupSnapshotOpsReconciler) waitForBind(
	ctx context.Context,
	ops *simplyblockv1alpha1.VolumeGroupSnapshotOps,
) (ctrl.Result, error) {
	bound := 0
	members := make([]simplyblockv1alpha1.RestoredMemberStatus, len(ops.Status.Members))
	copy(members, ops.Status.Members)
	for i := range members {
		claim := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, types.NamespacedName{Name: members[i].PersistentVolumeClaimName, Namespace: ops.Namespace}, claim)
		if err != nil && !kerrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		members[i].Bound = err == nil && claim.Status.Phase == corev1.ClaimBound
		if members[i].Bound {
			bound++
		}
	}

	complete := bound == len(members) && len(members) > 0
	err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha1.VolumeGroupSnapshotOpsStatus) {
		status.Members = members
		status.MembersBound = bound
		if complete {
			status.Phase = simplyblockv1alpha1.VolumeGroupSnapshotOpsPhaseSucceeded
			status.Message = fmt.Sprintf("restored %d member(s)", bound)
			status.CompletedAt = &metav1.Time{Time: time.Now()}
		} else {
			status.Message = fmt.Sprintf("%d of %d claim(s) bound", bound, len(members))
		}
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if complete {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, eventReasonGroupRestoreComplete,
			eventReasonGroupRestoreComplete, "restored %d member(s) from VolumeGroupSnapshot %q",
			bound, ops.Spec.VolumeGroupSnapshotRef)
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: groupRestoreBindRequeueInterval}, nil
}

// buildClaim assembles one restored claim: dataSource the member snapshot,
// class and size from the explicit spec, the source claim, or the snapshot's
// restore size, in that order.
func (r *VolumeGroupSnapshotOpsReconciler) buildClaim(
	ctx context.Context,
	ops *simplyblockv1alpha1.VolumeGroupSnapshotOps,
	member simplyblockv1alpha1.RestoredMemberStatus,
) (*corev1.PersistentVolumeClaim, error) {
	snapshot := &snapshotv1.VolumeSnapshot{}
	if err := r.Get(ctx, types.NamespacedName{Name: member.VolumeSnapshotName, Namespace: ops.Namespace}, snapshot); err != nil {
		return nil, fmt.Errorf("member snapshot %q is gone: %w", member.VolumeSnapshotName, err)
	}

	accessModes := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	var storageClass *string
	if name := restoreSpec(ops).StorageClassName; name != "" {
		storageClass = &name
	}
	var request *corev1.ResourceList
	if snapshot.Spec.Source.PersistentVolumeClaimName != nil {
		source := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, types.NamespacedName{
			Name: *snapshot.Spec.Source.PersistentVolumeClaimName, Namespace: ops.Namespace,
		}, source)
		if err == nil {
			if len(source.Spec.AccessModes) > 0 {
				accessModes = source.Spec.AccessModes
			}
			if storageClass == nil {
				storageClass = source.Spec.StorageClassName
			}
			request = &source.Spec.Resources.Requests
		} else if !kerrors.IsNotFound(err) {
			return nil, err
		}
	}
	if storageClass == nil {
		return nil, fmt.Errorf(
			"claim %q has no storage class: the source claim is gone and restore.storageClassName is empty",
			member.PersistentVolumeClaimName)
	}
	if request == nil {
		if snapshot.Status == nil || snapshot.Status.RestoreSize == nil {
			return nil, fmt.Errorf(
				"claim %q has no size: the source claim is gone and snapshot %q reports no restore size",
				member.PersistentVolumeClaimName, member.VolumeSnapshotName)
		}
		request = &corev1.ResourceList{corev1.ResourceStorage: *snapshot.Status.RestoreSize}
	}

	labels := map[string]string{restoreOpsLabel: ops.Name}
	if group := restoreSpec(ops).ConsistencyGroup; group != "" {
		labels[consistencyGroupLabel] = group
	}
	apiGroup := snapshotv1.GroupName
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      member.PersistentVolumeClaimName,
			Namespace: ops.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      accessModes,
			StorageClassName: storageClass,
			Resources:        corev1.VolumeResourceRequirements{Requests: *request},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     member.VolumeSnapshotName,
			},
		},
	}, nil
}

// memberSnapshots lists the VolumeSnapshots the snapshot-controller
// materialized for the target, backref'd by status.volumeGroupSnapshotName
// (design §5.3), in a stable name order.
func (r *VolumeGroupSnapshotOpsReconciler) memberSnapshots(
	ctx context.Context, namespace, vgsName string,
) ([]snapshotv1.VolumeSnapshot, error) {
	var list snapshotv1.VolumeSnapshotList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var members []snapshotv1.VolumeSnapshot
	for _, snapshot := range list.Items {
		if snapshot.Status != nil && snapshot.Status.VolumeGroupSnapshotName != nil &&
			*snapshot.Status.VolumeGroupSnapshotName == vgsName {
			members = append(members, snapshot)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	return members, nil
}

// expectedMembers is the member count of the generation at the take: the
// content's source volume handles, recorded when the group snapshot was taken.
// A content that cannot be read leaves the found count as the expectation, so
// an unreadable content never fails a complete restore.
func (r *VolumeGroupSnapshotOpsReconciler) expectedMembers(
	ctx context.Context, vgs *volumegroupsnapshotv1beta1.VolumeGroupSnapshot, found int,
) int {
	if vgs.Status == nil || vgs.Status.BoundVolumeGroupSnapshotContentName == nil {
		return found
	}
	content := &volumegroupsnapshotv1beta1.VolumeGroupSnapshotContent{}
	err := r.Get(ctx, types.NamespacedName{Name: *vgs.Status.BoundVolumeGroupSnapshotContentName}, content)
	if err != nil || len(content.Spec.Source.VolumeHandles) == 0 {
		return found
	}
	return len(content.Spec.Source.VolumeHandles)
}

// restoreSpec returns the restore parameters, defaulted when the block is
// absent.
func restoreSpec(ops *simplyblockv1alpha1.VolumeGroupSnapshotOps) simplyblockv1alpha1.RestoreOpsSpec {
	if ops.Spec.Restore == nil {
		return simplyblockv1alpha1.RestoreOpsSpec{}
	}
	return *ops.Spec.Restore
}

// setStep persists the machine's position, deadline included, so a restart
// restores exactly where the operation stood.
func setStep(status *simplyblockv1alpha1.VolumeGroupSnapshotOpsStatus, machine *statemachine.Machine[restoreStep]) {
	snap := machine.Snapshot()
	status.Step = simplyblockv1alpha1.VolumeGroupSnapshotOpsStepSnapshot{State: snap.State}
	if !snap.Deadline.IsZero() {
		status.Step.Deadline = &metav1.Time{Time: snap.Deadline}
	}
}

// block parks the operation in Pending with the reason, evented once per
// distinct message so a held restore is visible without spamming.
func (r *VolumeGroupSnapshotOpsReconciler) block(
	ctx context.Context, ops *simplyblockv1alpha1.VolumeGroupSnapshotOps, message string,
) error {
	if ops.Status.Message != message {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, eventReasonGroupRestoreBlocked,
			eventReasonGroupRestoreBlocked, "%s", message)
	}
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha1.VolumeGroupSnapshotOpsStatus) {
		status.Phase = simplyblockv1alpha1.VolumeGroupSnapshotOpsPhasePending
		status.Message = message
	})
}

// fail turns the operation terminally Failed with the reason. A failure is a
// permanent verdict, so it is recorded and evented rather than returned as an
// error to retry.
func (r *VolumeGroupSnapshotOpsReconciler) fail(
	ctx context.Context, ops *simplyblockv1alpha1.VolumeGroupSnapshotOps, message string,
) error {
	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, eventReasonGroupRestoreBlocked,
		eventReasonGroupRestoreBlocked, "%s", message)
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha1.VolumeGroupSnapshotOpsStatus) {
		status.Phase = simplyblockv1alpha1.VolumeGroupSnapshotOpsPhaseFailed
		status.Message = message
		status.CompletedAt = &metav1.Time{Time: time.Now()}
	})
}

// writeStatus applies the mutation to a fresh copy under optimistic
// concurrency, so a conflicting writer is re-read rather than overwritten, and
// stamps observedGeneration on every write.
func (r *VolumeGroupSnapshotOpsReconciler) writeStatus(
	ctx context.Context,
	ops *simplyblockv1alpha1.VolumeGroupSnapshotOps,
	mutate func(*simplyblockv1alpha1.VolumeGroupSnapshotOpsStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &simplyblockv1alpha1.VolumeGroupSnapshotOps{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(ops), fresh); err != nil {
			return err
		}
		mutate(&fresh.Status)
		fresh.Status.ObservedGeneration = fresh.Generation
		if err := r.Status().Update(ctx, fresh); err != nil {
			return err
		}
		ops.Status = fresh.Status
		return nil
	})
}

// SetupWithManager registers the reconciler, waking it on restored-claim
// events through the ownership label as well as on the operation itself.
func (r *VolumeGroupSnapshotOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.VolumeGroupSnapshotOps{}).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, object client.Object) []ctrl.Request {
				owner := object.GetLabels()[restoreOpsLabel]
				if owner == "" {
					return nil
				}
				return []ctrl.Request{{NamespacedName: types.NamespacedName{
					Name: owner, Namespace: object.GetNamespace(),
				}}}
			})).
		Complete(r)
}
