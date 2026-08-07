package nvmeof

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
)

// connection builds the control plane's answer for the given addresses, in
// priority order.
func connection(addrs ...string) lvol.Connection {
	conn := lvol.Connection{NQN: testNQN}
	for _, a := range addrs {
		conn.Endpoints = append(conn.Endpoints, lvol.Endpoint{Transport: "tcp", Address: a, Port: 4420})
	}
	return conn
}

func addresses(ctrls []nvme.Controller) []string {
	out := make([]string, len(ctrls))
	for i, c := range ctrls {
		out[i] = c.Address.TrAddr
	}
	return out
}

func TestReconcilePaths_AttachesWhatIsMissing(t *testing.T) {
	f := &fabric{}
	// One path already up; the reconcile must add the other two and touch the
	// first one not at all.
	f.ctrls = append(f.ctrls, ctrl("nvme0", "10.0.0.1", "live"))

	state, err := ReconcilePaths(context.Background(), f.connector(), fakeSubs{byNQN: f.byNQN},
		connection("10.0.0.1", "10.0.0.2", "10.0.0.3"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.order, []string{"10.0.0.2", "10.0.0.3"}) {
		t.Errorf("connect writes = %v, want only the two missing paths", f.order)
	}
	if state.Expected != 3 || state.Live != 3 || !state.Complete() {
		t.Errorf("state = %+v, want 3/3 complete", state)
	}
	if !state.Results[0].AlreadyPresent {
		t.Error("the path that was already up is not marked AlreadyPresent")
	}
}

func TestReconcilePaths_IdempotentWhenComplete(t *testing.T) {
	f := &fabric{}
	conn := connection("10.0.0.1", "10.0.0.2")
	c := f.connector()

	if _, err := ReconcilePaths(context.Background(), c, fakeSubs{byNQN: f.byNQN}, conn); err != nil {
		t.Fatal(err)
	}
	writes := len(f.order)

	state, err := ReconcilePaths(context.Background(), c, fakeSubs{byNQN: f.byNQN}, conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.order) != writes {
		t.Errorf("second reconcile wrote the fabrics device again: %v", f.order)
	}
	if !state.Complete() || state.Degraded() || state.Down() {
		t.Errorf("state = %+v, want complete", state)
	}
}

func TestReconcilePaths_DegradedWhenAPathIsUnreachable(t *testing.T) {
	f := &fabric{fail: map[string]error{"10.0.0.1": errors.New("connection refused")}}

	state, err := ReconcilePaths(context.Background(), f.connector(), fakeSubs{byNQN: f.byNQN},
		connection("10.0.0.1", "10.0.0.2"))
	if err != nil {
		t.Fatalf("err = %v, want nil: one path is up, so the volume is usable", err)
	}
	if !state.Degraded() || state.Live != 1 || state.Expected != 2 {
		t.Errorf("state = %+v, want degraded 1/2", state)
	}
	if state.Results[0].Err == nil {
		t.Error("the unreachable path carries no reason")
	}
}

func TestReconcilePaths_DownWhenNothingComesUp(t *testing.T) {
	f := &fabric{fail: map[string]error{
		"10.0.0.1": errors.New("connection refused"),
		"10.0.0.2": errors.New("connection refused"),
	}}

	state, err := ReconcilePaths(context.Background(), f.connector(), fakeSubs{byNQN: f.byNQN},
		connection("10.0.0.1", "10.0.0.2"))
	if err == nil {
		t.Error("err = nil, want an error when no path could be established")
	}
	if !state.Down() || len(state.Results) != 2 {
		t.Errorf("state = %+v, want down with both attempts recorded", state)
	}
}

// After a migration the old primary's controller is still attached but no longer
// published. It is reported, not removed: dropping a live data path is the
// caller's call.
func TestReconcilePaths_ReportsStalePathsWithoutRemovingThem(t *testing.T) {
	f := &fabric{}
	f.ctrls = append(f.ctrls,
		ctrl("nvme0", "10.0.0.9", "live"), // the node we migrated away from
		ctrl("nvme1", "10.0.0.1", "live"),
	)

	state, err := ReconcilePaths(context.Background(), f.connector(), fakeSubs{byNQN: f.byNQN},
		connection("10.0.0.1", "10.0.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	if got := addresses(state.Stale); !slices.Equal(got, []string{"10.0.0.9"}) {
		t.Errorf("stale = %v, want [10.0.0.9]", got)
	}
	if !state.Complete() {
		t.Errorf("state = %+v, want the two published paths complete", state)
	}
	if slices.Contains(f.order, "10.0.0.9") {
		t.Error("the stale path was reconnected")
	}
}

func TestReconcilePaths_SkipsStaleCheckWithoutAResolver(t *testing.T) {
	f := &fabric{}
	f.ctrls = append(f.ctrls, ctrl("nvme0", "10.0.0.9", "live"))

	state, err := ReconcilePaths(context.Background(), f.connector(), nil, connection("10.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Stale != nil {
		t.Errorf("stale = %v, want none reported without a subsystem resolver", addresses(state.Stale))
	}
}

func TestReconcilePaths_RejectsAnEmptyAnswer(t *testing.T) {
	f := &fabric{}

	t.Run("no endpoints", func(t *testing.T) {
		_, err := ReconcilePaths(context.Background(), f.connector(), nil, lvol.Connection{NQN: testNQN})
		if !errors.Is(err, errs.ErrNotConnected) {
			t.Errorf("err = %v, want errs.ErrNotConnected", err)
		}
	})

	t.Run("no nqn", func(t *testing.T) {
		conn := connection("10.0.0.1")
		conn.NQN = ""
		if _, err := ReconcilePaths(context.Background(), f.connector(), nil, conn); !errors.Is(err, errs.ErrUnsupported) {
			t.Errorf("err = %v, want errs.ErrUnsupported", err)
		}
	})
}
