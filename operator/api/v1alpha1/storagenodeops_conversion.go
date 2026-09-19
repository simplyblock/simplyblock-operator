// Conversion of StorageNodeOps between this version and the v1alpha2 hub.
//
// Four properties are renames (design-property-renames.md §2.1, §2.4, and §2.5):
// storageNodeRef becomes nodeRef, drain becomes remove, targetWorkerNode and
// newSsdPcie regroup under migrate, and the action enum is recased. See
// controlplane_conversion.go for why an unmapped enum value is passed through
// rather than rejected.
//
// The rest is the step machine of design-storagenode.md §6.3 and §7, which this
// version cannot express, and it divides in three:
//
//   - status.subPhase against status.step. The two carry the same position and
//     convert by table rather than by a stash, except that this version's table
//     is not a function: Migrating means "move the volumes off" under Remove and
//     "issue the relocation restart" under Migrate, and Restarting means "wait
//     for the node to come back" under Migrate and "restart it" under
//     HostMaintenance. The action is therefore an input to both directions, which
//     is the whole reason §6.3 splits the value in two.
//   - The drain counters. status.volumesMigrated and status.volumesPending
//     become status.drain, and the arithmetic is exact in both directions:
//     volumesTotal is the sum of the two, and volumesPending is what is left of
//     it. A pending count that has to be kept in step with a total is what the
//     regrouping removes.
//   - Everything else. status.step's deadline, spec.abort,
//     status.observedGeneration, and the Aborted phase have nowhere to go here,
//     so they are stashed on the way down and restored on the way up. The Aborted
//     phase is the sharp one: this version's Enum marker does not accept the
//     value, so writing it would make the object rejected at admission rather
//     than merely odd, and it is narrowed to Failed with the true phase in its
//     annotation.
//
// One field travels the other way. status.triggered is this version's and the hub
// removed it (§7.2), so it stashes on the way up under
// storage.simplyblock.io/v1alpha1-status.triggered. What is preserved is the text
// that was stored rather than any behavior: the hub's controller reads the
// persisted step instead, and every call it makes is skipped when its target is
// already at or past what that call would produce.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/simplyblock/atlas/statemachine"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The annotation holding the one status field the hub removed.
const annoV1Alpha1NodeOpsTriggered = "storage.simplyblock.io/v1alpha1-status.triggered"

// The annotations holding the hub fields this version cannot express.
const (
	annoNodeOpsStep     = "storage.simplyblock.io/conversion-status.step"
	annoNodeOpsPhase    = "storage.simplyblock.io/conversion-status.phase"
	annoNodeOpsObserved = "storage.simplyblock.io/conversion-status.observedGeneration"
	annoNodeOpsAbort    = "storage.simplyblock.io/conversion-spec.abort"
)

// storageNodeOpsActionToHub maps this version's lowercase actions onto the hub's
// PascalCase ones. Every value is listed, because unlike a phase this enum is
// user-authored and appears in scripts and runbooks.
//
// HostMaintenance has no row: the action is the hub's addition and this version
// never accepted it, so it passes through and is rejected by this version's own
// Enum marker, which is the correct outcome for an operation this version cannot
// perform.
var storageNodeOpsActionToHub = map[string]string{
	"shutdown": string(v1alpha2.StorageNodeOpsActionShutdown),
	"restart":  string(v1alpha2.StorageNodeOpsActionRestart),
	"suspend":  string(v1alpha2.StorageNodeOpsActionSuspend),
	"resume":   string(v1alpha2.StorageNodeOpsActionResume),
	"remove":   string(v1alpha2.StorageNodeOpsActionRemove),
	"migrate":  string(v1alpha2.StorageNodeOpsActionMigrate),
}

// storageNodeOpsActionFromHub is the inverse, derived so the two cannot disagree
// about a value.
var storageNodeOpsActionFromHub = invertStringMap(storageNodeOpsActionToHub)

// subPhaseToStep reads this version's sub-phase as the hub's step, keyed by the
// hub action the operation is performing.
//
// It is keyed rather than flat because two of this version's eight values are
// ambiguous by construction, which is the defect §6.3 names: Migrating is the
// volume drain under Remove and the relocation restart under Migrate, and
// Restarting is the wait for the node to come back under Migrate. Reading either
// without the action produces a step belonging to the other workflow, which the
// hub's per-action graph then refuses.
var subPhaseToStep = map[v1alpha2.StorageNodeOpsAction]map[string]string{
	v1alpha2.StorageNodeOpsActionRemove: {
		string(StorageNodeOpsSubPhaseValidating): string(v1alpha2.StorageNodeOpsStepValidating),
		string(StorageNodeOpsSubPhaseSuspending): string(v1alpha2.StorageNodeOpsStepSuspending),
		string(StorageNodeOpsSubPhaseMigrating):  string(v1alpha2.StorageNodeOpsStepMigratingVolumes),
		string(StorageNodeOpsSubPhaseVerifying):  string(v1alpha2.StorageNodeOpsStepVerifying),
		string(StorageNodeOpsSubPhaseRemoving):   string(v1alpha2.StorageNodeOpsStepRemoving),
	},
	v1alpha2.StorageNodeOpsActionMigrate: {
		string(StorageNodeOpsSubPhasePreparing): string(v1alpha2.StorageNodeOpsStepPreparing),
		// This version issues the restart in Restarting and waits for the node
		// in the same value. The hub splits the two, and the half a stored
		// object is in is the wait: the call was made on entry.
		string(StorageNodeOpsSubPhaseRestarting): string(v1alpha2.StorageNodeOpsStepAwaitingNode),
		string(StorageNodeOpsSubPhasePromoting):  string(v1alpha2.StorageNodeOpsStepPromoting),
	},
}

// stepToSubPhase projects a hub step back onto a sub-phase this version's Enum
// accepts, so that a v1alpha1 reader sees where the operation is rather than an
// empty field.
//
// It is not the inverse of subPhaseToStep and cannot be. Four of the hub's steps
// have no v1alpha1 spelling at all: Requesting and Awaiting, because the four
// single-step actions never had a sub-phase here, and Relocating and AwaitingNode,
// which are the two halves this version wrote as one Restarting. Every step of
// HostMaintenance is likewise absent, since the action is. Those read as an empty
// sub-phase, and the stash carries the real step.
var stepToSubPhase = map[string]StorageNodeOpsSubPhase{
	string(v1alpha2.StorageNodeOpsStepValidating):       StorageNodeOpsSubPhaseValidating,
	string(v1alpha2.StorageNodeOpsStepSuspending):       StorageNodeOpsSubPhaseSuspending,
	string(v1alpha2.StorageNodeOpsStepMigratingVolumes): StorageNodeOpsSubPhaseMigrating,
	string(v1alpha2.StorageNodeOpsStepVerifying):        StorageNodeOpsSubPhaseVerifying,
	string(v1alpha2.StorageNodeOpsStepRemoving):         StorageNodeOpsSubPhaseRemoving,
	string(v1alpha2.StorageNodeOpsStepPreparing):        StorageNodeOpsSubPhasePreparing,
	string(v1alpha2.StorageNodeOpsStepRelocating):       StorageNodeOpsSubPhaseRestarting,
	string(v1alpha2.StorageNodeOpsStepAwaitingNode):     StorageNodeOpsSubPhaseRestarting,
	string(v1alpha2.StorageNodeOpsStepPromoting):        StorageNodeOpsSubPhasePromoting,
}

// ConvertTo converts this StorageNodeOps to the v1alpha2 hub.
func (src *StorageNodeOps) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.StorageNodeOps)

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	stashOpsFlag(&dst.ObjectMeta, annoV1Alpha1NodeOpsTriggered, src.Status.Triggered)

	action := v1alpha2.StorageNodeOpsAction(
		mapOrPassThrough(storageNodeOpsActionToHub, src.Spec.Action),
	)
	dst.Spec = v1alpha2.StorageNodeOpsSpec{
		NodeRef:        src.Spec.StorageNodeRef,
		Action:         action,
		Force:          src.Spec.Force,
		ReattachVolume: src.Spec.ReattachVolume,
	}

	// The migrate block exists only if one of the two fields that regroup into it
	// was set. Either one alone is enough: extra drives without a target worker
	// still have to reach the control-plane restart, and an empty block would be
	// a value nobody could have authored, since the target worker is required.
	if src.Spec.TargetWorkerNode != "" || len(src.Spec.NewSsdPcie) > 0 {
		dst.Spec.Migrate = &v1alpha2.MigrateSpec{
			TargetWorkerNode: src.Spec.TargetWorkerNode,
			NewSsdPcie:       src.Spec.NewSsdPcie,
		}
	}
	if src.Spec.Drain != nil {
		dst.Spec.Remove = &v1alpha2.RemoveSpec{
			SystemVolumeFilterRegex: src.Spec.Drain.SystemVolumeFilterRegex,
		}
	}

	dst.Status = v1alpha2.StorageNodeOpsStatus{
		Phase:       v1alpha2.StorageNodeOpsPhase(src.Status.Phase),
		Message:     src.Status.Message,
		StartedAt:   src.Status.StartedAt,
		CompletedAt: src.Status.CompletedAt,
	}
	// A sub-phase this version's tables do not place under this action is
	// dropped rather than passed through. The target is a field of a shared
	// type, so an unrecognized step fails the machine's restore rather than the
	// object's admission (§6.3), and an empty step restores to the graph's
	// initial state, which is where an operation with nothing recorded belongs.
	if step, ok := subPhaseToStep[action][string(src.Status.SubPhase)]; ok {
		dst.Status.Step = statemachine.KubeSnapshot{State: step}
	}
	// The drain block exists only for an operation that had one. The counters
	// are declared "drain only" here and default to zero on every other action,
	// so allocating a block for them would give a Shutdown a drain that says it
	// moved none of no volumes.
	if src.Status.VolumesMigrated != 0 || src.Status.VolumesPending != 0 {
		dst.Status.Drain = &v1alpha2.DrainStatus{
			VolumesTotal:    int32(src.Status.VolumesMigrated + src.Status.VolumesPending),
			VolumesMigrated: int32(src.Status.VolumesMigrated),
		}
	}

	return restoreNodeOpsHubOnly(&dst.ObjectMeta, dst)
}

// ConvertFrom converts the v1alpha2 hub into this StorageNodeOps.
func (dst *StorageNodeOps) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.StorageNodeOps)

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()

	dst.Spec = StorageNodeOpsSpec{
		StorageNodeRef: src.Spec.NodeRef,
		Action:         mapOrPassThrough(storageNodeOpsActionFromHub, string(src.Spec.Action)),
		Force:          src.Spec.Force,
		ReattachVolume: src.Spec.ReattachVolume,
	}
	if m := src.Spec.Migrate; m != nil {
		dst.Spec.TargetWorkerNode = m.TargetWorkerNode
		dst.Spec.NewSsdPcie = m.NewSsdPcie
	}
	if r := src.Spec.Remove; r != nil {
		dst.Spec.Drain = &DrainOpsSpec{SystemVolumeFilterRegex: r.SystemVolumeFilterRegex}
	}

	dst.Status = StorageNodeOpsStatus{
		Phase:       narrowNodeOpsPhase(src.Status.Phase),
		SubPhase:    stepToSubPhase[src.Status.Step.State],
		Message:     src.Status.Message,
		Triggered:   unstashRemoved(&dst.ObjectMeta, annoV1Alpha1NodeOpsTriggered) == stashedTrue,
		StartedAt:   src.Status.StartedAt,
		CompletedAt: src.Status.CompletedAt,
	}
	if d := src.Status.Drain; d != nil {
		dst.Status.VolumesMigrated = int(d.VolumesMigrated)
		dst.Status.VolumesPending = int(d.VolumesTotal - d.VolumesMigrated)
	}

	return stashNodeOpsHubOnly(&dst.ObjectMeta, src)
}

// stashNodeOpsHubOnly writes every hub field this version has nowhere to put. A
// field at its zero value writes no annotation, so an operation that reached none
// of them is not given metadata it never had.
func stashNodeOpsHubOnly(meta *metav1.ObjectMeta, src *v1alpha2.StorageNodeOps) error {
	if err := stashNodeOpsStep(meta, src.Spec.Action, src.Status.Step); err != nil {
		return err
	}
	if err := stash(meta, annoNodeOpsObserved, src.Status.ObservedGeneration); err != nil {
		return err
	}

	// Only the phase this version cannot spell is recorded. Every other value
	// survives the narrowing, and an annotation for each would be noise on every
	// operation that ever ran.
	if src.Status.Phase == v1alpha2.StorageNodeOpsPhaseAborted {
		stashRemoved(meta, annoNodeOpsPhase, string(src.Status.Phase))
	} else {
		clear(meta, annoNodeOpsPhase)
	}

	// spec.abort is a bool rather than a pointer, so false and unset are the same
	// value and only true is worth a note.
	if src.Spec.Abort {
		stashRemoved(meta, annoNodeOpsAbort, stashedTrue)
	} else {
		clear(meta, annoNodeOpsAbort)
	}
	return nil
}

// stashNodeOpsStep records the step only when subPhase cannot carry it.
//
// The projection is one-way, so the five Remove steps and two of the four Migrate
// steps survive being written to subPhase and read back, and annotating those
// would put a note on every drain that ever ran. What does not survive is a step
// with a deadline, either half of the Relocating and AwaitingNode pair this
// version spelled as one Restarting, and every step of an action subPhase never
// covered.
func stashNodeOpsStep(
	meta *metav1.ObjectMeta,
	action v1alpha2.StorageNodeOpsAction,
	step statemachine.KubeSnapshot,
) error {
	roundTrips := subPhaseToStep[action][string(stepToSubPhase[step.State])] == step.State
	if step.Deadline == nil && roundTrips {
		clear(meta, annoNodeOpsStep)
		return nil
	}
	return stash(meta, annoNodeOpsStep, step)
}

// restoreNodeOpsHubOnly reads them back and removes the annotations, so an object
// converted up carries the fields rather than both the fields and the notes about
// them.
func restoreNodeOpsHubOnly(meta *metav1.ObjectMeta, dst *v1alpha2.StorageNodeOps) error {
	// The stash wins where there is one, because only it carries a deadline and
	// the steps subPhase cannot spell. ConvertTo has already read
	// status.subPhase for an object a real v1alpha1 client wrote.
	var step statemachine.KubeSnapshot
	if err := unstash(meta, annoNodeOpsStep, &step); err != nil {
		return err
	}
	if step.State != "" || step.Deadline != nil {
		dst.Status.Step = step
	}

	if err := unstash(meta, annoNodeOpsObserved, &dst.Status.ObservedGeneration); err != nil {
		return err
	}

	if phase := unstashRemoved(meta, annoNodeOpsPhase); phase != "" {
		dst.Status.Phase = v1alpha2.StorageNodeOpsPhase(phase)
	}
	dst.Spec.Abort = unstashRemoved(meta, annoNodeOpsAbort) == stashedTrue
	return nil
}

// narrowNodeOpsPhase maps a hub phase onto one this version's Enum accepts. Only
// Aborted needs it, and it reads as Failed here: the operation did stop before
// finishing, which is the closest true statement this version can make, and the
// annotation carries the distinction.
func narrowNodeOpsPhase(phase v1alpha2.StorageNodeOpsPhase) StorageNodeOpsPhase {
	if phase == v1alpha2.StorageNodeOpsPhaseAborted {
		return StorageNodeOpsPhaseFailed
	}
	return StorageNodeOpsPhase(phase)
}
