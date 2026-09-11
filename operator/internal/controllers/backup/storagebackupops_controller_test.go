// Tests for StorageBackupOps: the Restore graph, the lock it takes on its
// target, and the refusal that stands between a restore and overwriting a
// running workload's data.
//
// That refusal is the most important assertion in this package. Everything else
// here is about an operation failing to do something; that one is about an
// operation succeeding at the wrong thing, and the damage is somebody's live
// data.

package backup

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The step values live in three places: the graph declares them, the kind's
// Enum marker constrains the step type, and a CEL rule constrains the stored
// string. Nothing but this test makes the three agree, and every Ops kind owes
// it (design-crd-model.md §3.1).
func TestDeclaredStepsMatchTheKindsEnum(t *testing.T) {
	declared := statemachine.DeclaredMultiStates(restoreGraph())

	// The Enum marker's values, which are also the CEL rule's list. Written out
	// rather than read from the type, because writing them out is the point: the
	// marker is a comment the compiler does not check.
	marker := []string{"AwaitingVolume", "Binding", "Restoring", "Validating"}

	if !slices.Equal(declared, marker) {
		t.Errorf("the graph declares %v and the kind's Enum marker lists %v", declared, marker)
	}
}

// Every step an abort is honored from has to be one the graph declares, or the
// table and the graph would disagree about what the action can do.
func TestAbortableStepsAreDeclaredByTheGraph(t *testing.T) {
	declared := statemachine.DeclaredMultiStates(restoreGraph())
	for abortableStep := range abortableSteps {
		if !slices.Contains(declared, string(abortableStep)) {
			t.Errorf("abortableSteps names %q, which the graph does not declare", abortableStep)
		}
	}
	// The two the design fixes as the point of no return. A step that gained an
	// abort edge without the graph gaining a way to unwind it would let an
	// operation stop with a volume nothing accounts for.
	for _, beyondReturn := range []step{stepAwaitingVolume, stepBinding} {
		if abortable(beyondReturn) {
			t.Errorf("step %q is abortable, and it has already created a logical volume", beyondReturn)
		}
	}
}

// testOpsName is the operation every test in this file drives, named once
// because it is also what the restored-by label and the volume name carry.
const testOpsName = "restore-1"

func restoreOps(name string) *simplyblockv1alpha2.StorageBackupOps {
	return &simplyblockv1alpha2.StorageBackupOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  testNamespace,
			UID:        types.UID("uid-" + name),
			Finalizers: []string{opsFinalizer},
		},
		Spec: simplyblockv1alpha2.StorageBackupOpsSpec{
			ClusterRef: testClusterCR,
			BackupRef:  "backup-1",
			Action:     simplyblockv1alpha2.StorageBackupOpsActionRestore,
			Restore: &simplyblockv1alpha2.RestoreSpec{
				ClaimName:  "restored-claim",
				TargetPool: testPoolCR,
			},
		},
	}
}

func availableBackup() *simplyblockv1alpha2.StorageBackup {
	return &simplyblockv1alpha2.StorageBackup{
		ObjectMeta: objectMeta("backup-1"),
		Spec: simplyblockv1alpha2.StorageBackupSpec{
			ClusterRef: testClusterCR, BackupID: testBackupID,
		},
		Status: simplyblockv1alpha2.StorageBackupStatus{
			Phase:     simplyblockv1alpha2.StorageBackupPhaseAvailable,
			ClusterID: testClusterID,
			Backup:    &simplyblockv1alpha2.BackupCopy{BackupID: testBackupID, Size: ptr.To(int64(1 << 30))},
			Source:    &simplyblockv1alpha2.BackupSource{FSType: "xfs", ClaimName: "claim-1"},
		},
	}
}

func opsReconciler(
	t *testing.T, api RestoreClient, objs ...client.Object,
) *StorageBackupOpsReconciler {
	t.Helper()
	return &StorageBackupOpsReconciler{
		Client:   testClient(t, objs...),
		Scheme:   testScheme(t),
		Recorder: testRecorder(),
		API:      api,
	}
}

func opsRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name}}
}

// reconcileUntilSettled runs the reconciler until it stops asking to be run
// again, or until the budget runs out. It exists because one reconcile advances
// at most one step, deliberately, so driving a whole restore means driving the
// loop the manager would.
func reconcileUntilSettled(
	t *testing.T, r *StorageBackupOpsReconciler,
) *simplyblockv1alpha2.StorageBackupOps {
	// One reconcile advances at most one step, deliberately, so a whole restore
	// takes a handful. The bound is what turns a machine that stopped moving
	// into a failed test rather than a hang.
	const budget = 10

	name := testOpsName
	t.Helper()
	for range budget {
		result, err := r.Reconcile(context.Background(), opsRequest(name))
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		var ops simplyblockv1alpha2.StorageBackupOps
		if err := r.Get(context.Background(), opsRequest(name).NamespacedName, &ops); err != nil {
			t.Fatalf("read the operation back: %v", err)
		}
		if terminal(ops.Status.Phase) {
			return &ops
		}
		if result.RequeueAfter == 0 {
			return &ops
		}
	}
	t.Fatalf("the operation did not settle within %d reconciles", budget)
	return nil
}

// The refusal. A claim that already exists belongs to somebody, and a restore
// that adopted it would replace that workload's data with the backup's.
func TestRestoreRefusesAClaimThatAlreadyExists(t *testing.T) {
	occupied := &corev1.PersistentVolumeClaim{ObjectMeta: objectMeta("restored-claim")}
	api := &fakeControlPlane{restoredID: testRestoreID}
	r := opsReconciler(t, api,
		testClusterObject(), testPoolObject(), availableBackup(), restoreOps(testOpsName), occupied)

	ops := reconcileUntilSettled(t, r)

	if ops.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "already exists") {
		t.Errorf("message = %q, want it to say the claim already exists", ops.Status.Message)
	}
	if api.restores != 0 {
		t.Errorf("the control plane was asked for %d restore(s) despite the refusal", api.restores)
	}
}

// The whole graph, driven end to end: Validating resolves, Restoring asks,
// AwaitingVolume waits, and Binding produces the claim.
func TestRestoreRunsTheWholeGraphAndProducesAClaim(t *testing.T) {
	handle := lvol.NewVolumeHandle(testClusterID, testPoolID, testRestoreID)
	api := &fakeControlPlane{
		restoredID: testRestoreID,
		volumes:    map[string]lvol.Volume{string(handle): {Status: cpVolumeOnline}},
		connection: lvol.Connection{
			NQN:       "nqn.2023-01.io.simplyblock:lvol",
			Endpoints: []lvol.Endpoint{{Transport: "tcp", Address: "10.0.0.1", Port: 4420}},
		},
	}
	r := opsReconciler(t, api,
		testClusterObject(), testPoolObject(), availableBackup(), restoreOps(testOpsName))

	// The claim only binds once something binds it, which in a real cluster is
	// the volume controller. Driving to Binding and then binding the claim is
	// what stands in for that here.
	for range 6 {
		if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		var claim corev1.PersistentVolumeClaim
		if err := r.Get(context.Background(),
			client.ObjectKey{Name: "restored-claim", Namespace: testNamespace}, &claim); err == nil {
			claim.Status.Phase = corev1.ClaimBound
			if err := r.Status().Update(context.Background(), &claim); err != nil {
				t.Fatalf("bind the claim: %v", err)
			}
			break
		}
	}

	ops := reconcileUntilSettled(t, r)

	if ops.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if api.restores != 1 {
		t.Errorf("the control plane was asked for %d restores, want exactly 1", api.restores)
	}

	// The claim outlives the operation, so it carries no owner reference back to
	// it: deleting the audit record must not delete the recovered volume.
	var claim corev1.PersistentVolumeClaim
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "restored-claim", Namespace: testNamespace}, &claim); err != nil {
		t.Fatalf("the restore produced no claim: %v", err)
	}
	if len(claim.OwnerReferences) != 0 {
		t.Errorf("the claim carries %d owner reference(s), and must carry none",
			len(claim.OwnerReferences))
	}
	if got := claim.Labels[RestoredByLabel]; got != testOpsName {
		t.Errorf("restored-by label = %q, want restore-1", got)
	}

	// The filesystem the copy was taken with, so the restored volume mounts the
	// way it was backed up rather than with the driver's default.
	var volume corev1.PersistentVolume
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "restore-uid-restore-1"}, &volume); err != nil {
		t.Fatalf("the restore produced no volume: %v", err)
	}
	if got := volume.Spec.CSI.FSType; got != "xfs" {
		t.Errorf("volume fsType = %q, want the source's xfs", got)
	}
}

// One operation at a time per backup. The second is admitted, acquires nothing,
// and waits: queueing is the default rather than a feature anything built.
func TestASecondRestoreOfOneBackupWaitsRatherThanRunning(t *testing.T) {
	api := &fakeControlPlane{restoredID: testRestoreID}
	first, second := restoreOps(testOpsName), restoreOps("restore-2")
	second.Spec.Restore.ClaimName = "another-claim"

	r := opsReconciler(t, api, testClusterObject(), testPoolObject(), availableBackup(), first, second)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatalf("Reconcile the first: %v", err)
	}
	var backup simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "backup-1", Namespace: testNamespace}, &backup); err != nil {
		t.Fatal(err)
	}
	if got := backup.Status.ActiveOpsRef; got != testOpsName {
		t.Fatalf("activeOpsRef = %q, want restore-1 to hold the lock", got)
	}

	if _, err := r.Reconcile(context.Background(), opsRequest("restore-2")); err != nil {
		t.Fatalf("Reconcile the second: %v", err)
	}
	var waiting simplyblockv1alpha2.StorageBackupOps
	if err := r.Get(context.Background(), opsRequest("restore-2").NamespacedName, &waiting); err != nil {
		t.Fatal(err)
	}
	if waiting.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhasePending {
		t.Errorf("the queued operation is %q, want Pending", waiting.Status.Phase)
	}
	if waiting.Status.Step.State != "" {
		t.Errorf("the queued operation entered step %q while another holds the lock",
			waiting.Status.Step.State)
	}
	if api.restores != 0 {
		t.Errorf("a queued operation issued %d restore(s)", api.restores)
	}
}

// Release checks ownership, so a late release from an operation that no longer
// holds the lock cannot steal it from whoever holds it now.
func TestReleasingTheLockChecksOwnershipFirst(t *testing.T) {
	held := availableBackup()
	held.Status.ActiveOpsRef = "somebody-else"

	stale := restoreOps(testOpsName)
	stale.Status.Phase = simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded

	r := opsReconciler(t, &fakeControlPlane{}, testClusterObject(), testPoolObject(), held, stale)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var backup simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "backup-1", Namespace: testNamespace}, &backup); err != nil {
		t.Fatal(err)
	}
	if got := backup.Status.ActiveOpsRef; got != "somebody-else" {
		t.Errorf("activeOpsRef = %q: a terminal operation released a lock it did not hold", got)
	}
}

// An abort arriving at a step the graph declares no way out of is reported and
// the operation runs on, rather than stopping with a volume nothing accounts for.
func TestAbortIsRefusedOnceAVolumeExists(t *testing.T) {
	running := restoreOps(testOpsName)
	running.Spec.Abort = true
	running.Status = simplyblockv1alpha2.StorageBackupOpsStatus{
		Phase:          simplyblockv1alpha2.StorageBackupOpsPhaseRunning,
		ClusterID:      testClusterID,
		PoolUUID:       testPoolID,
		BackupID:       testBackupID,
		RestoredLvolID: testRestoreID,
		Step:           statemachine.KubeSnapshot{State: string(stepAwaitingVolume)},
	}
	held := availableBackup()
	held.Status.ActiveOpsRef = testOpsName

	r := opsReconciler(t, &fakeControlPlane{}, testClusterObject(), testPoolObject(), held, running)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var ops simplyblockv1alpha2.StorageBackupOps
	if err := r.Get(context.Background(), opsRequest(testOpsName).NamespacedName, &ops); err != nil {
		t.Fatal(err)
	}
	if ops.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseRunning {
		t.Errorf("phase = %q, want the operation to run on", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "cannot be undone") {
		t.Errorf("message = %q, want it to say the abort arrived too late", ops.Status.Message)
	}
}

// An abort from a step that has created nothing is honored, and Aborted is
// terminal but distinct from Failed: the operation stopped without going wrong.
func TestAbortIsHonoredBeforeAnythingIsCreated(t *testing.T) {
	running := restoreOps(testOpsName)
	running.Spec.Abort = true
	running.Status = simplyblockv1alpha2.StorageBackupOpsStatus{
		Phase: simplyblockv1alpha2.StorageBackupOpsPhaseRunning,
		Step:  statemachine.KubeSnapshot{State: string(stepValidating)},
	}
	held := availableBackup()
	held.Status.ActiveOpsRef = testOpsName

	api := &fakeControlPlane{restoredID: testRestoreID}
	r := opsReconciler(t, api, testClusterObject(), testPoolObject(), held, running)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var ops simplyblockv1alpha2.StorageBackupOps
	if err := r.Get(context.Background(), opsRequest(testOpsName).NamespacedName, &ops); err != nil {
		t.Fatal(err)
	}
	if ops.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseAborted {
		t.Errorf("phase = %q, want Aborted", ops.Status.Phase)
	}

	// The lock goes with the operation, or the backup would be held by something
	// that has stopped.
	var backup simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "backup-1", Namespace: testNamespace}, &backup); err != nil {
		t.Fatal(err)
	}
	if backup.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q after an abort, want it released", backup.Status.ActiveOpsRef)
	}
}

// A backup that failed has no copy behind it, and it does not recover.
func TestRestoreFailsAgainstAFailedBackup(t *testing.T) {
	broken := availableBackup()
	broken.Status.Phase = simplyblockv1alpha2.StorageBackupPhaseFailed

	r := opsReconciler(t, &fakeControlPlane{},
		testClusterObject(), testPoolObject(), broken, restoreOps(testOpsName))

	ops := reconcileUntilSettled(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed against a backup with no copy", ops.Status.Phase)
	}
}

// A restore is never asked for twice. The second request would produce a second
// volume nothing accounts for, so the recorded volume is what guards it.
func TestTheRestoreRequestIsIssuedOnlyOnce(t *testing.T) {
	handle := lvol.NewVolumeHandle(testClusterID, testPoolID, testRestoreID)
	api := &fakeControlPlane{
		restoredID: testRestoreID,
		// The volume never comes online, so the operation sits on AwaitingVolume
		// and every further reconcile passes back through the guard.
		volumes: map[string]lvol.Volume{string(handle): {Status: "in_creation"}},
	}
	r := opsReconciler(t, api,
		testClusterObject(), testPoolObject(), availableBackup(), restoreOps(testOpsName))

	for range 6 {
		if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	if api.restores != 1 {
		t.Errorf("the control plane was asked for %d restores, want exactly 1", api.restores)
	}
}
