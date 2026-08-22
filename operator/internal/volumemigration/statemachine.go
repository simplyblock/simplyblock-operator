package volumemigration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/simplyblock/atlas/statemachine"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// The lifecycle of a VolumeMigration as a state machine. The CRD's own phase type is
// the state type, so there is no parallel enum to keep in step with status.phase:
//
//	Pending ─┬─ Validating ── Running ── Completed
//	   │     └───────┴───────────┴────── Failed
//	   └─(deferred)  └───────────┴────── Aborted
//	                 └────────────────── Aborted
//
// Three properties of the graph are worth stating, because each replaces a check
// that used to be written by hand:
//
//   - Every phase declares what may follow it, so a transition the lifecycle does
//     not allow is refused by the machine rather than by a switch that has to
//     remember. Aborting a migration that was never submitted, for instance, is a
//     different outcome from a backend failure, and [statemachine.IllegalTransitionError]
//     is what tells them apart.
//   - Entering a phase runs its hook, and only a hook that succeeds moves the
//     machine. The side effects of a phase and the phase itself can therefore not
//     drift apart: there is no path that writes status.phase = Running without
//     having called ContinueMigration.
//   - Pending, Validating and Running each carry a bound (see [phaseBound]), armed
//     when the phase is entered and persisted in status.phaseDeadline. A migration
//     cannot sit in a non-terminal phase forever, which matters because everything
//     that waits on a VolumeMigration — the rebalancer, StorageNodeOps, the PVC
//     controller — waits for a terminal one.
//
// Pending has a self-edge, which is how a deferral is expressed: the control plane
// refused the migration, the phase is re-entered, and the window that bounds the
// retrying is armed on the way in.
type phase = simplyblockv1alpha1.VolumeMigrationPhase

const (
	phasePending    = simplyblockv1alpha1.VolumeMigrationPhasePending
	phaseValidating = simplyblockv1alpha1.VolumeMigrationPhaseValidating
	phaseRunning    = simplyblockv1alpha1.VolumeMigrationPhaseRunning
	phaseCompleted  = simplyblockv1alpha1.VolumeMigrationPhaseCompleted
	phaseFailed     = simplyblockv1alpha1.VolumeMigrationPhaseFailed
	phaseAborted    = simplyblockv1alpha1.VolumeMigrationPhaseAborted
)

// migrationPass is one reconcile of one VolumeMigration: the object being
// reconciled, the copy its status is patched against, and the handful of values a
// step has to hand to the entry hook of the phase it decided to move into.
//
// The hand-off exists because a hook runs as part of a transition rather than as
// part of the step that decided on it. The step that submits the migration is the
// one holding the CreateMigration answer, and the step that gives up is the one
// holding the reason — but it is the hook that writes them, because the hook is what
// runs if and only if the phase is actually entered. Closing over one of these per
// reconcile keeps that explicit, and keeps the reconciler itself free of per-object
// state.
type migrationPass struct {
	vm *simplyblockv1alpha1.VolumeMigration

	// before is the object as read, and the base of the single status patch each
	// reconcile ends with.
	before *simplyblockv1alpha1.VolumeMigration

	// created is the CreateMigration answer, set by the Pending step and consumed by
	// the Validating hook.
	created *webapi.MigrationDTO

	// failure is why the migration is being failed, set by whoever decided to and
	// consumed by the Failed hook.
	failure string

	// cancelBackend asks the Failed hook to take back what the control plane created.
	// It is set where the operator gives up on a migration the storage API still
	// believes in; the routes that fail before anything reached it, or after it has
	// finished with the migration itself, leave it false.
	cancelBackend bool

	// releasePaths asks the Failed hook to give back the NVMe target paths the
	// consumer nodes connected while validating. It is separate from cancelBackend
	// because the two are not always wanted together: a migration the control plane
	// has already cancelled still has host paths nothing has taken back.
	releasePaths bool

	// finished is the outcome the Running step read from the storage API, consumed by
	// the Completed and Failed hooks so a migration that ends on its own is recorded
	// with the control plane's own error message.
	finished *webapi.MigrationDTO
}

// phaseBound is how long a phase may last. Zero means the phase is not bounded by
// the clock.
//
// It is a function rather than a value on each [statemachine.StateDef] because the
// bound is needed in two places: an entry hook returns it when the phase is entered,
// and [Reconciler.restorePhase] re-arms it for an object that carries
// a phase from before status.phaseDeadline existed. One definition, two readers.
func phaseBound(p phase) time.Duration {
	switch p {
	case phasePending:
		// Only ever armed on a deferral: a migration nobody has submitted yet is
		// bounded by the control plane accepting it, not by the clock.
		return maxMigrationDeferral
	case phaseValidating:
		// Long enough for the two things the phase does in sequence — wait for the
		// consumer pods, then run a Job per consuming node — and no longer. The
		// control plane does not hold a created-but-unstarted migration open
		// indefinitely, so a validation that has not finished by now is not going to
		// be useful when it does.
		return maxConsumerWait + validationJobDeadline
	default:
		// Running takes as long as there is data to copy, and Completed, Failed and
		// Aborted are over.
		return 0
	}
}

// machineFor builds the lifecycle machine for one migration. The hooks close over p,
// so the machine belongs to a single reconcile rather than to the reconciler.
//
// ctx bounds the machine and, through it, every phase deadline it arms — see
// [statemachine.New]. That is the reconcile's context, which is shorter-lived than
// the phases themselves; it does not matter, because the deadline that outlives the
// reconcile is the persisted one, and the live context only bounds work done within
// this pass.
func (r *Reconciler) machineFor(
	ctx context.Context,
	p *migrationPass,
) *statemachine.Machine[phase] {
	return statemachine.Must(ctx, statemachine.Config[phase]{
		Initial: phasePending,
		States: map[phase]statemachine.StateDef[phase]{
			// The self-edge is a deferral; Aborted is reachable because a user who
			// asks to stop a migration that is being retried should not have to wait
			// out the retry window first.
			phasePending: {
				To:      []phase{phasePending, phaseValidating, phaseFailed, phaseAborted},
				OnEnter: r.onDeferred(p),
			},
			// No self-edge: the Jobs of a validation in progress are polled, which is
			// not a transition, and re-entering the phase would submit a second
			// migration for the same subsystem.
			phaseValidating: {
				To:      []phase{phaseRunning, phaseFailed, phaseAborted},
				OnEnter: r.onValidating(p),
			},
			// Likewise: polling a running migration is not a transition, and
			// re-entering would call ContinueMigration on a migration that has left
			// pre_created and would reject it.
			phaseRunning: {
				To:      []phase{phaseCompleted, phaseFailed, phaseAborted},
				OnEnter: r.onRunning(p),
			},
			// Terminal phases, declared with no exits: a finished migration is
			// finished, and Reconcile returns on IsTerminal before touching anything.
			phaseCompleted: {OnEnter: r.onCompleted(p)},
			phaseFailed:    {OnEnter: r.onFailed(p)},
			phaseAborted:   {OnEnter: r.onAborted(p)},
		},
	})
}

// onDeferred re-enters Pending for a migration the control plane refused because the
// cluster is busy with work that ends on its own — typically the data realignment a
// previous migration triggered, which the control plane will not migrate through.
//
// Its job is to arm the window that bounds the retrying, which is why the step only
// takes this edge on the first refusal: re-arming on every refusal would push the
// window out each time and the migration would retry forever. Without a bound, a
// condition that never clears — say a rebalancing task whose runner is not running,
// which keeps _can_add_lvol_migration false indefinitely — would leave the migration
// in a non-terminal phase for good, stalling everything that waits on it instead of
// reporting a failure they can act on.
func (r *Reconciler) onDeferred(p *migrationPass) statemachine.TransitionFunc[phase] {
	return func(_ context.Context, _, _ phase) (time.Duration, error) {
		if p.vm.Status.DeferredSince == nil {
			now := metav1.Now()
			p.vm.Status.DeferredSince = &now
		}
		return phaseBound(phasePending), nil
	}
}

// onValidating records a migration the control plane has accepted. Everything it
// writes comes from the CreateMigration answer the Pending step is holding, so that
// every later call — continue, poll, cancel — can address the migration without
// resolving the volume again.
//
// The phase it opens is the one where the operator, not the control plane, has work
// to do: the new target-side paths have to exist and be verified on every node that
// consumes a volume of the migrated subsystem before the cutover is allowed to
// happen. That work is bounded, hence the returned deadline.
func (r *Reconciler) onValidating(p *migrationPass) statemachine.TransitionFunc[phase] {
	return func(ctx context.Context, _, _ phase) (time.Duration, error) {
		vm, migration := p.vm, p.created
		if migration == nil {
			// Unreachable: the Pending step only takes this edge with an answer in
			// hand. Refusing the transition rather than writing a phase with no
			// migration behind it, which is what the old code had to detect later.
			return 0, fmt.Errorf("no CreateMigration result to record")
		}

		now := metav1.Now()
		vm.Status.MigrationUUID = migration.ID
		vm.Status.SubsystemNQN = migration.TargetNQN
		vm.Status.MemberCount = migration.MemberCount
		vm.Status.Connections = migrationConnections(migration)
		vm.Status.StartedAt = &now
		// Accepted, so the deferral window no longer applies.
		vm.Status.DeferredSince = nil
		// SourceNodeUUID is populated from GetMigration once the migration runs.

		r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "MigrationCreated", "MigrationCreated",
			"Migration %s created for subsystem %s (%d volume(s)): validating %d connection(s) to node %s",
			migration.ID, migration.TargetNQN, migration.MemberCount,
			len(vm.Status.Connections), vm.Spec.TargetNodeUUID)
		logf.FromContext(ctx).Info("Volume migration submitted",
			"migration", migration.ID, "volume", vm.Status.VolumeUUID,
			"cluster", vm.Status.ClusterUUID, "subsystem", migration.TargetNQN,
			"target", vm.Spec.TargetNodeUUID, "members", migration.MemberCount)

		return phaseBound(phaseValidating), nil
	}
}

// migrationConnections converts the connect strings CreateMigration answered into
// the status entries the validation and release Jobs are built from.
func migrationConnections(m *webapi.MigrationDTO) []simplyblockv1alpha1.MigrationConnection {
	conns := make([]simplyblockv1alpha1.MigrationConnection, 0, len(m.ConnectStrings))
	for _, c := range m.ConnectStrings {
		conns = append(conns, simplyblockv1alpha1.MigrationConnection{
			NQN:            c.Nqn,
			IP:             c.IP,
			Port:           c.Port,
			Transport:      c.TargetType,
			NrIoQueues:     c.NrIoQueues,
			ReconnectDelay: c.ReconnectDelay,
			// Not c.CtrlLossTmo: the host connects every path with the same loss
			// timeout the CSI driver uses, and the control plane's answer here is an
			// hour. See CtrlLossTmoSec. Overridden at ingestion rather than
			// where the Job is built so that status.connections records the connect
			// that will actually be made.
			CtrlLossTmo:   CtrlLossTmoSec,
			FastIOFailTmo: c.FastIOFailTmo,
			KeepAliveTmo:  c.KeepAliveTmo,
		})
	}
	return conns
}

// onRunning cuts the migration over: the paths have been validated everywhere they
// had to be, so the control plane is told to start moving data.
//
// ContinueMigration is not idempotent — it only accepts a migration in phase
// pre_created — so the backend phase is read first and the call is made only if the
// migration has not advanced already. That is what makes this transition safe to
// replay: a previous reconcile that continued the migration and then failed to
// persist Running must not have its healthy, running migration cancelled by the
// retry. Per-object reconcile serialisation (a key is processed by at most one
// worker at a time) makes the read-then-continue window race-free within the
// operator.
//
// The phase gets no deadline. A data copy takes as long as there is data to copy,
// and a bound guessed here would either fail migrations that were making progress or
// be so generous as to bound nothing; the Running step watches progress instead.
func (r *Reconciler) onRunning(p *migrationPass) statemachine.TransitionFunc[phase] {
	return func(ctx context.Context, _, _ phase) (time.Duration, error) {
		log := logf.FromContext(ctx)
		vm := p.vm

		m, err := r.apiClient.GetMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID)
		if err != nil {
			// Refusing the transition leaves the migration in Validating with its
			// paths connected and its deadline running; the next pass tries again.
			return 0, fmt.Errorf("read migration before continue: %w", err)
		}

		switch {
		case webapi.MigrationIsTerminal(m.Status):
			// The migration reached a terminal state out-of-band. Do not cancel or
			// re-continue; enter Running and let the Running step classify it.
			log.Info("Migration already terminal before continue; advancing to Running for classification",
				"migration", vm.Status.MigrationUUID, "status", m.Status)
		case m.Phase == webapi.MigrationPhasePreCreated:
			if err := r.continueMigration(ctx, vm); err != nil {
				return 0, err
			}
		default:
			// Already past pre_created: a prior reconcile continued the migration but
			// did not persist Running. Skip the (now-invalid) continue call.
			log.Info("Migration already continued (past pre_created); skipping ContinueMigration",
				"migration", vm.Status.MigrationUUID, "phase", m.Phase)
		}

		// The validation Jobs have served their purpose and their logs are already in
		// the operator log; reap them as the migration leaves the Validating phase.
		r.deleteValidationJobs(ctx, vm)
		vm.Status.Connections = nil
		vm.Status.ValidationJobs = nil

		r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "MigrationStarted", "MigrationStarted",
			"Migration %s started: volume %s → node %s",
			vm.Status.MigrationUUID, vm.Status.VolumeUUID, vm.Spec.TargetNodeUUID)
		return phaseBound(phaseRunning), nil
	}
}

// continueMigration starts the data movement of a migration still in pre_created.
//
// A failing ContinueMigration may still have taken effect, so the migration is
// re-read before anything is given up on: only one still stuck in pre_created is a
// genuine start failure, and only that one is cancelled. Returning an error here
// refuses the transition, which is what fails the migration — the caller cannot
// distinguish "could not start" from "started", so it must not guess.
func (r *Reconciler) continueMigration(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) error {
	log := logf.FromContext(ctx)

	err := r.apiClient.ContinueMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID)
	if err == nil {
		return nil
	}

	m, gerr := r.apiClient.GetMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID)
	if gerr == nil && m.Phase != webapi.MigrationPhasePreCreated {
		log.Info("ContinueMigration errored but migration has advanced past pre_created; treating as continued",
			"migration", vm.Status.MigrationUUID, "error", err.Error())
		return nil
	}

	// Best-effort: the migration fails either way, but a failed cancel leaves
	// target-side objects behind, so it must not be silent.
	if cerr := r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID); cerr != nil {
		log.Error(cerr, "Cannot cancel migration that failed to continue; target-side objects may remain",
			"migration", vm.Status.MigrationUUID, "subsystem", vm.Status.SubsystemNQN)
	}
	return fmt.Errorf("%w: ContinueMigration: %w", errMigrationStartFailed, err)
}

// errMigrationStartFailed marks the one way of failing to enter Running that is not
// worth retrying: ContinueMigration was rejected and the migration is provably still
// in pre_created, so it never started, and [Reconciler.continueMigration]
// has already cancelled it. Every other failure leaves it unknown whether the
// migration was continued, which is a reason to ask again rather than to give up.
var errMigrationStartFailed = errors.New("migration could not be started")

// onCompleted records a migration the storage API reported as done, and tells the
// owning cluster that a volume moved.
func (r *Reconciler) onCompleted(p *migrationPass) statemachine.TransitionFunc[phase] {
	return func(ctx context.Context, _, _ phase) (time.Duration, error) {
		vm := p.vm
		now := metav1.Now()
		vm.Status.CompletedAt = &now

		r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "MigrationCompleted", "MigrationCompleted",
			"Migration %s completed successfully", vm.Status.MigrationUUID)
		// A volume moved: count it on the owning cluster so the rebalancer's periodic
		// loop triggers a control-plane data realignment once enough have accumulated
		// and the cluster is quiet. Best-effort in both directions — realignment is
		// idempotent, so a missed count is picked up by the next completing migration,
		// and a count written twice because this pass failed to persist Completed only
		// brings the next realignment forward by one migration.
		MarkVolumeMoved(ctx, r.Client, vm.Namespace, vm.Status.ClusterUUID)
		return phaseBound(phaseCompleted), nil
	}
}

// onFailed records why a migration failed and, where the operator is giving up on a
// migration it had already submitted, takes back what that submission created.
//
// "What it created" is both halves of it. The control plane's target-side objects are
// what CancelMigration takes back; the host's are the NVMe paths every consumer node
// connected in order to validate, which nothing else will ever release — the Job that
// failed releases its own on the way out, but the nodes whose validation *passed*
// exited successfully and are never told that the migration was given up on anyway.
// Their target paths stay connected, retry a target that has stopped answering for
// them, and settle into the husk that blocks the next migration of the subsystem.
//
// This hook cannot fail. A migration that has been decided against has to reach a
// terminal phase on this pass: refusing the transition because a cleanup call errored
// would leave it non-terminal, still holding paths, with nothing left that intends to
// finish it. So the cancel is best-effort and says so in the recorded reason.
func (r *Reconciler) onFailed(p *migrationPass) statemachine.TransitionFunc[phase] {
	return func(ctx context.Context, from, _ phase) (time.Duration, error) {
		log := logf.FromContext(ctx)
		vm := p.vm

		reason := p.failure
		if p.finished != nil {
			// The storage API ended the migration itself; its own message is the reason.
			reason = p.finished.ErrorMessage
		}

		if p.cancelBackend && vm.Status.MigrationUUID != "" {
			if err := r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID); err != nil {
				// Report the original reason regardless; a failed cancel only adds to it.
				log.Error(err, "Cannot cancel migration; target-side objects may remain",
					"migration", vm.Status.MigrationUUID, "subsystem", vm.Status.SubsystemNQN)
				reason += fmt.Sprintf(" (cancelling the migration also failed: %v)", err)
			}
		}
		if p.releasePaths {
			r.releaseMigrationPaths(ctx, vm)
		}

		now := metav1.Now()
		vm.Status.ErrorMessage = reason
		vm.Status.CompletedAt = &now

		log.Info("Volume migration failed", "migration", vm.Status.MigrationUUID,
			"phase", from, "reason", reason)
		r.Recorder.Eventf(vm, nil, corev1.EventTypeWarning, "MigrationFailed", "MigrationFailed", "%s", reason)
		return phaseBound(phaseFailed), nil
	}
}

// onAborted cancels a migration the user asked to stop.
//
// Unlike [Reconciler.onFailed] this hook can refuse: an abort is a
// request, and a migration whose cancellation the control plane did not accept has
// not been aborted. Returning the error leaves the migration in the phase it was in
// so the next pass asks again — which is the honest outcome, because reporting
// Aborted here would claim the data movement had stopped when it may not have.
func (r *Reconciler) onAborted(p *migrationPass) statemachine.TransitionFunc[phase] {
	return func(ctx context.Context, from, _ phase) (time.Duration, error) {
		log := logf.FromContext(ctx)
		vm := p.vm

		if vm.Status.MigrationUUID == "" {
			// A migration that was never submitted — still pending, or deferred by a
			// busy cluster — has nothing to cancel on the backend, and calling with an
			// empty id would only produce errors to retry forever.
			log.Info("No backend migration was created yet; aborting the request only")
		} else if err := r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID); err != nil {
			return 0, fmt.Errorf("CancelMigration: %w", err)
		}

		// Best-effort cleanup when aborting during Validating. The validation Jobs go
		// first so that nothing is still connecting paths while the release Jobs look
		// for them — a Job deleted here may still have a pod winding down, so this
		// narrows the race rather than closing it, and what slips through is caught by
		// the reap before the next validation. Both happen before status is cleared,
		// because releasing needs the node list and the connections it is about to drop.
		if len(vm.Status.ValidationJobs) > 0 {
			r.deleteValidationJobs(ctx, vm)
			r.releaseMigrationPaths(ctx, vm)
			vm.Status.ValidationJobs = nil
			vm.Status.Connections = nil
		}

		now := metav1.Now()
		vm.Status.CompletedAt = &now

		log.Info("Volume migration aborted", "migration", vm.Status.MigrationUUID, "phase", from)
		r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "MigrationAborted", "MigrationAborted",
			"Migration %s cancelled", vm.Status.MigrationUUID)
		return phaseBound(phaseAborted), nil
	}
}

// restorePhase puts the machine where the resource says the migration is. It runs no
// entry hook, because the transition into that phase already happened — possibly in
// another process — and CreateMigration is not to be called twice.
//
// An empty phase is a migration nobody has reconciled yet, which is already what
// [statemachine.Config.Initial] means, so it restores nothing.
//
// A phase carrying no deadline is re-armed with its full bound rather than left
// unbounded. That is the object written before status.phaseDeadline existed, or one
// whose deadline a hand edit dropped; granting it a fresh window costs an upgrade one
// extra window's patience, where leaving it unbounded would mean a migration that can
// wait forever — the exact failure the bound exists to prevent.
func (r *Reconciler) restorePhase(
	ctx context.Context,
	sm *statemachine.Machine[phase],
	vm *simplyblockv1alpha1.VolumeMigration,
) error {
	if vm.Status.Phase == "" {
		return nil
	}

	snap := statemachine.Snapshot[phase]{State: vm.Status.Phase}
	switch {
	case vm.Status.PhaseDeadline != nil:
		snap.Deadline = vm.Status.PhaseDeadline.Time
	case phaseBound(vm.Status.Phase) > 0:
		snap.Deadline = time.Now().Add(phaseBound(vm.Status.Phase))
		logf.FromContext(ctx).Info("Phase carried no deadline; arming its full bound",
			"phase", vm.Status.Phase, "bound", phaseBound(vm.Status.Phase))
	}
	if err := sm.Restore(snap); err != nil {
		return fmt.Errorf("restoring phase %q: %w", vm.Status.Phase, err)
	}
	return nil
}

// persist writes the machine's position and everything the hooks changed in one
// status patch. One patch, at the end, because the phase and its deadline have to
// land together: a phase persisted without its deadline is a phase that can never
// time out, which is why [statemachine.Machine.Snapshot] returns both.
//
// A migration deleted mid-reconcile is not an error — there is nothing left to
// record.
func (r *Reconciler) persist(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
) error {
	snap := sm.Snapshot()
	p.vm.Status.Phase = snap.State
	p.vm.Status.PhaseDeadline = nil
	if !snap.Deadline.IsZero() {
		deadline := metav1.NewTime(snap.Deadline)
		p.vm.Status.PhaseDeadline = &deadline
	}

	if err := r.Status().Patch(ctx, p.vm, client.MergeFrom(p.before)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("patch status %s: %w", snap.State, err)
	}
	return nil
}

// requeueWithin shortens a step's requeue so that it does not sleep past the current
// phase's bound: a phase that runs out of time should be noticed when it does rather
// than one poll interval later.
//
// It only ever shortens. A step that asked for no requeue at all is waiting on a
// watch — a validation in progress is woken by its Jobs, which carry a deadline of
// their own no longer than what is left of the phase — and turning that into a timer
// would poll where the design deliberately does not. A phase whose bound has already
// passed is left alone too: it needs handling now, and controller-runtime reads a
// non-positive RequeueAfter as no requeue at all.
func requeueWithin(res ctrl.Result, sm *statemachine.Machine[phase]) ctrl.Result {
	remaining, ok := sm.RequeueAfter()
	if !ok || res.RequeueAfter <= 0 {
		return res
	}
	if remaining < res.RequeueAfter {
		res.RequeueAfter = remaining
	}
	return res
}
