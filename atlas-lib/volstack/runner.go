// Bringing a stack up, and taking it down.
//
// The order is the contract. Up walks the plan bottom to top and unwinds with
// Release, never Destroy, because a mkfs that failed must not trigger a vgremove
// over data a misfiring format check failed to see. Down walks it top to bottom
// and tolerates a foundation that is already gone, which is the normal case
// after total path loss rather than an edge case.

package volstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Runner brings a volume's stack up and down against a record of what was
// planned.
type Runner struct {
	store *Store
}

// NewRunner returns a runner recording into store.
func NewRunner(store *Store) *Runner { return &Runner{store: store} }

// Up brings the plan up bottom to top and returns what the topmost layer
// exposes, which is what the RPC acts on.
//
// A failure releases the layers already brought up and leaves the record behind,
// so a stack left partly up is removable rather than stranded. That is not an
// error state needing resolution: NodeStageVolume is retried, every verb is
// convergent, and the next attempt observes what is there and continues.
func (r *Runner) Up(ctx context.Context, handle string, plan Plan) (Artifact, error) {
	rec, err := recordFor(handle, plan)
	if err != nil {
		return Artifact{}, err
	}
	// Before any side effect. A crash between a fabric connect and the recording
	// of that connect leaves paths attached that nothing will ever release.
	if err := r.store.Write(rec); err != nil {
		return Artifact{}, err
	}

	below := Artifact{}
	for i, layer := range plan {
		// Before this layer's side effect, for the same reason.
		if err := r.store.MarkAttempted(handle, i); err != nil {
			return Artifact{}, err
		}

		if _, err := layer.Observe(ctx, below); err != nil {
			r.unwind(ctx, plan[:i])
			return Artifact{}, fmt.Errorf("volstack: observe %s: %w", layer.Name(), err)
		}
		above, err := layer.Ensure(ctx, below)
		if err != nil {
			r.unwind(ctx, plan[:i])
			return Artifact{}, fmt.Errorf("volstack: bring up %s: %w", layer.Name(), err)
		}
		below = above
	}
	return below, nil
}

// unwind releases the layers a failed bring-up already brought up, top-down.
//
// It never calls Destroy. A release that fails is reported and the walk
// continues, because stopping would strand the layers beneath it and there is
// nothing left to protect by halting: the caller is already failing.
func (r *Runner) unwind(ctx context.Context, brought Plan) {
	for i := len(brought) - 1; i >= 0; i-- {
		below, err := r.below(ctx, brought, i)
		if err != nil {
			continue
		}
		_ = brought[i].Release(ctx, below)
	}
}

// Down releases the plan top to bottom and removes the record.
//
// Release returning without error does not mean the object is gone: a fabric
// layer legitimately leaves its device present when the subsystem is shared. So
// this asserts nothing about the state a released layer is in, and removes the
// record either way.
func (r *Runner) Down(ctx context.Context, handle string, plan Plan) error {
	attempted := r.attempted(handle, len(plan))

	for i := len(plan) - 1; i >= 0; i-- {
		if attempted != nil && !attempted[i] {
			continue
		}
		below, err := r.below(ctx, plan, i)
		if err != nil {
			return err
		}
		state, err := plan[i].Observe(ctx, below)
		if err != nil {
			return fmt.Errorf("volstack: observe %s: %w", plan[i].Name(), err)
		}
		if state == StateAbsent {
			continue
		}
		if err := plan[i].Release(ctx, below); err != nil {
			return fmt.Errorf("volstack: release %s: %w", plan[i].Name(), err)
		}
	}
	return r.store.Remove(handle)
}

// Heal repairs the layers that report themselves unhealthy, bottom to top.
//
// It never recreates: the data already exists, which is why this and not Up is
// what a restage runs.
func (r *Runner) Heal(ctx context.Context, handle string, plan Plan) error {
	below := Artifact{}
	for _, layer := range plan {
		own, err := layer.Ensure(ctx, below)
		if err != nil {
			return fmt.Errorf("volstack: resolve %s while healing: %w", layer.Name(), err)
		}

		healer, ok := layer.(Healer)
		if !ok {
			below = own
			continue
		}
		healthy, err := healer.Healthy(ctx, own)
		if err != nil {
			return fmt.Errorf("volstack: check %s: %w", layer.Name(), err)
		}
		if !healthy {
			if err := healer.Heal(ctx, below, own); err != nil {
				return fmt.Errorf("volstack: heal %s: %w", layer.Name(), err)
			}
		}
		below = own
	}
	return nil
}

// Grow enlarges the layers that can grow, bottom to top, skipping those that
// implement no Grower. A plan with none needs no node-side expansion at all,
// which is the correct answer for a pNFS client.
func (r *Runner) Grow(ctx context.Context, plan Plan) error {
	below := Artifact{}
	for _, layer := range plan {
		grower, ok := layer.(Grower)
		if !ok {
			above, err := layer.Ensure(ctx, below)
			if err != nil {
				return fmt.Errorf("volstack: resolve %s while growing: %w", layer.Name(), err)
			}
			below = above
			continue
		}
		above, err := grower.Grow(ctx, below)
		if err != nil {
			return fmt.Errorf("volstack: grow %s: %w", layer.Name(), err)
		}
		below = above
	}
	return nil
}

// Destroy removes the plan's durable objects, top to bottom, so a layer goes
// before what it sits on. Only a deletion path calls it, never an unstage.
func (r *Runner) Destroy(ctx context.Context, handle string, plan Plan) error {
	for i := len(plan) - 1; i >= 0; i-- {
		below, err := r.below(ctx, plan, i)
		if err != nil {
			return err
		}
		if err := plan[i].Destroy(ctx, below); err != nil {
			return fmt.Errorf("volstack: destroy %s: %w", plan[i].Name(), err)
		}
	}
	return r.store.Remove(handle)
}

// below re-derives layer i's input by resolving the layers beneath it, rather
// than reading a device path out of the record. A path is not stable across a
// reconnect, which is why the record holds parameters and never paths.
func (r *Runner) below(ctx context.Context, plan Plan, i int) (Artifact, error) {
	below := Artifact{}
	for j := range i {
		above, err := plan[j].Ensure(ctx, below)
		if err != nil {
			return Artifact{}, fmt.Errorf("volstack: re-derive the input of %s from %s: %w",
				plan[i].Name(), plan[j].Name(), err)
		}
		below = above
	}
	return below, nil
}

// attempted reads the per-layer markers, or reports nil when there is no record
// to read them from, which means every layer is walked.
//
// The markers are a diagnostic and an optimization rather than a correctness
// mechanism: Release on a layer whose Observe reports StateAbsent is already a
// no-op, so a teardown ignoring them reaches the same end state.
func (r *Runner) attempted(handle string, layers int) []bool {
	rec, err := r.store.Load(handle)
	if err != nil || len(rec.Plan) != layers {
		return nil
	}
	marks := make([]bool, layers)
	for i, e := range rec.Plan {
		marks[i] = e.Attempted
	}
	return marks
}

// recordFor builds the record a plan is written as, asking each layer for the
// parameters a later process needs to rebuild it.
func recordFor(handle string, plan Plan) (Record, error) {
	rec := Record{Version: RecordVersion, VolumeHandle: handle, Plan: make([]Entry, 0, len(plan))}
	for _, layer := range plan {
		entry := Entry{Layer: layer.Name()}
		if recorder, ok := layer.(Recorder); ok {
			params, err := recorder.Params()
			if err != nil {
				return Record{}, fmt.Errorf("volstack: record the parameters of %s: %w", layer.Name(), err)
			}
			if params != nil {
				raw, err := json.Marshal(params)
				if err != nil {
					return Record{}, fmt.Errorf("volstack: encode the parameters of %s: %w", layer.Name(), err)
				}
				entry.Params = raw
			}
		}
		rec.Plan = append(rec.Plan, entry)
	}
	if len(rec.Plan) == 0 {
		return Record{}, errors.New("volstack: a plan with no layers builds nothing")
	}
	return rec, nil
}
