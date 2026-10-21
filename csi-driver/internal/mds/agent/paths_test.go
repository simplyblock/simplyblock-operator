// Tests for the guest's path keeper: which exports it keeps, what it
// reconciles them with, that it never runs beside a call for the same export,
// and what it says about the paths.

package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simplyblock/atlas/lvol"
	export "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nvmeof"
)

// fakeAssembler answers Create and Delete with err, and can hold a call open.
type fakeAssembler struct {
	err   error
	hold  chan struct{} // when set, Create waits on it
	entry chan struct{} // when set, Create signals it on entry
}

func (f *fakeAssembler) Create(context.Context, export.Spec) error {
	if f.entry != nil {
		f.entry <- struct{}{}
	}
	if f.hold != nil {
		<-f.hold
	}
	return f.err
}
func (f *fakeAssembler) Delete(context.Context, export.Spec) error { return f.err }
func (f *fakeAssembler) Check(context.Context, export.Spec) error  { return nil }

// recordingLog keeps every line.
type recordingLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLog) Infof(format string, args ...any) { l.add("I " + fmt.Sprintf(format, args...)) }
func (l *recordingLog) Warningf(format string, args ...any) {
	l.add("W " + fmt.Sprintf(format, args...))
}
func (l *recordingLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, s)
}
func (l *recordingLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.lines
	l.lines = nil
	return out
}

// reconciles records the connections reconciled, and answers with state.
type reconciles struct {
	mu    sync.Mutex
	seen  []string
	state func(conn lvol.Connection) nvmeof.PathState
}

func (r *reconciles) reconcile(_ context.Context, conn lvol.Connection, hostNQN string) (nvmeof.PathState, error) {
	r.mu.Lock()
	r.seen = append(r.seen, conn.NQN+" as "+hostNQN)
	r.mu.Unlock()
	if r.state == nil {
		return nvmeof.PathState{NQN: conn.NQN}, nil
	}
	st := r.state(conn)
	if st.Live == 0 {
		return st, errors.New("no path came up")
	}
	return st, nil
}

func (r *reconciles) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

func spec(path, nqn string) export.Spec {
	return export.Spec{
		Path:       path,
		HostNQN:    "nqn.2014-08.io.simplyblock:uuid:guest",
		Connection: &lvol.Connection{NQN: nqn},
	}
}

func newKeeper(r *reconciles, log *recordingLog) *PathKeeper {
	return &PathKeeper{Reconcile: r.reconcile, Log: log}
}

func TestTheKeeperReconcilesWhatWasCreatedUntilItIsDeleted(t *testing.T) {
	r := &reconciles{}
	k := newKeeper(r, &recordingLog{})
	a := k.Wrap(&fakeAssembler{})
	ctx := context.Background()

	if err := a.Create(ctx, spec("/exports/a", "nqn:lvol:a")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := a.Create(ctx, spec("/exports/b", "nqn:lvol:b")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	k.Tick(ctx)
	if err := a.Delete(ctx, spec("/exports/a", "nqn:lvol:a")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	k.Tick(ctx)

	want := []string{
		"nqn:lvol:a as nqn.2014-08.io.simplyblock:uuid:guest",
		"nqn:lvol:b as nqn.2014-08.io.simplyblock:uuid:guest",
		"nqn:lvol:b as nqn.2014-08.io.simplyblock:uuid:guest",
	}
	// A tick reconciles exports beside each other, so in no particular order.
	got := r.calls()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("reconciled %v, want %v", got, want)
	}
}

func TestTheKeeperKeepsTheLatestConnection(t *testing.T) {
	r := &reconciles{}
	k := newKeeper(r, &recordingLog{})
	a := k.Wrap(&fakeAssembler{})
	ctx := context.Background()

	_ = a.Create(ctx, spec("/exports/a", "nqn:lvol:a-before-migration"))
	_ = a.Create(ctx, spec("/exports/a", "nqn:lvol:a-after-migration"))
	k.Tick(ctx)

	if got := r.calls(); len(got) != 1 || !strings.HasPrefix(got[0], "nqn:lvol:a-after-migration ") {
		t.Errorf("reconciled %v, want only the connection the last Create carried", got)
	}
}

func TestTheKeeperIgnoresCallsThatFailed(t *testing.T) {
	r := &reconciles{}
	k := newKeeper(r, &recordingLog{})
	ctx := context.Background()

	failing := k.Wrap(&fakeAssembler{err: errors.New("mkfs failed")})
	if err := failing.Create(ctx, spec("/exports/a", "nqn:lvol:a")); err == nil {
		t.Fatal("the wrapper swallowed Create's error")
	}
	k.Tick(ctx)
	if got := r.calls(); len(got) != 0 {
		t.Errorf("reconciled %v for an export whose Create failed", got)
	}

	_ = k.Wrap(&fakeAssembler{}).Create(ctx, spec("/exports/b", "nqn:lvol:b"))
	if err := failing.Delete(ctx, spec("/exports/b", "nqn:lvol:b")); err == nil {
		t.Fatal("the wrapper swallowed Delete's error")
	}
	k.Tick(ctx)
	if got := r.calls(); len(got) != 1 {
		t.Errorf("reconciled %v, want the export whose Delete failed still kept", got)
	}
}

func TestTheKeeperSkipsAnExportWithoutAConnection(t *testing.T) {
	r := &reconciles{}
	k := newKeeper(r, &recordingLog{})
	s := spec("/exports/a", "")
	s.Connection = nil
	_ = k.Wrap(&fakeAssembler{}).Create(context.Background(), s)
	k.Tick(context.Background())
	if got := r.calls(); len(got) != 0 {
		t.Errorf("reconciled %v with no connection to reconcile against", got)
	}
}

// A reconcile beside a Create of the same export would connect the same path
// twice at once, and one of the two would fail.
func TestTheKeeperWaitsForACallOnTheSameExport(t *testing.T) {
	r := &reconciles{}
	k := newKeeper(r, &recordingLog{})
	ctx := context.Background()
	a := k.Wrap(&fakeAssembler{})
	_ = a.Create(ctx, spec("/exports/a", "nqn:lvol:a"))

	slow := &fakeAssembler{hold: make(chan struct{}), entry: make(chan struct{})}
	created := make(chan struct{})
	go func() {
		_ = k.Wrap(slow).Create(ctx, spec("/exports/a", "nqn:lvol:a"))
		close(created)
	}()
	<-slow.entry

	ticked := make(chan struct{})
	go func() {
		k.Tick(ctx)
		close(ticked)
	}()
	select {
	case <-ticked:
		t.Fatal("the reconcile ran while a Create of the same export was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(slow.hold)
	<-created
	<-ticked
	if got := r.calls(); len(got) != 1 {
		t.Errorf("reconciled %v, want once, after the Create", got)
	}
}

func target(addr string) nvmeof.Target {
	return nvmeof.Target{NQN: "nqn:lvol:a", Transport: "tcp", Address: addr, Port: 4430}
}

func TestTheKeeperSaysWhenAPathDropsAndWhenItIsBack(t *testing.T) {
	down, connected := true, false
	r := &reconciles{state: func(lvol.Connection) nvmeof.PathState {
		st := nvmeof.PathState{NQN: "nqn:lvol:a", Expected: 2}
		st.Results = []nvmeof.PathResult{{Target: target("10.10.10.142"), Live: true, AlreadyPresent: true}}
		if down {
			refused := nvmeof.PathResult{Target: target("10.10.10.143"), Err: errors.New("connection refused")}
			st.Results = append(st.Results, refused)
		} else {
			// Connected by this reconcile the first time, and found there after.
			st.Results = append(st.Results,
				nvmeof.PathResult{Target: target("10.10.10.143"), Live: true, AlreadyPresent: connected})
			connected = true
		}
		for _, res := range st.Results {
			if res.Live {
				st.Live++
			}
		}
		return st
	}}
	log := &recordingLog{}
	k := newKeeper(r, log)
	ctx := context.Background()
	_ = k.Wrap(&fakeAssembler{}).Create(ctx, spec("/exports/a", "nqn:lvol:a"))

	k.Tick(ctx)
	first := log.take()
	if len(first) != 1 || !strings.HasPrefix(first[0], "W ") || !strings.Contains(first[0], "10.10.10.143") ||
		!strings.Contains(first[0], "connection refused") || !strings.Contains(first[0], "1 of 2") {
		t.Errorf("first tick logged %q, want one warning naming the path, why, and 1 of 2", first)
	}

	k.Tick(ctx)
	if again := log.take(); len(again) != 0 {
		t.Errorf("a path still down was logged again: %q", again)
	}

	down = false
	k.Tick(ctx)
	back := log.take()
	if len(back) != 1 || !strings.HasPrefix(back[0], "I ") || !strings.Contains(back[0], "10.10.10.143") ||
		!strings.Contains(back[0], "2 of 2") {
		t.Errorf("recovery logged %q, want one line naming the reconnected path and 2 of 2", back)
	}

	k.Tick(ctx)
	if quiet := log.take(); len(quiet) != 0 {
		t.Errorf("a whole volume was logged: %q", quiet)
	}
	states := k.States()
	if st, ok := states["/exports/a"]; !ok || st.Live != 2 || st.Expected != 2 {
		t.Errorf("States() = %+v, want /exports/a at 2 of 2", states)
	}
}

// overlap is a reconcile that waits for company: it reports how many
// reconciles ran at once, and how many of them shared a subsystem.
type overlap struct {
	mu      sync.Mutex
	running map[string]int
	total   int
	most    int
	shared  bool
	want    int
	arrived chan struct{}
}

func (o *overlap) reconcile(ctx context.Context, conn lvol.Connection, _ string) (nvmeof.PathState, error) {
	o.mu.Lock()
	o.running[conn.NQN]++
	o.total++
	o.most = max(o.most, o.total)
	o.shared = o.shared || o.running[conn.NQN] > 1
	if o.total == o.want {
		close(o.arrived)
	}
	o.mu.Unlock()
	// A path that does not come up holds its reconcile for the connector's
	// per-path timeout. Here it is held until every reconcile that could run
	// beside it has started, or for long enough that none will.
	select {
	case <-o.arrived:
	case <-time.After(300 * time.Millisecond):
	}
	o.mu.Lock()
	o.running[conn.NQN]--
	o.total--
	o.mu.Unlock()
	return nvmeof.PathState{NQN: conn.NQN}, nil
}

// A path that is slow to come up holds only its own export: the other exports'
// paths are reconciled beside it in the same tick.
func TestTheKeeperReconcilesExportsBesideEachOther(t *testing.T) {
	o := &overlap{running: map[string]int{}, want: 3, arrived: make(chan struct{})}
	k := &PathKeeper{Reconcile: o.reconcile, Log: &recordingLog{}}
	a := k.Wrap(&fakeAssembler{})
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c"} {
		if err := a.Create(ctx, spec("/exports/"+name, "nqn:lvol:"+name)); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	k.Tick(ctx)
	if o.most != 3 {
		t.Errorf("at most %d of 3 exports were reconciled at once, want all 3", o.most)
	}
}

// Two exports of one subsystem are never reconciled at once: both would
// connect the same missing path, and the kernel would get two controllers for
// one endpoint.
func TestTheKeeperReconcilesOneSubsystemAtATime(t *testing.T) {
	o := &overlap{running: map[string]int{}, want: 3, arrived: make(chan struct{})}
	k := &PathKeeper{Reconcile: o.reconcile, Log: &recordingLog{}}
	a := k.Wrap(&fakeAssembler{})
	ctx := context.Background()
	for path, nqn := range map[string]string{"a": "nqn:lvol:x", "b": "nqn:lvol:x", "c": "nqn:lvol:y"} {
		if err := a.Create(ctx, spec("/exports/"+path, nqn)); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	k.Tick(ctx)
	if o.shared {
		t.Error("two exports of one subsystem were reconciled at once")
	}
}
