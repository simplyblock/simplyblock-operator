package nvmeof

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

// isMultiNamespace asks whether a device's subsystem can hold more than one
// namespace. A variable so tests can substitute the Identify Controller command
// the answer may need.
var isMultiNamespace = func(d nvme.Device) (bool, error) { return d.IsMultiNamespace() }

// DetachOutcome reports what DetachDevice did, because "nothing" is a correct
// and expected result: a subsystem shared with other volumes has to stay up.
type DetachOutcome struct {
	// Disconnected reports whether the subsystem was actually torn down.
	Disconnected bool
	// SharedSubsystem reports why it was not: the subsystem can hold more than
	// one namespace, so tearing it down is never one volume's decision.
	SharedSubsystem bool
}

// DetachDevice releases a volume's fabric connection, or deliberately does
// nothing when the volume's subsystem is one that can be shared.
//
// It exists because the wrong answer here destroys data that is not the
// caller's: disconnecting an NVMe-oF subsystem tears down every namespace on
// it, and simplyblock's "namespaced" volumes put many volumes on one subsystem.
// A CSI NodeUnstage that disconnects unconditionally rips the block device out
// from under every co-tenant, on nodes where nothing looks wrong until I/O
// fails. So the question is asked here, once, rather than left to each caller.
//
// The question asked is nvme.Device.IsMultiNamespace — *can* this subsystem hold
// other volumes — and not whether it currently does. Enumerating the neighbours
// only describes the moment it was looked at: a namespace can join a shared
// subsystem at any time, including between the check and the disconnect, and
// then a "no co-tenants right now" answer would have been correct and still
// destructive. A subsystem provisioned to be shared is therefore never
// disconnected on one volume's behalf, even while it happens to hold only this
// one. Callers that want to name the current neighbours for an event can ask
// nvme.Device.CoTenants, whose answer is inherently a snapshot.
//
// A question that cannot be answered is never assumed away: the Identify
// Controller command the answer may need requires a live controller
// (errs.ErrNotConnected without one) and Linux (errs.ErrUnsupported elsewhere),
// and either way DetachDevice returns the error without touching the fabric.
// Reaping a subsystem whose controllers are all dead is therefore an explicit
// act — Connector.Disconnect — not something this function does by default.
//
// Unmounting, and releasing the block devices of a volume that surfaced more
// than once (see nvme.Device.Siblings), stay with the caller: this function
// owns the fabric, not the filesystem.
func DetachDevice(ctx context.Context, c Connector, dev nvme.Device) (DetachOutcome, error) {
	nqn := dev.Subsystem.NQN
	if nqn == "" {
		return DetachOutcome{}, fmt.Errorf("detach %s: no subsystem NQN: %w",
			dev.Namespace.Name, errs.ErrUnsupported)
	}

	shared, err := isMultiNamespace(dev)
	if err != nil {
		return DetachOutcome{}, fmt.Errorf("detach %s: cannot tell whether the subsystem "+
			"is shared with other volumes: %w", nqn, err)
	}
	if shared {
		return DetachOutcome{SharedSubsystem: true}, nil
	}

	if err := c.Disconnect(ctx, nqn); err != nil {
		return DetachOutcome{}, fmt.Errorf("detach %s: %w", nqn, err)
	}
	return DetachOutcome{Disconnected: true}, nil
}
