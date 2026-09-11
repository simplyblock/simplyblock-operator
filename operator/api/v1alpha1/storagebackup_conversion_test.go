// Tests for the StorageBackup conversion between v1alpha1 and the v1alpha2 hub.
//
// One property is renamed (design-property-renames.md §2.1): spec.clusterName
// becomes spec.clusterRef, matching every other reference in the group. Nothing
// else about the kind changes, so the tests worth having are the rename in both
// directions and a round trip that proves the rest of the object survives it.

package v1alpha1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestStorageBackupConvertToRenamesClusterName(t *testing.T) {
	src := &StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "sb"},
		Spec: StorageBackupSpec{
			ClusterName:  "production",
			SnapshotName: "snap-1",
		},
	}

	var dst v1alpha2.StorageBackup
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if got := dst.Spec.ClusterRef; got != "production" {
		t.Errorf("spec.clusterRef = %q, want %q", got, "production")
	}
	if got := dst.Spec.SnapshotName; got != "snap-1" {
		t.Errorf("spec.snapshotName = %q, want %q", got, "snap-1")
	}
}

func TestStorageBackupConvertFromRenamesClusterRef(t *testing.T) {
	src := &v1alpha2.StorageBackup{
		Spec: v1alpha2.StorageBackupSpec{ClusterRef: "production"},
	}

	var dst StorageBackup
	if err := dst.ConvertFrom(src); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if got := dst.Spec.ClusterName; got != "production" {
		t.Errorf("spec.clusterName = %q, want %q", got, "production")
	}
}

// Everything on this kind other than the one renamed field has to survive the
// trip. The status block is twenty-odd fields the operator alone writes, and a
// conversion that forgets one loses it on the next read.
func TestStorageBackupRoundTripsThroughTheHub(t *testing.T) {
	created := metav1.Now()

	obj := &StorageBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-1", Namespace: "sb"},
		Spec: StorageBackupSpec{
			ClusterName:       "production",
			PVCRef:            &PersistentVolumeClaimRef{Name: "claim-1", Namespace: "apps"},
			SnapshotName:      "snap-1",
			SourceClusterUUID: "11111111-2222-3333-4444-555555555555",
		},
		Status: StorageBackupStatus{
			Phase:             BackupPhaseDone,
			APIStatus:         "done",
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
			SourceClusterUUID: "11111111-2222-3333-4444-555555555555",
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
