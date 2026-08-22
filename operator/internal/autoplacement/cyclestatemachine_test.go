package autoplacement

import (
	"context"
	"testing"
	"time"

	"github.com/simplyblock/atlas/statemachine/smtest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/ctrltest"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// newCycle builds a reconciler and a cycle for one cluster, wired to a fake client with
// the StorageCluster status subresource enabled — the Migrating and Completed hooks
// patch status.rebalancing.
//
// The cluster name is the metric label, so each test passes its own: the evaluation
// counter is a package-level Prometheus vector shared by every test in the package.
func newCycle(t *testing.T, clusterName string, deadline time.Time) (*VolumeRebalancerReconciler, *cycle, client.Client) {
	t.Helper()

	cr := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "sb"},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: clusterName + "-uuid"},
	}
	scheme := ctrltest.NewScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := ctrltest.NewClient(t, scheme, []client.Object{&simplyblockv1alpha1.StorageCluster{}}, cr)

	r := &VolumeRebalancerReconciler{
		Client:         cl,
		Scheme:         scheme,
		Recorder:       events.NewFakeRecorder(16),
		migrationState: volumemigration.NewMigrationState(),
	}
	return r, &cycle{cluster: cr, clusterUUID: cr.Status.UUID, deadline: deadline}, cl
}

// evaluations reads the evaluation counter for one cluster and outcome straight out of
// the registry the controller registers into. Going through Gather rather than
// prometheus/testutil keeps this test from adding a module requirement, and it asserts
// on what a scrape would actually see.
func evaluations(t *testing.T, clusterName, outcome string) float64 {
	t.Helper()

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "simplyblock_rebalancer_evaluation_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if labels["cluster"] == clusterName && labels["result"] == outcome {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0 // never incremented for this pair, which is a legitimate zero
}

func rebalancingFlag(t *testing.T, cl client.Client, clusterName string) *bool {
	t.Helper()
	cr := &simplyblockv1alpha1.StorageCluster{}
	key := client.ObjectKey{Namespace: "sb", Name: clusterName}
	if err := cl.Get(context.Background(), key, cr); err != nil {
		t.Fatalf("get cluster: %v", err)
	}
	return cr.Status.Rebalancing
}

// A cycle fans out from Evaluating to its four outcomes, and no outcome is stranded.
func TestCycleGraph_ShapeAndReachability(t *testing.T) {
	r, c, _ := newCycle(t, "graph-cluster", time.Now().Add(time.Minute))
	sm := r.cycleMachine(t.Context(), c)
	defer sm.Close()

	smtest.Check(t, sm).
		In(cycleEvaluating).
		NotTerminal().
		NoDeadline().
		Allows(cycleMigrating, cycleDeferred, cycleDryRun, cycleFailed).
		Reachable()
}

// Migrating is the only phase that is neither start nor end, and Completed is its only
// exit — which is what makes "the rebalancing flag is always lowered again" structural
// rather than a deferred closure that a new return path could bypass.
func TestCycleGraph_MigratingExitsOnlyThroughCompleted(t *testing.T) {
	r, c, _ := newCycle(t, "exit-cluster", time.Now().Add(time.Minute))
	sm := r.cycleMachine(t.Context(), c)
	defer sm.Close()

	smtest.Check(t, sm).
		Enters(t.Context(), cycleMigrating).
		Allows(cycleCompleted).
		NotTerminal().
		Refuses(t.Context(), cycleDeferred).
		Refuses(t.Context(), cycleFailed).
		Refuses(t.Context(), cycleMigrating). // no self-edge: it would re-raise the flag
		Enters(t.Context(), cycleCompleted).
		Terminal()
}

// A cycle cannot report having migrated without having gone through Migrating.
func TestCycleGraph_RefusesCompletedWithoutMigrating(t *testing.T) {
	r, c, _ := newCycle(t, "skip-cluster", time.Now().Add(time.Minute))
	sm := r.cycleMachine(t.Context(), c)
	defer sm.Close()

	smtest.Check(t, sm).Refuses(t.Context(), cycleCompleted).In(cycleEvaluating)
}

// The point of the graph: every terminal phase records exactly one outcome, under the
// label the dashboards read. This is the invariant that a dozen hand-written Inc() call
// sites did not hold — one route recorded nothing at all.
func TestCycleOutcomes_EachTerminalRecordsExactlyOne(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase   cyclePhase
		outcome string
	}{
		{"deferred", cycleDeferred, outcomeSkipped},
		{"dry run", cycleDryRun, outcomeDryRun},
		{"failed", cycleFailed, outcomeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := "outcome-" + tc.name
			r, c, _ := newCycle(t, cluster, time.Now().Add(time.Minute))
			sm := r.cycleMachine(t.Context(), c)
			defer sm.Close()

			before := evaluations(t, cluster, tc.outcome)
			smtest.Check(t, sm).Enters(t.Context(), tc.phase).Terminal()

			if got := evaluations(t, cluster, tc.outcome) - before; got != 1 {
				t.Errorf("%s recorded %v %q outcome(s), want exactly 1", tc.phase, got, tc.outcome)
			}
			// And nothing else was recorded for this cluster.
			for _, other := range []string{outcomeSkipped, outcomeMigrated, outcomeDryRun, outcomeError} {
				if other == tc.outcome {
					continue
				}
				if got := evaluations(t, cluster, other); got != 0 {
					t.Errorf("%s also recorded %v %q outcome(s), want 0", tc.phase, got, other)
				}
			}
		})
	}
}

// Completed distinguishes a cycle that moved something from one that reached the
// migrating phase and created nothing — the same phase, two different outcomes.
func TestCycleCompleted_OutcomeDependsOnWhatMoved(t *testing.T) {
	for _, tc := range []struct {
		name     string
		migrated int
		outcome  string
	}{
		{"created migrations", 3, outcomeMigrated},
		{"created none", 0, outcomeSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := "completed-" + tc.name
			r, c, _ := newCycle(t, cluster, time.Now().Add(time.Minute))
			sm := r.cycleMachine(t.Context(), c)
			defer sm.Close()

			before := evaluations(t, cluster, tc.outcome)
			check := smtest.Check(t, sm).Enters(t.Context(), cycleMigrating)
			c.migrated = tc.migrated
			check.Enters(t.Context(), cycleCompleted)

			if got := evaluations(t, cluster, tc.outcome) - before; got != 1 {
				t.Errorf("recorded %v %q outcome(s) for %d migration(s), want exactly 1",
					got, tc.outcome, tc.migrated)
			}
		})
	}
}

// The flag goes up on the way into Migrating and comes down on the way into Completed.
func TestCycleMigrating_RaisesAndLowersTheRebalancingFlag(t *testing.T) {
	r, c, cl := newCycle(t, "flag-cluster", time.Now().Add(time.Minute))
	sm := r.cycleMachine(t.Context(), c)
	defer sm.Close()

	if got := rebalancingFlag(t, cl, "flag-cluster"); got != nil {
		t.Fatalf("status.rebalancing = %v before the cycle, want unset", *got)
	}

	check := smtest.Check(t, sm).Enters(t.Context(), cycleMigrating)
	if got := rebalancingFlag(t, cl, "flag-cluster"); got == nil || !*got {
		t.Errorf("status.rebalancing = %v while migrating, want true", got)
	}

	check.Enters(t.Context(), cycleCompleted)
	if got := rebalancingFlag(t, cl, "flag-cluster"); got == nil || *got {
		t.Errorf("status.rebalancing = %v after the cycle, want false", got)
	}
}

// Migrating is bounded by what is left of the cycle, so a cycle with more candidates
// than fit in an evaluation interval leaves the rest to the next one.
func TestCycleMigrating_CarriesTheRemainingCycleBudget(t *testing.T) {
	r, c, _ := newCycle(t, "budget-cluster", time.Now().Add(40*time.Second))
	sm := r.cycleMachine(t.Context(), c)
	defer sm.Close()

	smtest.Check(t, sm).
		Enters(t.Context(), cycleMigrating).
		DeadlineIn(40*time.Second, 2*time.Second).
		NotTimedOut()
}

// A cycle that spent its whole interval evaluating must create nothing. The bound has
// to survive that as a bound: returning a zero duration would tell the machine the
// phase has no deadline at all, which is the opposite of what an exhausted cycle means.
func TestCycleMigrating_ExhaustedCycleStillHasABound(t *testing.T) {
	r, c, _ := newCycle(t, "exhausted-cluster", time.Now().Add(-time.Minute))
	sm := r.cycleMachine(t.Context(), c)
	defer sm.Close()

	check := smtest.Check(t, sm).Enters(t.Context(), cycleMigrating)
	if _, ok := sm.Deadline(); !ok {
		t.Fatalf("Migrating has no deadline for an exhausted cycle; it would migrate everything")
	}
	// executeMigrations selects on Done() before each migration, so it must already be
	// closed rather than merely due.
	select {
	case <-sm.Done():
	case <-time.After(time.Second):
		t.Errorf("Migrating is not done for a cycle whose deadline has passed")
	}

	// And the cycle can still report its outcome: the deadline bounds the phase, not
	// the machine, so the transition out of it is still available.
	check.Enters(t.Context(), cycleCompleted).Terminal()
}

// A cycle that runs out of time mid-migration still records an outcome. This is why the
// machine is built on the reconcile's context rather than on a context carrying the
// cycle deadline — a closed machine refuses every transition, and the outcome would be
// lost exactly on the path where the cycle did the most work.
func TestCycleMachine_OutlivesItsOwnDeadline(t *testing.T) {
	r, c, cl := newCycle(t, "outlive-cluster", time.Now().Add(50*time.Millisecond))
	sm := r.cycleMachine(t.Context(), c)
	defer sm.Close()

	smtest.Check(t, sm).Enters(t.Context(), cycleMigrating)
	<-sm.Done() // the cycle's time is up

	before := evaluations(t, "outlive-cluster", outcomeSkipped)
	if err := sm.TransitionTo(t.Context(), cycleCompleted); err != nil {
		t.Fatalf("a cycle that ran out of time cannot report its outcome: %v", err)
	}
	if got := evaluations(t, "outlive-cluster", outcomeSkipped) - before; got != 1 {
		t.Errorf("recorded %v outcome(s) after the deadline passed, want 1", got)
	}
	if got := rebalancingFlag(t, cl, "outlive-cluster"); got == nil || *got {
		t.Errorf("status.rebalancing = %v, want it lowered even after the deadline", got)
	}
}

// endCycle is the only way to a terminal phase, so a caller cannot record an outcome
// without also saying why.
func TestEndCycleRecordsTheReason(t *testing.T) {
	r, c, _ := newCycle(t, "reason-cluster", time.Now().Add(time.Minute))
	sm := r.cycleMachine(t.Context(), c)
	defer sm.Close()

	res, err := r.deferCycle(t.Context(), c, sm, "cluster has offline node(s)", 25*time.Second)
	if err != nil {
		t.Fatalf("deferCycle: %v", err)
	}
	if res.RequeueAfter != 25*time.Second {
		t.Errorf("RequeueAfter = %v, want the caller's 25s", res.RequeueAfter)
	}
	if c.reason != "cluster has offline node(s)" {
		t.Errorf("reason = %q, want the one the caller gave", c.reason)
	}
	smtest.Check(t, sm).In(cycleDeferred).Terminal()
}
