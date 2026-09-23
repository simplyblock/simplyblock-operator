// Draining a node before it leaves.
//
// Removing a storage node destroys it, so every logical volume whose data lives
// on it has to be somewhere else first. The five steps are ordered around that
// one fact: validation runs before the suspend, because suspending a node whose
// drain cannot complete takes capacity out of the cluster and leaves it out for
// as long as the blocker goes unnoticed; verification runs after the migration,
// because the census is the authority on what is left rather than the counter of
// what moved; and the removal is the last step rather than the operation.
//
// design-storagenode.md §8.

package node

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// scriptedMover is the fan-out as a drain sees it: whatever moves the test says
// are outstanding, plus whatever the drain raised during the case.
//
// The kind that carries a move is the deployment's and not the drain's, which is
// exactly why this is an interface the drain talks through and why a fake here
// is a fair stand-in for either one.
type scriptedMover struct {
	moves   []vmigration.Move
	started []vmigration.MoveRequest
	deleted []string
}

func (m *scriptedMover) Start(_ context.Context, request vmigration.MoveRequest) error {
	m.started = append(m.started, request)
	return nil
}

func (m *scriptedMover) List(
	_ context.Context, _ string, _ map[string]string,
) ([]vmigration.Move, error) {
	return m.moves, nil
}

func (m *scriptedMover) Get(_ context.Context, name, _ string) (vmigration.Move, error) {
	for _, move := range m.moves {
		if move.Name == name {
			return move, nil
		}
	}
	return vmigration.Move{}, errors.New("no such move")
}

func (m *scriptedMover) Delete(_ context.Context, move vmigration.Move) error {
	m.deleted = append(m.deleted, move.Name)
	remaining := m.moves[:0]
	for _, existing := range m.moves {
		if existing.Name != move.Name {
			remaining = append(remaining, existing)
		}
	}
	m.moves = remaining
	return nil
}

// aDrain is the operation these cases run.
func aDrain() *simplyblockv1alpha2.StorageNodeOps {
	return anOperation("a-drain", simplyblockv1alpha2.StorageNodeOpsActionRemove)
}

// aDraining builds the world, with the drain object already in it so that its
// status can be written and read back.
func aDraining(
	t *testing.T, api *scriptedControlPlane, mover *scriptedMover, objects ...client.Object,
) (*StorageNodeOpsReconciler, client.Client) {
	t.Helper()
	r, apiClient := anOpsWorld(t, api, append(objects, aDrain())...)
	r.Mover = mover
	return r, apiClient
}

// A pinned claim stops the drain where nothing has been done yet, and says which
// annotation to remove from which volume. Blocking here rather than later is
// what leaves the node fully operational while somebody decides.
func TestAPinnedVolumeStopsTheDrainBeforeItSuspendsAnything(t *testing.T) {
	api := aControlPlane().holding(onNode("volume-1", "pvc-abc"))
	r, _ := aDraining(t, api, &scriptedMover{},
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", true))

	_, err := r.perform(context.Background(), aDrain(), stepValidating)

	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want the drain held by the pin", err)
	}
	if blocked.reason != DrainBlocked {
		t.Errorf("the hold is announced as %q, want %q", blocked.reason, DrainBlocked)
	}
	if asked := api.asked("Suspend"); asked != 0 {
		t.Errorf("the node was suspended %d time(s) by a drain that cannot finish", asked)
	}
}

// A volume nothing in Kubernetes accounts for blocks the same way, and for the
// stronger reason: there is no object to move and deleting it would destroy data
// nobody is tracking.
func TestAnUnmanagedVolumeStopsTheDrain(t *testing.T) {
	api := aControlPlane().holding(onNode("volume-orphan", "hand-made"))
	r, _ := aDraining(t, api, &scriptedMover{})

	_, err := r.perform(context.Background(), aDrain(), stepValidating)

	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want the drain held by the volume nothing accounts for", err)
	}
}

// Validation fixes the total every later step reports progress against, and
// writes it once. A node with nothing movable on it is a total of zero rather
// than an absent one.
func TestValidationWritesTheTotalTheDrainIsMeasuredAgainst(t *testing.T) {
	api := aControlPlane().holding(onNode("volume-1", "pvc-abc"), onNode("volume-2", "pvc-def"))
	r, apiClient := aDraining(t, api, &scriptedMover{},
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false),
		aPersistentVolume("pv-2", "volume-2"), aClaim("pv-2", false))

	done, err := r.perform(context.Background(), aDrain(), stepValidating)
	if err != nil {
		t.Fatalf("validating: %v", err)
	}
	if !done {
		t.Error("validation did not finish although nothing blocks the drain")
	}

	got := operationRead(t, apiClient, "a-drain")
	if got.Status.Drain == nil || got.Status.Drain.VolumesTotal != 2 {
		t.Errorf("drain = %+v, want the two movable volumes counted", got.Status.Drain)
	}
	if got.Status.Drain.VolumesMigrated != 0 {
		t.Errorf("%d volumes are already recorded as moved before any move was raised",
			got.Status.Drain.VolumesMigrated)
	}
}

// A census that could not be completed is retried rather than acted on: a claim
// that could not be read put a volume where it blocks, and reporting that as a
// blocker would name a volume that is in fact accounted for.
func TestAnIncompleteCensusIsRetriedRatherThanReportedAsABlocker(t *testing.T) {
	api := aControlPlane().holding(onNode("volume-1", "pvc-abc"))
	r, _ := anOpsWorldWith(t, api, refusingClaims(),
		aDrain(), aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	_, err := r.perform(context.Background(), aDrain(), stepValidating)
	if err == nil {
		t.Fatal("the drain acted on a census it knows is incomplete")
	}
	var blocked *blockedStepError
	if errors.As(err, &blocked) {
		t.Errorf("err = %v, want an ordinary retry rather than a hold naming a volume", err)
	}
}

// The suspend is skipped against a node already at or past where it would put
// it, which is what makes re-entering the step after a lost response harmless.
func TestTheSuspendIsSkippedWhenTheNodeIsAlreadyOutOfService(t *testing.T) {
	for _, status := range []string{nodeStatusSuspended, nodeStatusOffline} {
		t.Run(status, func(t *testing.T) {
			api := aControlPlane().reporting(status)
			r, _ := aDraining(t, api, &scriptedMover{})

			done, err := r.perform(context.Background(), aDrain(), stepSuspending)
			if err != nil {
				t.Fatalf("suspending: %v", err)
			}
			if !done {
				t.Errorf("the step did not finish against a node already %s", status)
			}
			if asked := api.asked("Suspend"); asked != 0 {
				t.Errorf("Suspend was issued %d time(s) against a node already %s", asked, status)
			}
		})
	}
}

// An online node is suspended, and the step does not finish on the call: it
// finishes when the control plane reports the node suspended.
func TestAnOnlineNodeIsSuspendedAndWaitedFor(t *testing.T) {
	api := aControlPlane()
	r, _ := aDraining(t, api, &scriptedMover{})

	done, err := r.perform(context.Background(), aDrain(), stepSuspending)
	if err != nil {
		t.Fatalf("suspending: %v", err)
	}
	if done {
		t.Error("the step finished on the call rather than on the node reporting suspended")
	}
	if asked := api.asked("Suspend"); asked != 1 {
		t.Errorf("Suspend was issued %d time(s), want once", asked)
	}
}

// Every movable volume gets a move, named so that a second pass finds the object
// it made rather than making another.
func TestEveryMovableVolumeIsGivenAMove(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		holding(onNode("volume-1", "pvc-abc"), onNode("volume-2", "pvc-def"))
	mover := &scriptedMover{}
	r, _ := aDraining(t, api, mover,
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false),
		aPersistentVolume("pv-2", "volume-2"), aClaim("pv-2", false))

	done, err := r.perform(context.Background(), aDrain(), stepMigratingVolumes)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if done {
		t.Error("the step finished on the pass that raised the moves")
	}
	if len(mover.started) != 2 {
		t.Fatalf("%d moves were raised for two movable volumes", len(mover.started))
	}
	for _, request := range mover.started {
		if request.TargetNodeUUID != opsPeerID {
			t.Errorf("%s is being moved to %q, want the cluster's one online peer",
				request.PVName, request.TargetNodeUUID)
		}
		if request.Name != migrationName(opsNodeID, request.PVName) {
			t.Errorf("the move is named %q, and a second pass would raise it again",
				request.Name)
		}
		if request.Labels[drainNodeLabel] != opsNodeID {
			t.Errorf("the move carries %v, and the drain could not find its own fan-out",
				request.Labels)
		}
	}
}

// A move already raised is not raised again, which is what makes the step safe
// to re-enter on every pass while the copies run.
func TestAVolumeAlreadyMovingIsNotGivenASecondMove(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		holding(onNode("volume-1", "pvc-abc"))
	mover := &scriptedMover{moves: []vmigration.Move{{
		Name: migrationName(opsNodeID, "pv-1"), PVName: "pv-1", Phase: vmigration.MoveRunning,
	}}}
	r, apiClient := aDraining(t, api, mover,
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	done, err := r.perform(context.Background(), aDrain(), stepMigratingVolumes)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if done {
		t.Error("the step finished while a move was still running")
	}
	if len(mover.started) != 0 {
		t.Errorf("%d further moves were raised for a volume already moving", len(mover.started))
	}

	got := operationRead(t, apiClient, "a-drain")
	if got.Status.Drain == nil || got.Status.Drain.VolumesMigrated != 0 {
		t.Errorf("drain = %+v, want nothing recorded as moved while the move runs",
			got.Status.Drain)
	}
}

// A failed move is deleted and replaced against a fresh target rather than
// failing the drain: the volume is still on the node, and the peer that could
// not take it is not the only peer.
func TestAFailedMoveIsRetriedRatherThanFailingTheDrain(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		holding(onNode("volume-1", "pvc-abc"))
	mover := &scriptedMover{moves: []vmigration.Move{{
		Name:    migrationName(opsNodeID, "pv-1"),
		PVName:  "pv-1",
		Phase:   vmigration.MoveFailed,
		Message: "the target refused the copy",
	}}}
	r, _ := aDraining(t, api, mover,
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))
	ops := aDrain()

	done, err := r.perform(context.Background(), ops, stepMigratingVolumes)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if done {
		t.Error("the step finished although a volume is still on the node")
	}
	if len(mover.deleted) != 1 {
		t.Errorf("the failed move was deleted %d time(s), so nothing would replace it",
			len(mover.deleted))
	}
	if !announced(r.Recorder.(*events.FakeRecorder), MigrationRetried) {
		t.Error("nothing announced the retry, so a drain that keeps retrying looks like one that stalled")
	}

	// The next pass raises it again, against a target chosen afresh.
	if _, err := r.perform(context.Background(), ops, stepMigratingVolumes); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if len(mover.started) != 1 {
		t.Errorf("%d moves were raised on the pass after the retry, want the replacement",
			len(mover.started))
	}
}

// Every move finished is progress recorded and the objects reaped, and the
// counter is written before the delete so that a crash between the two leaves
// the progress recorded rather than lost.
func TestFinishedMovesAreRecordedAndThenReaped(t *testing.T) {
	api := aControlPlane().withPeer(opsPeerID, nodeStatusOnline)
	mover := &scriptedMover{moves: []vmigration.Move{
		{Name: migrationName(opsNodeID, "pv-1"), PVName: "pv-1", Phase: vmigration.MoveSucceeded},
		{Name: migrationName(opsNodeID, "pv-2"), PVName: "pv-2", Phase: vmigration.MoveSucceeded},
	}}
	r, apiClient := aDraining(t, api, mover)

	done, err := r.perform(context.Background(), aDrain(), stepMigratingVolumes)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if done {
		t.Error("the step finished on the pass that reaped the moves rather than on a fresh census")
	}

	got := operationRead(t, apiClient, "a-drain")
	if got.Status.Drain == nil || got.Status.Drain.VolumesMigrated != 2 {
		t.Errorf("drain = %+v, want both moves recorded", got.Status.Drain)
	}
	if len(mover.deleted) != 2 {
		t.Errorf("%d of two finished moves were reaped, and a hundred-volume drain leaves "+
			"a hundred objects behind", len(mover.deleted))
	}
}

// Nothing movable left and nothing outstanding is a drain that is done, and the
// census is the authority on that rather than the counter: the counter records
// what this operation did, and the census is what is on the node.
func TestADrainIsDoneWhenTheNodeHoldsNothingMovable(t *testing.T) {
	r, _ := aDraining(t, aControlPlane().withPeer(opsPeerID, nodeStatusOnline), &scriptedMover{})

	done, err := r.perform(context.Background(), aDrain(), stepMigratingVolumes)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if !done {
		t.Error("the step did not finish against a node with nothing left to move")
	}
	if !announced(r.Recorder.(*events.FakeRecorder), DrainCompleted) {
		t.Error("nothing announced that the node had been emptied")
	}
}

// Verification deletes the benchmark volumes the migration skipped, and does not
// call the node empty on the pass that deleted them: the control plane's
// deletion is asynchronous, so a later pass has to re-read.
func TestVerificationDeletesTheBenchmarkVolumesAndRereads(t *testing.T) {
	api := aControlPlane().holding(onNode("volume-bench", "sb-fio-baseline-read"))
	r, _ := aDraining(t, api, &scriptedMover{})

	done, err := r.perform(context.Background(), aDrain(), stepVerifying)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if done {
		t.Error("the step called the node empty on the pass that asked for the deletions")
	}
	if asked := api.asked("DeleteVolume"); asked != 1 {
		t.Errorf("DeleteVolume was issued %d time(s), want one per benchmark volume", asked)
	}
}

// A node that still holds a user's volume after the migration is not a node to
// remove, and the drain holds rather than destroying it.
func TestVerificationHoldsWhileAUsersVolumeIsStillThere(t *testing.T) {
	api := aControlPlane().holding(onNode("volume-1", "pvc-abc"))
	r, _ := aDraining(t, api, &scriptedMover{},
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false))

	_, err := r.perform(context.Background(), aDrain(), stepVerifying)

	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want the removal held while the node still holds a volume", err)
	}
}

// A benchmark volume that cannot be deleted is one the removal would destroy, so
// the operation fails rather than proceeding.
func TestABenchmarkVolumeThatCannotBeDeletedEndsTheDrain(t *testing.T) {
	api := aControlPlane().
		holding(onNode("volume-bench", "sb-fio-baseline-read")).
		refusing("DeleteVolume", errors.New("the control plane refused"))
	r, _ := aDraining(t, api, &scriptedMover{})

	_, err := r.perform(context.Background(), aDrain(), stepVerifying)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind: the node still holds the volume", err)
	}
}

// An empty node passes verification.
func TestAnEmptyNodePassesVerification(t *testing.T) {
	r, _ := aDraining(t, aControlPlane(), &scriptedMover{})

	done, err := r.perform(context.Background(), aDrain(), stepVerifying)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !done {
		t.Error("an empty node did not pass verification")
	}
}

// The removal is the last step, and a refusal is the control plane's answer
// about what the cluster can afford to lose. Retrying cannot change it, so the
// operation fails and the unwind puts the node back into service.
func TestARefusedRemovalEndsTheDrain(t *testing.T) {
	api := aControlPlane().refusing("RemoveNode", errors.New("the cluster cannot lose this node"))
	r, _ := aDraining(t, api, &scriptedMover{})

	_, err := r.perform(context.Background(), aDrain(), stepRemoving)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for a removal the control plane refused", err)
	}
}

// An accepted removal is the end of the drain.
func TestAnAcceptedRemovalFinishesTheDrain(t *testing.T) {
	api := aControlPlane()
	r, _ := aDraining(t, api, &scriptedMover{})

	done, err := r.perform(context.Background(), aDrain(), stepRemoving)
	if err != nil {
		t.Fatalf("removing: %v", err)
	}
	if !done {
		t.Error("the step did not finish although the removal was accepted")
	}
	if asked := api.asked("RemoveNode"); asked != 1 {
		t.Errorf("RemoveNode was issued %d time(s), want once", asked)
	}
}

// Deleting a drain stops what it fanned out, so nothing keeps copying for an
// operation that no longer exists.
func TestDeletingADrainStopsWhatItFannedOut(t *testing.T) {
	mover := &scriptedMover{moves: []vmigration.Move{{
		Name: migrationName(opsNodeID, "pv-1"), PVName: "pv-1", Phase: vmigration.MoveSucceeded,
	}}}
	r, _ := aDraining(t, aControlPlane(), mover)

	pending, err := r.cascadeMigrations(context.Background(), aDrain())
	if err != nil {
		t.Fatalf("cascading: %v", err)
	}
	if pending {
		t.Error("a finished move was reported as still running")
	}
	if len(mover.deleted) != 1 {
		t.Errorf("%d moves were reaped, want the drain's whole fan-out", len(mover.deleted))
	}
}

// A move still running holds the deletion, because the cascade's own deletes
// have to be admissible before the operation goes.
func TestADrainBeingDeletedWaitsForAMoveStillRunning(t *testing.T) {
	mover := &scriptedMover{moves: []vmigration.Move{{
		Name: migrationName(opsNodeID, "pv-1"), PVName: "pv-1", Phase: vmigration.MoveRunning,
	}}}
	r, _ := aDraining(t, aControlPlane(), mover)

	pending, err := r.cascadeMigrations(context.Background(), aDrain())
	if err != nil {
		t.Fatalf("cascading: %v", err)
	}
	if !pending {
		t.Error("a move still copying was not reported as pending")
	}
	if len(mover.deleted) != 0 {
		t.Errorf("%d moves were reaped mid-copy", len(mover.deleted))
	}
}
