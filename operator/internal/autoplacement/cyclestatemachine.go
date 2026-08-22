package autoplacement

import (
	"context"
	"time"

	"github.com/simplyblock/atlas/statemachine"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// One rebalancing evaluation cycle as a state machine. A cycle looks at the cluster's
// latency spread and either moves volumes or explains why it did not:
//
//	Evaluating ──┬── Deferred                        (skipped)
//	             ├── Migrating ── Completed          (migrated | skipped)
//	             ├── DryRun                          (dry_run)
//	             └── Failed                          (error)
//
// The four leaves are the four values of the `result` label on
// simplyblock_rebalancer_evaluation_total, which is what every dashboard and alert on
// rebalancing reads. Making them states puts the recording in one place per outcome —
// the state's entry hook — so a cycle can neither finish without an outcome nor be
// counted twice. It used to be a bare Inc() at each of a dozen return sites, and one
// of them had already been missed: a cluster whose UUID could not be resolved returned
// without recording anything, so auth failures were invisible in the metric while
// every other error path reported one.
//
// # Not persisted
//
// Unlike the VolumeMigration machine, this one is built and closed inside a single
// Reconcile and never written down. There is nothing to restore: the next reconcile
// starts a *new* cycle rather than resuming this one, and the cluster has no phase of
// its own to resume from. (status.rebalancing looks like one but is not — the control
// plane owns that field, and four other controllers write it from the API's own
// answer.) So [statemachine.Machine.Snapshot] and [statemachine.Machine.Restore] are
// deliberately unused here; what the machine is used for is the graph, the hooks, and
// the bound that Migrating carries.
type cyclePhase string

const (
	// cycleEvaluating is the gate-running phase: read the nodes, check for offline
	// ones, ask the rebalancer for candidates. Every other phase is entered from it.
	cycleEvaluating cyclePhase = "Evaluating"
	// cycleMigrating is creating VolumeMigration CRs. It is the only phase that is
	// neither initial nor terminal, and the only one that carries a deadline.
	cycleMigrating cyclePhase = "Migrating"
	// cycleDeferred means a gate said no: nothing was wrong, and nothing moved.
	cycleDeferred cyclePhase = "Deferred"
	// cycleDryRun means candidates were found and deliberately not acted on.
	cycleDryRun cyclePhase = "DryRun"
	// cycleCompleted means the cycle got as far as creating migrations, however many
	// that turned out to be.
	cycleCompleted cyclePhase = "Completed"
	// cycleFailed means the cycle could not finish evaluating.
	cycleFailed cyclePhase = "Failed"
)

// Outcome labels on simplyblock_rebalancer_evaluation_total. Kept next to the phases
// they belong to, because the pairing is the whole point of modelling the cycle.
const (
	outcomeSkipped  = "skipped"
	outcomeMigrated = "migrated"
	outcomeDryRun   = "dry_run"
	outcomeError    = "error"
)

// cycle is one evaluation of one cluster: what the hooks need in order to record the
// outcome, handed to them by whichever step decided on it.
type cycle struct {
	cluster *simplyblockv1alpha1.StorageCluster

	// clusterUUID is the backend cluster id, resolved early in the cycle and needed by
	// the Completed hook for the cool-down gauge. Empty on the routes that end before
	// it could be resolved.
	clusterUUID string

	// deadline is when the cycle is out of time — one evaluation interval from its
	// start. It bounds the Migrating phase, so a cycle with more candidates than fit
	// in an interval leaves the rest to the next one instead of running long.
	deadline time.Time

	// reason is why the cycle ended where it did, for the log line. The call site
	// already logged the specifics; this is what the hook can say about it.
	reason string

	// migrated is how many VolumeMigration CRs the Migrating phase created, read by
	// the Completed hook to tell "moved something" from "moved nothing".
	migrated int
}

// cycleMachine builds the machine for one cycle. The hooks close over c, so it belongs
// to a single reconcile.
//
// ctx is the reconcile's context and bounds the machine. Note that it is deliberately
// *not* given the cycle's deadline: the machine has to outlive that deadline, because
// a cycle that runs out of time still has to record its outcome, and
// [statemachine.Machine.TransitionTo] refuses to move a machine whose own context is
// done. The cycle bound therefore lives on the Migrating state, which is the only
// phase where running long is possible.
func (r *VolumeRebalancerReconciler) cycleMachine(
	ctx context.Context,
	c *cycle,
) *statemachine.Machine[cyclePhase] {
	return statemachine.Must(ctx, statemachine.Config[cyclePhase]{
		Initial: cycleEvaluating,
		States: map[cyclePhase]statemachine.StateDef[cyclePhase]{
			// No hook: a cycle is born evaluating, and the machine does not run the
			// initial state's hook at construction.
			cycleEvaluating: {
				To: []cyclePhase{cycleMigrating, cycleDeferred, cycleDryRun, cycleFailed},
			},
			// Migrating cannot fail as a whole — a VolumeMigration that cannot be
			// created is logged and skipped per volume — so Completed is its only exit.
			cycleMigrating: {
				To:      []cyclePhase{cycleCompleted},
				OnEnter: r.onCycleMigrating(c),
			},
			cycleDeferred:  {OnEnter: r.onCycleEnded(c, outcomeSkipped)},
			cycleDryRun:    {OnEnter: r.onCycleEnded(c, outcomeDryRun)},
			cycleFailed:    {OnEnter: r.onCycleEnded(c, outcomeError)},
			cycleCompleted: {OnEnter: r.onCycleCompleted(c)},
		},
	})
}

// onCycleMigrating marks the cluster as rebalancing and bounds the phase by what is
// left of the cycle.
//
// The flag going on here and off again in [VolumeRebalancerReconciler.onCycleCompleted]
// is the pairing that used to be a deferred closure: entering Migrating is the only way
// to set it, and Migrating's only exit is Completed, so it cannot be left on by a
// return path that forgot.
//
// Setting it is best effort, as it was before. The flag is advisory — other controllers
// read it to avoid disruptive work during a rebalance — and failing to set it is not a
// reason to abandon migrations the cycle has already decided on.
func (r *VolumeRebalancerReconciler) onCycleMigrating(c *cycle) statemachine.TransitionFunc[cyclePhase] {
	return func(ctx context.Context, _, _ cyclePhase) (time.Duration, error) {
		if err := r.setRebalancing(ctx, c.cluster, true); err != nil {
			logf.FromContext(ctx).Error(err, "Failed to set status.rebalancing=true")
		}

		// What is left of the cycle. Floored just above zero rather than returning
		// zero, because zero means "no deadline" to the machine — and a cycle that has
		// already spent its interval evaluating must create nothing, not lose its bound
		// altogether.
		remaining := time.Until(c.deadline)
		if remaining <= 0 {
			remaining = time.Nanosecond
		}
		return remaining, nil
	}
}

// onCycleCompleted closes out a cycle that created migrations: the flag comes off, the
// cool-down gauge is refreshed, and the outcome distinguishes a cycle that actually
// moved something from one that ran out of time before it could.
func (r *VolumeRebalancerReconciler) onCycleCompleted(c *cycle) statemachine.TransitionFunc[cyclePhase] {
	return func(ctx context.Context, _, _ cyclePhase) (time.Duration, error) {
		log := logf.FromContext(ctx)

		if err := r.setRebalancing(ctx, c.cluster, false); err != nil {
			log.Error(err, "Failed to clear status.rebalancing")
		}
		if c.clusterUUID != "" {
			active := r.migrationState.GetCooldownCountByCluster(c.clusterUUID, time.Now())
			SetCooldownVolumes(c.clusterUUID, float64(active))
		}

		outcome := outcomeMigrated
		if c.migrated == 0 {
			outcome = outcomeSkipped
		}
		rebalancerEvaluationTotal.WithLabelValues(c.cluster.Name, outcome).Inc()
		log.V(1).Info("Rebalancing cycle finished", "cluster", c.cluster.Name,
			"outcome", outcome, "migrations", c.migrated)
		return 0, nil
	}
}

// onCycleEnded records a cycle that ended without creating anything. One hook serves
// Deferred, DryRun and Failed because the three differ only in the label they report —
// the call site that chose the phase has already logged what happened in its own terms.
func (r *VolumeRebalancerReconciler) onCycleEnded(
	c *cycle,
	outcome string,
) statemachine.TransitionFunc[cyclePhase] {
	return func(ctx context.Context, _, to cyclePhase) (time.Duration, error) {
		rebalancerEvaluationTotal.WithLabelValues(c.cluster.Name, outcome).Inc()
		logf.FromContext(ctx).V(1).Info("Rebalancing cycle ended", "cluster", c.cluster.Name,
			"phase", to, "outcome", outcome, "reason", c.reason)
		return 0, nil
	}
}

// endCycle moves the cycle to a terminal phase, recording reason and answering with the
// requeue the caller should return. The three wrappers below name the outcomes so a
// call site reads as the decision it is making rather than as a transition.
func (r *VolumeRebalancerReconciler) endCycle(
	ctx context.Context,
	c *cycle,
	sm *statemachine.Machine[cyclePhase],
	to cyclePhase,
	reason string,
	requeue time.Duration,
) (ctrl.Result, error) {
	c.reason = reason
	if err := sm.TransitionTo(ctx, to); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// deferCycle ends a cycle that found nothing to do, or was not in a position to look.
func (r *VolumeRebalancerReconciler) deferCycle(
	ctx context.Context,
	c *cycle,
	sm *statemachine.Machine[cyclePhase],
	reason string,
	requeue time.Duration,
) (ctrl.Result, error) {
	return r.endCycle(ctx, c, sm, cycleDeferred, reason, requeue)
}

// failCycle ends a cycle that could not finish evaluating, which is a different thing
// from one that evaluated and chose not to move anything.
func (r *VolumeRebalancerReconciler) failCycle(
	ctx context.Context,
	c *cycle,
	sm *statemachine.Machine[cyclePhase],
	reason string,
	requeue time.Duration,
) (ctrl.Result, error) {
	return r.endCycle(ctx, c, sm, cycleFailed, reason, requeue)
}

// dryRunCycle ends a cycle that found candidates and was configured not to act on them.
func (r *VolumeRebalancerReconciler) dryRunCycle(
	ctx context.Context,
	c *cycle,
	sm *statemachine.Machine[cyclePhase],
	requeue time.Duration,
) (ctrl.Result, error) {
	return r.endCycle(ctx, c, sm, cycleDryRun, "migrationEnabled=false", requeue)
}
