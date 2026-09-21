// NFSExportReconciler drives one pNFS export from creation to Ready and back.
//
// status.mdsNodeName is mutual exclusion, not a label: one XFS may be mounted
// by exactly one node, so no second host is a candidate until this controller
// rewrites the field under optimistic concurrency. Getting it wrong destroys
// data rather than degrading service.
//
// The phases are a declared graph, so an illegal jump errors at the transition
// and the position survives a restart.

package controller

import (
	"context"
	"errors"
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

	exportpkg "github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// phase is an alias so the graph below reads as a table.
type phase = simplyblockv1alpha2.NFSExportPhase

const (
	phasePending    = simplyblockv1alpha2.NFSExportPhasePending
	phaseAssembling = simplyblockv1alpha2.NFSExportPhaseAssembling
	phaseReady      = simplyblockv1alpha2.NFSExportPhaseReady
	phaseDegraded   = simplyblockv1alpha2.NFSExportPhaseDegraded
)

// The controller's cadence, in one place so it can be read and tuned.
const (
	// No host is eligible. Slower than the rest: usually a cluster still
	// coming up, and polling it hard helps nothing.
	nfsExportNoHostRequeue = 30 * time.Second
	// The bound host has no live csi-link session, which is normal during a
	// rollout rather than a failure.
	nfsExportNoSessionRequeue = 10 * time.Second
	// How long an export may sit in Assembling. That is a mkfs and a mount on
	// one host, so minutes are generous.
	nfsExportAssembleDeadline = 5 * time.Minute
	// Come back with a re-read object after a write that bumped
	// resourceVersion, so status is not written over a copy already stale.
	nfsExportFreshCopyRequeue = time.Second
)

// ExportAssembler is what this controller drives on the MDS host over
// csi-link. An interface so the controller is testable without a node.
type ExportAssembler interface {
	// CreateExport attaches, formats, mounts, and publishes on the named node.
	// Idempotent: a reconcile that died mid-assembly calls it again.
	CreateExport(ctx context.Context, nodeName string, export *simplyblockv1alpha2.NFSExport) error
	// DeleteExport tears it down in reverse. Idempotent, and an already-absent
	// export is success: the finalizer path has to converge.
	DeleteExport(ctx context.Context, nodeName string, export *simplyblockv1alpha2.NFSExport) error
	// HasSession separates "not connected right now," which is a requeue, from
	// a call that failed, which is not.
	HasSession(nodeName string) bool
}

// NFSExportReconciler reconciles NFSExport objects.
type NFSExportReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Assembler performs the host-side work. Nil makes every assembly a
	// requeue, so the kind can be deployed before csi-link is on.
	Assembler ExportAssembler
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=nfsexports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=nfsexports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=nfsexports/finalizers,verbs=update
// The export's client set is the cluster's own nodes, which is what decides
// who may mount it.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
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

	// Degraded is terminal on purpose: it is reached by refusing to act, and
	// retrying the refusal on a timer would bury the event that explains it.
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
		// A downgrade or a hand-edited resource. Guessing is worse than
		// stopping.
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
	default:
		// restore refuses to build a machine for an unknown phase, so this is
		// a known phase with nothing to do in it.
		return ctrl.Result{}, nil
	}
}

// reconcilePending picks an MDS host. The only place the binding is made, and
// it is written before anything is asked of the host.
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
		// A refusal owes an event, or it looks like a reconcile that never ran.
		r.event(export, corev1.EventTypeWarning, "NoEligibleMDS",
			fmt.Sprintf("no storage node is eligible to serve this export; "+
				"label the nodes that may with %s=true", MDSCapableLabel))
		if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
			s.Phase = phasePending
			s.Message = "waiting for an eligible MDS host"
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: nfsExportNoHostRequeue}, nil
	}

	// Before the transition: an export bound with no client set is one the
	// host refuses, and refusing here names the cause.
	clients, err := r.clusterNodeAddresses(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(clients) == 0 {
		r.event(export, corev1.EventTypeWarning, "NoAllowedClients",
			"no cluster node publishes an internal address, so the export would publish to nobody")
		return ctrl.Result{RequeueAfter: nfsExportNoHostRequeue}, nil
	}

	// Same reason: an export bound to an unreachable host fails at a client's
	// mount, several steps from the cause.
	address, err := r.nodeAddress(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	if address == "" {
		r.event(export, corev1.EventTypeWarning, "MDSUnaddressable",
			fmt.Sprintf("node %s publishes no address for clients to mount", node))
		return ctrl.Result{RequeueAfter: nfsExportNoHostRequeue}, nil
	}

	logger.Info("binding export to MDS host", "node", node, "address", address, "clients", len(clients))
	if err := machine.TransitionTo(ctx, phaseAssembling); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Assembling: %w", err)
	}

	// Write ahead of the side effect, so a reconcile that dies during assembly
	// finds the binding rather than choosing a second host for one export.
	if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
		s.Phase = phaseAssembling
		s.MDSNodeName = node
		s.MDSNodeIP = address
		s.AllowedClients = clients
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
	node := export.Status.MDSNodeName
	if node == "" {
		// Something wrote the phase without the field it depends on.
		return ctrl.Result{}, r.toDegraded(ctx, export, machine, "assembling with no bound MDS host")
	}

	if deadlineExpired(export) {
		r.event(export, corev1.EventTypeWarning, "AssembleTimeout",
			fmt.Sprintf("export did not assemble on %s within %s", node, nfsExportAssembleDeadline))
		return ctrl.Result{}, r.toDegraded(ctx, export, machine, "assembly timed out")
	}

	if r.Assembler == nil || !r.Assembler.HasSession(node) {
		// Normal during a rollout, not a failure.
		return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, nil
	}

	if err := r.Assembler.CreateExport(ctx, node, export); err != nil {
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
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(export, corev1.EventTypeNormal, "ExportReady", fmt.Sprintf("export assembled on %s", node))
	return ctrl.Result{}, nil
}

// reconcileReady re-assembles when the record has changed, and does nothing
// otherwise.
//
// The change that matters is a grow: the control plane enlarges the backing
// volume and records the new size here, and assembly is what runs xfs_growfs on
// the host serving the export. No CSI call reaches that host, so nothing else
// can. Assembly is idempotent, so re-running it costs a read of each step.
func (r *NFSExportReconciler) reconcileReady(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
) error {
	if export.Status.ObservedGeneration == export.Generation {
		return nil
	}

	node := export.Status.MDSNodeName
	if node != "" && r.Assembler != nil && r.Assembler.HasSession(node) {
		if err := r.Assembler.CreateExport(ctx, node, export); err != nil {
			return fmt.Errorf("re-assembling export on %s: %w", node, err)
		}
	}
	return r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {})
}

// reconcileDelete tears the export down and drops the finalizer, on every
// path including the one where nothing was ever assembled.
func (r *NFSExportReconciler) reconcileDelete(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(export, simplyblockv1alpha2.NFSExportFinalizer) {
		return ctrl.Result{}, nil
	}

	node := export.Status.MDSNodeName
	if node != "" && r.Assembler != nil {
		if !r.Assembler.HasSession(node) {
			// The mount and the exports entry exist only on the host, so
			// tearing down without reaching it orphans both.
			r.event(export, corev1.EventTypeNormal, "AwaitingNodeForTeardown",
				fmt.Sprintf("waiting for %s to become reachable to tear the export down", node))
			return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, nil
		}
		switch err := r.Assembler.DeleteExport(ctx, node, export); {
		case err == nil:
		case errors.Is(err, exportpkg.ErrInvalidSpec):
			// The record cannot describe an export, so it never became one and
			// there is nothing on the host to orphan. Retrying would leave a
			// finalizer nothing can remove.
			r.event(export, corev1.EventTypeNormal, "NothingToTearDown",
				fmt.Sprintf("export was never assembled on %s: %v", node, err))
		default:
			// Reached the host and failed there, so the mount may exist and
			// the finalizer stays.
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

// MDSCapableLabel marks a Kubernetes node as able to serve a pNFS export.
//
// It gates selection rather than describing it: a node without nfs-utils or a
// kernel nfsd cannot serve, and the operator cannot see either from here. Until
// a node agent reports it, the label is how an administrator says which nodes
// are equipped, and selection never picks one that has not said so.
//
// An unlabeled cluster therefore serves no exports, visibly: the export waits
// in Pending with an event naming the label, which is a better first run than
// binding a host that will fail in Assembling for a reason nobody can guess.
const MDSCapableLabel = "storage.simplyblock.io/pnfs-mds"

// selectMDS picks a node to serve this export, or "" when none can.
//
// It asks Kubernetes, not the storage cluster. An MDS is whichever node runs
// the csi-node pod that assembles the export; it reaches the volume over
// NVMe-oF exactly as a client does, so it does not have to be, or be near, the
// storage node holding the data. Requiring one would confine every export to
// the storage nodes for no reason the code can state.
func (r *NFSExportReconciler) selectMDS(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
) (string, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.MatchingLabels{MDSCapableLabel: "true"}); err != nil {
		return "", fmt.Errorf("listing the nodes labeled %s: %w", MDSCapableLabel, err)
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

	// Hash the name, so one export lands on one host across reconciles while
	// different exports spread.
	slices.Sort(eligible)
	return eligible[nameHash(export.Name)%uint32(len(eligible))], nil
}

// mdsEligible reports whether a labeled node may serve an export now.
//
// The label says the host is equipped; these say it is available. A node being
// deleted or cordoned is one an administrator is taking away, and binding an
// export to it would put the filesystem somewhere that is about to go.
func mdsEligible(node *corev1.Node) bool {
	if !node.DeletionTimestamp.IsZero() || node.Spec.Unschedulable {
		return false
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// restore rebuilds the phase machine from status. No entry hooks run: the
// transition into the phase already happened.
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

// exportPhases is the declared graph. An edge not written here is an error at
// the transition rather than a silent status write.
func exportPhases() statemachine.Config[phase] {
	return statemachine.Config[phase]{
		Initial: phasePending,
		States: map[phase]statemachine.StateDef[phase]{
			// Nothing asked of a host yet.
			phasePending: {To: []phase{phaseAssembling, phaseDegraded}},
			phaseAssembling: {
				To: []phase{phaseReady, phaseDegraded},
				OnEnter: func(context.Context, phase, phase) (time.Duration, error) {
					return nfsExportAssembleDeadline, nil
				},
			},
			// Ready leaves only downward. Moving an export to another host is
			// §13 and is not built, so the phase is not declared. Nor is a
			// Deleting phase: teardown runs off the deletion timestamp.
			phaseReady: {To: []phase{phaseDegraded}},
			// Reached by declining to act, and left by a human.
			phaseDegraded: {},
		},
	}
}

// toDegraded parks the export and says why, in a message and an event.
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

// writeStatus applies mutate to a freshly read copy, retrying on conflict.
// Re-reading is the point: a blind overwrite reverts a concurrent writer.
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
		// On every write: a lagging observedGeneration is indistinguishable
		// from an uninteresting one.
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

// setDeadline copies the machine's bound into status, to outlive the process.
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

// SetupWithManager wires the controller.
func (r *NFSExportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.NFSExport{}).
		Named("nfsexport").
		Complete(r)
}

// nameHash spreads exports across hosts deterministically.
func nameHash(name string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return h.Sum32()
}
