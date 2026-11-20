// The tracker: it keeps the guest's exports on the endpoints the control plane
// publishes now, by asking again periodically and reassembling an export whose
// volume is published somewhere else (design-pnfs-mds-vm.md §7.6).
//
// The guest reconnects missing paths by itself, from the connection its last
// Create carried, but it cannot ask the control plane, so a volume that moved
// after a migration or a failover would keep its old endpoints until the next
// grow or restart. The runner can ask, and does it here.

package relay

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/simplyblock/atlas/lvol"
	export "github.com/simplyblock/atlas/nfsexport"
)

// DefaultTrackInterval is how often the tracker asks the control plane where
// each export's volume is published. A volume moves rarely, and each question
// is one control-plane call per export.
const DefaultTrackInterval = 5 * time.Minute

// Tracker is a Relay that remembers what it assembled.
type Tracker struct {
	Relay Relay
	// Log reports each reassembly and each failed refresh.
	Log func(format string, args ...any)

	mu      sync.Mutex
	exports map[string]tracked
	locks   map[string]*sync.Mutex
	// version numbers every track, so a refresh can tell that a Create landed
	// after it read an export.
	version uint64
}

type tracked struct {
	spec    export.Spec
	conn    lvol.Connection
	version uint64
}

// Create assembles the export and remembers the connection it was handed. It
// holds the export, so no reassembly or Delete of it runs beside it.
func (t *Tracker) Create(ctx context.Context, spec export.Spec) error {
	unlock := t.lock(spec.Path)
	defer unlock()
	return t.create(ctx, spec)
}

func (t *Tracker) create(ctx context.Context, spec export.Spec) error {
	conn, err := t.Relay.create(ctx, spec)
	if err != nil {
		return err
	}
	t.track(spec, conn)
	return nil
}

// Delete tears the export down and forgets it, holding the export as Create
// does.
func (t *Tracker) Delete(ctx context.Context, spec export.Spec) error {
	unlock := t.lock(spec.Path)
	defer unlock()
	if err := t.Relay.Delete(ctx, spec); err != nil {
		return err
	}
	t.mu.Lock()
	delete(t.exports, spec.Path)
	t.mu.Unlock()
	return nil
}

// Check is the relay's.
func (t *Tracker) Check(ctx context.Context, spec export.Spec) error {
	return t.Relay.Check(ctx, spec)
}

func (t *Tracker) track(spec export.Spec, conn lvol.Connection) {
	spec.Connection = nil
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exports == nil {
		t.exports = map[string]tracked{}
	}
	t.version++
	t.exports[spec.Path] = tracked{spec: spec, conn: conn, version: t.version}
}

// lock holds one export's lock and returns its release.
func (t *Tracker) lock(path string) func() {
	t.mu.Lock()
	if t.locks == nil {
		t.locks = map[string]*sync.Mutex{}
	}
	l, ok := t.locks[path]
	if !ok {
		l = &sync.Mutex{}
		t.locks[path] = l
	}
	t.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Refresh asks the control plane where every tracked export's volume is
// published, and reassembles the ones published somewhere else.
func (t *Tracker) Refresh(ctx context.Context) {
	t.mu.Lock()
	all := make([]tracked, 0, len(t.exports))
	for _, e := range t.exports {
		all = append(all, e)
	}
	t.mu.Unlock()
	slices.SortFunc(all, func(a, b tracked) int { return strings.Compare(a.spec.Path, b.spec.Path) })

	for _, e := range all {
		if ctx.Err() != nil {
			return
		}
		now, _, err := t.Relay.Resolve(ctx, e.spec, e.spec.HostNQN)
		if err != nil {
			t.logf("export %s: asking where volume %s is published: %v", e.spec.Path, e.spec.VolumeUUID, err)
			continue
		}
		if samePaths(now, e.conn) {
			continue
		}
		t.reassemble(ctx, e, now)
	}
}

// reassemble hands the guest the export again, unless a Delete or a Create of
// it landed since the refresh read it: a Delete is final, and a Create carried
// a newer spec and a connection resolved after this one.
func (t *Tracker) reassemble(ctx context.Context, e tracked, now lvol.Connection) {
	unlock := t.lock(e.spec.Path)
	defer unlock()
	if cur, ok := t.current(e.spec.Path); !ok || cur.version != e.version {
		return
	}
	t.logf("export %s: volume %s is published at %v, not %v; reassembling",
		e.spec.Path, e.spec.VolumeUUID, paths(now), paths(e.conn))
	if err := t.create(ctx, e.spec); err != nil {
		t.logf("export %s: reassembling: %v", e.spec.Path, err)
	}
}

func (t *Tracker) current(path string) (tracked, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.exports[path]
	return e, ok
}

// Run refreshes every interval until ctx ends.
func (t *Tracker) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.Refresh(ctx)
		}
	}
}

func (t *Tracker) logf(format string, args ...any) {
	if t.Log != nil {
		t.Log(format, args...)
	}
}

// samePaths reports whether two connections reach the same namespace over the
// same endpoints, in any order: the order is attach priority, and the guest's
// paths are the same set whichever comes first.
func samePaths(a, b lvol.Connection) bool {
	return a.NQN == b.NQN && a.NSID == b.NSID && slices.Equal(paths(a), paths(b))
}

// paths is a connection's endpoints as sorted transport/address:port strings.
func paths(c lvol.Connection) []string {
	out := make([]string, 0, len(c.Endpoints))
	for _, e := range c.Endpoints {
		out = append(out, e.Transport+"/"+e.Address+":"+strconv.Itoa(e.Port))
	}
	slices.Sort(out)
	return out
}
