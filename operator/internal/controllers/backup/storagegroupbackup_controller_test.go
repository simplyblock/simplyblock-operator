// Tests for the StorageGroupBackup reconciler: it takes one generation of the
// group its claims belong to, backs it up as one unit, and follows the member
// copies. The counts of what the control plane was asked are the assertions,
// because a second generation or a second group backup is the failure under
// test.

package backup

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	groupBackupNamespace = "app"
	groupBackupName      = "orders-now"
	groupBackupUID       = "99999999-9999-9999-9999-999999999999"
	testGroupName        = "orders"
	testCGID             = testClusterID + "/cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	memberLvolA          = "aaaaaaaa-0000-4000-8000-000000000001"
	memberLvolB          = "aaaaaaaa-0000-4000-8000-000000000002"
	createdGroupID       = "55555555-5555-5555-5555-555555555555"
)

// fakeGroupAPI is the control plane as the group reconciler writes to it. It
// answers a take id it has seen with the generation it took for it, as the
// control plane does, and counts what it was asked.
type fakeGroupAPI struct {
	groups       map[string]string // consistency-group name -> id; missing is not found
	takes        int
	takenSeq     int
	takeIDs      []string
	takenFor     map[string]int
	groupCreates int
	createdGroup controlplane.CreatedGroupBackup
	groupErr     error
}

func (f *fakeGroupAPI) ConsistencyGroupByName(_ context.Context, _, name string) (string, error) {
	if id, ok := f.groups[name]; ok {
		return id, nil
	}
	return "", fmt.Errorf("no consistency group named %q: %w", name, errs.ErrNotFound)
}

func (f *fakeGroupAPI) TakeGroupSnapshot(_ context.Context, _, _, requestID string) (int, error) {
	f.takeIDs = append(f.takeIDs, requestID)
	if seq, ok := f.takenFor[requestID]; ok && requestID != "" {
		return seq, nil
	}
	f.takes++
	if f.takenFor == nil {
		f.takenFor = map[string]int{}
	}
	f.takenFor[requestID] = f.takenSeq
	return f.takenSeq, nil
}

func (f *fakeGroupAPI) CreateGroupBackup(_ context.Context, _, _ string, _ int) (controlplane.CreatedGroupBackup, error) {
	f.groupCreates++
	return f.createdGroup, f.groupErr
}

func groupAPI() *fakeGroupAPI {
	return &fakeGroupAPI{
		groups:       map[string]string{testGroupName: testCGID},
		takenSeq:     7,
		createdGroup: controlplane.CreatedGroupBackup{ID: createdGroupID, MemberCount: 2},
	}
}

// fakeGroupStream stands in for the backup subscription's cache. synced is what
// Synced reports for every scope, so a cold cache is the default.
type fakeGroupStream struct {
	backups []subscriptions.BackupDTO
	synced  bool
}

func (f *fakeGroupStream) Synced(cpinformer.Scope) bool { return f.synced }

func (f *fakeGroupStream) FindGroupMembers(_, groupBackupID string) []subscriptions.BackupDTO {
	var out []subscriptions.BackupDTO
	for _, b := range f.backups {
		if b.GroupBackupID != "" && simplyblockv1alpha2.GroupBackupLabelValue(b.GroupBackupID) ==
			simplyblockv1alpha2.GroupBackupLabelValue(groupBackupID) {
			out = append(out, b)
		}
	}
	return out
}

func (f *fakeGroupStream) FindGroupBackupByGeneration(_ string, seq int64, lvolIDs []string) (string, bool) {
	for _, b := range f.backups {
		for _, id := range lvolIDs {
			if b.GroupBackupID != "" && b.GroupSeq == seq && b.LvolID == id {
				return b.GroupBackupID, true
			}
		}
	}
	return "", false
}

func (f *fakeGroupStream) GroupTriggers() <-chan event.GenericEvent { return nil }

// memberClaim is a claim in the group backup's namespace, labeled into the
// group and bound to a volume of the given cluster, and the volume it is bound to.
func memberClaim(name, lvolID, clusterID string) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	pvName := "pv-" + name
	return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: groupBackupNamespace,
				Labels: map[string]string{consistencyGroupLabel: testGroupName},
			},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
		}, &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: pvName},
			Spec: corev1.PersistentVolumeSpec{
				ClaimRef: &corev1.ObjectReference{Name: name, Namespace: groupBackupNamespace},
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						VolumeHandle: clusterID + ":" + testPoolID + ":" + lvolID, FSType: "xfs",
					},
				},
			},
		}
}

// newGroupBackupReconciler holds a StorageGroupBackup for testGroupName in the
// app namespace, with two member claims and the cluster, plus any extra objects.
func newGroupBackupReconciler(
	t *testing.T, api *fakeGroupAPI, stream *fakeGroupStream,
	status *simplyblockv1alpha2.StorageGroupBackupStatus, extra ...client.Object,
) *StorageGroupBackupReconciler {
	t.Helper()
	sgb := &simplyblockv1alpha2.StorageGroupBackup{
		ObjectMeta: metav1.ObjectMeta{Name: groupBackupName, Namespace: groupBackupNamespace, UID: groupBackupUID},
		Spec: simplyblockv1alpha2.StorageGroupBackupSpec{
			Source: simplyblockv1alpha2.GroupBackupSource{ConsistencyGroupName: testGroupName},
		},
	}
	if status != nil {
		sgb.Status = *status
	}
	claimA, pvA := memberClaim("db", memberLvolA, testClusterID)
	claimB, pvB := memberClaim("data", memberLvolB, testClusterID)
	cluster := testClusterObject()
	cluster.Spec.Backup = &simplyblockv1alpha2.BackupStoreSpec{}
	objs := append([]client.Object{sgb, claimA, pvA, claimB, pvB, cluster}, extra...)
	return &StorageGroupBackupReconciler{
		Client: testClient(t, objs...), Scheme: testScheme(t), Recorder: testRecorder(), API: api, Backups: stream,
	}
}

func reconcileGroupBackup(t *testing.T, r *StorageGroupBackupReconciler) *simplyblockv1alpha2.StorageGroupBackup {
	t.Helper()
	key := types.NamespacedName{Namespace: groupBackupNamespace, Name: groupBackupName}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got simplyblockv1alpha2.StorageGroupBackup
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get the group backup: %v", err)
	}
	return &got
}

// One pass takes one generation, records it, and asks for one group backup. A
// second pass, with the stream not yet reporting the copies, asks for nothing.
func TestGroupBackupTakesAGenerationAndBacksItUpOnce(t *testing.T) {
	api, stream := groupAPI(), &fakeGroupStream{synced: true}
	r := newGroupBackupReconciler(t, api, stream, nil)

	got := reconcileGroupBackup(t, r)
	if api.takes != 1 || api.groupCreates != 1 {
		t.Fatalf("generations taken = %d, group backups = %d, want one of each", api.takes, api.groupCreates)
	}
	if api.takeIDs[0] != "sgb-"+groupBackupUID {
		t.Errorf("take id = %q, want sgb-<UID>", api.takeIDs[0])
	}
	st := got.Status
	if st.Generation != 7 || st.GroupBackupID != createdGroupID || st.MemberCount != 2 {
		t.Fatalf("status = %+v, want generation 7 backed up as %s with 2 members", st, createdGroupID)
	}
	if st.Phase != simplyblockv1alpha2.StorageGroupBackupPhaseCreating || st.ClusterID != testClusterID {
		t.Errorf("phase = %q, cluster = %q, want Creating on %s", st.Phase, st.ClusterID, testClusterID)
	}
	if st.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", st.ObservedGeneration, got.Generation)
	}

	reconcileGroupBackup(t, r)
	if api.takes != 1 || api.groupCreates != 1 {
		t.Errorf("a second pass asked again: generations = %d, group backups = %d", api.takes, api.groupCreates)
	}
}

// The take carries an id derived from the UID, so a pass that took a generation
// and lost the status write that recorded it is answered with the same
// generation: the group is frozen once, not once per lost write.
func TestGroupBackupRetakesNoGenerationAfterALostWrite(t *testing.T) {
	api, stream := groupAPI(), &fakeGroupStream{} // a cold stream stops each pass after the take
	r := newGroupBackupReconciler(t, api, stream, nil)
	reconcileGroupBackup(t, r)

	var lost simplyblockv1alpha2.StorageGroupBackup
	key := types.NamespacedName{Namespace: groupBackupNamespace, Name: groupBackupName}
	if err := r.Get(context.Background(), key, &lost); err != nil {
		t.Fatal(err)
	}
	lost.Status = simplyblockv1alpha2.StorageGroupBackupStatus{}
	if err := r.Status().Update(context.Background(), &lost); err != nil {
		t.Fatal(err)
	}
	got := reconcileGroupBackup(t, r)

	if len(api.takeIDs) != 2 || api.takeIDs[1] != api.takeIDs[0] {
		t.Errorf("take ids = %v, want the same id on both passes", api.takeIDs)
	}
	if api.takes != 1 {
		t.Errorf("generations taken = %d, want 1", api.takes)
	}
	if got.Status.Generation != 7 {
		t.Errorf("generation = %d, want 7 recorded again", got.Status.Generation)
	}
}

// A pass that asked for the group backup and lost the answer finds it in the
// stream by the generation it recorded, and asks for nothing.
func TestGroupBackupRecoversItsGroupBackupByGeneration(t *testing.T) {
	api := groupAPI()
	stream := &fakeGroupStream{synced: true, backups: []subscriptions.BackupDTO{
		{ID: "m-1", LvolID: memberLvolA, GroupBackupID: testClusterID + "/" + createdGroupID, GroupSeq: 7, Status: cpBackupInProgress},
	}}
	r := newGroupBackupReconciler(t, api, stream, &simplyblockv1alpha2.StorageGroupBackupStatus{
		Phase: simplyblockv1alpha2.StorageGroupBackupPhaseCreating, ClusterID: testClusterID, Generation: 7,
	})

	got := reconcileGroupBackup(t, r)
	if api.takes != 0 || api.groupCreates != 0 {
		t.Errorf("generations = %d, group backups = %d, want none", api.takes, api.groupCreates)
	}
	if got.Status.GroupBackupID != testClusterID+"/"+createdGroupID {
		t.Errorf("group backup = %q, want the one the stream reports", got.Status.GroupBackupID)
	}
}

// With the generation recorded and the stream not synced, the stream is no
// evidence the group backup is absent, so nothing is asked.
func TestGroupBackupWaitsForTheStreamBeforeAskingAgain(t *testing.T) {
	api := groupAPI()
	r := newGroupBackupReconciler(t, api, &fakeGroupStream{}, &simplyblockv1alpha2.StorageGroupBackupStatus{
		Phase: simplyblockv1alpha2.StorageGroupBackupPhaseCreating, ClusterID: testClusterID, Generation: 7,
	})

	reconcileGroupBackup(t, r)
	if api.groupCreates != 0 {
		t.Errorf("group backups = %d, want none while the stream is cold", api.groupCreates)
	}
}

// The group backup is Available once every member copy has completed, and
// Failed when any member fails. Its status lists the members, naming the claim
// each was taken from and the record beside it once there is one, and totals
// the size.
func TestGroupBackupFollowsItsMembers(t *testing.T) {
	recorded := func() *simplyblockv1alpha2.StorageGroupBackupStatus {
		return &simplyblockv1alpha2.StorageGroupBackupStatus{
			Phase: simplyblockv1alpha2.StorageGroupBackupPhaseCreating, ClusterID: testClusterID,
			Generation: 7, GroupBackupID: createdGroupID, MemberCount: 2,
		}
	}
	member := func(id, lvolID, status string) subscriptions.BackupDTO {
		return subscriptions.BackupDTO{
			ID: id, LvolID: lvolID, GroupBackupID: testClusterID + "/" + createdGroupID,
			GroupSeq: 7, Status: status, Size: 100, CompletedAt: 1700000000,
		}
	}
	for name, tc := range map[string]struct {
		members []subscriptions.BackupDTO
		want    simplyblockv1alpha2.StorageGroupBackupPhase
	}{
		"one still uploading": {
			members: []subscriptions.BackupDTO{member("m-1", memberLvolA, cpBackupCompleted), member("m-2", memberLvolB, cpBackupInProgress)},
			want:    simplyblockv1alpha2.StorageGroupBackupPhaseCreating,
		},
		"one not reported yet": {
			members: []subscriptions.BackupDTO{member("m-1", memberLvolA, cpBackupCompleted)},
			want:    simplyblockv1alpha2.StorageGroupBackupPhaseCreating,
		},
		"all completed": {
			members: []subscriptions.BackupDTO{member("m-1", memberLvolA, cpBackupCompleted), member("m-2", memberLvolB, cpBackupCompleted)},
			want:    simplyblockv1alpha2.StorageGroupBackupPhaseAvailable,
		},
		"one failed": {
			members: []subscriptions.BackupDTO{member("m-1", memberLvolA, cpBackupCompleted), member("m-2", memberLvolB, cpBackupFailed)},
			want:    simplyblockv1alpha2.StorageGroupBackupPhaseFailed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := groupAPI()
			placed := &simplyblockv1alpha2.StorageBackup{ObjectMeta: metav1.ObjectMeta{
				Name: simplyblockv1alpha2.StorageBackupName("m-1"), Namespace: groupBackupNamespace,
			}, Spec: simplyblockv1alpha2.StorageBackupSpec{ClusterRef: testClusterCR}}
			r := newGroupBackupReconciler(t, api, &fakeGroupStream{synced: true, backups: tc.members}, recorded(), placed)
			got := reconcileGroupBackup(t, r)
			st := got.Status
			if st.Phase != tc.want {
				t.Errorf("phase = %q (%s), want %q", st.Phase, st.Message, tc.want)
			}
			if api.takes != 0 || api.groupCreates != 0 {
				t.Errorf("a recorded group backup asked again: generations = %d, group backups = %d", api.takes, api.groupCreates)
			}
			if len(st.Members) != len(tc.members) {
				t.Fatalf("members = %+v, want %d", st.Members, len(tc.members))
			}
			for _, m := range st.Members {
				if m.BackupID == "m-1" && (m.ClaimName != "db" || m.BackupName != placed.Name) {
					t.Errorf("member m-1 = %+v, want claim db and record %s", m, placed.Name)
				}
				if m.BackupID == "m-2" && m.BackupName != "" {
					t.Errorf("member m-2 names record %q, which is not beside the group backup", m.BackupName)
				}
			}
			cond := apimeta.FindStatusCondition(st.Conditions, simplyblockv1alpha2.GroupBackupConditionAvailable)
			available := tc.want == simplyblockv1alpha2.StorageGroupBackupPhaseAvailable
			if cond == nil || (cond.Status == metav1.ConditionTrue) != available {
				t.Errorf("Available condition = %+v, want status %v", cond, available)
			}
			if available {
				if st.Size == nil || *st.Size != 200 || st.CompletedAt == nil {
					t.Errorf("size = %v, completedAt = %v, want 200 and a time", st.Size, st.CompletedAt)
				}
			}
		})
	}
}

// A group the control plane does not know yet holds the group backup in
// Pending and asks for nothing. Claims of one group on two clusters fail it: a
// group lives on one cluster.
func TestGroupBackupPrerequisites(t *testing.T) {
	t.Run("the group is unknown to the control plane", func(t *testing.T) {
		api := groupAPI()
		api.groups = nil
		r := newGroupBackupReconciler(t, api, &fakeGroupStream{synced: true}, nil)
		got := reconcileGroupBackup(t, r)
		if got.Status.Phase != simplyblockv1alpha2.StorageGroupBackupPhasePending || api.takes != 0 {
			t.Errorf("phase = %q, generations = %d, want Pending and none", got.Status.Phase, api.takes)
		}
	})
	t.Run("members on two clusters", func(t *testing.T) {
		api := groupAPI()
		other, otherPV := memberClaim("cache", memberLvolA, "99999999-0000-4000-8000-000000000009")
		r := newGroupBackupReconciler(t, api, &fakeGroupStream{synced: true}, nil, other, otherPV)
		got := reconcileGroupBackup(t, r)
		if got.Status.Phase != simplyblockv1alpha2.StorageGroupBackupPhaseFailed || api.takes != 0 {
			t.Errorf("phase = %q, generations = %d, want Failed and none", got.Status.Phase, api.takes)
		}
	})
}

// With no claim in the namespace labeled into the group, the group backup waits.
func TestGroupBackupWithNoMemberClaimsWaits(t *testing.T) {
	api := groupAPI()
	sgb := &simplyblockv1alpha2.StorageGroupBackup{
		ObjectMeta: metav1.ObjectMeta{Name: groupBackupName, Namespace: groupBackupNamespace, UID: groupBackupUID},
		Spec: simplyblockv1alpha2.StorageGroupBackupSpec{
			Source: simplyblockv1alpha2.GroupBackupSource{ConsistencyGroupName: "nothing-labeled"},
		},
	}
	cluster := testClusterObject()
	cluster.Spec.Backup = &simplyblockv1alpha2.BackupStoreSpec{}
	r := &StorageGroupBackupReconciler{
		Client: testClient(t, sgb, cluster), Scheme: testScheme(t), Recorder: testRecorder(), API: api,
		Backups: &fakeGroupStream{synced: true},
	}
	got := reconcileGroupBackup(t, r)
	if got.Status.Phase != simplyblockv1alpha2.StorageGroupBackupPhasePending || api.takes != 0 {
		t.Errorf("phase = %q, generations = %d, want Pending and none", got.Status.Phase, api.takes)
	}
}

// A change to a member copy wakes the group backup that owns it.
func TestGroupBackupIsWokenByItsGroupBackup(t *testing.T) {
	r := newGroupBackupReconciler(t, groupAPI(), &fakeGroupStream{synced: true}, &simplyblockv1alpha2.StorageGroupBackupStatus{
		GroupBackupID: testClusterID + "/" + createdGroupID,
	})
	trigger := &simplyblockv1alpha2.StorageBackup{}
	trigger.SetName(createdGroupID)
	got := r.requestsForGroupBackup(context.Background(), trigger)
	if len(got) != 1 || got[0].Name != groupBackupName {
		t.Errorf("woken = %v, want %s", got, groupBackupName)
	}
}
