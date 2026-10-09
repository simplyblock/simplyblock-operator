// NFSExportReconciler drives one pNFS export from creation to Ready and back.
//
// Every export is served by the metadata server of its storage cluster, a
// QEMU guest in a pod of its own (design-pnfs-mds-vm.md). status.mdsPodName is
// mutual exclusion, not a label: one XFS may be mounted by exactly one host, so
// no second one is a candidate until this controller rewrites the field under
// optimistic concurrency. Getting it wrong destroys data rather than degrading
// service.
//
// The phases are a declared graph, so an illegal jump errors at the transition
// and the position survives a restart.

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/link"
	exportpkg "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nqn"
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
	// There is no metadata server to bind yet. Slower than the rest: usually a
	// cluster still coming up, and polling it hard helps nothing.
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
	// How often a Ready export's health is polled. A probe is only useful
	// ahead of a failure, so this has to be far shorter than how long an
	// operator team would tolerate not knowing about one.
	nfsExportHealthCheckInterval = time.Minute
	// A failed check is retried sooner than a healthy one is re-checked, so a
	// transient fault (a rollout restarting nfsd) is not mistaken for a
	// standing one by the time anything downstream reads the event.
	nfsExportUnhealthyRequeue = 15 * time.Second
)

// ExportAssembler is what this controller drives in the metadata server's guest
// over csi-link. An interface so the controller is testable without a guest.
type ExportAssembler interface {
	// CreateExport attaches, formats, mounts, and publishes on the host.
	// Idempotent: a reconcile that died mid-assembly calls it again.
	CreateExport(ctx context.Context, host ExportHost, export *simplyblockv1alpha2.NFSExport) error
	// DeleteExport tears it down in reverse. Idempotent, and an already-absent
	// export is success: the finalizer path has to converge.
	DeleteExport(ctx context.Context, host ExportHost, export *simplyblockv1alpha2.NFSExport) error
	// CheckExport reports the export unhealthy as an error naming why, or nil
	// when the host says it is actually being served. Detection only: nothing
	// here acts on a failure yet, because there is nowhere to move an export
	// to until §13's failover lands.
	CheckExport(ctx context.Context, host ExportHost, export *simplyblockv1alpha2.NFSExport) error
	// HasSession separates "not connected right now," which is a requeue, from
	// a call that failed, which is not.
	HasSession(host link.PeerID) bool
}

// NFSExportReconciler reconciles NFSExport objects.
type NFSExportReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Assembler performs the host-side work. Nil makes every assembly a
	// requeue, so the kind can be deployed before csi-link is on.
	Assembler ExportAssembler

	// OperatorNamespace is where the metadata server pods run.
	OperatorNamespace string

	// Flows has the nodes forget the connections translated to a metadata
	// server address the export no longer uses. Nil skips it, so the kind
	// works without csi-link (design-pnfs-mds-vm.md §8.1).
	Flows FlowForgetter

	// withdrawn is each export's address withdrawn while its pod went away,
	// until the replacement's address replaces it.
	withdrawnMu sync.Mutex
	withdrawn   map[client.ObjectKey]string
}

// FlowForgetter asks every node to drop its connection-tracking entries for
// NFS traffic translated to the given address. It returns at once, because the
// work happens in the background and never holds a reconcile.
type FlowForgetter interface {
	Forget(ip string)
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=nfsexports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=nfsexports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=nfsexports/finalizers,verbs=update
// The export's client set is the cluster's own nodes, which is what decides
// who may mount it.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// The stable mount address: a Service this reconciler owns, and the
// EndpointSlice behind it (§13.3).
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch
// The metadata server pod an export is bound to: read to bind an export to it,
// and to tell a restarting pod from a gone one at teardown.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

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
		return r.reconcileReady(ctx, &export)
	default:
		// restore refuses to build a machine for an unknown phase, so this is
		// a known phase with nothing to do in it.
		return ctrl.Result{}, nil
	}
}

// reconcilePending binds the export to the metadata server of its storage
// cluster, bringing the server up on the cluster's first export.
//
// With no driver configuring one there is nothing to bind, and the export waits:
// a node's own nfsd could serve it, but never fail it over.
func (r *NFSExportReconciler) reconcilePending(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
	machine *statemachine.Machine[phase],
	logger interface{ Info(string, ...any) },
) (ctrl.Result, error) {
	d, err := r.mdsDriver(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if d == nil {
		// A refusal owes an event, or it looks like a reconcile that never ran.
		r.event(export, corev1.EventTypeWarning, "NoMetadataServer",
			"no SimplyblockDriver configures a pNFS metadata server; set spec.pnfs.mds")
		return r.waitForMDS(ctx, export,
			"waiting for a SimplyblockDriver to configure spec.pnfs.mds", nfsExportNoHostRequeue)
	}
	return r.bindMDS(ctx, export, machine, logger, d)
}

// mdsBinding is the metadata server pod an export is bound to, and the address
// the export's Service endpoints at.
type mdsBinding struct {
	pod     string
	address string
}

func (b mdsBinding) String() string {
	return "metadata server pod " + b.pod
}

// bindExport writes the binding. The only place one is made, and it is written
// before anything is asked of the host.
func (r *NFSExportReconciler) bindExport(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
	machine *statemachine.Machine[phase],
	logger interface{ Info(string, ...any) },
	host mdsBinding,
) (ctrl.Result, error) {
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

	// The stable address, ahead of the transition for the same reason as the
	// client set: an export bound with nothing to mount it at fails at a
	// client, several steps from the cause.
	serviceAddress, err := r.reconcileExportService(ctx, export, host.address)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring the Service for %s: %w", export.Name, err)
	}

	logger.Info("binding export to MDS host", "host", host.String(), "address", host.address, "clients", len(clients))
	if err := machine.TransitionTo(ctx, phaseAssembling); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Assembling: %w", err)
	}

	// Write ahead of the side effect, so a reconcile that dies during assembly
	// finds the binding rather than choosing a second host for one export.
	if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
		s.Phase = phaseAssembling
		s.MDSPodName = host.pod
		s.MDSNodeIP = host.address
		s.ServiceAddress = serviceAddress
		s.AllowedClients = clients
		s.Message = "assembling the export"
		setDeadline(s, machine)
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(export, corev1.EventTypeNormal, "MDSSelected", fmt.Sprintf("bound to %s", host))
	return ctrl.Result{RequeueAfter: nfsExportFreshCopyRequeue}, nil
}

// reconcileAssembling asks the bound host to build the export.
func (r *NFSExportReconciler) reconcileAssembling(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
	machine *statemachine.Machine[phase],
	logger interface{ Info(string, ...any) },
) (ctrl.Result, error) {
	host := mdsHost(export)
	if host.Zero() {
		// Something wrote the phase without the field it depends on.
		return ctrl.Result{}, r.toDegraded(ctx, export, machine, "assembling with no bound MDS host")
	}

	if deadlineExpired(export) {
		r.event(export, corev1.EventTypeWarning, "AssembleTimeout",
			fmt.Sprintf("export did not assemble on %s within %s", host, nfsExportAssembleDeadline))
		return ctrl.Result{}, r.toDegraded(ctx, export, machine, "assembly timed out")
	}

	if r.Assembler == nil || !r.Assembler.HasSession(host) {
		// Normal during a rollout, not a failure.
		return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, nil
	}

	if err := r.Assembler.CreateExport(ctx, r.exportHost(ctx, export, host), export); err != nil {
		// Assembly is idempotent and will be retried with backoff.
		return ctrl.Result{}, fmt.Errorf("assembling export on %s: %w", host, err)
	}

	logger.Info("export assembled", "host", host.String())
	if err := machine.TransitionTo(ctx, phaseReady); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Ready: %w", err)
	}
	assembledBy := r.assemblingInstance(ctx, export)
	if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {
		s.Phase = phaseReady
		s.Message = ""
		s.PhaseDeadline = nil
		s.AssembledBy = assembledBy
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(export, corev1.EventTypeNormal, "ExportReady", fmt.Sprintf("export assembled on %s", host))
	return ctrl.Result{}, nil
}

// reconcileReady re-assembles when the record has changed, and either way
// polls the export's health before it is done.
//
// The change that matters is a grow: the control plane enlarges the backing
// volume and records the new size here, and assembly is what runs xfs_growfs on
// the host serving the export. No CSI call reaches that host, so nothing else
// can. Assembly is idempotent, so re-running it costs a read of each step.
func (r *NFSExportReconciler) reconcileReady(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
) (ctrl.Result, error) {
	if result, handled, err := r.resyncAfterRestart(ctx, export); handled || err != nil {
		return result, err
	}
	if err := r.refreshMDS(ctx, export); err != nil {
		return ctrl.Result{}, err
	}

	host := mdsHost(export)

	if export.Status.ObservedGeneration != export.Generation {
		if host.Zero() || r.Assembler == nil || !r.Assembler.HasSession(host) {
			r.event(export, corev1.EventTypeNormal, "AwaitingMDSForGrow",
				fmt.Sprintf("waiting for %s to become reachable to grow the export", host))
			return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, nil
		}
		if err := r.Assembler.CreateExport(ctx, r.exportHost(ctx, export, host), export); err != nil {
			return ctrl.Result{}, fmt.Errorf("re-assembling export on %s: %w", host, err)
		}
		if err := r.writeStatus(ctx, export, func(s *simplyblockv1alpha2.NFSExportStatus) {}); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.reconcileHealth(ctx, export, host)
}

// reconcileHealth polls the bound host and reports what it finds, and never
// moves the phase.
//
// A probe with no failover yet to hand a failure to (§13.5, not implemented)
// could only turn Ready into Degraded, which this controller treats as
// terminal -- reached by declining to act, not by a check that may recover on
// its own next pass. A rollout that bounces nfsd for a few seconds is not
// grounds for an export to need a human, so an unhealthy check is a loud
// event and a fast recheck, and it is left to a future §13 to decide when an
// unhealthy export should actually move.
func (r *NFSExportReconciler) reconcileHealth(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
	host link.PeerID,
) (ctrl.Result, error) {
	if host.Zero() || r.Assembler == nil || !r.Assembler.HasSession(host) {
		return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, nil
	}
	if err := r.Assembler.CheckExport(ctx, r.exportHost(ctx, export, host), export); err != nil {
		r.event(export, corev1.EventTypeWarning, "MDSUnhealthy",
			fmt.Sprintf("export on %s failed its health check: %v", host, err))
		return ctrl.Result{RequeueAfter: nfsExportUnhealthyRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: nfsExportHealthCheckInterval}, nil
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

	host := mdsHost(export)
	if !host.Zero() && r.Assembler != nil {
		if !r.Assembler.HasSession(host) {
			if gone, err := r.mdsPodGone(ctx, export); err != nil {
				return ctrl.Result{}, err
			} else if gone {
				// The guest's mounts and exports table went with its pod, so
				// there is nothing left to tear down or to orphan.
				r.event(export, corev1.EventTypeNormal, "NothingToTearDown",
					fmt.Sprintf("metadata server pod %s is gone, and its mounts with it", export.Status.MDSPodName))
				return ctrl.Result{}, r.releaseFinalizer(ctx, export)
			}
			// The pod still exists, so its guest may still hold the mount
			// and the exports entry, and tearing down without reaching it
			// orphans both.
			r.event(export, corev1.EventTypeNormal, "AwaitingMDSForTeardown",
				fmt.Sprintf("waiting for %s to become reachable to tear the export down", host))
			return ctrl.Result{RequeueAfter: nfsExportNoSessionRequeue}, nil
		}
		switch err := r.Assembler.DeleteExport(ctx, r.exportHost(ctx, export, host), export); {
		case err == nil:
		case errors.Is(err, exportpkg.ErrInvalidSpec):
			// The record cannot describe an export, so it never became one and
			// there is nothing on the host to orphan. Retrying would leave a
			// finalizer nothing can remove.
			r.event(export, corev1.EventTypeNormal, "NothingToTearDown",
				fmt.Sprintf("export was never assembled on %s: %v", host, err))
		default:
			// Reached the host and failed there, so the mount may exist and
			// the finalizer stays.
			return ctrl.Result{}, fmt.Errorf("tearing down export on %s: %w", host, err)
		}
	}

	return ctrl.Result{}, r.releaseFinalizer(ctx, export)
}

// releaseFinalizer drops the export's finalizer against a fresh copy.
func (r *NFSExportReconciler) releaseFinalizer(ctx context.Context, export *simplyblockv1alpha2.NFSExport) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.NFSExport
		if err := r.Get(ctx, client.ObjectKeyFromObject(export), &fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		controllerutil.RemoveFinalizer(&fresh, simplyblockv1alpha2.NFSExportFinalizer)
		return r.Update(ctx, &fresh)
	})
}

// ExportHost is where an export call goes, and the NVMe host identity the
// guest attaches the namespace as.
type ExportHost struct {
	Peer link.PeerID
	// HostNQN is the guest's: it is one host across pod restarts, which it
	// cannot know itself (design-pnfs-mds-vm.md §6.5).
	HostNQN string
}

// exportHost adds the host identity to the peer. The guest attaches as its
// StatefulSet, whose UID survives the pod and changes only when the
// StatefulSet is recreated, which is when an old authorization should not
// carry over. A pod that cannot be read leaves the identity empty: the call
// then fails at the control plane with its own message, or, for a teardown,
// needs none.
func (r *NFSExportReconciler) exportHost(
	ctx context.Context, export *simplyblockv1alpha2.NFSExport, peer link.PeerID,
) ExportHost {
	host := ExportHost{Peer: peer}
	name := export.Status.MDSPodName
	if name == "" {
		return host
	}
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: name}, &pod); err != nil {
		return host
	}
	if owner := metav1.GetControllerOf(&pod); owner != nil && owner.Kind == "StatefulSet" {
		host.HostNQN = nqn.Host(string(owner.UID))
	}
	return host
}

// mdsHost is the peer serving the export, its metadata server pod, and zero
// while it is unbound.
func mdsHost(export *simplyblockv1alpha2.NFSExport) link.PeerID {
	if pod := export.Status.MDSPodName; pod != "" {
		return link.MDSPeer(pod)
	}
	return link.PeerID{}
}

// mdsPodGone reports whether the export's metadata server pod no longer
// exists, which takes the guest's mounts and exports table with it.
func (r *NFSExportReconciler) mdsPodGone(ctx context.Context, export *simplyblockv1alpha2.NFSExport) (bool, error) {
	pod := export.Status.MDSPodName
	if pod == "" {
		return false, nil
	}
	err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: pod}, &corev1.Pod{})
	switch {
	case err == nil:
		return false, nil
	case apierrors.IsNotFound(err):
		return true, nil
	default:
		return false, fmt.Errorf("reading metadata server pod %s: %w", pod, err)
	}
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
//
// Owns the Service and EndpointSlice it creates, so a manual delete of either
// is noticed and repaired on the export's own reconcile rather than left
// until something else touches the NFSExport.
func (r *NFSExportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.NFSExport{}).
		Owns(&corev1.Service{}).
		Owns(&discoveryv1.EndpointSlice{}).
		// A metadata server pod that restarts or turns Ready wakes the exports
		// bound to it: the ones waiting to bind, and the ones to resync.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				pod, ok := obj.(*corev1.Pod)
				if !ok {
					return nil
				}
				return r.exportsBoundToPod(ctx, pod)
			}),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				_, ok := obj.GetLabels()[driver.MDSClusterLabel]
				return ok
			}))).
		Named("nfsexport").
		Complete(r)
}
