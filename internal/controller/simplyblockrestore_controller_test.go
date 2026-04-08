package controller

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	webapimock "github.com/simplyblock/simplyblock-manager/internal/webapi/mock"
)

func TestRestoreReconcileAddsFinalizer(t *testing.T) {
	restore := &simplyblockv1alpha1.SimplyBlockRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-a",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.SimplyBlockRestoreSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-a",
			Source: simplyblockv1alpha1.RestoreSourceSpec{
				BackupRef: simplyblockv1alpha1.NamedReference{Name: "backup-a"},
			},
			Target: simplyblockv1alpha1.RestoreTargetSpec{
				VolumeName: "restored-a",
			},
		},
	}

	r := newRestoreStateTestReconciler(t, restore)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(restore)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.SimplyBlockRestore{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(restore), current); err != nil {
		t.Fatalf("failed to get restore: %v", err)
	}
	if !contains(current.Finalizers, restoreFinalizer) {
		t.Fatalf("expected restore finalizer to be added")
	}
}

func TestRestoreReconcileStartsAndTracksRestore(t *testing.T) {
	const clusterUUID = "cluster-uuid-restore"
	const poolUUID = "pool-uuid-restore"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()

	mock.Register(http.MethodGet, "/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID+"/volumes/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[]`, Headers: map[string]string{"Content-Type": "application/json"}},
	)
	mock.Register(http.MethodPost, "/api/v2/clusters/"+clusterUUID+"/backups/restore",
		webapimock.RouteResponse{Status: http.StatusAccepted, Body: `{"lvol_id":"restored-vol-id"}`, Headers: map[string]string{"Content-Type": "application/json"}},
	)
	mock.Register(http.MethodGet, "/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID+"/volumes/restored-vol-id/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{"id":"restored-vol-id","name":"restored-vol","status":"online"}`, Headers: map[string]string{"Content-Type": "application/json"}},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	backup := &simplyblockv1alpha1.SimplyBlockBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-complete",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.SimplyBlockBackupSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-a",
			Source: simplyblockv1alpha1.BackupSourceSpec{
				VolumeRef: simplyblockv1alpha1.NamedReference{Name: "sample-volume"},
			},
		},
		Status: simplyblockv1alpha1.SimplyBlockBackupStatus{
			Phase:    backupPhaseCompleted,
			BackupID: "backup-1",
		},
	}
	restore := &simplyblockv1alpha1.SimplyBlockRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "restore-flow",
			Namespace:  "default",
			Finalizers: []string{restoreFinalizer},
		},
		Spec: simplyblockv1alpha1.SimplyBlockRestoreSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-a",
			Source: simplyblockv1alpha1.RestoreSourceSpec{
				BackupRef: simplyblockv1alpha1.NamedReference{Name: "backup-complete"},
			},
			Target: simplyblockv1alpha1.RestoreTargetSpec{
				VolumeName: "restored-vol",
			},
		},
	}

	r := newRestoreStateTestReconciler(t,
		restore,
		backup,
		testCluster("default", "cluster-a", clusterUUID),
		testClusterSecret("default", "cluster-a", clusterUUID, "secret"),
		testPool("default", "pool-a", "cluster-a", poolUUID),
	)

	for range 2 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(restore)}); err != nil {
			t.Fatalf("reconcile returned error: %v", err)
		}
	}

	current := &simplyblockv1alpha1.SimplyBlockRestore{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(restore), current); err != nil {
		t.Fatalf("failed to get restore: %v", err)
	}
	if current.Status.Phase != restorePhaseCompleted {
		t.Fatalf("expected completed phase, got %q", current.Status.Phase)
	}
	if current.Status.TargetVolumeID != "restored-vol-id" {
		t.Fatalf("expected targetVolumeID restored-vol-id, got %q", current.Status.TargetVolumeID)
	}
	if current.Status.BackupID != "backup-1" {
		t.Fatalf("expected backupID backup-1, got %q", current.Status.BackupID)
	}
}

func newRestoreStateTestReconciler(t *testing.T, objects ...client.Object) *SimplyBlockRestoreReconciler {
	t.Helper()

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.SimplyBlockRestore{},
		&simplyblockv1alpha1.SimplyBlockBackup{},
		&simplyblockv1alpha1.SimplyBlockStorageCluster{},
		&simplyblockv1alpha1.SimplyBlockPool{},
	}, objects...)

	return &SimplyBlockRestoreReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
