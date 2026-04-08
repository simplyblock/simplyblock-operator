package controller

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	webapimock "github.com/simplyblock/simplyblock-manager/internal/webapi/mock"
)

func TestBackupReconcileAddsFinalizer(t *testing.T) {
	backup := &simplyblockv1alpha1.SimplyBlockBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-a",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.SimplyBlockBackupSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-a",
			Source: simplyblockv1alpha1.BackupSourceSpec{
				VolumeRef: simplyblockv1alpha1.NamedReference{Name: "vol-a"},
			},
		},
	}

	r := newBackupStateTestReconciler(t,
		backup,
		testCluster("default", "cluster-a", "cluster-uuid"),
		testClusterSecret("default", "cluster-a", "cluster-uuid", "secret"),
		testPool("default", "pool-a", "cluster-a", "pool-uuid"),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(backup)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.SimplyBlockBackup{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(backup), current); err != nil {
		t.Fatalf("failed to get backup: %v", err)
	}
	if !contains(current.Finalizers, backupFinalizer) {
		t.Fatalf("expected backup finalizer to be added")
	}
}

func TestBackupReconcileCreatesBackupAndDeletesTemporarySnapshot(t *testing.T) {
	const clusterUUID = "cluster-uuid-backup"
	const poolUUID = "pool-uuid-backup"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()

	mock.Register(http.MethodGet, "/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID+"/volumes/",
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body:   `[{"id":"vol-1","name":"sample-volume","status":"online"}]`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)
	mock.Register(http.MethodGet, "/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID+"/volumes/vol-1/snapshots",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[]`, Headers: map[string]string{"Content-Type": "application/json"}},
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[{"id":"snap-1","name":"snap-backup-1","status":"online"}]`, Headers: map[string]string{"Content-Type": "application/json"}},
	)
	mock.Register(http.MethodPost, "/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID+"/volumes/vol-1/snapshots",
		webapimock.RouteResponse{Status: http.StatusCreated, Body: `{}`, Headers: map[string]string{"Content-Type": "application/json"}},
	)
	mock.Register(http.MethodGet, "/api/v2/clusters/"+clusterUUID+"/backups/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[]`, Headers: map[string]string{"Content-Type": "application/json"}},
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[{"id":"backup-1","s3_id":7,"lvol_id":"vol-1","lvol_name":"sample-volume","snapshot_id":"snap-1","snapshot_name":"snap-backup-1","status":"completed","prev_backup_id":"","created_at":1710000000,"completed_at":1710000300}]`, Headers: map[string]string{"Content-Type": "application/json"}},
	)
	mock.Register(http.MethodPost, "/api/v2/clusters/"+clusterUUID+"/backups/",
		webapimock.RouteResponse{Status: http.StatusCreated, Body: `{}`, Headers: map[string]string{"X-Backup-Id": "backup-1"}},
	)
	mock.Register(http.MethodDelete, "/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID+"/snapshots/snap-1/",
		webapimock.RouteResponse{Status: http.StatusNoContent},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	backup := &simplyblockv1alpha1.SimplyBlockBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "backup-flow",
			Namespace:  "default",
			Finalizers: []string{backupFinalizer},
			UID:        types.UID("backup-flow-uid"),
		},
		Spec: simplyblockv1alpha1.SimplyBlockBackupSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-a",
			Source: simplyblockv1alpha1.BackupSourceSpec{
				VolumeRef: simplyblockv1alpha1.NamedReference{Name: "sample-volume"},
			},
			Snapshot: simplyblockv1alpha1.BackupSnapshotSpec{
				Name:   "snap-backup-1",
				Retain: false,
			},
		},
	}

	r := newBackupStateTestReconciler(t,
		backup,
		testCluster("default", "cluster-a", clusterUUID),
		testClusterSecret("default", "cluster-a", clusterUUID, "secret"),
		testPool("default", "pool-a", "cluster-a", poolUUID),
	)

	for range 4 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(backup)}); err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
	}

	current := &simplyblockv1alpha1.SimplyBlockBackup{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(backup), current); err != nil {
		t.Fatalf("failed to get backup: %v", err)
	}
	if current.Status.Phase != backupPhaseCompleted {
		t.Fatalf("expected completed phase, got %q", current.Status.Phase)
	}
	if current.Status.BackupID != "backup-1" {
		t.Fatalf("expected backupID backup-1, got %q", current.Status.BackupID)
	}
	if current.Status.SnapshotID != "snap-1" {
		t.Fatalf("expected snapshotID snap-1, got %q", current.Status.SnapshotID)
	}
	if !current.Status.SnapshotDeleted {
		t.Fatalf("expected temporary snapshot to be deleted")
	}
	if current.Status.S3ID == nil || *current.Status.S3ID != 7 {
		t.Fatalf("expected s3ID 7, got %#v", current.Status.S3ID)
	}
}

func newBackupStateTestReconciler(t *testing.T, objects ...client.Object) *SimplyBlockBackupReconciler {
	t.Helper()

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.SimplyBlockBackup{},
		&simplyblockv1alpha1.SimplyBlockStorageCluster{},
		&simplyblockv1alpha1.SimplyBlockPool{},
	}, objects...)

	return &SimplyBlockBackupReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
