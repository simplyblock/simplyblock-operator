// The host NQN of each metadata server pod (design-pnfs-mds-standby.md §5.2).

package mdsstate

import (
	"testing"

	"github.com/simplyblock/atlas/nqn"
)

const testUID = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"

// The vector pins the derivation: the guest, the control plane's allowed hosts,
// and the target's registrations all hold these strings, so a change of the
// derivation locks every standby out of its volumes.
func TestHostNQNHasAStableVector(t *testing.T) {
	for ordinal, want := range map[int32]string{
		0: "nqn.2014-08.io.simplyblock:uuid:4b50e3a1-3fc2-5890-9d9a-be600c044e13",
		1: "nqn.2014-08.io.simplyblock:uuid:bfede67d-37d4-568d-9f1f-b097dc120b01",
		4: "nqn.2014-08.io.simplyblock:uuid:b0a9c0ce-97d5-5e34-8528-1977eb23a782",
	} {
		got, err := HostNQN(testUID, ordinal)
		if err != nil || got != want {
			t.Errorf("HostNQN(%s, %d) = %q, %v, want %q", testUID, ordinal, got, err, want)
		}
	}
}

// A recreated StatefulSet is a new host, and each ordinal is its own.
func TestHostNQNSeparatesOrdinalsAndStatefulSets(t *testing.T) {
	a0, _ := HostNQN(testUID, 0)
	a1, _ := HostNQN(testUID, 1)
	b0, _ := HostNQN("0a1b2c3d-4a5b-4c6d-8e7f-6f1c2d3e4e5f", 0)
	if a0 == a1 || a0 == b0 {
		t.Errorf("host NQNs collide: %s %s %s", a0, a1, b0)
	}
}

// The target keys a registration by the host identifier derived from the
// NQN's UUID, so the NQN has to carry one.
func TestHostNQNCarriesAUUID(t *testing.T) {
	got, _ := HostNQN(testUID, 2)
	if _, ok := nqn.HostUUID(got); !ok {
		t.Errorf("%q carries no UUID", got)
	}
}

func TestHostNQNRefusesWhatIsNotAUID(t *testing.T) {
	if got, err := HostNQN("not-a-uid", 0); err == nil {
		t.Errorf("HostNQN of a malformed UID = %q, want an error", got)
	}
	if got, err := HostNQN(testUID, -1); err == nil {
		t.Errorf("HostNQN of ordinal -1 = %q, want an error", got)
	}
}
