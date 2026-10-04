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
//
// The walk's two shapes are the other half. The hub states one immutable list
// and an index into it; this version states two lists it drains from one into
// the other. They carry the same information, and the tests below assert that
// in both directions rather than trusting the arithmetic.

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
	if got := dst.Status.Step.State; got != string(v1alpha2.StorageClusterOpsStepRestartingNode) {
		t.Errorf("status.step.state = %q, want %q",
			got, v1alpha2.StorageClusterOpsStepRestartingNode)
	}
	// The walk keeps the order it was planned in: what is done, then what is
	// left, with the index between them.
	if diff := cmp.Diff([]string{"node-a", "node-b"}, dst.Status.RollingRestart.Nodes); diff != "" {
		t.Errorf("nodes (-want +got):\n%s", diff)
	}
	if got := dst.Status.RollingRestart.NodeIndex; got != 1 {
		t.Errorf("nodeIndex = %d, want 1", got)
	}
}

// A walk that has finished every node puts the index at the end of the list,
// which is what completion means. Reading it back must leave nothing pending,
// because a pending node is one the operation would restart again.
func TestStorageClusterOpsAFinishedWalkHasNothingPending(t *testing.T) {
	hub := &v1alpha2.StorageClusterOps{
		Spec: v1alpha2.StorageClusterOpsSpec{
			Action: v1alpha2.StorageClusterOpsActionRollingRestart,
		},
		Status: v1alpha2.StorageClusterOpsStatus{
			RollingRestart: &v1alpha2.RollingRestartStatus{
				Nodes:     []string{"node-a", "node-b"},
				NodeIndex: 2,
			},
		},
	}

	var back StorageClusterOps
	if err := back.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	walk := back.Status.NodeRollingRestartStatus
	if walk == nil {
		t.Fatal("status.nodeRollingRestartStatus is absent")
	}
	if len(walk.PendingNodes) != 0 {
		t.Errorf("pendingNodes = %v, want none", walk.PendingNodes)
	}
	if diff := cmp.Diff([]string{"node-a", "node-b"}, walk.ProcessedNodes); diff != "" {
		t.Errorf("processedNodes (-want +got):\n%s", diff)
	}
}

// Aborted is the one phase this version's Enum does not accept, so writing it
// through would make the stored object rejected at admission rather than merely
// odd. It narrows to Failed and comes back as itself.
func TestStorageClusterOpsAbortedNarrowsAndIsRestored(t *testing.T) {
	hub := &v1alpha2.StorageClusterOps{
		Spec:   v1alpha2.StorageClusterOpsSpec{Action: v1alpha2.StorageClusterOpsActionShutdown},
		Status: v1alpha2.StorageClusterOpsStatus{Phase: v1alpha2.StorageClusterOpsPhaseAborted},
	}

	var stored StorageClusterOps
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if stored.Status.Phase != StorageClusterOpsPhaseFailed {
		t.Errorf("stored phase = %q, want %q", stored.Status.Phase, StorageClusterOpsPhaseFailed)
	}

	var back v1alpha2.StorageClusterOps
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if back.Status.Phase != v1alpha2.StorageClusterOpsPhaseAborted {
		t.Errorf("restored phase = %q, want %q",
			back.Status.Phase, v1alpha2.StorageClusterOpsPhaseAborted)
	}
}

// The CancelTask action has no v1alpha1 spelling, and its parameter block has
// nowhere to be stored. Both survive the round trip: the action passes through
// and is refused by this version's own Enum, and the block is stashed.
func TestStorageClusterOpsCancelTaskSurvivesStorage(t *testing.T) {
	hub := &v1alpha2.StorageClusterOps{
		Spec: v1alpha2.StorageClusterOpsSpec{
			ClusterRef: "production",
			Action:     v1alpha2.StorageClusterOpsActionCancelTask,
			CancelTask: &v1alpha2.CancelTaskSpec{TaskID: "task-uuid"},
		},
	}

	var stored StorageClusterOps
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StorageClusterOps
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if back.Spec.CancelTask == nil || back.Spec.CancelTask.TaskID != "task-uuid" {
		t.Errorf("spec.cancelTask = %+v, want the task it named", back.Spec.CancelTask)
	}
	if back.Spec.Action != v1alpha2.StorageClusterOpsActionCancelTask {
		t.Errorf("action = %q, want it carried through", back.Spec.Action)
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
