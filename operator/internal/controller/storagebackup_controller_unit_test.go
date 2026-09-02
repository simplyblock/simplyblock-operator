// Unit tests for the StorageBackup reconciler's status polling: which phase it
// writes for a backend status, and whether it schedules another poll. They live
// beside the sync controller's tests because the behavior is one reconciler's
// decision, which a fake client and an httptest control plane drive directly
// without a cluster.

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// ---- fixtures ----

const (
	mergeTestClusterName  = "merge-test-cluster"
	mergeTestClusterUUID  = "merge-cuuid-1"
	mergeTestNamespace    = "default"
	mergeTestBackupID     = "bkp-merge-1"
	mergeTestSnapshotID   = "snap-merge-1"
	mergeTestSnapshotName = "snap-merge-name-1"
	mergeTestLvolID       = "lvol-merge-1"
	mergeTestPoolName     = "pool-merge-a"
	mergeTestPoolUUID     = "pool-uuid-merge-a"
	mergeTestPVCName      = "pvc-merge-1"
	mergeTestPVName       = "pv-merge-1"
)

// mergeTestControlPlane serves the two routes the reconciler needs to reach
// syncBackupProgress: the pool list, and the cluster's backup list reporting
// backendStatus for the one backup under test.
func mergeTestControlPlane(t *testing.T, backendStatus string) *httptest.Server {
	t.Helper()

	pools, err := json.Marshal([]storagePoolAPIResponse{
		{ID: mergeTestPoolUUID, Name: mergeTestPoolName},
	})
	if err != nil {
		t.Fatalf("marshal test pools: %v", err)
	}
	backups, err := json.Marshal([]backupAPIResponse{
		{
			ID:           mergeTestBackupID,
			LvolID:       mergeTestLvolID,
			SnapshotID:   mergeTestSnapshotID,
			SnapshotName: mergeTestSnapshotName,
			Status:       backendStatus,
		},
	})
	if err != nil {
		t.Fatalf("marshal test backups: %v", err)
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if strings.Contains(req.URL.Path, "/storage-pools/") {
			_, _ = w.Write(pools)
			return
		}
		_, _ = w.Write(backups)
	}))
}

// mergeTestBackupCR returns a StorageBackup whose snapshot and backup already
// exist in the backend and whose recorded phase is phase, so a reconcile goes
// straight to polling rather than creating anything.
func mergeTestBackupCR(phase string) *simplyblockv1alpha1.StorageBackup {
	return &simplyblockv1alpha1.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:       mergeTestBackupID,
			Namespace:  mergeTestNamespace,
			Finalizers: []string{backupFinalizer},
		},
		Spec: simplyblockv1alpha1.StorageBackupSpec{
			ClusterName: mergeTestClusterName,
			PVCRef: &simplyblockv1alpha1.PersistentVolumeClaimRef{
				Name: mergeTestPVCName,
			},
		},
		Status: simplyblockv1alpha1.StorageBackupStatus{
			Phase:        phase,
			ClusterUUID:  mergeTestClusterUUID,
			PoolName:     mergeTestPoolName,
			PoolUUID:     mergeTestPoolUUID,
			LvolID:       mergeTestLvolID,
			SnapshotID:   mergeTestSnapshotID,
			SnapshotName: mergeTestSnapshotName,
			BackupID:     mergeTestBackupID,
		},
	}
}

func mergeTestPVAndPVC() (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: mergeTestPVName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					VolumeHandle: mergeTestClusterUUID + ":" + mergeTestPoolName + ":" + mergeTestLvolID,
				},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mergeTestPVCName,
			Namespace: mergeTestNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: mergeTestPVName,
		},
	}
	return pv, pvc
}

func newMergeTestReconciler(t *testing.T, apiURL string, objects ...client.Object) *StorageBackupReconciler {
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

	return &StorageBackupReconciler{
		Client:    cl,
		Scheme:    scheme,
		Recorder:  events.NewFakeRecorder(32),
		APIClient: webapi.NewClient(apiURL),
	}
}

func mergeReconcileRequest() ctrl.Request {
	return ctrl.Request{
		NamespacedName: client.ObjectKey{
			Name:      mergeTestBackupID,
			Namespace: mergeTestNamespace,
		},
	}
}

// ---- tests ----

// Regression: 2026-09-02-backup-merge-status-invisible — a backup that reached
// "completed" stopped being polled, because completion was treated as terminal.
// Retention merges the backup at an arbitrary later point, so the merging and
// merged transitions never reached StorageBackup.status: consumers could not
// tell a merged backup from a restorable one, and the e2e retention test had no
// signal to assert on at all. A merged backup is retained in the control plane's
// database rather than removed, so inferring the merge from a shrinking backup
// count cannot work either.
func TestStorageBackupKeepsPollingAfterCompletedSoALaterMergeIsSeen(t *testing.T) {
	srv := mergeTestControlPlane(t, backupAPIStatusCompleted)
	defer srv.Close()

	pv, pvc := mergeTestPVAndPVC()
	r := newMergeTestReconciler(t, srv.URL,
		testCluster(mergeTestNamespace, mergeTestClusterName, mergeTestClusterUUID),
		mergeTestBackupCR(simplyblockv1alpha1.BackupPhaseDone),
		pv, pvc,
	)

	res, err := r.Reconcile(context.Background(), mergeReconcileRequest())
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	if res.RequeueAfter <= 0 {
		t.Errorf("expected a completed backup to be polled again so a later merge is observed, got RequeueAfter=%v", res.RequeueAfter)
	}

	updated := &simplyblockv1alpha1.StorageBackup{}
	if err := r.Get(context.Background(), mergeReconcileRequest().NamespacedName, updated); err != nil {
		t.Fatalf("get StorageBackup: %v", err)
	}
	if updated.Status.Phase != simplyblockv1alpha1.BackupPhaseDone {
		t.Errorf("expected Status.Phase=%q, got %q", simplyblockv1alpha1.BackupPhaseDone, updated.Status.Phase)
	}
}

// Regression: 2026-09-02-backup-merge-status-invisible — backupPhaseFromAPIStatus
// had no case for the control plane's "merged" status, so a merged backup fell
// through to the default and was reported as Pending: a backup that had already
// been folded into its successor looked like one whose metadata had not arrived
// yet, and a BackupRestore against it would wait for a phase that never comes.
func TestStorageBackupPropagatesMergePhases(t *testing.T) {
	for _, tc := range []struct {
		name          string
		backendStatus string
		wantPhase     string
		wantPolling   bool
	}{
		{
			name:          "merge in flight is reported and still polled",
			backendStatus: backupAPIStatusMerging,
			wantPhase:     simplyblockv1alpha1.BackupPhaseMerging,
			wantPolling:   true,
		},
		{
			name:          "merged is reported and terminal",
			backendStatus: backupAPIStatusMerged,
			wantPhase:     simplyblockv1alpha1.BackupPhaseMerged,
			wantPolling:   false,
		},
		{
			name:          "failed stays terminal",
			backendStatus: backupAPIStatusFailed,
			wantPhase:     simplyblockv1alpha1.BackupPhaseFailed,
			wantPolling:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mergeTestControlPlane(t, tc.backendStatus)
			defer srv.Close()

			pv, pvc := mergeTestPVAndPVC()
			r := newMergeTestReconciler(t, srv.URL,
				testCluster(mergeTestNamespace, mergeTestClusterName, mergeTestClusterUUID),
				mergeTestBackupCR(simplyblockv1alpha1.BackupPhaseDone),
				pv, pvc,
			)

			res, err := r.Reconcile(context.Background(), mergeReconcileRequest())
			if err != nil {
				t.Fatalf("Reconcile returned error: %v", err)
			}

			updated := &simplyblockv1alpha1.StorageBackup{}
			if err := r.Get(context.Background(), mergeReconcileRequest().NamespacedName, updated); err != nil {
				t.Fatalf("get StorageBackup: %v", err)
			}
			if updated.Status.Phase != tc.wantPhase {
				t.Errorf("backend status %q: expected Status.Phase=%q, got %q",
					tc.backendStatus, tc.wantPhase, updated.Status.Phase)
			}
			if updated.Status.APIStatus != tc.backendStatus {
				t.Errorf("expected Status.APIStatus=%q, got %q", tc.backendStatus, updated.Status.APIStatus)
			}
			if polling := res.RequeueAfter > 0; polling != tc.wantPolling {
				t.Errorf("backend status %q: expected polling=%v, got RequeueAfter=%v",
					tc.backendStatus, tc.wantPolling, res.RequeueAfter)
			}
		})
	}
}
