// The JoinConsistencyGroup steps up to the swap: resolving the group, the plan
// and its preflight, the child migration, the join, and the selection of the
// subsystem the group ends up in (design-cg-live-subsystem-flip.md §5.5, §7).

package volume

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// children lists the Migrate operations the join created.
func (w *joinWorld) children() []simplyblockv1alpha2.PersistentVolumeOps {
	w.t.Helper()
	var list simplyblockv1alpha2.PersistentVolumeOpsList
	if err := w.r.List(context.Background(), &list); err != nil {
		w.t.Fatalf("list operations: %v", err)
	}
	var out []simplyblockv1alpha2.PersistentVolumeOps
	for _, ops := range list.Items {
		if ops.Spec.Action == simplyblockv1alpha2.PersistentVolumeOpsActionMigrate {
			out = append(out, ops)
		}
	}
	return out
}

// offPin makes the plan move the volume onto the group's node first.
func (w *joinWorld) offPin() {
	w.api.plan.Steps = []string{controlplane.JoinStepMigrate, controlplane.JoinStepJoin, controlplane.JoinStepColocate}
	w.api.plan.TargetNodeID = testTargetID
}

// setChildPhase reports the child migration as having reached phase.
func (w *joinWorld) setChildPhase(phase simplyblockv1alpha2.PersistentVolumeOpsPhase) {
	w.t.Helper()
	children := w.children()
	if len(children) != 1 {
		w.t.Fatalf("children = %d, want 1", len(children))
	}
	child := children[0]
	child.Status.Phase = phase
	if err := w.r.Status().Update(context.Background(), &child); err != nil {
		w.t.Fatalf("set the child's phase: %v", err)
	}
}

// U-58
func TestAGroupNameThatResolvesToNothingHoldsTheOperationPending(t *testing.T) {
	w := newJoinWorld(t)
	w.api.groupMissing = true

	result, err := w.pass()
	if err != nil {
		t.Fatal(err)
	}
	ops := w.ops()
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhasePending || ops.Status.DeferredSince == nil {
		t.Errorf("phase = %q, deferredSince = %v, want Pending and deferred", ops.Status.Phase, ops.Status.DeferredSince)
	}
	if result.RequeueAfter == 0 {
		t.Error("a held operation must requeue")
	}
	if !hasEvent(w.events(), ReasonConsistencyGroupPending) {
		t.Error("no ConsistencyGroupPending event")
	}
	if w.log.count("plan") != 0 {
		t.Errorf("planned %d times for a group that does not exist", w.log.count("plan"))
	}
}

// U-59
func TestARefusedPlanFailsTheJoin(t *testing.T) {
	w := newJoinWorld(t)
	w.api.planErr = fmt.Errorf("plan: %w: volume is in another consistency group", controlplane.ErrMembershipRefused)

	ops := w.run(10)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if !hasEvent(w.events(), ReasonConsistencyGroupJoinRefused) {
		t.Error("no ConsistencyGroupJoinRefused event")
	}
	if w.log.count("join") != 0 || len(w.children()) != 0 {
		t.Error("a refused plan joined or migrated")
	}
}

// U-60
func TestAPreflightRefusedForCapacityFailsBeforeAnyMigration(t *testing.T) {
	w := newJoinWorld(t)
	w.offPin()
	w.api.plan.Colocation = &controlplane.Colocation{
		Refusal: "SubsystemLimitReached: node n-1 holds its maximum of 500 subsystems",
	}

	ops := w.run(10)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if !hasEvent(w.events(), ReasonSubsystemLimitReached) {
		t.Error("no SubsystemLimitReached event")
	}
	if n := len(w.children()); n != 0 {
		t.Errorf("created %d child migrations for a join that cannot finish", n)
	}
	if w.log.count("join") != 0 {
		t.Error("joined a volume whose colocation was refused")
	}
}

// U-61
func TestAnOffPinPlanCreatesOneChildMigrationNamingItsCreator(t *testing.T) {
	w := newJoinWorld(t)
	w.offPin()

	w.runUntil(stepJoinMigrating, 10)
	for range 3 {
		if _, err := w.pass(); err != nil {
			t.Fatal(err)
		}
	}

	children := w.children()
	if len(children) != 1 {
		t.Fatalf("children = %d, want exactly one", len(children))
	}
	child := children[0]
	if child.Spec.PersistentVolumeName != testPVName || child.Spec.Migrate == nil ||
		child.Spec.Migrate.TargetNodeRef.Name != testTargetNode ||
		child.Spec.Migrate.TargetNodeRef.Namespace != testNamespace {
		t.Errorf("child spec = %+v, want a migration of the volume to %s", child.Spec, testTargetNode)
	}
	ref := child.Spec.CreatorRef
	if ref == nil || ref.Kind != "PersistentVolumeOps" || ref.Name != joinOpsName || ref.UID != w.ops().UID {
		t.Errorf("creatorRef = %+v, want the join with its UID", ref)
	}
	if got := w.ops().Status.JoinConsistencyGroup.MigrationName; got != child.Name {
		t.Errorf("status.migrationName = %q, want %q", got, child.Name)
	}
}

// U-62
func TestAnOnPinPlanSkipsTheMigration(t *testing.T) {
	w := newJoinWorld(t)

	w.runUntil(stepJoining, 10)
	if n := len(w.children()); n != 0 {
		t.Errorf("created %d child migrations for a volume already on the pin", n)
	}
}

// U-61, the join waits for the child
func TestTheJoinFollowsTheChildMigrationOnceItSucceeded(t *testing.T) {
	w := newJoinWorld(t)
	w.offPin()

	w.runUntil(stepJoinMigrating, 10)
	w.run(3)
	if w.log.count("join") != 0 {
		t.Fatal("joined before the migration finished")
	}
	w.setChildPhase(simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded)
	w.runUntil(stepSelectingSubsystem, 10)
	if w.log.count("join") != 1 {
		t.Errorf("joins = %d, want 1", w.log.count("join"))
	}
}

// U-63
func TestAFailedChildMigrationFailsTheJoin(t *testing.T) {
	w := newJoinWorld(t)
	w.offPin()

	w.runUntil(stepJoinMigrating, 10)
	w.run(2)
	w.setChildPhase(simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed)
	ops := w.run(10)

	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if !hasEvent(w.events(), ReasonConsistencyGroupMigrationFailed) {
		t.Error("no ConsistencyGroupMigrationFailed event")
	}
	if w.log.count("join") != 0 {
		t.Error("joined after the migration failed")
	}
}

// U-64
func TestAnAbortDuringMigratingAbortsTheChild(t *testing.T) {
	w := newJoinWorld(t)
	w.offPin()

	w.runUntil(stepJoinMigrating, 10)
	w.run(2)
	w.update(func(ops *simplyblockv1alpha2.PersistentVolumeOps) { ops.Spec.Abort = true })
	w.run(2)

	if child := w.children()[0]; !child.Spec.Abort {
		t.Fatal("the child migration was not asked to abort")
	}
	w.setChildPhase(simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted)
	if ops := w.run(10); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted {
		t.Errorf("phase = %q (%s), want Aborted", ops.Status.Phase, ops.Status.Message)
	}
}

// U-65
func TestJoiningCallsTheMembersEndpointOnce(t *testing.T) {
	w := newJoinWorld(t)

	w.runUntil(stepSelectingSubsystem, 10)
	w.run(3)
	if n := w.log.count("join"); n != 1 {
		t.Errorf("joins = %d, want 1", n)
	}
	if !w.ops().Status.JoinConsistencyGroup.Joined {
		t.Error("status.joined is not set")
	}
}

// U-65
func TestAJoinTheGroupRefusesFailsTheOperation(t *testing.T) {
	w := newJoinWorld(t)
	w.api.joinErr = fmt.Errorf("join: %w: the group is full", controlplane.ErrMembershipRefused)

	ops := w.run(10)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if !hasEvent(w.events(), ReasonConsistencyGroupJoinRefused) {
		t.Error("no ConsistencyGroupJoinRefused event")
	}
}

// A volume already a member is not joined again.
func TestAVolumeAlreadyAMemberIsNotJoinedAgain(t *testing.T) {
	w := newJoinWorld(t)
	w.api.plan.Steps = []string{controlplane.JoinStepColocate}

	w.runUntil(stepSelectingSubsystem, 10)
	if n := w.log.count("join"); n != 0 {
		t.Errorf("joins = %d, want 0 for a member", n)
	}
}

// U-66
func TestTheMoveIsCreatedWithTheOperationsUIDAsItsRequestID(t *testing.T) {
	w := newJoinWorld(t)

	w.runUntil(stepPreparingTarget, 15)
	if len(w.api.requestIDs) == 0 {
		t.Fatal("no move was created")
	}
	for _, id := range w.api.requestIDs {
		if id != string(w.ops().UID) {
			t.Errorf("request id = %q, want the operation's UID", id)
		}
	}
}

// U-67
func TestAMoveWithNothingToMoveSucceedsWithoutCallingAnyNode(t *testing.T) {
	w := newJoinWorld(t)
	w.api.move = controlplane.SubsystemMove{}

	ops := w.run(15)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if !hasEvent(w.events(), ReasonConsistencyGroupColocated) {
		t.Error("no ConsistencyGroupColocated event")
	}
	for _, c := range w.log.calls {
		if slices.Contains([]string{"prepare", "move", "commit"}, c) {
			t.Errorf("called %s for an empty move", c)
		}
	}
	if n := w.log.count("suspend") + w.log.count("describe"); n != 0 {
		t.Errorf("called a node %d times for an empty move", n)
	}
}

// U-68, U-69
func TestAWholeGroupMoveRecordsEveryVolumeAndTheTarget(t *testing.T) {
	w := newJoinWorld(t)

	ops := w.runUntil(stepPreparingTarget, 15)
	status := ops.Status.JoinConsistencyGroup
	if status.MoveID != joinMoveID || status.TargetNQN != joinTargetNQN || !status.TargetIsNew {
		t.Errorf("status = %+v, want move %s to a fresh %s", status, joinMoveID, joinTargetNQN)
	}
	sources := map[string]string{}
	for _, v := range status.Volumes {
		sources[v.VolumeUUID] = v.SourceNQN
	}
	if sources[testVolumeID] != joinSourceNQN || sources[memberVolumeID] != memberNQN || len(sources) != 2 {
		t.Errorf("volumes = %+v, want both members with their sources", status.Volumes)
	}
}

// U-70
func TestASelectionRefusedForCapacityFailsAndKeepsTheMembership(t *testing.T) {
	w := newJoinWorld(t)
	w.api.createErr = fmt.Errorf("create: %w", controlplane.ErrSubsystemLimitReached)

	ops := w.run(15)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if !ops.Status.JoinConsistencyGroup.Joined {
		t.Error("status.joined was cleared; the membership stays")
	}
	if !hasEvent(w.events(), ReasonSubsystemLimitReached) {
		t.Error("no SubsystemLimitReached event")
	}
	if n := w.log.count("suspend") + w.log.count("prepare "); n != 0 {
		t.Errorf("called a node %d times after a refused selection", n)
	}
}

// U-71
func TestAPrepareFailureIsRetriedWithoutTouchingANode(t *testing.T) {
	w := newJoinWorld(t)
	w.api.prepareErr = statusErr

	w.runUntil(stepPreparingTarget, 15)
	w.run(4)
	ops := w.ops()
	if ops.Status.Step.State != string(stepPreparingTarget) || terminal(ops.Status.Phase) {
		t.Errorf("step = %q, phase = %q, want still preparing", ops.Status.Step.State, ops.Status.Phase)
	}
	if w.log.count("prepare") < 2 {
		t.Errorf("prepares = %d, want retries", w.log.count("prepare"))
	}
	if n := w.log.count("prepare ") + w.log.count("suspend"); n != 0 {
		t.Errorf("called a node %d times while the target was not ready", n)
	}
}

var _ = types.NamespacedName{}

// U-135
// Regression: 2026-11-03-cg-join-stale-step-message — the steps after Migrating
// kept reporting "waiting on Migrating: migration ... is Running" after the
// migration had finished, because entering a step never replaced the message
// the step before it had written.
func TestEnteringTheNextStepReplacesTheWaitingMessageOfThePreviousOne(t *testing.T) {
	w := newJoinWorld(t)
	w.offPin()

	ops := w.runUntil(stepJoinMigrating, 10)
	ops = w.run(2)
	if !strings.Contains(ops.Status.Message, "waiting on Migrating") {
		t.Fatalf("message = %q while the child runs, want it waiting on Migrating", ops.Status.Message)
	}
	w.setChildPhase(simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded)
	ops = w.runUntil(stepSelectingSubsystem, 10)
	if strings.Contains(ops.Status.Message, "Migrating") {
		t.Errorf("in step %s the message still reads %q", ops.Status.Step.State, ops.Status.Message)
	}
}
