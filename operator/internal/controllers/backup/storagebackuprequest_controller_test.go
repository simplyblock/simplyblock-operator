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
)

// fakeRequestAPI is the control plane as the request reconciler writes to it.
// What it reads comes from the streams, so a call it receives is a change.
type fakeRequestAPI struct {
	snapCreates int
	bakCreates  int
	snapName    string
}

func (f *fakeRequestAPI) CreateSnapshot(_ context.Context, _, _, _, name string) (string, error) {
	f.snapCreates++
	f.snapName = name
	return requestSnapID, nil
}

func (f *fakeRequestAPI) CreateBackup(context.Context, string, string) error {
	f.bakCreates++
	return nil
}

// fakeStreams stands in for the snapshot and backup subscriptions' caches. The
// scopes it reports synced are the ones a test opens, so a cold cache is the
// default and an empty, synced one is a deliberate setup.
type fakeStreams struct {
	snapshots []subscriptions.SnapshotDTO
	backups   []subscriptions.BackupDTO
	synced    bool
}

func (f *fakeStreams) FindByName(_ cpinformer.Scope, name string) (subscriptions.SnapshotDTO, bool) {
	for _, s := range f.snapshots {
		if s.Name == name {
			return s, true
		}
	}
	return subscriptions.SnapshotDTO{}, false
}

func (f *fakeStreams) FindBySnapshot(_, snapshotID string) (subscriptions.BackupDTO, bool) {
	for _, b := range f.backups {
		if b.SnapshotID == snapshotID {
			return b, true
		}
	}
	return subscriptions.BackupDTO{}, false
}

func (f *fakeStreams) Synced(cpinformer.Scope) bool                { return f.synced }
func (f *fakeStreams) Triggers() <-chan event.GenericEvent         { return nil }
func (f *fakeStreams) SnapshotTriggers() <-chan event.GenericEvent { return nil }

// newRequestReconciler holds a request for testClaim in the app namespace.
func newRequestReconciler(
	t *testing.T, api *fakeRequestAPI, streams *fakeStreams, withCluster bool,
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
		Client: testClient(t, objs...), Scheme: testScheme(t), Recorder: testRecorder(), API: api,
		Snapshots: streams, Backups: streams,
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
	api, streams := &fakeRequestAPI{}, &fakeStreams{synced: true}
	r := newRequestReconciler(t, api, streams, true)

	// The control plane has been asked, and the stream has not reported the
	// copy yet: nothing to record, and nothing to ask a second time.
	res, got := reconcileRequest(t, r)
	if api.snapCreates != 1 || api.bakCreates != 1 || api.snapName != "sbk-"+requestUID {
		t.Errorf("snapshots = %d (%q), backups = %d, want one of each, the snapshot named from the UID",
			api.snapCreates, api.snapName, api.bakCreates)
	}
	if got.BackupID() != "" || res.RequeueAfter == 0 {
		t.Errorf("backup ID = %q, requeue = %v, want none recorded and a fallback requeue", got.BackupID(), res.RequeueAfter)
	}

	// The stream reports the snapshot and then the copy.
	streams.snapshots = []subscriptions.SnapshotDTO{{ID: requestSnapID, Name: "sbk-" + requestUID}}
	streams.backups = []subscriptions.BackupDTO{{ID: testBackupID, SnapshotID: requestSnapID, Status: cpBackupInProgress}}
	_, got = reconcileRequest(t, r)
	if got.Status.Phase != simplyblockv1alpha2.StorageBackupPhaseCreating || got.BackupID() != testBackupID {
		t.Errorf("status = %q %q, want Creating with the backup's ID", got.Status.Phase, got.BackupID())
	}
	if got.Source().SnapshotID != requestSnapID || got.Source().ClaimName != testClaim {
		t.Errorf("source = %+v, want the snapshot and claim recorded", got.Source())
	}
	if api.snapCreates != 1 || api.bakCreates != 1 {
		t.Errorf("snapshots = %d, backups = %d, want the second pass to ask for nothing", api.snapCreates, api.bakCreates)
	}

	for status, want := range map[string]simplyblockv1alpha2.StorageBackupPhase{
		cpBackupCompleted: simplyblockv1alpha2.StorageBackupPhaseAvailable,
		cpBackupFailed:    simplyblockv1alpha2.StorageBackupPhaseFailed,
	} {
		streams.backups[0].Status, streams.backups[0].Size = status, 1<<30
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
	api := &fakeRequestAPI{}
	streams := &fakeStreams{
		synced:    true,
		snapshots: []subscriptions.SnapshotDTO{{ID: requestSnapID, Name: "sbk-" + requestUID}},
		backups:   []subscriptions.BackupDTO{{ID: testBackupID, SnapshotID: requestSnapID, Status: cpBackupInProgress}},
	}

	_, got := reconcileRequest(t, newRequestReconciler(t, api, streams, true))

	if api.snapCreates+api.bakCreates != 0 || got.BackupID() != testBackupID {
		t.Errorf("snapshots = %d, backups = %d, ID = %q, want the existing ones adopted",
			api.snapCreates, api.bakCreates, got.BackupID())
	}
}

// A cache that has not had its first frame has not said the snapshot is absent,
// and asking for a second one on the strength of that is the failure.
func TestRequestAsksForNothingFromAColdCache(t *testing.T) {
	api := &fakeRequestAPI{}

	res, _ := reconcileRequest(t, newRequestReconciler(t, api, &fakeStreams{synced: false}, true))

	if api.snapCreates+api.bakCreates != 0 || res.RequeueAfter == 0 {
		t.Errorf("snapshots = %d, backups = %d, requeue = %v, want no calls and a requeue until the stream syncs",
			api.snapCreates, api.bakCreates, res.RequeueAfter)
	}
}

// The stream is what moves a request along, so an in-progress copy is not polled
// on a short timer. The requeue is only the net under a missed event.
func TestRequestFollowsAnInProgressCopyByTheStreamNotATimer(t *testing.T) {
	streams := &fakeStreams{
		synced:    true,
		snapshots: []subscriptions.SnapshotDTO{{ID: requestSnapID, Name: "sbk-" + requestUID}},
		backups:   []subscriptions.BackupDTO{{ID: testBackupID, SnapshotID: requestSnapID, Status: cpBackupInProgress}},
	}

	res, _ := reconcileRequest(t, newRequestReconciler(t, &fakeRequestAPI{}, streams, true))

	if res.RequeueAfter != requestResync {
		t.Errorf("requeue = %v, want the %v resync", res.RequeueAfter, requestResync)
	}
}

func TestRequestWaitsForAClusterWithABackupLocation(t *testing.T) {
	api := &fakeRequestAPI{}

	res, got := reconcileRequest(t, newRequestReconciler(t, api, &fakeStreams{synced: true}, false))

	if api.snapCreates != 0 || got.Status.Phase != simplyblockv1alpha2.StorageBackupPhasePending ||
		got.Status.Message == "" || res.RequeueAfter == 0 {
		t.Errorf("snapshots = %d, status = %q %q, requeue = %v, want Pending with a reason, requeued, no calls",
			api.snapCreates, got.Status.Phase, got.Status.Message, res.RequeueAfter)
	}
}

// The stream names a snapshot, and only the request reconciler knows which
// request took it.
func TestRequestIsWokenBySnapshotName(t *testing.T) {
	r := newRequestReconciler(t, &fakeRequestAPI{}, &fakeStreams{}, true)
	named := func(name string) *metav1.PartialObjectMetadata {
		o := &metav1.PartialObjectMetadata{}
		o.SetName(name)
		return o
	}

	got := r.requestsForSnapshot(context.Background(), named("sbk-"+requestUID))
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: requestNamespace, Name: requestName}}
	if len(got) != 1 || got[0] != want {
		t.Errorf("requests = %v, want [%v]", got, want)
	}
	if other := r.requestsForSnapshot(context.Background(), named("sbk-nobody")); len(other) != 0 {
		t.Errorf("requests = %v for a snapshot no request took, want none", other)
	}
}
