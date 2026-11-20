// What the deployment metrics say, and the two ways they would lie quietly.
//
// A counter incremented on every reconcile counts passes rather than events,
// which is the failure a validation counter is most prone to: a draft nobody
// fixes is re-validated every thirty seconds forever. And a duration measured
// from an instant nobody recorded is indistinguishable from a real one once it
// is in a histogram.
//
// Every case resets the series it asserts on. The metrics are package-level and
// the harness next door shares one namespace, so a test that read a running
// total would be asserting about the whole file's history.

package deployment

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aDraftNaming builds an unapproved document whose only fault is the worker it
// names, which is the validation failure a reviewer actually hits.
func aDraftNaming(worker string) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.NodeSets[0].Groups[0].Workers = []string{worker}
	})
}

// A draft nobody fixes is validated again every pass. The counter is about what
// reviewers keep hitting, so it counts the outcome rather than the reconcile.
func TestAValidationFailureIsCountedOncePerOutcome(t *testing.T) {
	validationFailuresTotal.Reset()
	config := aDraftNaming("worker-does-not-exist")
	r := reconcilerFor(t, config, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(config)}
	for pass := 0; pass < 3; pass++ {
		if _, err := r.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	counted := testutil.ToFloat64(
		validationFailuresTotal.WithLabelValues(theNamespace, WorkerNotFound))
	if counted != 1 {
		t.Errorf("three passes over one unfixed draft counted %v failures, want 1", counted)
	}
}

// The phase gauge answers for every document rather than only for the ones in
// the phase asked about, which is what makes "how many are stuck in Draft" a
// query rather than an absence.
func TestThePhaseGaugeIsOneForTheCurrentPhaseAndZeroForTheRest(t *testing.T) {
	config := aDocument(nil)
	config.Namespace = "phases"
	r := reconcilerFor(t, config)

	err := r.note(context.Background(), config,
		simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding, "creating the cluster")
	if err != nil {
		t.Fatalf("note: %v", err)
	}

	expanding := testutil.ToFloat64(configPhaseState.WithLabelValues(
		"phases", config.Name, string(simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding)))
	if expanding != 1 {
		t.Errorf("the Expanding series is %v, want 1", expanding)
	}
	draft := testutil.ToFloat64(configPhaseState.WithLabelValues(
		"phases", config.Name, string(simplyblockv1alpha2.ClusterDeploymentConfigPhaseDraft)))
	if draft != 0 {
		t.Errorf("the Draft series is %v for an expanding document, want 0", draft)
	}
}

// A deleted document's series would otherwise report the phase of something
// nobody can look at.
func TestADeletedDocumentStopsReportingAPhase(t *testing.T) {
	config := aDocument(nil)
	config.Namespace = "forgotten"
	r := reconcilerFor(t, config)

	if err := r.note(context.Background(), config,
		simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanded, "done"); err != nil {
		t.Fatalf("note: %v", err)
	}
	forgetConfig(config)

	if count := testutil.CollectAndCount(configPhaseState); count == 0 {
		t.Skip("nothing is published at all, so this proves nothing")
	}
	got := testutil.ToFloat64(configPhaseState.WithLabelValues("forgotten", config.Name,
		string(simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanded)))
	if got != 0 {
		t.Errorf("a forgotten document still reports %v", got)
	}
}

// The duration is from approval rather than from the document being written, and
// the only durable record of that instant is the status field.
func TestAnExpansionIsTimedFromTheInstantItStarted(t *testing.T) {
	expansionDurationSeconds.Reset()
	config := aDocument(nil)
	config.Namespace = "timed"
	started := metav1.NewTime(time.Now().Add(-90 * time.Second))
	config.Status.ExpansionStartedAt = &started
	config.Status.ClusterRef = theCluster
	r := reconcilerFor(t, config)

	if err := r.succeed(context.Background(), config); err != nil {
		t.Fatalf("succeed: %v", err)
	}

	if count := testutil.CollectAndCount(expansionDurationSeconds); count != 1 {
		t.Fatalf("the histogram holds %d series, want the one this expansion observed", count)
	}
}

// A document the operator was upgraded underneath has no recorded start, and a
// duration invented for it would be a sample nothing can tell from a real one.
func TestAnExpansionWithNoRecordedStartIsNotTimed(t *testing.T) {
	expansionDurationSeconds.Reset()
	config := aDocument(nil)
	config.Namespace = "untimed"
	config.Status.ClusterRef = theCluster
	r := reconcilerFor(t, config)

	if err := r.succeed(context.Background(), config); err != nil {
		t.Fatalf("succeed: %v", err)
	}

	if count := testutil.CollectAndCount(expansionDurationSeconds); count != 0 {
		t.Errorf("an expansion with no start was timed anyway (%d series)", count)
	}
}

// The counter is the fleet's growth, so it counts objects created and not
// objects the document describes. Creating nodes is idempotent and runs again on
// every retry of the step.
func TestOnlyTheNodesActuallyCreatedAreCounted(t *testing.T) {
	nodesCreatedTotal.Reset()
	config := aDocument(nil)
	config.Status.ClusterRef = theCluster
	cluster := aCluster(nil)
	objects := append(workers("worker-1", "worker-2"), config, cluster)
	r := reconcilerFor(t, objects...)

	for pass := 0; pass < 2; pass++ {
		if _, err := r.createNodes(context.Background(), config); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	counted := testutil.ToFloat64(nodesCreatedTotal.WithLabelValues(theNamespace))
	if counted != 2 {
		t.Errorf("two workers created over two passes counted %v nodes, want 2", counted)
	}
}

// The two discovery gauges are what the last run of a namespace concluded, and
// they are settled by different steps: which workers the run is about is decided
// in Inspecting, and how many devices survived the rules is only known once the
// plan is built in Writing.
func TestADiscoveryRunPublishesWhatItFound(t *testing.T) {
	r := newRunner(t, discoverRun(nil), worker("worker-1"), worker("worker-2"))

	r.step() // start
	r.step() // inspect

	workersFound := testutil.ToFloat64(discoveryWorkersFound.WithLabelValues(opsNamespace))
	if workersFound != 2 {
		t.Errorf("the run found %v workers, want 2", workersFound)
	}

	r.step() // probing: creates the Jobs
	for _, node := range []string{"worker-1", "worker-2"} {
		cm := reportConfigMap(t, node,
			"0000:5e:00.0", "0000:5f:00.0", "0000:af:00.0", "0000:b0:00.0")
		if err := r.client.Create(context.Background(), cm); err != nil {
			t.Fatalf("write a report: %v", err)
		}
	}
	r.step() // probing: sees the reports
	r.step() // writing

	// The placement chose one memory node, so two of each worker's four disks.
	devicesFound := testutil.ToFloat64(discoveryDevicesFound.WithLabelValues(opsNamespace))
	if devicesFound != 4 {
		t.Errorf("the run found %v devices, want the four the draft names", devicesFound)
	}
}

// A run that reached a terminal phase is counted with the outcome it reached and
// timed from when it started, which is the pair every other Ops kind publishes.
func TestATerminalRunIsCountedWithItsResult(t *testing.T) {
	operatorOperationsTotal.Reset()
	operatorOperationDurationSeconds.Reset()

	ops := discoverRun(nil)
	started := metav1.NewTime(time.Now().Add(-30 * time.Second))
	ops.Status.StartedAt = &started
	r := newRunner(t, ops, worker("worker-1"))

	if err := r.reconciler.succeed(context.Background(), ops); err != nil {
		t.Fatalf("succeed: %v", err)
	}

	counted := testutil.ToFloat64(operatorOperationsTotal.
		WithLabelValues(opsNamespace, string(ops.Spec.Action), "succeeded"))
	if counted != 1 {
		t.Errorf("a finished run counted %v, want 1", counted)
	}
	if series := testutil.CollectAndCount(operatorOperationDurationSeconds); series != 1 {
		t.Errorf("the duration histogram holds %d series, want the one this run observed", series)
	}
}
