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
