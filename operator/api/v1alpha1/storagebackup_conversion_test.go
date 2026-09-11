// Tests for the StorageBackup conversion between v1alpha1 and the v1alpha2 hub.
//
// The kind changes shape rather than a field name. spec.clusterName becomes
// spec.clusterRef, the backup's store identifier moves into the spec, and the
// twenty-two flat status fields regroup under status.backup and status.source
// (design-storagebackup.md §5.1 and §5.2). So the tests worth having are the
// two directions of that regrouping, the phase values in both spellings, and
// the round trip each direction owes.
//
// Both round trips have to be lossless, and neither is by construction: four
// fields have no counterpart in the other version and are carried in annotations
// instead (design-api-upgrade.md §6.2). The trips are what prove the stash
// works, and the assertions below are what prove it does not leak — an
// annotation left behind after a value was restored would state the same fact
// twice and let the two disagree.

package v1alpha1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestStorageBackupConvertToRegroupsTheStatus(t *testing.T) {
	created := metav1.Now()

	src := &StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "sb"},
		Spec: StorageBackupSpec{
			ClusterName: "production",
			PVCRef:      &PersistentVolumeClaimRef{Name: "claim-1", Namespace: "apps"},
		},
		Status: StorageBackupStatus{
			Phase:             BackupPhaseDone,
			APIStatus:         "completed",
			ClusterUUID:       "cluster-uuid",
			PVCNamespace:      "apps",
			PVName:            "pv-1",
			PoolName:          "pool-1",
			PoolUUID:          "pool-uuid",
			LvolID:            "lvol-uuid",
			LvolName:          "lvol-1",
			FSType:            "xfs",
			SnapshotID:        "snapshot-uuid",
			SnapshotName:      "snap-1",
			SourceClusterUUID: "source-cluster-uuid",
			NodeID:            "node-uuid",
			BackupID:          "backup-uuid",
			S3ID:              42,
			PrevBackupID:      "prev-uuid",
			Size:              1 << 30,
			CreatedAt:         &created,
			CompletedAt:       &created,
		},
	}

	var dst v1alpha2.StorageBackup
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if got := dst.Spec.ClusterRef; got != "production" {
		t.Errorf("spec.clusterRef = %q, want %q", got, "production")
	}
	// The store's identifier is the object's identity in the hub, and v1alpha1
	// only ever held it in status.
	if got := dst.Spec.BackupID; got != "backup-uuid" {
		t.Errorf("spec.backupID = %q, want %q", got, "backup-uuid")
	}
	if got := dst.Status.Phase; got != v1alpha2.StorageBackupPhaseAvailable {
		t.Errorf("status.phase = %q, want %q", got, v1alpha2.StorageBackupPhaseAvailable)
	}

	wantCopy := &v1alpha2.BackupCopy{
		BackupID:         "backup-uuid",
		S3ID:             42,
		Size:             ptr.To(int64(1 << 30)),
		PreviousBackupID: "prev-uuid",
		StartedAt:        &created,
		CompletedAt:      &created,
	}
	if diff := cmp.Diff(wantCopy, dst.Status.Backup); diff != "" {
		t.Errorf("status.backup (-want +got):\n%s", diff)
	}

	wantSource := &v1alpha2.BackupSource{
		ClaimName:            "claim-1",
		ClaimNamespace:       "apps",
		PersistentVolumeName: "pv-1",
		PoolName:             "pool-1",
		PoolUUID:             "pool-uuid",
		LvolID:               "lvol-uuid",
		LvolName:             "lvol-1",
		FSType:               "xfs",
		SnapshotID:           "snapshot-uuid",
		SnapshotName:         "snap-1",
		NodeID:               "node-uuid",
		ClusterUUID:          "source-cluster-uuid",
	}
	if diff := cmp.Diff(wantSource, dst.Status.Source); diff != "" {
		t.Errorf("status.source (-want +got):\n%s", diff)
	}
}

// A backup nothing has observed yet has neither group, and an empty pointer to
// each would be two objects of nothing that every reader then has to check
// past.
func TestStorageBackupConvertToLeavesEmptyGroupsAbsent(t *testing.T) {
	src := &StorageBackup{Spec: StorageBackupSpec{ClusterName: "production"}}

	var dst v1alpha2.StorageBackup
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if dst.Status.Backup != nil {
		t.Errorf("status.backup = %+v, want nil", dst.Status.Backup)
	}
	if dst.Status.Source != nil {
		t.Errorf("status.source = %+v, want nil", dst.Status.Source)
	}
}

func TestStorageBackupConvertFromFlattensTheStatus(t *testing.T) {
	src := &v1alpha2.StorageBackup{
		Spec: v1alpha2.StorageBackupSpec{ClusterRef: "production", BackupID: "backup-uuid"},
		Status: v1alpha2.StorageBackupStatus{
			Phase:     v1alpha2.StorageBackupPhaseCreating,
			ClusterID: "cluster-uuid",
			Backup:    &v1alpha2.BackupCopy{BackupID: "backup-uuid", Size: ptr.To(int64(99))},
			Source:    &v1alpha2.BackupSource{ClaimName: "claim-1", ClaimNamespace: "apps", PoolName: "pool-1"},
		},
	}

	var dst StorageBackup
	if err := dst.ConvertFrom(src); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	if got := dst.Spec.ClusterName; got != "production" {
		t.Errorf("spec.clusterName = %q, want %q", got, "production")
	}
	if dst.Spec.PVCRef == nil || dst.Spec.PVCRef.Name != "claim-1" {
		t.Errorf("spec.pvcRef = %+v, want claim-1 in apps", dst.Spec.PVCRef)
	}
	if got := dst.Status.Phase; got != BackupPhaseInProgress {
		t.Errorf("status.phase = %q, want %q", got, BackupPhaseInProgress)
	}
	if got := dst.Status.BackupID; got != "backup-uuid" {
		t.Errorf("status.backupID = %q, want %q", got, "backup-uuid")
	}
	if got := dst.Status.Size; got != 99 {
		t.Errorf("status.size = %d, want 99", got)
	}
	if got := dst.Status.ClusterUUID; got != "cluster-uuid" {
		t.Errorf("status.clusterUUID = %q, want %q", got, "cluster-uuid")
	}
	if got := dst.Status.PoolName; got != "pool-1" {
		t.Errorf("status.poolName = %q, want %q", got, "pool-1")
	}
}

// A restore reads a backup by name and refuses one whose phase is Failed, so
// both spellings of every phase have to agree. The two the hub does not declare
// are the control plane's own lifecycle states, which status.apiStatus keeps
// verbatim and which the four-value phase folds into Creating.
func TestStorageBackupPhaseConvertsBothWays(t *testing.T) {
	assertEnumConvertsBothWays(t,
		[]enumPair{
			{v1alpha1: BackupPhasePending, hub: string(v1alpha2.StorageBackupPhasePending)},
			{v1alpha1: BackupPhaseInProgress, hub: string(v1alpha2.StorageBackupPhaseCreating)},
			{v1alpha1: BackupPhaseDone, hub: string(v1alpha2.StorageBackupPhaseAvailable)},
			{v1alpha1: BackupPhaseFailed, hub: string(v1alpha2.StorageBackupPhaseFailed)},
		},
		func(t *testing.T, stored string) string {
			t.Helper()
			src := &StorageBackup{Status: StorageBackupStatus{Phase: stored}}
			var dst v1alpha2.StorageBackup
			if err := src.ConvertTo(&dst); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			return string(dst.Status.Phase)
		},
		func(t *testing.T, hub string) string {
			t.Helper()
			src := &v1alpha2.StorageBackup{
				Status: v1alpha2.StorageBackupStatus{Phase: v1alpha2.StorageBackupPhase(hub)},
			}
			var dst StorageBackup
			if err := dst.ConvertFrom(src); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}
			return dst.Status.Phase
		},
	)
}

func TestStorageBackupMergingAndDeletingFoldIntoCreating(t *testing.T) {
	for _, stored := range []string{BackupPhaseMerging, BackupPhaseDeleting} {
		src := &StorageBackup{Status: StorageBackupStatus{Phase: stored, APIStatus: stored}}
		var dst v1alpha2.StorageBackup
		if err := src.ConvertTo(&dst); err != nil {
			t.Fatalf("ConvertTo: %v", err)
		}
		if got := dst.Status.Phase; got != v1alpha2.StorageBackupPhaseCreating {
			t.Errorf("%q converted to phase %q, want %q", stored, got, v1alpha2.StorageBackupPhaseCreating)
		}
		// The value itself is not lost, which is what apiStatus is for.
		if got := dst.Status.APIStatus; got != stored {
			t.Errorf("status.apiStatus = %q, want %q", got, stored)
		}
	}
}

// The whole object survives the trip up and back, which it does only because the
// four fields with no counterpart are stashed. Before that they were dropped,
// and this test is what would have caught it.
func TestStorageBackupRoundTripsThroughTheHub(t *testing.T) {
	obj := storedBackup()

	var hub v1alpha2.StorageBackup
	if err := obj.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	var back StorageBackup
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	if diff := cmp.Diff(obj, &back); diff != "" {
		t.Errorf("round trip changed the object (-before +after):\n%s", diff)
	}
}

// The two fields this version has and the hub does not, proved to survive a trip
// through a shape with nowhere to put them.
//
// A v1alpha1 client that reads an object a v1alpha2 controller last wrote, and
// writes it back, is the case this protects: without the stash its allowed-hosts
// list and its requested snapshot name would be truncated by a client that never
// touched either.
func TestStorageBackupCarriesThisVersionsOwnFieldsThroughTheHub(t *testing.T) {
	obj := storedBackup()

	var hub v1alpha2.StorageBackup
	if err := obj.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	// The hub has no field for either, so the annotation is where they are while
	// the object is in this shape.
	if _, stashed := hub.Annotations["storage.simplyblock.io/conversion-status.allowedHosts"]; !stashed {
		t.Errorf("status.allowedHosts was dropped rather than stashed: %v", hub.Annotations)
	}
	if _, stashed := hub.Annotations["storage.simplyblock.io/conversion-spec.snapshotName"]; !stashed {
		t.Errorf("spec.snapshotName was dropped rather than stashed: %v", hub.Annotations)
	}

	var back StorageBackup
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	if diff := cmp.Diff(obj.Status.AllowedHosts, back.Status.AllowedHosts); diff != "" {
		t.Errorf("status.allowedHosts changed (-before +after):\n%s", diff)
	}
	if got := back.Spec.SnapshotName; got != obj.Spec.SnapshotName {
		t.Errorf("spec.snapshotName = %q, want %q", got, obj.Spec.SnapshotName)
	}
	// Restored into its field, so the annotation that carried it is gone.
	for _, key := range []string{
		"storage.simplyblock.io/conversion-status.allowedHosts",
		"storage.simplyblock.io/conversion-spec.snapshotName",
	} {
		if _, leaked := back.Annotations[key]; leaked {
			t.Errorf("%s survived after the value was restored into its field", key)
		}
	}
}

// What an object asked for and what happened are different facts, and this
// version can hold one without the other. Recovering the request from the
// observation would invent a request nobody made, so both are stashed.
func TestStorageBackupKeepsTheRequestApartFromWhatHappened(t *testing.T) {
	obj := storedBackup()
	// The import recorded a source cluster that nothing asked for.
	obj.Spec.SourceClusterUUID = ""
	obj.Spec.SnapshotName = ""

	var hub v1alpha2.StorageBackup
	if err := obj.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	var back StorageBackup
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	if got := back.Spec.SourceClusterUUID; got != "" {
		t.Errorf("spec.sourceClusterUUID = %q, want it to stay unset", got)
	}
	if got := back.Spec.SnapshotName; got != "" {
		t.Errorf("spec.snapshotName = %q, want it to stay unset", got)
	}
	// What did happen is still recorded.
	if got := back.Status.SourceClusterUUID; got != "source-cluster-uuid" {
		t.Errorf("status.sourceClusterUUID = %q, want the observation to survive", got)
	}
}

// An object nothing has stashed into carries no annotations at all. The stash is
// for values that exist, and writing an empty one would put a key on every
// object of the kind for a fact none of them has.
func TestStorageBackupStashesNothingForAnEmptyObject(t *testing.T) {
	obj := &StorageBackup{Spec: StorageBackupSpec{ClusterName: "production"}}

	var hub v1alpha2.StorageBackup
	if err := obj.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	for key := range hub.Annotations {
		t.Errorf("an empty object gained the annotation %q", key)
	}
}

// A field that was set and then cleared must not come back on the next read.
// The stash is rewritten on every conversion rather than added to, which is the
// property that makes that true.
func TestStorageBackupStashIsRewrittenRatherThanAccumulated(t *testing.T) {
	obj := storedBackup()

	var hub v1alpha2.StorageBackup
	if err := obj.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	// The hub's controller clears the lock, and the object goes back down.
	hub.Status.ActiveOpsRef = ""
	var stored StorageBackup
	if err := stored.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if _, stashed := stored.Annotations["storage.simplyblock.io/conversion-status.activeOpsRef"]; stashed {
		t.Fatal("the cleared lock is still stashed, so the next read would restore it")
	}

	var reread v1alpha2.StorageBackup
	if err := stored.ConvertTo(&reread); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if got := reread.Status.ActiveOpsRef; got != "" {
		t.Errorf("status.activeOpsRef = %q after being cleared, want empty", got)
	}
}

// storedBackup is a fully populated v1alpha1 object, so that a field added to
// either version without a conversion shows up as a diff rather than as a zero
// that happened to match.
func storedBackup() *StorageBackup {
	created := metav1.Now()

	return &StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "sb"},
		Spec: StorageBackupSpec{
			ClusterName:       "production",
			PVCRef:            &PersistentVolumeClaimRef{Name: "claim-1", Namespace: "apps"},
			SnapshotName:      "snap-1",
			SourceClusterUUID: "source-cluster-uuid",
		},
		Status: StorageBackupStatus{
			Phase:             BackupPhaseDone,
			APIStatus:         "completed",
			Message:           "completed",
			ClusterUUID:       "cluster-uuid",
			PVCNamespace:      "apps",
			PVName:            "pv-1",
			PoolName:          "pool-1",
			PoolUUID:          "pool-uuid",
			LvolID:            "lvol-uuid",
			LvolName:          "lvol-1",
			FSType:            "ext4",
			SnapshotID:        "snapshot-uuid",
			SnapshotName:      "snap-1",
			SourceClusterUUID: "source-cluster-uuid",
			BackupID:          "backup-uuid",
			S3ID:              42,
			NodeID:            "node-uuid",
			PrevBackupID:      "prev-uuid",
			Size:              1 << 30,
			AllowedHosts:      []map[string]string{{"host": "a"}},
			CreatedAt:         &created,
			CompletedAt:       &created,
		},
	}
}
