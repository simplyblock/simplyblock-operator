// The rules that decide when a migration target is abandoned.
//
// A failure the target caused burns it once. A failure nobody can pin on the
// target is counted against that target, and only that target: the bound
// abandons a node that keeps failing for reasons outside itself, and a count
// kept per volume alone reached the bound on whichever node happened to be the
// tenth and burnt it for failures it never saw. Each failed CR is counted once,
// however many reconciles observe it before it is deleted.

package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func failedVM(name, target, sourceNode string) simplyblockv1alpha1.VolumeMigration {
	m := simplyblockv1alpha1.VolumeMigration{}
	m.Name = name
	m.UID = types.UID("uid-" + name)
	m.Spec.PVName = drainTestPVA
	m.Spec.TargetNodeUUID = target
	m.Status.SourceNodeUUID = sourceNode
	m.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseFailed
	return m
}

func entryFor(ops *simplyblockv1alpha1.StorageNodeOps) *simplyblockv1alpha1.VolumeDrainTargets {
	for i := range ops.Status.DrainTargetsTried {
		if ops.Status.DrainTargetsTried[i].PVName == drainTestPVA {
			return &ops.Status.DrainTargetsTried[i]
		}
	}
	return nil
}

func TestUnattributedFailuresAreCountedPerTarget(t *testing.T) {
	ops := &simplyblockv1alpha1.StorageNodeOps{}
	half := MaxUnattributedFailures / 2

	// half the bound against node-2, then half against node-3: neither node
	// has failed enough on its own to be abandoned.
	for i := 0; i < half; i++ {
		recordExhaustedTargets(ops, []simplyblockv1alpha1.VolumeMigration{
			failedVM("a"+string(rune('0'+i)), drainTestNode2, "")})
	}
	for i := 0; i < half; i++ {
		recordExhaustedTargets(ops, []simplyblockv1alpha1.VolumeMigration{
			failedVM("b"+string(rune('0'+i)), drainTestNode3, "")})
	}
	if got := drainTargetsTriedFor(ops, drainTestPVA); len(got) != 0 {
		t.Fatalf("burnt %v after %d failures on one node and %d on another: the count "+
			"must not carry over between targets", got, half, half)
	}
	e := entryFor(ops)
	if e == nil || e.FailingTarget != drainTestNode3 || e.Failures != half {
		t.Fatalf("count should follow the current target: got %+v", e)
	}
}

func TestUnattributedFailuresOnOneTargetStillReachTheBound(t *testing.T) {
	ops := &simplyblockv1alpha1.StorageNodeOps{}
	for i := 0; i < MaxUnattributedFailures; i++ {
		recordExhaustedTargets(ops, []simplyblockv1alpha1.VolumeMigration{
			failedVM("f"+string(rune('a'+i)), drainTestNode2, "")})
	}
	if got := drainTargetsTriedFor(ops, drainTestPVA); len(got) != 1 || got[0] != drainTestNode2 {
		t.Fatalf("exhausted = %v, want [node-2] after %d unattributed failures on it", got, MaxUnattributedFailures)
	}
	if e := entryFor(ops); e.Failures != 0 || e.FailingTarget != "" {
		t.Errorf("after burning the target the count must restart: %+v", e)
	}
}

func TestAFailedCRIsCountedOnceHoweverOftenItIsObserved(t *testing.T) {
	// A failed CR stays until the drain deletes it; a pass cut short by a
	// refused status write, a refused delete or a restart observes the same
	// CR again. The count must not move on the repeat.
	ops := &simplyblockv1alpha1.StorageNodeOps{}
	same := failedVM("once", drainTestNode2, "")
	for i := 0; i < MaxUnattributedFailures+2; i++ {
		recordExhaustedTargets(ops, []simplyblockv1alpha1.VolumeMigration{same})
	}
	if e := entryFor(ops); e == nil || e.Failures != 1 {
		t.Fatalf("one failure observed %d times counted as %+v, want Failures=1", MaxUnattributedFailures+2, e)
	}
	if got := drainTargetsTriedFor(ops, drainTestPVA); len(got) != 0 {
		t.Fatalf("a single failure re-observed burnt the target: %v", got)
	}

	// A new CR for the same volume (a recreated one has a new UID) counts.
	next := failedVM("twice", drainTestNode2, "")
	recordExhaustedTargets(ops, []simplyblockv1alpha1.VolumeMigration{next})
	if e := entryFor(ops); e.Failures != 2 {
		t.Errorf("a distinct failed CR was not counted: %+v", e)
	}
}

func TestAnEngagedFailureBurnsTheTargetExactlyOnce(t *testing.T) {
	ops := &simplyblockv1alpha1.StorageNodeOps{}
	vm := failedVM("ran", drainTestNode2, "node-src")
	if !recordExhaustedTargets(ops, []simplyblockv1alpha1.VolumeMigration{vm}) {
		t.Fatal("first observation recorded nothing")
	}
	if recordExhaustedTargets(ops, []simplyblockv1alpha1.VolumeMigration{vm}) {
		t.Fatal("the same failure re-observed reported a change")
	}
	if got := drainTargetsTriedFor(ops, drainTestPVA); len(got) != 1 {
		t.Errorf("exhausted = %v, want node-2 exactly once", got)
	}
}
