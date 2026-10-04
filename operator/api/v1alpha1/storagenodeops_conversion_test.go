// Tests for the StorageNodeOps conversion between v1alpha1 and the v1alpha2 hub.
//
// Four properties move (design-property-renames.md §2.1, §2.4, and §2.5):
// storageNodeRef becomes nodeRef, drain becomes remove, targetWorkerNode and
// newSsdPcie regroup under migrate, and the action enum is recased.
//
// The regrouping is the one to watch. Two flat fields become one block, so the
// conversion has to decide when the block exists at all, and a block allocated
// for an operation that set neither field hands the user a value they never
// wrote.

package v1alpha1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

//nolint:dupl // parallel per-kind action table; the structure is the point, and Go cannot abstract the field access
func TestStorageNodeOpsActionConvertsBothWays(t *testing.T) {
	assertEnumConvertsBothWays(t,
		[]enumPair{
			{"shutdown", string(v1alpha2.StorageNodeOpsActionShutdown)},
			{"restart", string(v1alpha2.StorageNodeOpsActionRestart)},
			{"suspend", string(v1alpha2.StorageNodeOpsActionSuspend)},
			{"resume", string(v1alpha2.StorageNodeOpsActionResume)},
			{"remove", string(v1alpha2.StorageNodeOpsActionRemove)},
			{"migrate", string(v1alpha2.StorageNodeOpsActionMigrate)},
		},
		func(t *testing.T, action string) string {
			src := &StorageNodeOps{Spec: StorageNodeOpsSpec{Action: action}}
			var hub v1alpha2.StorageNodeOps
			if err := src.ConvertTo(&hub); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			return string(hub.Spec.Action)
		},
		func(t *testing.T, action string) string {
			src := &v1alpha2.StorageNodeOps{
				Spec: v1alpha2.StorageNodeOpsSpec{
					Action: v1alpha2.StorageNodeOpsAction(action),
				},
			}
			var back StorageNodeOps
			if err := back.ConvertFrom(src); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}
			return back.Spec.Action
		},
	)
}

func TestStorageNodeOpsConvertToRenamesNodeRefAndRemove(t *testing.T) {
	filter := testSystemVolumeFilter

	src := &StorageNodeOps{
		Spec: StorageNodeOpsSpec{
			StorageNodeRef: "node-1",
			Action:         "remove",
			Drain:          &DrainOpsSpec{SystemVolumeFilterRegex: &filter},
		},
	}

	var dst v1alpha2.StorageNodeOps
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if got := dst.Spec.NodeRef; got != "node-1" {
		t.Errorf("spec.nodeRef = %q, want %q", got, "node-1")
	}
	if dst.Spec.Remove == nil || dst.Spec.Remove.SystemVolumeFilterRegex == nil {
		t.Fatal("spec.remove.systemVolumeFilterRegex is absent")
	}
	if got := *dst.Spec.Remove.SystemVolumeFilterRegex; got != filter {
		t.Errorf("spec.remove.systemVolumeFilterRegex = %q, want %q", got, filter)
	}
}

func TestStorageNodeOpsConvertToRegroupsMigrateFields(t *testing.T) {
	src := &StorageNodeOps{
		Spec: StorageNodeOpsSpec{
			StorageNodeRef:   "node-1",
			Action:           "migrate",
			TargetWorkerNode: "worker-5",
			NewSsdPcie:       []string{"0000:5e:00.0"},
		},
	}

	var dst v1alpha2.StorageNodeOps
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if dst.Spec.Migrate == nil {
		t.Fatal("spec.migrate is absent")
	}
	if got := dst.Spec.Migrate.TargetWorkerNode; got != "worker-5" {
		t.Errorf("spec.migrate.targetWorkerNode = %q, want %q", got, "worker-5")
	}
	if diff := cmp.Diff([]string{"0000:5e:00.0"}, dst.Spec.Migrate.NewSsdPcie); diff != "" {
		t.Errorf("spec.migrate.newSsdPcie (-want +got):\n%s", diff)
	}
}

// An operation that set neither migrate field must not gain an empty migrate
// block. The block is required to carry a target worker, so an empty one is a
// value that could never have been authored.
func TestStorageNodeOpsConvertToLeavesMigrateAbsentWhenUnset(t *testing.T) {
	src := &StorageNodeOps{
		Spec: StorageNodeOpsSpec{StorageNodeRef: "node-1", Action: "restart"},
	}

	var dst v1alpha2.StorageNodeOps
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if dst.Spec.Migrate != nil {
		t.Errorf("spec.migrate = %+v, want nil", dst.Spec.Migrate)
	}
	if dst.Spec.Remove != nil {
		t.Errorf("spec.remove = %+v, want nil", dst.Spec.Remove)
	}
}

// newSsdPcie alone is enough to need the block: an operation may name extra
// drives without naming a target worker, and dropping them would silently change
// which devices the relocated node binds.
func TestStorageNodeOpsConvertToAllocatesMigrateForNewSsdPcieAlone(t *testing.T) {
	src := &StorageNodeOps{
		Spec: StorageNodeOpsSpec{
			StorageNodeRef: "node-1",
			Action:         "migrate",
			NewSsdPcie:     []string{"0000:5e:00.0"},
		},
	}

	var dst v1alpha2.StorageNodeOps
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if dst.Spec.Migrate == nil {
		t.Fatal("spec.migrate is absent, so the extra drives were dropped")
	}
}

func TestStorageNodeOpsRoundTripsThroughTheHub(t *testing.T) {
	started := metav1.Now()
	filter := testSystemVolumeFilter
	force := true
	reattach := false

	obj := &StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "sb"},
		Spec: StorageNodeOpsSpec{
			StorageNodeRef:   "node-1",
			Action:           "migrate",
			TargetWorkerNode: "worker-5",
			Force:            &force,
			ReattachVolume:   &reattach,
			NewSsdPcie:       []string{"0000:5e:00.0", "0000:5f:00.0"},
			Drain:            &DrainOpsSpec{SystemVolumeFilterRegex: &filter},
		},
		Status: StorageNodeOpsStatus{
			Phase:           StorageNodeOpsPhaseRunning,
			SubPhase:        StorageNodeOpsSubPhaseRestarting,
			Message:         "waiting for node-1 to come back",
			VolumesMigrated: 7,
			VolumesPending:  3,
			Triggered:       true,
			StartedAt:       &started,
			CompletedAt:     &started,
		},
	}

	var hub v1alpha2.StorageNodeOps
	if err := obj.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	var back StorageNodeOps
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	if diff := cmp.Diff(obj, &back); diff != "" {
		t.Errorf("round trip changed the object (-before +after):\n%s", diff)
	}
}
