// What provisioning does while the worker it is adding is not there.
//
// A worker is cordoned, drained, rebooted and uncordoned whenever a
// MachineConfig reaches it, and the storage pool's own config is one: CPU
// isolation and huge pages are applied to a node by rebooting it, so the first
// node of a fresh cluster is rebooted in the middle of being added. The step
// that was running keeps its deadline through all of it and the control plane
// keeps being asked for a node the worker cannot produce.

package node

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// aWorkerIn is a Kubernetes node in the state the arguments describe. It is
// separate from aWorker because these cases turn on the cordon and the system
// UUID, which that fixture does not carry.
func aWorkerIn(ready bool, cordoned bool) *corev1.Node {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: workerKubernetesIP}},
			NodeInfo:   corev1.NodeSystemInfo{SystemUUID: workerSystemUUID},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
		},
	}
}

// aProvisioningNode is a node partway through the path, at the step given.
func aProvisioningNode(step nodeStep, deadline time.Duration) *simplyblockv1alpha2.StorageNode {
	node := aResolvingNode()
	node.Status.Step.State = string(step)
	at := metav1.NewTime(time.Now().Add(deadline))
	node.Status.Step.Deadline = &at
	return node
}

// aProvisioner drives the provisioning path over one worker.
func aProvisioner(t *testing.T, worker *corev1.Node, node *simplyblockv1alpha2.StorageNode) (
	*StorageNodeReconciler, *simplyblockv1alpha2.StorageCluster, client.Client, *int,
) {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	cluster := aClusterWithTasks()
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(worker, cluster, node).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNode{}).
		Build()

	adds := 0
	return &StorageNodeReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(64),
		API:      countingBackend{adds: &adds},
	}, cluster, apiClient, &adds
}

// stepOf reads back the step the provisioning pass recorded.
func stepOf(t *testing.T, apiClient client.Client, node *simplyblockv1alpha2.StorageNode) nodeStep {
	t.Helper()
	var read simplyblockv1alpha2.StorageNode
	key := client.ObjectKeyFromObject(node)
	if err := apiClient.Get(context.Background(), key, &read); err != nil {
		t.Fatalf("reading the node back: %v", err)
	}
	return nodeStep(read.Status.Step.State)
}

// TestAWorkerThatWentAwayHoldsTheNodeRatherThanFailingIt covers the reboot a
// MachineConfig performs in the middle of an add.
//
// Regression: 2026-09-20-a-rebooting-worker-burned-the-step-deadline — the
// storage pool's MachineConfig cordoned, drained and rebooted worker-5 while its
// node was being added. Nothing in the path represented that, so the step kept
// the deadline it entered with, the control plane kept being asked for a node,
// and the step would have failed the node for outliving a budget it spent
// waiting for a machine to come back.
func TestAWorkerThatWentAwayHoldsTheNodeRatherThanFailingIt(t *testing.T) {
	for _, away := range []struct {
		name   string
		worker *corev1.Node
	}{
		{"not ready", aWorkerIn(false, false)},
		{"cordoned", aWorkerIn(true, true)},
		{"drained and rebooting", aWorkerIn(false, true)},
	} {
		t.Run(away.name, func(t *testing.T) {
			node := aProvisioningNode(stepPosting, time.Minute)
			r, cluster, apiClient, adds := aProvisioner(t, away.worker, node)

			if _, err := r.provision(context.Background(), node, cluster); err != nil {
				t.Fatalf("provision: %v", err)
			}
			if got := stepOf(t, apiClient, node); got != stepAwaitingWorker {
				t.Errorf("the step is %q, want AwaitingWorker while the machine is not there", got)
			}
			if *adds != 0 {
				t.Errorf("the control plane was asked for a node %d time(s) on a machine that is not there", *adds)
			}
		})
	}
}

// The deadline of the step it left does not keep running: entering the new step
// sets that step's own budget, which is what makes the wait a wait rather than a
// countdown to failure.
func TestTheHeldNodeGetsAFreshBudget(t *testing.T) {
	node := aProvisioningNode(stepPosting, 5*time.Second)
	r, cluster, apiClient, _ := aProvisioner(t, aWorkerIn(false, true), node)

	if _, err := r.provision(context.Background(), node, cluster); err != nil {
		t.Fatalf("provision: %v", err)
	}

	var read simplyblockv1alpha2.StorageNode
	if err := apiClient.Get(context.Background(), client.ObjectKeyFromObject(node), &read); err != nil {
		t.Fatalf("reading the node back: %v", err)
	}
	if got := nodeStep(read.Status.Step.State); got != stepAwaitingWorker {
		t.Fatalf("the step is %q, so this is not measuring the held step's budget", got)
	}
	if read.Status.Step.Deadline == nil {
		t.Fatal("the held step carries no deadline, so it can never be given up on")
	}
	if left := time.Until(read.Status.Step.Deadline.Time); left <= 5*time.Second {
		t.Errorf("the held step inherited %v, so it is still counting down the step it left", left)
	}
}

// A worker that came back starts the path again from its first step, which is
// where a backend node that was created before the reboot is adopted rather than
// added a second time.
func TestAWorkerThatCameBackRestartsThePath(t *testing.T) {
	node := aProvisioningNode(stepAwaitingWorker, time.Hour)
	r, cluster, apiClient, _ := aProvisioner(t, aWorkerIn(true, false), node)

	if _, err := r.provision(context.Background(), node, cluster); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := stepOf(t, apiClient, node); got != stepCheckingHost {
		t.Errorf("the step is %q, want CheckingHost so the path is walked again", got)
	}
}

// While it is still away the node stays put, and says so: a step that holds
// without an event is indistinguishable from a reconcile that never ran.
func TestTheHeldNodeStaysAndSaysSo(t *testing.T) {
	node := aProvisioningNode(stepAwaitingWorker, time.Hour)
	r, cluster, apiClient, _ := aProvisioner(t, aWorkerIn(false, true), node)

	if _, err := r.provision(context.Background(), node, cluster); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := stepOf(t, apiClient, node); got != stepAwaitingWorker {
		t.Errorf("the step is %q, want it to stay at AwaitingWorker", got)
	}

	recorder, ok := r.Recorder.(*events.FakeRecorder)
	if !ok {
		t.Fatal("the recorder is not the fake one")
	}
	select {
	case line := <-recorder.Events:
		if !strings.Contains(line, WorkerAway) {
			t.Errorf("the event was %q, want one naming %s", line, WorkerAway)
		}
	default:
		t.Error("the node was held with no event, so nothing says why it is not progressing")
	}
}

// A node that has not claimed its worker is held where it is rather than
// diverted, because a claimed sibling on the same worker is read as an add that
// already happened. It still refuses the slot, which is what stops a second
// worker being given a configuration change while the first is rebooting.
func TestAnUnclaimedNodeTakesNoSlotWhileItsWorkerIsAway(t *testing.T) {
	node := aProvisioningNode(stepAwaitingSlot, time.Hour)
	r, cluster, apiClient, adds := aProvisioner(t, aWorkerIn(false, true), node)

	if _, err := r.provision(context.Background(), node, cluster); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := stepOf(t, apiClient, node); got != stepAwaitingSlot {
		t.Errorf("the step is %q, want it to wait at AwaitingSlot", got)
	}
	if *adds != 0 {
		t.Errorf("a node-add was posted %d time(s) for a machine that is not there", *adds)
	}
}

// The claim survives the wait, so the cluster's node-add cap stays closed and no
// sibling starts an add of its own while a worker reboots.
func TestAHeldNodeKeepsItsClaim(t *testing.T) {
	held := aProvisioningNode(stepAwaitingWorker, time.Hour)
	if !claimedWorker(held) {
		t.Error("a node held for its worker released its claim, so a sibling may start an add beside it")
	}
}

// A sibling of a held node stays at AwaitingSlot: the cap counts the held node,
// so the sibling's worker is not handed a configuration change alongside the
// reboot already running.
func TestASiblingWaitsWhileAnotherWorkerIsHeld(t *testing.T) {
	held := aProvisioningNode(stepAwaitingWorker, time.Hour)
	held.Name = "a-cluster-worker-9-0"
	held.Spec.WorkerNode = "worker-9"

	waiting := aProvisioningNode(stepAwaitingSlot, time.Hour)
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	cluster := aClusterWithTasks()
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(aWorkerIn(true, false), &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-9"},
		}, cluster, held, waiting).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNode{}).
		Build()
	adds := 0
	r := &StorageNodeReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(64),
		API:      countingBackend{adds: &adds},
	}

	if _, err := r.provision(context.Background(), waiting, cluster); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := stepOf(t, apiClient, waiting); got != stepAwaitingSlot {
		t.Errorf("the sibling moved to %q while another worker was held", got)
	}
	if adds != 0 {
		t.Errorf("the sibling posted %d add(s) while another worker was rebooting", adds)
	}
}

// TestAnAddThatGaveUpIsActuallyReposted covers the retry the resolve step asks
// for, through the transition that performs it.
//
// Regression: 2026-09-20-resolving-could-not-reach-posting — resolve returns
// Posting when the add it was waiting on left the task window without producing
// a node, and the graph gave Resolving no exit, so the transition was refused:
//
//	enter step Posting: statemachine: illegal transition Resolving -> Posting
//
// The reconcile errored before re-posting and backed off, so the node emitted
// NodeAddGaveUp once a second for its whole deadline while exactly one add was
// ever sent. The step that exists to ask again could not.
//
// TestResolvingAsksAgainWhenTheAddIsOver covers the same decision one level
// down, and passed throughout: it reads the step resolve returns and never
// performs the transition, which is the only place the graph is consulted.
func TestAnAddThatGaveUpIsActuallyReposted(t *testing.T) {
	node := aProvisioningNode(stepResolving, time.Hour)
	r, cluster, apiClient, _ := aProvisioner(t, aWorkerIn(true, false), node)
	cluster.Status.Tasks = []simplyblockv1alpha2.ClusterTask{
		{ID: "task-1", Type: "node_add", Status: "done"},
	}

	if _, err := r.provision(context.Background(), node, cluster); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := stepOf(t, apiClient, node); got != stepPosting {
		t.Errorf("the step is %q, want Posting so the add is actually asked for again", got)
	}
}

// theAddTask is the ID the fake control plane gives the task of every add.
const theAddTask = "task-1"

// nodeAddTaskBackend answers every task read with one task.
type nodeAddTaskBackend struct {
	countingBackend
	task TaskReading
}

func (b nodeAddTaskBackend) Task(context.Context, string, string) (TaskReading, error) {
	return b.task, nil
}

// Regression: 2026-10-02-node-add-suspended-invisible — a node_add the control plane
// had restarted and suspended was read as work in progress, so the node showed
// only the step it waited on, with no event, for its whole deadline. The task
// is the node's own: it was read by the ID the add returned.
func TestAFailingAddIsReportedOnTheNodeThatPostedIt(t *testing.T) {
	node := aProvisioningNode(stepResolving, time.Hour)
	node.Status.NodeAddTaskID = theAddTask
	r, cluster, apiClient, adds := aProvisioner(t, aWorkerIn(true, false), node)
	r.API = nodeAddTaskBackend{
		countingBackend: countingBackend{adds: adds},
		task:            TaskReading{Status: "suspended", Retry: 5, Result: "Node add result: False: no failure domain"},
	}

	if _, err := r.provision(context.Background(), node, cluster); err != nil {
		t.Fatalf("provision: %v", err)
	}

	var read simplyblockv1alpha2.StorageNode
	if err := apiClient.Get(context.Background(), client.ObjectKeyFromObject(node), &read); err != nil {
		t.Fatalf("reading the node back: %v", err)
	}
	if want := "task task-1 failed: Node add result: False: no failure domain"; !strings.Contains(read.Status.Message, want) {
		t.Errorf("status.message = %q, want it to contain %q", read.Status.Message, want)
	}
	if line := <-r.Recorder.(*events.FakeRecorder).Events; !strings.Contains(line, NodeAddFailing) {
		t.Errorf("the event was %q, want one naming %s", line, NodeAddFailing)
	}
}

// The node keeps the ID the add returned, which is what lets a later pass read
// that task and no other.
func TestThePostedAddsTaskIsRecordedOnTheNode(t *testing.T) {
	node := aProvisioningNode(stepPosting, time.Hour)
	r, cluster, apiClient, _ := aProvisioner(t, aWorkerIn(true, false), node)

	if _, err := r.provision(context.Background(), node, cluster); err != nil {
		t.Fatalf("provision: %v", err)
	}

	var read simplyblockv1alpha2.StorageNode
	if err := apiClient.Get(context.Background(), client.ObjectKeyFromObject(node), &read); err != nil {
		t.Fatalf("reading the node back: %v", err)
	}
	if read.Status.NodeAddTaskID != theAddTask {
		t.Errorf("status.nodeAddTaskID = %q, want the task the add returned", read.Status.NodeAddTaskID)
	}
}

// Regression: 2026-10-02-node-add-result-overflows-event — the task's result is
// free text and went into the event and the status message whole. An event note
// is at most 1 KiB, so a long result made the event fail to record.
func TestALongTaskResultIsClippedToWhatAnEventTakes(t *testing.T) {
	node := aProvisioningNode(stepResolving, time.Hour)
	node.Status.NodeAddTaskID = theAddTask
	r, cluster, apiClient, adds := aProvisioner(t, aWorkerIn(true, false), node)
	// Two-byte characters after an odd-length prefix, so that a cut at the limit
	// falls inside one.
	r.API = nodeAddTaskBackend{
		countingBackend: countingBackend{adds: adds},
		task:            TaskReading{Status: "suspended", Retry: 5, Result: "x" + strings.Repeat("é", 600)},
	}

	if _, err := r.provision(context.Background(), node, cluster); err != nil {
		t.Fatalf("provision: %v", err)
	}

	var read simplyblockv1alpha2.StorageNode
	if err := apiClient.Get(context.Background(), client.ObjectKeyFromObject(node), &read); err != nil {
		t.Fatalf("reading the node back: %v", err)
	}
	message := read.Status.Message
	if len(message) > 1024 {
		t.Errorf("status.message is %d bytes, want at most 1024", len(message))
	}
	if !utf8.ValidString(message) {
		t.Error("status.message was cut inside a character")
	}
	if !strings.HasPrefix(message, "node_add task task-1 failed: x") || !strings.HasSuffix(message, "…") {
		t.Errorf("status.message = %.60q…, want the task's result, clipped and marked", message)
	}
}
