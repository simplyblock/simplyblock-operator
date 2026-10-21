// Tests for the tracker: it hands the guest a new connection when the control
// plane publishes a volume somewhere else, and does nothing while it does not.

package relay

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/simplyblock/atlas/lvol"
	export "github.com/simplyblock/atlas/nfsexport"
)

// movingResolver answers with conn, which a test changes between calls.
type movingResolver struct {
	conn  lvol.Connection
	err   error
	asked int
}

func (m *movingResolver) resolve(context.Context, export.Spec, string) (lvol.Connection, string, error) {
	m.asked++
	return m.conn, "", m.err
}

func moved(addrs ...string) lvol.Connection {
	c := lvol.Connection{NQN: resolved.NQN, NSID: resolved.NSID, UUID: resolved.UUID}
	for _, a := range addrs {
		c.Endpoints = append(c.Endpoints, lvol.Endpoint{Transport: "tcp", Address: a, Port: 4420})
	}
	return c
}

func newTracker(g *guest, m *movingResolver) *Tracker {
	return &Tracker{Relay: Relay{Guest: g, Resolve: m.resolve}, Log: func(string, ...any) {}}
}

func TestTheTrackerLeavesAnUnmovedVolumeAlone(t *testing.T) {
	g, m := &guest{}, &movingResolver{conn: moved("10.10.1.11", "10.10.1.12")}
	tr := newTracker(g, m)
	ctx := context.Background()
	if err := tr.Create(ctx, spec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The same endpoints in another order are the same paths.
	m.conn = moved("10.10.1.12", "10.10.1.11")
	tr.Refresh(ctx)
	if len(g.created) != 1 {
		t.Errorf("the guest got %d Creates, want only the operator's", len(g.created))
	}
	if m.asked != 2 {
		t.Errorf("the control plane was asked %d times, want once per Create and once per refresh", m.asked)
	}
}

func TestTheTrackerReassemblesAVolumePublishedElsewhere(t *testing.T) {
	g, m := &guest{}, &movingResolver{conn: moved("10.10.1.11", "10.10.1.12")}
	tr := newTracker(g, m)
	ctx := context.Background()
	_ = tr.Create(ctx, spec())

	m.conn = moved("10.10.1.13", "10.10.1.12")
	tr.Refresh(ctx)
	if len(g.created) != 2 {
		t.Fatalf("the guest got %d Creates, want a second one with the new endpoints", len(g.created))
	}
	got := g.created[1].Connection
	if got == nil || len(got.Endpoints) != 2 || got.Endpoints[0].Address != "10.10.1.13" {
		t.Errorf("the second Create carried %+v, want the endpoints the control plane publishes now", got)
	}

	tr.Refresh(ctx)
	if len(g.created) != 2 {
		t.Errorf("the guest got %d Creates, want none once it has the new endpoints", len(g.created))
	}
}

func TestTheTrackerForgetsADeletedExport(t *testing.T) {
	g, m := &guest{}, &movingResolver{conn: moved("10.10.1.11")}
	tr := newTracker(g, m)
	ctx := context.Background()
	_ = tr.Create(ctx, spec())
	if err := tr.Delete(ctx, spec()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	m.conn = moved("10.10.1.13")
	asked := m.asked
	tr.Refresh(ctx)
	if len(g.created) != 1 || m.asked != asked {
		t.Errorf("a deleted export was refreshed: %d Creates, %d more questions", len(g.created), m.asked-asked)
	}
}

func TestTheTrackerKeepsTheGuestAsItIsWhenTheControlPlaneDoesNotAnswer(t *testing.T) {
	g, m := &guest{}, &movingResolver{conn: moved("10.10.1.11")}
	tr := newTracker(g, m)
	ctx := context.Background()
	_ = tr.Create(ctx, spec())
	m.err = errors.New("control plane unreachable")
	m.conn = lvol.Connection{}
	tr.Refresh(ctx)
	if len(g.created) != 1 {
		t.Errorf("the guest got %d Creates on an unanswered question", len(g.created))
	}
}

func TestTheTrackerDoesNotTrackACreateThatFailed(t *testing.T) {
	g, m := &guest{}, &movingResolver{err: errors.New("control plane unreachable")}
	tr := newTracker(g, m)
	ctx := context.Background()
	if err := tr.Create(ctx, spec()); err == nil {
		t.Fatal("Create succeeded without a connection")
	}
	m.err = nil
	m.conn = moved("10.10.1.11")
	tr.Refresh(ctx)
	if len(g.created) != 0 {
		t.Errorf("the guest got %d Creates for an export that never assembled", len(g.created))
	}
}

// orderedGuest records every Create and Delete in the order the guest saw them,
// from any goroutine.
type orderedGuest struct {
	mu  sync.Mutex
	ops []string
	// last is the spec of the last Create.
	last export.Spec
}

func (g *orderedGuest) Create(_ context.Context, s export.Spec) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ops = append(g.ops, "create")
	g.last = s
	return nil
}

func (g *orderedGuest) Delete(context.Context, export.Spec) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ops = append(g.ops, "delete")
	return nil
}

func (g *orderedGuest) Check(context.Context, export.Spec) error { return nil }

func (g *orderedGuest) snapshot() ([]string, export.Spec) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.ops), g.last
}

// hookedResolver answers with conn and runs hook once, on the call numbered
// at (counting from one).
type hookedResolver struct {
	mu    sync.Mutex
	conn  lvol.Connection
	calls int
	at    int
	hook  func()
}

func (h *hookedResolver) resolve(context.Context, export.Spec, string) (lvol.Connection, string, error) {
	h.mu.Lock()
	h.calls++
	conn, fire := h.conn, h.calls == h.at
	h.mu.Unlock()
	if fire {
		h.hook()
	}
	return conn, "", nil
}

func (h *hookedResolver) set(conn lvol.Connection, at int, hook func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conn, h.calls, h.at, h.hook = conn, 0, at, hook
}

// A Delete that arrives while a refresh is reassembling the export is not
// undone by it: the guest's last word on the export is the Delete, and the
// tracker no longer refreshes it.
func TestTheTrackerDoesNotRecreateAnExportDeletedDuringAReassembly(t *testing.T) {
	g, h := &orderedGuest{}, &hookedResolver{}
	tr := &Tracker{Relay: Relay{Guest: g, Resolve: h.resolve}, Log: func(string, ...any) {}}
	ctx := context.Background()
	h.set(moved("10.10.1.11"), 0, nil)
	if err := tr.Create(ctx, spec()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The refresh asks once to compare and once more inside the reassembly's
	// Create. The Delete starts there, after any membership check, and is
	// given a moment to finish before the reassembly reaches the guest.
	deleted := make(chan error, 1)
	h.set(moved("10.10.1.13"), 2, func() {
		go func() { deleted <- tr.Delete(ctx, spec()) }()
		select {
		case err := <-deleted:
			deleted <- err
		case <-time.After(200 * time.Millisecond):
		}
	})
	tr.Refresh(ctx)
	if err := <-deleted; err != nil {
		t.Fatalf("Delete: %v", err)
	}

	ops, _ := g.snapshot()
	if ops[len(ops)-1] != "delete" {
		t.Errorf("the guest saw %v, want the Delete last", ops)
	}
	if _, ok := tr.current(spec().Path); ok {
		t.Error("the deleted export is still tracked")
	}
}

// A Create that lands while a refresh is asking the control plane is not
// overwritten by the refresh's older copy of the spec.
func TestTheTrackerDoesNotOverwriteANewerCreate(t *testing.T) {
	g, h := &orderedGuest{}, &hookedResolver{}
	tr := &Tracker{Relay: Relay{Guest: g, Resolve: h.resolve}, Log: func(string, ...any) {}}
	ctx := context.Background()
	h.set(moved("10.10.1.11"), 0, nil)
	if err := tr.Create(ctx, spec()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	newer := spec()
	newer.Clients = []string{"10.0.0.2"}
	h.set(moved("10.10.1.13"), 1, func() {
		if err := tr.Create(ctx, newer); err != nil {
			t.Errorf("the newer Create: %v", err)
		}
	})
	tr.Refresh(ctx)

	if _, last := g.snapshot(); !slices.Equal(last.Clients, newer.Clients) {
		t.Errorf("the guest last got clients %v, want the newer Create's %v", last.Clients, newer.Clients)
	}
}
