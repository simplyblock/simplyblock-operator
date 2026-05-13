package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// ---- fixtures ----

const (
	syncTestClusterName   = "sync-test-cluster"
	syncTestClusterUUID   = "sync-cuuid-1"
	syncTestClusterSecret = "sync-csecret-1"
	syncTestNamespace     = "default"
	syncTestBackupID      = "bkp-sync-1"
	syncTestSnapshotID    = "snap-sync-1"
	syncTestLvolID        = "lvol-sync-1"
	syncTestPVCName       = "pvc-sync-1"
	syncTestPVName        = "pv-sync-1"
)

func syncTestCluster() *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      syncTestClusterName,
			Namespace: syncTestNamespace,
		},
		Spec: simplyblockv1alpha1.StorageClusterSpec{},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			UUID: syncTestClusterUUID,
		},
	}
}

func syncTestClusterAuthSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-cluster-" + syncTestClusterName,
			Namespace: syncTestNamespace,
		},
		Data: map[string][]byte{
			"uuid":   []byte(syncTestClusterUUID),
			"secret": []byte(syncTestClusterSecret),
		},
	}
}

// syncTestPVAndPVC returns a PV and PVC that map syncTestLvolID to syncTestPVCName.
func syncTestPVAndPVC() (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: syncTestPVName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					VolumeHandle: syncTestClusterUUID + ":pool-a:" + syncTestLvolID,
				},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      syncTestPVCName,
			Namespace: syncTestNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: syncTestPVName,
		},
	}
	return pv, pvc
}

// syncTestBackupServer returns an httptest.Server that serves a single backend
// backup entry whose LvolID matches syncTestLvolID.
func syncTestBackupServer(t *testing.T) *httptest.Server {
	t.Helper()
	backups := []backupAPIResponse{
		{
			ID:           syncTestBackupID,
			LvolID:       syncTestLvolID,
			SnapshotID:   syncTestSnapshotID,
			SnapshotName: "snap-name-1",
			Status:       backupAPIStatusCompleted,
		},
	}
	body, err := json.Marshal(backups)
	if err != nil {
		t.Fatalf("marshal test backups: %v", err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
}

func newSyncTestReconciler(t *testing.T, apiURL string, objects ...client.Object) *StorageBackupSyncReconciler {
	t.Helper()

	scheme := newTestScheme(
		t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
	)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.StorageCluster{},
		&simplyblockv1alpha1.StorageBackup{},
	}, objects...)

	return &StorageBackupSyncReconciler{
		Client:    cl,
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(32),
		APIClient: webapi.NewClient(apiURL),
	}
}

func syncReconcileRequest() ctrl.Request {
	return ctrl.Request{
		NamespacedName: client.ObjectKey{
			Name:      syncTestClusterName,
			Namespace: syncTestNamespace,
		},
	}
}

// ---- tests ----

// TestStorageBackupSyncImportsBackup verifies that when the backend has a backup
// whose lvol ID maps to an existing PVC, the reconciler creates a StorageBackup
// CR with the imported label and patches its status with the backend IDs.
func TestStorageBackupSyncImportsBackup(t *testing.T) {
	srv := syncTestBackupServer(t)
	defer srv.Close()

	pv, pvc := syncTestPVAndPVC()
	r := newSyncTestReconciler(t, srv.URL,
		syncTestCluster(),
		syncTestClusterAuthSecret(),
		pv, pvc,
	)

	res, err := r.Reconcile(context.Background(), syncReconcileRequest())
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected periodic requeue, got zero RequeueAfter")
	}

	// StorageBackup CR must have been created.
	created := &simplyblockv1alpha1.StorageBackup{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Name:      syncTestBackupID,
		Namespace: syncTestNamespace,
	}, created); err != nil {
		t.Fatalf("expected StorageBackup CR %q to exist: %v", syncTestBackupID, err)
	}

	// Must carry the imported label so the backup reconciler won't create a
	// duplicate snapshot/backup in the backend before the status patch lands.
	if created.Labels[backupSyncImportedLabel] != "true" {
		t.Errorf("expected label %s=true, got %q", backupSyncImportedLabel, created.Labels[backupSyncImportedLabel])
	}
	if created.Spec.PVCRef.Name != syncTestPVCName {
		t.Errorf("expected PVCRef.Name=%q, got %q", syncTestPVCName, created.Spec.PVCRef.Name)
	}

	// Status must have been patched with the backend IDs.
	if created.Status.BackupID != syncTestBackupID {
		t.Errorf("expected Status.BackupID=%q, got %q", syncTestBackupID, created.Status.BackupID)
	}
	if created.Status.SnapshotID != syncTestSnapshotID {
		t.Errorf("expected Status.SnapshotID=%q, got %q", syncTestSnapshotID, created.Status.SnapshotID)
	}
	if created.Status.Phase != simplyblockv1alpha1.BackupPhaseDone {
		t.Errorf("expected Phase=%q, got %q", simplyblockv1alpha1.BackupPhaseDone, created.Status.Phase)
	}
}

// TestStorageBackupSyncSkipsWhenNoPVCMatches verifies that when no PVC in the
// cluster maps to the backend backup's lvol ID, no StorageBackup CR is created.
func TestStorageBackupSyncSkipsWhenNoPVCMatches(t *testing.T) {
	srv := syncTestBackupServer(t)
	defer srv.Close()

	// No PVC/PV in the cluster — the lvol ID has no matching PVC.
	r := newSyncTestReconciler(t, srv.URL,
		syncTestCluster(),
		syncTestClusterAuthSecret(),
	)

	_, err := r.Reconcile(context.Background(), syncReconcileRequest())
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	list := &simplyblockv1alpha1.StorageBackupList{}
	if err := r.List(context.Background(), list, client.InNamespace(syncTestNamespace)); err != nil {
		t.Fatalf("List StorageBackups: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected no StorageBackup CRs, found %d", len(list.Items))
	}
}

// TestStorageBackupSyncSkipsAlreadyTracked verifies that a backend backup whose
// ID is already reflected in an existing StorageBackup CR's status is not
// imported again.
func TestStorageBackupSyncSkipsAlreadyTracked(t *testing.T) {
	srv := syncTestBackupServer(t)
	defer srv.Close()

	existing := &simplyblockv1alpha1.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "already-tracked",
			Namespace: syncTestNamespace,
		},
		Spec: simplyblockv1alpha1.StorageBackupSpec{},
		Status: simplyblockv1alpha1.StorageBackupStatus{
			BackupID: syncTestBackupID,
		},
	}

	pv, pvc := syncTestPVAndPVC()
	r := newSyncTestReconciler(t, srv.URL,
		syncTestCluster(),
		syncTestClusterAuthSecret(),
		pv, pvc,
		existing,
	)

	_, err := r.Reconcile(context.Background(), syncReconcileRequest())
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	list := &simplyblockv1alpha1.StorageBackupList{}
	if err := r.List(context.Background(), list, client.InNamespace(syncTestNamespace)); err != nil {
		t.Fatalf("List StorageBackups: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected exactly 1 StorageBackup CR (the pre-existing one), found %d", len(list.Items))
	}
	if list.Items[0].Name != "already-tracked" {
		t.Errorf("expected the pre-existing CR %q to remain, found %q", "already-tracked", list.Items[0].Name)
	}
}

// TestStorageBackupSyncSkipsAnnotationMismatch verifies that a PVC whose lvol-id
// annotation disagrees with the PV's CSI volume handle is excluded from the
// lvol→PVC map, so the backup is not associated with a potentially wrong PVC.
func TestStorageBackupSyncSkipsAnnotationMismatch(t *testing.T) {
	srv := syncTestBackupServer(t)
	defer srv.Close()

	pv, pvc := syncTestPVAndPVC()
	// Set an annotation that disagrees with the PV handle's lvol ID.
	pvc.Annotations = map[string]string{
		pvcLvolIDAnnotation: "different-lvol-id",
	}

	r := newSyncTestReconciler(t, srv.URL,
		syncTestCluster(),
		syncTestClusterAuthSecret(),
		pv, pvc,
	)

	_, err := r.Reconcile(context.Background(), syncReconcileRequest())
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	list := &simplyblockv1alpha1.StorageBackupList{}
	if err := r.List(context.Background(), list, client.InNamespace(syncTestNamespace)); err != nil {
		t.Fatalf("List StorageBackups: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected no StorageBackup CRs when annotation mismatches handle, found %d", len(list.Items))
	}
}
