package volumemigration

import (
	"errors"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/nvmeof"
)

// The connect flags themselves are atlas's to render; what stays this package's
// responsibility is the mapping onto them — in particular that an unset tuning
// value reaches atlas as unset. A zero must mean "leave the kernel default", not
// "ask for 0": nvme-cli rejects some zero values outright, and 0 is a meaningful
// request for the two timeouts (fail I/O immediately), so the difference is not
// cosmetic.
func TestConnectionTarget(t *testing.T) {
	base := Connection{NQN: "nqn.test:vol", IP: "10.0.0.1", Port: 4420, Transport: "tcp"}

	t.Run("endpoint identity is carried over", func(t *testing.T) {
		got := base.target()
		want := nvmeof.Target{
			NQN:       "nqn.test:vol",
			Transport: nvmeof.TransportTCP,
			Address:   "10.0.0.1",
			Port:      4420,
		}
		if got != want {
			t.Errorf("target = %+v, want %+v", got, want)
		}
	})

	t.Run("every tuning value is passed when set", func(t *testing.T) {
		c := base
		c.NrIoQueues, c.ReconnectDelay, c.CtrlLossTmo, c.FastIOFailTmo, c.KeepAliveTmo = 3, 2, 3600, 8, 4
		got := c.target()
		if got.NrIOQueues != 3 || got.ReconnectDelaySec != 2 || got.KeepAliveTMOSec != 4 {
			t.Errorf("target = %+v, want the plain tunables carried over", got)
		}
		if got.CtrlLossTMOSec == nil || *got.CtrlLossTMOSec != 3600 {
			t.Errorf("CtrlLossTMOSec = %v, want 3600", got.CtrlLossTMOSec)
		}
		if got.FastIOFailTMOSec == nil || *got.FastIOFailTMOSec != 8 {
			t.Errorf("FastIOFailTMOSec = %v, want 8", got.FastIOFailTMOSec)
		}
	})

	t.Run("zero values are left unset", func(t *testing.T) {
		got := base.target()
		if got.NrIOQueues != 0 || got.ReconnectDelaySec != 0 || got.KeepAliveTMOSec != 0 {
			t.Errorf("target = %+v, want no tunables for zero values", got)
		}
		if got.CtrlLossTMOSec != nil {
			t.Errorf("CtrlLossTMOSec = %v, want nil for a zero value", *got.CtrlLossTMOSec)
		}
		if got.FastIOFailTMOSec != nil {
			t.Errorf("FastIOFailTMOSec = %v, want nil for a zero value", *got.FastIOFailTMOSec)
		}
	})
}

// A migration moves one subsystem, so the grouping normally yields exactly one
// connect. It still has to be a grouping rather than an assumption: atlas attaches
// one subsystem per call and rejects a target list that spans several, so a stray
// second NQN would otherwise turn into a hard failure instead of a second connect.
func TestSubsystemOrder(t *testing.T) {
	conns := []Connection{
		{NQN: "nqn.a", IP: "10.0.0.1", Port: 4420},
		{NQN: "nqn.b", IP: "10.0.0.2", Port: 4420},
		{NQN: "nqn.a", IP: "10.0.0.3", Port: 4420},
	}
	got := subsystemOrder(conns)
	want := []string{"nqn.a", "nqn.b"}
	if len(got) != len(want) {
		t.Fatalf("subsystemOrder = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("subsystemOrder = %v, want %v (first-seen order)", got, want)
		}
	}
}

func TestRequireAllLive(t *testing.T) {
	src := nvmeof.Target{NQN: "nqn.x", Address: "10.0.0.1", Port: 4420}
	tgt := nvmeof.Target{NQN: "nqn.x", Address: "10.0.0.2", Port: 4428}

	if err := requireAllLive("nqn.x", []nvmeof.PathResult{
		{Target: src, Live: true, AlreadyPresent: true},
		{Target: tgt, Live: true},
	}); err != nil {
		t.Fatalf("all live: got %v, want nil", err)
	}

	// The 2026-10-23 preconnect: the source path was already there, the target
	// attach failed. One live path must not read as success.
	err := requireAllLive("nqn.x", []nvmeof.PathResult{
		{Target: src, Live: true, AlreadyPresent: true},
		{Target: tgt, Err: errors.New("sudo: a password is required")},
	})
	if err == nil {
		t.Fatal("source live, target down: got nil, want an error")
	}
	if !strings.Contains(err.Error(), "1 of 2") || !strings.Contains(err.Error(), "sudo") {
		t.Errorf("error %q should count the down path and carry its reason", err)
	}

	if err := requireAllLive("nqn.x", []nvmeof.PathResult{{Target: tgt}}); err == nil ||
		!strings.Contains(err.Error(), "10.0.0.2:4428") {
		t.Errorf("down path without a reason: got %v, want one naming the endpoint", err)
	}
}
