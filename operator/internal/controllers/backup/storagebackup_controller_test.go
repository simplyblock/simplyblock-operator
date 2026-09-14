// Tests for the StorageBackup mirror: the projection from a control-plane
// status to a typed phase, and the create, update, and delete paths of the
// reconciler that publishes it.
//
// The delete path carries the assertion that matters most here. A backup object
// is the whole of what Kubernetes knows about a copy, so deleting one on the
// strength of a cold cache would empty the inventory every time the operator
// restarted, and every restore would then have nothing to name.

package backup

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

func backupObjectName() string { return simplyblockv1alpha2.StorageBackupName(testBackupID) }

func backupRequest() ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: backupObjectName()},
	}
}

func reportedBackup() subscriptions.BackupDTO {
	return subscriptions.BackupDTO{
		ID:           testBackupID,
		LvolID:       testLvolID,
		LvolName:     "vol1",
		SnapshotID:   "snapshot-id",
		SnapshotName: "snap-1",
		NodeID:       "node-1",
		Status:       cpBackupCompleted,
		Size:         1 << 30,
		CreatedAt:    1700000000,
		CompletedAt:  1700000060,
	}
}

// backupMirror builds a reconciler over the given cache and objects.
func backupMirror(
	t *testing.T, cache BackupCache, objs ...client.Object,
) *StorageBackupReconciler {
	t.Helper()
	c := testClient(t, objs...)
	return &StorageBackupReconciler{
		Client: c, Scheme: testScheme(t), Backups: cache, Recorder: testRecorder(),
	}
}

func syncedCache(backups ...subscriptions.BackupDTO) *fakeBackupCache {
	cache := &fakeBackupCache{synced: true, backups: map[string]subscriptions.BackupDTO{}}
	for _, backup := range backups {
		cache.backups[simplyblockv1alpha2.StorageBackupName(backup.ID)] = backup
	}
	return cache
}

// sourceVolume is a PersistentVolume the mirror resolves a backup's claim, pool,
// and filesystem through.
func sourceVolume() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Name: "claim-1", Namespace: testNamespace},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					VolumeHandle: testClusterID + ":" + testPoolID + ":" + testLvolID,
					FSType:       "xfs",
				},
			},
		},
	}
}

func TestMirrorCreatesAnObjectForADiscoveredBackup(t *testing.T) {
	r := backupMirror(t, syncedCache(reportedBackup()), testClusterObject(), sourceVolume())

	if _, err := r.Reconcile(context.Background(), backupRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var backup simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(), backupRequest().NamespacedName, &backup); err != nil {
		t.Fatalf("the mirror created no object: %v", err)
	}

	if backup.Spec.ClusterRef != testClusterCR || backup.Spec.BackupID != testBackupID {
		t.Errorf("spec = %+v, want the cluster and the store's identifier", backup.Spec)
	}
	if got := backup.Status.Phase; got != simplyblockv1alpha2.StorageBackupPhaseAvailable {
		t.Errorf("status.phase = %q, want %q", got, simplyblockv1alpha2.StorageBackupPhaseAvailable)
	}
	if got := backup.Copy().Size; got == nil || *got != 1<<30 {
		t.Errorf("status.backup.size = %v, want 1GiB", got)
	}
	// The Kubernetes half of the source, which the control plane does not report
	// and the mirror resolves through the volume's handle.
	source := backup.Source()
	if source.ClaimName != "claim-1" || source.FSType != "xfs" || source.PoolUUID != testPoolID {
		t.Errorf("status.source = %+v, want the claim, filesystem, and pool of the source volume", source)
	}
	if got := backup.Labels[simplyblockv1alpha2.BackupLabelClaim]; got != "claim-1" {
		t.Errorf("claim label = %q, want claim-1", got)
	}
}

// A backup whose volume is gone still gets an object. The copy is in the bucket
// and is restorable, and refusing to record it would make it unreachable: a
// restore names a StorageBackup, and there would be none to name.
func TestMirrorRecordsABackupWhoseSourceVolumeIsGone(t *testing.T) {
	r := backupMirror(t, syncedCache(reportedBackup()), testClusterObject())

	if _, err := r.Reconcile(context.Background(), backupRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var backup simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(), backupRequest().NamespacedName, &backup); err != nil {
		t.Fatalf("the mirror created no object: %v", err)
	}
	if got := backup.Source().ClaimName; got != "" {
		t.Errorf("status.source.claimName = %q, want empty for a backup with no source volume", got)
	}
	if _, labeled := backup.Labels[simplyblockv1alpha2.BackupLabelClaim]; labeled {
		t.Error("the object carries a claim label naming a claim that does not exist")
	}
	// What the control plane did report is still recorded.
	if got := backup.Source().LvolID; got != testLvolID {
		t.Errorf("status.source.lvolID = %q, want %q", got, testLvolID)
	}
}

// The source group is written once and never updated. The pool a volume was in
// when it was backed up is a fact about the backup, and rewriting it when the
// volume moves would destroy the only record of where the data came from, which
// is what a restore reads.
func TestMirrorNeverRewritesTheSourceGroup(t *testing.T) {
	recorded := &simplyblockv1alpha2.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: backupObjectName(), Namespace: testNamespace},
		Spec: simplyblockv1alpha2.StorageBackupSpec{
			ClusterRef: testClusterCR, BackupID: testBackupID,
		},
		Status: simplyblockv1alpha2.StorageBackupStatus{
			ClusterID: testClusterID,
			Source:    &simplyblockv1alpha2.BackupSource{PoolName: "the-pool-it-came-from"},
		},
	}
	r := backupMirror(t, syncedCache(reportedBackup()), testClusterObject(), sourceVolume(), recorded)

	if _, err := r.Reconcile(context.Background(), backupRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var backup simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(), backupRequest().NamespacedName, &backup); err != nil {
		t.Fatal(err)
	}
	if got := backup.Source().PoolName; got != "the-pool-it-came-from" {
		t.Errorf("status.source.poolName = %q, want the pool recorded when the copy was taken", got)
	}
}

// A synced stream that does not mention a backup is a backup that left the
// store, which is the ordinary end of a copy's life.
func TestMirrorDeletesAnObjectWhoseBackupLeftTheStore(t *testing.T) {
	recorded := &simplyblockv1alpha2.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: backupObjectName(), Namespace: testNamespace},
		Spec: simplyblockv1alpha2.StorageBackupSpec{
			ClusterRef: testClusterCR, BackupID: testBackupID,
		},
		Status: simplyblockv1alpha2.StorageBackupStatus{ClusterID: testClusterID},
	}
	r := backupMirror(t, syncedCache(), testClusterObject(), recorded)

	if _, err := r.Reconcile(context.Background(), backupRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var backup simplyblockv1alpha2.StorageBackup
	err := r.Get(context.Background(), backupRequest().NamespacedName, &backup)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the object survived a synced stream that no longer reports it: %v", err)
	}
}

// A cold cache has said nothing at all, and deleting on that would empty the
// inventory on every operator restart. This is the single most important
// assertion about the mirror's delete path.
func TestMirrorKeepsAnObjectWhileTheStreamHasNotSynced(t *testing.T) {
	recorded := &simplyblockv1alpha2.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: backupObjectName(), Namespace: testNamespace},
		Spec: simplyblockv1alpha2.StorageBackupSpec{
			ClusterRef: testClusterCR, BackupID: testBackupID,
		},
		Status: simplyblockv1alpha2.StorageBackupStatus{ClusterID: testClusterID},
	}
	cold := &fakeBackupCache{synced: false, backups: map[string]subscriptions.BackupDTO{}}
	r := backupMirror(t, cold, testClusterObject(), recorded)

	result, err := r.Reconcile(context.Background(), backupRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("a cold cache produced no requeue, so the object would never be looked at again")
	}

	var backup simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(), backupRequest().NamespacedName, &backup); err != nil {
		t.Errorf("the object was deleted on the strength of a cache that has said nothing: %v", err)
	}
}

func TestBackupPhaseGroupsTheControlPlanesStatuses(t *testing.T) {
	for status, want := range map[string]simplyblockv1alpha2.StorageBackupPhase{
		cpBackupPending:    simplyblockv1alpha2.StorageBackupPhasePending,
		cpBackupInProgress: simplyblockv1alpha2.StorageBackupPhaseCreating,
		cpBackupCompleted:  simplyblockv1alpha2.StorageBackupPhaseAvailable,
		cpBackupFailed:     simplyblockv1alpha2.StorageBackupPhaseFailed,
		// Both describe a copy being rewritten, so neither is restorable and
		// neither has failed.
		cpBackupMerging:  simplyblockv1alpha2.StorageBackupPhaseCreating,
		cpBackupDeleting: simplyblockv1alpha2.StorageBackupPhaseCreating,
		// A status this operator has never seen says the operator does not know,
		// which is not the same as saying the copy is usable.
		"something_new": simplyblockv1alpha2.StorageBackupPhasePending,
	} {
		if got := backupPhaseFor(status); got != want {
			t.Errorf("backupPhaseFor(%q) = %q, want %q", status, got, want)
		}
	}
}

// The index is what lets the mirror resolve a backup's claim without listing
// every volume in the cluster, and a volume it cannot read is one it must not
// index under a wrong key.
func TestVolumeIndexCoversOnlyWellFormedSimplyblockVolumes(t *testing.T) {
	indexed := IndexPersistentVolumeLvolID(sourceVolume())
	if len(indexed) != 1 || indexed[0] != testLvolID {
		t.Errorf("a simplyblock volume indexed as %v, want [%s]", indexed, testLvolID)
	}

	foreign := &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{
		PersistentVolumeSource: corev1.PersistentVolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: "/data"},
		},
	}}
	if indexed := IndexPersistentVolumeLvolID(foreign); indexed != nil {
		t.Errorf("a non-CSI volume indexed as %v, want nothing", indexed)
	}

	malformed := &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{
		PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: "not-a-handle"},
		},
	}}
	if indexed := IndexPersistentVolumeLvolID(malformed); indexed != nil {
		t.Errorf("a malformed handle indexed as %v, want nothing", indexed)
	}
}
