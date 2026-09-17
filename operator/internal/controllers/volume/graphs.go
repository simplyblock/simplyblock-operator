// The state graph of the PersistentVolumeOps action, declared as data.
//
// It is a MultiConfig although the kind carries one action, and not because a
// second one is coming: every other Ops controller in this group drives its
// steps through a MultiConfig keyed by action, and a kind that read differently
// for having one entry would make a reader check whether the difference meant
// something. A MultiConfig with one entry costs nothing.
//
// The graph is built per operation rather than shared, because one of its
// bounds is computed. The copy's deadline scales with how many volumes the
// migrated subsystem holds, which is only known once the migration has been
// created, so the hook closes over that count — the shape atlas-lib's
// statemachine documents for exactly this case.
//
// design-persistentvolumeops.md §5 is the specification.

package volume

import (
	"context"
	"time"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// step is the operation's step type, aliased so the graph literal below reads
// as the graph rather than as a wall of package qualifiers.
type step = simplyblockv1alpha2.PersistentVolumeOpsStep

const (
	stepValidating = simplyblockv1alpha2.PersistentVolumeOpsStepValidating
	stepMigrating  = simplyblockv1alpha2.PersistentVolumeOpsStepMigrating
	stepVerifying  = simplyblockv1alpha2.PersistentVolumeOpsStepVerifying
)

// actionMigrate is the MultiConfig key for the one action this kind carries.
const actionMigrate = statemachine.Action(simplyblockv1alpha2.PersistentVolumeOpsActionMigrate)

// How long each step may take before the operation is reported as stuck.
//
// They differ by what the step is waiting on rather than by preference.
const (
	// validatingDeadline bounds everything between creating the migration and
	// continuing it: waiting for the consuming pods to be Running, and running
	// one Job per consuming node. It is finite because the control plane does
	// not hold a created-but-unstarted migration open indefinitely, and an
	// operation that sat in this step past that window would continue a
	// migration the backend has already given up on.
	validatingDeadline = 15 * time.Minute

	// copyBaseDeadline is what a migration of one volume gets, and the floor
	// under every larger one.
	copyBaseDeadline = 30 * time.Minute

	// copyPerMemberDeadline is added for each volume the subsystem holds. A
	// migration is addressed by the subsystem, so every sibling volume moves
	// along with the named one and each of them is data to copy.
	//
	// There is no ceiling. A subsystem large enough to make this hours is one
	// whose migration genuinely takes hours, and capping the bound would fail
	// it for being big rather than for being stuck — which is the one thing a
	// deadline here is for.
	copyPerMemberDeadline = 10 * time.Minute

	// verifyingDeadline bounds the cleanup: deleting the validation Jobs and
	// confirming that no path they connected is left behind. It is short
	// because nothing in it waits on the data path, and it is bounded at all
	// because a cleanup that cannot finish has to be visible — a path connected
	// with nothing tracking it blocks every later migration of the volume, and
	// has.
	verifyingDeadline = 10 * time.Minute
)

// deadline is the entry hook every state here carries: it sets the step's
// budget and performs nothing. The side effect of a step is performed on the
// pass that follows, against the step the entry's patch persisted, which is
// where the write-ahead record is needed and what it records.
func deadline(d time.Duration) statemachine.TransitionFunc[step] {
	return func(context.Context, step, step) (time.Duration, error) { return d, nil }
}

// copyDeadline is the bound on the copy, given how many volumes the subsystem
// holds. A count of zero is a migration whose creation has not reported one
// yet, and it gets the base bound rather than none: a step that cannot time out
// is the failure mode the bounds exist to prevent.
func copyDeadline(members int32) time.Duration {
	if members < 1 {
		members = 1
	}
	return copyBaseDeadline + time.Duration(members)*copyPerMemberDeadline
}

// graphs declares the state graph of each action, for a subsystem of the given
// member count.
func graphs(members int32) statemachine.MultiConfig[step] {
	return statemachine.MultiConfig[step]{
		actionMigrate: {
			Initial: stepValidating,
			States: map[step]statemachine.StateDef[step]{
				// Validating and Migrating are abortable because both have
				// something to take back: a backend migration that has not
				// copied anything yet, and the paths its creation published on
				// every consuming host.
				stepValidating: {
					To:        []step{stepMigrating},
					Abortable: true,
					OnEnter:   deadline(validatingDeadline),
				},
				stepMigrating: {
					To:        []step{stepVerifying},
					Abortable: true,
					OnEnter:   deadline(copyDeadline(members)),
				},
				// Verifying is not. The copy has finished, the volume has
				// moved, and what is left is the cleanup that makes the move
				// safe — so stopping here would leave exactly the state this
				// step exists to prevent.
				stepVerifying: {OnEnter: deadline(verifyingDeadline)},
			},
		},
	}
}

// UnabortableSteps are the declared steps an abort cannot be honored from,
// sorted.
//
// It is exported for the DELETE guard on this kind, which asks the same
// question this package's unwind asks: a deletion may not express something
// spec.abort could not, so both channels read one graph (design-crd-model.md
// §3.1). The guard has a step out of a status and no machine, which is the
// whole reason this reads the graph rather than the machine the reconciler
// holds.
//
// The member count is irrelevant to the answer, so the guard does not have to
// know one: which steps can be stopped is a property of the graph's shape, and
// the count only sets how long one of them may take.
func UnabortableSteps() []step {
	return statemachine.UnabortableMultiStates(graphs(0))
}
