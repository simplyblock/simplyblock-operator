// The migrate walk, declared as an atlas-lib/statemachine graph. §23 requires
// it: the package validates that every edge points at a declared state when the
// graph is built rather than at runtime on the unhappy path, it carries a
// state's deadline alongside its name so a phase that hangs is distinguishable
// from one that is working, and restoring a snapshot runs no entry hooks, which
// is how a resumed run avoids repeating the side effect of the phase it resumes
// into.

package upgrade

import (
	"context"
	"time"

	"github.com/simplyblock/atlas/statemachine"
)

// Phase is one state of the migrate walk. The phases are the design's, and each
// of them is a bucket steps are registered into rather than a function: adding
// work to the migration is adding a [Step] that names its phase, and the graph
// does not change.
type Phase string

const (
	// PhasePending is where a run starts and where a resumed run that had not
	// begun restores to.
	PhasePending Phase = "Pending"

	// PhaseValidating runs the checks registered for the migrate stage,
	// including everything the preflight already asked, because an object
	// created since has never been checked (§18).
	PhaseValidating Phase = "Validating"

	// PhaseTransforming copies the renamed and absorbed kinds and rewrites the
	// annotation and label keys (§16.2, §16.3).
	PhaseTransforming Phase = "Transforming"

	// PhaseOwnership reparents the objects a retired owner holds, in the order
	// §20 sets, and never deletes an owner before its dependents have moved.
	PhaseOwnership Phase = "Ownership"

	// PhaseHandles normalizes the volume handles whose pool segment carries a
	// name rather than a UUID (§16.4).
	PhaseHandles Phase = "Handles"

	// PhaseDeleting removes what the transformations left behind, once their
	// replacements have been verified.
	PhaseDeleting Phase = "Deleting"

	// PhaseRewriting switches the storage version and writes every object of
	// the converting kinds back unchanged (§24).
	PhaseRewriting Phase = "Rewriting"

	// PhaseVerifying is the final pass: the post-conditions of the whole
	// migration rather than of one step.
	PhaseVerifying Phase = "Verifying"

	// PhaseCompleted is terminal and successful.
	PhaseCompleted Phase = "Completed"

	// PhaseFailed is terminal. A run reaching it left the cluster in a state
	// the record describes, and rerunning the migration resumes from what the
	// cluster says rather than from this phase.
	PhaseFailed Phase = "Failed"
)

// MigratePhases is the order the phases run in, and the order the steps of the
// migrate stage are grouped into. It is derived from nothing: it is the
// sequence §23 declares, and [MigrateGraph] is built from it so the two cannot
// drift.
var MigratePhases = []Phase{
	PhasePending,
	PhaseValidating,
	PhaseTransforming,
	PhaseOwnership,
	PhaseHandles,
	PhaseDeleting,
	PhaseRewriting,
	PhaseVerifying,
	PhaseCompleted,
}

// PhaseEnter is the work of entering one phase. It returns how long the phase
// may take, which becomes the state's deadline, so a phase that hangs is
// reported as a phase that hangs.
type PhaseEnter func(ctx context.Context, from, to Phase) (time.Duration, error)

// MigrateGraph builds the state machine's configuration, wiring each phase's
// entry hook to the function the runner supplies for it. A phase with no hook
// entered runs nothing and is left without a deadline, which is what
// [PhaseCompleted] and [PhaseFailed] want.
//
// Every phase but the terminal ones may reach [PhaseFailed], because any of
// them can refuse, and [PhaseFailed] is terminal because a failed migration is
// rerun rather than resumed in place.
func MigrateGraph(hooks map[Phase]PhaseEnter) statemachine.Config[Phase] {
	states := make(map[Phase]statemachine.StateDef[Phase], len(MigratePhases)+1)

	for i, phase := range MigratePhases {
		def := statemachine.StateDef[Phase]{}
		if i+1 < len(MigratePhases) {
			def.To = []Phase{MigratePhases[i+1], PhaseFailed}
		}
		if hook, ok := hooks[phase]; ok && hook != nil {
			def.OnEnter = statemachine.TransitionFunc[Phase](hook)
		}
		states[phase] = def
	}
	states[PhaseFailed] = statemachine.StateDef[Phase]{}

	return statemachine.Config[Phase]{Initial: PhasePending, States: states}
}

// Describe is the sentence a progress view narrates while the phase runs, in
// the same voice as [Stage.Describe].
func (p Phase) Describe() string {
	switch p {
	case PhasePending:
		return "Starting"
	case PhaseValidating:
		return "Verifying the resource graph"
	case PhaseTransforming:
		return "Copying the renamed and absorbed kinds"
	case PhaseOwnership:
		return "Reparenting what the retired owners hold"
	case PhaseHandles:
		return "Normalizing volume handles"
	case PhaseDeleting:
		return "Removing what has been replaced"
	case PhaseRewriting:
		return "Rewriting objects into the new storage version"
	case PhaseVerifying:
		return "Verifying the migration"
	case PhaseCompleted:
		return "Completed"
	case PhaseFailed:
		return "Failed"
	default:
		return string(p)
	}
}

// Terminal reports whether a phase ends the walk.
func (p Phase) Terminal() bool {
	return p == PhaseCompleted || p == PhaseFailed
}
