// Tests for the group path of the StorageBackup request reconciler: a request
// with spec.source.consistencyGroupName takes one generation of the group its
// claims belong to, backs it up as one unit, and follows the member copies. The
// counts of what the control plane was asked are the assertions, because a
// second generation or a second group backup is the failure under test.

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
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	groupRequestName = "orders-now"
	testGroupName    = "orders"
	testCGID         = testClusterID + "/cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	memberLvolA      = "aaaaaaaa-0000-4000-8000-000000000001"
	memberLvolB      = "aaaaaaaa-0000-4000-8000-000000000002"
	createdGroupID   = "55555555-5555-5555-5555-555555555555"
)

// memberClaim is a claim in the request's namespace, labeled into the group and
// bound to a volume of the test cluster, and the volume it is bound to.
func memberClaim(name, lvolID, clusterID string) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	pvName := "pv-" + name
	return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: requestNamespace,
				Labels: map[string]string{consistencyGroupLabel: testGroupName},
			},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
		}, &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: pvName},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					VolumeHandle: clusterID + ":" + testPoolID + ":" + lvolID, FSType: "xfs",
				},
			}},
		}
}

// newGroupRequestReconciler holds a group request for testGroupName in the app
// namespace, with two member claims and the cluster, plus any extra objects.
func newGroupRequestReconciler(
	t *testing.T, api *fakeRequestAPI, streams *fakeStreams, status *simplyblockv1alpha2.StorageBackupStatus,
	extra ...client.Object,
) *StorageBackupRequestReconciler {
	t.Helper()
	request := &simplyblockv1alpha2.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: groupRequestName, Namespace: requestNamespace, UID: requestUID},
		Spec: simplyblockv1alpha2.StorageBackupSpec{
			Source: &simplyblockv1alpha2.BackupRequest{ConsistencyGroupName: testGroupName},
		},
	}
	if status != nil {
		request.Status = *status
	}
	claimA, pvA := memberClaim("db", memberLvolA, testClusterID)
	claimB, pvB := memberClaim("data", memberLvolB, testClusterID)
	cluster := testClusterObject()
	cluster.Spec.Backup = &simplyblockv1alpha2.BackupStoreSpec{}
	objs := append([]client.Object{request, claimA, pvA, claimB, pvB, cluster}, extra...)
	return &StorageBackupRequestReconciler{
		Client: testClient(t, objs...), Scheme: testScheme(t), Recorder: testRecorder(), API: api,
		Snapshots: streams, Backups: streams,
	}
}

func reconcileGroupRequest(t *testing.T, r *StorageBackupRequestReconciler) *simplyblockv1alpha2.StorageBackup {
	t.Helper()
	key := types.NamespacedName{Namespace: requestNamespace, Name: groupRequestName}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get the request: %v", err)
	}
	return &got
}

func groupAPI() *fakeRequestAPI {
	return &fakeRequestAPI{
		groups:       map[string]string{testGroupName: testCGID},
		takenSeq:     7,
		createdGroup: controlplane.CreatedGroupBackup{ID: createdGroupID, MemberCount: 2},
	}
}

// One pass takes one generation, records it, and backs that generation up. A
// second pass, with the stream not yet reporting the copies, asks for nothing.
func TestGroupRequestTakesAGenerationAndBacksItUpOnce(t *testing.T) {
	api, streams := groupAPI(), &fakeStreams{synced: true}
	r := newGroupRequestReconciler(t, api, streams, nil)

	got := reconcileGroupRequest(t, r)
	if api.takes != 1 || api.groupCreates != 1 {
		t.Fatalf("generations taken = %d, group backups = %d, want one of each", api.takes, api.groupCreates)
	}
	group := got.Source().Group
	if group == nil || group.Generation != 7 || group.GroupBackupID != createdGroupID ||
		group.ConsistencyGroupName != testGroupName || group.MemberCount != 2 {
		t.Fatalf("status.source.group = %+v, want generation 7 of %s backed up as %s with 2 members",
			group, testGroupName, createdGroupID)
	}
	if got.Status.Phase != simplyblockv1alpha2.StorageBackupPhaseCreating || got.Status.ClusterID != testClusterID {
		t.Errorf("phase = %q, cluster = %q, want Creating on %s", got.Status.Phase, got.Status.ClusterID, testClusterID)
	}
	// No status.backup: the request owns the group, not one copy, so the mirror
	// keeps recording the members beside the cluster.
	if got.BackupID() != "" {
		t.Errorf("status.backup.backupID = %q, want none", got.BackupID())
	}

	reconcileGroupRequest(t, r)
	if api.takes != 1 || api.groupCreates != 1 {
		t.Errorf("a second pass asked again: generations = %d, group backups = %d", api.takes, api.groupCreates)
	}
}

// A pass that asked for the group backup and lost the answer finds it in the
// stream by the generation it recorded, and asks for nothing.
func TestGroupRequestRecoversItsGroupBackupByGeneration(t *testing.T) {
	api := groupAPI()
	streams := &fakeStreams{synced: true, backups: []subscriptions.BackupDTO{
		{ID: "m-1", LvolID: memberLvolA, GroupBackupID: testClusterID + "/" + createdGroupID, GroupSeq: 7, Status: cpBackupInProgress},
	}}
	r := newGroupRequestReconciler(t, api, streams, &simplyblockv1alpha2.StorageBackupStatus{
		Phase: simplyblockv1alpha2.StorageBackupPhaseCreating, ClusterID: testClusterID,
		Source: &simplyblockv1alpha2.BackupSource{Group: &simplyblockv1alpha2.BackupGroup{
			ConsistencyGroupName: testGroupName, Generation: 7,
		}},
	})

	got := reconcileGroupRequest(t, r)
	if api.takes != 0 || api.groupCreates != 0 {
		t.Errorf("generations = %d, group backups = %d, want none", api.takes, api.groupCreates)
	}
	if got.GroupBackupID() != testClusterID+"/"+createdGroupID {
		t.Errorf("group backup = %q, want the one the stream reports", got.GroupBackupID())
	}
}

// With the generation recorded and the stream not synced, the stream is no
// evidence the group backup is absent, so nothing is asked.
func TestGroupRequestWaitsForTheStreamBeforeAskingAgain(t *testing.T) {
	api, streams := groupAPI(), &fakeStreams{}
	r := newGroupRequestReconciler(t, api, streams, &simplyblockv1alpha2.StorageBackupStatus{
		Phase: simplyblockv1alpha2.StorageBackupPhaseCreating, ClusterID: testClusterID,
		Source: &simplyblockv1alpha2.BackupSource{Group: &simplyblockv1alpha2.BackupGroup{
			ConsistencyGroupName: testGroupName, Generation: 7,
		}},
	})

	reconcileGroupRequest(t, r)
	if api.groupCreates != 0 {
		t.Errorf("group backups = %d, want none while the stream is cold", api.groupCreates)
	}
}

// The request is Available once every member copy has completed, and Failed
// when any member fails.
func TestGroupRequestFollowsItsMembers(t *testing.T) {
	recorded := func() *simplyblockv1alpha2.StorageBackupStatus {
		return &simplyblockv1alpha2.StorageBackupStatus{
			Phase: simplyblockv1alpha2.StorageBackupPhaseCreating, ClusterID: testClusterID,
			Source: &simplyblockv1alpha2.BackupSource{Group: &simplyblockv1alpha2.BackupGroup{
				ConsistencyGroupName: testGroupName, Generation: 7, GroupBackupID: createdGroupID, MemberCount: 2,
			}},
		}
	}
	member := func(id, lvolID, status string) subscriptions.BackupDTO {
		return subscriptions.BackupDTO{
			ID: id, LvolID: lvolID, GroupBackupID: testClusterID + "/" + createdGroupID,
			GroupSeq: 7, Status: status, Size: 100,
		}
	}
	for name, tc := range map[string]struct {
		members []subscriptions.BackupDTO
		want    simplyblockv1alpha2.StorageBackupPhase
	}{
		"one still uploading": {
			members: []subscriptions.BackupDTO{member("m-1", memberLvolA, cpBackupCompleted), member("m-2", memberLvolB, cpBackupInProgress)},
			want:    simplyblockv1alpha2.StorageBackupPhaseCreating,
		},
		"one not reported yet": {
			members: []subscriptions.BackupDTO{member("m-1", memberLvolA, cpBackupCompleted)},
			want:    simplyblockv1alpha2.StorageBackupPhaseCreating,
		},
		"all completed": {
			members: []subscriptions.BackupDTO{member("m-1", memberLvolA, cpBackupCompleted), member("m-2", memberLvolB, cpBackupCompleted)},
			want:    simplyblockv1alpha2.StorageBackupPhaseAvailable,
		},
		"one failed": {
			members: []subscriptions.BackupDTO{member("m-1", memberLvolA, cpBackupCompleted), member("m-2", memberLvolB, cpBackupFailed)},
			want:    simplyblockv1alpha2.StorageBackupPhaseFailed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := groupAPI()
			r := newGroupRequestReconciler(t, api, &fakeStreams{synced: true, backups: tc.members}, recorded())
			got := reconcileGroupRequest(t, r)
			if got.Status.Phase != tc.want {
				t.Errorf("phase = %q (%s), want %q", got.Status.Phase, got.Status.Message, tc.want)
			}
			if api.takes != 0 || api.groupCreates != 0 {
				t.Errorf("a recorded group backup asked again: generations = %d, group backups = %d", api.takes, api.groupCreates)
			}
		})
	}
}

// A group with no labeled claims yet, or one the control plane does not know
// yet, holds the request in Pending and asks for nothing. Claims of one group on
// two clusters fail it: a group lives on one cluster.
func TestGroupRequestPrerequisites(t *testing.T) {
	t.Run("the group is unknown to the control plane", func(t *testing.T) {
		api := groupAPI()
		api.groups = nil
		r := newGroupRequestReconciler(t, api, &fakeStreams{synced: true}, nil)
		got := reconcileGroupRequest(t, r)
		if got.Status.Phase != simplyblockv1alpha2.StorageBackupPhasePending || api.takes != 0 {
			t.Errorf("phase = %q, generations = %d, want Pending and none", got.Status.Phase, api.takes)
		}
	})
	t.Run("members on two clusters", func(t *testing.T) {
		api := groupAPI()
		other, otherPV := memberClaim("cache", memberLvolA, "99999999-0000-4000-8000-000000000009")
		r := newGroupRequestReconciler(t, api, &fakeStreams{synced: true}, nil, other, otherPV)
		got := reconcileGroupRequest(t, r)
		if got.Status.Phase != simplyblockv1alpha2.StorageBackupPhaseFailed || api.takes != 0 {
			t.Errorf("phase = %q, generations = %d, want Failed and none", got.Status.Phase, api.takes)
		}
	})
}

// With no claim in the namespace labeled into the group, the request waits.
func TestGroupRequestWithNoMemberClaimsWaits(t *testing.T) {
	api := groupAPI()
	request := &simplyblockv1alpha2.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: groupRequestName, Namespace: requestNamespace, UID: requestUID},
		Spec: simplyblockv1alpha2.StorageBackupSpec{
			Source: &simplyblockv1alpha2.BackupRequest{ConsistencyGroupName: "nothing-labeled"},
		},
	}
	cluster := testClusterObject()
	cluster.Spec.Backup = &simplyblockv1alpha2.BackupStoreSpec{}
	r := &StorageBackupRequestReconciler{
		Client: testClient(t, request, cluster), Scheme: testScheme(t), Recorder: testRecorder(), API: api,
		Snapshots: &fakeStreams{synced: true}, Backups: &fakeStreams{synced: true},
	}
	got := reconcileGroupRequest(t, r)
	if got.Status.Phase != simplyblockv1alpha2.StorageBackupPhasePending || api.takes != 0 {
		t.Errorf("phase = %q, generations = %d, want Pending and none", got.Status.Phase, api.takes)
	}
}

// A change to a member copy wakes the request that owns its group backup.
func TestGroupRequestIsWokenByItsGroupBackup(t *testing.T) {
	r := newGroupRequestReconciler(t, groupAPI(), &fakeStreams{synced: true}, &simplyblockv1alpha2.StorageBackupStatus{
		Source: &simplyblockv1alpha2.BackupSource{Group: &simplyblockv1alpha2.BackupGroup{
			GroupBackupID: testClusterID + "/" + createdGroupID,
		}},
	})
	trigger := &simplyblockv1alpha2.StorageBackup{}
	trigger.SetName(createdGroupID)
	got := r.requestsForGroupBackup(context.Background(), trigger)
	if len(got) != 1 || got[0].Name != groupRequestName {
		t.Errorf("woken = %v, want %s", got, groupRequestName)
	}
}

// The take carries an id derived from the request's UID, so a pass that took a
// generation and lost the status write that recorded it is answered with the
// same generation: the group is frozen once, not once per lost write.
func TestGroupRequestRetakesNoGenerationAfterALostWrite(t *testing.T) {
	api, streams := groupAPI(), &fakeStreams{} // a cold stream stops each pass after the take
	r := newGroupRequestReconciler(t, api, streams, nil)
	reconcileGroupRequest(t, r)

	// The status write is lost: the request is back to having recorded nothing.
	var lost simplyblockv1alpha2.StorageBackup
	key := types.NamespacedName{Namespace: requestNamespace, Name: groupRequestName}
	if err := r.Get(context.Background(), key, &lost); err != nil {
		t.Fatal(err)
	}
	lost.Status = simplyblockv1alpha2.StorageBackupStatus{}
	if err := r.Status().Update(context.Background(), &lost); err != nil {
		t.Fatal(err)
	}
	got := reconcileGroupRequest(t, r)

	if len(api.takeIDs) != 2 || api.takeIDs[0] != "sbk-"+requestUID || api.takeIDs[1] != api.takeIDs[0] {
		t.Errorf("take ids = %v, want the same UID-derived id on both passes", api.takeIDs)
	}
	if api.takes != 1 {
		t.Errorf("generations taken = %d, want 1", api.takes)
	}
	if g := got.Source().Group; g == nil || g.Generation != 7 {
		t.Errorf("status.source.group = %+v, want generation 7 recorded again", g)
	}
}
