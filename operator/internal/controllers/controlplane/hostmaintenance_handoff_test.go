// Tests for the hand-off of design-fdb-host-maintenance.md §5.12: the window
// excludes the worker's log and stateless processes before it releases the
// budget, so their roles move while they are alive, and includes them again
// once they are back. They run the steps against the fake client with Jobs
// whose conditions the test sets by hand, as hostmaintenance_database_test.go
// does for the other database steps.

package controlplane

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// handoffWorld is the lab after the stateless layout: a log, a stateless, and
// a storage process on vm11, the coordinators already off it.
func handoffWorld() []client.Object {
	return []client.Object{
		localControlPlane(),
		aNode(drainedWorker, drainedZone), aNode(otherWorker, otherZone),
		aNode(thirdWorker, thirdZone), aNode(fourthWorker, fourthZone),
		anFDBPod("log-1", drainedWorker), anFDBPod("stateless-1", drainedWorker), anFDBPod("storage-1", drainedWorker),
		anFDBPod("log-2", otherWorker), anFDBPod("log-3", thirdWorker), anFDBPod("storage-4", fourthWorker),
		anFDBCluster(
			group("log-1", "log", drainedZone), group("stateless-1", "stateless", drainedZone),
			group("storage-1", "storage", drainedZone),
			group("log-2", "log", otherZone), group("log-3", "log", thirdZone),
			group("storage-4", "storage", fourthZone),
		),
		aClusterFile("log-2", "log-3", "storage-4"),
	}
}

// recorded rewrites the process groups Holding recorded for the window.
func (h *windowHarness) recorded(ids ...string) {
	h.t.Helper()
	ops := h.current()
	base := ops.DeepCopy()
	ops.Status.HostMaintenance.ProcessGroups = ids
	if err := h.c.Status().Patch(context.Background(), ops, client.MergeFrom(base)); err != nil {
		h.t.Fatal(err)
	}
}

// excluded writes the exclusion an earlier ExcludingProcesses would have.
func (h *windowHarness) excluded(ids ...string) {
	h.t.Helper()
	ops := h.current()
	base := ops.DeepCopy()
	ops.Status.HostMaintenance.ExcludedProcesses = ids
	if err := h.c.Status().Patch(context.Background(), ops, client.MergeFrom(base)); err != nil {
		h.t.Fatal(err)
	}
}

func commandOf(job *batchv1.Job) string {
	return job.Spec.Template.Spec.Containers[0].Args[0]
}

func jobEnv(job *batchv1.Job, name string) string {
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}

// U-102, U-103: the log and stateless groups are recorded before the Job,
// named by locality and never the storage group, and the completed exclusion
// releases with ProcessesHandedOff.
func TestExcludingProcessesHandsTheLogAndStatelessRolesOff(t *testing.T) {
	h := windowAt(t, handoffWorld(), stepExcludingProcesses)
	h.recorded("log-1", "stateless-1", "storage-1")

	h.pass()
	ops := h.current()
	want := []string{"log-1", "stateless-1"}
	if !slices.Equal(ops.Status.HostMaintenance.ExcludedProcesses, want) {
		t.Fatalf("status.excludedProcesses = %v, want %v", ops.Status.HostMaintenance.ExcludedProcesses, want)
	}
	job := h.mustJob(fdbCommandExclude)
	if got := commandOf(job); got != "exclude locality_instance_id:log-1 locality_instance_id:stateless-1" {
		t.Errorf("command = %q", got)
	}
	if strings.Contains(commandOf(job), "storage") {
		t.Errorf("the storage process is in the exclusion: %q", commandOf(job))
	}
	if got := jobEnv(job, fdbcliTimeoutEnv); got != strconv.Itoa(excludeTimeoutSeconds) {
		t.Errorf("fdbcli timeout for the waiting exclude = %q, want %d", got, excludeTimeoutSeconds)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != excludeJobDeadlineSeconds {
		t.Errorf("exclude Job deadline = %v, want %d", job.Spec.ActiveDeadlineSeconds, excludeJobDeadlineSeconds)
	}
	if h.step() != string(stepExcludingProcesses) {
		t.Fatalf("step = %s, want ExcludingProcesses while the Job runs", h.step())
	}

	h.jobEnds(fdbCommandExclude, true)
	h.pass()
	if h.step() != string(stepReleasing) {
		t.Fatalf("step = %s, want Releasing after the exclusion: %s", h.step(), h.current().Status.Message)
	}
	if h.recorder.count(ProcessesHandedOff) != 1 {
		t.Errorf("ProcessesHandedOff emitted %d times, want once", h.recorder.count(ProcessesHandedOff))
	}
}

// U-104: a failed exclusion releases anyway, says so, and keeps the list for
// the include.
func TestAFailedExclusionReleasesAndKeepsTheListForTheInclude(t *testing.T) {
	h := windowAt(t, handoffWorld(), stepExcludingProcesses)
	h.recorded("log-1", "stateless-1", "storage-1")
	h.pass()
	h.jobEnds(fdbCommandExclude, false)

	h.pass()
	if h.step() != string(stepReleasing) {
		t.Fatalf("step = %s, want Releasing after a failed exclusion", h.step())
	}
	if h.recorder.count(ProcessesNotHandedOff) != 1 {
		t.Errorf("ProcessesNotHandedOff emitted %d times, want once", h.recorder.count(ProcessesNotHandedOff))
	}
	if got := h.current().Status.HostMaintenance.ExcludedProcesses; !slices.Equal(got, []string{"log-1", "stateless-1"}) {
		t.Errorf("excludedProcesses = %v after the failure, want the list kept", got)
	}
}

// U-105: a host with only storage processes has nothing to hand off.
func TestAStorageOnlyHostExcludesNothing(t *testing.T) {
	h := windowAt(t, handoffWorld(), stepExcludingProcesses)
	h.recorded("storage-1")

	h.pass()
	if h.step() != string(stepReleasing) {
		t.Fatalf("step = %s, want Releasing with nothing to exclude", h.step())
	}
	if _, ok := h.job(fdbCommandExclude); ok {
		t.Error("an exclude Job was created for a storage-only host")
	}
	if len(h.current().Status.HostMaintenance.ExcludedProcesses) != 0 {
		t.Errorf("excludedProcesses = %v, want none", h.current().Status.HostMaintenance.ExcludedProcesses)
	}
	if h.recorder.count(ProcessesHandedOff)+h.recorder.count(ProcessesNotHandedOff) != 0 {
		t.Error("a hand-off event was emitted with nothing handed off")
	}
}

// U-106: a restart in ExcludingProcesses reads the Job it created.
func TestARestartInExcludingProcessesReadsTheExistingJob(t *testing.T) {
	h := windowAt(t, handoffWorld(), stepExcludingProcesses)
	h.recorded("log-1", "stateless-1", "storage-1")
	h.pass()

	restarted := newWindowHarness(t, h.c, h.window)
	restarted.pass()
	var jobs batchv1.JobList
	if err := h.c.List(context.Background(), &jobs, client.InNamespace(testNamespace),
		client.MatchingLabels{fdbMaintenanceApp: ""}); err != nil {
		t.Fatal(err)
	}
	excludes := 0
	for i := range jobs.Items {
		if strings.HasSuffix(jobs.Items[i].Name, "-"+string(fdbCommandExclude)) {
			excludes++
		}
	}
	var all batchv1.JobList
	if err := h.c.List(context.Background(), &all, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	for i := range all.Items {
		if strings.HasSuffix(all.Items[i].Name, "-"+string(fdbCommandExclude)) {
			excludes++
		}
	}
	if excludes != 1 {
		t.Errorf("%d exclude Jobs after a restart, want one", excludes)
	}
	if restarted.step() != string(stepExcludingProcesses) {
		t.Errorf("step = %s, want ExcludingProcesses while the Job runs", restarted.step())
	}
}

// U-107: the recorded exclusion is lifted once the processes are back.
func TestIncludingProcessesLiftsTheRecordedExclusion(t *testing.T) {
	h := windowAt(t, handoffWorld(), stepIncludingProcesses)
	h.excluded("log-1", "stateless-1")

	h.pass()
	job := h.mustJob(fdbCommandInclude)
	if got := commandOf(job); got != "include locality_instance_id:log-1 locality_instance_id:stateless-1" {
		t.Errorf("command = %q", got)
	}
	if h.step() != string(stepIncludingProcesses) {
		t.Fatalf("step = %s, want IncludingProcesses while the Job runs", h.step())
	}

	h.jobEnds(fdbCommandInclude, true)
	h.pass()
	if h.step() != string(stepLeavingMaintenance) {
		t.Fatalf("step = %s, want LeavingMaintenance after the include: %s", h.step(), h.current().Status.Message)
	}
	if h.recorder.count(ProcessesIncluded) != 1 {
		t.Errorf("ProcessesIncluded emitted %d times, want once", h.recorder.count(ProcessesIncluded))
	}
}

// U-108: a failed include completes the step and hands the command to a human.
func TestAFailedIncludeNamesTheCommandToRunByHand(t *testing.T) {
	h := windowAt(t, handoffWorld(), stepIncludingProcesses)
	h.excluded("log-1", "stateless-1")
	h.pass()
	h.jobEnds(fdbCommandInclude, false)

	h.pass()
	if h.step() != string(stepLeavingMaintenance) {
		t.Fatalf("step = %s, want LeavingMaintenance after a failed include", h.step())
	}
	if h.recorder.count(ProcessesLeftExcluded) != 1 {
		t.Fatalf("ProcessesLeftExcluded emitted %d times, want once", h.recorder.count(ProcessesLeftExcluded))
	}
	if msg := h.recorder.last(ProcessesLeftExcluded); !strings.Contains(msg, "include locality_instance_id:log-1 locality_instance_id:stateless-1") {
		t.Errorf("ProcessesLeftExcluded = %q, want the include command in it", msg)
	}
}

// U-109: a window that excluded nothing includes nothing.
func TestAWindowThatExcludedNothingIncludesNothing(t *testing.T) {
	h := windowAt(t, handoffWorld(), stepIncludingProcesses)

	h.pass()
	if h.step() != string(stepLeavingMaintenance) {
		t.Fatalf("step = %s, want LeavingMaintenance with nothing to include", h.step())
	}
	if _, ok := h.job(fdbCommandInclude); ok {
		t.Error("an include Job was created with nothing excluded")
	}
}

// U-110: a window that fails after excluding leaves the include behind it.
func TestAWindowThatFailsAfterExcludingStillIncludes(t *testing.T) {
	h := windowAt(t, handoffWorld(), stepAwaitingHost)
	h.excluded("log-1", "stateless-1")
	h.atStep(stepAwaitingHost, time.Now().Add(-time.Minute))

	ops := h.runUntilDone()
	if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseFailed {
		t.Fatalf("phase = %s, want Failed on the deadline", ops.Status.Phase)
	}
	job, ok := h.job(fdbCommandInclude)
	if !ok {
		t.Fatal("no include Job after the failed window's teardown")
	}
	if got := commandOf(job); got != "include locality_instance_id:log-1 locality_instance_id:stateless-1" {
		t.Errorf("command = %q", got)
	}
	if h.recorder.count(MaintenanceMarkersLeft) != 0 {
		t.Errorf("MaintenanceMarkersLeft emitted although the include Job was created: %s",
			h.recorder.last(MaintenanceMarkersLeft))
	}
}

// The include Job is a window Job like the others: placed off the worker,
// tolerant of the cordon, owned by the operation.
func TestTheIncludeJobIsPlacedLikeEveryWindowJob(t *testing.T) {
	h := windowAt(t, handoffWorld(), stepIncludingProcesses)
	h.excluded("log-1")
	h.pass()
	job := h.mustJob(fdbCommandInclude)
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Name != h.window.Name {
		t.Errorf("owner = %v, want the window", job.OwnerReferences)
	}
	tolerated := false
	for _, tol := range job.Spec.Template.Spec.Tolerations {
		if tol.Key == corev1.TaintNodeUnschedulable {
			tolerated = true
		}
	}
	if !tolerated {
		t.Error("the include Job does not tolerate the cordon")
	}
}
