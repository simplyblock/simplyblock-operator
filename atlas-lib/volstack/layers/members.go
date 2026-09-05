// The members layer: several namespaces where every other plan has one.
//
// It is the only non-linear shape any plan has, and expressing it as a composite
// layer is what lets the rest of the stack stay a list. A dependency graph would
// make the ordering of everything else implicit in order to describe the one
// place fan-in occurs.

package layers

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/volstack"
)

// Members is n layers presented to the stack above as one.
//
// The order it holds them in is contract rather than convenience: a stripe
// assembled over the same members in a different order is a different device, so
// the order the plan recorded is the order they are brought up in and the order
// their devices are handed upward. It is also why the plan is recorded rather
// than rebuilt from a StorageClass, since an order cannot be recovered from a
// set.
type Members struct {
	members volstack.Plan
}

// NewMembers returns a composite over the given layers, in the order they are to
// be assembled.
func NewMembers(members volstack.Plan) *Members { return &Members{members: members} }

// Name is what the record calls this layer.
func (m *Members) Name() string { return "members" }

// Members is the sub-plan, which the record carries as a field of its own so
// that a teardown replays the order rather than re-deriving it.
func (m *Members) Members() volstack.Plan { return m.members }

// Observe reports the composite as only as present as its members.
//
// A stripe missing one member is not a stripe, so anything short of all of them
// present and serving is partial: a layer above a partial set would assemble
// something over devices that are not all there.
func (m *Members) Observe(ctx context.Context, below volstack.Artifact) (volstack.State, volstack.Artifact, error) {
	devices := make([]blockdev.Device, 0, len(m.members))
	present, ready := 0, 0

	for _, member := range m.members {
		state, own, err := member.Observe(ctx, below)
		if err != nil {
			return volstack.StateAbsent, volstack.Artifact{},
				fmt.Errorf("members: observe %s: %w", member.Name(), err)
		}
		if state != volstack.StateAbsent {
			present++
			devices = append(devices, own.Devices...)
		}
		if state == volstack.StateReady {
			ready++
		}
	}

	switch {
	case present == 0:
		return volstack.StateAbsent, volstack.Artifact{}, nil
	case ready == len(m.members):
		return volstack.StateReady, volstack.Artifact{Devices: devices}, nil
	default:
		return volstack.StatePartial, volstack.Artifact{Devices: devices}, nil
	}
}

// Ensure brings every member up in order and hands their devices upward in that
// same order.
//
// One member failing brings the composite down with it, and it then exposes
// nothing: a partial set of devices is not a stripe, and a layer above one would
// assemble something over the devices that happened to arrive.
func (m *Members) Ensure(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	devices := make([]blockdev.Device, 0, len(m.members))
	for _, member := range m.members {
		own, err := member.Ensure(ctx, below)
		if err != nil {
			return volstack.Artifact{}, fmt.Errorf("members: bring up %s: %w", member.Name(), err)
		}
		devices = append(devices, own.Devices...)
	}
	return volstack.Artifact{Devices: devices}, nil
}

// Release lets the members go in the reverse of the order they were brought up
// in, and lets every one of them go even when an earlier release failed: the
// members are independent holds, and stopping at the first failure would strand
// the rest.
func (m *Members) Release(ctx context.Context, below volstack.Artifact) error {
	var firstErr error
	for i := len(m.members) - 1; i >= 0; i-- {
		if err := m.members[i].Release(ctx, below); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("members: release %s: %w", m.members[i].Name(), err)
		}
	}
	return firstErr
}

// Destroy removes the members' durable objects in reverse order. For a stack of
// fabric layers there is nothing to remove, because the namespaces belong to the
// control plane.
func (m *Members) Destroy(ctx context.Context, below volstack.Artifact) error {
	for i := len(m.members) - 1; i >= 0; i-- {
		if err := m.members[i].Destroy(ctx, below); err != nil {
			return fmt.Errorf("members: destroy %s: %w", m.members[i].Name(), err)
		}
	}
	return nil
}

// Healthy reports the composite healthy only when every member is. One member
// that cannot serve is a stripe that cannot serve.
func (m *Members) Healthy(ctx context.Context, own volstack.Artifact) (bool, error) {
	for _, member := range m.members {
		healer, ok := member.(volstack.Healer)
		if !ok {
			continue
		}
		healthy, err := healer.Healthy(ctx, own)
		if err != nil {
			return false, fmt.Errorf("members: check %s: %w", member.Name(), err)
		}
		if !healthy {
			return false, nil
		}
	}
	return true, nil
}

// Heal repairs the members that are not serving and leaves the rest alone.
func (m *Members) Heal(ctx context.Context, below, own volstack.Artifact) error {
	for _, member := range m.members {
		healer, ok := member.(volstack.Healer)
		if !ok {
			continue
		}
		healthy, err := healer.Healthy(ctx, own)
		if err != nil {
			return fmt.Errorf("members: check %s: %w", member.Name(), err)
		}
		if healthy {
			continue
		}
		if err := healer.Heal(ctx, below, own); err != nil {
			return fmt.Errorf("members: heal %s: %w", member.Name(), err)
		}
	}
	return nil
}
