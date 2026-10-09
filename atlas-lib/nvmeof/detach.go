package nvmeof

import (
	"context"
	"errors"
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
// The question asked is nvme.Device.IsMultiNamespace, meaning *can* this
// subsystem hold other volumes, and not whether it currently does. Enumerating the neighbors
// only describes the moment it was looked at: a namespace can join a shared
// subsystem at any time, including between the check and the disconnect, and
// then a "no co-tenants right now" answer would have been correct and still
// destructive. A subsystem provisioned to be shared is therefore never
// disconnected on one volume's behalf, even while it happens to hold only this
// one. Callers that want to name the current neighbors for an event can ask
// nvme.Device.CoTenants, whose answer is inherently a snapshot.
//
// A question that cannot be answered is never assumed away: the Identify
// Controller command the answer may need requires a live controller
// (errs.ErrNotConnected without one) and Linux (errs.ErrUnsupported elsewhere),
// and either way DetachDevice returns the error without touching the fabric.
// Reaping a subsystem whose controllers are all dead is therefore an explicit
// act, Connector.Disconnect, and not something this function does by default.
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

// ReleaseDeletedVolume releases the fabric connection of a volume the control
// plane has already deleted, given the NQN of the subsystem named after it.
//
// DetachDevice cannot do it. The target removed the subsystem with the volume,
// so this host's controllers are reconnecting rather than live, and the
// Identify that tells a dedicated subsystem from a shareable one needs a live
// controller. Without this the controllers retry against the removed subsystem
// until their loss timeout, and an unstage that waits on them never finishes.
//
// The subsystem is torn down only when nothing says another volume uses it:
// no namespace with another UUID attached, and no live controller reporting it
// as shareable. With no live controller left, a shareable subsystem serves no
// co-tenant either, so reaping it takes nothing away. Nothing attached under
// subsystemNQN is success: there is nothing to release.
func ReleaseDeletedVolume(
	ctx context.Context, c Connector, subs nvme.SubsystemResolver, subsystemNQN, volumeUUID string,
) (DetachOutcome, error) {
	s, err := subs.ByNQN(ctx, subsystemNQN)
	if errors.Is(err, errs.ErrNotFound) {
		return DetachOutcome{}, nil
	}
	if err != nil {
		return DetachOutcome{}, fmt.Errorf("release %s: %w", subsystemNQN, err)
	}
	for _, ns := range s.Namespaces {
		if ns.UUID != "" && ns.UUID != volumeUUID {
			return DetachOutcome{SharedSubsystem: true}, nil
		}
	}

	shared, err := isMultiNamespace(nvme.Device{Subsystem: s})
	switch {
	case err != nil && !errors.Is(err, errs.ErrNotConnected):
		return DetachOutcome{}, fmt.Errorf("release %s: cannot tell whether the subsystem "+
			"is shared with other volumes: %w", subsystemNQN, err)
	case err == nil && shared:
		return DetachOutcome{SharedSubsystem: true}, nil
	}

	if err := c.Disconnect(ctx, subsystemNQN); err != nil {
		return DetachOutcome{}, fmt.Errorf("release %s: %w", subsystemNQN, err)
	}
	return DetachOutcome{Disconnected: true}, nil
}
