// Tests for where the mirror records a copy: beside the StorageGroupBackup that
// owns its group backup, or beside the cluster when nothing does
// (design-consistency-group-backup.md §7.5). A record in the wrong namespace is
// moved, never lost: the copy is still in the store, and a record is the only
// thing a restore can name.

package backup

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	placementNamespace = "app-a"
	placementGroupID   = testClusterID + "/" + createdGroupID
)

func groupMemberCopy() subscriptions.BackupDTO {
	dto := reportedBackup()
	dto.GroupBackupID = placementGroupID
	dto.GroupSeq = 7
	return dto
}

func owningGroupBackup() *simplyblockv1alpha2.StorageGroupBackup {
	return &simplyblockv1alpha2.StorageGroupBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-now", Namespace: placementNamespace},
		Spec: simplyblockv1alpha2.StorageGroupBackupSpec{
			Source: simplyblockv1alpha2.GroupBackupSource{ConsistencyGroupName: testGroupName},
		},
		// The bare id, as the create call answers it: the member copies report the
		// composite one, and the two must still match.
		Status: simplyblockv1alpha2.StorageGroupBackupStatus{ClusterID: testClusterID, GroupBackupID: createdGroupID},
	}
}

func memberRecord(namespace string) *simplyblockv1alpha2.StorageBackup {
	return &simplyblockv1alpha2.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: backupObjectName(), Namespace: namespace},
		Spec:       simplyblockv1alpha2.StorageBackupSpec{ClusterRef: testClusterCR},
		Status:     simplyblockv1alpha2.StorageBackupStatus{ClusterID: testClusterID},
	}
}

func recordIn(t *testing.T, r *StorageBackupReconciler, namespace string) (*simplyblockv1alpha2.StorageBackup, bool) {
	t.Helper()
	var sb simplyblockv1alpha2.StorageBackup
	err := r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: backupObjectName()}, &sb)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return &sb, true
}

func reconcileKey(t *testing.T, r *StorageBackupReconciler, namespace string) {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: backupObjectName()}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile %s: %v", req, err)
	}
}

// A member of a group backup a StorageGroupBackup owns is recorded beside it,
// labeled with its name, and not beside the cluster.
func TestMirrorRecordsAMemberBesideItsGroupBackup(t *testing.T) {
	r := backupMirror(t, syncedCache(groupMemberCopy()), testClusterObject(), sourceVolume(), owningGroupBackup())
	reconcileKey(t, r, testNamespace)

	sb, ok := recordIn(t, r, placementNamespace)
	if !ok {
		t.Fatalf("no record beside the StorageGroupBackup in %s", placementNamespace)
	}
	if sb.Labels[simplyblockv1alpha2.BackupLabelGroupBackup] != "orders-now" {
		t.Errorf("group-backup label = %q, want orders-now", sb.Labels[simplyblockv1alpha2.BackupLabelGroupBackup])
	}
	if sb.Labels[simplyblockv1alpha2.BackupLabelGroupBackupID] != createdGroupID {
		t.Errorf("group-backup-id label = %q, want %s", sb.Labels[simplyblockv1alpha2.BackupLabelGroupBackupID], createdGroupID)
	}
	if sb.Status.Phase != simplyblockv1alpha2.StorageBackupPhaseAvailable || sb.Spec.ClusterRef != testClusterCR {
		t.Errorf("record = phase %q, clusterRef %q, want Available on %s", sb.Status.Phase, sb.Spec.ClusterRef, testClusterCR)
	}
	if _, ok := recordIn(t, r, testNamespace); ok {
		t.Errorf("the member is also recorded beside the cluster")
	}
}

// The control plane can report a member before the StorageGroupBackup recorded
// the id it got back, so the member was first recorded beside the cluster. Once
// the owner is known, it moves.
func TestMirrorMovesAMemberRecordedBeforeItsOwnerWasKnown(t *testing.T) {
	r := backupMirror(t, syncedCache(groupMemberCopy()),
		testClusterObject(), sourceVolume(), owningGroupBackup(), memberRecord(testNamespace))
	reconcileKey(t, r, testNamespace)

	if _, ok := recordIn(t, r, placementNamespace); !ok {
		t.Errorf("the member was not moved beside its StorageGroupBackup")
	}
	if _, ok := recordIn(t, r, testNamespace); ok {
		t.Errorf("the member's old record beside the cluster is still there")
	}
}

// A member whose StorageGroupBackup is gone goes back beside the cluster, and
// keeps no group-backup label naming an object that no longer exists.
func TestMirrorMovesAMemberBackWhenItsOwnerIsGone(t *testing.T) {
	away := memberRecord(placementNamespace)
	away.Labels = map[string]string{simplyblockv1alpha2.BackupLabelGroupBackup: "orders-now"}
	r := backupMirror(t, syncedCache(groupMemberCopy()), testClusterObject(), sourceVolume(), away)
	reconcileKey(t, r, testNamespace)

	sb, ok := recordIn(t, r, testNamespace)
	if !ok {
		t.Fatalf("the member was not moved back beside the cluster")
	}
	if _, labeled := sb.Labels[simplyblockv1alpha2.BackupLabelGroupBackup]; labeled {
		t.Errorf("the record beside the cluster still carries the group-backup label")
	}
	if _, ok := recordIn(t, r, placementNamespace); ok {
		t.Errorf("the record beside the deleted StorageGroupBackup is still there")
	}
}

// The mirror's own watch enqueues a record by the key it lives at. A record
// beside its StorageGroupBackup is not one the stream names, and must not be
// taken for a copy that left the store.
func TestMirrorKeepsAMemberReconciledByItsOwnKey(t *testing.T) {
	r := backupMirror(t, syncedCache(groupMemberCopy()),
		testClusterObject(), sourceVolume(), owningGroupBackup(), memberRecord(placementNamespace))
	reconcileKey(t, r, placementNamespace)

	if _, ok := recordIn(t, r, placementNamespace); !ok {
		t.Errorf("a member beside its StorageGroupBackup was deleted when reconciled by its own key")
	}
	if _, ok := recordIn(t, r, testNamespace); ok {
		t.Errorf("reconciling the member by its own key recorded it beside the cluster too")
	}
}

// A member that left the store is deleted where it is, beside its group.
func TestMirrorDeletesAMemberThatLeftTheStore(t *testing.T) {
	r := backupMirror(t, syncedCache(), testClusterObject(), owningGroupBackup(), memberRecord(placementNamespace))
	reconcileKey(t, r, testNamespace)

	if _, ok := recordIn(t, r, placementNamespace); ok {
		t.Errorf("the record of a copy that left the store is still beside its StorageGroupBackup")
	}
}

// A StorageGroupBackup changing, or going, enqueues its members by the key the
// stream names them by, beside the cluster, so each is placed again.
func TestMirrorIsWokenByAStorageGroupBackup(t *testing.T) {
	r := backupMirror(t, syncedCache(groupMemberCopy()), testClusterObject(), owningGroupBackup())
	got := r.membersOf(context.Background(), owningGroupBackup())
	want := types.NamespacedName{Namespace: testNamespace, Name: backupObjectName()}
	if len(got) != 1 || got[0].NamespacedName != want {
		t.Errorf("woken = %v, want [%s]", got, want)
	}

	// No group backup recorded yet: nothing to place.
	empty := owningGroupBackup()
	empty.Status.GroupBackupID = ""
	if got := r.membersOf(context.Background(), empty); len(got) != 0 {
		t.Errorf("woken = %v for a StorageGroupBackup with no group backup, want none", got)
	}
}

var _ client.Object = (*simplyblockv1alpha2.StorageGroupBackup)(nil)
