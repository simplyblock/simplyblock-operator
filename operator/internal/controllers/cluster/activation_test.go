// Tests for an Activate whose attempt the control plane gave up on.
//
// The control plane accepts an activation with a 202 and runs it in a thread.
// When that thread fails, it puts the cluster back at the status it had before
// the attempt, and nothing else reports it: no error reaches the caller, and the
// cluster simply stops being in_activation. The cases below are that reading on
// either side of the line the reconciler draws between "the attempt is over" and
// "the attempt may not have started yet," plus the readings that must never be
// taken for a failed attempt.
//
// Regression: e2e run 36883427064, where the attempt failed after 300s on missing
// cross-node device connections and the operation waited out its 30-minute
// Awaiting deadline on a cluster nothing was activating any more.

package cluster

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// awaitingActivation is an Activate that requested its call `inStep` ago and
// took the cluster's lock `running` ago.
//
// The step's entry is not stored. It is the deadline minus the Awaiting budget,
// which is how the reconciler reads it back as well.
func awaitingActivation(inStep, running time.Duration) func(*simplyblockv1alpha2.StorageClusterOps) {
	return func(o *simplyblockv1alpha2.StorageClusterOps) {
		deadline := metav1.NewTime(time.Now().Add(awaitingDeadline - inStep))
		started := metav1.NewTime(time.Now().Add(-running))
		o.Status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
		o.Status.Step = statemachine.KubeSnapshot{State: string(stepAwaiting), Deadline: &deadline}
		o.Status.StartedAt = &started
	}
}

// lockedBy is a cluster whose lock the operation under test already holds.
func lockedBy(name string) func(*simplyblockv1alpha2.StorageCluster) {
	return func(c *simplyblockv1alpha2.StorageCluster) { c.Status.ActiveOpsRef = name }
}

// clusterReadingAt is the control plane answering with one status throughout.
func clusterReadingAt(status string) *fakeControlPlane {
	return &fakeControlPlane{cluster: func(string) (webapi.ClusterResponse, error) {
		reading := activeCluster()
		reading.Status = status
		return reading, nil
	}}
}

// activationRig is a 1+1 cluster with the three nodes its stripe needs, so the
// activation gates pass and only the control plane's reading decides.
func activationRig(
	t *testing.T, api *fakeControlPlane, rec *recorder,
	ops *simplyblockv1alpha2.StorageClusterOps,
) *StorageClusterOpsReconciler {
	t.Helper()
	return newOpsReconciler(t, api, rec,
		newTestCluster(withStripe(1, 1), lockedBy(testOpsName)),
		ops,
		nodeOfTestCluster("node-1", "worker-1"),
		nodeOfTestCluster("node-2", "worker-2"),
		nodeOfTestCluster("node-3", "worker-3"))
}

// The incident: six minutes after the call, the cluster is back at unready. The
// attempt is over and failed, so the call is made again, once, and the
// operation is waiting on the new attempt.
func TestAnActivationTheControlPlaneGaveUpOnIsRequestedAgain(t *testing.T) {
	api := clusterReadingAt(utils.ClusterStatusUnready)
	rec := &recorder{}
	r := activationRig(t, api, rec, newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate,
		awaitingActivation(6*time.Minute, 7*time.Minute)))

	// One pass to see the attempt has ended, one to request again, and one to
	// enter Awaiting for the new attempt.
	ops, _ := reconcileOps(t, r, 3)
	if api.activateCalls != 1 {
		t.Errorf("the control plane was asked to activate %d times, want 1 retry", api.activateCalls)
	}
	if !rec.has(ActivationRetried) {
		t.Errorf("the retry emitted no %s; events: %+v", ActivationRetried, rec.events)
	}
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseRunning {
		t.Errorf("phase = %q, want Running on the second attempt (message: %s)",
			ops.Status.Phase, ops.Status.Message)
	}
	if got := ops.Status.Step.State; got != string(stepAwaiting) {
		t.Errorf("step = %q, want Awaiting on the second attempt", got)
	}
}

// A suspended cluster that was being re-activated reverts to suspended, which
// is the same outcome under the other pre-activation status.
func TestAReactivationThatRevertedToSuspendedIsRequestedAgain(t *testing.T) {
	api := clusterReadingAt(utils.ClusterStatusSuspended)
	r := activationRig(t, api, &recorder{}, newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate,
		awaitingActivation(6*time.Minute, 7*time.Minute)))

	reconcileOps(t, r, 3)
	if api.activateCalls != 1 {
		t.Errorf("the control plane was asked to activate %d times, want 1 retry", api.activateCalls)
	}
}

// Seconds after the 202 the cluster may still read unready: the control plane
// answers before its thread writes in_activation, and a cache is behind both.
// That reading says nothing about the attempt, so no second call is made.
func TestAnActivationJustRequestedIsNotRequestedAgain(t *testing.T) {
	api := clusterReadingAt(utils.ClusterStatusUnready)
	rec := &recorder{}
	r := activationRig(t, api, rec, newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate,
		awaitingActivation(5*time.Second, 10*time.Second)))

	ops, _ := reconcileOps(t, r, 3)
	if api.activateCalls != 0 {
		t.Errorf("the control plane was asked to activate %d times, want 0 inside the grace",
			api.activateCalls)
	}
	if got := ops.Status.Step.State; got != string(stepAwaiting) {
		t.Errorf("step = %q, want Awaiting", got)
	}
	if rec.has(ActivationRetried) {
		t.Error("a retry was reported inside the grace")
	}
}

// An attempt still running is waited on however long it has taken, because the
// control plane refuses a second activate on a cluster in_activation.
func TestAnActivationStillRunningIsNotRequestedAgain(t *testing.T) {
	api := clusterReadingAt("in_activation")
	r := activationRig(t, api, &recorder{}, newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate,
		awaitingActivation(20*time.Minute, 21*time.Minute)))

	reconcileOps(t, r, 3)
	if api.activateCalls != 0 {
		t.Errorf("the control plane was asked to activate %d times, want 0 while in_activation",
			api.activateCalls)
	}
}

// A degraded cluster is one an activation has already brought up. It is not a
// failed attempt, and activating it again would re-run the activation on a
// cluster that is serving.
func TestADegradedClusterIsNotActivatedAgain(t *testing.T) {
	api := clusterReadingAt("degraded")
	r := activationRig(t, api, &recorder{}, newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate,
		awaitingActivation(6*time.Minute, 7*time.Minute)))

	reconcileOps(t, r, 3)
	if api.activateCalls != 0 {
		t.Errorf("the control plane was asked to activate %d times, want 0 on a degraded cluster",
			api.activateCalls)
	}
}

// Retrying is bounded by how long the operation has been running. Past the
// budget, a failed attempt fails the operation, says why, and frees the lock.
func TestAnActivationPastItsRetryBudgetFails(t *testing.T) {
	api := clusterReadingAt(utils.ClusterStatusUnready)
	rec := &recorder{}
	r := activationRig(t, api, rec, newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate,
		awaitingActivation(6*time.Minute, activationRetryBudget+time.Minute)))

	ops, cluster := reconcileOps(t, r, 1)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed past the retry budget (message: %s)",
			ops.Status.Phase, ops.Status.Message)
	}
	if api.activateCalls != 0 {
		t.Errorf("the control plane was asked to activate %d times, want 0 past the budget",
			api.activateCalls)
	}
	if cluster.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock released", cluster.Status.ActiveOpsRef)
	}
	if !rec.has(OperationFailed) {
		t.Error("the failure emitted no OperationFailed")
	}
}

// withFailureDomains turns on failure-domain mode for a cluster.
func withFailureDomains(c *simplyblockv1alpha2.StorageCluster) {
	c.Spec.EnableFailureDomains = ptr.To(true)
}

// reportingDomain is a node the control plane reports in the given domain.
func reportingDomain(name, worker, domain string) *simplyblockv1alpha2.StorageNode {
	node := nodeOfTestCluster(name, worker)
	node.Status.FailureDomain = domain
	return node
}

// Regression: 2026-10-02-activation-reads-retired-nodeset — the activation gate
// read the domains from StorageNodeSet.status.nodes, which nothing writes any
// more, so it saw no domain on a cluster whose nodes all reported one. The
// operation held on FailureDomainNotReady until its step deadline and failed.
func TestAnActivationIsRequestedOnceTheNodesReportTheirDomains(t *testing.T) {
	api := clusterReadingAt(utils.ClusterStatusUnready)
	r := newOpsReconciler(t, api, &recorder{},
		newTestCluster(withStripe(1, 1), withFailureDomains, lockedBy(testOpsName)),
		newTestOps(simplyblockv1alpha2.StorageClusterOpsActionActivate),
		reportingDomain("node-1", "worker-1", "0"),
		reportingDomain("node-2", "worker-2", "1"),
		reportingDomain("node-3", "worker-3", "2"))

	reconcileOps(t, r, 6)
	if api.activateCalls != 1 {
		t.Errorf("the control plane was asked to activate %d times, want 1", api.activateCalls)
	}
}
