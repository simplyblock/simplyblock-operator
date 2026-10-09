// Tests for the StorageBackup request reconciler. The cases that re-run a pass
// count what the control plane was asked to do, because a second snapshot or a
// second backup is the failure under test.

package backup

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	requestNamespace = "app"
	requestName      = "nightly"
	requestUID       = "99999999-9999-9999-9999-999999999999"
	requestSnapID    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

// fakeRequestAPI is the control plane as the request reconciler sees it.
type fakeRequestAPI struct {
	snapshots   []controlplane.Snapshot
	backups     []controlplane.Backup
	snapCreates int
	bakCreates  int
}

func (f *fakeRequestAPI) CreateSnapshot(_ context.Context, _, _, _, name string) (string, error) {
	f.snapCreates++
	f.snapshots = append(f.snapshots, controlplane.Snapshot{ID: requestSnapID, Name: name})
	return requestSnapID, nil
}

func (f *fakeRequestAPI) VolumeSnapshots(context.Context, string, string, string) ([]controlplane.Snapshot, error) {
	return f.snapshots, nil
}

func (f *fakeRequestAPI) CreateBackup(_ context.Context, _, snapshotID string) error {
	f.bakCreates++
	f.backups = append(f.backups, controlplane.Backup{ID: testBackupID, SnapshotID: snapshotID, Status: cpBackupInProgress})
	return nil
}

func (f *fakeRequestAPI) ListBackups(context.Context, string) ([]controlplane.Backup, error) {
	return f.backups, nil
}

// newRequestReconciler holds a request for testClaim in the app namespace.
func newRequestReconciler(t *testing.T, api *fakeRequestAPI, withCluster bool) *StorageBackupRequestReconciler {
	t.Helper()
	objs := []client.Object{
		&simplyblockv1alpha2.StorageBackup{
			ObjectMeta: metav1.ObjectMeta{Name: requestName, Namespace: requestNamespace, UID: requestUID},
			Spec:       simplyblockv1alpha2.StorageBackupSpec{Source: &simplyblockv1alpha2.BackupRequest{ClaimName: testClaim}},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: testClaim, Namespace: requestNamespace},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-1"},
		},
		sourceVolume(),
	}
	if withCluster {
		cluster := testClusterObject()
		cluster.Spec.Backup = &simplyblockv1alpha2.BackupStoreSpec{}
		objs = append(objs, cluster)
	}
	return &StorageBackupRequestReconciler{
		Client: testClient(t, objs...), Scheme: testScheme(t), Recorder: testRecorder(), API: api,
	}
}

// reconcileRequest runs one pass and returns the request as it is afterward.
func reconcileRequest(t *testing.T, r *StorageBackupRequestReconciler) (ctrl.Result, *simplyblockv1alpha2.StorageBackup) {
	t.Helper()
	key := types.NamespacedName{Namespace: requestNamespace, Name: requestName}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get the request: %v", err)
	}
	return res, &got
}

func TestRequestSnapshotsThenBacksUpAndFollowsTheCopy(t *testing.T) {
	api := &fakeRequestAPI{}
	r := newRequestReconciler(t, api, true)

	res, got := reconcileRequest(t, r)

	if api.snapCreates != 1 || api.bakCreates != 1 || api.snapshots[0].Name != "sbk-"+requestUID {
		t.Errorf("snapshots = %d (%v), backups = %d, want one of each, the snapshot named from the UID",
			api.snapCreates, api.snapshots, api.bakCreates)
	}
	if got.Status.Phase != simplyblockv1alpha2.StorageBackupPhaseCreating || got.BackupID() != testBackupID {
		t.Errorf("status = %q %q, want Creating with the backup's ID", got.Status.Phase, got.BackupID())
	}
	if got.Source().SnapshotID != requestSnapID || got.Source().ClaimName != testClaim || res.RequeueAfter == 0 {
		t.Errorf("source = %+v, requeue = %v, want the snapshot and claim recorded and a requeue", got.Source(), res.RequeueAfter)
	}

	for status, want := range map[string]simplyblockv1alpha2.StorageBackupPhase{
		cpBackupCompleted: simplyblockv1alpha2.StorageBackupPhaseAvailable,
		cpBackupFailed:    simplyblockv1alpha2.StorageBackupPhaseFailed,
	} {
		api.backups[0].Status, api.backups[0].SizeBytes = status, 1<<30
		if _, got = reconcileRequest(t, r); got.Status.Phase != want {
			t.Errorf("backup %s: phase = %q, want %q", status, got.Status.Phase, want)
		}
		got.Status.Phase = simplyblockv1alpha2.StorageBackupPhaseCreating // let the next status through
		if err := r.Status().Update(context.Background(), got); err != nil {
			t.Fatal(err)
		}
	}
}

// A pass that died after a call and before its status write finds what the call
// made, so the retry repeats neither.
func TestRequestRetryRepeatsNeitherCall(t *testing.T) {
	api := &fakeRequestAPI{
		snapshots: []controlplane.Snapshot{{ID: requestSnapID, Name: "sbk-" + requestUID}},
		backups:   []controlplane.Backup{{ID: testBackupID, SnapshotID: requestSnapID, Status: cpBackupInProgress}},
	}

	_, got := reconcileRequest(t, newRequestReconciler(t, api, true))

	if api.snapCreates+api.bakCreates != 0 || got.BackupID() != testBackupID {
		t.Errorf("snapshots = %d, backups = %d, ID = %q, want the existing ones adopted",
			api.snapCreates, api.bakCreates, got.BackupID())
	}
}

func TestRequestWaitsForAClusterWithABackupLocation(t *testing.T) {
	api := &fakeRequestAPI{}

	res, got := reconcileRequest(t, newRequestReconciler(t, api, false))

	if api.snapCreates != 0 || got.Status.Phase != simplyblockv1alpha2.StorageBackupPhasePending ||
		got.Status.Message == "" || res.RequeueAfter == 0 {
		t.Errorf("snapshots = %d, status = %q %q, requeue = %v, want Pending with a reason, requeued, no calls",
			api.snapCreates, got.Status.Phase, got.Status.Message, res.RequeueAfter)
	}
}
