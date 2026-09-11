// Tests for the StorageClusterOps conversion between v1alpha1 and the v1alpha2 hub.
//
// Three properties move (design-property-renames.md §2.1, §2.2, and §2.5): the
// rolling-restart spec and status blocks lose their Node prefix, and the action
// enum is recased with node-rolling-restart becoming RollingRestart.
//
// The action table is the part worth testing hardest. It is one field under both
// spellings, so unlike a renamed field there is nowhere for a wrong mapping to
// leave evidence: an operation converted to the wrong action runs the wrong
// operation against a cluster.

package v1alpha1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

//nolint:dupl // parallel per-kind action table; the structure is the point, and Go cannot abstract the field access
func TestStorageClusterOpsActionConvertsBothWays(t *testing.T) {
	assertEnumConvertsBothWays(t,
		[]enumPair{
			{"activate", string(v1alpha2.StorageClusterOpsActionActivate)},
			{"expand", string(v1alpha2.StorageClusterOpsActionExpand)},
			{"shutdown", string(v1alpha2.StorageClusterOpsActionShutdown)},
			{"start", string(v1alpha2.StorageClusterOpsActionStart)},
			{"restart", string(v1alpha2.StorageClusterOpsActionRestart)},
			{"node-rolling-restart", string(v1alpha2.StorageClusterOpsActionRollingRestart)},
		},
		func(t *testing.T, action string) string {
			src := &StorageClusterOps{Spec: StorageClusterOpsSpec{Action: action}}
			var hub v1alpha2.StorageClusterOps
			if err := src.ConvertTo(&hub); err != nil {
				t.Fatalf("ConvertTo: %v", err)
			}
			return string(hub.Spec.Action)
		},
		func(t *testing.T, action string) string {
			src := &v1alpha2.StorageClusterOps{
				Spec: v1alpha2.StorageClusterOpsSpec{
					Action: v1alpha2.StorageClusterOpsAction(action),
				},
			}
			var back StorageClusterOps
			if err := back.ConvertFrom(src); err != nil {
				t.Fatalf("ConvertFrom: %v", err)
			}
			return back.Spec.Action
		},
	)
}

// An action in neither table is carried through as written. Each version's Enum
// marker already refuses what it does not accept, and a conversion that errors
// makes the object unreadable rather than invalid.
func TestStorageClusterOpsUnknownActionPassesThrough(t *testing.T) {
	src := &StorageClusterOps{Spec: StorageClusterOpsSpec{Action: "teleport"}}

	var hub v1alpha2.StorageClusterOps
	if err := src.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if got := string(hub.Spec.Action); got != "teleport" {
		t.Errorf("action = %q, want it passed through as %q", got, "teleport")
	}
}

func TestStorageClusterOpsConvertToRenamesRollingRestart(t *testing.T) {
	src := &StorageClusterOps{
		Spec: StorageClusterOpsSpec{
			ClusterRef:         "production",
			Action:             "node-rolling-restart",
			NodeRollingRestart: &NodeRollingRestartSpec{RefreshSNodeAPI: true},
		},
		Status: StorageClusterOpsStatus{
			NodeRollingRestartStatus: &NodeRollingRestartStatus{
				PendingNodes:   []string{"node-b"},
				ProcessedNodes: []string{"node-a"},
				NodePhase:      "restarting",
				PhaseTriggered: true,
			},
		},
	}

	var dst v1alpha2.StorageClusterOps
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if dst.Spec.RollingRestart == nil {
		t.Fatal("spec.rollingRestart is absent")
	}
	if !dst.Spec.RollingRestart.RefreshSNodeAPI {
		t.Error("spec.rollingRestart.refreshSNodeAPI was not carried")
	}
	if dst.Status.RollingRestart == nil {
		t.Fatal("status.rollingRestart is absent")
	}
	if got := dst.Status.RollingRestart.NodePhase; got != "restarting" {
		t.Errorf("status.rollingRestart.nodePhase = %q, want %q", got, "restarting")
	}
	if diff := cmp.Diff([]string{"node-a"}, dst.Status.RollingRestart.ProcessedNodes); diff != "" {
		t.Errorf("processedNodes (-want +got):\n%s", diff)
	}
}

// An absent rolling-restart block must stay absent rather than becoming an empty
// struct, so that an operation of another action does not gain a block it never
// had.
func TestStorageClusterOpsConvertToLeavesRollingRestartAbsent(t *testing.T) {
	src := &StorageClusterOps{Spec: StorageClusterOpsSpec{Action: "activate"}}

	var dst v1alpha2.StorageClusterOps
	if err := src.ConvertTo(&dst); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if dst.Spec.RollingRestart != nil {
		t.Errorf("spec.rollingRestart = %+v, want nil", dst.Spec.RollingRestart)
	}
	if dst.Status.RollingRestart != nil {
		t.Errorf("status.rollingRestart = %+v, want nil", dst.Status.RollingRestart)
	}
}

func TestStorageClusterOpsRoundTripsThroughTheHub(t *testing.T) {
	started := metav1.Now()

	obj := &StorageClusterOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "sb"},
		Spec: StorageClusterOpsSpec{
			ClusterRef:         "production",
			Action:             "node-rolling-restart",
			NodeRollingRestart: &NodeRollingRestartSpec{RefreshSNodeAPI: true},
		},
		Status: StorageClusterOpsStatus{
			Phase:       StorageClusterOpsPhaseRunning,
			Triggered:   true,
			Message:     "restarting node-b",
			StartedAt:   &started,
			CompletedAt: &started,
			NodeRollingRestartStatus: &NodeRollingRestartStatus{
				PendingNodes:   []string{"node-b", "node-c"},
				ProcessedNodes: []string{"node-a"},
				NodePhase:      "restarting",
				PhaseTriggered: true,
			},
		},
	}

	var hub v1alpha2.StorageClusterOps
	if err := obj.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	var back StorageClusterOps
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	if diff := cmp.Diff(obj, &back); diff != "" {
		t.Errorf("round trip changed the object (-before +after):\n%s", diff)
	}
}
