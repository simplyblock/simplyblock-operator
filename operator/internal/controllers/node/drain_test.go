// Draining a node before it leaves.
//
// Removing a storage node destroys it, so every logical volume whose data lives
// on it has to be somewhere else first. The steps are ordered around that one
// fact. Validation runs before prepare-removal, because there is no way back from
// it. Verification runs after the migration, because the census is the authority
// on what is left rather than the counter of what moved. The removal is the last
// step rather than the operation.
//
// design-storagenode.md §8.

package node

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
func TestAPinnedVolumeStopsTheDrainBeforeItTouchesTheNode(t *testing.T) {
	api := aControlPlane().holding(onNode("volume-1", "pvc-abc"))
	r, _ := aDraining(t, api, &scriptedMover{},
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", true))

	_, err := performing(t, r, aDrain(), stepValidating)

	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want the drain held by the pin", err)
	}
	if blocked.reason != DrainBlocked {
		t.Errorf("the hold is announced as %q, want %q", blocked.reason, DrainBlocked)
	}
	if asked := api.asked("PrepareRemoval"); asked != 0 {
		t.Errorf("prepare-removal was sent %d time(s) for a drain that cannot finish", asked)
	}
}

// A volume nothing in Kubernetes accounts for blocks the same way, and for the
// stronger reason: there is no object to move and deleting it would destroy data
// nobody is tracking.
func TestAnUnmanagedVolumeStopsTheDrain(t *testing.T) {
	api := aControlPlane().holding(onNode("volume-orphan", "hand-made"))
	r, _ := aDraining(t, api, &scriptedMover{})

	_, err := performing(t, r, aDrain(), stepValidating)

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

	done, err := performing(t, r, aDrain(), stepValidating)
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

	_, err := performing(t, r, aDrain(), stepValidating)
	if err == nil {
		t.Fatal("the drain acted on a census it knows is incomplete")
	}
	var blocked *blockedStepError
	if errors.As(err, &blocked) {
		t.Errorf("err = %v, want an ordinary retry rather than a hold naming a volume", err)
	}
}

// Regression: 2026-10-02-removal-three-steps: the drain suspended the node and
// left it serving, while the control plane's removal expects it shut down and
// its devices rebuilt before the volumes move. The first step is prepare-removal,
// which admits the node and shuts it down if it is still running, and the
// operator issues no shutdown of its own.
func TestShuttingDownAsksTheControlPlaneToPrepareTheRemoval(t *testing.T) {
	for _, status := range []string{nodeStatusOnline, nodeStatusSuspended, nodeStatusOffline} {
		t.Run(status, func(t *testing.T) {
			api := aControlPlane().reporting(status)
			r, _ := aDraining(t, api, &scriptedMover{})

			done, err := performing(t, r, aDrain(), stepShuttingDown)
			if err != nil {
				t.Fatalf("shutting down: %v", err)
			}
			if !done {
				t.Errorf("the step did not finish although prepare-removal was accepted for a node %s",
					status)
			}
			if asked := api.asked("PrepareRemoval"); asked != 1 {
				t.Errorf("PrepareRemoval was issued %d time(s) against a node %s, want once",
					asked, status)
			}
			for _, call := range []string{"ShutdownNode", "Suspend"} {
				if asked := api.asked(call); asked != 0 {
					t.Errorf("%s was issued %d time(s); the control plane shuts the node down itself",
						call, asked)
				}
			}
		})
	}
}

// Regression: 2026-10-02-removal-three-steps: a node already shutting down is
// somebody else's shutdown, which prepare-removal refuses to run under, and a
// node already admitted needs no second admission.
func TestShuttingDownSendsNothingToANodeAlreadyOnItsWay(t *testing.T) {
	for status, wantDone := range map[string]bool{
		nodeStatusInShutdown:       false,
		nodeStatusPendingRemoval:   true,
		nodeStatusMigratingDevices: true,
		nodeStatusMigratingLvols:   true,
	} {
		t.Run(status, func(t *testing.T) {
			api := aControlPlane().reporting(status)
			r, _ := aDraining(t, api, &scriptedMover{})

			done, err := performing(t, r, aDrain(), stepShuttingDown)
			if err != nil {
				t.Fatalf("shutting down: %v", err)
			}
			if done != wantDone {
				t.Errorf("done = %t against a node %s, want %t", done, status, wantDone)
			}
			if asked := api.asked("PrepareRemoval"); asked != 0 {
				t.Errorf("PrepareRemoval was issued %d time(s) against a node %s, want none",
					asked, status)
			}
		})
	}
}

// Regression: 2026-10-02-removal-three-steps: a refused admission is the control
// plane saying the cluster cannot lose this node, and it changed nothing.
func TestARefusedPrepareRemovalEndsTheDrain(t *testing.T) {
	api := aControlPlane().refusing("PrepareRemoval", &ControlPlaneError{
		Status: http.StatusBadRequest, Body: `{"detail":"Can not remove node: FTT"}`,
	})
	r, _ := aDraining(t, api, &scriptedMover{})

	_, err := performing(t, r, aDrain(), stepShuttingDown)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for a refused admission", err)
	}
}

// Regression: 2026-10-02-removal-three-steps: prepare-removal shuts the node down
// before it answers, which can outlast the client's timeout. No answer is not a
// refusal, and the next pass reads the node.
func TestAPrepareRemovalWithNoAnswerIsRetried(t *testing.T) {
	api := aControlPlane().refusing("PrepareRemoval",
		fmt.Errorf("http error: %w", context.DeadlineExceeded))
	r, _ := aDraining(t, api, &scriptedMover{})

	done, err := performing(t, r, aDrain(), stepShuttingDown)

	var fatal *terminalStepError
	if errors.As(err, &fatal) || done {
		t.Errorf("done, err = %t, %v; want a retry for a call the control plane never answered",
			done, err)
	}
}

// Regression: 2026-10-02-removal-three-steps: the device rebuild is the control
// plane's, and the step finishes when the control plane says it has.
func TestMigratingDevicesFinishesWhenTheRebuildIsDone(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusMigratingLvols)
	api.progress = RemovalProgress{Done: true, Total: 2, Completed: 2,
		NodeStatus: nodeStatusMigratingLvols}
	r, _ := aDraining(t, api, &scriptedMover{})

	done, err := performing(t, r, aDrain(), stepMigratingDevices)
	if err != nil {
		t.Fatalf("migrating devices: %v", err)
	}
	if !done {
		t.Error("the step did not finish although the control plane reports the rebuild done")
	}
}

// Regression: 2026-10-02-removal-three-steps: a rebuild still running is waited
// on, and prepare-removal is sent again, which the control plane treats as a
// no-op while the rebuild runs and as a restart of it when it has stopped.
func TestMigratingDevicesWaitsAndKeepsTheRebuildRunning(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusMigratingDevices)
	api.progress = RemovalProgress{Total: 2, Completed: 1, NodeStatus: nodeStatusMigratingDevices}
	r, _ := aDraining(t, api, &scriptedMover{})

	done, err := performing(t, r, aDrain(), stepMigratingDevices)
	if err != nil {
		t.Fatalf("migrating devices: %v", err)
	}
	if done {
		t.Error("the step finished while the rebuild is still running")
	}
	if asked := api.asked("PrepareRemoval"); asked != 1 {
		t.Errorf("PrepareRemoval was issued %d time(s), want once to keep the rebuild running", asked)
	}
}

// Regression: 2026-10-02-removal-three-steps: the node is still on its way down,
// and prepare-removal refuses to start the rebuild under a running shutdown.
func TestMigratingDevicesSendsNothingWhileTheNodeShutsDown(t *testing.T) {
	for _, status := range []string{nodeStatusPendingRemoval, nodeStatusInShutdown} {
		t.Run(status, func(t *testing.T) {
			api := aControlPlane().reporting(status)
			api.progress = RemovalProgress{Total: 2, NodeStatus: status}
			r, _ := aDraining(t, api, &scriptedMover{})

			done, err := performing(t, r, aDrain(), stepMigratingDevices)
			if err != nil {
				t.Fatalf("migrating devices: %v", err)
			}
			if done {
				t.Errorf("the step finished against a node %s", status)
			}
			if asked := api.asked("PrepareRemoval"); asked != 0 {
				t.Errorf("PrepareRemoval was issued %d time(s) against a node %s, want none",
					asked, status)
			}
		})
	}
}

// Regression: 2026-10-02-removal-three-steps: a rebuild the control plane gave
// up on is reported as a failure, and waiting longer cannot change it.
func TestMigratingDevicesFailsWhenTheRebuildGivesUp(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusMigratingDevices)
	api.progress = RemovalProgress{Total: 2, Completed: 1, Failed: 1,
		Message: "device 2 stalled", NodeStatus: nodeStatusMigratingDevices}
	r, _ := aDraining(t, api, &scriptedMover{})

	_, err := performing(t, r, aDrain(), stepMigratingDevices)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for a rebuild the control plane gave up on", err)
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

	done, err := performing(t, r, aDrain(), stepMigratingVolumes)
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

	done, err := performing(t, r, aDrain(), stepMigratingVolumes)
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

	done, err := performing(t, r, ops, stepMigratingVolumes)
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
	if _, err := performing(t, r, ops, stepMigratingVolumes); err != nil {
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

	done, err := performing(t, r, aDrain(), stepMigratingVolumes)
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

	done, err := performing(t, r, aDrain(), stepMigratingVolumes)
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

	done, err := performing(t, r, aDrain(), stepVerifying)
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

	_, err := performing(t, r, aDrain(), stepVerifying)

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

	_, err := performing(t, r, aDrain(), stepVerifying)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind: the node still holds the volume", err)
	}
}

// An empty node passes verification.
func TestAnEmptyNodePassesVerification(t *testing.T) {
	r, _ := aDraining(t, aControlPlane(), &scriptedMover{})

	done, err := performing(t, r, aDrain(), stepVerifying)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !done {
		t.Error("an empty node did not pass verification")
	}
}

// A refusal of the removal is the control plane's answer about what the cluster
// can afford to lose. Retrying cannot change it, so the operation fails.
func TestARefusedRemovalEndsTheDrain(t *testing.T) {
	api := aControlPlane().refusing("RemoveNode", &ControlPlaneError{
		Status: http.StatusBadRequest, Body: `{"detail":"the cluster cannot lose this node"}`,
	})
	r, _ := aDraining(t, api, &scriptedMover{})

	_, err := performing(t, r, aDrain(), stepRemoving)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for a removal the control plane refused", err)
	}
}

// Regression: 2026-10-02-remove-timeout-read-as-refusal: the DELETE that removes
// a node outlived the HTTP client's timeout while the control plane went on and
// removed the node. The drain read the timeout as a refusal, failed the
// operation, and tried to resume a node that was being removed.
func TestARemovalWithNoAnswerIsRetriedRatherThanFailed(t *testing.T) {
	cases := map[string]error{
		"a timeout": fmt.Errorf("http error: Delete %q: %w",
			"https://webappapi/storage-nodes/x", context.DeadlineExceeded),
		"a 5xx": &ControlPlaneError{Status: http.StatusBadGateway, Body: "bad gateway"},
	}
	for name, failure := range cases {
		t.Run(name, func(t *testing.T) {
			api := aControlPlane().refusing("RemoveNode", failure)
			r, _ := aDraining(t, api, &scriptedMover{})

			done, err := performing(t, r, aDrain(), stepRemoving)

			var fatal *terminalStepError
			if errors.As(err, &fatal) {
				t.Errorf("err = %v, want a retry for a removal the control plane never answered", err)
			}
			if done {
				t.Error("the step finished although the removal was never answered")
			}
		})
	}
}

// Regression: 2026-10-02-remove-timeout-read-as-refusal: the pass after a
// removal whose answer was lost finds the node already being removed. That is
// the removal accepted, and a second DELETE against it is not one to send.
func TestARemovalAlreadyUnderwayFinishesTheDrain(t *testing.T) {
	for _, status := range []string{
		nodeStatusInRemoval, nodeStatusRemoved, nodeStatusRemovedFailed,
	} {
		t.Run(status, func(t *testing.T) {
			api := aControlPlane().reporting(status)
			r, _ := aDraining(t, api, &scriptedMover{})

			done, err := performing(t, r, aDrain(), stepRemoving)
			if err != nil {
				t.Fatalf("removing: %v", err)
			}
			if !done {
				t.Errorf("the step did not finish although the node is already %s", status)
			}
			if asked := api.asked("RemoveNode"); asked != 0 {
				t.Errorf("RemoveNode was issued %d time(s) against a node already %s, want none",
					asked, status)
			}
		})
	}
}

// Regression: 2026-10-02-delete-skipped-after-prepare (PR #612 review): after
// prepare-removal the node is migrating_lvols, and Removing read every removal
// status as the DELETE already accepted. The DELETE was never sent, so the
// teardown was never asked for and AwaitingRemoval waited out its budget. A
// preparation status is not the DELETE, which is idempotent and is sent.
func TestAPreparedNodeIsStillDeleted(t *testing.T) {
	for _, status := range []string{
		nodeStatusMigratingLvols, nodeStatusMigratingDevices, nodeStatusPendingRemoval,
	} {
		t.Run(status, func(t *testing.T) {
			api := aControlPlane().reporting(status)
			r, _ := aDraining(t, api, &scriptedMover{})

			done, err := performing(t, r, aDrain(), stepRemoving)
			if err != nil {
				t.Fatalf("removing: %v", err)
			}
			if !done {
				t.Errorf("the step did not finish after the DELETE was accepted for a node %s", status)
			}
			if asked := api.asked("RemoveNode"); asked != 1 {
				t.Errorf("RemoveNode was issued %d time(s) against a node %s, want once",
					asked, status)
			}
		})
	}
}

// An accepted removal is the end of the drain.
func TestAnAcceptedRemovalFinishesTheDrain(t *testing.T) {
	api := aControlPlane()
	r, _ := aDraining(t, api, &scriptedMover{})

	done, err := performing(t, r, aDrain(), stepRemoving)
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

// Regression: 2026-10-02-remove-timeout-read-as-refusal: a node still shutting
// down is not yet proof that the removal was accepted, and a second DELETE while
// it shuts down is not one to send either. The step waits for the control plane
// to say which it was.
func TestARemovalStillShuttingTheNodeDownWaits(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusInShutdown)
	r, _ := aDraining(t, api, &scriptedMover{})

	done, err := performing(t, r, aDrain(), stepRemoving)
	if err != nil {
		t.Fatalf("removing: %v", err)
	}
	if done {
		t.Error("the step finished although the node is only shutting down")
	}
	if asked := api.asked("RemoveNode"); asked != 0 {
		t.Errorf("RemoveNode was issued %d time(s) against a node shutting down, want none", asked)
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

// Regression: 2026-10-02-remove-timeout-read-as-refusal: the removal ended as soon
// as the DELETE was accepted, while the control plane went on migrating the
// node's devices and volumes for as long as that takes. A removal that stalled or
// gave up there was invisible to the operation, which had already succeeded.
func TestAwaitingRemovalWaitsWhileTheControlPlaneRemovesTheNode(t *testing.T) {
	for _, status := range []string{
		nodeStatusPendingRemoval, nodeStatusMigratingDevices,
		nodeStatusMigratingLvols, nodeStatusInRemoval,
	} {
		t.Run(status, func(t *testing.T) {
			r, _ := aDraining(t, aControlPlane().reporting(status), &scriptedMover{})

			done, err := performing(t, r, aDrain(), stepAwaitingRemoval)
			if err != nil {
				t.Fatalf("awaiting the removal: %v", err)
			}
			if done {
				t.Errorf("the step finished while the control plane still reports %s", status)
			}
		})
	}
}

// Regression: 2026-10-02-remove-timeout-read-as-refusal: removed is the outcome
// the operation exists for.
func TestAwaitingRemovalFinishesWhenTheNodeIsRemoved(t *testing.T) {
	r, _ := aDraining(t, aControlPlane().reporting(nodeStatusRemoved), &scriptedMover{})

	done, err := performing(t, r, aDrain(), stepAwaitingRemoval)
	if err != nil {
		t.Fatalf("awaiting the removal: %v", err)
	}
	if !done {
		t.Error("the step did not finish although the control plane reports the node removed")
	}
}

// Regression: 2026-10-02-remove-timeout-read-as-refusal: removed_failed is the
// control plane giving up on the removal. It is terminal on that side, so the
// operation fails rather than waiting for a status that will not come.
func TestAwaitingRemovalFailsWhenTheControlPlaneGivesUp(t *testing.T) {
	r, _ := aDraining(t, aControlPlane().reporting(nodeStatusRemovedFailed), &scriptedMover{})

	_, err := performing(t, r, aDrain(), stepAwaitingRemoval)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for a removal the control plane gave up on", err)
	}
}

// Regression: 2026-10-02-removal-three-steps: the census walks the pools for
// volumes, and a snapshot is not one of them, so a node that still held a
// snapshot passed verification and the DELETE refused it. The control plane's
// verify-drained sees both, and the step finishes only when it says drained.
func TestVerifyingHoldsWhileTheControlPlaneSeesSomethingLeft(t *testing.T) {
	api := aControlPlane()
	api.verification = DrainVerification{Snapshots: []string{"snap-1"}}
	r, _ := aDraining(t, api, &scriptedMover{})

	done, err := performing(t, r, aDrain(), stepVerifying)

	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("done, err = %t, %v; want the step held while the node holds a snapshot", done, err)
	}
	if !strings.Contains(blocked.message, "snap-1") {
		t.Errorf("the hold says %q, want it to name what is left", blocked.message)
	}
}

// aPendingDrain is a removal in MigratingDevices whose node is still
// pending_removal, with the progress and prepare attempts it last recorded.
func aPendingDrain(
	t *testing.T, api *scriptedControlPlane, recorded *simplyblockv1alpha2.RemovalStatus,
) (*StorageNodeOpsReconciler, client.Client, *simplyblockv1alpha2.StorageNodeOps) {
	t.Helper()
	ops := aDrain()
	ops.Status.Removal = recorded
	r, apiClient := anOpsWorld(t, api, ops)
	r.Mover = &scriptedMover{}
	return r, apiClient, ops
}

// minutesAgo is a status timestamp the given number of minutes in the past.
func minutesAgo(minutes int) *metav1.Time {
	at := metav1.NewTime(time.Now().Add(-time.Duration(minutes) * time.Minute))
	return &at
}

// Regression: 2026-10-02-prepare-removal-shutdown-failed: prepare-removal marked
// worker-4 pending_removal and then failed to shut it down, and the operation
// waited on a node nothing was driving. A prepare the control plane reports
// failed is sent again, which is the retry it is idempotent for.
func TestAFailedPrepareIsSentAgain(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusPendingRemoval)
	api.progress = RemovalProgress{Total: 2, Failed: 1, Message: "shutdown failed",
		NodeStatus: nodeStatusPendingRemoval}
	r, apiClient, ops := aPendingDrain(t, api, &simplyblockv1alpha2.RemovalStatus{
		NodeStatus: nodeStatusPendingRemoval, LastProgressTime: minutesAgo(2),
	})

	done, err := performing(t, r, ops, stepMigratingDevices)
	if err != nil || done {
		t.Fatalf("done, err = %t, %v; want a pass that waits after sending prepare-removal again",
			done, err)
	}
	if asked := api.asked("PrepareRemoval"); asked != 1 {
		t.Errorf("PrepareRemoval was issued %d time(s), want once to retry the failed step", asked)
	}
	got := operationRead(t, apiClient, ops.Name)
	if got.Status.Removal == nil || got.Status.Removal.PrepareAttempts != 1 ||
		got.Status.Removal.LastPrepareTime == nil {
		t.Errorf("status.removal = %+v, want the attempt counted and timed", got.Status.Removal)
	}
}

// Regression: 2026-10-02-prepare-removal-shutdown-failed: the control plane can
// also fail the shutdown without saying so, which leaves the node
// pending_removal with nothing changing. Once nothing has moved for longer than
// a shutdown takes, prepare-removal is sent again.
func TestAStalledPrepareIsSentAgain(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusPendingRemoval)
	api.progress = RemovalProgress{Total: 2, NodeStatus: nodeStatusPendingRemoval}
	r, _, ops := aPendingDrain(t, api, &simplyblockv1alpha2.RemovalStatus{
		NodeStatus: nodeStatusPendingRemoval, LastProgressTime: minutesAgo(20),
	})

	if _, err := performing(t, r, ops, stepMigratingDevices); err != nil {
		t.Fatalf("migrating devices: %v", err)
	}
	if asked := api.asked("PrepareRemoval"); asked != 1 {
		t.Errorf("PrepareRemoval was issued %d time(s) for a node stalled 20 minutes, want once",
			asked)
	}
}

// The other half: a node that only just became pending_removal may be shutting
// down under the first prepare-removal, and a second one is not sent under it.
// A retry already sent is not sent again until it had time to answer.
func TestAPrepareIsNotSentAgainTooSoon(t *testing.T) {
	for name, recorded := range map[string]*simplyblockv1alpha2.RemovalStatus{
		"recently pending": {NodeStatus: nodeStatusPendingRemoval, LastProgressTime: minutesAgo(2)},
		"recently retried": {NodeStatus: nodeStatusPendingRemoval, LastProgressTime: minutesAgo(30),
			PrepareAttempts: 1, LastPrepareTime: minutesAgo(0)},
	} {
		t.Run(name, func(t *testing.T) {
			api := aControlPlane().reporting(nodeStatusPendingRemoval)
			api.progress = RemovalProgress{Total: 2, NodeStatus: nodeStatusPendingRemoval}
			if name == "recently retried" {
				api.progress.Failed = 1
			}
			r, _, ops := aPendingDrain(t, api, recorded)

			if _, err := performing(t, r, ops, stepMigratingDevices); err != nil {
				t.Fatalf("migrating devices: %v", err)
			}
			if asked := api.asked("PrepareRemoval"); asked != 0 {
				t.Errorf("PrepareRemoval was issued %d time(s), want none yet", asked)
			}
		})
	}
}

// Regression: 2026-10-02-prepare-removal-shutdown-failed: a prepare that keeps
// failing is not retried for the rest of the step's budget. Once the attempts
// run out the operation fails with what the control plane said.
func TestAPrepareThatKeepsFailingEndsTheDrain(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusPendingRemoval)
	api.progress = RemovalProgress{Total: 2, Failed: 1, Message: "Failed to kill SPDK",
		NodeStatus: nodeStatusPendingRemoval}
	r, _, ops := aPendingDrain(t, api, &simplyblockv1alpha2.RemovalStatus{
		NodeStatus: nodeStatusPendingRemoval, LastProgressTime: minutesAgo(30),
		PrepareAttempts: maxPrepareAttempts, LastPrepareTime: minutesAgo(5),
	})

	_, err := performing(t, r, ops, stepMigratingDevices)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) || !strings.Contains(err.Error(), "Failed to kill SPDK") {
		t.Errorf("err = %v, want the terminal kind carrying the control plane's message", err)
	}
	if asked := api.asked("PrepareRemoval"); asked != 0 {
		t.Errorf("PrepareRemoval was issued %d time(s) after the attempts ran out", asked)
	}
}
