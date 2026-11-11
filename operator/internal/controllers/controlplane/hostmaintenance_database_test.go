// The HostMaintenance window's database steps against a fake client, with the
// command Jobs' conditions and termination messages set by the test, and the
// cluster-file ConfigMap rewritten by hand as the FoundationDB operator would.
//
// Rows U-34 through U-38, U-40 through U-46, U-48, U-50, U-51, U-68 through
// U-71, U-84, U-86 through U-88, U-92, and U-95 in
// test-plan-fdb-host-maintenance.md.

package controlplane

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	thirdWorker  = "vm13"
	thirdZone    = "vm13.lab.internal"
	fourthWorker = "vm14"
	fourthZone   = "vm14.lab.internal"
)

// labAddr is a process group's cluster-file entry in the lab's namespace and
// DNS domain, which is what the window writes.
func labAddr(id string) string {
	return ComponentFDBCluster + "-" + id + "." + ComponentFDBCluster + "." + testNamespace + ".svc." + utils.ClusterDNSDomain() + ":4501"
}

func labClusterFile(ids ...string) string {
	addrs := make([]string, 0, len(ids))
	for _, id := range ids {
		addrs = append(addrs, labAddr(id))
	}
	return "simplyblock_fdb_cluster:dKilY3sx@" + strings.Join(addrs, ",")
}

func aClusterFile(ids ...string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: clusterFileConfigMapName, Namespace: testNamespace},
		Data:       map[string]string{clusterFileKey: labClusterFile(ids...)},
	}
}

// fourHostWorld is the lab: a log and a storage process on vm11 and vm12, a log
// on vm13, a storage process on vm14, the three logs as coordinators, and a
// window for vm11.
func fourHostWorld() []client.Object {
	return []client.Object{
		localControlPlane(),
		aNode(drainedWorker, drainedZone), aNode(otherWorker, otherZone),
		aNode(thirdWorker, thirdZone), aNode(fourthWorker, fourthZone),
		anFDBPod("log-1", drainedWorker), anFDBPod("storage-1", drainedWorker),
		anFDBPod("log-2", otherWorker), anFDBPod("log-3", thirdWorker), anFDBPod("storage-4", fourthWorker),
		anFDBCluster(
			group("log-1", "log", drainedZone), group("storage-1", "storage", drainedZone),
			group("log-2", "log", otherZone), group("log-3", "log", thirdZone),
			group("storage-4", "storage", fourthZone),
		),
		aClusterFile("log-1", "log-2", "log-3"),
	}
}

func (h *windowHarness) job(cmd fdbCommand) (*batchv1.Job, bool) {
	h.t.Helper()
	var job batchv1.Job
	err := h.c.Get(context.Background(), client.ObjectKey{
		Namespace: testNamespace, Name: fdbJobName(h.window.Name, cmd),
	}, &job)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return &job, true
}

func (h *windowHarness) mustJob(cmd fdbCommand) *batchv1.Job {
	h.t.Helper()
	job, ok := h.job(cmd)
	if !ok {
		h.t.Fatalf("no %s Job", cmd)
	}
	return job
}

// jobEnds gives a Job the condition the Job controller would.
func (h *windowHarness) jobEnds(cmd fdbCommand, succeeded bool) {
	h.t.Helper()
	job := h.mustJob(cmd)
	condition := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
	if !succeeded {
		condition.Type = batchv1.JobFailed
	}
	job.Status.Conditions = append(job.Status.Conditions, condition)
	if err := h.c.Status().Update(context.Background(), job); err != nil {
		h.t.Fatal(err)
	}
}

// statusSays gives the status Job's pod the termination message the fdbcli
// script writes.
func (h *windowHarness) statusSays(message string) {
	h.t.Helper()
	job := h.mustJob(fdbCommandMaintenanceStatus)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job.Name + "-x1y2z", Namespace: testNamespace,
			Labels: map[string]string{"batch.kubernetes.io/job-name": job.Name},
		},
		Spec: corev1.PodSpec{NodeName: otherWorker},
	}
	if err := h.c.Create(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
	}}
	if err := h.c.Status().Update(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
}

func (h *windowHarness) rewriteClusterFile(ids ...string) {
	h.t.Helper()
	cm := aClusterFile(ids...)
	var current corev1.ConfigMap
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(cm), &current); err != nil {
		h.t.Fatal(err)
	}
	current.Data = cm.Data
	if err := h.c.Update(context.Background(), &current); err != nil {
		h.t.Fatal(err)
	}
}

func (h *windowHarness) setMaintenanceUntil(until time.Time) {
	h.t.Helper()
	ops := h.current()
	base := ops.DeepCopy()
	ops.Status.HostMaintenance.MaintenanceUntil = &metav1.Time{Time: until}
	if err := h.c.Status().Patch(context.Background(), ops, client.MergeFrom(base)); err != nil {
		h.t.Fatal(err)
	}
}

// fdbReports rewrites the FoundationDBCluster's process groups as the
// FoundationDB operator would.
func (h *windowHarness) fdbReports(groups ...fdbProcessGroup) {
	h.t.Helper()
	desired := anFDBCluster(groups...)
	current := anFDBCluster()
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(desired), current); err != nil {
		h.t.Fatal(err)
	}
	list, _, _ := unstructured.NestedSlice(desired.Object, "status", "processGroups")
	if err := unstructured.SetNestedSlice(current.Object, list, "status", "processGroups"); err != nil {
		h.t.Fatal(err)
	}
	if err := h.c.Update(context.Background(), current); err != nil {
		h.t.Fatal(err)
	}
}

func (h *windowHarness) step() string {
	h.t.Helper()
	return h.current().Status.Step.State
}

func (h *windowHarness) lockHolder() string {
	h.t.Helper()
	var cp simplyblockv1alpha2.ControlPlane
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: SingletonName}, &cp); err != nil {
		h.t.Fatal(err)
	}
	return cp.Status.ActiveOpsRef
}

// windowAt is a window placed at a step in a world, past the lock as the
// controller would have taken it.
func windowAt(t *testing.T, world []client.Object, step opsStep) *windowHarness {
	t.Helper()
	window := aWindowFor(drainedWorker)
	cp := world[0].(*simplyblockv1alpha2.ControlPlane)
	if holdsLock(window.Spec.Action, step) {
		cp.Status.ActiveOpsRef = window.Name
	}
	h := newWindowHarness(t, newClient(t, append(world, window)...), window)
	h.pass()
	h.atStep(step, time.Now().Add(time.Hour))
	return h
}

// U-34, U-35: maintenance mode is set for the zone, for the AwaitingHost budget.
func TestEnteringMaintenanceSetsTheZoneForTheHostBudget(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepEnteringMaintenance)

	h.pass()
	job := h.mustJob(fdbCommandMaintenanceOn)
	want := "maintenance on " + drainedZone + " 14400"
	if got := job.Spec.Template.Spec.Containers[0].Args[0]; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
	if h.step() != string(stepEnteringMaintenance) {
		t.Fatalf("step = %s, want EnteringMaintenance while the Job runs", h.step())
	}

	h.jobEnds(fdbCommandMaintenanceOn, true)
	h.pass()
	ops := h.current()
	if ops.Status.Step.State != string(stepMovingCoordinators) {
		t.Fatalf("step = %s, want MovingCoordinators: %s", ops.Status.Step.State, ops.Status.Message)
	}
	until := ops.Status.HostMaintenance.MaintenanceUntil
	if until == nil || time.Until(until.Time) < 3*time.Hour {
		t.Errorf("maintenanceUntil = %v, want about four hours out", until)
	}
	if h.recorder.count(MaintenanceModeSet) != 1 {
		t.Errorf("MaintenanceModeSet emitted %d times, want once", h.recorder.count(MaintenanceModeSet))
	}
}

// U-36: a failed command completes the step degraded.
func TestAFailedMaintenanceOnCompletesTheStepWithoutIt(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepEnteringMaintenance)
	h.pass()
	h.jobEnds(fdbCommandMaintenanceOn, false)
	h.pass()
	ops := h.current()
	if ops.Status.Step.State != string(stepMovingCoordinators) {
		t.Fatalf("step = %s, want MovingCoordinators after the failed command", ops.Status.Step.State)
	}
	if ops.Status.HostMaintenance.MaintenanceUntil != nil {
		t.Error("maintenanceUntil is set although the command failed")
	}
	if h.recorder.count(MaintenanceModeNotSet) != 1 {
		t.Errorf("MaintenanceModeNotSet emitted %d times, want once", h.recorder.count(MaintenanceModeNotSet))
	}
}

// U-86: a later window's zone ends an earlier window's, and says so on it.
func TestEnteringMaintenanceTakesTheZoneOverFromAnEarlierWindow(t *testing.T) {
	earlier := aWindowFor(otherWorker)
	earlier.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	earlier.Status.Step.State = string(stepAwaitingHost)
	earlier.Status.HostMaintenance = &simplyblockv1alpha2.HostMaintenanceStatus{
		Zone: otherZone, MaintenanceUntil: &metav1.Time{Time: time.Now().Add(time.Hour)},
	}
	h := windowAt(t, append(fourHostWorld(), earlier), stepEnteringMaintenance)
	h.pass()
	h.jobEnds(fdbCommandMaintenanceOn, true)
	h.pass()
	if h.recorder.count(MaintenanceZoneTakenOver) != 1 {
		t.Errorf("MaintenanceZoneTakenOver emitted %d times, want once on the earlier window", h.recorder.count(MaintenanceZoneTakenOver))
	}
}

// U-40, U-41, U-45: the set is written before the Job, the step holds until
// the file agrees, and then reports the move.
func TestMovingCoordinatorsWritesTheSetThenWaitsForTheFile(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepMovingCoordinators)

	h.pass()
	ops := h.current()
	want := []string{labAddr("log-2"), labAddr("log-3"), labAddr("storage-4")}
	if !slices.Equal(ops.Status.HostMaintenance.Coordinators, want) {
		t.Fatalf("status.coordinators = %v, want %v", ops.Status.HostMaintenance.Coordinators, want)
	}
	if ops.Status.HostMaintenance.CoordinatorPlacement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSpread {
		t.Errorf("placement = %s, want Spread", ops.Status.HostMaintenance.CoordinatorPlacement)
	}
	job := h.mustJob(fdbCommandCoordinators)
	if got := job.Spec.Template.Spec.Containers[0].Args[0]; got != "coordinators "+strings.Join(want, " ") {
		t.Errorf("command = %q", got)
	}

	h.jobEnds(fdbCommandCoordinators, true)
	h.pass()
	if h.step() != string(stepMovingCoordinators) {
		t.Fatalf("step = %s, want MovingCoordinators while the file still names the host", h.step())
	}

	h.rewriteClusterFile("log-2", "log-3", "storage-4")
	h.pass()
	if h.step() != string(stepReleasing) {
		t.Fatalf("step = %s, want Releasing once the file agrees: %s", h.step(), h.current().Status.Message)
	}
	if h.recorder.count(CoordinatorsMoved) != 1 {
		t.Errorf("CoordinatorsMoved emitted %d times, want once", h.recorder.count(CoordinatorsMoved))
	}
}

// U-42: a completed command whose file still names the host is a reverted set.
func TestARevertedSetIsReportedAndFailsOnTheDeadline(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepMovingCoordinators)
	h.pass()
	h.jobEnds(fdbCommandCoordinators, true)
	h.pass()
	if h.recorder.count(CoordinatorsReverted) == 0 {
		t.Error("no CoordinatorsReverted event while the file still names the host after a completed move")
	}
	h.atStep(stepMovingCoordinators, time.Now().Add(-time.Minute))
	ops := h.runUntilDone()
	if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseFailed {
		t.Errorf("phase = %s, want Failed on the deadline", ops.Status.Phase)
	}
	if h.lockHolder() != "" {
		t.Errorf("activeOpsRef = %q, want the lock released by the failure", h.lockHolder())
	}
}

// U-43: a cluster too small to fill the list writes nothing and says so.
func TestATooSmallClusterMovesNothingAndSaysSo(t *testing.T) {
	world := append(labWorld(), aClusterFile("log-1", "storage-1", "log-2"))
	h := windowAt(t, world, stepMovingCoordinators)
	h.pass()
	ops := h.current()
	if ops.Status.HostMaintenance.CoordinatorPlacement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged {
		t.Errorf("placement = %s, want Unchanged", ops.Status.HostMaintenance.CoordinatorPlacement)
	}
	if _, created := h.job(fdbCommandCoordinators); created {
		t.Error("a coordinators Job was created with nothing to write")
	}
	if h.recorder.count(CoordinatorsNotMoved) != 1 {
		t.Errorf("CoordinatorsNotMoved emitted %d times, want once", h.recorder.count(CoordinatorsNotMoved))
	}
	if ops.Status.Step.State != string(stepReleasing) {
		t.Errorf("step = %s, want Releasing", ops.Status.Step.State)
	}
}

// A host holding no coordinator moves nothing and says nothing.
func TestAHostHoldingNoCoordinatorMovesNothingQuietly(t *testing.T) {
	world := fourHostWorld()
	world[len(world)-1] = aClusterFile("log-2", "log-3", "storage-4")
	h := windowAt(t, world, stepMovingCoordinators)
	h.pass()
	if h.step() != string(stepReleasing) {
		t.Errorf("step = %s, want Releasing", h.step())
	}
	if h.recorder.count(CoordinatorsNotMoved) != 0 || h.recorder.count(CoordinatorsMoved) != 0 {
		t.Error("an event was emitted for a host that held no coordinator")
	}
}

// U-44: a failed command leaves the placement Unchanged and says so.
func TestAFailedCoordinatorsCommandLeavesTheSetUnchanged(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepMovingCoordinators)
	h.pass()
	h.jobEnds(fdbCommandCoordinators, false)
	h.pass()
	ops := h.current()
	if ops.Status.HostMaintenance.CoordinatorPlacement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementUnchanged {
		t.Errorf("placement = %s, want Unchanged", ops.Status.HostMaintenance.CoordinatorPlacement)
	}
	if h.recorder.count(CoordinatorsNotMoved) != 1 {
		t.Errorf("CoordinatorsNotMoved emitted %d times, want once", h.recorder.count(CoordinatorsNotMoved))
	}
	if ops.Status.Step.State != string(stepReleasing) {
		t.Errorf("step = %s, want Releasing", ops.Status.Step.State)
	}
}

// U-46: a restart in MovingCoordinators reads the Job rather than recreating it.
func TestARestartInMovingCoordinatorsReadsTheExistingJob(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepMovingCoordinators)
	h.pass()
	first := h.mustJob(fdbCommandCoordinators)

	restarted := newWindowHarness(t, h.c, h.window)
	restarted.pass()
	again := restarted.mustJob(fdbCommandCoordinators)
	if first.UID != again.UID {
		t.Error("the restarted manager created a second coordinators Job")
	}
	var jobs batchv1.JobList
	if err := h.c.List(context.Background(), &jobs, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	n := 0
	for i := range jobs.Items {
		if strings.HasSuffix(jobs.Items[i].Name, "-"+string(fdbCommandCoordinators)) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d coordinators Jobs, want one", n)
	}
	if set := restarted.current().Status.HostMaintenance.Coordinators; len(set) != 3 {
		t.Errorf("the recorded set did not survive the restart: %v", set)
	}
}

// U-48, U-69, U-70: the step waits for the recorded process groups, not for
// the Node.
func TestAwaitingHostWaitsForTheRecordedProcessGroups(t *testing.T) {
	world := fourHostWorld()
	world[len(world)-2] = anFDBCluster(
		fdbProcessGroup{ID: "log-1", Class: "log", FaultDomain: drainedZone, Healthy: false},
		group("storage-1", "storage", drainedZone), group("log-2", "log", otherZone),
		group("log-3", "log", thirdZone), group("storage-4", "storage", fourthZone),
	)
	h := windowAt(t, world, stepAwaitingHost)

	h.pass()
	if h.step() != string(stepAwaitingHost) {
		t.Fatalf("step = %s, want AwaitingHost while log-1 is missing", h.step())
	}
	if h.recorder.count(AwaitingHostReturn) == 0 {
		t.Error("no AwaitingHostReturn event while the window waits")
	}

	// The Node stays cordoned, and the processes come back anyway.
	var node corev1.Node
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: drainedWorker}, &node); err != nil {
		t.Fatal(err)
	}
	node.Spec.Unschedulable = true
	if err := h.c.Update(context.Background(), &node); err != nil {
		t.Fatal(err)
	}
	h.fdbReports(
		group("log-1", "log", drainedZone), group("storage-1", "storage", drainedZone),
		group("log-2", "log", otherZone), group("log-3", "log", thirdZone), group("storage-4", "storage", fourthZone),
	)
	// The pods the eviction produced, ready: younger than the drain and running.
	drainedAt := metav1.NewTime(time.Now().Add(-time.Minute))
	ops := h.current()
	base := ops.DeepCopy()
	ops.Status.HostMaintenance.DrainObservedAt = &drainedAt
	if err := h.c.Status().Patch(context.Background(), ops, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"log-1", "storage-1"} {
		var pod corev1.Pod
		if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: ComponentFDBCluster + "-" + id}, &pod); err != nil {
			t.Fatal(err)
		}
		if err := h.c.Delete(context.Background(), &pod); err != nil {
			t.Fatal(err)
		}
		fresh := anFDBPod(id, drainedWorker)
		fresh.CreationTimestamp = metav1.Now()
		fresh.Status.Phase = corev1.PodRunning
		fresh.Status.ContainerStatuses = []corev1.ContainerStatus{{Ready: true}}
		if err := h.c.Create(context.Background(), fresh); err != nil {
			t.Fatal(err)
		}
	}
	h.pass()
	if h.step() != string(stepLeavingMaintenance) {
		t.Errorf("step = %s, want LeavingMaintenance once the replacements are ready, cordon or not: %s", h.step(), h.current().Status.Message)
	}
}

// U-71, U-50, U-92: a deleted Node and a process group that left the status
// both count as returned.
func TestAwaitingHostCompletesWithTheNodeGoneAndTheGroupsReplaced(t *testing.T) {
	world := fourHostWorld()
	world[len(world)-2] = anFDBCluster(
		group("log-2", "log", otherZone), group("log-3", "log", thirdZone), group("storage-4", "storage", fourthZone),
	)
	h := windowAt(t, world, stepAwaitingHost)
	if err := h.c.Delete(context.Background(), aNode(drainedWorker, drainedZone)); err != nil {
		t.Fatal(err)
	}
	h.pass()
	if h.step() != string(stepLeavingMaintenance) {
		t.Errorf("step = %s, want LeavingMaintenance with the groups gone from the status: %s", h.step(), h.current().Status.Message)
	}
}

// U-51, U-84: the deadline fails the window, which held no lock to release.
func TestAwaitingHostFailsOnItsDeadlineHoldingNoLock(t *testing.T) {
	world := fourHostWorld()
	world[len(world)-2] = anFDBCluster(
		fdbProcessGroup{ID: "log-1", Class: "log", FaultDomain: drainedZone, Healthy: false},
	)
	h := windowAt(t, world, stepAwaitingHost)
	h.atStep(stepAwaitingHost, time.Now().Add(-time.Minute))
	ops := h.runUntilDone()
	if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseFailed {
		t.Errorf("phase = %s, want Failed", ops.Status.Phase)
	}
	if h.lockHolder() != "" {
		t.Errorf("activeOpsRef = %q, want nothing: AwaitingHost holds no lock", h.lockHolder())
	}
}

// U-83, the restart half: a lock still held in a lock-free step is released on
// the next pass.
func TestALockLeftHeldInALockFreeStepIsReleased(t *testing.T) {
	world := fourHostWorld()
	cp := world[0].(*simplyblockv1alpha2.ControlPlane)
	window := aWindowFor(drainedWorker)
	cp.Status.ActiveOpsRef = window.Name
	h := newWindowHarness(t, newClient(t, append(world, window)...), window)
	h.pass()
	h.atStep(stepAwaitingHost, time.Now().Add(time.Hour))
	h.pass()
	if h.lockHolder() != "" {
		t.Errorf("activeOpsRef = %q, want the stale lock released", h.lockHolder())
	}
}

// U-37: the zone is cleared when it is still the window's own.
func TestLeavingMaintenanceClearsTheWindowsOwnZone(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepLeavingMaintenance)
	h.setMaintenanceUntil(time.Now().Add(time.Hour))

	h.pass()
	status := h.mustJob(fdbCommandMaintenanceStatus)
	if got := status.Spec.Template.Spec.Containers[0].Args[0]; got != "maintenance" {
		t.Errorf("status command = %q, want the bare maintenance command, which prints the status", got)
	}
	h.statusSays("Maintenance for zone " + drainedZone + " will continue for 3000 seconds.")
	h.jobEnds(fdbCommandMaintenanceStatus, true)
	h.pass()
	off := h.mustJob(fdbCommandMaintenanceOff)
	if got := off.Spec.Template.Spec.Containers[0].Args[0]; got != "maintenance off" {
		t.Errorf("off command = %q", got)
	}
	h.jobEnds(fdbCommandMaintenanceOff, true)
	h.pass()
	if h.step() != string(stepCleanup) {
		t.Fatalf("step = %s, want Cleanup: %s", h.step(), h.current().Status.Message)
	}
	if h.recorder.count(MaintenanceModeCleared) != 1 {
		t.Errorf("MaintenanceModeCleared emitted %d times, want once", h.recorder.count(MaintenanceModeCleared))
	}
}

// U-87, U-88: another window's zone, or none, is left alone.
func TestLeavingMaintenanceLeavesAnotherZoneAlone(t *testing.T) {
	for name, message := range map[string]string{
		"another zone": "Maintenance for zone " + otherZone + " will continue for 3000 seconds.",
		"no zone":      "No ongoing maintenance.",
	} {
		t.Run(name, func(t *testing.T) {
			h := windowAt(t, fourHostWorld(), stepLeavingMaintenance)
			h.setMaintenanceUntil(time.Now().Add(time.Hour))
			h.pass()
			h.statusSays(message)
			h.jobEnds(fdbCommandMaintenanceStatus, true)
			h.pass()
			if _, created := h.job(fdbCommandMaintenanceOff); created {
				t.Error("maintenance off was issued for a zone that is not the window's")
			}
			if h.step() != string(stepCleanup) {
				t.Errorf("step = %s, want Cleanup: %s", h.step(), h.current().Status.Message)
			}
			if h.recorder.count(MaintenanceModeCleared) != 0 {
				t.Error("MaintenanceModeCleared emitted although nothing was cleared")
			}
		})
	}
}

// U-38, U-95: a failed clear, and an answer the step cannot read, both leave
// the zone to expire and say so.
func TestLeavingMaintenanceReportsWhatItCouldNotClear(t *testing.T) {
	t.Run("the off command fails", func(t *testing.T) {
		h := windowAt(t, fourHostWorld(), stepLeavingMaintenance)
		h.setMaintenanceUntil(time.Now().Add(time.Hour))
		h.pass()
		h.statusSays("Maintenance for zone " + drainedZone + " will continue for 3000 seconds.")
		h.jobEnds(fdbCommandMaintenanceStatus, true)
		h.pass()
		h.jobEnds(fdbCommandMaintenanceOff, false)
		h.pass()
		if h.step() != string(stepCleanup) || h.recorder.count(MaintenanceModeLeft) != 1 {
			t.Errorf("step = %s, MaintenanceModeLeft %d times; want Cleanup and once", h.step(), h.recorder.count(MaintenanceModeLeft))
		}
	})
	t.Run("the status cannot be read", func(t *testing.T) {
		h := windowAt(t, fourHostWorld(), stepLeavingMaintenance)
		h.setMaintenanceUntil(time.Now().Add(time.Hour))
		h.pass()
		h.statusSays("ERROR: something fdbcli never said before")
		h.jobEnds(fdbCommandMaintenanceStatus, true)
		h.pass()
		if h.step() != string(stepCleanup) || h.recorder.count(MaintenanceModeLeft) != 1 {
			t.Errorf("step = %s, MaintenanceModeLeft %d times; want Cleanup and once", h.step(), h.recorder.count(MaintenanceModeLeft))
		}
	})
}

// A window that never set maintenance mode has nothing to clear.
func TestLeavingMaintenanceWithNothingSetIssuesNothing(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepLeavingMaintenance)
	h.pass()
	if _, created := h.job(fdbCommandMaintenanceStatus); created {
		t.Error("a status Job was created although the window set no zone")
	}
	if h.step() != string(stepCleanup) {
		t.Errorf("step = %s, want Cleanup", h.step())
	}
}

// U-68: a finished window reports Completed, Succeeded, and its placement.
func TestAFinishedWindowReportsItsPlacement(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepCleanup)
	ops := h.current()
	base := ops.DeepCopy()
	ops.Status.HostMaintenance.CoordinatorPlacement = simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSharedHost
	if err := h.c.Status().Patch(context.Background(), ops, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	done := h.runUntilDone()
	if done.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded ||
		done.Status.Step.State != string(simplyblockv1alpha2.ControlPlaneOpsStepCompleted) {
		t.Errorf("phase %s step %s, want Succeeded and Completed", done.Status.Phase, done.Status.Step.State)
	}
	if done.Status.HostMaintenance.CoordinatorPlacement != simplyblockv1alpha2.HostMaintenanceCoordinatorPlacementSharedHost {
		t.Errorf("placement = %s, want SharedHost kept on the record", done.Status.HostMaintenance.CoordinatorPlacement)
	}
}

// The lab's first drain ended AwaitingHost nine seconds before the FoundationDB
// operator wrote MissingProcesses on the evicted groups, so the window cleared
// the zone while the host was still away. The step has to wait for the pods
// the drain evicted to be replaced and ready, and may take a pod older than
// the drain as returned only once the worker is schedulable again, which is
// the cordon that was never drained.
//
// Regression: 2026-11-11-awaiting-host-ends-before-the-eviction-is-visible.
func TestAwaitingHostWaitsForThePodsTheDrainEvictedToReturn(t *testing.T) {
	drainedAt := metav1.NewTime(time.Now().Add(-time.Minute))
	before := metav1.NewTime(drainedAt.Add(-time.Hour))
	after := metav1.NewTime(drainedAt.Add(30 * time.Second))

	readyPod := func(id, worker string, created metav1.Time, ready bool) *corev1.Pod {
		pod := anFDBPod(id, worker)
		pod.CreationTimestamp = created
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Ready: ready}}
		return pod
	}
	withDrain := func(h *windowHarness) {
		ops := h.current()
		base := ops.DeepCopy()
		ops.Status.HostMaintenance.DrainObservedAt = &drainedAt
		if err := h.c.Status().Patch(context.Background(), ops, client.MergeFrom(base)); err != nil {
			t.Fatal(err)
		}
	}
	world := func(pods ...*corev1.Pod) []client.Object {
		w := fourHostWorld()
		kept := w[:0]
		for _, o := range w {
			if p, isPod := o.(*corev1.Pod); isPod && (p.Name == ComponentFDBCluster+"-log-1" || p.Name == ComponentFDBCluster+"-storage-1") {
				continue
			}
			kept = append(kept, o)
		}
		for _, p := range pods {
			kept = append(kept, p)
		}
		return kept
	}

	t.Run("the pods from before the drain, still running, do not count while the node is cordoned", func(t *testing.T) {
		cordon := world(readyPod("log-1", drainedWorker, before, true), readyPod("storage-1", drainedWorker, before, true))
		for i := range cordon {
			if n, isNode := cordon[i].(*corev1.Node); isNode && n.Name == drainedWorker {
				n.Spec.Unschedulable = true
			}
		}
		h := windowAt(t, cordon, stepAwaitingHost)
		withDrain(h)
		h.pass()
		if h.step() != string(stepAwaitingHost) {
			t.Errorf("step = %s, want AwaitingHost: the eviction has not shown yet", h.step())
		}
	})

	t.Run("replacement pods that are not ready do not count", func(t *testing.T) {
		h := windowAt(t, world(readyPod("log-1", drainedWorker, after, false), readyPod("storage-1", drainedWorker, after, false)), stepAwaitingHost)
		withDrain(h)
		h.pass()
		if h.step() != string(stepAwaitingHost) {
			t.Errorf("step = %s, want AwaitingHost while the replacements are not ready", h.step())
		}
	})

	t.Run("replacement pods that are ready complete the step, cordon or not", func(t *testing.T) {
		h := windowAt(t, world(readyPod("log-1", otherWorker, after, true), readyPod("storage-1", otherWorker, after, true)), stepAwaitingHost)
		withDrain(h)
		h.pass()
		if h.step() != string(stepLeavingMaintenance) {
			t.Errorf("step = %s, want LeavingMaintenance: %s", h.step(), h.current().Status.Message)
		}
	})

	t.Run("the pods from before the drain count once the node is schedulable again", func(t *testing.T) {
		h := windowAt(t, world(readyPod("log-1", drainedWorker, before, true), readyPod("storage-1", drainedWorker, before, true)), stepAwaitingHost)
		withDrain(h)
		h.pass()
		if h.step() != string(stepLeavingMaintenance) {
			t.Errorf("step = %s, want LeavingMaintenance for a cordon that was never drained: %s", h.step(), h.current().Status.Message)
		}
	})
}

// A pass that runs on a window read before the previous pass's status write
// has reached the cache must not perform the step again: the Job it finds is
// the same, but the event it would emit is a duplicate on the audit record,
// which both lab runs showed as a doubled MaintenanceModeSet. The status write
// carries the window's resourceVersion, so the stale pass fails on a conflict
// and the next pass reads the fresh object.
//
// Regression: 2026-11-11-a-stale-pass-emits-the-maintenance-event-twice.
func TestAStalePassDoesNotRepeatAStepsEvent(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepEnteringMaintenance)
	h.pass()
	h.jobEnds(fdbCommandMaintenanceOn, true)
	stale := h.current()

	h.pass()
	if h.recorder.count(MaintenanceModeSet) != 1 {
		t.Fatalf("MaintenanceModeSet emitted %d times after the pass, want once", h.recorder.count(MaintenanceModeSet))
	}

	// The same step performed again on the object as it was before that pass.
	cp := &simplyblockv1alpha2.ControlPlane{}
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: SingletonName}, cp); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.r.windowEnterMaintenance(context.Background(), stale, cp)
	if err == nil {
		t.Fatal("the stale pass succeeded, and its event is a duplicate on the record")
	}
	if !apierrors.IsConflict(err) {
		t.Errorf("err = %v, want a conflict the controller requeues on", err)
	}
	if h.recorder.count(MaintenanceModeSet) != 1 {
		t.Errorf("MaintenanceModeSet emitted %d times, want still once", h.recorder.count(MaintenanceModeSet))
	}
}
