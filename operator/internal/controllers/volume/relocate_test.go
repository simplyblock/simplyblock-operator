// The Relocate action's steps (design-superclusters.md §8.2): replicate into
// the target member, commit, attach the target's paths on every consuming
// node while each cutover waits, release it, and verify that the volume's
// handle resolves to the target.
//
// The control plane is fakeRelocation, which answers from maps a test sets
// and records every call: the counts are what the restart and resend cases
// assert on.

package volume

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	relocateTargetCR   = "prod-w2"
	relocateTargetID   = "77777777-7777-7777-7777-777777777777"
	relocateTargetPool = "88888888-8888-8888-8888-888888888888"
	relocateSupercl    = "prod"
	relocateConsumer   = "worker-1"
	relocateClaim      = "data"
)

// targetVolumeOf is the UUID a volume has in the target after its cutover.
func targetVolumeOf(volumeID string) string { return "9" + volumeID[1:] }

// fakeRelocation is the control plane's side of a Relocate.
type fakeRelocation struct {
	startErr  error
	starts    map[string]controlplane.MoveTarget
	stops     int
	info      map[string]controlplane.ReplicationStatus
	commitErr error
	commits   map[string]int
	rel       map[string]controlplane.Relationship
	proceeds  map[string]int
	conn      lvol.Connection
	tasks     map[string]controlplane.Task
}

func (f *fakeRelocation) init() {
	if f.starts == nil {
		f.starts = map[string]controlplane.MoveTarget{}
		f.info = map[string]controlplane.ReplicationStatus{}
		f.commits = map[string]int{}
		f.rel = map[string]controlplane.Relationship{}
		f.proceeds = map[string]int{}
		f.tasks = map[string]controlplane.Task{}
	}
}

func volumeOf(h lvol.VolumeHandle) string {
	_, _, v, _ := h.Split()
	return v.String()
}

func (f *fakeRelocation) StartVolumeMove(_ context.Context, h lvol.VolumeHandle, t controlplane.MoveTarget) error {
	f.init()
	if f.startErr != nil {
		return f.startErr
	}
	f.starts[volumeOf(h)] = t
	f.info[volumeOf(h)] = controlplane.ReplicationStatus{Role: "source", State: "replicating"}
	return nil
}

func (f *fakeRelocation) StopVolumeReplication(context.Context, lvol.VolumeHandle) error {
	f.stops++
	return nil
}

func (f *fakeRelocation) GetVolumeReplicationInfo(_ context.Context, h lvol.VolumeHandle) (controlplane.ReplicationStatus, error) {
	f.init()
	if info, ok := f.info[volumeOf(h)]; ok {
		return info, nil
	}
	return controlplane.ReplicationStatus{Role: "none", State: "not_replicating"}, nil
}

// CommitVolumeMove answers every commit of a volume with the same task, as the
// control plane does with its 409.
func (f *fakeRelocation) CommitVolumeMove(_ context.Context, h lvol.VolumeHandle) (string, error) {
	f.init()
	if f.commitErr != nil {
		return "", f.commitErr
	}
	f.commits[volumeOf(h)]++
	task := "task-" + volumeOf(h)
	if _, ok := f.tasks[task]; !ok {
		f.tasks[task] = controlplane.Task{ID: task, Status: "running"}
	}
	return task, nil
}

func (f *fakeRelocation) GetVolumeReplicationRelationship(_ context.Context, h lvol.VolumeHandle) (controlplane.Relationship, error) {
	f.init()
	if rel, ok := f.rel[volumeOf(h)]; ok {
		return rel, nil
	}
	return controlplane.Relationship{}, fmt.Errorf("relationship of %s: %w", h, errs.ErrNotFound)
}

func (f *fakeRelocation) Connection(context.Context, lvol.VolumeHandle, ...lvol.ConnectionOption) (lvol.Connection, error) {
	return f.conn, nil
}

func (f *fakeRelocation) ProceedVolumeCutover(_ context.Context, h lvol.VolumeHandle) error {
	f.init()
	f.proceeds[volumeOf(h)]++
	return nil
}

func (f *fakeRelocation) Task(_ context.Context, _, taskID string) (controlplane.Task, error) {
	f.init()
	if task, ok := f.tasks[taskID]; ok {
		return task, nil
	}
	return controlplane.Task{}, fmt.Errorf("task %s: %w", taskID, errs.ErrNotFound)
}

// inSync reports every started volume in sync.
func (f *fakeRelocation) inSync() {
	for volume := range f.starts {
		f.info[volume] = controlplane.ReplicationStatus{Role: "source", State: "in_sync"}
	}
}

// cutoverPending is the control plane holding a volume's cutover for the
// hosts.
func (f *fakeRelocation) cutoverPending(volume string) {
	f.rel[volume] = controlplane.Relationship{
		IsSource: true, State: controlplane.RelationshipCutoverPending, Mode: "migration",
		SourceClusterID: testClusterID, SourceLvolID: volume,
		TargetClusterID: relocateTargetID, TargetPoolID: relocateTargetPool, TargetLvolID: targetVolumeOf(volume),
	}
}

// cutOver is the volume served by the target, its task done.
func (f *fakeRelocation) cutOver(volume string) {
	f.cutoverPending(volume)
	rel := f.rel[volume]
	rel.State = controlplane.RelationshipCutoverDone
	rel.ActiveLvolID = targetVolumeOf(volume)
	f.rel[volume] = rel
	task := "task-" + volume
	f.tasks[task] = controlplane.Task{ID: task, Status: "done", Result: "cutover done"}
}

func relocateOps() *simplyblockv1alpha2.PersistentVolumeOps {
	ops := testOperation()
	ops.Spec.Action = simplyblockv1alpha2.PersistentVolumeOpsActionRelocate
	ops.Spec.Migrate = nil
	ops.Spec.Relocate = &simplyblockv1alpha2.RelocateVolumeSpec{TargetClusterRef: relocateTargetCR}
	return ops
}

func relocateMember(name, uuid string) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       simplyblockv1alpha2.StorageClusterSpec{SuperclusterRef: relocateSupercl},
		Status: simplyblockv1alpha2.StorageClusterStatus{
			UUID: uuid, Phase: simplyblockv1alpha2.StorageClusterPhaseOnline,
		},
	}
}

// relocateWorld is a volume in member production, consumed on worker-1, and
// a target member prod-w2 holding the same supercluster pool.
func relocateWorld(extra ...client.Object) []client.Object {
	return append([]client.Object{
		relocateOps(),
		claimedVolume(testPVName, relocateClaim),
		runningPodOn(relocateConsumer, relocateClaim),
		relocateMember(testClusterCR, testClusterID),
		relocateMember(relocateTargetCR, relocateTargetID),
		&simplyblockv1alpha2.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-default", Namespace: testNamespace},
			Spec:       simplyblockv1alpha2.StoragePoolSpec{ClusterRef: relocateSupercl},
			Status: simplyblockv1alpha2.StoragePoolStatus{Members: []simplyblockv1alpha2.PoolMemberStatus{
				{Cluster: testClusterCR, UUID: testPoolID},
				{Cluster: relocateTargetCR, UUID: relocateTargetPool},
			}},
		},
	}, extra...)
}

func relocatingSubsystem() *fakeControlPlane {
	return &fakeControlPlane{
		volume: lvol.Volume{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, testVolumeID), NQN: testNQN},
		fakeRelocation: fakeRelocation{conn: lvol.Connection{NQN: testNQN, Endpoints: []lvol.Endpoint{
			{Transport: "tcp", Address: "10.0.1.7", Port: 4420},
			{Transport: "tcp", Address: "10.0.0.1", Port: 4420},
		}}},
	}
}

func relocateReconciler(t *testing.T, api *fakeControlPlane) *PersistentVolumeOpsReconciler {
	t.Helper()
	api.init()
	return testReconciler(t, api, relocateWorld()...)
}

// passesUntil runs passes until the operation reaches the step or ends.
func passesUntil(t *testing.T, r *PersistentVolumeOpsReconciler, want step) *simplyblockv1alpha2.PersistentVolumeOps {
	t.Helper()
	for range 30 {
		runPass(t, r)
		ops := operationFrom(t, r)
		if ops.Status.Step.State == string(want) || isTerminal(ops.Status.Phase) {
			return ops
		}
	}
	ops := operationFrom(t, r)
	t.Fatalf("the operation never reached %s: step %s, phase %s, message %q",
		want, ops.Status.Step.State, ops.Status.Phase, ops.Status.Message)
	return nil
}

func isTerminal(phase simplyblockv1alpha2.PersistentVolumeOpsPhase) bool {
	switch phase {
	case simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded,
		simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed,
		simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted:
		return true
	}
	return false
}

// pathJobs are the Jobs a Relocate created.
func pathJobs(t *testing.T, r *PersistentVolumeOpsReconciler) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	return jobs.Items
}

// finishJobs marks every Job complete, or failed.
func finishJobs(t *testing.T, r *PersistentVolumeOpsReconciler, condition batchv1.JobConditionType) {
	t.Helper()
	for _, job := range pathJobs(t, r) {
		job.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue}}
		if err := r.Status().Update(context.Background(), &job); err != nil {
			t.Fatalf("update job %s: %v", job.Name, err)
		}
	}
}

func eventsOf(r *PersistentVolumeOpsReconciler) string {
	recorder := r.Recorder.(*events.FakeRecorder)
	var out []string
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return strings.Join(out, "\n")
		}
	}
}

// I-44: start, wait for sync, commit once, connect the target's paths on the
// consuming node at cutover_pending, release once, and verify through the
// relationship, marking the volume with its new handle.
func TestARelocateCutsTheVolumeOverToTheTarget(t *testing.T) {
	api := relocatingSubsystem()
	r := relocateReconciler(t, api)

	passesUntil(t, r, stepReplicating)
	runPass(t, r)
	if got, ok := api.starts[testVolumeID]; !ok || got.ClusterID != relocateTargetID ||
		got.PoolID != relocateTargetPool || got.IntervalMinutes < 1 {
		t.Fatalf("start = %+v, want the target member and its pool with an interval", api.starts)
	}

	api.inSync()
	passesUntil(t, r, stepAwaitingCutover)
	runPass(t, r)
	if api.commits[testVolumeID] != 1 {
		t.Errorf("commits = %v, want one", api.commits)
	}
	if len(pathJobs(t, r)) != 0 {
		t.Fatal("paths attached before the cutover was pending")
	}

	api.cutoverPending(testVolumeID)
	passesUntil(t, r, stepAttachingPaths)
	runPass(t, r)
	jobs := pathJobs(t, r)
	if len(jobs) != 1 || jobs[0].Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != relocateConsumer {
		t.Fatalf("jobs = %d, want one on %s", len(jobs), relocateConsumer)
	}
	if !strings.Contains(fmt.Sprint(jobs[0].Spec.Template.Spec.Containers[0].Env), "10.0.1.7") {
		t.Error("the Job does not carry the target's path")
	}
	if api.proceeds[testVolumeID] != 0 {
		t.Fatal("the cutover was released before the node held the target's paths")
	}

	finishJobs(t, r, batchv1.JobComplete)
	passesUntil(t, r, stepProceeding)
	runPass(t, r)
	if api.proceeds[testVolumeID] != 1 {
		t.Fatalf("proceeds = %v, want one", api.proceeds)
	}

	api.cutOver(testVolumeID)
	ops := passesUntil(t, r, "never")
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %s (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	var pv corev1.PersistentVolume
	if err := r.Get(context.Background(), types.NamespacedName{Name: testPVName}, &pv); err != nil {
		t.Fatal(err)
	}
	want := string(lvol.NewVolumeHandle(relocateTargetID, relocateTargetPool, targetVolumeOf(testVolumeID)))
	if got := pv.Annotations[RelocatedToAnnotation]; got != want {
		t.Errorf("relocated-to = %q, want %q", got, want)
	}
	if len(pathJobs(t, r)) != 0 {
		t.Error("the path Jobs outlived the operation")
	}
	if api.commits[testVolumeID] != 1 || api.proceeds[testVolumeID] != 1 {
		t.Errorf("commits %v, proceeds %v, want one each", api.commits, api.proceeds)
	}
}

// I-31: volumes sharing the subsystem are listed and moved together, each
// cut over in turn.
func TestVolumesSharingTheSubsystemMoveTogether(t *testing.T) {
	const sibling = "a3333333-3333-3333-3333-333333333333"
	api := relocatingSubsystem()
	api.members = []lvol.Volume{
		{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, testVolumeID), NQN: testNQN},
		{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, sibling), NQN: testNQN},
	}
	r := relocateReconciler(t, api)

	ops := passesUntil(t, r, stepReplicating)
	if ops.Status.Relocate == nil || len(ops.Status.Relocate.Volumes) != 2 {
		t.Fatalf("status.relocate = %+v, want both volumes listed", ops.Status.Relocate)
	}
	runPass(t, r)
	api.inSync()
	passesUntil(t, r, stepAwaitingCutover)
	runPass(t, r)
	if api.commits[testVolumeID] != 1 || api.commits[sibling] != 1 {
		t.Fatalf("commits = %v, want both volumes committed", api.commits)
	}

	for _, volume := range []string{testVolumeID, sibling} {
		api.cutoverPending(volume)
		passesUntil(t, r, stepAttachingPaths)
		runPass(t, r)
		finishJobs(t, r, batchv1.JobComplete)
		passesUntil(t, r, stepProceeding)
		runPass(t, r)
		api.cutOver(volume)
	}
	ops = passesUntil(t, r, "never")
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %s (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	// Each release is resent while its cutover is pending, so the count is
	// at least one rather than exactly one.
	if api.proceeds[testVolumeID] < 1 || api.proceeds[sibling] < 1 {
		t.Errorf("proceeds = %v, want each volume released", api.proceeds)
	}
}

// I-32: an abort before the commit stops the replication and ends Aborted.
func TestAnAbortBeforeTheCommitStopsTheReplication(t *testing.T) {
	api := relocatingSubsystem()
	r := relocateReconciler(t, api)
	passesUntil(t, r, stepReplicating)
	runPass(t, r)

	abort(t, r)
	ops := passesUntil(t, r, "never")

	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted || api.stops != 1 {
		t.Errorf("phase %s, stops %d; want Aborted after one stop", ops.Status.Phase, api.stops)
	}
	if len(api.commits) != 0 {
		t.Errorf("commits = %v, want none", api.commits)
	}
}

// I-33: an abort after the commit is ignored with RelocationAbortIgnored,
// and the move carries on.
func TestAnAbortAfterTheCommitIsIgnored(t *testing.T) {
	api := relocatingSubsystem()
	r := relocateReconciler(t, api)
	passesUntil(t, r, stepReplicating)
	runPass(t, r)
	api.inSync()
	passesUntil(t, r, stepAwaitingCutover)

	abort(t, r)
	runPass(t, r)

	if ops := operationFrom(t, r); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning {
		t.Errorf("phase = %s, want the move still running", ops.Status.Phase)
	}
	if api.stops != 0 {
		t.Error("a committed move was stopped")
	}
	if !strings.Contains(eventsOf(r), ReasonRelocationAbortIgnored) {
		t.Error("no RelocationAbortIgnored event")
	}
}

// I-35 and I-36: a start the control plane fails with 5xx is retried in the
// same step, and one it refuses with 4xx fails the operation with its reason.
func TestAFailingStartIsRetriedAndARefusedOneFails(t *testing.T) {
	api := relocatingSubsystem()
	api.startErr = &controlplane.StatusError{Op: "start", StatusCode: http.StatusServiceUnavailable}
	r := relocateReconciler(t, api)
	passesUntil(t, r, stepReplicating)
	runPass(t, r)
	if ops := operationFrom(t, r); ops.Status.Step.State != string(stepReplicating) || isTerminal(ops.Status.Phase) {
		t.Fatalf("after a 503: step %s, phase %s; want still Replicating", ops.Status.Step.State, ops.Status.Phase)
	}

	api.startErr = &controlplane.StatusError{Op: "start", StatusCode: http.StatusBadRequest,
		Body: `{"detail":"volume is encrypted"}`}
	runPass(t, r)
	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed ||
		!strings.Contains(ops.Status.Message, "volume is encrypted") {
		t.Errorf("phase %s, message %q; want Failed with the control plane's reason", ops.Status.Phase, ops.Status.Message)
	}
}

// I-38: a handle that does not resolve to the target after the cutover fails
// the operation with HandleUnresolved.
func TestAHandleThatDoesNotResolveToTheTargetFails(t *testing.T) {
	api := relocatingSubsystem()
	r := relocateReconciler(t, api)
	passesUntil(t, r, stepReplicating)
	runPass(t, r)
	api.inSync()
	passesUntil(t, r, stepAwaitingCutover)
	api.cutoverPending(testVolumeID)
	passesUntil(t, r, stepAttachingPaths)
	runPass(t, r)
	finishJobs(t, r, batchv1.JobComplete)
	passesUntil(t, r, stepProceeding)
	api.cutOver(testVolumeID)
	// The relationship names the source as the active volume.
	rel := api.rel[testVolumeID]
	rel.TargetClusterID = testClusterID
	api.rel[testVolumeID] = rel

	ops := passesUntil(t, r, "never")

	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed ||
		!strings.Contains(ops.Status.Message, ReasonHandleUnresolved) {
		t.Errorf("phase %s, message %q; want Failed with HandleUnresolved", ops.Status.Phase, ops.Status.Message)
	}
}

// I-45: a failing path Job holds the step with PathAttachHeld and releases
// nothing.
func TestAFailingPathJobHoldsTheCutover(t *testing.T) {
	api := relocatingSubsystem()
	r := relocateReconciler(t, api)
	passesUntil(t, r, stepReplicating)
	runPass(t, r)
	api.inSync()
	passesUntil(t, r, stepAwaitingCutover)
	api.cutoverPending(testVolumeID)
	passesUntil(t, r, stepAttachingPaths)
	runPass(t, r)
	finishJobs(t, r, batchv1.JobFailed)

	runPass(t, r)
	runPass(t, r)

	if ops := operationFrom(t, r); ops.Status.Step.State != string(stepAttachingPaths) {
		t.Errorf("step = %s, want held at AttachingPaths", ops.Status.Step.State)
	}
	if api.proceeds[testVolumeID] != 0 {
		t.Error("the cutover was released with a node missing the target's paths")
	}
	if !strings.Contains(eventsOf(r), ReasonPathAttachHeld) {
		t.Error("no PathAttachHeld event")
	}
}

// I-71 and I-72: committed is written before the commit is sent, and a pass
// after a restart sends the commit again; the control plane answers with the
// same task, so one task is recorded.
func TestACommitInterruptedBeforeItsTaskIsRecordedIsResent(t *testing.T) {
	api := relocatingSubsystem()
	r := relocateReconciler(t, api)
	passesUntil(t, r, stepReplicating)
	runPass(t, r)
	api.inSync()
	passesUntil(t, r, stepCommitting)
	api.commitErr = errors.New("connection reset")
	runPass(t, r)
	if ops := operationFrom(t, r); ops.Status.Relocate == nil || !ops.Status.Relocate.Committed {
		t.Fatal("committed was not written before the commit was sent")
	}

	api.commitErr = nil
	runPass(t, r)
	runPass(t, r)

	ops := operationFrom(t, r)
	tasks := make([]string, 0, len(ops.Status.Relocate.Volumes))
	for _, v := range ops.Status.Relocate.Volumes {
		tasks = append(tasks, v.TaskID)
	}
	if len(tasks) != 1 || tasks[0] != "task-"+testVolumeID {
		t.Errorf("tasks = %v, want the one task", tasks)
	}
}

// I-74 and I-75: the release is resent while the cutover is pending, and not
// once it has moved on.
func TestTheReleaseIsResentOnlyWhileTheCutoverIsPending(t *testing.T) {
	api := relocatingSubsystem()
	r := relocateReconciler(t, api)
	passesUntil(t, r, stepReplicating)
	runPass(t, r)
	api.inSync()
	passesUntil(t, r, stepAwaitingCutover)
	api.cutoverPending(testVolumeID)
	passesUntil(t, r, stepAttachingPaths)
	runPass(t, r)
	finishJobs(t, r, batchv1.JobComplete)
	passesUntil(t, r, stepProceeding)

	runPass(t, r)
	runPass(t, r)
	if api.proceeds[testVolumeID] != 2 {
		t.Fatalf("proceeds = %d, want it resent while pending", api.proceeds[testVolumeID])
	}
	api.cutOver(testVolumeID)
	runPass(t, r)
	runPass(t, r)
	if api.proceeds[testVolumeID] != 2 {
		t.Errorf("proceeds = %d, want no release past cutover_pending", api.proceeds[testVolumeID])
	}
}

// U-132: an encrypted volume sharing the subsystem fails Validating, and the
// control plane receives no start and no commit.
func TestAnEncryptedVolumeInTheSubsystemFailsValidating(t *testing.T) {
	api := relocatingSubsystem()
	encrypted := claimedVolume(testPVName, relocateClaim)
	encrypted.Spec.CSI.VolumeAttributes = map[string]string{"encryption": "true"}
	objects := relocateWorld()
	objects[1] = encrypted
	api.init()
	r := testReconciler(t, api, objects...)

	ops := passesUntil(t, r, "never")

	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed ||
		!strings.Contains(ops.Status.Message, "encrypted") {
		t.Errorf("phase %s, message %q; want Failed naming the encryption", ops.Status.Phase, ops.Status.Message)
	}
	if len(api.starts) != 0 || len(api.commits) != 0 {
		t.Errorf("starts %v, commits %v; want none", api.starts, api.commits)
	}
}

// abort sets spec.abort.
func abort(t *testing.T, r *PersistentVolumeOpsReconciler) {
	t.Helper()
	ops := operationFrom(t, r)
	ops.Spec.Abort = true
	if err := r.Update(context.Background(), ops); err != nil {
		t.Fatalf("abort: %v", err)
	}
}

// I-39: a Relocate waits on a Migrate of the same volume, through the one
// lock both actions take, and starts nothing while it waits.
func TestARelocateWaitsOnAMigrateOfTheSameVolume(t *testing.T) {
	holder := testOperation()
	holder.Name = "migrate-1"
	holder.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning
	pv := claimedVolume(testPVName, relocateClaim)
	pv.Annotations = map[string]string{simplyblockv1alpha2.PersistentVolumeOpsLock: holder.Name}
	objects := relocateWorld(holder)
	objects[1] = pv
	api := relocatingSubsystem()
	api.init()
	r := testReconciler(t, api, objects...)

	runPass(t, r)
	runPass(t, r)

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhasePending || ops.Status.Step.State != "" {
		t.Errorf("phase %s, step %q; want Pending behind the Migrate", ops.Status.Phase, ops.Status.Step.State)
	}
	if len(api.starts) != 0 {
		t.Errorf("starts = %v, want none while the volume is held", api.starts)
	}
}

// I-79: after the commit the operation awaits the cutover in a step of its
// own, reporting the control plane's progress from the cutover task, and
// attaches no path until the cutover is pending.
func TestTheCutoverIsAwaitedBeforeAnyPathIsAttached(t *testing.T) {
	api := relocatingSubsystem()
	r := relocateReconciler(t, api)
	passesUntil(t, r, stepReplicating)
	runPass(t, r)
	api.inSync()
	ops := passesUntil(t, r, stepAwaitingCutover)
	if ops.Status.Step.State != string(stepAwaitingCutover) {
		t.Fatalf("step = %s, want AwaitingCutover after the commit", ops.Status.Step.State)
	}

	api.tasks["task-"+testVolumeID] = controlplane.Task{ID: "task-" + testVolumeID, Status: "suspended",
		Result: "shrink round 9: waiting for the transfer"}
	runPass(t, r)
	ops = operationFrom(t, r)
	if ops.Status.Step.State != string(stepAwaitingCutover) {
		t.Fatalf("step = %s, want still AwaitingCutover before the cutover is pending", ops.Status.Step.State)
	}
	if !strings.Contains(ops.Status.Message, "shrink round 9: waiting for the transfer") {
		t.Errorf("message = %q, want the cutover task's progress", ops.Status.Message)
	}
	if len(pathJobs(t, r)) != 0 {
		t.Fatal("paths attached before the cutover was pending")
	}

	api.cutoverPending(testVolumeID)
	ops = passesUntil(t, r, stepAttachingPaths)
	runPass(t, r)
	if ops.Status.Step.State != string(stepAttachingPaths) || len(pathJobs(t, r)) != 1 {
		t.Errorf("step %s, %d jobs; want AttachingPaths with a Job once the cutover is pending",
			ops.Status.Step.State, len(pathJobs(t, r)))
	}
}
