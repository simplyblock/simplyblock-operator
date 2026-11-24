// Tests of lvolsOnSubsystem, which decides whether a broken volume's pods
// restart on their own or together with the other volumes on its subsystem.
package guardian

import (
	"context"
	"slices"
	"testing"

	atlasnvme "github.com/simplyblock/atlas/nvme"
)

const sharedNQN = "nqn.2023-02.io.simplyblock:c30a691a-1d2e-4f3a-9b8c-5d6e7f809a1b:lvol:shared"

// listingResolver answers ListWithSelector with a fixed sysfs snapshot.
type listingResolver struct {
	nullDeviceResolver
	devs []atlasnvme.Device
}

func (r listingResolver) ListWithSelector(_ context.Context, _ atlasnvme.DeviceSelector) ([]atlasnvme.Device, error) {
	return r.devs, nil
}

// subsystemEntry is one sysfs subsystem directory for sharedNQN whose
// controllers are in the given states.
func subsystemEntry(id string, states ...string) atlasnvme.Subsystem {
	s := atlasnvme.Subsystem{ID: atlasnvme.SubsystemID(id), NQN: sharedNQN}
	for i, state := range states {
		s.Controllers = append(s.Controllers, atlasnvme.Controller{
			ID:    atlasnvme.ControllerID(id + "-ctrl" + string(rune('0'+i))),
			NQN:   sharedNQN,
			State: state,
		})
	}
	return s
}

func onEntry(s atlasnvme.Subsystem, uuids ...string) []atlasnvme.Device {
	devs := make([]atlasnvme.Device, 0, len(uuids))
	for i, uuid := range uuids {
		devs = append(devs, atlasnvme.Device{
			Subsystem: s,
			Namespace: atlasnvme.Namespace{ID: atlasnvme.NamespaceID(i + 1), UUID: uuid},
		})
	}
	return devs
}

func guardianWith(devs []atlasnvme.Device) *Guardian {
	g := newTestGuardian(nil)
	g.devices = listingResolver{devs: devs}
	return g
}

func assertIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("volumes on the subsystem = %v, want %v", got, want)
	}
}

// Issue #427: a stale entry still awaiting reaping can hold a volume the
// reconnected subsystem no longer exports. Counting it would make a private
// volume look shared and force a coordinated restart.
func TestLvolsOnSubsystemIgnoresAStaleEntryBesideALiveOne(t *testing.T) {
	stale := subsystemEntry("nvme-subsys0", "connecting")
	fresh := subsystemEntry("nvme-subsys3", "live")
	devs := append(onEntry(stale, "lvol-a", "lvol-gone"), onEntry(fresh, "lvol-a")...)

	assertIDs(t, guardianWith(devs).lvolsOnSubsystem(context.Background(), sharedNQN), "lvol-a")
}

// During a path outage, which is when the guardian runs, no controller is
// live. Dropping every entry would route a shared volume to an individual
// restart, so nothing is filtered.
func TestLvolsOnSubsystemKeepsEveryEntryWhenNoneIsLive(t *testing.T) {
	a := subsystemEntry("nvme-subsys0", "connecting")
	b := subsystemEntry("nvme-subsys3", "resetting", "connecting")
	devs := append(onEntry(a, "lvol-a", "lvol-b"), onEntry(b, "lvol-c")...)

	assertIDs(t, guardianWith(devs).lvolsOnSubsystem(context.Background(), sharedNQN),
		"lvol-a", "lvol-b", "lvol-c")
}

// An entry that reports no namespace UUIDs says nothing about the volumes on
// the subsystem, so the registrations cached at publish time answer instead
// of an empty list, which would read as a private volume.
func TestLvolsOnSubsystemFallsBackToRegistrationsWhenSysfsNamesNoVolume(t *testing.T) {
	devs := onEntry(subsystemEntry("nvme-subsys0", "live"), "", "")
	g := guardianWith(devs)
	g.lvols["lvol-a"] = &LvolState{SubsystemNQN: sharedNQN}
	g.lvols["lvol-b"] = &LvolState{SubsystemNQN: sharedNQN}
	g.lvols["lvol-other"] = &LvolState{SubsystemNQN: "nqn.other"}

	assertIDs(t, g.lvolsOnSubsystem(context.Background(), sharedNQN), "lvol-a", "lvol-b")
}

func TestLvolsOnSubsystemListsTheVolumesOfAHealthySubsystemOnce(t *testing.T) {
	entry := subsystemEntry("nvme-subsys0", "live", "live")
	// Without a multipath head each controller exposes its own device for a
	// namespace, so a volume appears once per path.
	devs := append(onEntry(entry, "lvol-a", "lvol-b"), onEntry(entry, "lvol-a", "lvol-b")...)

	assertIDs(t, guardianWith(devs).lvolsOnSubsystem(context.Background(), sharedNQN), "lvol-a", "lvol-b")
}
