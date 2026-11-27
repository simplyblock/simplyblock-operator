// Moving a FoundationDB process back onto a worker that has none of its class:
// when it is asked for, and every condition that holds it back. Rows U-116
// through U-123 in test-plan-fdb-host-maintenance.md.

package controlplane

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// rebalanceGroup is one entry of the FoundationDBCluster's status.processGroups.
type rebalanceGroup struct {
	id, class, zone string
	removing        bool
	unhealthy       bool
}

// readyWorker is a schedulable Node with a Ready condition, whose hostname label,
// the FoundationDB fault domain, differs from its name as it does on clouds.
func readyWorker(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelHostname: name + ".lab"}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue,
		}}},
	}
}

// fdbClusterFor is a FoundationDBCluster at generation 1, reconciled and fully
// replicated, with the given process groups and pending removals.
func fdbClusterFor(groups []rebalanceGroup, pending ...string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(fdbClusterGVK)
	obj.SetName(ComponentFDBCluster)
	obj.SetNamespace(testNamespace)
	obj.SetGeneration(1)
	entries := make([]any, 0, len(groups))
	for _, g := range groups {
		entry := map[string]any{"processGroupID": g.id, "processClass": g.class, "faultDomain": g.zone}
		if g.removing {
			entry["removalTimestamp"] = "2026-11-27T16:00:00Z"
		}
		if g.unhealthy {
			entry["processGroupConditions"] = []any{map[string]any{"conditionType": "MissingProcesses"}}
		}
		entries = append(entries, entry)
	}
	_ = unstructured.SetNestedSlice(obj.Object, entries, "status", "processGroups")
	_ = unstructured.SetNestedField(obj.Object, int64(1), "status", "generations", "reconciled")
	_ = unstructured.SetNestedField(obj.Object, true, "status", "health", "fullReplication")
	_ = unstructured.SetNestedField(obj.Object, true, "status", "health", "available")
	if len(pending) > 0 {
		ids := make([]any, 0, len(pending))
		for _, id := range pending {
			ids = append(ids, id)
		}
		_ = unstructured.SetNestedSlice(obj.Object, ids, "spec", "processGroupsToRemove")
	}
	return obj
}

// crowded is the AWS cluster of 2026-11-27 in miniature: every process of both
// classes on w1 and w2 after w3 rebooted, and w3 back and empty.
func crowded() []rebalanceGroup {
	return []rebalanceGroup{
		{id: "log-1", class: "log", zone: "w1.lab"},
		{id: "log-2", class: "log", zone: "w1.lab"},
		{id: "log-3", class: "log", zone: "w2.lab"},
		{id: "storage-1", class: "storage", zone: "w1.lab"},
		{id: "storage-2", class: "storage", zone: "w2.lab"},
		{id: "storage-3", class: "storage", zone: "w2.lab"},
		{id: "stateless-1", class: "stateless", zone: "w1.lab"},
		{id: "stateless-2", class: "stateless", zone: "w1.lab"},
	}
}

// spread is the same cluster with every class on all three workers.
func spread() []rebalanceGroup {
	return []rebalanceGroup{
		{id: "log-1", class: "log", zone: "w1.lab"},
		{id: "log-2", class: "log", zone: "w2.lab"},
		{id: "log-3", class: "log", zone: "w3.lab"},
		{id: "storage-1", class: "storage", zone: "w1.lab"},
		{id: "storage-2", class: "storage", zone: "w2.lab"},
		{id: "storage-3", class: "storage", zone: "w3.lab"},
		{id: "stateless-1", class: "stateless", zone: "w1.lab"},
		{id: "stateless-2", class: "stateless", zone: "w1.lab"},
	}
}

type rebalanceHarness struct {
	t        *testing.T
	c        client.Client
	r        *ControlPlaneReconciler
	recorder *recordingRecorder
	cp       *simplyblockv1alpha2.ControlPlane
}

func newRebalanceHarness(
	t *testing.T, cp *simplyblockv1alpha2.ControlPlane, cluster *unstructured.Unstructured, objects ...client.Object,
) *rebalanceHarness {
	t.Helper()
	recorder := &recordingRecorder{}
	c := newClient(t, append(objects, cp, cluster)...)
	return &rebalanceHarness{
		t: t, c: c, recorder: recorder, cp: cp,
		r: &ControlPlaneReconciler{Client: c, Scheme: testScheme(t), Recorder: recorder},
	}
}

func (h *rebalanceHarness) rebalance() {
	h.t.Helper()
	if err := h.r.rebalanceFoundationDB(context.Background(), h.cp); err != nil {
		h.t.Fatalf("rebalanceFoundationDB: %v", err)
	}
}

// toRemove is the FoundationDBCluster's spec.processGroupsToRemove as it is now.
func (h *rebalanceHarness) toRemove() []string {
	h.t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(fdbClusterGVK)
	if err := h.c.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: ComponentFDBCluster}, obj); err != nil {
		h.t.Fatalf("read the FoundationDBCluster: %v", err)
	}
	ids, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "processGroupsToRemove")
	return ids
}

func threeWorkers() []client.Object {
	return []client.Object{readyWorker("w1"), readyWorker("w2"), readyWorker("w3")}
}

// U-116. Regression: 2026-11-27-fdb-coordinators-two-workers. On the AWS
// certification cluster, worker ip-10-0-63-238 rebooted and its FoundationDB
// pods were rescheduled onto the other two workers, as the preferred
// anti-affinity allows. Nothing moved any of them back when the worker
// returned, so all ten pods ran on two of three workers, two coordinators
// shared one worker whose loss would have taken the quorum, and the
// FoundationDB operator logged "Could only select 2 processes, but 3 are
// required" every ten seconds (1,570 ChangingCoordinators events).
func TestACrowdedClassGetsOneProcessMovedToTheEmptyWorker(t *testing.T) {
	h := newRebalanceHarness(t, localControlPlane(), fdbClusterFor(crowded()), threeWorkers()...)
	h.rebalance()

	removed := h.toRemove()
	if len(removed) != 1 {
		t.Fatalf("processGroupsToRemove = %v, want exactly one process group replaced", removed)
	}
	if got := removed[0]; got != "log-2" && got != "log-1" && got != "storage-2" && got != "storage-3" {
		t.Errorf("replaced %s, want a log or storage process from the worker it shares with its own class", got)
	}
	if h.recorder.last(FoundationDBRebalanceRequested) == "" {
		t.Errorf("no %s event; got %v", FoundationDBRebalanceRequested, h.recorder.reasons)
	}
}

// U-117: every class already on its own workers.
func TestABalancedClusterIsLeftAlone(t *testing.T) {
	h := newRebalanceHarness(t, localControlPlane(), fdbClusterFor(spread()), threeWorkers()...)
	h.rebalance()
	if removed := h.toRemove(); len(removed) != 0 {
		t.Errorf("processGroupsToRemove = %v on a balanced cluster", removed)
	}
}

// U-118: the empty worker has to be one the replacement can land on.
func TestOnlyAnEligibleEmptyWorkerDrawsAProcess(t *testing.T) {
	cordonedW3 := readyWorker("w3")
	cordonedW3.Spec.Unschedulable = true
	notReadyW3 := readyWorker("w3")
	notReadyW3.Status.Conditions[0].Status = corev1.ConditionFalse
	taintedW3 := readyWorker("w3")
	taintedW3.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
	unlabeledW3 := readyWorker("w3")

	selecting := localControlPlane()
	selecting.Spec.Source.Local.NodeSelector = map[string]string{"role": "control"}
	tolerating := localControlPlane()
	tolerating.Spec.Source.Local.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}}
	labeled := func(n *corev1.Node) *corev1.Node { n.Labels["role"] = "control"; return n }

	for name, tc := range map[string]struct {
		cp    *simplyblockv1alpha2.ControlPlane
		nodes []client.Object
		moved bool
	}{
		"cordoned":               {localControlPlane(), []client.Object{readyWorker("w1"), readyWorker("w2"), cordonedW3}, false},
		"not ready":              {localControlPlane(), []client.Object{readyWorker("w1"), readyWorker("w2"), notReadyW3}, false},
		"untolerated taint":      {localControlPlane(), []client.Object{readyWorker("w1"), readyWorker("w2"), taintedW3}, false},
		"tolerated taint":        {tolerating, []client.Object{readyWorker("w1"), readyWorker("w2"), taintedW3.DeepCopy()}, true},
		"outside the selector":   {selecting, []client.Object{labeled(readyWorker("w1")), labeled(readyWorker("w2")), unlabeledW3}, false},
		"inside the selector":    {selecting, []client.Object{labeled(readyWorker("w1")), labeled(readyWorker("w2")), labeled(readyWorker("w3"))}, true},
		"no third worker at all": {localControlPlane(), []client.Object{readyWorker("w1"), readyWorker("w2")}, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRebalanceHarness(t, tc.cp, fdbClusterFor(crowded()), tc.nodes...)
			h.rebalance()
			if moved := len(h.toRemove()) > 0; moved != tc.moved {
				t.Errorf("a process moved = %v, want %v (processGroupsToRemove = %v)", moved, tc.moved, h.toRemove())
			}
		})
	}
}

// U-119: one replacement at a time, whichever writer asked for the one in flight.
func TestARemovalInFlightHoldsTheNext(t *testing.T) {
	marked := crowded()
	marked[1].removing = true // the FoundationDB operator is removing log-2
	for name, cluster := range map[string]*unstructured.Unstructured{
		"marked in status":         fdbClusterFor(marked),
		"asked for and not marked": fdbClusterFor(crowded(), "storage-3"),
	} {
		t.Run(name, func(t *testing.T) {
			before, _, _ := unstructured.NestedStringSlice(cluster.Object, "spec", "processGroupsToRemove")
			h := newRebalanceHarness(t, localControlPlane(), cluster, threeWorkers()...)
			h.rebalance()
			if got := h.toRemove(); len(got) != len(before) {
				t.Errorf("processGroupsToRemove = %v, was %v: a second replacement was asked for", got, before)
			}
		})
	}
}

// U-120: a database that is not in a settled, fully replicated state is not
// asked to move anything.
func TestAnUnsettledDatabaseIsNotRebalanced(t *testing.T) {
	notReplicated := fdbClusterFor(crowded())
	_ = unstructured.SetNestedField(notReplicated.Object, false, "status", "health", "fullReplication")
	behind := fdbClusterFor(crowded())
	behind.SetGeneration(2)
	sick := crowded()
	sick[5].unhealthy = true
	for name, cluster := range map[string]*unstructured.Unstructured{
		"not fully replicated":         notReplicated,
		"spec not reconciled yet":      behind,
		"a process group with trouble": fdbClusterFor(sick),
	} {
		t.Run(name, func(t *testing.T) {
			h := newRebalanceHarness(t, localControlPlane(), cluster, threeWorkers()...)
			h.rebalance()
			if got := h.toRemove(); len(got) != 0 {
				t.Errorf("processGroupsToRemove = %v on a database that is %s", got, name)
			}
		})
	}
}

// U-121: an operation on the control plane, a HostMaintenance window above all,
// moves processes itself and is not raced.
func TestAnOperationInFlightHoldsTheRebalance(t *testing.T) {
	locked := localControlPlane()
	locked.Status.ActiveOpsRef = "simplyblock-restart"
	window := func(phase simplyblockv1alpha2.ControlPlaneOpsPhase) *simplyblockv1alpha2.ControlPlaneOps {
		return &simplyblockv1alpha2.ControlPlaneOps{
			ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-maintenance-1", Namespace: testNamespace},
			Spec: simplyblockv1alpha2.ControlPlaneOpsSpec{
				ControlPlaneRef: SingletonName,
				Action:          simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance,
				HostMaintenance: &simplyblockv1alpha2.HostMaintenanceSpec{WorkerNode: "w3"},
			},
			Status: simplyblockv1alpha2.ControlPlaneOpsStatus{Phase: phase},
		}
	}
	for name, tc := range map[string]struct {
		cp    *simplyblockv1alpha2.ControlPlane
		extra []client.Object
		moved bool
	}{
		"the lock is held":         {locked, nil, false},
		"a window is open":         {localControlPlane(), []client.Object{window(simplyblockv1alpha2.ControlPlaneOpsPhaseRunning)}, false},
		"a window not started yet": {localControlPlane(), []client.Object{window("")}, false},
		"only a finished window":   {localControlPlane(), []client.Object{window(simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded)}, true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRebalanceHarness(t, tc.cp, fdbClusterFor(crowded()), append(threeWorkers(), tc.extra...)...)
			h.rebalance()
			if moved := len(h.toRemove()) > 0; moved != tc.moved {
				t.Errorf("a process moved = %v, want %v", moved, tc.moved)
			}
		})
	}
}

// U-122: a second pass before the FoundationDB operator has acted asks for
// nothing more, and the install's own apply of the cluster, which every steady
// state pass makes, keeps the entry it did not write.
func TestTheRequestSurvivesASecondPassAndTheInstallsApply(t *testing.T) {
	h := newRebalanceHarness(t, localControlPlane(), fdbClusterFor(crowded()), threeWorkers()...)
	h.rebalance()
	first := h.toRemove()
	h.rebalance()
	if got := h.toRemove(); len(got) != 1 || len(first) != 1 || got[0] != first[0] {
		t.Fatalf("processGroupsToRemove = %v after a second pass, was %v", got, first)
	}
	if err := applyAll(context.Background(), h.c, h.cp, h.r.Scheme, foundationDBObjects(h.cp)); err != nil {
		t.Fatalf("apply the install's FoundationDB objects: %v", err)
	}
	if got := h.toRemove(); len(got) != 1 || got[0] != first[0] {
		t.Errorf("processGroupsToRemove = %v after the install's apply, want %v kept", got, first)
	}
}

// U-123: an entry for a process group the FoundationDB operator has already
// removed is not carried into the next request, as kubectl-fdb drops it too.
func TestARequestDropsEntriesForRemovedProcessGroups(t *testing.T) {
	h := newRebalanceHarness(t, localControlPlane(), fdbClusterFor(crowded(), "log-9"), threeWorkers()...)
	h.rebalance()
	removed := h.toRemove()
	if len(removed) != 1 || removed[0] == "log-9" {
		t.Errorf("processGroupsToRemove = %v, want one new entry and no log-9", removed)
	}
}
