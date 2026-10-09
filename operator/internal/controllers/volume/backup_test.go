// Tests for the Backup action of PersistentVolumeOps: the walk from a named
// volume to a finished backup, the two calls that must each be made once, and
// the volumes that are refused.
//
// They run against a fake control plane that counts what it was asked to do,
// because the property under test in most of them is a call that was not
// repeated.

package volume

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testSnapshotID = "77777777-7777-7777-7777-777777777777"
	testBackupID   = "88888888-8888-8888-8888-888888888888"
	testGroupID    = "99999999-9999-9999-9999-999999999999"
)

// testSnapshotName is the name the controller requests the snapshot under,
// which is derived from the operation so that a retry can find it.
var testSnapshotName = "pvops-backup-uid-" + testOpsName

// fakeBackupPlane answers the snapshot and backup calls from fields a test sets.
type fakeBackupPlane struct {
	snapshots []controlplane.Snapshot
	backups   []controlplane.Backup

	snapshotErr error
	backupErr   error

	// status is what BackupByID reports for the backup.
	status string

	snapshotCalls int
	backupCalls   int
	deleted       []string
	backupRequest string
}

func (f *fakeBackupPlane) VolumeSnapshots(
	context.Context, string, string, string,
) ([]controlplane.Snapshot, error) {
	return f.snapshots, nil
}

func (f *fakeBackupPlane) CreateSnapshot(_ context.Context, _, _, _, name string) (string, error) {
	f.snapshotCalls++
	if f.snapshotErr != nil {
		return "", f.snapshotErr
	}
	f.snapshots = append(f.snapshots, controlplane.Snapshot{ID: testSnapshotID, Name: name})
	return testSnapshotID, nil
}

func (f *fakeBackupPlane) DeleteSnapshot(_ context.Context, _, _, snapshotID string) error {
	f.deleted = append(f.deleted, snapshotID)
	return nil
}

func (f *fakeBackupPlane) CreateBackup(_ context.Context, _, snapshotID string) (string, error) {
	f.backupCalls++
	f.backupRequest = snapshotID
	if f.backupErr != nil {
		return "", f.backupErr
	}
	return testBackupID, nil
}

func (f *fakeBackupPlane) ListBackups(context.Context, string) ([]controlplane.Backup, error) {
	return f.backups, nil
}

func (f *fakeBackupPlane) BackupByID(_ context.Context, _, backupID string) (controlplane.Backup, error) {
	return controlplane.Backup{ID: backupID, SnapshotID: testSnapshotID, Status: f.status}, nil
}

func testBackupOperation() *simplyblockv1alpha2.PersistentVolumeOps {
	ops := testOperation()
	ops.Spec.Action = simplyblockv1alpha2.PersistentVolumeOpsActionBackup
	ops.Spec.Migrate = nil
	return ops
}

// testBackupCluster is a cluster that has a backup store, which is what a
// backup needs and a migration does not.
func testBackupCluster() *simplyblockv1alpha2.StorageCluster {
	cluster := testClusterObject()
	cluster.Spec.Backup = &simplyblockv1alpha2.BackupStoreSpec{
		Endpoint:             "https://s3.example.com",
		Bucket:               "backups",
		CredentialsSecretRef: corev1.LocalObjectReference{Name: "s3"},
	}
	return cluster
}

func backupWorld() []client.Object {
	return []client.Object{testBackupOperation(), testVolumeObject(), testBackupCluster()}
}

func backupReconciler(
	t *testing.T, plane *fakeBackupPlane, volume lvol.Volume, objs ...client.Object,
) *PersistentVolumeOpsReconciler {
	t.Helper()
	r := testReconciler(t, &fakeControlPlane{volume: volume}, objs...)
	r.Backups = plane
	return r
}

func plainVolume() lvol.Volume {
	return lvol.Volume{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, testVolumeID), NQN: testNQN}
}

// runToTheEnd drives the operation until it reaches a terminal phase, and fails
// the test if it does not within a bounded number of passes.
func runToTheEnd(t *testing.T, r *PersistentVolumeOpsReconciler) *simplyblockv1alpha2.PersistentVolumeOps {
	t.Helper()
	for pass := 0; pass < 30; pass++ {
		runPass(t, r)
		if ops := operationFrom(t, r); terminal(ops.Status.Phase) {
			return ops
		}
	}
	t.Fatalf("the operation did not finish: %+v", operationFrom(t, r).Status)
	return nil
}

// operationAtStep is a running Backup positioned at one step, holding the lock,
// with whatever the earlier steps would have recorded.
func operationAtStep(
	state simplyblockv1alpha2.PersistentVolumeOpsStep,
	backup simplyblockv1alpha2.VolumeBackupStatus,
) (*simplyblockv1alpha2.PersistentVolumeOps, *corev1.PersistentVolume) {
	ops := testBackupOperation()
	deadline := metav1.NewTime(time.Now().Add(time.Hour))
	ops.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning
	ops.Status.Step = statemachine.KubeSnapshot{State: string(state), Deadline: &deadline}
	backup.ClusterUUID, backup.PoolUUID, backup.VolumeUUID =
		testClusterID, testPoolID, testVolumeID
	ops.Status.Backup = &backup

	pv := testVolumeObject()
	pv.Annotations = map[string]string{simplyblockv1alpha2.PersistentVolumeOpsLock: testOpsName}
	return ops, pv
}

// TestABackupSnapshotsRequestsAndWaitsThenSucceeds. The whole walk, ending with
// the two identifiers recorded and the volume released.
func TestABackupSnapshotsRequestsAndWaitsThenSucceeds(t *testing.T) {
	plane := &fakeBackupPlane{status: "completed"}
	r := backupReconciler(t, plane, plainVolume(), backupWorld()...)

	ops := runToTheEnd(t, r)

	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if ops.Status.Backup == nil ||
		ops.Status.Backup.SnapshotID != testSnapshotID || ops.Status.Backup.BackupID != testBackupID {
		t.Errorf("status.backup = %+v, want the snapshot and the backup recorded", ops.Status.Backup)
	}
	if plane.snapshotCalls != 1 || plane.backupCalls != 1 {
		t.Errorf("snapshots taken = %d, backups requested = %d, want one of each",
			plane.snapshotCalls, plane.backupCalls)
	}
	if plane.backupRequest != testSnapshotID {
		t.Errorf("the backup was requested of snapshot %q, want %q", plane.backupRequest, testSnapshotID)
	}
	if got := lockOn(t, r); got != "" {
		t.Errorf("the volume's lock still names %q after the operation finished", got)
	}
	if !strings.Contains(ops.Status.Message, testBackupID) {
		t.Errorf("message = %q, want it to name the backup", ops.Status.Message)
	}
}

// TestASnapshotAnEarlierAttemptTookIsAdoptedByName. The snapshot is requested
// before its identifier can be written down, so a pass that died in between
// leaves a snapshot nothing names. Asking again would make a second one.
func TestASnapshotAnEarlierAttemptTookIsAdoptedByName(t *testing.T) {
	plane := &fakeBackupPlane{
		status:    "completed",
		snapshots: []controlplane.Snapshot{{ID: testSnapshotID, Name: testSnapshotName}},
	}
	ops, pv := operationAtStep(simplyblockv1alpha2.PersistentVolumeOpsStepSnapshotting,
		simplyblockv1alpha2.VolumeBackupStatus{SnapshotName: testSnapshotName})
	r := backupReconciler(t, plane, plainVolume(), ops, pv, testBackupCluster())

	runPass(t, r)

	if plane.snapshotCalls != 0 {
		t.Errorf("a second snapshot was taken (%d calls) although one of that name existed", plane.snapshotCalls)
	}
	if got := operationFrom(t, r).Status.Backup.SnapshotID; got != testSnapshotID {
		t.Errorf("snapshotID = %q, want the adopted %q", got, testSnapshotID)
	}
}

// TestABackupAnEarlierAttemptRequestedIsAdoptedBySnapshot. The same window, one
// step later: the backup request is made before its identifier is recorded.
func TestABackupAnEarlierAttemptRequestedIsAdoptedBySnapshot(t *testing.T) {
	plane := &fakeBackupPlane{
		status:  "in_progress",
		backups: []controlplane.Backup{{ID: testBackupID, SnapshotID: testSnapshotID}},
	}
	ops, pv := operationAtStep(simplyblockv1alpha2.PersistentVolumeOpsStepBackingUp,
		simplyblockv1alpha2.VolumeBackupStatus{SnapshotName: testSnapshotName, SnapshotID: testSnapshotID})
	r := backupReconciler(t, plane, plainVolume(), ops, pv, testBackupCluster())

	runPass(t, r)

	if plane.backupCalls != 0 {
		t.Errorf("a second backup was requested (%d calls) although one of that snapshot existed", plane.backupCalls)
	}
	if got := operationFrom(t, r).Status.Backup.BackupID; got != testBackupID {
		t.Errorf("backupID = %q, want the adopted %q", got, testBackupID)
	}
}

// TestAVolumeInAConsistencyGroupIsRefused. A group is snapshotted as one frozen
// cut. A backup of one member would be a cut taken at another instant, and the
// restored members would disagree with each other.
func TestAVolumeInAConsistencyGroupIsRefused(t *testing.T) {
	member := plainVolume()
	member.ConsistencyGroup = testGroupID
	plane := &fakeBackupPlane{status: "completed"}
	r := backupReconciler(t, plane, member, backupWorld()...)

	ops := runToTheEnd(t, r)

	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "consistency group") {
		t.Errorf("message = %q, want it to say why", ops.Status.Message)
	}
	if plane.snapshotCalls != 0 {
		t.Errorf("a member of a group was snapshotted alone (%d calls)", plane.snapshotCalls)
	}
	if got := lockOn(t, r); got != "" {
		t.Errorf("the volume's lock still names %q", got)
	}
}

// TestAClusterWithNoBackupStoreIsRefused. Without a store the control plane
// refuses the backup request, but only after a snapshot has been taken, so the
// refusal is made first.
func TestAClusterWithNoBackupStoreIsRefused(t *testing.T) {
	plane := &fakeBackupPlane{status: "completed"}
	r := backupReconciler(t, plane, plainVolume(),
		testBackupOperation(), testVolumeObject(), testClusterObject())

	ops := runToTheEnd(t, r)

	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "backup store") {
		t.Errorf("message = %q, want it to name the missing backup store", ops.Status.Message)
	}
	if plane.snapshotCalls != 0 {
		t.Errorf("a snapshot was taken (%d calls) with nowhere to back it up to", plane.snapshotCalls)
	}
}

// TestARefusedBackupRequestKeepsTheControlPlanesReasonAndDropsTheSnapshot. The
// reason is what a user can act on, and a snapshot left behind by a backup that
// never started is storage nobody asked to keep.
func TestARefusedBackupRequestKeepsTheControlPlanesReasonAndDropsTheSnapshot(t *testing.T) {
	plane := &fakeBackupPlane{
		status: "completed",
		backupErr: &controlplane.StatusError{
			Op: "back up snapshot", StatusCode: 400, Body: "backup is not configured on this cluster"},
	}
	r := backupReconciler(t, plane, plainVolume(), backupWorld()...)

	ops := runToTheEnd(t, r)

	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "backup is not configured") {
		t.Errorf("message = %q, want the control plane's reason", ops.Status.Message)
	}
	if len(plane.deleted) != 1 || plane.deleted[0] != testSnapshotID {
		t.Errorf("deleted snapshots = %v, want [%s]", plane.deleted, testSnapshotID)
	}
}

// TestAnAbortBeforeTheBackupIsRequestedDeletesTheSnapshot.
func TestAnAbortBeforeTheBackupIsRequestedDeletesTheSnapshot(t *testing.T) {
	plane := &fakeBackupPlane{status: "completed"}
	ops, pv := operationAtStep(simplyblockv1alpha2.PersistentVolumeOpsStepSnapshotting,
		simplyblockv1alpha2.VolumeBackupStatus{SnapshotName: testSnapshotName, SnapshotID: testSnapshotID})
	ops.Spec.Abort = true
	r := backupReconciler(t, plane, plainVolume(), ops, pv, testBackupCluster())

	runPass(t, r)

	got := operationFrom(t, r)
	if got.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted {
		t.Fatalf("phase = %q (%s), want Aborted", got.Status.Phase, got.Status.Message)
	}
	if len(plane.deleted) != 1 || plane.deleted[0] != testSnapshotID {
		t.Errorf("deleted snapshots = %v, want [%s]", plane.deleted, testSnapshotID)
	}
}

// TestAnAbortAfterTheBackupIsRequestedIsRefusedAndTheOperationRunsOn. The
// control plane cannot cancel a backup, so stopping here would leave one
// running that nothing accounts for.
func TestAnAbortAfterTheBackupIsRequestedIsRefusedAndTheOperationRunsOn(t *testing.T) {
	plane := &fakeBackupPlane{status: "in_progress"}
	ops, pv := operationAtStep(simplyblockv1alpha2.PersistentVolumeOpsStepAwaitingBackup,
		simplyblockv1alpha2.VolumeBackupStatus{SnapshotID: testSnapshotID, BackupID: testBackupID})
	ops.Spec.Abort = true
	r := backupReconciler(t, plane, plainVolume(), ops, pv, testBackupCluster())

	runPass(t, r)

	got := operationFrom(t, r)
	if terminal(got.Status.Phase) {
		t.Fatalf("phase = %q, want the operation to run on", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "cannot be honored") {
		t.Errorf("message = %q, want it to say the abort came too late", got.Status.Message)
	}
	if len(plane.deleted) != 0 {
		t.Errorf("snapshots deleted = %v, want none: the backup is reading it", plane.deleted)
	}
}

// TestAnAbortTooLateDoesNotStallTheBackup.
//
// Regression: 2026-10-09-backup-late-abort-stall: an abort that arrived after
// the backup was requested was answered with a note and nothing else, so the
// step never ran again and a backup the control plane had finished stayed
// Running until its 24-hour deadline.
func TestAnAbortTooLateDoesNotStallTheBackup(t *testing.T) {
	plane := &fakeBackupPlane{status: "completed"}
	ops, pv := operationAtStep(simplyblockv1alpha2.PersistentVolumeOpsStepAwaitingBackup,
		simplyblockv1alpha2.VolumeBackupStatus{SnapshotID: testSnapshotID, BackupID: testBackupID})
	ops.Spec.Abort = true
	r := backupReconciler(t, plane, plainVolume(), ops, pv, testBackupCluster())

	got := runToTheEnd(t, r)

	if got.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", got.Status.Phase, got.Status.Message)
	}
}

// TestABackupTheControlPlaneGivesUpOnFailsTheOperation.
func TestABackupTheControlPlaneGivesUpOnFailsTheOperation(t *testing.T) {
	plane := &fakeBackupPlane{status: "failed"}
	r := backupReconciler(t, plane, plainVolume(), backupWorld()...)

	ops := runToTheEnd(t, r)

	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if len(plane.deleted) != 0 {
		t.Errorf("snapshots deleted = %v: the backup was requested, so the snapshot is not ours to drop", plane.deleted)
	}
}

// TestABackupWaitsForTheVolumeAnotherOperationHolds.
func TestABackupWaitsForTheVolumeAnotherOperationHolds(t *testing.T) {
	plane := &fakeBackupPlane{status: "completed"}
	holder := testOperation()
	holder.Name = "someone-else"
	holder.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning
	pv := testVolumeObject()
	pv.Annotations = map[string]string{simplyblockv1alpha2.PersistentVolumeOpsLock: holder.Name}
	r := backupReconciler(t, plane, plainVolume(), testBackupOperation(), holder, pv, testBackupCluster())

	runPass(t, r)

	got := operationFrom(t, r)
	if got.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	if plane.snapshotCalls != 0 {
		t.Errorf("a snapshot was taken (%d calls) while another operation held the volume", plane.snapshotCalls)
	}
}

// TestADeletedOperationBeforeTheBackupWasRequestedDropsItsSnapshot.
func TestADeletedOperationBeforeTheBackupWasRequestedDropsItsSnapshot(t *testing.T) {
	plane := &fakeBackupPlane{status: "completed"}
	ops, pv := operationAtStep(simplyblockv1alpha2.PersistentVolumeOpsStepBackingUp,
		simplyblockv1alpha2.VolumeBackupStatus{SnapshotID: testSnapshotID})
	r := backupReconciler(t, plane, plainVolume(), ops, pv, testBackupCluster())
	if err := r.Delete(context.Background(), ops); err != nil {
		t.Fatalf("deleting the operation: %v", err)
	}

	runPass(t, r)

	if len(plane.deleted) != 1 || plane.deleted[0] != testSnapshotID {
		t.Errorf("deleted snapshots = %v, want [%s]", plane.deleted, testSnapshotID)
	}
}
