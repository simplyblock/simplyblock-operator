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

		// No observation here. Ensure has to observe anyway in order to know
		// what to converge, and a layer's observation is the expensive step: on
		// a degraded device it is a read of the device itself. Observing here as
		// well would read it twice per stage and take two answers to one
		// question, so the layer is asked once and dispatches on what it found.
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
	// Best effort: the bring-up is already failing, and a survey that cannot
	// complete is no reason to leave the layers beneath it held. A layer
	// resolves the object it owns from its own parameters, so what is lost when
	// the survey fails is the input it sits on rather than its own identity.
	inputs, _, err := r.survey(ctx, brought)
	if err != nil {
		inputs = make([]Artifact, len(brought))
	}
	for i := len(brought) - 1; i >= 0; i-- {
		_ = brought[i].Release(ctx, inputs[i])
	}
}

// Down releases the plan top to bottom and removes the record.
//
// Release returning without error does not mean the object is gone: a fabric
// layer legitimately leaves its device present when the subsystem is shared. So
// this asserts nothing about the state a released layer is in, and removes the
// record either way.
//
// A survey that cannot complete is no reason to leave the stack up, which is the
// reasoning unwind already carries and the case this one meets more often.
// Total path loss takes the device out from under a live stack: the mount above
// it answers EIO, and the filesystem layer reports that as an error rather than
// as a state, so the survey fails on the very teardown every layer's force path
// was written for. What the failed survey costs is the input each layer sits on
// and the knowledge of which layers are already gone — neither of which a
// release needs, since a layer resolves the object it owns from its own
// parameters and Release on an absent one is a no-op.
func (r *Runner) Down(ctx context.Context, handle string, plan Plan) error {
	attempted := r.attempted(handle, len(plan))

	inputs, states, err := r.survey(ctx, plan)
	if err != nil {
		inputs, states = make([]Artifact, len(plan)), nil
	}

	for i := len(plan) - 1; i >= 0; i-- {
		if attempted != nil && !attempted[i] {
			continue
		}
		// Already down, so a release has nothing to do to it. Absent is nothing
		// of the layer being there at all, and Inactive is the state Release
		// itself leaves behind: complete, and not mapped on this host. A
		// teardown resuming over a stack something already took part of down
		// meets both, and acting on either is work against an object that is in
		// the state the caller is asking for.
		//
		// Said out loud, because otherwise it cannot be told apart from the
		// teardown that did the work: both return the same nothing.
		//
		// Partial is not among them, and must not be. A fabric device that is
		// present and cannot serve is still the device a release has to detach.
		if states != nil && (states[i] == StateAbsent || states[i] == StateInactive) {
			Infof("volstack: release %s skipped, already down (%s)", plan[i].Name(), states[i])
			continue
		}
		if err := plan[i].Release(ctx, inputs[i]); err != nil {
			return fmt.Errorf("volstack: release %s: %w", plan[i].Name(), err)
		}
	}
	return r.store.Remove(handle)
}

// Heal repairs the layers that report themselves unhealthy, bottom to top.
//
// It never recreates: the data already exists, which is why this and not Up is
// what a restage runs.
//
// A layer that cannot be observed is one to repair rather than one to report,
// where it can repair itself. That is the case this verb exists for: total path
// loss removes the device, the mount above it answers EIO, and the filesystem
// layer's Observe reports that as an error rather than as a state — while its
// Healthy answers "not healthy" for the same mount precisely so that a heal
// runs. Reading the Observe error as fatal defeats the layer's own intent, and
// what it costs is the restage: the volume is never republished and the
// workload stays down. A layer that implements no Healer keeps the old
// behavior, because nothing in the plan can repair it and the layers above it
// would be walked against a reading nobody has.
func (r *Runner) Heal(ctx context.Context, handle string, plan Plan) error {
	below := Artifact{}
	for _, layer := range plan {
		// A read, not a bring-up: a heal repairs what is broken and must not
		// build the layers it passes through on the way there.
		_, own, err := layer.Observe(ctx, below)
		if err != nil {
			healer, ok := layer.(Healer)
			if !ok {
				return fmt.Errorf("volstack: observe %s while healing: %w", layer.Name(), err)
			}
			if err := healer.Heal(ctx, below, Artifact{}); err != nil {
				return fmt.Errorf("volstack: heal %s: %w", layer.Name(), err)
			}
			// Read it again, because the layers above this one sit on what it
			// exposes and a heal that worked is one that can now be read.
			if _, own, err = layer.Observe(ctx, below); err != nil {
				return fmt.Errorf("volstack: observe %s after healing it: %w", layer.Name(), err)
			}
			below = own
			continue
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
			// Passed through, so read what it exposes rather than converging it.
			_, above, err := layer.Observe(ctx, below)
			if err != nil {
				return fmt.Errorf("volstack: observe %s while growing: %w", layer.Name(), err)
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

// Observe walks a live stack bottom to top without changing any of it, and
// returns what the topmost layer currently exposes.
//
// It is the read behind an operation that acts on a staged volume rather than
// building one: a raw block volume is bind-mounted from the device its fabric
// layer exposes, and a publish may not converge the stack on the way to finding
// it. Up would, and on a volume whose paths are gone it would block doing so.
func (r *Runner) Observe(ctx context.Context, plan Plan) (Artifact, error) {
	below := Artifact{}
	for _, layer := range plan {
		_, own, err := layer.Observe(ctx, below)
		if err != nil {
			return Artifact{}, fmt.Errorf("volstack: observe %s: %w", layer.Name(), err)
		}
		below = own
	}
	return below, nil
}

// Destroy removes the plan's durable objects, top to bottom, so a layer goes
// before what it sits on. Only a deletion path calls it, never an unstage.
func (r *Runner) Destroy(ctx context.Context, handle string, plan Plan) error {
	inputs, states, err := r.survey(ctx, plan)
	if err != nil {
		return err
	}
	for i := len(plan) - 1; i >= 0; i-- {
		// Nothing of this layer is there, so there is nothing of it to remove.
		// The same rule Down applies to Release, for the same reason and read
		// off the same survey: an absent layer's object is already in the state
		// the caller is asking for.
		//
		// It is not a corner case on this path, it is the normal one. The only
		// caller destroys after releasing, and releasing is what detaches the
		// fabric, so every layer standing on that fabric is absent by the time
		// this walk reaches it. Destroying one anyway means removing an object
		// whose metadata lives on a device that is no longer attached, which
		// cannot work: on an LVM stack it surfaced as `lvremove: Volume group
		// "vol-..." not found`, failing the RPC for a volume that had in fact
		// been torn down correctly.
		if states[i] == StateAbsent {
			continue
		}
		if err := plan[i].Destroy(ctx, inputs[i]); err != nil {
			return fmt.Errorf("volstack: destroy %s: %w", plan[i].Name(), err)
		}
	}
	return r.store.Remove(handle)
}

// survey walks the stack bottom to top once, reporting what each layer is handed
// and what state each one is in.
//
// It observes rather than ensures, which is the whole point: this runs on the
// teardown and deletion paths, and neither may bring a layer up on the way to the
// one it is acting on. A teardown that ensured would reconnect a fabric before
// detaching it, and would block doing so on the volume whose paths are already
// gone, which is when an unstage arrives.
//
// One pass rather than re-deriving each layer's input as it is reached. That is
// n observations instead of n(n+1)/2, and each one is real work, but the reason
// is consistency rather than cost: every re-observation is a fresh look at a
// world that changes underneath it, so a stack walked that way can hand one layer
// an artifact that disagrees with what the layer above it was given.
//
// Layer inputs, not layer outputs: inputs[i] is what layer i sits on, so
// inputs[0] is the zero artifact and inputs[i+1] is what layer i exposes.
func (r *Runner) survey(ctx context.Context, plan Plan) (inputs []Artifact, states []State, err error) {
	inputs = make([]Artifact, len(plan))
	states = make([]State, len(plan))

	below := Artifact{}
	for i, layer := range plan {
		inputs[i] = below
		state, own, err := layer.Observe(ctx, below)
		if err != nil {
			return nil, nil, fmt.Errorf("volstack: observe %s: %w", layer.Name(), err)
		}
		states[i] = state
		below = own
	}
	return inputs, states, nil
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
			if params := recorder.Params(); params != nil {
				raw, err := json.Marshal(params)
				if err != nil {
					return Record{}, fmt.Errorf("volstack: encode the parameters of %s: %w", layer.Name(), err)
				}
				entry.Params = raw
			}
		}
		// A fan-in layer's sub-plan is recorded in order, because a stripe
		// assembled over the same members in another order is another device.
		if composite, ok := layer.(Composite); ok {
			sub, err := recordFor(handle, composite.Members())
			if err != nil {
				return Record{}, fmt.Errorf("volstack: record the members of %s: %w", layer.Name(), err)
			}
			entry.Members = sub.Plan
		}
		rec.Plan = append(rec.Plan, entry)
	}
	if len(rec.Plan) == 0 {
		return Record{}, errors.New("volstack: a plan with no layers builds nothing")
	}
	return rec, nil
}
