// The migrate walk. It drives the phase graph of §23, running each phase's
// steps as the phase's entry hook, and it keeps its position in the record so a
// run killed anywhere resumes into the phase it was in rather than starting
// over.
//
// The two properties that make a resume safe are the state machine's, not this
// file's: restoring a snapshot runs no entry hook, so the phase a run resumes
// into does not repeat its side effects, and every step is asked whether it is
// done before it is applied, so the phase that is re-entered performs only what
// is left.

package upgrade

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/simplyblock/atlas/statemachine"
)

// PhaseTimeout bounds a phase. It is per phase rather than global because §23
// wants a phase that hangs to be distinguishable from one that is working, and
// the deadline is what a report says the difference is.
type PhaseTimeout map[Phase]time.Duration

// DefaultPhaseTimeouts is what a phase gets when the caller names none. They
// are generous: the point is to notice a phase that will never finish, not to
// cut one short on a large cluster.
var DefaultPhaseTimeouts = PhaseTimeout{
	PhaseValidating:   10 * time.Minute,
	PhaseTransforming: 30 * time.Minute,
	PhaseOwnership:    30 * time.Minute,
	PhaseHandles:      30 * time.Minute,
	PhaseDeleting:     15 * time.Minute,
	PhaseRewriting:    2 * time.Hour,
	PhaseVerifying:    15 * time.Minute,
}

// Migration runs the migrate stage.
type Migration struct {
	// Runner executes the steps and the checks.
	Runner *Runner

	// Store is where the position is kept between runs.
	Store RecordStore

	// Timeouts bound each phase. Nil means [DefaultPhaseTimeouts].
	Timeouts PhaseTimeout
}

// NewMigration builds a migration over a runner and a store.
func NewMigration(runner *Runner, store RecordStore) *Migration {
	return &Migration{Runner: runner, Store: store, Timeouts: DefaultPhaseTimeouts}
}

// Run walks the graph from wherever the record says the last run stopped, to
// completion or to the first phase that refuses.
//
// A phase that fails leaves the record on the phase it failed in rather than on
// PhaseFailed, because the next run resumes into the work that is left rather
// than into a terminal state it would have to be reset out of. PhaseFailed is
// entered only when the failure is one a rerun cannot help with.
func (m *Migration) Run(ctx context.Context) error {
	record, existed, err := m.Store.Load(ctx)
	if err != nil {
		return err
	}
	if !existed {
		record = &Record{Snapshot: statemachine.Snapshot[Phase]{State: PhasePending}}
	}

	machine, err := statemachine.NewFromSnapshot(ctx, MigrateGraph(m.hooks(record)), record.Snapshot)
	if err != nil {
		return fmt.Errorf("the migration record holds a phase this build does not declare: %w", err)
	}
	defer machine.Close()

	for {
		current := machine.CurrentState()
		if current.Terminal() {
			break
		}

		next, ok := nextPhase(current)
		if !ok {
			break
		}
		if err := machine.TransitionTo(ctx, next); err != nil {
			// The phase's own error is what the user needs. The machine's
			// wrapping says only which transition carried it.
			record.Snapshot = machine.Snapshot()
			if saveErr := m.Store.Save(ctx, record); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			return err
		}

		record.Snapshot = machine.Snapshot()
		record.Step = ""
		if err := m.Store.Save(ctx, record); err != nil {
			return err
		}
	}

	if machine.CurrentState() == PhaseCompleted {
		return m.Store.Delete(ctx)
	}
	return nil
}

// hooks binds each phase to the steps registered for it. A phase with no steps
// still has a hook, so that entering it is recorded and its deadline is armed:
// a phase nothing is registered in is a phase this release has no work for, not
// a phase that was skipped.
func (m *Migration) hooks(record *Record) map[Phase]PhaseEnter {
	hooks := make(map[Phase]PhaseEnter, len(MigratePhases))
	for _, phase := range MigratePhases {
		if phase.Terminal() || phase == PhasePending {
			continue
		}
		hooks[phase] = m.phaseHook(record)
	}
	return hooks
}

// phaseHook is one phase's entry hook: run its steps, then report how long the
// phase is allowed to take. Which phase that is comes from the hook's own
// argument rather than from the closure, because the state machine is what
// decides which state was entered.
func (m *Migration) phaseHook(record *Record) PhaseEnter {
	return func(ctx context.Context, _, to Phase) (time.Duration, error) {
		steps, err := m.Runner.Catalog.StepsInPhase(to, m.Runner.Scope.Options)
		if err != nil {
			return 0, err
		}
		m.Runner.Scope.Report.Phase(to, len(steps))

		for _, step := range steps {
			// The step is recorded before it runs, so a run killed inside a
			// step names it on the next pass. It is the one position the
			// cluster does not answer for (§22.1).
			record.Step = step.ID()
			if err := m.Runner.Apply(ctx, step); err != nil {
				return 0, err
			}
		}
		return m.timeout(to), nil
	}
}

// timeout reports the deadline for a phase.
func (m *Migration) timeout(phase Phase) time.Duration {
	if m.Timeouts != nil {
		if d, ok := m.Timeouts[phase]; ok {
			return d
		}
		return 0
	}
	return DefaultPhaseTimeouts[phase]
}

// nextPhase returns the phase that follows this one in the declared order.
func nextPhase(current Phase) (Phase, bool) {
	for i, phase := range MigratePhases {
		if phase == current && i+1 < len(MigratePhases) {
			return MigratePhases[i+1], true
		}
	}
	return "", false
}
