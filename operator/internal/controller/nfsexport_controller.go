// NFSExportReconciler drives one pNFS export from creation to Ready and back
// out again: it picks the MDS host, asks that host to assemble the export, and
// is the only writer of the binding that says which host may have the
// filesystem mounted.
//
// The binding is the point. A single XFS can be mounted by exactly one node, so
// status.storageNodeRef is a mutual-exclusion field rather than a label: no
// second host becomes a candidate until this controller rewrites it, under
// optimistic concurrency. Getting that wrong destroys data rather than degrading
// service, which is why the field is in status where a user edit cannot race it.
//
// The phases are a declared graph rather than a switch over strings, so an
// illegal jump is an error at the transition instead of silently skipped work,
// and the position survives a restart through the snapshot in status.

package controller

import (
	"context"
	"fmt"
	"time"

	"hash/fnv"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// phase is an alias so the graph below reads as a table.
type phase = simplyblockv1alpha2.NFSExportPhase

const (
	phasePending     = simplyblockv1alpha2.NFSExportPhasePending
	phaseAssembling  = simplyblockv1alpha2.NFSExportPhaseAssembling
	phaseReady       = simplyblockv1alpha2.NFSExportPhaseReady
	phaseFailingOver = simplyblockv1alpha2.NFSExportPhaseFailingOver
	phaseDegraded    = simplyblockv1alpha2.NFSExportPhaseDegraded
	phaseDeleting    = simplyblockv1alpha2.NFSExportPhaseDeleting
)

// How long each wait lasts, named rather than written inline at the return so
// the controller's cadence can be read in one place and tuned.
const (
	// nfsExportNoHostRequeue is how long to wait when no host is eligible. It
	// is deliberately slower than the others: the condition is usually a
	// cluster that has not finished coming up, and polling it hard helps
	// nothing.
	nfsExportNoHostRequeue = 30 * time.Second
	// nfsExportNoSessionRequeue is how long to wait when the bound host has no
	// live csi-link session. A node is legitimately disconnected during a
	// rollout, so this is a normal state rather than a failure.
	nfsExportNoSessionRequeue = 10 * time.Second
	// nfsExportAssembleDeadline bounds how long an export may sit in
	// Assembling before the phase is given up on. Assembly is a filesystem
	// make and a mount on one host; minutes are generous.
	nfsExportAssembleDeadline = 5 * time.Minute
	// nfsExportFreshCopyRequeue comes back with a re-read object after a write
	// that bumped resourceVersion. Nothing is being waited on, so it is as
	// short as a named interval sensibly gets: the next pass exists only so
	// that status is not written over a copy this one already made stale.
	nfsExportFreshCopyRequeue = time.Second
)

// ExportAssembler is the host-side half of an export: the operations this
// controller drives on the MDS host over csi-link.
//
// It is an interface because the controller is complete without the transport
// and must be testable without a node. The implementation that reaches a real
// host lands with the csi-link export service; until then a reconcile against a
// host that cannot be reached is a requeue, which is the same thing the
// controller does for a node that is merely disconnected.
type ExportAssembler interface {
	// CreateExport assembles the export on the named node: attach the
	// namespace, make or find the filesystem, mount it, and publish it. It is
	// idempotent, because a reconcile that died mid-assembly will call it
	// again.
	//
	// It returns the NGUID the host observed on the namespace, which is the
	// only place that value can come from: the target assigns it and a host
	// reads it off the device, so neither provisioning nor this controller
	// knows it. An empty string means the host could not report one, which is
	// not an assembly failure.
	CreateExport(ctx context.Context, nodeName string, export *simplyblockv1alpha2.NFSExport) (string, error)
	// DeleteExport tears the export down on the named node in reverse order. It
	// is idempotent and treats an already-absent export as success, because the
	// finalizer path has to converge.
	DeleteExport(ctx context.Context, nodeName string, export *simplyblockv1alpha2.NFSExport) error
	// HasSession reports whether the named node is currently reachable. It
	// distinguishes "not connected right now," which is a requeue, from an
	// operation that failed, which is not.
	HasSession(nodeName string) bool
}

// NFSExportReconciler reconciles NFSExport objects.
type NFSExportReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Assembler performs the host-side work. A nil Assembler makes every
	// assembly a requeue rather than a panic, which is what lets the kind be
	// deployed before its transport exists.
	Assembler ExportAssembler
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=nfsexports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=nfsexports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=nfsexports/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one export toward Ready, or tears it down.
func (r *NFSExportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("nfsexport", req.NamespacedName)

	var export simplyblockv1alpha2.NFSExport
	if err := r.Get(ctx, req.NamespacedName, &export); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !export.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &export)
	}

	// A terminal phase re-reconciles to nothing. Degraded is terminal for this
	// controller on purpose: it is reached by refusing to act, and retrying the
	// same refusal on a timer would bury the event that explains it.
	if export.Status.Phase == phaseDegraded {
		return ctrl.Result{}, nil
	}

	if added, err := r.ensureFinalizer(ctx, &export); err != nil {
		return ctrl.Result{}, err
	} else if added {
		// The finalizer write bumped resourceVersion; come back with a fresh
		// copy rather than writing status over a stale one.
		return ctrl.Result{RequeueAfter: nfsExportFreshCopyRequeue}, nil
	}

	machine, err := r.restore(ctx, &export)
	if err != nil {
		// An unrecognized phase is a downgrade or a hand-edited resource, and
		// guessing which state it meant is worse than stopping.
		r.event(&export, corev1.EventTypeWarning, "PhaseUnrecognized",
			fmt.Sprintf("cannot restore phase %q", export.Status.Phase))
		return ctrl.Result{}, err
	}
	defer machine.Close()

	switch machine.CurrentState() {
	case phasePending:
		return r.reconcilePending(ctx, &export, machine, logger)
	case phaseAssembling:
		return r.reconcileAssembling(ctx, &export, machine, logger)
	case phaseReady:
		return ctrl.Result{}, r.reconcileReady(ctx, &export)
	case phaseFailingOver:
		// Failover is the next milestone. Until it exists, an export that
		// somehow reached the phase is parked rather than half-driven.
		r.event(&export, corev1.EventTypeWarning, "FailoverNotImplemented",
			"failover is not implemented yet; the export is parked")
		return ctrl.Result{}, r.toDegraded(ctx, &export, machine, "failover not implemented")
	default:
		return ctrl.Result{}, nil
	}
}

// reconcilePending picks an MDS host. It is the only place the binding is
// established, and it writes the binding before anything is asked of the host.
func (r *NFSExportReconciler) reconcilePending(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
	machine *statemachine.Machine[phase],
	logger interface{ Info(string, ...any) },
) (ctrl.Result, error) {
	node, err := r.selectMDS(ctx, export)
	if err != nil {
		return ctrl.Result{}, err
	}
	if node == "" {
		// A refusal to act owes an event, or it is indistinguishable from a
		// reconcile that never ran.
		r.event(export, corev1.EventTypeWarning, "NoEligibleMDS",
			"no storage node is eligible to serve this export")
		if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
			s.Phase = phasePending
			s.Message = "waiting for an eligible MDS host"
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: nfsExportNoHostRequeue}, nil
	}

	logger.Info("binding export to MDS host", "node", node)
	if err := machine.TransitionTo(ctx, phaseAssembling); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Assembling: %w", err)
	}

	// Write ahead of the side effect: the binding and the phase are persisted
	// before the host is asked to do anything, so a reconcile that dies during
	// assembly finds the binding already made rather than choosing a second
	// host for the same export.
	if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
		s.Phase = phaseAssembling
		s.StorageNodeRef = node
		s.Message = "assembling the export"
		setDeadline(s, machine)
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(export, corev1.EventTypeNormal, "MDSSelected", fmt.Sprintf("bound to %s", node))
	return ctrl.Result{RequeueAfter: nfsExportFreshCopyRequeue}, nil
}

// reconcileAssembling asks the bound host to build the export.
func (r *NFSExportReconciler) reconcileAssembling(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
	machine *statemachine.Machine[phase],
	logger interface{ Info(string, ...any) },
) (ctrl.Result, error) {
	node := export.Status.StorageNodeRef
	if node == "" {
		// Assembling with no binding is not recoverable by retrying: something
		// wrote a phase without the field the phase depends on.
		return ctrl.Result{}, r.toDegraded(ctx, export, machine, "assembling with no bound MDS host")
	}

	if deadlineExpired(export) {
		r.event(export, corev1.EventTypeWarning, "AssembleTimeout",
			fmt.Sprintf("export did not assemble on %s within %s", node, nfsExportAssembleDeadline))
		return ctrl.Result{}, r.toDegraded(ctx, export, machine, "assembly timed out")
	}

	if r.Assembler == nil || !r.Assembler.HasSession(node) {
		// No session is a normal state, not a failure: a node is legitimately
		// disconnected during a rollout.
		return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, nil
	}

	nguid, err := r.Assembler.CreateExport(ctx, node, export)
	if err != nil {
		// Assembly is idempotent and will be retried with backoff.
		return ctrl.Result{}, fmt.Errorf("assembling export on %s: %w", node, err)
	}

	logger.Info("export assembled", "node", node)
	if err := machine.TransitionTo(ctx, phaseReady); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Ready: %w", err)
	}
	if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
		s.Phase = phaseReady
		s.Message = ""
		s.PhaseDeadline = nil
		if nguid != "" {
			// Only ever set, never cleared: the NGUID is a property of the
			// namespace rather than of this pass, and blanking it would take a
			// working device alias away from a client that already has one.
			s.NGUID = nguid
		}
		setCondition(s, simplyblockv1alpha2.NFSExportConditionAssembled, metav1.ConditionTrue,
			"MountPresent", "the filesystem is mounted on the bound host")
		setCondition(s, simplyblockv1alpha2.NFSExportConditionExported, metav1.ConditionTrue,
			"ExportfsApplied", "the mount is published to clients")
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(export, corev1.EventTypeNormal, "ExportReady", fmt.Sprintf("export assembled on %s", node))
	return ctrl.Result{}, nil
}

// reconcileReady is a no-op that keeps observedGeneration honest. A Ready export
// changes when its spec changes or when its host goes away, and both arrive as
// events rather than as a poll.
func (r *NFSExportReconciler) reconcileReady(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
) error {
	if export.Status.ObservedGeneration == export.Generation {
		return nil
	}
	return r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {})
}

// reconcileDelete tears the export down and drops the finalizer. The finalizer
// comes off on every path, including the one where nothing was ever assembled.
func (r *NFSExportReconciler) reconcileDelete(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(export, simplyblockv1alpha2.NFSExportFinalizer) {
		return ctrl.Result{}, nil
	}

	node := export.Status.StorageNodeRef
	if node != "" && r.Assembler != nil {
		if !r.Assembler.HasSession(node) {
			// The host is the only place the mount and the export entry exist,
			// so tearing down without reaching it would orphan both. Wait.
			r.event(export, corev1.EventTypeNormal, "AwaitingNodeForTeardown",
				fmt.Sprintf("waiting for %s to become reachable to tear the export down", node))
			return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, nil
		}
		if err := r.Assembler.DeleteExport(ctx, node, export); err != nil {
			return ctrl.Result{}, fmt.Errorf("tearing down export on %s: %w", node, err)
		}
	}

	return ctrl.Result{}, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.NFSExport
		if err := r.Get(ctx, client.ObjectKeyFromObject(export), &fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		controllerutil.RemoveFinalizer(&fresh, simplyblockv1alpha2.NFSExportFinalizer)
		return r.Update(ctx, &fresh)
	})
}

// selectMDS picks an eligible host, or returns "" when none is. Selection is
// round-robin by name over the eligible set, which is deterministic and spreads
// exports without needing state of its own.
func (r *NFSExportReconciler) selectMDS(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
) (string, error) {
	var nodes simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(export.Namespace)); err != nil {
		return "", fmt.Errorf("listing storage nodes: %w", err)
	}

	eligible := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		if mdsEligible(&nodes.Items[i]) {
			eligible = append(eligible, nodes.Items[i].Name)
		}
	}
	if len(eligible) == 0 {
		return "", nil
	}

	// Spread by hashing the export's own name, so the same export lands on the
	// same host across reconciles while different exports spread across hosts.
	slices.Sort(eligible)
	return eligible[nameHash(export.Name)%uint32(len(eligible))], nil
}

// mdsEligible reports whether a node may serve an export. The kernel and
// package requirements are host facts the node reports; until StorageNode
// carries them, being online is the whole test and a node that cannot actually
// serve fails later, visibly, in Assembling.
func mdsEligible(node *simplyblockv1alpha1.StorageNode) bool {
	return node.DeletionTimestamp.IsZero() &&
		node.Status.Status == utils.NodeStatusOnline &&
		node.Status.ActiveOpsRef == ""
}

// restore rebuilds the phase machine from status, so the position and its
// deadline survive a restart. Restoring runs no entry hooks: the transition
// into the phase already happened.
func (r *NFSExportReconciler) restore(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
) (*statemachine.Machine[phase], error) {
	snap := statemachine.Snapshot[phase]{State: export.Status.Phase}
	if export.Status.PhaseDeadline != nil {
		snap.Deadline = export.Status.PhaseDeadline.Time
	}
	return statemachine.NewFromSnapshot(ctx, exportPhases(), snap)
}

// exportPhases is the declared graph. Every edge that exists is written here,
// and an edge that is not written is an error at the transition rather than a
// silent status write.
func exportPhases() statemachine.Config[phase] {
	return statemachine.Config[phase]{
		Initial: phasePending,
		States: map[phase]statemachine.StateDef[phase]{
			// Nothing has been asked of a host yet, so there is nothing to fail
			// over and nothing to tear down.
			phasePending: {To: []phase{phaseAssembling, phaseDegraded, phaseDeleting}},
			phaseAssembling: {
				To: []phase{phaseReady, phaseDegraded, phaseDeleting},
				OnEnter: func(context.Context, phase, phase) (time.Duration, error) {
					return nfsExportAssembleDeadline, nil
				},
			},
			phaseReady:       {To: []phase{phaseFailingOver, phaseDegraded, phaseDeleting}},
			phaseFailingOver: {To: []phase{phaseReady, phaseDegraded, phaseDeleting}},
			// Terminal for this controller. Degraded is reached by declining to
			// act, and is left by a human.
			phaseDegraded: {To: []phase{phaseDeleting}},
			phaseDeleting: {},
		},
	}
}

// toDegraded parks the export and says why, in both a status message and an
// event, because a phase alone does not explain itself.
func (r *NFSExportReconciler) toDegraded(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
	machine *statemachine.Machine[phase],
	reason string,
) error {
	if err := machine.TransitionTo(ctx, phaseDegraded); err != nil {
		return fmt.Errorf("transition to Degraded: %w", err)
	}
	r.event(export, corev1.EventTypeWarning, "ExportDegraded", reason)
	return r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
		s.Phase = phaseDegraded
		s.Message = reason
		s.PhaseDeadline = nil
	})
}

// writeStatus applies mutate to a freshly read copy and writes it back, retrying
// on conflict. Re-reading is the point: a blind overwrite reverts whatever a
// concurrent writer put there.
func (r *NFSExportReconciler) writeStatus(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
	mutate func(*simplyblockv1alpha2.NFSExportStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.NFSExport
		if err := r.Get(ctx, client.ObjectKeyFromObject(export), &fresh); err != nil {
			return err
		}
		mutate(&fresh.Status)
		// Set on every status write, not only the interesting ones: a status
		// whose observedGeneration lags is indistinguishable from one that is
		// merely uninteresting.
		fresh.Status.ObservedGeneration = fresh.Generation
		if err := r.Status().Update(ctx, &fresh); err != nil {
			return err
		}
		fresh.Status.DeepCopyInto(&export.Status)
		return nil
	})
}

// ensureFinalizer adds the finalizer if it is absent, reporting whether it wrote.
func (r *NFSExportReconciler) ensureFinalizer(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
) (bool, error) {
	if controllerutil.ContainsFinalizer(export, simplyblockv1alpha2.NFSExportFinalizer) {
		return false, nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.NFSExport
		if err := r.Get(ctx, client.ObjectKeyFromObject(export), &fresh); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(&fresh, simplyblockv1alpha2.NFSExportFinalizer) {
			return nil
		}
		controllerutil.AddFinalizer(&fresh, simplyblockv1alpha2.NFSExportFinalizer)
		return r.Update(ctx, &fresh)
	})
	return err == nil, err
}

func (r *NFSExportReconciler) event(export *simplyblockv1alpha2.NFSExport, kind, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(export, nil, kind, reason, reason, "%s", message)
	}
}

// setDeadline copies the machine's current bound into status so it outlives the
// process.
func setDeadline(s *simplyblockv1alpha2.NFSExportStatus, machine *statemachine.Machine[phase]) {
	snap := machine.Snapshot()
	if snap.Deadline.IsZero() {
		s.PhaseDeadline = nil
		return
	}
	s.PhaseDeadline = &metav1.Time{Time: snap.Deadline}
}

// deadlineExpired reports whether the current phase has outlived its bound.
func deadlineExpired(export *simplyblockv1alpha2.NFSExport) bool {
	d := export.Status.PhaseDeadline
	return d != nil && time.Now().After(d.Time)
}

// setCondition upserts one condition, keeping the list a map keyed by type.
func setCondition(
	s *simplyblockv1alpha2.NFSExportStatus,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	cond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	for i := range s.Conditions {
		if s.Conditions[i].Type != condType {
			continue
		}
		if s.Conditions[i].Status == status {
			// An unchanged condition keeps its original transition time, which
			// is what makes the field mean what it says.
			cond.LastTransitionTime = s.Conditions[i].LastTransitionTime
		}
		s.Conditions[i] = cond
		return
	}
	s.Conditions = append(s.Conditions, cond)
}

// SetupWithManager wires the controller.
func (r *NFSExportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.NFSExport{}).
		Named("nfsexport").
		Complete(r)
}

// nameHash spreads exports across eligible hosts deterministically, so the same
// export picks the same host on every pass while different exports do not pile
// onto one.
func nameHash(name string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return h.Sum32()
}
