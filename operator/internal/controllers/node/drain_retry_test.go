// What a drain remembers about a move that failed, and what it does with it.
//
// A failed move is kept rather than deleted, because it is the record of where
// the volume could not go: the target it was headed for, and whether the
// control plane had accepted it before it failed. The replacement is chosen
// with that record in hand, and a restart reads the same record back from the
// objects, so the memory survives without a field of its own.
//
// design-storagenode.md §8.2 and §8.4.

package node

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/tools/events"

	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

const otherPeerID = "node-3333"

// failedMove is a move of pv-1 that failed on its way to the first peer.
func failedMove(name string, engaged bool, message string) vmigration.Move {
	return vmigration.Move{
		Name: name, PVName: "pv-1", Phase: vmigration.MoveFailed,
		TargetNodeUUID: opsPeerID, Engaged: engaged, Message: message,
	}
}

// retrying runs the step a few times, the way the controller re-enters it,
// and returns the last error.
func retrying(t *testing.T, r *StorageNodeOpsReconciler, passes int) error {
	t.Helper()
	var err error
	for range passes {
		if _, err = performing(t, r, aDrain(), stepMigratingVolumes); err != nil {
			return err
		}
	}
	return err
}

// Regression: 2026-10-05-drain-target-memory — a failed move was deleted and
// replaced against the first eligible peer, which is the peer it had just
// failed on, so the replacement went straight back there.
func TestAFailedMovesReplacementAvoidsTheTargetItFailedOn(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		withPeer(otherPeerID, nodeStatusOnline).
		holding(onNode("volume-1", "pvc-1"))
	failed := failedMove(migrationName(opsNodeID, "pv-1"), true, "the copy failed")
	mover := &scriptedMover{moves: []vmigration.Move{failed}}
	r, _ := aDraining(t, api, mover, aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	if err := retrying(t, r, 2); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if len(mover.started) == 0 {
		t.Fatal("no replacement was raised for the failed move")
	}
	for _, request := range mover.started {
		if request.TargetNodeUUID != otherPeerID {
			t.Errorf("the replacement is headed for %q, want %s: the move just failed on %s",
				request.TargetNodeUUID, otherPeerID, opsPeerID)
		}
		if request.Name == failed.Name {
			t.Errorf("the replacement reuses the failed move's name %q, so it would never be created",
				request.Name)
		}
	}
}

// Regression: 2026-10-05-drain-target-memory — a target the control plane
// refuses by name is one it will refuse again, whether or not the migration
// was ever created.
func TestARefusalNamingTheTargetBurnsIt(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		withPeer(otherPeerID, nodeStatusOnline).
		holding(onNode("volume-1", "pvc-1"))
	mover := &scriptedMover{moves: []vmigration.Move{failedMove(migrationName(opsNodeID, "pv-1"), false,
		"the control plane refused to migrate the volume to node "+opsPeerID+": Cannot migrate to node "+
			opsPeerID+": it serves as the fallback source")}}
	r, _ := aDraining(t, api, mover, aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	if err := retrying(t, r, 2); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if len(mover.started) == 0 || mover.started[0].TargetNodeUUID != otherPeerID {
		t.Errorf("moves raised = %+v, want the replacement headed for %s", mover.started, otherPeerID)
	}
}

// A failure that never reached the target, such as a migration the cluster
// would not accept in time, says little about the target. It is retried there,
// and only a repeated one rules the target out.
func TestAnUnattributedFailureDoesNotRuleOutItsTarget(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		withPeer(otherPeerID, nodeStatusOnline).
		holding(onNode("volume-1", "pvc-1"))
	mover := &scriptedMover{moves: []vmigration.Move{
		failedMove("drain-a", false, "the cluster did not accept the migration in time"),
		failedMove("drain-b", false, "the cluster did not accept the migration in time"),
	}}
	r, _ := aDraining(t, api, mover, aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	if err := retrying(t, r, 1); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if len(mover.started) != 1 || mover.started[0].TargetNodeUUID != opsPeerID {
		t.Errorf("moves raised = %+v, want the replacement still allowed onto %s after two "+
			"failures that never reached it", mover.started, opsPeerID)
	}
}

// Regression: 2026-10-05-drain-target-memory — without a bound, failures that
// never reach the target would be retried against it for the whole step.
func TestAThirdUnattributedFailureRulesOutItsTarget(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		withPeer(otherPeerID, nodeStatusOnline).
		holding(onNode("volume-1", "pvc-1"))
	mover := &scriptedMover{moves: []vmigration.Move{
		failedMove("drain-a", false, "the cluster did not accept the migration in time"),
		failedMove("drain-b", false, "the cluster did not accept the migration in time"),
		failedMove("drain-c", false, "the cluster did not accept the migration in time"),
	}}
	r, _ := aDraining(t, api, mover, aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	if err := retrying(t, r, 2); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if len(mover.started) == 0 || mover.started[0].TargetNodeUUID != otherPeerID {
		t.Errorf("moves raised = %+v, want the replacement headed for %s after three failures on %s",
			mover.started, otherPeerID, opsPeerID)
	}
}

// Regression: 2026-10-05-drain-target-memory — an aborted move counted as one
// still running, so the drain waited on it until the step's deadline.
func TestAnAbortedMoveIsReissuedWithoutBlamingItsTarget(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		holding(onNode("volume-1", "pvc-1"))
	aborted := vmigration.Move{
		Name: migrationName(opsNodeID, "pv-1"), PVName: "pv-1", Phase: vmigration.MoveAborted,
		TargetNodeUUID: opsPeerID, Engaged: true,
	}
	mover := &scriptedMover{moves: []vmigration.Move{aborted}}
	r, _ := aDraining(t, api, mover, aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	if err := retrying(t, r, 2); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if len(mover.started) != 1 || mover.started[0].TargetNodeUUID != opsPeerID {
		t.Errorf("moves raised = %+v, want the aborted move re-issued, %s still allowed", mover.started, opsPeerID)
	}
	if !announced(r.Recorder.(*events.FakeRecorder), MigrationRetried) {
		t.Error("nothing announced that the aborted move was re-issued")
	}
}

// Regression: 2026-10-05-drain-target-memory — with every peer ruled out the
// drain sent the volume back to a peer it had already failed on. It holds
// instead, naming what it tried, so a node added meanwhile lets it go on.
func TestADrainWithEveryPeerRuledOutHoldsAndNamesThem(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		holding(onNode("volume-1", "pvc-1"))
	mover := &scriptedMover{moves: []vmigration.Move{
		failedMove(migrationName(opsNodeID, "pv-1"), true, "the copy failed"),
	}}
	r, _ := aDraining(t, api, mover, aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	err := retrying(t, r, 2)

	var blocked *blockedStepError
	if !errors.As(err, &blocked) || blocked.reason != NoMigrationTarget {
		t.Fatalf("err = %v, want the drain held for having no target left", err)
	}
	if !strings.Contains(blocked.message, opsPeerID) {
		t.Errorf("message = %q, want it to name the target already tried", blocked.message)
	}
	if len(mover.started) != 0 {
		t.Errorf("%d moves were raised onto a peer the volume already failed on", len(mover.started))
	}
}

// Regression: 2026-10-05-drain-target-memory — failed moves are kept as the
// record while the drain runs, and reaped on the pass that finds the node
// empty, which is the pass that finishes the step.
func TestFailedMovesAreReapedWhenTheNodeHoldsNothingMovable(t *testing.T) {
	api := aControlPlane().withPeer(opsPeerID, nodeStatusOnline)
	mover := &scriptedMover{moves: []vmigration.Move{
		failedMove(migrationName(opsNodeID, "pv-1"), true, "the copy failed"),
	}}
	r, _ := aDraining(t, api, mover)

	done, err := performing(t, r, aDrain(), stepMigratingVolumes)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if !done {
		t.Error("the step did not finish against an empty node with only a failed move left")
	}
	if len(mover.deleted) != 1 {
		t.Errorf("%d failed moves were reaped, want the one left over", len(mover.deleted))
	}
}

// Regression: 2026-10-06-drain-inherits-another-drains-failures — moves are
// found by the drained node's label, so a drain of a node an earlier drain had
// given up on inherited that drain's failed moves: their targets were ruled
// out and their attempts counted for a drain that had never tried them. A
// failed move another drain raised is that drain's record, and it is reaped.
func TestAnotherDrainsFailedMovesAreNeitherRememberedNorKept(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		withPeer(otherPeerID, nodeStatusOnline).
		holding(onNode("volume-1", "pvc-1"))
	earlier := failedMove(migrationName(opsNodeID, "pv-1"), true, "the copy failed")
	earlier.CreatorUID = "uid-of-an-earlier-drain"
	mover := &scriptedMover{moves: []vmigration.Move{earlier}}
	r, _ := aDraining(t, api, mover, aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	if err := retrying(t, r, 2); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if !slices.Contains(mover.deleted, earlier.Name) {
		t.Error("the earlier drain's failed move was kept as if it were this drain's record")
	}
	if len(mover.started) != 1 || mover.started[0].TargetNodeUUID != opsPeerID {
		t.Errorf("moves raised = %+v, want one headed for %s, which this drain has never tried",
			mover.started, opsPeerID)
	}
}
