// Tests for the StorageBackup request reconciler. The cases that re-run a pass
// count the calls, because a second backup is the failure under test.

package backup

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	requestNamespace = "app"
	requestName      = "nightly"
	requestUID       = "99999999-9999-9999-9999-999999999999"
	requestSnapID    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	requestSnapName  = "backup_vol1_1760000000"
)

// fakeRequestAPI is the control plane as the request reconciler writes to it.
// What it reads comes from the stream, so a call it receives is a change.
type fakeRequestAPI struct {
	creates int
	volume  string
	err     error
}

func (f *fakeRequestAPI) CreateVolumeBackup(_ context.Context, _, _, volumeID string) (string, error) {
	f.creates++
	f.volume = volumeID
	return testBackupID, f.err
}

// fakeStream stands in for the backup subscription's cache.
type fakeStream struct {
	backups []subscriptions.BackupDTO
}

func (f *fakeStream) Find(id string) (cpinformer.Scope, subscriptions.BackupDTO, bool) {
	for _, b := range f.backups {
		if b.ID == id {
			return cpinformer.Scope{testClusterID}, b, true
		}
	}
	return nil, subscriptions.BackupDTO{}, false
}

func (f *fakeStream) RequestTriggers() <-chan event.GenericEvent { return nil }

// newRequestReconciler holds a request for testClaim in the app namespace.
func newRequestReconciler(
	t *testing.T, api *fakeRequestAPI, stream *fakeStream, withCluster bool,
) *StorageBackupRequestReconciler {
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
		Client: testClient(t, objs...), Scheme: testScheme(t), Recorder: testRecorder(), API: api, Backups: stream,
	}
}

// recordBackup is a request after a pass that made the call and wrote the id.
func recordBackup(t *testing.T, r *StorageBackupRequestReconciler) {
	t.Helper()
	var sb simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: requestNamespace, Name: requestName}, &sb); err != nil {
		t.Fatal(err)
	}
	sb.Status.Phase = simplyblockv1alpha2.StorageBackupPhaseCreating
	sb.Status.ClusterID = testClusterID
	sb.Status.Backup = &simplyblockv1alpha2.BackupCopy{BackupID: testBackupID}
	if err := r.Status().Update(context.Background(), &sb); err != nil {
		t.Fatal(err)
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

func inProgressBackup() subscriptions.BackupDTO {
	return subscriptions.BackupDTO{
		ID: testBackupID, SnapshotID: requestSnapID, SnapshotName: requestSnapName, Status: cpBackupInProgress,
	}
}

func TestRequestRecordsTheBackupID(t *testing.T) {
	api, stream := &fakeRequestAPI{}, &fakeStream{}
	r := newRequestReconciler(t, api, stream, true)

	res, got := reconcileRequest(t, r)

	if api.creates != 1 || api.volume != testLvolID {
		t.Errorf("backups asked for = %d of volume %q, want one of %q", api.creates, api.volume, testLvolID)
	}
	if got.BackupID() != testBackupID || got.Status.Phase != simplyblockv1alpha2.StorageBackupPhaseCreating {
		t.Errorf("status = %q with backup %q, want Creating with the id from the call, which is the only handle on the copy",
			got.Status.Phase, got.BackupID())
	}
	if got.Status.ClusterID != testClusterID {
		t.Errorf("cluster = %q, want %q", got.Status.ClusterID, testClusterID)
	}
	if res.RequeueAfter != requestResync {
		t.Errorf("requeue = %v, want the %v resync until the stream reports the copy", res.RequeueAfter, requestResync)
	}
}

func TestRequestFollowsTheCopyToItsEnd(t *testing.T) {
	api, stream := &fakeRequestAPI{}, &fakeStream{backups: []subscriptions.BackupDTO{inProgressBackup()}}
	r := newRequestReconciler(t, api, stream, true)
	recordBackup(t, r)

	_, got := reconcileRequest(t, r)
	if got.Status.Phase != simplyblockv1alpha2.StorageBackupPhaseCreating {
		t.Errorf("phase = %q, want Creating while the copy runs", got.Status.Phase)
	}
	if src := got.Source(); src.SnapshotID != requestSnapID || src.SnapshotName != requestSnapName {
		t.Errorf("source = %+v, want the snapshot the control plane took, read from the backup", src)
	}

	for status, want := range map[string]simplyblockv1alpha2.StorageBackupPhase{
		cpBackupCompleted: simplyblockv1alpha2.StorageBackupPhaseAvailable,
		cpBackupFailed:    simplyblockv1alpha2.StorageBackupPhaseFailed,
	} {
		stream.backups[0].Status, stream.backups[0].Size = status, 1<<30
		if _, got = reconcileRequest(t, r); got.Status.Phase != want {
			t.Errorf("backup %s: phase = %q, want %q", status, got.Status.Phase, want)
		}
		got.Status.Phase = simplyblockv1alpha2.StorageBackupPhaseCreating // let the next status through
		if err := r.Status().Update(context.Background(), got); err != nil {
			t.Fatal(err)
		}
	}
	if api.creates != 0 {
		t.Errorf("backups asked for = %d, want a request that holds the id to ask for none", api.creates)
	}
}

func TestRequestWhoseCallFailsRecordsNoBackup(t *testing.T) {
	api := &fakeRequestAPI{err: errors.New("node is not online")}
	r := newRequestReconciler(t, api, &fakeStream{}, true)

	_, got := reconcileRequest(t, r)

	if got.BackupID() != "" || got.Status.Message == "" {
		t.Errorf("backup = %q, message = %q, want no backup recorded and a reason", got.BackupID(), got.Status.Message)
	}
}

func TestRequestWaitsForAClusterWithABackupLocation(t *testing.T) {
	api := &fakeRequestAPI{}

	res, got := reconcileRequest(t, newRequestReconciler(t, api, &fakeStream{}, false))

	if api.creates != 0 || got.Status.Phase != simplyblockv1alpha2.StorageBackupPhasePending ||
		got.Status.Message == "" || res.RequeueAfter == 0 {
		t.Errorf("backups asked for = %d, status = %q %q, requeue = %v, want Pending with a reason, requeued, no calls",
			api.creates, got.Status.Phase, got.Status.Message, res.RequeueAfter)
	}
}

// The stream names the record the mirror would make for a backup, and only the
// request reconciler knows which request owns that backup.
func TestRequestIsWokenByTheNameOfItsBackupsRecord(t *testing.T) {
	r := newRequestReconciler(t, &fakeRequestAPI{}, &fakeStream{}, true)
	recordBackup(t, r)
	named := func(name string) *metav1.PartialObjectMetadata {
		o := &metav1.PartialObjectMetadata{}
		o.SetName(name)
		return o
	}

	got := r.requestsForBackup(context.Background(), named(simplyblockv1alpha2.StorageBackupName(testBackupID)))
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: requestNamespace, Name: requestName}}
	if len(got) != 1 || got[0] != want {
		t.Errorf("requests = %v, want [%v]", got, want)
	}
	if other := r.requestsForBackup(context.Background(), named(simplyblockv1alpha2.StorageBackupName("nobody"))); len(other) != 0 {
		t.Errorf("requests = %v for a backup no request owns, want none", other)
	}
}
