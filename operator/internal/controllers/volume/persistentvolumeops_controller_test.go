// The operation's spine and its three steps, driven end to end against a fake
// client and a fake control plane.
//
// The assertions worth reading twice are the ones about what the operator does
// when a call is ambiguous. A migration is a data-path operation, so the
// expensive failures here are not crashes: they are an operation that reports
// success and lost writes, or one that reports failure and canceled a copy
// that had already committed. Both of those have happened, and the cases that
// cover them are copy-once, and continue only from pre_created.

package volume

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// runPass drives one reconcile over the test operation.
func runPass(t *testing.T, r *PersistentVolumeOpsReconciler) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: testOpsName}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return result
}

// operationFrom reads the operation back.
func operationFrom(t *testing.T, r *PersistentVolumeOpsReconciler) *simplyblockv1alpha2.PersistentVolumeOps {
	t.Helper()
	var ops simplyblockv1alpha2.PersistentVolumeOps
	if err := r.Get(context.Background(), types.NamespacedName{Name: testOpsName}, &ops); err != nil {
		t.Fatalf("reading the operation: %v", err)
	}
	return &ops
}

// createdMigration is what the control plane answers a creation with: the
// subsystem's migration, its members, and the paths the target now answers on.
func createdMigration() controlplane.Migration {
	return controlplane.Migration{
		Kind:         controlplane.MigrationOfSubsystem,
		ID:           testMigrationID,
		SourceNodeID: testSourceID,
		TargetNodeID: testTargetID,
		Phase:        migrationPhasePreCreated,
		Status:       "running",
		TargetNQN:    testNQN,
		MemberCount:  3,
		Paths: []lvol.Endpoint{
			{Transport: "tcp", Address: "10.0.0.1", Port: 4420, NrIOQueues: 8},
			{Transport: "tcp", Address: "10.0.0.2", Port: 4420, NrIOQueues: 8},
		},
	}
}

// idleSubsystem is a control plane whose subsystem no host consumes, which is
// the shortest path through the graph: nothing to check, so the copy starts.
func idleSubsystem() *fakeControlPlane {
	return &fakeControlPlane{
		volume:  lvol.Volume{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, testVolumeID), NQN: testNQN},
		created: createdMigration(),
		read:    createdMigration(),
	}
}

func testWorld() []client.Object {
	return []client.Object{testOperation(), testVolumeObject(), testClusterObject(), testNodeObject()}
}

// TestAFreshOperationTakesTheLockAndStartsAtValidating. The first pass is the
// one that decides whether the operation runs at all, and the step it records is
// the one the write-ahead record has to name.
func TestAFreshOperationTakesTheLockAndStartsAtValidating(t *testing.T) {
	r := testReconciler(t, idleSubsystem(), testWorld()...)

	runPass(t, r)

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning {
		t.Errorf("phase = %q, want Running", ops.Status.Phase)
	}
	if ops.Status.Step.State != string(stepValidating) {
		t.Errorf("step = %q, want Validating", ops.Status.Step.State)
	}
	if _, bounded := ops.Status.Step.KubeDeadline(); !bounded {
		t.Error("the first step carries no deadline, so it is the one step that cannot time out")
	}
	if got := lockOn(t, r); got != testOpsName {
		t.Errorf("the volume's lock names %q, want the operation", got)
	}
}

// TestTheMigrationIsCreatedOnce. The creation allocates on the backend and
// publishes the paths every host is then checked against, so a second one for
// the same operation is a second migration of the same subsystem — which the
// control plane would either refuse or, worse, accept.
func TestTheMigrationIsCreatedOnce(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api, testWorld()...)

	for range 4 {
		runPass(t, r)
	}

	if api.creates != 1 {
		t.Errorf("the control plane was asked for %d migrations, want exactly 1", api.creates)
	}

	ops := operationFrom(t, r)
	if ops.Status.Migration == nil {
		t.Fatal("nothing about the migration was recorded")
	}
	if got := ops.Status.Migration.MigrationUUID; got != testMigrationID {
		t.Errorf("migration = %q, want the one that was created", got)
	}
	if got := ops.Status.Migration.SubsystemNQN; got != testNQN {
		t.Errorf("subsystem = %q, want the one the volume publishes under", got)
	}
	if got := ops.Status.Migration.MemberCount; got == nil || *got != 3 {
		t.Errorf("member count = %v, want the 3 the subsystem holds", got)
	}
	if n := len(ops.Status.Migration.Connections); n != 2 {
		t.Errorf("connections = %d, want both paths the target published", n)
	}
}

// TestTheRecordedPathsCarryTheDriversLossTimeout. What status.connections shows
// has to be the connect that will actually be made. Every volume path in this
// system is established with the driver's controller-loss timeout, and a
// migration target path becomes the data path at cutover: recording the hour
// the control plane answers with would describe a connect nobody performs.
func TestTheRecordedPathsCarryTheDriversLossTimeout(t *testing.T) {
	api := idleSubsystem()
	// The control plane's own answer, which is deliberately not what is kept.
	hour := 3600
	paths := api.created.Paths
	for i := range paths {
		paths[i].CtrlLossTMOSec = &hour
	}
	api.created.Paths = paths

	r := testReconciler(t, api, testWorld()...)
	runPass(t, r)
	runPass(t, r)

	ops := operationFrom(t, r)
	for _, conn := range ops.Status.Migration.Connections {
		if conn.CtrlLossTimeoutSeconds == nil || *conn.CtrlLossTimeoutSeconds != migrationCtrlLossTimeout {
			t.Errorf("ctrl-loss-tmo = %v, want the driver's %d",
				conn.CtrlLossTimeoutSeconds, migrationCtrlLossTimeout)
		}
	}
}

// TestASubsystemNobodyConsumesSkipsStraightToTheCopy. There are no host paths
// to check when no pod has any of the subsystem's volumes mounted, and starting
// Jobs to check nothing would only spend the step's deadline.
func TestASubsystemNobodyConsumesSkipsStraightToTheCopy(t *testing.T) {
	r := testReconciler(t, idleSubsystem(), testWorld()...)

	for range 4 {
		runPass(t, r)
	}

	if got := operationFrom(t, r).Status.Step.State; got != string(stepMigrating) {
		t.Errorf("step = %q, want Migrating", got)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("%d Jobs were started for a subsystem nobody consumes", len(jobs.Items))
	}
}

// TestEveryConsumingHostIsCheckedBeforeTheCutover. A migration moves the whole
// subsystem, so every sibling volume moves with the named one. Checking only
// the named volume's host leaves every sibling's consumer pointing at the
// source, and at cutover those hosts lose their volume.
func TestEveryConsumingHostIsCheckedBeforeTheCutover(t *testing.T) {
	const siblingVolume = "77777777-7777-7777-7777-777777777777"

	api := idleSubsystem()
	api.members = []lvol.Volume{
		{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, testVolumeID), NQN: testNQN},
		{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, siblingVolume), NQN: testNQN},
	}

	// The named volume is replaced by a claimed one, because a consumer is
	// found through the claim the volume names.
	r := testReconciler(t, api,
		testOperation(), testClusterObject(), testNodeObject(),
		claimedVolume(testPVName, "app", "data-0"),
		claimedVolume("pvc-"+siblingVolume, "app", "data-1"),
		runningPodOn("worker-1", "app", "data-0"),
		runningPodOn("worker-2", "app", "data-1"),
	)

	for range 3 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	nodes := map[string]bool{}
	for _, job := range ops.Status.Migration.ValidationJobs {
		nodes[job.Node] = true
	}
	if !nodes["worker-1"] || !nodes["worker-2"] {
		t.Errorf("checked %v, want both hosts that consume the subsystem", nodes)
	}
}

// TestTheCopyIsContinuedOnlyFromPreCreated. Continue is not idempotent: it
// accepts a migration in pre_created and rejects any later call. A pass that
// continued the copy and crashed before recording it must not have that copy
// canceled by the pass that follows.
func TestTheCopyIsContinuedOnlyFromPreCreated(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api, testWorld()...)

	// Reach the copy.
	for range 4 {
		runPass(t, r)
	}
	if got := operationFrom(t, r).Status.Step.State; got != string(stepMigrating) {
		t.Fatalf("step = %q, want Migrating", got)
	}

	// The first pass in the step continues it; the control plane then reports
	// a migration that has advanced.
	runPass(t, r)
	api.read = controlplane.Migration{ID: testMigrationID, Phase: "migrating", Status: "running"}
	runPass(t, r)
	runPass(t, r)

	if api.continues != 1 {
		t.Errorf("the copy was continued %d times, want exactly 1", api.continues)
	}
	if api.cancels != 0 {
		t.Errorf("a running copy was canceled %d times", api.cancels)
	}
}

// TestTheCopyFinishesOnTheReportedStateRatherThanTheCall. A five-second read
// timeout on a continue that took slightly longer once made the operator retry
// a transfer that had already committed, and the retry copied nothing while the
// source was unfrozen. The step therefore finishes on what the migration says
// about itself.
func TestTheCopyFinishesOnTheReportedStateRatherThanTheCall(t *testing.T) {
	api := idleSubsystem()
	api.continueErr = errors.New("read timeout after 5s")
	r := testReconciler(t, api, testWorld()...)

	for range 4 {
		runPass(t, r)
	}

	// The continue fails, so the operation stays where it is rather than
	// failing: the copy may well have started.
	runPass(t, r)
	if ops := operationFrom(t, r); terminal(ops.Status.Phase) {
		t.Fatalf("a failed continue ended the operation at %q", ops.Status.Phase)
	}

	// The migration itself then reports the copy finished, which is the
	// statement that counts.
	api.read = controlplane.Migration{ID: testMigrationID, Phase: "done", Status: migrationStatusDone}
	runPass(t, r)

	if got := operationFrom(t, r).Status.Step.State; got != string(stepVerifying) {
		t.Errorf("step = %q, want Verifying: the copy reported itself finished", got)
	}
}

// TestAFinishedMigrationSucceedsAndReleasesTheVolume. The lock has to come off
// on every terminal path, or the next migration of that volume waits on an
// operation that has finished.
func TestAFinishedMigrationSucceedsAndReleasesTheVolume(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api, testWorld()...)

	api.read = controlplane.Migration{ID: testMigrationID, Phase: "done", Status: migrationStatusDone}
	for range 8 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if ops.Status.CompletedAt == nil {
		t.Error("a terminal operation records no completion time")
	}
	if got := lockOn(t, r); got != "" {
		t.Errorf("the volume is still locked by %q", got)
	}
}

// TestAnAbortBeforeTheCutoverTakesTheMigrationBack. Stopping a migration that
// has not cut over means canceling the backend migration: leaving it would
// block the subsystem's next one.
func TestAnAbortBeforeTheCutoverTakesTheMigrationBack(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api, testWorld()...)

	runPass(t, r)
	runPass(t, r)

	ops := operationFrom(t, r)
	ops.Spec.Abort = true
	if err := r.Update(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
	runPass(t, r)

	ops = operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted {
		t.Fatalf("phase = %q, want Aborted", ops.Status.Phase)
	}
	if api.cancels != 1 {
		t.Errorf("the backend migration was canceled %d times, want 1", api.cancels)
	}
	if got := lockOn(t, r); got != "" {
		t.Errorf("the volume is still locked by %q", got)
	}
}

// TestAnAbortAfterTheCutoverIsRefusedAndTheOperationRunsOn. By Verifying the
// volume has moved and the cleanup is what makes the move safe. Honoring an
// abort there would leave the system holding exactly the state the step exists
// to prevent — and the DELETE guard reads the same graph, so a deletion cannot
// express it either.
func TestAnAbortAfterTheCutoverIsRefusedAndTheOperationRunsOn(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api, testWorld()...)

	ops := operationFrom(t, r)
	ops.Spec.Abort = true
	if err := r.Update(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
	if err := atStep(r, stepVerifying); err != nil {
		t.Fatal(err)
	}

	runPass(t, r)

	ops = operationFrom(t, r)
	if terminal(ops.Status.Phase) {
		t.Fatalf("the abort was honored at Verifying: phase = %q", ops.Status.Phase)
	}
	if api.cancels != 0 {
		t.Errorf("a migration that had already cut over was canceled %d times", api.cancels)
	}
	if ops.Status.Message == "" {
		t.Error("the refusal is not reported anywhere the user can read it")
	}
}

// TestAVolumeThatWentAwayEndsTheOperationAsAborted. A claim deleted under a
// Delete reclaim policy makes the driver delete the backing logical volume, and
// moving a volume that is being deleted is work nobody will read. Aborted
// rather than Failed, because a migration whose volume went away did not go
// wrong.
func TestAVolumeThatWentAwayEndsTheOperationAsAborted(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api, testWorld()...)

	runPass(t, r)
	runPass(t, r)

	pv := volumeFrom(t, r)
	if err := r.Delete(context.Background(), pv); err != nil {
		t.Fatal(err)
	}
	runPass(t, r)

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted {
		t.Fatalf("phase = %q (%s), want Aborted", ops.Status.Phase, ops.Status.Message)
	}
	// Cancel before the object goes: a logical volume with a migration running
	// against it is not one the control plane can cleanly delete.
	if api.cancels != 1 {
		t.Errorf("the backend migration was canceled %d times, want 1", api.cancels)
	}
}

// TestDeletingAnOperationUnwindsBeforeTheObjectGoes. An operation removed
// mid-flight would otherwise leave a backend migration running and the paths it
// published connected, with the only record naming them gone.
func TestDeletingAnOperationUnwindsBeforeTheObjectGoes(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api, testWorld()...)

	runPass(t, r)
	runPass(t, r)

	if err := r.Delete(context.Background(), operationFrom(t, r)); err != nil {
		t.Fatal(err)
	}
	runPass(t, r)

	if api.cancels != 1 {
		t.Errorf("the backend migration was canceled %d times, want 1", api.cancels)
	}
	var ops simplyblockv1alpha2.PersistentVolumeOps
	err := r.Get(context.Background(), types.NamespacedName{Name: testOpsName}, &ops)
	if err == nil {
		t.Errorf("the operation still exists with finalizers %v", ops.Finalizers)
	}
	if got := lockOn(t, r); got != "" {
		t.Errorf("the volume is still locked by %q", got)
	}
}

// TestADeleteIsHeldWhileTheCleanupCannotFinish. Leaving a visibly stuck object
// is the intended outcome: a path connected with nothing tracking it blocks
// every later migration of the volume, and releasing the finalizer would lose
// the only record naming it.
func TestADeleteIsHeldWhileTheCleanupCannotFinish(t *testing.T) {
	api := idleSubsystem()
	api.cancelErr = errors.New("the control plane is unreachable")
	r := testReconciler(t, api, testWorld()...)

	runPass(t, r)
	runPass(t, r)

	if err := r.Delete(context.Background(), operationFrom(t, r)); err != nil {
		t.Fatal(err)
	}
	runPass(t, r)

	ops := operationFrom(t, r)
	if len(ops.Finalizers) == 0 {
		t.Fatal("the finalizer was released while the cleanup had not finished")
	}
	if ops.Status.Message == "" {
		t.Error("nothing says why the delete is held")
	}
}

// TestAStepThatOutlivesItsDeadlineFailsTheOperation. Everything that waits on a
// migration waits for a terminal phase, so a step that could not time out would
// stall those flows silently rather than reporting something they can act on.
func TestAStepThatOutlivesItsDeadlineFailsTheOperation(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api, testWorld()...)

	runPass(t, r)

	// Wind the step's deadline into the past, which is the only part of a
	// timeout a unit test can reach.
	ops := operationFrom(t, r)
	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	ops.Status.Step = statemachine.KubeSnapshot{
		State:    string(stepValidating),
		Deadline: &expired,
	}
	if err := r.Status().Update(context.Background(), ops); err != nil {
		t.Fatal(err)
	}

	runPass(t, r)

	ops = operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if got := lockOn(t, r); got != "" {
		t.Errorf("the volume is still locked by %q", got)
	}
}

// TestAVolumeOfAnotherDriverIsRefusedRatherThanRetried. It is a well-formed
// request against the wrong object, and no reconcile will ever make it work.
func TestAVolumeOfAnotherDriverIsRefusedRatherThanRetried(t *testing.T) {
	foreign := testVolumeObject()
	foreign.Spec.CSI.Driver = "ebs.csi.aws.com"

	r := testReconciler(t, idleSubsystem(),
		testOperation(), foreign, testClusterObject(), testNodeObject())

	runPass(t, r)

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
}

// TestAQueuedOperationWaitsAtPendingAndSaysWhy. A second operation for a locked
// volume is admitted, acquires nothing, and waits — which is what makes a
// drain's fan-out proceed in turn rather than half of it failing.
func TestAQueuedOperationWaitsAtPendingAndSaysWhy(t *testing.T) {
	holder := testOperation()
	holder.Name = testHolderName
	holder.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning

	pv := testVolumeObject()
	pv.Annotations = map[string]string{
		simplyblockv1alpha2.PersistentVolumeOpsLock: holder.Name,
	}
	r := testReconciler(t, idleSubsystem(),
		holder, testOperation(), pv, testClusterObject(), testNodeObject())

	result := runPass(t, r)

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhasePending {
		t.Errorf("phase = %q, want Pending", ops.Status.Phase)
	}
	if ops.Status.DeferredSince == nil {
		t.Error("nothing records since when the operation has been waiting")
	}
	if result.RequeueAfter == 0 {
		t.Error("a queued operation asked for no requeue, so nothing would look at it again")
	}
}

// atStep puts the operation at a step without driving it there, for the cases
// whose interesting behavior is at the far end of the graph.
func atStep(r *PersistentVolumeOpsReconciler, at step) error {
	ctx := context.Background()
	var ops simplyblockv1alpha2.PersistentVolumeOps
	if err := r.Get(ctx, types.NamespacedName{Name: testOpsName}, &ops); err != nil {
		return err
	}
	deadline := metav1.NewTime(time.Now().Add(time.Hour))
	ops.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning
	ops.Status.Step = statemachine.KubeSnapshot{State: string(at), Deadline: &deadline}
	ops.Status.Migration = &simplyblockv1alpha2.MigrationStatus{
		MigrationUUID: testMigrationID,
		ClusterUUID:   testClusterID,
		PoolUUID:      testPoolID,
		VolumeUUID:    testVolumeID,
		SubsystemNQN:  testNQN,
	}
	return r.Status().Update(ctx, &ops)
}

// claimedVolume is a PersistentVolume of this driver bound to a claim, which is
// how a consumer is found: the volume names the claim and a pod mounts it.
func claimedVolume(name, namespace, claim string) *corev1.PersistentVolume {
	pv := testVolumeObject()
	pv.Name = name
	if name != testPVName {
		pv.Spec.CSI.VolumeHandle = string(lvol.NewVolumeHandle(
			testClusterID, testPoolID, name[len("pvc-"):]))
	}
	pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: namespace, Name: claim}
	return pv
}

func runningPodOn(node, namespace, claim string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-" + claim, Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeName: node,
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: claim,
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}
