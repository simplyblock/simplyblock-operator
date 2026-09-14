// The Restore action's state graph and the work each of its steps performs.
//
// The graph is declared rather than switched on, which is what makes an illegal
// transition an error instead of an accepted status write: a step belonging to
// another action fails at TransitionTo rather than being recorded
// (design-crd-model.md §3.1). It is a MultiConfig although there is one action
// today, for the same reason spec.action exists on a single-action kind — the
// shape does not have to change the day it gains a second.
//
// The steps do the work and the graph declares what may follow what. Each step's
// perform is idempotent and reports whether it is finished, so a reconcile that
// arrives twice on the same step does the same thing twice and converges; the
// side effects that cannot be repeated are each guarded by a status field
// written before them.
//
// Two steps are abortable and two are not, and the line is where a logical
// volume comes into existence. Validating has resolved names and created
// nothing. Restoring has asked the control plane for a copy back and may or may
// not have been answered, so an abort there deletes whatever it finds. Once
// AwaitingVolume is entered there is a volume the control plane is filling, and
// the graph declares no way out: an abort arriving then is reported as an
// illegal transition while the operation runs on, rather than leaving a
// half-restored volume nothing accounts for.

package backup

import (
	"context"
	"time"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// step is the kind's step type, aliased so the graph literal below reads as the
// graph rather than as a wall of package qualifiers.
type step = simplyblockv1alpha2.StorageBackupOpsStep

const (
	stepValidating     = simplyblockv1alpha2.StorageBackupOpsStepValidating
	stepRestoring      = simplyblockv1alpha2.StorageBackupOpsStepRestoring
	stepAwaitingVolume = simplyblockv1alpha2.StorageBackupOpsStepAwaitingVolume
	stepBinding        = simplyblockv1alpha2.StorageBackupOpsStepBinding
)

// actionRestore is the MultiConfig key for the one action this kind carries.
const actionRestore = statemachine.Action(simplyblockv1alpha2.StorageBackupOpsActionRestore)

// How long each step may take before it is reported as stuck.
//
// AwaitingVolume is the long one by a wide margin, and deliberately so: it is
// waiting on an S3 transfer whose length is the size of the backup divided by
// whatever bandwidth the bucket gives, which for a first full copy of a large
// volume is hours. A deadline short enough to catch a stall would fail every
// large restore, so this one is a bound on the pathological case rather than on
// the slow one. The other three are bounded by a single API call each.
const (
	validatingDeadline     = 2 * time.Minute
	restoringDeadline      = 10 * time.Minute
	awaitingVolumeDeadline = 24 * time.Hour
	bindingDeadline        = 15 * time.Minute
)

// abortableSteps are the steps from which an abort unwinds cleanly.
//
// It is a table beside the graph rather than an edge in it, because the kind's
// step enum has four values and none of them is an abort state: a terminal
// Aborted step would be a fifth value in the API, and the phase already carries
// that meaning. The test in this package's suite asserts that every step here is
// one the graph declares, so the two cannot drift.
var abortableSteps = map[step]bool{
	stepValidating: true,
	stepRestoring:  true,
}

// restoreGraph is the Restore action's declared steps. The deadlines are set on
// entry, which is what makes a step that outlived its own budget detectable
// after an operator restart: the instant is absolute and travels in the status.
func restoreGraph() statemachine.MultiConfig[step] {
	deadline := func(d time.Duration) statemachine.TransitionFunc[step] {
		return func(context.Context, step, step) (time.Duration, error) { return d, nil }
	}
	return statemachine.MultiConfig[step]{
		actionRestore: {
			Initial: stepValidating,
			States: map[step]statemachine.StateDef[step]{
				stepValidating:     {To: []step{stepRestoring}, OnEnter: deadline(validatingDeadline)},
				stepRestoring:      {To: []step{stepAwaitingVolume}, OnEnter: deadline(restoringDeadline)},
				stepAwaitingVolume: {To: []step{stepBinding}, OnEnter: deadline(awaitingVolumeDeadline)},
				stepBinding:        {OnEnter: deadline(bindingDeadline)},
			},
		},
	}
}

// initialDeadline is the budget of the step a machine is born in.
//
// A machine is already in its initial state when it is built, so that state's
// OnEnter never runs and the deadline the graph declares for it is never armed.
// Arming it explicitly is what stops the first step of every operation from
// being the one step that cannot time out.
const initialDeadline = validatingDeadline

// abortable reports whether an abort asked for while the operation sits on this
// step can be honored.
func abortable(current step) bool { return abortableSteps[current] }
