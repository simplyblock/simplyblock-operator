// The cordon raises a HostMaintenance window, the uncordon calls it off or
// deletes its record, and a Node maps to the control plane whose FoundationDB
// pods it carries. Rows U-52 through U-60 and U-74 in
// test-plan-fdb-host-maintenance.md.

package controlplane

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func cordoned(name, zone string) *corev1.Node {
	node := aNode(name, zone)
	node.Spec.Unschedulable = true
	return node
}

type raiseHarness struct {
	t        *testing.T
	c        client.Client
	r        *ControlPlaneReconciler
	recorder *recordingRecorder
	cp       *simplyblockv1alpha2.ControlPlane
}

func newRaiseHarness(t *testing.T, objects ...client.Object) *raiseHarness {
	t.Helper()
	cp := localControlPlane()
	recorder := &recordingRecorder{}
	c := newClient(t, append(objects, cp)...)
	return &raiseHarness{
		t: t, c: c, recorder: recorder, cp: cp,
		r: &ControlPlaneReconciler{Client: c, Scheme: testScheme(t), Recorder: recorder},
	}
}

func (h *raiseHarness) raise() {
	h.t.Helper()
	if err := h.r.reconcileHostMaintenance(context.Background(), h.cp); err != nil {
		h.t.Fatalf("reconcileHostMaintenance: %v", err)
	}
}

func (h *raiseHarness) window(worker string) (*simplyblockv1alpha2.ControlPlaneOps, bool) {
	h.t.Helper()
	var ops simplyblockv1alpha2.ControlPlaneOps
	err := h.c.Get(context.Background(), client.ObjectKey{
		Namespace: testNamespace, Name: windowName(h.cp, worker),
	}, &ops)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return &ops, true
}

func (h *raiseHarness) windows() int {
	h.t.Helper()
	var list simplyblockv1alpha2.ControlPlaneOpsList
	if err := h.c.List(context.Background(), &list, client.InNamespace(testNamespace)); err != nil {
		h.t.Fatal(err)
	}
	return len(list.Items)
}

// U-52: a cordoned worker with a FoundationDB pod gets a window.
func TestACordonedFDBHostGetsAWindow(t *testing.T) {
	h := newRaiseHarness(t, cordoned(drainedWorker, drainedZone), anFDBPod("log-1", drainedWorker))
	h.raise()

	window, ok := h.window(drainedWorker)
	if !ok {
		t.Fatal("no window was raised")
	}
	if window.Spec.Action != simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance ||
		window.Spec.ControlPlaneRef != SingletonName ||
		window.Spec.HostMaintenance == nil || window.Spec.HostMaintenance.WorkerNode != drainedWorker {
		t.Errorf("window spec = %+v, want HostMaintenance on %s for the singleton", window.Spec, drainedWorker)
	}
	if h.recorder.count(HostMaintenanceRaised) != 1 {
		t.Errorf("HostMaintenanceRaised emitted %d times, want once", h.recorder.count(HostMaintenanceRaised))
	}
}

// U-53, U-54: a cordoned worker without a FoundationDB pod, and a schedulable
// worker with one, get nothing.
func TestOnlyACordonedFDBHostGetsAWindow(t *testing.T) {
	h := newRaiseHarness(t,
		cordoned(drainedWorker, drainedZone), aPlainPod("webapi-x", drainedWorker),
		aNode(otherWorker, otherZone), anFDBPod("log-2", otherWorker),
	)
	h.raise()
	if n := h.windows(); n != 0 {
		t.Errorf("%d windows were raised, want none", n)
	}
	if h.recorder.count(HostMaintenanceRaised) != 0 {
		t.Error("HostMaintenanceRaised emitted with nothing to raise")
	}
}

// U-55: a second pass over the same cordon raises no second window.
func TestASecondPassOverTheSameCordonRaisesNothingNew(t *testing.T) {
	h := newRaiseHarness(t, cordoned(drainedWorker, drainedZone), anFDBPod("log-1", drainedWorker))
	h.raise()
	h.raise()
	if n := h.windows(); n != 1 {
		t.Errorf("%d windows, want one", n)
	}
	if h.recorder.count(HostMaintenanceRaised) != 1 {
		t.Errorf("HostMaintenanceRaised emitted %d times, want once", h.recorder.count(HostMaintenanceRaised))
	}
}

// A pool-wide cordon raises one window per FoundationDB host, at once.
func TestAPoolWideCordonRaisesAWindowPerFDBHost(t *testing.T) {
	h := newRaiseHarness(t,
		cordoned(drainedWorker, drainedZone), anFDBPod("log-1", drainedWorker),
		cordoned(otherWorker, otherZone), anFDBPod("log-2", otherWorker),
		cordoned(thirdWorker, thirdZone), aPlainPod("webapi-x", thirdWorker),
	)
	h.raise()
	if n := h.windows(); n != 2 {
		t.Errorf("%d windows, want one per FoundationDB host", n)
	}
}

// U-56: an uncordon aborts a window still Holding and leaves a later one alone.
func TestAnUncordonAbortsOnlyAWindowStillHolding(t *testing.T) {
	holding := aWindowFor(drainedWorker)
	holding.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	holding.Status.Step.State = string(stepHolding)
	moving := aWindowFor(otherWorker)
	moving.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	moving.Status.Step.State = string(stepEnteringMaintenance)
	h := newRaiseHarness(t,
		aNode(drainedWorker, drainedZone), anFDBPod("log-1", drainedWorker), holding,
		aNode(otherWorker, otherZone), anFDBPod("log-2", otherWorker), moving,
	)
	h.raise()

	w, ok := h.window(drainedWorker)
	if !ok || !w.Spec.Abort {
		t.Error("the window still Holding was not aborted on the uncordon")
	}
	m, ok := h.window(otherWorker)
	if !ok || m.Spec.Abort {
		t.Error("a window past Holding was aborted, and something has to clear what it set")
	}
}

// U-57, U-74: an uncordon, or a Node gone, deletes a finished window.
func TestAnUncordonOrAGoneNodeDeletesAFinishedWindow(t *testing.T) {
	finished := aWindowFor(drainedWorker)
	finished.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded
	gone := aWindowFor(otherWorker)
	gone.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseFailed
	running := aWindowFor(thirdWorker)
	running.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	running.Status.Step.State = string(stepAwaitingHost)
	h := newRaiseHarness(t,
		aNode(drainedWorker, drainedZone), anFDBPod("log-1", drainedWorker), finished,
		anFDBPod("log-2", otherWorker), gone,
		running,
	)
	h.raise()

	if _, still := h.window(drainedWorker); still {
		t.Error("the finished window survived the uncordon")
	}
	if _, still := h.window(otherWorker); still {
		t.Error("the finished window survived its Node being gone")
	}
	if _, still := h.window(thirdWorker); !still {
		t.Error("a running window was deleted because its Node is gone, and it has processes to wait for")
	}
}

// A finished window under a cordon that is still on stays: one window per
// cordon, and a failed one is not retried until the worker is uncordoned.
func TestAFinishedWindowUnderAStandingCordonIsNotReraised(t *testing.T) {
	failed := aWindowFor(drainedWorker)
	failed.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseFailed
	h := newRaiseHarness(t, cordoned(drainedWorker, drainedZone), anFDBPod("log-1", drainedWorker), failed)
	h.raise()
	if n := h.windows(); n != 1 {
		t.Errorf("%d windows, want the failed record left as it is", n)
	}
	if h.recorder.count(HostMaintenanceRaised) != 0 {
		t.Error("a window was raised beside a finished one under the same cordon")
	}
}

// U-58: the window's name is a DNS label for a long worker name.
func TestTheWindowNameIsADNSLabelForALongWorkerName(t *testing.T) {
	cp := localControlPlane()
	worker := "ip-10-0-123-245.eu-central-1.compute.internal.some-very-long-suffix"
	name := windowName(cp, worker)
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		t.Errorf("windowName = %q: %v", name, errs)
	}
	if name != SingletonName+"-maintenance-"+hash8(worker) {
		t.Errorf("windowName = %q, want the control plane, -maintenance-, and the worker's hash", name)
	}
}

// U-59: the Node watch maps to the control plane whose namespace holds a
// FoundationDB pod on the Node, or a window for it, and to nothing otherwise.
func TestANodeMapsToTheControlPlaneWhoseFDBPodsItCarries(t *testing.T) {
	window := aWindowFor(thirdWorker)
	h := newRaiseHarness(t, anFDBPod("log-1", drainedWorker), aPlainPod("webapi-x", otherWorker), window)

	if got := h.r.nodeRequests(context.Background(), aNode(drainedWorker, drainedZone)); len(got) != 1 ||
		got[0].Namespace != testNamespace || got[0].Name != SingletonName {
		t.Errorf("a Node with an FDB pod mapped to %v, want the singleton", got)
	}
	if got := h.r.nodeRequests(context.Background(), aNode(otherWorker, otherZone)); len(got) != 0 {
		t.Errorf("a Node with no FDB pod mapped to %v", got)
	}
	if got := h.r.nodeRequests(context.Background(), aNode(thirdWorker, thirdZone)); len(got) != 1 {
		t.Errorf("a Node with a window and no pod mapped to %v, want the singleton so the window is released", got)
	}
	if got := h.r.nodeRequests(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x"}}); len(got) != 0 {
		t.Errorf("a non-Node mapped to %v", got)
	}
}

// U-60: a window written by hand for a worker without FoundationDB pods is
// left to run: the raise neither deletes nor aborts a window under a cordon.
func TestAHandWrittenWindowUnderACordonIsLeftToRun(t *testing.T) {
	byHand := aWindowFor(thirdWorker)
	byHand.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	byHand.Status.Step.State = string(stepHolding)
	h := newRaiseHarness(t, cordoned(thirdWorker, thirdZone), byHand)
	h.raise()
	w, ok := h.window(thirdWorker)
	if !ok || w.Spec.Abort {
		t.Error("a hand-written window under a cordon was aborted or deleted")
	}
}
