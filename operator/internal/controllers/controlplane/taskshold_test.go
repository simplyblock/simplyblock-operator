// The tests of the tasks hold: that the task runner's pod is neither rolled nor
// evicted while it is adding a storage node, and that the hold ends.

package controlplane

import (
	"context"
	"testing"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The literal is deliberate, as for the stamp: it is what an administrator
// reading the budget sees.
const holdSinceAnnotation = "storage.simplyblock.io/node-add-hold-since"

// theNode is the one storage node these tests add.
const theNode = "node-a"

func storageNodeIn(step simplyblockv1alpha2.StorageNodeStep) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: theNode, Namespace: testNamespace},
		Status: simplyblockv1alpha2.StorageNodeStatus{
			Phase: simplyblockv1alpha2.StorageNodePhaseProvisioning,
			Step:  statemachine.KubeSnapshot{State: string(step)},
		},
	}
}

func tasksBudget(t *testing.T, c client.Client) *policyv1.PodDisruptionBudget {
	t.Helper()
	var budget policyv1.PodDisruptionBudget
	key := client.ObjectKey{Namespace: testNamespace, Name: ComponentTasks}
	if err := c.Get(context.Background(), key, &budget); err != nil {
		t.Fatalf("read the task runner's budget: %v", err)
	}
	return &budget
}

func moveCoordinators(t *testing.T, c client.Client, data string) {
	t.Helper()
	cm := clusterFileConfigMap(data)
	if err := c.Update(context.Background(), cm); err != nil {
		t.Fatalf("change the cluster file: %v", err)
	}
}

func setStep(t *testing.T, c client.Client, step simplyblockv1alpha2.StorageNodeStep) {
	t.Helper()
	var node simplyblockv1alpha2.StorageNode
	key := client.ObjectKey{Namespace: testNamespace, Name: theNode}
	if err := c.Get(context.Background(), key, &node); err != nil {
		t.Fatalf("read %s: %v", theNode, err)
	}
	node.Status.Step.State = string(step)
	if err := c.Update(context.Background(), &node); err != nil {
		t.Fatalf("move %s to %s: %v", theNode, step, err)
	}
}

// Regression: 2026-11-25-node-add-restarted-by-roll — every drain of a
// MachineConfig rollout moved a coordinator, every move rolled the task runner,
// and the node add it was driving was started again five times.
func TestACoordinatorMoveDuringANodeAddDoesNotRollTheTaskRunner(t *testing.T) {
	ctx := context.Background()
	cp := localControlPlane()
	c := newClient(t, cp, clusterFileConfigMap("sb:abc@10.0.0.1:4500"),
		storageNodeIn(simplyblockv1alpha2.StorageNodeStepAwaitingSlot))
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	before := stampOf(t, c, ComponentTasks)

	setStep(t, c, simplyblockv1alpha2.StorageNodeStepResolving)
	moveCoordinators(t, c, "sb:def@10.0.0.2:4500")
	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply during the add: %v", err)
	}

	if got := stampOf(t, c, ComponentTasks); got != before {
		t.Errorf("the task runner was restamped %s -> %s while it was adding a node, which replaces "+
			"its pod and starts the add again", before, got)
	}
	if got := stampOf(t, c, ComponentWebAPI); got == before {
		t.Error("the management API kept its stamp across a coordinator move; only the task runner is held")
	}

	// The add settles, and the runner catches up with the file.
	setStep(t, c, "")
	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply after the add: %v", err)
	}
	if got, want := stampOf(t, c, ComponentTasks), stampOf(t, c, ComponentWebAPI); got != want {
		t.Errorf("the task runner carries %s after the add settled and the management API %s; "+
			"a held stamp that is never applied leaves the runner on a file that is gone", got, want)
	}
}

// Regression: 2026-11-25-node-add-restarted-by-roll — the runner had no budget,
// so the drain that moved the coordinator could also evict it.
func TestTheTaskRunnerIsNotEvictedWhileItAddsANode(t *testing.T) {
	ctx := context.Background()
	cp := localControlPlane()
	c := newClient(t, cp, clusterFileConfigMap("sb:abc@10.0.0.1:4500"),
		storageNodeIn(simplyblockv1alpha2.StorageNodeStepAwaitingSlot))
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	idle := tasksBudget(t, c)
	if got := idle.Spec.MaxUnavailable.IntValue(); got != 1 {
		t.Errorf("the budget allows %d disruptions with no add in flight; it has to allow one, "+
			"or every drain of the runner's host waits for nothing", got)
	}
	if _, held := idle.Annotations[holdSinceAnnotation]; held {
		t.Error("the budget records a hold with no add in flight")
	}

	for _, step := range []simplyblockv1alpha2.StorageNodeStep{
		simplyblockv1alpha2.StorageNodeStepPosting, simplyblockv1alpha2.StorageNodeStepResolving,
	} {
		setStep(t, c, step)
		if err := r.applyEverything(ctx, cp); err != nil {
			t.Fatalf("apply in %s: %v", step, err)
		}
		if got := tasksBudget(t, c).Spec.MaxUnavailable.IntValue(); got != 0 {
			t.Errorf("the budget allows %d disruptions while a node is %s", got, step)
		}
	}

	setStep(t, c, "")
	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply after the add: %v", err)
	}
	settled := tasksBudget(t, c)
	if got := settled.Spec.MaxUnavailable.IntValue(); got != 1 {
		t.Errorf("the budget still allows %d disruptions after the add settled", got)
	}
	if _, held := settled.Annotations[holdSinceAnnotation]; held {
		t.Error("the budget still records a hold after the add settled, so the next add would " +
			"inherit this one's age and not be held at all")
	}
}

// A node whose worker is away has left Resolving for AwaitingWorker. The drain
// it is waiting on is the one a held budget would refuse.
func TestANodeThatIsNotBeingAddedHoldsNothing(t *testing.T) {
	for _, step := range []simplyblockv1alpha2.StorageNodeStep{
		simplyblockv1alpha2.StorageNodeStepCheckingHost,
		simplyblockv1alpha2.StorageNodeStepAwaitingSlot,
		simplyblockv1alpha2.StorageNodeStepAwaitingWorker,
		"",
	} {
		cp := localControlPlane()
		c := newClient(t, cp, storageNodeIn(step))
		r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}
		hold, err := r.tasksHoldNow(context.Background(), testNamespace)
		if err != nil {
			t.Fatalf("%q: %v", step, err)
		}
		if hold.held || hold.since != nil {
			t.Errorf("a node in step %q holds the task runner", step)
		}
	}
}

// An add that never settles must not hold a drain for good: a MachineConfigPool
// whose drain is refused for an hour reports itself degraded.
func TestTheHoldOpensAfterItsLimit(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 19, 50, 0, 0, time.UTC)
	cp := localControlPlane()
	c := newClient(t, cp, clusterFileConfigMap("sb:abc@10.0.0.1:4500"),
		storageNodeIn(simplyblockv1alpha2.StorageNodeStepResolving))
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t), Now: func() time.Time { return now }}

	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	began := tasksBudget(t, c).Annotations[holdSinceAnnotation]
	if began != "2026-11-25T19:50:00Z" {
		t.Fatalf("the hold began at %q, want the time of the first held pass", began)
	}
	held := stampOf(t, c, ComponentTasks)

	// Within the limit the hold keeps the time it began, whatever else changes:
	// the turns are counted from it.
	now = now.Add(tasksHoldLimit - time.Minute)
	moveCoordinators(t, c, "sb:def@10.0.0.2:4500")
	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply within the limit: %v", err)
	}
	budget := tasksBudget(t, c)
	if budget.Spec.MaxUnavailable.IntValue() != 0 || budget.Annotations[holdSinceAnnotation] != began {
		t.Errorf("within the limit the budget allows %d and began %q; want 0 and %q",
			budget.Spec.MaxUnavailable.IntValue(), budget.Annotations[holdSinceAnnotation], began)
	}
	if got := stampOf(t, c, ComponentTasks); got != held {
		t.Errorf("the runner was restamped within the limit: %s -> %s", held, got)
	}

	now = now.Add(2 * time.Minute)
	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply past the limit: %v", err)
	}
	budget = tasksBudget(t, c)
	if got := budget.Spec.MaxUnavailable.IntValue(); got != 0+1 {
		t.Errorf("past the limit the budget allows %d disruptions; the drain has to win", got)
	}
	if budget.Annotations[holdSinceAnnotation] != began {
		t.Error("the budget forgot when the hold began while an add is still in flight, " +
			"so the next pass would hold again from now and the open window would never come")
	}
	if got, want := stampOf(t, c, ComponentTasks), stampOf(t, c, ComponentWebAPI); got != want {
		t.Errorf("past the limit the runner still carries %s and the management API %s", got, want)
	}
}

func TestOnlyANodeEnteringOrLeavingItsAddReconcilesTheControlPlane(t *testing.T) {
	p := nodeAddStepChanged()
	slot := storageNodeIn(simplyblockv1alpha2.StorageNodeStepAwaitingSlot)
	posting := storageNodeIn(simplyblockv1alpha2.StorageNodeStepPosting)
	resolving := storageNodeIn(simplyblockv1alpha2.StorageNodeStepResolving)

	for _, tc := range []struct {
		name     string
		old, new *simplyblockv1alpha2.StorageNode
		want     bool
	}{
		{"takes its slot", slot, posting, true},
		{"moves on inside the add", posting, resolving, false},
		{"a status write inside the add", resolving, resolving, false},
		{"settles", resolving, storageNodeIn(""), true},
		{"a status write outside the add", slot, slot, false},
	} {
		if got := p.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}); got != tc.want {
			t.Errorf("%s: reconciles = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !p.Delete(event.DeleteEvent{Object: resolving}) {
		t.Error("a node deleted in the middle of its add leaves the hold on until the next steady-state pass")
	}
	if p.Create(event.CreateEvent{Object: slot}) {
		t.Error("a node created waiting for its slot reconciles the control plane")
	}
}

// The hold comes back after the open window, for as long as adds are in flight:
// a fleet's adds follow one another, and a hold that was spent after its first
// turn protected only the first twenty minutes of them.
func TestTheHoldRunsInTurns(t *testing.T) {
	for _, tc := range []struct {
		into time.Duration
		held bool
	}{
		{0, true},
		{tasksHoldLimit - time.Second, true},
		{tasksHoldLimit, false},
		{tasksHoldLimit + tasksHoldRelease - time.Second, false},
		{tasksHoldLimit + tasksHoldRelease, true},
		{2*tasksHoldLimit + tasksHoldRelease, false},
		{-time.Minute, true},
	} {
		if got := heldAt(tc.into); got != tc.held {
			t.Errorf("%v into a run of adds: held = %v, want %v", tc.into, got, tc.held)
		}
	}
	// A drain that has been failing retries every five minutes and gives an
	// attempt a minute and a half. The window has to fit both.
	if tasksHoldRelease < 5*time.Minute+90*time.Second {
		t.Errorf("the open window is %v, shorter than one retry of a failing drain", tasksHoldRelease)
	}
}

// Regression: 2026-11-25-node-add-restarted-by-roll — a node that fails keeps
// the step it failed in. Counted as an add in flight, one failed node kept the
// run of adds going for good, and every later add found the hold's turn spent.
func TestAFailedNodeIsNotAnAddInFlight(t *testing.T) {
	for _, step := range []simplyblockv1alpha2.StorageNodeStep{
		simplyblockv1alpha2.StorageNodeStepPosting, simplyblockv1alpha2.StorageNodeStepResolving,
	} {
		node := storageNodeIn(step)
		node.Status.Phase = simplyblockv1alpha2.StorageNodePhaseFailed
		cp := localControlPlane()
		r := &ControlPlaneReconciler{Client: newClient(t, cp, node), Scheme: testScheme(t)}
		hold, err := r.tasksHoldNow(context.Background(), testNamespace)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if hold.held || hold.since != nil {
			t.Errorf("a node that failed in %s holds the task runner", step)
		}
	}
}

// A node has its UUID when the control plane has written its record, which is
// the start of add_node and not the end of it. The runner is still adding it
// until the control plane stops reporting in_creation.
func TestANodeIsBeingAddedUntilTheControlPlaneHasFinishedCreatingIt(t *testing.T) {
	recorded := storageNodeIn("")
	recorded.Status.UUID = "41223fb2-47a1-4b72-9dba-6b85c3c6695a"

	for _, tc := range []struct {
		status string
		want   bool
	}{
		{"in_creation", true},
		{"online", false},
		{"unreachable", false},
		{"", false},
	} {
		node := recorded.DeepCopy()
		node.Status.Status = tc.status
		cp := localControlPlane()
		r := &ControlPlaneReconciler{Client: newClient(t, cp, node), Scheme: testScheme(t)}
		hold, err := r.tasksHoldNow(context.Background(), testNamespace)
		if err != nil {
			t.Fatalf("%q: %v", tc.status, err)
		}
		if hold.held != tc.want {
			t.Errorf("a recorded node the control plane reports as %q: held = %v, want %v",
				tc.status, hold.held, tc.want)
		}
	}
}

// A StorageCluster in any namespace belongs to this installation, and its node
// adds are driven by the one task runner.
func TestANodeAddInAnotherNamespaceHoldsTheTaskRunner(t *testing.T) {
	ctx := context.Background()
	elsewhere := storageNodeIn(simplyblockv1alpha2.StorageNodeStepResolving)
	elsewhere.Namespace = "tenant-b"
	cp := localControlPlane()
	c := newClient(t, cp, clusterFileConfigMap("sb:abc@10.0.0.1:4500"), elsewhere)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := tasksBudget(t, c).Spec.MaxUnavailable.IntValue(); got != 0 {
		t.Errorf("the budget allows %d disruptions while a node in another namespace is being added", got)
	}

	requests := r.storageNodeRequests(ctx, elsewhere)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(cp) {
		t.Errorf("a node in another namespace wakes %v; want the ControlPlane that holds the install", requests)
	}
}

// The release must not wait on the rest of a pass: a budget left at no
// disruption by a pass that failed further on is a drain nothing lets through.
func TestTheBudgetIsWrittenByItself(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 19, 50, 0, 0, time.UTC)
	cp := localControlPlane()
	c := newClient(t, cp, storageNodeIn(simplyblockv1alpha2.StorageNodeStepResolving))
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t), Now: func() time.Time { return now }}

	if err := r.applyTasksBudget(ctx, cp); err != nil {
		t.Fatalf("apply the budget: %v", err)
	}
	budget := tasksBudget(t, c)
	if got := budget.Spec.MaxUnavailable.IntValue(); got != 0 {
		t.Errorf("the budget allows %d disruptions while a node is being added", got)
	}
	if policy := budget.Spec.UnhealthyPodEvictionPolicy; policy == nil || *policy != policyv1.AlwaysAllow {
		t.Errorf("unhealthyPodEvictionPolicy = %v; a runner that is not ready is adding nothing "+
			"and must not be kept on a host being drained", policy)
	}

	now = now.Add(tasksHoldLimit + time.Minute)
	if err := r.applyTasksBudget(ctx, cp); err != nil {
		t.Fatalf("apply the budget in the open window: %v", err)
	}
	if got := tasksBudget(t, c).Spec.MaxUnavailable.IntValue(); got != 1 {
		t.Errorf("in the open window the budget allows %d disruptions, with nothing else applied", got)
	}

	now = now.Add(tasksHoldRelease)
	if err := r.applyTasksBudget(ctx, cp); err != nil {
		t.Fatalf("apply the budget in the next turn: %v", err)
	}
	if got := tasksBudget(t, c).Spec.MaxUnavailable.IntValue(); got != 0 {
		t.Errorf("in the next turn the budget allows %d disruptions; the hold comes back", got)
	}
}
