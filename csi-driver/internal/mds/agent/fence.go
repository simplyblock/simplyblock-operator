// The guest's fence: before the guest mounts a device it takes the device's
// reservation, so the previous active can no longer write to it
// (design-pnfs-mds-standby.md §6.4).
//
// It lives in the agent rather than in atlas-lib because the sequence is the
// metadata server's policy (which keys are its own range, when it refuses),
// while the commands are atlas-lib's nvmepr primitive.

package agent

import (
	"context"
	"errors"
	"fmt"

	export "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	"github.com/simplyblock/atlas/nvmepr"
)

// Opener opens one device's reservation interface.
type Opener func(path string) (nvmepr.Device, error)

// Fencer fences devices for one promotion. It never registers anything but Key,
// and never touches a key outside the metadata server's range, so a client is
// left as it is.
type Fencer struct {
	Open Opener
	// HostID is this guest's NVMe host identifier, which the target reports for
	// every registration this guest made.
	HostID [16]byte
	// Key is the key of this promotion (export.MDSReservationKey).
	Key uint64
	Log Logger
}

// FenceError is a refused or failed fence: the outcome the caller reports, the
// device, and the step that stopped it.
type FenceError struct {
	Outcome nfsexportrpc.PromoteOutcome
	Device  string
	Step    string
	Err     error
}

func (e *FenceError) Error() string {
	return fmt.Sprintf("fence of %s refused at %s (%s): %v", e.Device, e.Step, e.Outcome, e.Err)
}

func (e *FenceError) Unwrap() error { return e.Err }

// Fence fences every device in order and stops at the first failure. Devices
// fenced before it stay fenced.
func (f *Fencer) Fence(ctx context.Context, devices []string) *FenceError {
	run := &fenceRun{Fencer: f}
	for _, path := range devices {
		if err := run.device(ctx, path); err != nil {
			return err
		}
	}
	return nil
}

// fenceRun is one Fence call. noAbort remembers that the target answered
// Preempt and Abort with Invalid Field, so later victims go straight to the
// plain Preempt.
type fenceRun struct {
	*Fencer
	noAbort bool
}

func (r *fenceRun) fail(path, step string, err error) *FenceError {
	return &FenceError{Outcome: nfsexportrpc.PromoteFenceFailed, Device: path, Step: step, Err: err}
}

// device runs the steps of design §6.4 after the caller's connect.
func (r *fenceRun) device(ctx context.Context, path string) *FenceError {
	dev, err := r.Open(path)
	if err != nil {
		return r.fail(path, "open", err)
	}
	defer func() { _ = dev.Close() }()

	before, err := dev.Report(ctx)
	if err != nil {
		return r.fail(path, "report", err)
	}
	if refusal := r.refuse(path, before); refusal != nil {
		return refusal
	}
	if r.settled(before) {
		return nil
	}
	if err := r.take(ctx, dev, before); err != nil {
		return r.fail(path, err.step, err.err)
	}

	after, err := dev.Report(ctx)
	if err != nil {
		return r.fail(path, "confirm", err)
	}
	if err := r.confirm(after); err != nil {
		return r.fail(path, "confirm", err)
	}
	return nil
}

// refuse is step 2: the cases in which the fence must not change anything.
func (r *fenceRun) refuse(path string, rep nvmepr.Report) *FenceError {
	tag, epoch, _ := export.ParseMDSKey(r.Key)
	for _, reg := range rep.Registrants {
		if reg.Key == r.Key && reg.HostID != r.HostID {
			return &FenceError{Outcome: nfsexportrpc.PromoteCollision, Device: path, Step: "check-key",
				Err: fmt.Errorf("host %x is registered with key %x", reg.HostID, r.Key)}
		}
	}
	for _, reg := range rep.Registrants {
		if t, e, ok := export.ParseMDSKey(reg.Key); ok && t == tag && e > epoch {
			return &FenceError{Outcome: nfsexportrpc.PromoteSuperseded, Device: path, Step: "check-epoch",
				Err: fmt.Errorf("key %x of epoch %d is registered, above this promotion's epoch %d", reg.Key, e, epoch)}
		}
	}
	if h, held := rep.Holder(); held && !export.IsMDSKey(h.Key) {
		return r.fail(path, "check-holder",
			fmt.Errorf("key %x holds the reservation, which is outside the metadata server range", h.Key))
	}
	return nil
}

// settled reports a device this guest's key already holds with no other
// metadata server key registered, which needs no command.
func (r *fenceRun) settled(rep nvmepr.Report) bool {
	h, held := rep.Holder()
	return held && h.Key == r.Key && h.HostID == r.HostID && len(r.strangers(rep)) == 0
}

// strangers are the distinct metadata server keys of other hosts, which the
// fence removes. A key of this host under another epoch is not among them: the
// replacing Register drops it.
func (r *fenceRun) strangers(rep nvmepr.Report) []uint64 {
	var keys []uint64
	for _, reg := range rep.Registrants {
		if !export.IsMDSKey(reg.Key) || reg.Key == r.Key || reg.HostID == r.HostID {
			continue
		}
		if !contains(keys, reg.Key) {
			keys = append(keys, reg.Key)
		}
	}
	return keys
}

func contains(keys []uint64, k uint64) bool {
	for _, have := range keys {
		if have == k {
			return true
		}
	}
	return false
}

type stepError struct {
	step string
	err  error
}

// take is steps 3 to 5: register, acquire or preempt the holder, and preempt
// every other metadata server key.
func (r *fenceRun) take(ctx context.Context, dev nvmepr.Device, rep nvmepr.Report) *stepError {
	if !r.registered(rep) {
		// Replace with the new key as the old one, ignoring the existing key: a
		// plain Register is refused for a host registered under another key.
		if err := dev.Register(ctx, r.Key, r.Key, true); err != nil {
			return &stepError{"register", err}
		}
	}
	holder, held := rep.Holder()
	switch {
	case held && holder.HostID == r.HostID:
		// this host holds under an earlier boot's key, which Register replaced
	case held:
		if err := r.preempt(ctx, dev, holder.Key); err != nil {
			return &stepError{"preempt", err}
		}
	default:
		if err := dev.Acquire(ctx, r.Key, nvmepr.TypeExclusiveAccessRegOnly); err != nil {
			return &stepError{"acquire", err}
		}
	}
	for _, key := range r.strangers(rep) {
		if held && key == holder.Key {
			continue
		}
		if err := r.preempt(ctx, dev, key); err != nil {
			return &stepError{"preempt", err}
		}
	}
	return nil
}

// registered reports whether this host is already registered with Key.
func (r *fenceRun) registered(rep nvmepr.Report) bool {
	for _, reg := range rep.Registrants {
		if reg.Key == r.Key && reg.HostID == r.HostID {
			return true
		}
	}
	return false
}

// preempt takes victim's registration, with Preempt and Abort and a plain
// Preempt only where the target answers Invalid Field. Any other error from
// the abort form fails the fence: the preemption may have taken effect without
// the target confirming that the victim's commands finished.
func (r *fenceRun) preempt(ctx context.Context, dev nvmepr.Device, victim uint64) error {
	if !r.noAbort {
		err := dev.Preempt(ctx, r.Key, victim, nvmepr.TypeExclusiveAccessRegOnly, true)
		if err == nil || !errors.Is(err, nvmepr.ErrInvalidField) {
			return err
		}
		r.noAbort = true
		if r.Log != nil {
			r.Log.Infof("fence: the target does not accept Preempt and Abort, preempting without it")
		}
	}
	return dev.Preempt(ctx, r.Key, victim, nvmepr.TypeExclusiveAccessRegOnly, false)
}

// confirm is step 6: this key holds and no other metadata server key is left.
func (r *fenceRun) confirm(rep nvmepr.Report) error {
	h, held := rep.Holder()
	if !held || h.Key != r.Key || h.HostID != r.HostID {
		return fmt.Errorf("the reservation is not held by key %x after the fence", r.Key)
	}
	for _, reg := range rep.Registrants {
		if export.IsMDSKey(reg.Key) && reg.Key != r.Key {
			return fmt.Errorf("key %x is still registered after the fence", reg.Key)
		}
	}
	return nil
}
