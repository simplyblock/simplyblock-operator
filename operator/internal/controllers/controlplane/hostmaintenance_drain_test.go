// How a HostMaintenance window sees the drain of its worker, and how the
// Holding deadline follows the drains of the pool rather than the cordon.
//
// Rows U-72, U-76 through U-82, U-94, and U-96 through U-98 in
// test-plan-fdb-host-maintenance.md.

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// holdingWindow is a window two passes in: started, markers up, waiting for a
// drain.
func holdingWindow(t *testing.T, world []client.Object) *windowHarness {
	t.Helper()
	window := aWindowFor(drainedWorker)
	h := newWindowHarness(t, newClient(t, append(world, window)...), window)
	h.pass()
	h.pass()
	if h.step() != string(stepHolding) {
		t.Fatalf("step = %s, want Holding", h.step())
	}
	return h
}

// sentinelRuns gives the sentinel Job the pod the Job controller would create,
// with a finalizer so that an eviction leaves a deletion timestamp to see.
func (h *windowHarness) sentinelRuns() *corev1.Pod {
	h.t.Helper()
	job := h.mustJob(fdbCommandSentinel)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job.Name + "-s3nt1", Namespace: testNamespace,
			Labels:     map[string]string{"batch.kubernetes.io/job-name": job.Name},
			Finalizers: []string{"example.com/still-terminating"},
		},
		Spec: corev1.PodSpec{NodeName: h.window.Spec.HostMaintenance.WorkerNode},
	}
	if err := h.c.Create(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	job.Status.Active = 1
	job.Status.StartTime = &metav1.Time{Time: time.Now()}
	if err := h.c.Status().Update(context.Background(), job); err != nil {
		h.t.Fatal(err)
	}
	return pod
}

// neighborEvicted marks a pod on the worker the way the eviction API does.
func (h *windowHarness) neighborEvicted(name string) {
	h.t.Helper()
	var pod corev1.Pod
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &pod); err != nil {
		h.t.Fatal(err)
	}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "EvictionByEvictionAPI",
	})
	if err := h.c.Status().Update(context.Background(), &pod); err != nil {
		h.t.Fatal(err)
	}
}

func (h *windowHarness) observation() (*metav1.Time, simplyblockv1alpha2.HostMaintenanceDrainSignal) {
	h.t.Helper()
	hm := h.current().Status.HostMaintenance
	if hm == nil {
		return nil, ""
	}
	return hm.DrainObservedAt, hm.DrainObservedBy
}

// U-76: the sentinel's eviction is the drain.
func TestTheSentinelsEvictionIsTheDrain(t *testing.T) {
	h := holdingWindow(t, labWorld())
	pod := h.sentinelRuns()
	h.pass()
	if at, _ := h.observation(); at != nil {
		t.Fatal("a drain was observed while the sentinel is running")
	}

	if err := h.c.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	h.pass()
	at, by := h.observation()
	if at == nil || by != simplyblockv1alpha2.HostMaintenanceDrainSignalSentinel {
		t.Errorf("observation = %v %s, want the sentinel's eviction recorded", at, by)
	}
	if h.recorder.count(DrainObserved) != 1 {
		t.Errorf("DrainObserved emitted %d times, want once", h.recorder.count(DrainObserved))
	}
}

// U-76, the other half: a sentinel whose pod is gone after the Job ran it.
func TestASentinelWhosePodIsGoneIsTheDrainToo(t *testing.T) {
	h := holdingWindow(t, labWorld())
	job := h.mustJob(fdbCommandSentinel)
	job.Status.StartTime = &metav1.Time{Time: time.Now()}
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if err := h.c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	h.pass()
	if at, by := h.observation(); at == nil || by != simplyblockv1alpha2.HostMaintenanceDrainSignalSentinel {
		t.Errorf("observation = %v %s, want the sentinel's end recorded", at, by)
	}
}

// U-79, U-80: a neighbor's eviction on the worker is the drain, and one on
// another worker is not.
func TestANeighborsEvictionIsTheDrain(t *testing.T) {
	h := holdingWindow(t, append(labWorld(), aPlainPod("exporter-y", otherWorker)))
	h.neighborEvicted("exporter-y")
	h.pass()
	if at, _ := h.observation(); at != nil {
		t.Fatal("an eviction on another worker was taken for this worker's drain")
	}

	h.neighborEvicted("webapi-x")
	h.pass()
	at, by := h.observation()
	if at == nil || by != simplyblockv1alpha2.HostMaintenanceDrainSignalPodDisruption {
		t.Errorf("observation = %v %s, want the neighbor's eviction recorded", at, by)
	}
}

// U-80, the reason half: a DisruptionTarget with another reason is not a drain.
func TestADisruptionThatIsNotAnEvictionIsNotTheDrain(t *testing.T) {
	h := holdingWindow(t, labWorld())
	var pod corev1.Pod
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "webapi-x"}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "PreemptionByScheduler",
	}}
	if err := h.c.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	h.pass()
	if at, _ := h.observation(); at != nil {
		t.Error("a preemption was taken for a drain")
	}
}

// U-77: the first observation stays.
func TestTheFirstObservationIsNotOverwritten(t *testing.T) {
	h := holdingWindow(t, labWorld())
	h.neighborEvicted("webapi-x")
	h.pass()
	first, by := h.observation()
	if first == nil {
		t.Fatal("no observation")
	}
	pod := h.sentinelRuns()
	if err := h.c.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	h.pass()
	again, byAgain := h.observation()
	if !again.Equal(first) || byAgain != by {
		t.Errorf("observation changed from %v %s to %v %s", first, by, again, byAgain)
	}
}

// U-78: a window past Holding records nothing new.
func TestASignalAfterHoldingWritesNothing(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepAwaitingHost)
	h.neighborEvicted(ComponentFDBCluster + "-log-1")
	h.pass()
	if at, _ := h.observation(); at != nil {
		t.Error("a window in AwaitingHost recorded a drain")
	}
}

// U-81: Holding completes only with both the budget's report and the drain.
func TestHoldingNeedsBothTheReportAndTheDrain(t *testing.T) {
	h := holdingWindow(t, labWorld())
	h.budgetReports(2)
	h.pass()
	if h.step() != string(stepHolding) {
		t.Fatalf("step = %s, want Holding with the budget reported and no drain", h.step())
	}
	h.neighborEvicted("webapi-x")
	h.pass()
	if h.step() != string(stepEnteringMaintenance) {
		t.Errorf("step = %s, want EnteringMaintenance with both: %s", h.step(), h.current().Status.Message)
	}
}

// U-82: a window nothing reaches releases on its deadline.
func TestAHoldingWindowNothingReachesReleasesOnItsDeadline(t *testing.T) {
	h := holdingWindow(t, labWorld())
	h.atStep(stepHolding, time.Now().Add(-time.Minute))
	ops := h.runUntilDone()
	if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseFailed {
		t.Fatalf("phase = %s, want Failed", ops.Status.Phase)
	}
	if _, up := h.budget(); up {
		t.Error("the budget outlived the expired window")
	}
}

// U-94: a sentinel that cannot be created leaves the budget up, and the
// neighbor's eviction still completes the hold.
func TestASentinelThatCannotBeCreatedStillLeavesTheHoldWorking(t *testing.T) {
	window := aWindowFor(drainedWorker)
	c := windowClient(t, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isJob := obj.(*batchv1.Job); isJob {
				return errors.New("no Jobs today")
			}
			return cl.Create(ctx, obj, opts...)
		},
	}, append(labWorld(), window)...)
	h := newWindowHarness(t, c, window)
	h.pass()
	h.pass()
	if _, up := h.budget(); !up {
		t.Fatal("the budget is not up although only the sentinel failed")
	}
	if h.recorder.count(EvictionHeld) != 1 {
		t.Errorf("EvictionHeld emitted %d times, want once", h.recorder.count(EvictionHeld))
	}
	h.budgetReports(2)
	h.neighborEvicted("webapi-x")
	h.pass()
	if h.step() != string(stepEnteringMaintenance) {
		t.Errorf("step = %s, want EnteringMaintenance on the neighbor's signal: %s", h.step(), h.current().Status.Message)
	}
}

// U-72: a window whose drain arrives during another window's move waits in
// Holding with its budget up, and moves when the lock frees.
func TestAWindowWhoseDrainArrivesDuringAMoveQueuesBehindIt(t *testing.T) {
	other := aWindowFor(otherWorker)
	other.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	other.Status.Step.State = string(stepMovingCoordinators)
	other.Status.HostMaintenance = &simplyblockv1alpha2.HostMaintenanceStatus{Zone: otherZone}
	world := labWorld()
	world[0].(*simplyblockv1alpha2.ControlPlane).Status.ActiveOpsRef = other.Name
	h := holdingWindow(t, append(world, other))
	h.budgetReports(2)
	h.neighborEvicted("webapi-x")

	h.pass()
	if h.step() != string(stepHolding) {
		t.Fatalf("step = %s, want Holding while the other window moves", h.step())
	}
	if h.recorder.count(MoveQueued) == 0 {
		t.Error("no MoveQueued event: nothing says why the window has not moved")
	}
	if _, up := h.budget(); !up {
		t.Error("the budget went down while the window queued")
	}

	var cp simplyblockv1alpha2.ControlPlane
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: SingletonName}, &cp); err != nil {
		t.Fatal(err)
	}
	cp.Status.ActiveOpsRef = ""
	if err := h.c.Status().Update(context.Background(), &cp); err != nil {
		t.Fatal(err)
	}
	h.pass()
	if h.step() != string(stepEnteringMaintenance) {
		t.Errorf("step = %s, want EnteringMaintenance once the lock freed: %s", h.step(), h.current().Status.Message)
	}
	if h.lockHolder() != h.window.Name {
		t.Errorf("activeOpsRef = %q, want this window", h.lockHolder())
	}
}

// U-96, U-97: the Holding deadline runs from the last drain observed in the
// namespace, and from the window's creation while none is.
func TestTheHoldingDeadlineFollowsTheLastDrainInTheNamespace(t *testing.T) {
	other := aWindowFor(otherWorker)
	other.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	other.Status.Step.State = string(stepAwaitingHost)
	recent := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	other.Status.HostMaintenance = &simplyblockv1alpha2.HostMaintenanceStatus{
		Zone: otherZone, DrainObservedAt: &recent,
		DrainObservedBy: simplyblockv1alpha2.HostMaintenanceDrainSignalSentinel,
	}
	h := holdingWindow(t, append(labWorld(), other))

	// Older than the deadline by its own clock, but another window observed a
	// drain ten minutes ago: it keeps holding, and its deadline moves.
	h.atStep(stepHolding, time.Now().Add(-time.Minute))
	h.pass()
	ops := h.current()
	if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseRunning || ops.Status.Step.State != string(stepHolding) {
		t.Fatalf("window is %s at %s, want still Holding: %s", ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	want := recent.Add(holdingDeadline)
	if ops.Status.Step.Deadline == nil || ops.Status.Step.Deadline.Sub(want).Abs() > time.Minute {
		t.Errorf("deadline = %v, want the other window's drain plus the Holding budget, %v", ops.Status.Step.Deadline, want)
	}

	// With the other window's drain older than the budget, nothing restarts the
	// clock and the window fails.
	stale := metav1.NewTime(time.Now().Add(-holdingDeadline - time.Hour))
	var o simplyblockv1alpha2.ControlPlaneOps
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(other), &o); err != nil {
		t.Fatal(err)
	}
	o.Status.HostMaintenance.DrainObservedAt = &stale
	if err := h.c.Status().Update(context.Background(), &o); err != nil {
		t.Fatal(err)
	}
	h.atStep(stepHolding, time.Now().Add(-time.Minute))
	done := h.runUntilDone()
	if done.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseFailed {
		t.Errorf("phase = %s, want Failed once no drain is inside the budget", done.Status.Phase)
	}
}

// U-98: a window that observed a drain enqueues the other windows in Holding.
func TestADrainObservedByOneWindowWakesTheOthers(t *testing.T) {
	observed := aWindowFor(drainedWorker)
	now := metav1.Now()
	observed.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	observed.Status.Step.State = string(stepHolding)
	observed.Status.HostMaintenance = &simplyblockv1alpha2.HostMaintenanceStatus{Zone: drainedZone, DrainObservedAt: &now}
	waiting := aWindowFor(otherWorker)
	waiting.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	waiting.Status.Step.State = string(stepHolding)
	finished := aWindowFor(thirdWorker)
	finished.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded
	r := &ControlPlaneOpsReconciler{Client: newClient(t, localControlPlane(), observed, waiting, finished)}

	requests := r.holdingWindowsBeside(context.Background(), observed)
	if len(requests) != 1 || requests[0].Name != waiting.Name {
		t.Errorf("requests = %v, want only the window still Holding", requests)
	}
	if got := r.holdingWindowsBeside(context.Background(), waiting); len(got) != 0 {
		t.Errorf("a window with no observation woke %v", got)
	}
}

// The pod watch maps a pod on a held worker to that worker's window, and
// nothing else.
func TestAPodOnAHeldWorkerMapsToItsWindow(t *testing.T) {
	holding := aWindowFor(drainedWorker)
	holding.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	holding.Status.Step.State = string(stepHolding)
	moving := aWindowFor(otherWorker)
	moving.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	moving.Status.Step.State = string(stepMovingCoordinators)
	r := &ControlPlaneOpsReconciler{Client: newClient(t, localControlPlane(), holding, moving)}

	if got := r.windowsForPod(context.Background(), aPlainPod("p", drainedWorker)); len(got) != 1 || got[0].Name != holding.Name {
		t.Errorf("a pod on the held worker mapped to %v", got)
	}
	if got := r.windowsForPod(context.Background(), aPlainPod("q", otherWorker)); len(got) != 0 {
		t.Errorf("a pod on a worker whose window is past Holding mapped to %v", got)
	}
	if got := r.windowsForPod(context.Background(), aPlainPod("s", thirdWorker)); len(got) != 0 {
		t.Errorf("a pod on a worker with no window mapped to %v", got)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clusterFileConfigMapName, Namespace: testNamespace}}
	if got := r.windowsForClusterFile(context.Background(), cm); len(got) != 1 || got[0].Name != moving.Name {
		t.Errorf("the cluster file mapped to %v, want the window in MovingCoordinators", got)
	}
}

// A budget type reference keeps the import honest for the file's fixtures.
var _ = policyv1.PodDisruptionBudget{}
