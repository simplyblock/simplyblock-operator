// What the members layer has to guarantee.
//
// Order, mostly. A stripe over the same members in a different order is a
// different device, so the order the plan recorded is the order they are brought
// up in and the order their devices are handed upward.

package layers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/volstack"
)

// memberLayer stands in for one fabric layer beneath the composite.
type memberLayer struct {
	name    string
	log     *[]string
	device  string
	state   volstack.State
	healthy bool

	ensureErr error
}

func (m *memberLayer) Name() string { return m.name }

func (m *memberLayer) note(verb string) { *m.log = append(*m.log, m.name+":"+verb) }

func (m *memberLayer) own() volstack.Artifact {
	if m.state == volstack.StateAbsent {
		return volstack.Artifact{}
	}
	return volstack.Artifact{Devices: []blockdev.Device{{Name: m.device, Path: "/dev/" + m.device}}}
}

func (m *memberLayer) Observe(context.Context, volstack.Artifact) (volstack.State, volstack.Artifact, error) {
	m.note("observe")
	return m.state, m.own(), nil
}

func (m *memberLayer) Ensure(context.Context, volstack.Artifact) (volstack.Artifact, error) {
	m.note("ensure")
	if m.ensureErr != nil {
		return volstack.Artifact{}, m.ensureErr
	}
	return volstack.Artifact{Devices: []blockdev.Device{{Name: m.device, Path: "/dev/" + m.device}}}, nil
}

func (m *memberLayer) Release(context.Context, volstack.Artifact) error {
	m.note("release")
	return nil
}

func (m *memberLayer) Destroy(context.Context, volstack.Artifact) error {
	m.note("destroy")
	return nil
}

func (m *memberLayer) Healthy(context.Context, volstack.Artifact) (bool, error) {
	m.note("healthy")
	return m.healthy, nil
}

func (m *memberLayer) Heal(context.Context, volstack.Artifact, volstack.Artifact) error {
	m.note("heal")
	return nil
}

func threeMembers(log *[]string, state volstack.State) volstack.Plan {
	return volstack.Plan{
		&memberLayer{name: "m0", log: log, device: "nvme0n1", state: state, healthy: true},
		&memberLayer{name: "m1", log: log, device: "nvme1n1", state: state, healthy: true},
		&memberLayer{name: "m2", log: log, device: "nvme2n1", state: state, healthy: true},
	}
}

// Ensure brings the members up in the order the plan recorded, and hands their
// devices upward in that same order.
func TestMembersEnsureKeepsTheRecordedOrder(t *testing.T) {
	var log []string
	m := NewMembers(threeMembers(&log, volstack.StateReady))

	art, err := m.Ensure(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if got := strings.Join(log, " "); got != "m0:ensure m1:ensure m2:ensure" {
		t.Errorf("brought up in the order %q", got)
	}
	if len(art.Devices) != 3 {
		t.Fatalf("exposed %d devices, want 3", len(art.Devices))
	}
	for i, want := range []string{"nvme0n1", "nvme1n1", "nvme2n1"} {
		if art.Devices[i].Name != want {
			t.Errorf("device %d is %s, want %s: a stripe over the same members in another order is another device",
				i, art.Devices[i].Name, want)
		}
	}
}

// Release reverses, so a member is let go only after whatever was built on top
// of the composite has been.
func TestMembersReleaseReverses(t *testing.T) {
	var log []string
	m := NewMembers(threeMembers(&log, volstack.StateReady))

	if err := m.Release(context.Background(), volstack.Artifact{}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := strings.Join(log, " "); got != "m2:release m1:release m0:release" {
		t.Errorf("released in the order %q, want the reverse of the bring-up", got)
	}
}

// The composite is only as present as its members. A stripe missing one member
// is not a stripe, so anything short of all of them is partial rather than ready.
func TestMembersObserveAggregates(t *testing.T) {
	cases := []struct {
		name   string
		states []volstack.State
		want   volstack.State
	}{
		{"every member ready", []volstack.State{volstack.StateReady, volstack.StateReady}, volstack.StateReady},
		{"no member present", []volstack.State{volstack.StateAbsent, volstack.StateAbsent}, volstack.StateAbsent},
		{"one member missing", []volstack.State{volstack.StateReady, volstack.StateAbsent}, volstack.StatePartial},
		{"one member not serving", []volstack.State{volstack.StateReady, volstack.StatePartial}, volstack.StatePartial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var log []string
			plan := make(volstack.Plan, 0, len(tc.states))
			for i, st := range tc.states {
				plan = append(plan, &memberLayer{
					name: string(rune('a' + i)), log: &log,
					device: "nvme" + string(rune('0'+i)) + "n1", state: st,
				})
			}
			state, _, err := NewMembers(plan).Observe(context.Background(), volstack.Artifact{})
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if state != tc.want {
				t.Errorf("state = %s, want %s", state, tc.want)
			}
		})
	}
}

// A composite that could not bring every member up exposes nothing, because a
// partial set of devices is not a stripe and a layer above must not build on it.
func TestMembersEnsureFailsIfAMemberDoes(t *testing.T) {
	var log []string
	plan := threeMembers(&log, volstack.StateReady)
	plan[1].(*memberLayer).ensureErr = errors.New("no path to the namespace")

	art, err := NewMembers(plan).Ensure(context.Background(), volstack.Artifact{})
	if err == nil {
		t.Fatal("Ensure succeeded although a member failed")
	}
	if len(art.Devices) != 0 {
		t.Errorf("a failed composite exposed %d devices", len(art.Devices))
	}
}

// The composite is healthy only when every member is, and a heal repairs the
// members that are not.
func TestMembersHealRepairsOnlyTheBrokenMembers(t *testing.T) {
	var log []string
	plan := threeMembers(&log, volstack.StateReady)
	plan[1].(*memberLayer).healthy = false

	m := NewMembers(plan)
	healthy, err := m.Healthy(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Healthy: %v", err)
	}
	if healthy {
		t.Fatal("a composite with a broken member reported healthy")
	}

	log = nil
	if err := m.Heal(context.Background(), volstack.Artifact{}, volstack.Artifact{}); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if !logged(log, "m1:heal") {
		t.Errorf("the broken member was not healed:\n%s", strings.Join(log, " "))
	}
	if logged(log, "m0:heal") || logged(log, "m2:heal") {
		t.Errorf("a healthy member was healed anyway:\n%s", strings.Join(log, " "))
	}
}

// logged reports whether the log holds this call, matched whole.
//
// Whole rather than as a substring because the verbs share prefixes: `heal` is
// the start of `healthy`, so a substring test answers yes for a layer that was
// only asked whether it was serving.
func logged(log []string, call string) bool {
	for _, entry := range log {
		if entry == call {
			return true
		}
	}
	return false
}

// The sub-plan is what the record has to carry, in order, because the order
// cannot be recovered from a set and a failover that reassembles the members
// differently assembles a different device.
func TestMembersExposesItsSubPlanForTheRecord(t *testing.T) {
	var log []string
	plan := threeMembers(&log, volstack.StateReady)
	m := NewMembers(plan)

	got := m.Members()
	if len(got) != 3 {
		t.Fatalf("reported %d members, want 3", len(got))
	}
	for i, want := range []string{"m0", "m1", "m2"} {
		if got[i].Name() != want {
			t.Errorf("member %d is %s, want %s", i, got[i].Name(), want)
		}
	}
}
