// The HostMaintenance window's steps against a fake client: the hold, the
// release, the teardown on every terminal path, and the lock taken on the way
// out of Holding. The database steps are tested in the same file further down,
// with Jobs whose conditions the test sets by hand.
//
// Rows U-23 through U-33, U-83, and U-91 in test-plan-fdb-host-maintenance.md.

package controlplane

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	drainedWorker = "vm11"
	drainedZone   = "vm11.lab.internal"
	otherWorker   = "vm12"
	otherZone     = "vm12.lab.internal"
	// storageNodeLabel is the storage node's own maintenance label, which this
	// window must never touch.
	storageNodeLabel = "storage.simplyblock.io/maintenance-worker"
)

func aNode(name, zone string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: name, Labels: map[string]string{corev1.LabelHostname: zone},
	}}
}

func anFDBPod(groupID, worker string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: testFDBCluster + "-" + groupID, Namespace: testNamespace,
			Labels: map[string]string{
				utils.LabelFDBClusterName:               testFDBCluster,
				"foundationdb.org/fdb-process-group-id": groupID,
			},
		},
		Spec: corev1.PodSpec{NodeName: worker},
	}
}

func aPlainPod(name, worker string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: map[string]string{appLabel: name}},
		Spec:       corev1.PodSpec{NodeName: worker},
	}
}

// anFDBCluster is the FoundationDBCluster as the fake client stores it, with
// the process groups the window reads.
func anFDBCluster(groups ...fdbProcessGroup) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(fdbClusterGVK)
	obj.SetName(ComponentFDBCluster)
	obj.SetNamespace(testNamespace)
	list := make([]any, 0, len(groups))
	for _, g := range groups {
		entry := map[string]any{
			"processGroupID": g.ID, "processClass": g.Class, "faultDomain": g.FaultDomain,
		}
		if !g.Healthy {
			entry["processGroupConditions"] = []any{map[string]any{"conditionType": "MissingProcesses"}}
		}
		list = append(list, entry)
	}
	_ = unstructured.SetNestedSlice(obj.Object, list, "status", "processGroups")
	return obj
}

func aWindowFor(worker string) *simplyblockv1alpha2.ControlPlaneOps {
	w := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance)
	w.Name = SingletonName + "-maintenance-" + hash8(worker)
	w.UID = types.UID("window-" + worker)
	w.Spec.HostMaintenance = &simplyblockv1alpha2.HostMaintenanceSpec{WorkerNode: worker}
	return w
}

// labWorld is the lab: two FoundationDB hosts and a window for the first.
func labWorld() []client.Object {
	return []client.Object{
		localControlPlane(),
		aNode(drainedWorker, drainedZone), aNode(otherWorker, otherZone),
		anFDBPod("log-1", drainedWorker), anFDBPod("storage-1", drainedWorker),
		anFDBPod("log-2", otherWorker), aPlainPod("webapi-x", drainedWorker),
		anFDBCluster(
			group("log-1", "log", drainedZone), group("storage-1", "storage", drainedZone),
			group("log-2", "log", otherZone),
		),
	}
}

// windowClient is newClient with interceptors, for the tests that make a write
// fail.
func windowClient(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.ControlPlane{}, &simplyblockv1alpha2.ControlPlaneOps{}).
		WithInterceptorFuncs(funcs).
		Build()
}

type windowHarness struct {
	t        *testing.T
	c        client.Client
	r        *ControlPlaneOpsReconciler
	recorder *recordingRecorder
	window   *simplyblockv1alpha2.ControlPlaneOps
}

func newWindowHarness(t *testing.T, c client.Client, window *simplyblockv1alpha2.ControlPlaneOps) *windowHarness {
	t.Helper()
	recorder := &recordingRecorder{}
	return &windowHarness{
		t: t, c: c, recorder: recorder, window: window,
		r: &ControlPlaneOpsReconciler{Client: c, Scheme: testScheme(t), Recorder: recorder},
	}
}

func (h *windowHarness) pass() {
	h.t.Helper()
	if _, err := h.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(h.window),
	}); err != nil {
		h.t.Fatalf("Reconcile: %v", err)
	}
}

func (h *windowHarness) current() *simplyblockv1alpha2.ControlPlaneOps {
	h.t.Helper()
	var ops simplyblockv1alpha2.ControlPlaneOps
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.window), &ops); err != nil {
		h.t.Fatalf("read the window: %v", err)
	}
	return &ops
}

// runUntil reconciles until the window reaches the step, and fails the test
// when it ends first or does not get there in a bounded number of passes.
func (h *windowHarness) runUntil(step opsStep) {
	h.t.Helper()
	for i := 0; i < 12; i++ {
		ops := h.current()
		if ops.Status.Step.State == string(step) {
			return
		}
		if terminalOps(ops.Status.Phase) {
			h.t.Fatalf("the window ended %s at %s before reaching %s: %s",
				ops.Status.Phase, ops.Status.Step.State, step, ops.Status.Message)
		}
		h.pass()
	}
	ops := h.current()
	h.t.Fatalf("the window is at %s after 12 passes, want %s: %s", ops.Status.Step.State, step, ops.Status.Message)
}

// runUntilDone reconciles until the window is terminal.
func (h *windowHarness) runUntilDone() *simplyblockv1alpha2.ControlPlaneOps {
	h.t.Helper()
	for i := 0; i < 12; i++ {
		ops := h.current()
		if terminalOps(ops.Status.Phase) {
			return ops
		}
		h.pass()
	}
	ops := h.current()
	h.t.Fatalf("the window is still %s at %s after 12 passes: %s", ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	return nil
}

func (h *windowHarness) budget() (*policyv1.PodDisruptionBudget, bool) {
	h.t.Helper()
	var pdb policyv1.PodDisruptionBudget
	err := h.c.Get(context.Background(), client.ObjectKey{
		Namespace: testNamespace, Name: windowBudgetName(h.window.Spec.HostMaintenance.WorkerNode),
	}, &pdb)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		h.t.Fatalf("read the budget: %v", err)
	}
	return &pdb, true
}

func (h *windowHarness) podLabel(pod, key string) (string, bool) {
	h.t.Helper()
	var p corev1.Pod
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: pod}, &p); err != nil {
		h.t.Fatalf("read pod %s: %v", pod, err)
	}
	v, ok := p.Labels[key]
	return v, ok
}

// observeDrain writes what W-07's signals write, so the hold can complete.
func (h *windowHarness) observeDrain() {
	h.t.Helper()
	ops := h.current()
	base := ops.DeepCopy()
	if ops.Status.HostMaintenance == nil {
		ops.Status.HostMaintenance = &simplyblockv1alpha2.HostMaintenanceStatus{}
	}
	now := metav1.Now()
	ops.Status.HostMaintenance.DrainObservedAt = &now
	ops.Status.HostMaintenance.DrainObservedBy = simplyblockv1alpha2.HostMaintenanceDrainSignalSentinel
	if err := h.c.Status().Patch(context.Background(), ops, client.MergeFrom(base)); err != nil {
		h.t.Fatal(err)
	}
}

// budgetReports writes what the disruption controller would: its condition,
// the pods it expects, and that none may be disrupted.
func (h *windowHarness) budgetReports(expected int32) {
	h.t.Helper()
	pdb, ok := h.budget()
	if !ok {
		h.t.Fatal("no budget to report on")
	}
	pdb.Status.ExpectedPods = expected
	pdb.Status.CurrentHealthy = expected
	pdb.Status.DisruptionsAllowed = 0
	pdb.Status.ObservedGeneration = pdb.Generation
	pdb.Status.Conditions = []metav1.Condition{{
		Type: "DisruptionAllowed", Status: metav1.ConditionFalse, Reason: "InsufficientPods",
		LastTransitionTime: metav1.Now(),
	}}
	if err := h.c.Status().Update(context.Background(), pdb); err != nil {
		h.t.Fatal(err)
	}
}

// atStep places a window at a step with a live deadline, as a restart would
// find it, holding the markers the earlier steps would have put up.
func (h *windowHarness) atStep(step opsStep, deadline time.Time) {
	h.t.Helper()
	ops := h.current()
	base := ops.DeepCopy()
	ops.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	ops.Status.Step = statemachine.KubeSnapshot{State: string(step), Deadline: &metav1.Time{Time: deadline}}
	if ops.Status.HostMaintenance == nil {
		ops.Status.HostMaintenance = &simplyblockv1alpha2.HostMaintenanceStatus{
			Zone: drainedZone, ProcessGroups: []string{"log-1", "storage-1"},
		}
	}
	if err := h.c.Status().Patch(context.Background(), ops, client.MergeFrom(base)); err != nil {
		h.t.Fatal(err)
	}
}

// markersUp puts the budget and the labels up by hand, as Holding would have.
func (h *windowHarness) markersUp(finalizers ...string) {
	h.t.Helper()
	ctx := context.Background()
	worker := h.window.Spec.HostMaintenance.WorkerNode
	pdb := windowBudget(h.window)
	pdb.Finalizers = finalizers
	if err := h.c.Create(ctx, pdb); err != nil {
		h.t.Fatal(err)
	}
	for _, name := range []string{testFDBCluster + "-log-1", testFDBCluster + "-storage-1"} {
		var p corev1.Pod
		if err := h.c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: name}, &p); err != nil {
			h.t.Fatal(err)
		}
		p.Labels[fdbMaintenanceLabel] = worker
		if err := h.c.Update(ctx, &p); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.c.Create(ctx, sentinelJob(h.window, localControlPlane())); err != nil {
		h.t.Fatal(err)
	}
}

// U-23, U-24, U-91: what Holding puts up, and on which pods.
func TestHoldingLabelsTheWorkersFDBPodsAndRaisesTheBudget(t *testing.T) {
	window := aWindowFor(drainedWorker)
	h := newWindowHarness(t, newClient(t, append(labWorld(), window)...), window)

	h.pass() // start
	h.pass() // Holding

	ops := h.current()
	if ops.Status.HostMaintenance == nil || ops.Status.HostMaintenance.Zone != drainedZone {
		t.Fatalf("status.hostMaintenance = %+v, want the zone %s recorded", ops.Status.HostMaintenance, drainedZone)
	}
	if got := ops.Status.HostMaintenance.ProcessGroups; !slices.Equal(got, []string{"log-1", "storage-1"}) {
		t.Errorf("processGroups = %v, want the worker's two", got)
	}
	for _, pod := range []string{testFDBCluster + "-log-1", testFDBCluster + "-storage-1"} {
		if v, ok := h.podLabel(pod, fdbMaintenanceLabel); !ok || v != drainedWorker {
			t.Errorf("%s label = %q %v, want %s", pod, v, ok, drainedWorker)
		}
	}
	for _, pod := range []string{testFDBCluster + "-log-2", "webapi-x"} {
		if _, ok := h.podLabel(pod, fdbMaintenanceLabel); ok {
			t.Errorf("%s was labeled, and it is not one of the worker's FoundationDB pods", pod)
		}
	}
	pdb, ok := h.budget()
	if !ok {
		t.Fatal("no budget was created")
	}
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 0 {
		t.Errorf("maxUnavailable = %v, want 0", pdb.Spec.MaxUnavailable)
	}
	if pdb.Spec.Selector.MatchLabels[fdbMaintenanceLabel] != drainedWorker {
		t.Errorf("selector = %v, want the window's label", pdb.Spec.Selector.MatchLabels)
	}
	var sentinel batchv1.Job
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: fdbJobName(window.Name, fdbCommandSentinel)}, &sentinel); err != nil {
		t.Errorf("no sentinel Job: %v", err)
	}
	if h.recorder.count(EvictionHeld) != 1 {
		t.Errorf("EvictionHeld emitted %d times, want once", h.recorder.count(EvictionHeld))
	}
	if ops.Status.Step.State != string(stepHolding) {
		t.Errorf("step = %s, want still Holding with no drain observed", ops.Status.Step.State)
	}
}

// U-91, the ordering half: the zone and the process groups are written before
// the budget, so a budget that cannot be created leaves them recorded.
func TestHoldingRecordsTheZoneBeforeTheBudget(t *testing.T) {
	window := aWindowFor(drainedWorker)
	c := windowClient(t, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isBudget := obj.(*policyv1.PodDisruptionBudget); isBudget {
				return errors.New("the budget cannot be created today")
			}
			return cl.Create(ctx, obj, opts...)
		},
	}, append(labWorld(), window)...)
	h := newWindowHarness(t, c, window)
	h.pass()
	if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(window)}); err == nil {
		t.Fatal("Holding did not report the failed create")
	}
	ops := h.current()
	if ops.Status.HostMaintenance == nil || ops.Status.HostMaintenance.Zone != drainedZone {
		t.Errorf("the zone was not recorded before the budget was attempted: %+v", ops.Status.HostMaintenance)
	}
}

// U-25, U-83: Holding completes only once the budget reports and a drain is
// observed, and the lock is taken on the way out.
func TestHoldingCompletesOnTheBudgetReportAndTheDrainAndTakesTheLock(t *testing.T) {
	window := aWindowFor(drainedWorker)
	h := newWindowHarness(t, newClient(t, append(labWorld(), window)...), window)
	h.pass()
	h.pass()

	h.observeDrain()
	h.pass()
	if ops := h.current(); ops.Status.Step.State != string(stepHolding) {
		t.Fatalf("step = %s, want Holding until the budget reports its pods", ops.Status.Step.State)
	}

	h.budgetReports(2)
	h.pass()
	ops := h.current()
	if ops.Status.Step.State != string(stepEnteringMaintenance) {
		t.Fatalf("step = %s, want EnteringMaintenance: %s", ops.Status.Step.State, ops.Status.Message)
	}
	var cp simplyblockv1alpha2.ControlPlane
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: SingletonName}, &cp); err != nil {
		t.Fatal(err)
	}
	if cp.Status.ActiveOpsRef != window.Name {
		t.Errorf("activeOpsRef = %q, want the window holding the lock from EnteringMaintenance on", cp.Status.ActiveOpsRef)
	}
}

// U-26: a worker with no FoundationDB pod gets a budget over nothing, and the
// hold completes on the drain alone.
func TestHoldingOnAWorkerWithoutFDBPodsCompletesOnTheDrain(t *testing.T) {
	window := aWindowFor("vm13")
	world := append(labWorld(), aNode("vm13", "vm13.lab.internal"), window)
	h := newWindowHarness(t, newClient(t, world...), window)
	h.pass()
	h.pass()
	if _, ok := h.budget(); !ok {
		t.Fatal("no budget over a worker without pods")
	}
	h.observeDrain()
	h.budgetReports(0)
	h.pass()
	if ops := h.current(); ops.Status.Step.State != string(stepEnteringMaintenance) {
		t.Errorf("step = %s, want EnteringMaintenance: %s", ops.Status.Step.State, ops.Status.Message)
	}
}

// U-27: Releasing takes the window's markers down and leaves the storage node's.
func TestReleasingTakesOnlyTheWindowsMarkersDown(t *testing.T) {
	ctx := context.Background()
	window := aWindowFor(drainedWorker)
	storageBudget := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "sb-maintenance-abcd1234", Namespace: testNamespace}}
	h := newWindowHarness(t, newClient(t, append(labWorld(), window, storageBudget)...), window)
	h.pass()
	h.markersUp()
	var p corev1.Pod
	if err := h.c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testFDBCluster + "-log-1"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Labels[storageNodeLabel] = drainedWorker
	if err := h.c.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	h.atStep(stepReleasing, time.Now().Add(time.Hour))

	h.runUntil(stepAwaitingHost)

	if _, ok := h.budget(); ok {
		t.Error("the window's budget is still there")
	}
	if _, ok := h.podLabel(testFDBCluster+"-log-1", fdbMaintenanceLabel); ok {
		t.Error("the window's label is still on the pod")
	}
	if v, ok := h.podLabel(testFDBCluster+"-log-1", storageNodeLabel); !ok || v != drainedWorker {
		t.Error("the storage node's label was stripped")
	}
	var sb policyv1.PodDisruptionBudget
	if err := h.c.Get(ctx, client.ObjectKeyFromObject(storageBudget), &sb); err != nil {
		t.Errorf("the storage node's budget is gone: %v", err)
	}
	var sentinel batchv1.Job
	err := h.c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: fdbJobName(window.Name, fdbCommandSentinel)}, &sentinel)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the sentinel Job was left behind: %v", err)
	}
	if h.recorder.count(EvictionReleased) != 1 {
		t.Errorf("EvictionReleased emitted %d times, want once", h.recorder.count(EvictionReleased))
	}
	var cp simplyblockv1alpha2.ControlPlane
	if err := h.c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: SingletonName}, &cp); err != nil {
		t.Fatal(err)
	}
	if cp.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock given back after Releasing", cp.Status.ActiveOpsRef)
	}
}

// U-28: Releasing completes only once the budget is actually gone.
func TestReleasingWaitsForTheBudgetToGo(t *testing.T) {
	window := aWindowFor(drainedWorker)
	h := newWindowHarness(t, newClient(t, append(labWorld(), window)...), window)
	h.pass()
	h.markersUp("example.com/holds-the-budget")
	h.atStep(stepReleasing, time.Now().Add(time.Hour))

	h.pass()
	if ops := h.current(); ops.Status.Step.State != string(stepReleasing) {
		t.Fatalf("step = %s, want Releasing while the budget is still terminating", ops.Status.Step.State)
	}
	pdb, ok := h.budget()
	if !ok || pdb.DeletionTimestamp == nil {
		t.Fatal("the budget was not asked to go")
	}
	pdb.Finalizers = nil
	if err := h.c.Update(context.Background(), pdb); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	h.pass()
	if ops := h.current(); ops.Status.Step.State != string(stepAwaitingHost) {
		t.Errorf("step = %s, want AwaitingHost once the budget is gone: %s", ops.Status.Step.State, ops.Status.Message)
	}
}

// U-29: Cleanup removes a label the release left and the window succeeds.
func TestCleanupRemovesALeftoverLabelAndSucceeds(t *testing.T) {
	ctx := context.Background()
	window := aWindowFor(drainedWorker)
	h := newWindowHarness(t, newClient(t, append(labWorld(), window)...), window)
	h.pass()
	var p corev1.Pod
	if err := h.c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testFDBCluster + "-storage-1"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Labels[fdbMaintenanceLabel] = drainedWorker
	if err := h.c.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	h.atStep(stepCleanup, time.Now().Add(time.Hour))

	ops := h.runUntilDone()
	if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded {
		t.Errorf("phase = %s, want Succeeded: %s", ops.Status.Phase, ops.Status.Message)
	}
	if ops.Status.Step.State != string(simplyblockv1alpha2.ControlPlaneOpsStepCompleted) {
		t.Errorf("step = %s, want Completed", ops.Status.Step.State)
	}
	if _, ok := h.podLabel(testFDBCluster+"-storage-1", fdbMaintenanceLabel); ok {
		t.Error("the leftover label is still on the pod")
	}
}

// U-30: a window that fails runs the teardown.
func TestAWindowThatFailsTakesItsMarkersDown(t *testing.T) {
	window := aWindowFor(drainedWorker)
	cp := localControlPlane()
	cp.Status.ActiveOpsRef = window.Name
	world := append(labWorld()[1:], cp, window)
	h := newWindowHarness(t, newClient(t, world...), window)
	h.pass()
	h.markersUp()
	h.atStep(stepMovingCoordinators, time.Now().Add(-time.Minute))

	ops := h.runUntilDone()
	if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseFailed {
		t.Fatalf("phase = %s, want Failed on the expired deadline", ops.Status.Phase)
	}
	if _, ok := h.budget(); ok {
		t.Error("the budget outlived the failed window")
	}
	if _, ok := h.podLabel(testFDBCluster+"-log-1", fdbMaintenanceLabel); ok {
		t.Error("the label outlived the failed window")
	}
	var target simplyblockv1alpha2.ControlPlane
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(cp), &target); err != nil {
		t.Fatal(err)
	}
	if target.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock released by the failure", target.Status.ActiveOpsRef)
	}
}

// U-31: a window aborted while Holding runs the teardown.
func TestAWindowAbortedWhileHoldingTakesItsMarkersDown(t *testing.T) {
	window := aWindowFor(drainedWorker)
	h := newWindowHarness(t, newClient(t, append(labWorld(), window)...), window)
	h.pass()
	h.pass()
	if _, ok := h.budget(); !ok {
		t.Fatal("no budget to take down")
	}
	ops := h.current()
	ops.Spec.Abort = true
	if err := h.c.Update(context.Background(), ops); err != nil {
		t.Fatal(err)
	}

	done := h.runUntilDone()
	if done.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseAborted {
		t.Fatalf("phase = %s, want Aborted", done.Status.Phase)
	}
	if _, ok := h.budget(); ok {
		t.Error("the budget outlived the aborted window")
	}
	if _, ok := h.podLabel(testFDBCluster+"-log-1", fdbMaintenanceLabel); ok {
		t.Error("the label outlived the aborted window")
	}
}

// U-32: a teardown that cannot remove the budget says so and still ends.
func TestATeardownThatCannotRemoveTheBudgetSaysSo(t *testing.T) {
	window := aWindowFor(drainedWorker)
	c := windowClient(t, interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, isBudget := obj.(*policyv1.PodDisruptionBudget); isBudget {
				return errors.New("the budget will not go")
			}
			return cl.Delete(ctx, obj, opts...)
		},
	}, append(labWorld(), window)...)
	h := newWindowHarness(t, c, window)
	h.pass()
	h.markersUp()
	h.atStep(stepMovingCoordinators, time.Now().Add(-time.Minute))

	ops := h.runUntilDone()
	if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseFailed {
		t.Fatalf("phase = %s, want Failed", ops.Status.Phase)
	}
	if h.recorder.count(MaintenanceMarkersLeft) == 0 {
		t.Error("no MaintenanceMarkersLeft event, and the worker is undrainable with nothing saying why")
	}
}

// U-33: deleting a running window runs the teardown and releases the lock.
func TestDeletingARunningWindowTearsDownAndReleases(t *testing.T) {
	ctx := context.Background()
	window := aWindowFor(drainedWorker)
	cp := localControlPlane()
	cp.Status.ActiveOpsRef = window.Name
	world := append(labWorld()[1:], cp, window)
	h := newWindowHarness(t, newClient(t, world...), window)
	h.pass()
	h.markersUp()
	h.atStep(stepMovingCoordinators, time.Now().Add(time.Hour))
	if err := h.c.Delete(ctx, h.current()); err != nil {
		t.Fatal(err)
	}

	h.pass()

	if _, ok := h.budget(); ok {
		t.Error("the budget outlived the deleted window")
	}
	if _, ok := h.podLabel(testFDBCluster+"-log-1", fdbMaintenanceLabel); ok {
		t.Error("the label outlived the deleted window")
	}
	var target simplyblockv1alpha2.ControlPlane
	if err := h.c.Get(ctx, client.ObjectKeyFromObject(cp), &target); err != nil {
		t.Fatal(err)
	}
	if target.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock released with the window", target.Status.ActiveOpsRef)
	}
	var gone simplyblockv1alpha2.ControlPlaneOps
	if err := h.c.Get(ctx, client.ObjectKeyFromObject(window), &gone); !apierrors.IsNotFound(err) {
		t.Errorf("the window is still there: %v", err)
	}
}

// U-39: a Node without the hostname label cannot name a zone.
func TestHoldingFailsOnANodeWithoutAHostnameLabel(t *testing.T) {
	window := aWindowFor("vm14")
	bare := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "vm14"}}
	h := newWindowHarness(t, newClient(t, append(labWorld(), bare, window)...), window)

	ops := h.runUntilDone()
	if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseFailed {
		t.Fatalf("phase = %s, want Failed", ops.Status.Phase)
	}
	if !contains(ops.Status.Message, corev1.LabelHostname) {
		t.Errorf("message = %q, want it to name the missing label", ops.Status.Message)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// The disruption controller cannot count the pods of a budget whose owner has
// no scale subresource, which the FoundationDBCluster has not: it writes a
// SyncFailed condition and leaves expectedPods at zero, and the eviction API
// still refuses evictions under it. Holding has to take that as the budget
// reporting, or a window on a real cluster never leaves it.
//
// Regression: 2026-11-11-holding-waits-for-a-pod-count-the-budget-never-gets.
func TestHoldingAcceptsABudgetTheControllerCannotCount(t *testing.T) {
	window := aWindowFor(drainedWorker)
	h := newWindowHarness(t, newClient(t, append(labWorld(), window)...), window)
	h.pass()
	h.pass()
	h.observeDrain()

	pdb, ok := h.budget()
	if !ok {
		t.Fatal("no budget")
	}
	pdb.Status.DisruptionsAllowed = 0
	pdb.Status.ExpectedPods = 0
	pdb.Status.Conditions = []metav1.Condition{{
		Type: "DisruptionAllowed", Status: metav1.ConditionFalse, Reason: "SyncFailed",
		Message:            "foundationdbclusters.apps.foundationdb.org does not implement the scale subresource",
		LastTransitionTime: metav1.Now(),
	}}
	if err := h.c.Status().Update(context.Background(), pdb); err != nil {
		t.Fatal(err)
	}

	h.pass()
	if ops := h.current(); ops.Status.Step.State != string(stepEnteringMaintenance) {
		t.Errorf("step = %s, want EnteringMaintenance: the budget refuses evictions although it cannot count the pods: %s",
			ops.Status.Step.State, ops.Status.Message)
	}
}
