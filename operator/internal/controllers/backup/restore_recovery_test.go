// Tests for what a restore does when a previous pass did not finish.
//
// Every case here is the same shape: the control plane was asked for a volume
// and the operation could not write down what it got. That window is narrow and
// it is not theoretical — the request is made before the identifier can be
// persisted, so any crash, conflict, or eviction between the two lands in it.
// What separates a correct restore from a storage leak is entirely what happens
// on the next pass.

package backup

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// restoringOps is an operation parked on Restoring with the pool resolved and
// nothing recorded, which is exactly the state a pass that died after asking for
// the restore leaves behind.
func restoringOps() *simplyblockv1alpha2.StorageBackupOps {
	ops := restoreOps(testOpsName)
	ops.Status = simplyblockv1alpha2.StorageBackupOpsStatus{
		Phase:     simplyblockv1alpha2.StorageBackupOpsPhaseRunning,
		ClusterID: testClusterID,
		PoolUUID:  testPoolID,
		BackupID:  testBackupID,
		Step:      statemachine.KubeSnapshot{State: string(stepRestoring)},
	}
	return ops
}

// orphanedVolume is the volume such a pass left in the pool, findable only by
// the deterministic name the restore was asked for.
func orphanedVolume() lvol.Volume {
	return lvol.Volume{
		ID:   lvol.NewVolumeHandle(testClusterID, testPoolID, testRestoreID),
		Name: "restore-uid-" + testOpsName,
	}
}

func heldBackup() *simplyblockv1alpha2.StorageBackup {
	backup := availableBackup()
	backup.Status.ActiveOpsRef = testOpsName
	return backup
}

// The restore request is never issued twice. The control plane creates a volume
// on every call and cannot be asked twice for the same one, so a retry that
// could not tell whether the first call landed would double the storage the
// operation occupies and leave half of it unreferenced.
func TestARestoreThatWasAcceptedButNotRecordedIsAdoptedRatherThanReissued(t *testing.T) {
	api := &fakeControlPlane{
		restoredID: "a-second-volume-nobody-wants",
		pool:       []lvol.Volume{orphanedVolume()},
	}
	r := opsReconciler(t, api,
		testClusterObject(), testPoolObject(), heldBackup(), restoringOps())

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if api.restores != 0 {
		t.Errorf("the control plane was asked for %d restore(s); the volume was already there",
			api.restores)
	}

	var ops simplyblockv1alpha2.StorageBackupOps
	if err := r.Get(context.Background(), opsRequest(testOpsName).NamespacedName, &ops); err != nil {
		t.Fatal(err)
	}
	if got := ops.Status.RestoredLvolID; got != testRestoreID {
		t.Errorf("restoredLvolID = %q, want the volume that was already restored (%s)",
			got, testRestoreID)
	}
}

// A pool with no volume of this operation's name means nothing was created, so
// the restore is asked for normally.
func TestARestoreWithNothingToAdoptIsIssuedNormally(t *testing.T) {
	api := &fakeControlPlane{restoredID: testRestoreID}
	r := opsReconciler(t, api,
		testClusterObject(), testPoolObject(), heldBackup(), restoringOps())

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if api.restores != 1 {
		t.Errorf("the control plane was asked for %d restores, want exactly 1", api.restores)
	}
}

// Aborting from Restoring has something to take back. Reaching Aborted with a
// volume still filling would leave a copy being written into storage that
// nothing will ever claim, read, or remove.
func TestAbortingARestoreDiscardsTheVolumeItCreated(t *testing.T) {
	api := &fakeControlPlane{}
	aborting := restoringOps()
	aborting.Spec.Abort = true
	aborting.Status.RestoredLvolID = testRestoreID

	r := opsReconciler(t, api, testClusterObject(), testPoolObject(), heldBackup(), aborting)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	want := string(lvol.NewVolumeHandle(testClusterID, testPoolID, testRestoreID))
	if len(api.deleted) != 1 || api.deleted[0] != want {
		t.Errorf("deleted %v, want the restored volume %s", api.deleted, want)
	}

	var ops simplyblockv1alpha2.StorageBackupOps
	if err := r.Get(context.Background(), opsRequest(testOpsName).NamespacedName, &ops); err != nil {
		t.Fatal(err)
	}
	if ops.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseAborted {
		t.Errorf("phase = %q, want Aborted", ops.Status.Phase)
	}
}

// The same abort, from the same step, where the identifier was never written
// down. The volume is found by name rather than left behind.
func TestAbortingARestoreDiscardsAVolumeItNeverRecorded(t *testing.T) {
	api := &fakeControlPlane{pool: []lvol.Volume{orphanedVolume()}}
	aborting := restoringOps()
	aborting.Spec.Abort = true

	r := opsReconciler(t, api, testClusterObject(), testPoolObject(), heldBackup(), aborting)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(api.deleted) != 1 {
		t.Errorf("deleted %v, want the volume the operation had asked for", api.deleted)
	}
}

// An abort whose cleanup fails does not reach Aborted. Ending the operation
// there is precisely what leaks the volume, so it stays where it is and the
// abort is honored on a later pass.
func TestAnAbortWaitsWhenTheVolumeCannotBeDiscarded(t *testing.T) {
	api := &fakeControlPlane{deleteErr: errRefused{}}
	aborting := restoringOps()
	aborting.Spec.Abort = true
	aborting.Status.RestoredLvolID = testRestoreID

	r := opsReconciler(t, api, testClusterObject(), testPoolObject(), heldBackup(), aborting)

	result, err := r.Reconcile(context.Background(), opsRequest(testOpsName))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("the abort neither finished nor asked to be looked at again")
	}

	var ops simplyblockv1alpha2.StorageBackupOps
	if err := r.Get(context.Background(), opsRequest(testOpsName).NamespacedName, &ops); err != nil {
		t.Fatal(err)
	}
	if terminal(ops.Status.Phase) {
		t.Errorf("phase = %q: the operation ended while its volume was still there", ops.Status.Phase)
	}
	// The lock is still held, so nothing else can restore this backup while the
	// volume is unaccounted for.
	var backup simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "backup-1", Namespace: testNamespace}, &backup); err != nil {
		t.Fatal(err)
	}
	if backup.Status.ActiveOpsRef != testOpsName {
		t.Errorf("activeOpsRef = %q, want the operation to keep the lock while it unwinds",
			backup.Status.ActiveOpsRef)
	}
}

// Deleting the object removes the only record naming the volume, so the
// finalizer has to discard it first.
func TestDeletingANonTerminalRestoreDiscardsItsVolume(t *testing.T) {
	api := &fakeControlPlane{}
	deleting := restoringOps()
	deleting.Status.RestoredLvolID = testRestoreID
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	r := opsReconciler(t, api, testClusterObject(), testPoolObject(), heldBackup(), deleting)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(api.deleted) != 1 {
		t.Errorf("deleted %v, want the volume the operation had created", api.deleted)
	}
}

// A finished restore's volume belongs to the claim it was bound to, not to the
// operation, which is the whole reason that claim carries no owner reference
// back. Deleting the audit record must leave the data alone.
func TestDeletingATerminalRestoreLeavesItsVolumeAlone(t *testing.T) {
	api := &fakeControlPlane{}
	finished := restoreOps(testOpsName)
	finished.Status = simplyblockv1alpha2.StorageBackupOpsStatus{
		Phase:          simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded,
		ClusterID:      testClusterID,
		PoolUUID:       testPoolID,
		RestoredLvolID: testRestoreID,
	}
	now := metav1.Now()
	finished.DeletionTimestamp = &now

	r := opsReconciler(t, api, testClusterObject(), testPoolObject(), availableBackup(), finished)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(api.deleted) != 0 {
		t.Errorf("deleted %v: a succeeded restore's volume belongs to its claim", api.deleted)
	}
}

// The claim race the admission check cannot close. A claim created between the
// step's own check and its create would otherwise be bound to by a restore that
// never made it, which is the adoption the whole band exists to refuse.
func TestAClaimCreatedDuringBindingIsRefusedRatherThanAdopted(t *testing.T) {
	handle := lvol.NewVolumeHandle(testClusterID, testPoolID, testRestoreID)
	api := &fakeControlPlane{
		restoredID: testRestoreID,
		volumes:    map[string]lvol.Volume{string(handle): {Status: cpVolumeOnline}},
		connection: lvol.Connection{
			NQN:       "nqn.2023-01.io.simplyblock:lvol",
			Endpoints: []lvol.Endpoint{{Transport: "tcp", Address: "10.0.0.1", Port: 4420}},
		},
	}

	binding := restoreOps(testOpsName)
	binding.Status = simplyblockv1alpha2.StorageBackupOpsStatus{
		Phase:          simplyblockv1alpha2.StorageBackupOpsPhaseRunning,
		ClusterID:      testClusterID,
		PoolUUID:       testPoolID,
		BackupID:       testBackupID,
		RestoredLvolID: testRestoreID,
		ClaimName:      "restored-claim",
		Step:           statemachine.KubeSnapshot{State: string(stepBinding)},
	}

	// Somebody else's claim, of the name this restore was going to use, already
	// in the world by the time Binding runs.
	somebodyElses := &corev1.PersistentVolumeClaim{ObjectMeta: objectMeta("restored-claim")}

	r := opsReconciler(t, api,
		testClusterObject(), testPoolObject(), heldBackup(), binding, somebodyElses)

	ops := reconcileUntilSettled(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed rather than binding to somebody else's claim",
			ops.Status.Phase)
	}

	// The claim is untouched: no ownership marker was written onto it.
	var claim corev1.PersistentVolumeClaim
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "restored-claim", Namespace: testNamespace}, &claim); err != nil {
		t.Fatal(err)
	}
	if _, marked := claim.Labels[RestoredByLabel]; marked {
		t.Error("the restore claimed ownership of a claim it did not create")
	}
}

// The ownership marker is the operator's, not the request's. A request that
// could overwrite it would produce a claim failing its own owner check, and the
// restore would time out having created the thing it then refused.
func TestARequestCannotOverrideTheOwnershipMarker(t *testing.T) {
	handle := lvol.NewVolumeHandle(testClusterID, testPoolID, testRestoreID)
	api := &fakeControlPlane{
		restoredID: testRestoreID,
		volumes:    map[string]lvol.Volume{string(handle): {Status: cpVolumeOnline}},
		connection: lvol.Connection{
			NQN:       "nqn.2023-01.io.simplyblock:lvol",
			Endpoints: []lvol.Endpoint{{Transport: "tcp", Address: "10.0.0.1", Port: 4420}},
		},
	}

	hostile := restoreOps(testOpsName)
	hostile.Spec.Restore.ClaimLabels = map[string]string{RestoredByLabel: "somebody-else"}

	r := opsReconciler(t, api, testClusterObject(), testPoolObject(), availableBackup(), hostile)

	for range 6 {
		if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	var claim corev1.PersistentVolumeClaim
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "restored-claim", Namespace: testNamespace}, &claim); err != nil {
		t.Fatalf("the restore produced no claim: %v", err)
	}
	if got := claim.Labels[RestoredByLabel]; got != testOpsName {
		t.Errorf("restored-by = %q, want the operation's own name", got)
	}
}

// errRefused stands in for a control plane that will not delete right now.
type errRefused struct{}

func (errRefused) Error() string { return "the control plane refused the delete" }
