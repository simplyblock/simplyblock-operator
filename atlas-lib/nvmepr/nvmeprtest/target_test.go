// The fake target against the behavior SPDK showed on 2026-10-21, which is what
// the fence's tests rely on it to reproduce: a plain Register is refused for a
// host that is registered, a Replace with ignore-existing-key registers any
// host, a Preempt of an unknown key succeeds, and a host that is not registered
// can neither read nor write once a registrants-only reservation exists.
package nvmeprtest

import (
	"context"
	"errors"
	"testing"

	"github.com/simplyblock/atlas/nvmepr"
)

var (
	hostA = [16]byte{0xa}
	hostB = [16]byte{0xb}
	ctx   = context.Background()
)

const typ = nvmepr.TypeExclusiveAccessRegOnly

func TestPlainRegisterIsRefusedForARegisteredHost(t *testing.T) {
	ns := New()
	a := ns.Host(hostA)
	if err := a.Register(ctx, 0, 0x10, false); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := a.Register(ctx, 0, 0x11, false); !errors.Is(err, nvmepr.ErrReservationConflict) {
		t.Fatalf("second Register under another key: err = %v, want ErrReservationConflict", err)
	}
	if err := a.Register(ctx, 0, 0x10, false); err != nil {
		t.Errorf("Register under the same key: %v", err)
	}
}

func TestReplaceWithIgnoreKeyRegistersAndReplaces(t *testing.T) {
	ns := New()
	a := ns.Host(hostA)
	if err := a.Register(ctx, 1, 0x20, true); err != nil {
		t.Fatalf("Replace on an unregistered host: %v", err)
	}
	if err := a.Register(ctx, 1, 0x21, true); err != nil {
		t.Fatalf("Replace on a registered host: %v", err)
	}
	r, _ := a.Report(ctx)
	if len(r.Registrants) != 1 || r.Registrants[0].Key != 0x21 || r.Registrants[0].HostID != hostA {
		t.Fatalf("report = %+v", r)
	}
	if err := a.Register(ctx, 0x99, 0x22, false); !errors.Is(err, nvmepr.ErrReservationConflict) {
		t.Errorf("Replace with the wrong current key: err = %v, want ErrReservationConflict", err)
	}
}

func TestAcquireNeedsARegistrationAndTheReservationIsExclusive(t *testing.T) {
	ns := New()
	a, b := ns.Host(hostA), ns.Host(hostB)
	if err := a.Acquire(ctx, 0x10, typ); !errors.Is(err, nvmepr.ErrReservationConflict) {
		t.Fatalf("Acquire without a registration: err = %v", err)
	}
	_ = a.Register(ctx, 1, 0x10, true)
	_ = b.Register(ctx, 1, 0x11, true)
	if err := a.Acquire(ctx, 0x10, typ); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := b.Acquire(ctx, 0x11, typ); !errors.Is(err, nvmepr.ErrReservationConflict) {
		t.Errorf("a second host's Acquire: err = %v, want ErrReservationConflict", err)
	}
	r, _ := a.Report(ctx)
	if h, ok := r.Holder(); !ok || h.Key != 0x10 || r.Type != typ {
		t.Errorf("holder = %+v, %v, type %d", h, ok, r.Type)
	}
}

func TestPreemptMovesTheReservationAndUnregistersTheVictim(t *testing.T) {
	ns := New()
	a, b := ns.Host(hostA), ns.Host(hostB)
	_ = a.Register(ctx, 1, 0x10, true)
	_ = a.Acquire(ctx, 0x10, typ)
	_ = b.Register(ctx, 1, 0x11, true)
	if err := b.Preempt(ctx, 0x11, 0x10, typ, true); err != nil {
		t.Fatalf("Preempt: %v", err)
	}
	r, _ := b.Report(ctx)
	if len(r.Registrants) != 1 || r.Registrants[0].HostID != hostB || !r.Registrants[0].HoldsReservation {
		t.Fatalf("after the preempt: %+v", r)
	}
	if ns.CanWrite(hostA) || ns.CanRead(hostA) {
		t.Error("the preempted host can still reach the namespace")
	}
	if !ns.CanWrite(hostB) {
		t.Error("the preempting host cannot write")
	}
}

func TestPreemptOfAnUnknownKeySucceedsAndChangesNothing(t *testing.T) {
	ns := New()
	a := ns.Host(hostA)
	_ = a.Register(ctx, 1, 0x10, true)
	_ = a.Acquire(ctx, 0x10, typ)
	before, _ := a.Report(ctx)
	if err := a.Preempt(ctx, 0x10, 0x77, typ, false); err != nil {
		t.Fatalf("Preempt of an unknown key: %v", err)
	}
	after, _ := a.Report(ctx)
	if len(after.Registrants) != 1 || after.Generation < before.Generation {
		t.Errorf("report changed: %+v -> %+v", before, after)
	}
}

func TestPreemptWithoutTheCallersRegistrationIsAConflict(t *testing.T) {
	ns := New()
	b := ns.Host(hostB)
	if err := b.Preempt(ctx, 0x11, 0x10, typ, false); !errors.Is(err, nvmepr.ErrReservationConflict) {
		t.Errorf("err = %v, want ErrReservationConflict", err)
	}
}

func TestATargetWithoutPreemptAndAbortRefusesIt(t *testing.T) {
	ns := New(WithoutPreemptAndAbort())
	a, b := ns.Host(hostA), ns.Host(hostB)
	_ = a.Register(ctx, 1, 0x10, true)
	_ = a.Acquire(ctx, 0x10, typ)
	_ = b.Register(ctx, 1, 0x11, true)
	if err := b.Preempt(ctx, 0x11, 0x10, typ, true); !errors.Is(err, nvmepr.ErrInvalidField) {
		t.Fatalf("Preempt and Abort: err = %v, want ErrInvalidField", err)
	}
	if err := b.Preempt(ctx, 0x11, 0x10, typ, false); err != nil {
		t.Errorf("plain Preempt: %v", err)
	}
}

func TestTheRegistrantLimitRefusesTheNextHost(t *testing.T) {
	ns := New(WithMaxRegistrants(2))
	for i, h := range [][16]byte{{1}, {2}} {
		if err := ns.Host(h).Register(ctx, 1, uint64(0x30+i), true); err != nil {
			t.Fatalf("host %d: %v", i, err)
		}
	}
	if err := ns.Host([16]byte{3}).Register(ctx, 1, 0x32, true); err == nil {
		t.Error("a third registrant was accepted over the limit of 2")
	}
	// A host that is already registered still replaces its key on a full namespace.
	if err := ns.Host([16]byte{1}).Register(ctx, 1, 0x40, true); err != nil {
		t.Errorf("Replace on a full namespace: %v", err)
	}
}

func TestAnUnreservedNamespaceTakesIOFromAnyHost(t *testing.T) {
	ns := New()
	if !ns.CanWrite(hostA) || !ns.CanRead(hostA) {
		t.Error("a namespace with no reservation refused a host")
	}
	a := ns.Host(hostA)
	_ = a.Register(ctx, 1, 0x10, true)
	_ = a.Acquire(ctx, 0x10, typ)
	if ns.CanRead(hostB) || ns.CanWrite(hostB) {
		t.Error("an unregistered host reached a registrants-only reservation")
	}
}
