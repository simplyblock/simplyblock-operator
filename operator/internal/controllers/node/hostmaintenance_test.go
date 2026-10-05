// Surviving a Kubernetes worker being drained.
//
// The window runs backward from an ordinary disruption budget: one that allows no
// disruption at all is created *before* the backend node is shut down, so
// `kubectl drain` blocks on it while the node is taken down gracefully, and
// relaxing it is what lets the drain proceed. The ordering is the whole
// mechanism — a drain that reaches the pod before the budget exists kills the
// SPDK process under a running backend node, which is exactly what the action
// exists to prevent.
//
// The gate in front of it is cluster-wide and counted by worker rather than by
// operation, because two nodes on one host go into maintenance together and the
// pair is one worker's worth of unavailability.
//
// design-storagenode.md §10.

package node

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/controlplane"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// aWindow is a maintenance window on the fixture's node.
func aWindow(name string) *simplyblockv1alpha2.StorageNodeOps {
	return anOperation(name, simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance)
}

// aRunningWindow is somebody else's window, past Holding and therefore holding a
// slot, against a node of its own on the given worker.
func aRunningWindow(
	name, nodeName, worker string,
) (*simplyblockv1alpha2.StorageNodeOps, *simplyblockv1alpha2.StorageNode) {
	ops := aWindow(name)
	ops.Spec.NodeRef = nodeName
	ops.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseRunning
	ops.Status.Step = kubeStep(stepShuttingDown)

	node := anOpsNode()
	node.Name = nodeName
	node.Spec.WorkerNode = worker
	return ops, node
}

// allowing states how many workers the cluster can afford to have down at once,
// which is the smaller of what it asks for and the fault tolerance the control
// plane reports.
func allowing(t *testing.T, apiClient client.Client, windows int32) {
	t.Helper()
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsCluster}
	if err := apiClient.Get(context.Background(), key, &cluster); err != nil {
		t.Fatalf("reading the cluster: %v", err)
	}
	cluster.Status.MaxConcurrentWorkerRestarts = ptr.To(windows)
	if err := apiClient.Status().Update(context.Background(), &cluster); err != nil {
		t.Fatalf("stating the cluster's concurrency: %v", err)
	}
}

// held runs the gate and reports whether it held this window back.
func held(t *testing.T, r *StorageNodeOpsReconciler, ops *simplyblockv1alpha2.StorageNodeOps) bool {
	t.Helper()
	done, err := performing(t, r, ops, stepHolding)
	if err == nil {
		return !done
	}
	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("holding: %v", err)
	}
	if blocked.reason != MaintenanceQueued {
		t.Errorf("the hold is announced as %q, want %q", blocked.reason, MaintenanceQueued)
	}
	return true
}

// A cluster that says nothing about concurrency takes one worker at a time,
// which is the safe reading of a cluster whose fault tolerance is unknown.
func TestOneWorkerAtATimeIsWhatAnUnstatedConcurrencyMeans(t *testing.T) {
	other, itsNode := aRunningWindow("another-window", "another-node", "worker-9")
	r, _ := anOpsWorld(t, aControlPlane(), other, itsNode)

	if !held(t, r, aWindow("a-window")) {
		t.Error("a second worker entered maintenance although the cluster stated no concurrency")
	}
}

// What the cluster says it can afford is what the gate admits.
func TestTheClusterSaysHowManyWorkersMayBeDownAtOnce(t *testing.T) {
	other, itsNode := aRunningWindow("another-window", "another-node", "worker-9")
	r, apiClient := anOpsWorld(t, aControlPlane(), other, itsNode)
	allowing(t, apiClient, 2)

	if held(t, r, aWindow("a-window")) {
		t.Error("the window waited although the cluster allows two workers at once")
	}
}

// A window still in Holding holds no slot. Counting one would let a deployment
// deadlock with every window waiting for every other.
func TestAWindowStillWaitingHoldsNoSlot(t *testing.T) {
	queued, itsNode := aRunningWindow("another-window", "another-node", "worker-9")
	queued.Status.Step = kubeStep(stepHolding)
	r, _ := anOpsWorld(t, aControlPlane(), queued, itsNode)

	if held(t, r, aWindow("a-window")) {
		t.Error("a window queued behind the same gate was counted as occupying it")
	}
}

// A finished window holds nothing either, whatever its last step was.
func TestAFinishedWindowHoldsNoSlot(t *testing.T) {
	over, itsNode := aRunningWindow("another-window", "another-node", "worker-9")
	over.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded
	r, _ := anOpsWorld(t, aControlPlane(), over, itsNode)

	if held(t, r, aWindow("a-window")) {
		t.Error("a window that has finished was still counted as occupying a slot")
	}
}

// The count is by worker, so a sibling socket of the same host is not a second
// worker's worth of unavailability. This is the multi-socket case the gate
// exists to get right.
func TestASiblingSocketOfTheSameWorkerIsNotASecondWorker(t *testing.T) {
	sibling, itsNode := aRunningWindow("the-siblings-window", "the-sibling", opsWorker)
	r, _ := anOpsWorld(t, aControlPlane(), sibling, itsNode)

	if held(t, r, aWindow("a-window")) {
		t.Error("the second socket of the worker already in maintenance waited for itself")
	}
}

// Another cluster's maintenance is another cluster's business.
func TestAWindowInAnotherClusterDoesNotHoldThisOneBack(t *testing.T) {
	elsewhere, itsNode := aRunningWindow("another-clusters-window", "another-node", "worker-9")
	itsNode.Spec.ClusterRef = "another-cluster"
	r, _ := anOpsWorld(t, aControlPlane(), elsewhere, itsNode)

	if held(t, r, aWindow("a-window")) {
		t.Error("a window in another cluster held this one back")
	}
}

// The budget exists before the shutdown is issued, which is the ordering the
// whole action rests on.
func TestTheEvictionIsBlockedBeforeTheNodeIsTakenDown(t *testing.T) {
	api := aControlPlane()
	r, apiClient := anOpsWorld(t, api, aReadyStoragePod(opsWorker))

	done, err := performing(t, r, aWindow("a-window"), stepShuttingDown)
	if err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	if done {
		t.Error("the step finished while the node was still online")
	}
	if asked := api.asked("ShutdownNode"); asked != 1 {
		t.Errorf("ShutdownNode was issued %d time(s), want once", asked)
	}

	budget := budgetFor(t, apiClient)
	if budget == nil {
		t.Fatal("no budget holds the eviction, so a drain would evict the pod under a live node")
	}
	if budget.Spec.MaxUnavailable == nil || budget.Spec.MaxUnavailable.IntVal != 0 {
		t.Errorf("maxUnavailable = %v, want 0 while the node is being taken down",
			budget.Spec.MaxUnavailable)
	}
}

// A node that is already offline is where the shutdown was taking it.
func TestAnOfflineNodeNeedsNoShutdown(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusOffline)
	r, _ := anOpsWorld(t, api, aReadyStoragePod(opsWorker))

	done, err := performing(t, r, aWindow("a-window"), stepShuttingDown)
	if err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	if !done {
		t.Error("the step did not finish against a node that is already offline")
	}
	if asked := api.asked("ShutdownNode"); asked != 0 {
		t.Errorf("ShutdownNode was issued %d time(s) against an offline node", asked)
	}
}

// Mid-restart is not a state to shut down from: the call would be refused, and
// the node is on its way somewhere anyway.
func TestANodeMidRestartIsWaitedForRatherThanShutDown(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusInRestart)
	r, _ := anOpsWorld(t, api, aReadyStoragePod(opsWorker))

	done, err := performing(t, r, aWindow("a-window"), stepShuttingDown)
	if err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	if done {
		t.Error("the step finished against a node whose restart is still running")
	}
	if asked := api.asked("ShutdownNode"); asked != 0 {
		t.Errorf("ShutdownNode was issued %d time(s) into an in-flight restart", asked)
	}
}

// The node comes back on the host it left, and a restart is not issued against
// one that is already back or already on its way.
func TestTheNodeIsRestartedOnceTheHostIsBack(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusOffline)
	r, _ := anOpsWorld(t, api)

	done, err := performing(t, r, aWindow("a-window"), stepRestarting)
	if err != nil {
		t.Fatalf("restarting: %v", err)
	}
	if done {
		t.Error("the step finished before the node had come back")
	}
	if asked := api.asked("RestartNode"); asked != 1 {
		t.Errorf("RestartNode was issued %d time(s), want once", asked)
	}
	if api.restarts[0].NodeAddress != "" {
		t.Errorf("the restart names address %q; the node is coming back on the host it left",
			api.restarts[0].NodeAddress)
	}

	restarting, _ := anOpsWorld(t, aControlPlane().reporting(nodeStatusInRestart))
	if done, err := performing(t, restarting, aWindow("a-window"), stepRestarting); err != nil || done {
		t.Errorf("done, err = %v, %v; a restart in flight is waited for", done, err)
	}

	back := aControlPlane()
	online, _ := anOpsWorld(t, back)
	done, err = performing(t, online, aWindow("a-window"), stepRestarting)
	if err != nil {
		t.Fatalf("restarting: %v", err)
	}
	if !done {
		t.Error("the step did not finish against a node that is online again")
	}
	if asked := back.asked("RestartNode"); asked != 0 {
		t.Errorf("RestartNode was issued %d time(s) against a node already back", asked)
	}
}

// Cleanup takes the budget away, so the worker is drainable by the ordinary
// rules again. A budget left behind is what would make it undrainable forever.
func TestCleanupLeavesTheWorkerDrainableAgain(t *testing.T) {
	spdk := anSpdkPod(opsWorker, "4420")
	r, apiClient := anOpsWorld(t, aControlPlane(), aReadyStoragePod(opsWorker), spdk)
	ops := aWindow("a-window")

	if _, err := performing(t, r, ops, stepShuttingDown); err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	done, err := performing(t, r, ops, stepCleanup)
	if err != nil {
		t.Fatalf("cleaning up: %v", err)
	}
	if !done {
		t.Error("the step did not finish")
	}

	if budget := budgetFor(t, apiClient); budget != nil {
		t.Errorf("the budget %s outlived the window it belonged to", budget.Name)
	}
	if guarded(t, apiClient, spdk.Name) {
		t.Error("the pod still carries the window's label, which a later budget would select")
	}
}

// A step that belongs to another action is a hand-edited object or a downgrade,
// and neither resolves by reconciling again.
func TestAStepOfAnotherActionEndsTheWindow(t *testing.T) {
	r, _ := anOpsWorld(t, aControlPlane())

	_, err := r.performMaintenanceStep(context.Background(), aWindow("a-window"), stepPromoting)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for a step of another action", err)
	}
}

// budgetFor reads the window's budget on the fixture's worker, or reports that
// there is none.
func budgetFor(t *testing.T, apiClient client.Client) *policyv1.PodDisruptionBudget {
	t.Helper()
	var budget policyv1.PodDisruptionBudget
	key := client.ObjectKey{
		Namespace: opsNamespace,
		Name:      maintenanceBudgetName(opsCluster, opsWorker),
	}
	err := apiClient.Get(context.Background(), key, &budget)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the maintenance budget: %v", err)
	}
	return &budget
}

// kubeStep is a persisted step, which is what an operation somebody else is
// running carries in its status.
func kubeStep(s step) statemachine.KubeSnapshot {
	return statemachine.KubeSnapshot{State: string(s)}
}

// anSpdkPod is the pod the SPDK process itself runs in.
//
// It is what the window is about, and it is not the pod the node agent runs in:
// the control plane creates it directly, one per backend node, owned by nothing,
// so an ordinary drain evicts it and a budget can hold that eviction.
func anSpdkPod(worker, rpcPort string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "snode-spdk-pod-" + rpcPort + "-" + worker,
			Namespace: opsNamespace,
			Labels: map[string]string{
				"app":  "spdk-app-" + rpcPort,
				"role": utils.LabelSpdkProxyRole,
			},
		},
		Spec:   corev1.PodSpec{NodeName: worker},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// aWebAPIPod is one replica of the management API, and anFDBPod one FoundationDB
// process. Both are on the worker, and losing either mid-shutdown is what takes
// the control plane out from under the window that is driving it.
func aWebAPIPod(worker string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-webappapi-" + worker,
			Namespace: opsNamespace,
			Labels:    map[string]string{"app": controlplane.ComponentWebAPI},
		},
		Spec:   corev1.PodSpec{NodeName: worker},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func anFDBPod(worker string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-fdb-cluster-log-" + worker,
			Namespace: opsNamespace,
			Labels: map[string]string{
				utils.LabelFDBClusterName: controlplane.ComponentFDBCluster,
			},
		},
		Spec:   corev1.PodSpec{NodeName: worker},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// guarded reports whether one pod carries the window's budget label.
func guarded(t *testing.T, apiClient client.Client, name string) bool {
	t.Helper()
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: opsNamespace, Name: name}
	if err := apiClient.Get(context.Background(), key, &pod); err != nil {
		t.Fatalf("reading pod %s: %v", name, err)
	}
	return pod.Labels[maintenanceLabel] != ""
}

// Regression: 2026-09-29-maintenance-waits-on-the-node-agent — Releasing waited
// for the node agent's DaemonSet pod to leave the worker. `kubectl drain
// --ignore-daemonsets` never evicts that pod and the DaemonSet puts it straight
// back, so the step could only expire: every cordon cost a storage node fifteen
// minutes and left it offline, and HostMaintenance had never once got past
// Releasing on a real cluster.
func TestReleasingWaitsForTheSpdkPodRatherThanTheNodeAgent(t *testing.T) {
	spdk := anSpdkPod(opsWorker, "4420")
	r, apiClient := anOpsWorld(t, aControlPlane(), aReadyStoragePod(opsWorker), spdk)
	ops := aWindow("a-window")

	done, err := performing(t, r, ops, stepReleasing)
	if err != nil {
		t.Fatalf("releasing: %v", err)
	}
	if done {
		t.Error("the step finished while the SPDK pod was still on the worker")
	}

	if err := apiClient.Delete(context.Background(), spdk); err != nil {
		t.Fatalf("deleting the SPDK pod: %v", err)
	}

	// The node agent's pod is still there, because a drain never takes it and
	// the step whose turn is next needs it to answer.
	done, err = performing(t, r, ops, stepReleasing)
	if err != nil {
		t.Fatalf("releasing: %v", err)
	}
	if !done {
		t.Error("the step waits for a pod no drain evicts and no window can remove")
	}
}

// Regression: 2026-09-29-maintenance-guards-the-wrong-pod — the budget selected
// the node agent's DaemonSet pod, which a drain skips, while the pods a drain
// does evict carried nothing. The drain the window is built to hold finished in
// thirty-six seconds, taking the SPDK process with it mid-shutdown and the
// management API with it at the same time.
func TestTheBudgetGuardsThePodsADrainCanEvict(t *testing.T) {
	spdk := anSpdkPod(opsWorker, "4420")
	agent := aReadyStoragePod(opsWorker)
	r, apiClient := anOpsWorld(t, aControlPlane(),
		agent, spdk, aWebAPIPod(opsWorker), anFDBPod(opsWorker),
		anSpdkPod(opsTarget, "4422"))

	if _, err := performing(t, r, aWindow("a-window"), stepShuttingDown); err != nil {
		t.Fatalf("shutting down: %v", err)
	}

	for _, name := range []string{
		spdk.Name,
		"simplyblock-webappapi-" + opsWorker,
		"simplyblock-fdb-cluster-log-" + opsWorker,
	} {
		if !guarded(t, apiClient, name) {
			t.Errorf("%s is not guarded, so the drain evicts it while the node is going down", name)
		}
	}
	if guarded(t, apiClient, agent.Name) {
		t.Error("the node agent's pod is guarded, and a drain skips it whatever the budget says")
	}
	if guarded(t, apiClient, "snode-spdk-pod-4422-"+opsTarget) {
		t.Error("a pod on another worker is guarded by this worker's window")
	}
	if budget := budgetFor(t, apiClient); budget == nil ||
		budget.Spec.MaxUnavailable == nil || budget.Spec.MaxUnavailable.IntVal != 0 {
		t.Errorf("the budget is %v, want one that allows no disruption at all", budget)
	}
}

// Regression: 2026-09-29-maintenance-relaxes-rather-than-releases — the budget
// was relaxed to one disruption rather than removed. With several pods under it
// the first eviction drops the healthy count and the allowance returns to zero,
// and the SPDK pod has no replacement to restore it, so the drain the step just
// released would block on the budget forever.
func TestReleasingTakesTheBudgetAwayRatherThanRelaxingIt(t *testing.T) {
	r, apiClient := anOpsWorld(t, aControlPlane(),
		aReadyStoragePod(opsWorker), anSpdkPod(opsWorker, "4420"), aWebAPIPod(opsWorker))
	ops := aWindow("a-window")

	if _, err := performing(t, r, ops, stepShuttingDown); err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	if _, err := performing(t, r, ops, stepReleasing); err != nil {
		t.Fatalf("releasing: %v", err)
	}

	if budget := budgetFor(t, apiClient); budget != nil {
		t.Errorf("the budget %s outlived the step that lets the drain proceed, with %v",
			budget.Name, budget.Spec.MaxUnavailable)
	}
}

// Regression: 2026-09-29-maintenance-reissues-the-shutdown — a node already
// shutting down was neither waited for nor recognized, so every pass re-posted
// the shutdown and took the control plane's 409 as a step error. Eleven of them
// in two minutes on 2026-09-29, each one a backoff against a step with a budget
// to spend.
func TestAShutdownIsNotReissuedAgainstANodeAlreadyShuttingDown(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusInShutdown)
	r, _ := anOpsWorld(t, api, aReadyStoragePod(opsWorker), anSpdkPod(opsWorker, "4420"))

	done, err := performing(t, r, aWindow("a-window"), stepShuttingDown)
	if err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	if done {
		t.Error("the step finished against a node whose shutdown is still running")
	}
	if asked := api.asked("ShutdownNode"); asked != 0 {
		t.Errorf("ShutdownNode was issued %d time(s) into an in-flight shutdown", asked)
	}
}

// Regression: 2026-09-29-failed-maintenance-leaves-its-markers — nothing unwound
// a window that failed. The graph is a linear chain with no edge to Cleanup and
// unwinds() names no maintenance step, so the budget and the label stayed on the
// worker: thirteen hours after the run, worker-2 still carried both.
func TestAFailedWindowLeavesTheWorkerDrainable(t *testing.T) {
	spdk := anSpdkPod(opsWorker, "4420")
	ops := anAdvancingOperation("a-window",
		simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance, stepReleasing)
	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	ops.Status.Step.Deadline = &expired
	r, apiClient := anOpsWorld(t, aControlPlane(), ops, aReadyStoragePod(opsWorker), spdk)
	r.Workload.ManagerNode = opsWorker
	if err := r.Workload.BlockEviction(
		context.Background(), opsNamespace, opsCluster, opsWorker); err != nil {
		t.Fatalf("holding the eviction: %v", err)
	}
	if err := r.Workload.ProtectSelf(context.Background(), opsNamespace, opsWorker); err != nil {
		t.Fatalf("holding the manager's own eviction: %v", err)
	}
	lockedBy(t, apiClient, "a-window")

	pass(t, r, "a-window")

	if got := operationRead(t, apiClient, "a-window"); got.Status.Phase !=
		simplyblockv1alpha2.StorageNodeOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed past the deadline", got.Status.Phase)
	}
	if budget := budgetFor(t, apiClient); budget != nil {
		t.Errorf("the budget %s outlived the window that failed", budget.Name)
	}
	if guarded(t, apiClient, spdk.Name) {
		t.Error("the pod still carries the window's label, which a later budget would select")
	}
	var self policyv1.PodDisruptionBudget
	key := client.ObjectKey{Namespace: opsNamespace, Name: selfBudgetName}
	if err := apiClient.Get(context.Background(), key, &self); !apierrors.IsNotFound(err) {
		t.Errorf("the manager's own budget outlived the window that failed (err = %v)", err)
	}
}

// A multi-socket worker runs one window per socket, admitted together on
// purpose, and the budget and the label are the worker's rather than the
// node's. The first socket to take its node offline must not drop the guard
// the second socket's SPDK pod is still standing behind.
//
// Regression: 2026-09-29-a-sibling-window-drops-the-shared-budget (PR #582
// review). Releasing deleted the worker's budget unconditionally, so on a
// two-socket worker the drain could evict a live SPDK process as soon as
// either backend node went offline — the exact eviction the action exists to
// prevent.
func TestTheWorkersBudgetOutlivesTheFirstSocketToRelease(t *testing.T) {
	sibling, itsNode := aRunningWindow("the-siblings-window", "the-sibling", opsWorker)
	r, apiClient := anOpsWorld(t, aControlPlane(),
		sibling, itsNode, aReadyStoragePod(opsWorker),
		anSpdkPod(opsWorker, "4420"), anSpdkPod(opsWorker, "4422"))
	ops := aWindow("a-window")

	if _, err := performing(t, r, ops, stepShuttingDown); err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	if _, err := performing(t, r, ops, stepReleasing); err != nil {
		t.Fatalf("releasing: %v", err)
	}

	budget := budgetFor(t, apiClient)
	if budget == nil {
		t.Fatal("the budget went while the sibling socket's SPDK pod was still running")
	}
	if budget.Spec.MaxUnavailable == nil || budget.Spec.MaxUnavailable.IntVal != 0 {
		t.Errorf("maxUnavailable = %v, want the sibling still fully guarded",
			budget.Spec.MaxUnavailable)
	}

	// The sibling reaches Releasing too, and the last one out takes it down.
	sibling.Status.Step = kubeStep(stepReleasing)
	if err := apiClient.Status().Update(context.Background(), sibling); err != nil {
		t.Fatalf("advancing the sibling: %v", err)
	}
	if _, err := performing(t, r, ops, stepReleasing); err != nil {
		t.Fatalf("releasing: %v", err)
	}
	if budget := budgetFor(t, apiClient); budget != nil {
		t.Errorf("the budget %s outlived every window that needed it", budget.Name)
	}
}

// The same holds for the terminal teardown: a window that fails does not take
// a sibling's guard with it.
//
// Regression: 2026-09-29-a-sibling-window-drops-the-shared-budget (PR #582
// review).
func TestAFailedWindowLeavesASiblingsGuardStanding(t *testing.T) {
	sibling, itsNode := aRunningWindow("the-siblings-window", "the-sibling", opsWorker)
	spdk := anSpdkPod(opsWorker, "4420")
	ops := anAdvancingOperation("a-window",
		simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance, stepShuttingDown)
	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	ops.Status.Step.Deadline = &expired
	r, apiClient := anOpsWorld(t, aControlPlane(), ops, sibling, itsNode,
		aReadyStoragePod(opsWorker), spdk)
	if err := r.Workload.BlockEviction(
		context.Background(), opsNamespace, opsCluster, opsWorker); err != nil {
		t.Fatalf("holding the eviction: %v", err)
	}
	lockedBy(t, apiClient, "a-window")

	pass(t, r, "a-window")

	if got := operationRead(t, apiClient, "a-window"); got.Status.Phase !=
		simplyblockv1alpha2.StorageNodeOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed past the deadline", got.Status.Phase)
	}
	if budgetFor(t, apiClient) == nil {
		t.Error("the failed window took the sibling's budget with it")
	}
	if !guarded(t, apiClient, spdk.Name) {
		t.Error("the failed window unlabeled a pod the sibling's budget selects")
	}
}

// The teardown is best-effort, and best-effort means both halves are tried. A
// manager budget that cannot be deleted is no reason to leave the worker's
// budget standing, which is the one that makes it undrainable.
//
// Regression: 2026-09-29-terminal-teardown-stops-at-the-first-error (PR #582
// review).
func TestTheTerminalTeardownTakesDownWhatItCan(t *testing.T) {
	refuseSelfBudget := interceptor.Funcs{
		Delete: func(
			ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption,
		) error {
			if object.GetName() == selfBudgetName {
				return errors.New("the API server refused the manager's own budget")
			}
			return c.Delete(ctx, object, opts...)
		},
	}
	ops := anAdvancingOperation("a-window",
		simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance, stepReleasing)
	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	ops.Status.Step.Deadline = &expired
	r, apiClient := anOpsWorldWith(t, aControlPlane(), refuseSelfBudget, ops,
		aReadyStoragePod(opsWorker), anSpdkPod(opsWorker, "4420"))
	r.Workload.ManagerNode = opsWorker
	if err := r.Workload.BlockEviction(
		context.Background(), opsNamespace, opsCluster, opsWorker); err != nil {
		t.Fatalf("holding the eviction: %v", err)
	}
	if err := r.Workload.ProtectSelf(context.Background(), opsNamespace, opsWorker); err != nil {
		t.Fatalf("holding the manager's own eviction: %v", err)
	}
	lockedBy(t, apiClient, "a-window")

	pass(t, r, "a-window")

	if budget := budgetFor(t, apiClient); budget != nil {
		t.Errorf("the worker's budget %s was left standing by an unrelated failure",
			budget.Name)
	}
	if !announcedReason(r, MaintenanceMarkersLeft) {
		t.Error("nothing announced the marker the teardown could not remove")
	}
}

// A node the teardown cannot read is the case where the markers are certain to
// be left and nothing else will come back for them, so it is the case that most
// needs announcing.
//
// Regression: 2026-09-29-terminal-teardown-is-silent-on-an-unreadable-node
// (PR #582 review).
// The teardown is driven directly here rather than through a reconcile. The
// path that reaches it with an unreadable node is the node being deleted
// between the lock being taken and the deadline being noticed, and a whole
// reconcile cannot be made to stage that without an interceptor counting reads,
// which pins the number of times the node happens to be fetched. The contract
// is the event, and this asserts the event.
func TestTheTerminalTeardownAnnouncesANodeItCannotRead(t *testing.T) {
	ops := anAdvancingOperation("a-window",
		simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance, stepReleasing)
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)
	node := &simplyblockv1alpha2.StorageNode{}
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}
	if err := apiClient.Get(context.Background(), key, node); err != nil {
		t.Fatalf("reading the node: %v", err)
	}
	if err := apiClient.Delete(context.Background(), node); err != nil {
		t.Fatalf("deleting the node: %v", err)
	}

	r.clearMaintenanceMarkers(context.Background(), ops)

	if !announcedReason(r, MaintenanceMarkersLeft) {
		t.Error("a teardown that could not read its node said nothing about the markers")
	}
}

// Regression: 2026-10-05-maintenance-touches-removal — a removal that failed
// releases the node's lock with the node left removed_failed, and a
// maintenance window on its worker then shut it down and restarted it back
// into service. A node leaving the cluster belongs to its removal: the window
// still guards and releases the worker, and sends the node nothing.
func TestAMaintenanceWindowSendsNothingToANodeLeavingTheCluster(t *testing.T) {
	for _, status := range []string{
		nodeStatusPendingRemoval, nodeStatusMigratingLvols, nodeStatusRemovedFailed,
	} {
		t.Run(status, func(t *testing.T) {
			api := aControlPlane().reporting(status)
			r, _ := anOpsWorld(t, api, aReadyStoragePod(opsWorker))

			for _, at := range []step{stepShuttingDown, stepRestarting} {
				done, err := performing(t, r, aWindow("a-window"), at)
				if err != nil {
					t.Fatalf("%s: %v", at, err)
				}
				if !done {
					t.Errorf("%s did not finish against a node its removal owns", at)
				}
			}
			if shut, restarted := api.asked("ShutdownNode"), api.asked("RestartNode"); shut+restarted != 0 {
				t.Errorf("shutdown=%d restart=%d sent to a node leaving the cluster, want none",
					shut, restarted)
			}
		})
	}
}
