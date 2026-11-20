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

	ensureErr  error
	observeErr error
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
	if m.observeErr != nil {
		return volstack.StateAbsent, volstack.Artifact{}, m.observeErr
	}
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
	// The releases alone: each is preceded by the read that decides whether it
	// can be skipped, and what this is about is the order of the releases.
	if got := strings.Join(verbsIn(log, "release"), " "); got != "m2:release m1:release m0:release" {
		t.Errorf("released in the order %q, want the reverse of the bring-up", got)
	}
}

// verbsIn is the entries of one verb, in the order they happened.
func verbsIn(log []string, verb string) []string {
	var found []string
	for _, entry := range log {
		if strings.HasSuffix(entry, ":"+verb) {
			found = append(found, entry)
		}
	}
	return found
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

// A member that is already down is one a release has nothing to do to, and the
// composite walks its own members rather than going through the runner, so the
// rule the runner applies to a plan has to be applied here too.
//
// Absent is nothing of the member being there. Inactive is the state Release
// itself leaves behind. A teardown resuming over a stripe something already took
// part of down meets both, and a member that is down is a hold this host has
// already given up.
func TestMembersReleaseSkipsMembersAlreadyDown(t *testing.T) {
	var log []string
	plan := volstack.Plan{
		&memberLayer{name: "m0", log: &log, device: "nvme0n1", state: volstack.StateReady},
		&memberLayer{name: "m1", log: &log, state: volstack.StateAbsent},
		&memberLayer{name: "m2", log: &log, device: "nvme2n1", state: volstack.StateInactive},
	}

	if err := NewMembers(plan).Release(context.Background(), volstack.Artifact{}); err != nil {
		t.Fatalf("Release: %v", err)
	}

	joined := strings.Join(log, " ")
	for _, down := range []string{"m1:release", "m2:release"} {
		if strings.Contains(joined, down) {
			t.Errorf("a member that was already down was released (%s):\n%s", down, joined)
		}
	}
	if !strings.Contains(joined, "m0:release") {
		t.Errorf("the member that was still up was not released:\n%s", joined)
	}
}

// The same for Destroy, and only for Absent: removing what is already gone is
// the state the caller asked for, while an inactive member's object is still
// there to remove.
func TestMembersDestroySkipsMembersAlreadyGone(t *testing.T) {
	var log []string
	plan := volstack.Plan{
		&memberLayer{name: "m0", log: &log, device: "nvme0n1", state: volstack.StateReady},
		&memberLayer{name: "m1", log: &log, state: volstack.StateAbsent},
	}

	if err := NewMembers(plan).Destroy(context.Background(), volstack.Artifact{}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	joined := strings.Join(log, " ")
	if strings.Contains(joined, "m1:destroy") {
		t.Errorf("a member that was already gone was destroyed:\n%s", joined)
	}
	if !strings.Contains(joined, "m0:destroy") {
		t.Errorf("the member that was still there was not destroyed:\n%s", joined)
	}
}

// A member whose state cannot be read is released anyway. The reading is what
// decides whether work can be skipped, not whether the hold exists, and a
// teardown that skipped on a failed read would strand it.
func TestMembersReleaseWhenAMemberCannotBeRead(t *testing.T) {
	var log []string
	plan := volstack.Plan{
		&memberLayer{name: "m0", log: &log, device: "nvme0n1", state: volstack.StateReady,
			observeErr: errors.New("sysfs unreadable")},
	}

	if err := NewMembers(plan).Release(context.Background(), volstack.Artifact{}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !strings.Contains(strings.Join(log, " "), "m0:release") {
		t.Errorf("a member that could not be read was not released:\n%v", log)
	}
}
