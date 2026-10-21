// The guest's path keeper: it keeps every export's namespace on all the paths
// the control plane published, by reconciling each one periodically against
// the connection its last Create carried (design-pnfs-mds-vm.md §7.6).
//
// It runs in the guest because the guest holds the connection, secrets
// included, from the runner's last Create, and so reconnects a path without the
// control plane or the operator. A path that drops when its storage node
// restarts, one that failed when the export was assembled, and one whose
// controller the kernel removed after ctrl_loss_tmo all come back within one
// interval of being reachable. A changed endpoint is the runner's to bring,
// with a fresh connection on its next Create.

package agent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/simplyblock/atlas/lvol"
	export "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nvmeof"
)

// DefaultPathInterval is how often every export's paths are reconciled.
const DefaultPathInterval = 10 * time.Second

// DefaultPathConcurrency is how many subsystems a tick reconciles at once. A
// path that does not come up holds its reconcile for the connector's per-path
// timeout, so a storage node that is down would otherwise hold every export
// behind the first one with a path to it.
const DefaultPathConcurrency = 8

// Assembler is the export service the keeper wraps.
type Assembler interface {
	Create(ctx context.Context, spec export.Spec) error
	Delete(ctx context.Context, spec export.Spec) error
	Check(ctx context.Context, spec export.Spec) error
}

// ReconcileFunc reconciles one namespace's paths, presenting hostNQN.
type ReconcileFunc func(ctx context.Context, conn lvol.Connection, hostNQN string) (nvmeof.PathState, error)

// PathKeeper reconciles the paths of every export assembled through Wrap.
type PathKeeper struct {
	Reconcile ReconcileFunc
	Log       Logger
	// Concurrency is how many subsystems a tick reconciles at once. Zero is
	// DefaultPathConcurrency.
	Concurrency int

	mu      sync.Mutex
	exports map[string]*kept
	locks   map[string]*sync.Mutex
}

// kept is one export's connection and what was last seen of its paths.
type kept struct {
	conn    lvol.Connection
	hostNQN string
	state   nvmeof.PathState
	// down is every path last seen down, by address, with the reason.
	down    map[string]string
	lastErr string
}

// Wrap returns inner with every successful Create kept and every successful
// Delete forgotten. A call holds its export, so no reconcile of the same
// export runs beside it.
func (k *PathKeeper) Wrap(inner Assembler) Assembler {
	return keeping{inner: inner, k: k}
}

type keeping struct {
	inner Assembler
	k     *PathKeeper
}

func (w keeping) Create(ctx context.Context, spec export.Spec) error {
	unlock := w.k.lock(spec.Path)
	defer unlock()
	if err := w.inner.Create(ctx, spec); err != nil {
		return err
	}
	w.k.keep(spec)
	return nil
}

func (w keeping) Delete(ctx context.Context, spec export.Spec) error {
	unlock := w.k.lock(spec.Path)
	defer unlock()
	if err := w.inner.Delete(ctx, spec); err != nil {
		return err
	}
	w.k.forget(spec.Path)
	return nil
}

func (w keeping) Check(ctx context.Context, spec export.Spec) error {
	return w.inner.Check(ctx, spec)
}

// lock holds one export's lock and returns its release.
func (k *PathKeeper) lock(path string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*sync.Mutex{}
	}
	l, ok := k.locks[path]
	if !ok {
		l = &sync.Mutex{}
		k.locks[path] = l
	}
	k.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (k *PathKeeper) keep(spec export.Spec) {
	if spec.Connection == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.exports == nil {
		k.exports = map[string]*kept{}
	}
	prev := k.exports[spec.Path]
	next := &kept{conn: *spec.Connection, hostNQN: spec.HostNQN, down: map[string]string{}}
	if prev != nil {
		// What was said about the paths stays said: a reassembly does not
		// make a path that is still down news again.
		next.down, next.state, next.lastErr = prev.down, prev.state, prev.lastErr
	}
	k.exports[spec.Path] = next
}

func (k *PathKeeper) forget(path string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.exports, path)
}

// Tick reconciles every kept export once. Subsystems are reconciled beside
// each other, and the exports of one subsystem one after another, since both
// would connect the same missing path.
func (k *PathKeeper) Tick(ctx context.Context) {
	k.mu.Lock()
	bySubsystem := map[string][]string{}
	for path, e := range k.exports {
		bySubsystem[e.conn.NQN] = append(bySubsystem[e.conn.NQN], path)
	}
	k.mu.Unlock()

	limit := k.Concurrency
	if limit <= 0 {
		limit = DefaultPathConcurrency
	}
	slots := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, nqn := range slices.Sorted(maps.Keys(bySubsystem)) {
		paths := bySubsystem[nqn]
		slices.Sort(paths)
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			for _, path := range paths {
				if ctx.Err() != nil {
					return
				}
				k.reconcile(ctx, path)
			}
		})
	}
	wg.Wait()
}

func (k *PathKeeper) reconcile(ctx context.Context, path string) {
	unlock := k.lock(path)
	defer unlock()
	k.mu.Lock()
	e, ok := k.exports[path]
	k.mu.Unlock()
	if !ok {
		return
	}

	state, err := k.Reconcile(ctx, e.conn, e.hostNQN)
	live := fmt.Sprintf("%d of %d paths live", state.Live, state.Expected)
	for _, r := range state.Results {
		addr := r.Target.Address + ":" + strconv.Itoa(r.Target.Port)
		reason, wasDown := e.down[addr]
		switch {
		case !r.Live:
			why := fmt.Sprint(r.Err)
			if !wasDown || reason != why {
				k.Log.Warningf("export %s: the path to %s for %s is down: %s; %s", path, addr, e.conn.NQN, why, live)
			}
			e.down[addr] = why
		case wasDown || !r.AlreadyPresent:
			k.Log.Infof("export %s: reconnected the path to %s for %s; %s", path, addr, e.conn.NQN, live)
			delete(e.down, addr)
		}
	}
	if err != nil && len(state.Results) == 0 {
		if why := err.Error(); why != e.lastErr {
			k.Log.Warningf("export %s: reconciling the paths of %s: %s", path, e.conn.NQN, why)
		}
		e.lastErr = err.Error()
	} else {
		e.lastErr = ""
	}

	k.mu.Lock()
	e.state = state
	k.mu.Unlock()
}

// States returns what was last seen of each kept export's paths, by export
// path.
func (k *PathKeeper) States() map[string]nvmeof.PathState {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make(map[string]nvmeof.PathState, len(k.exports))
	for path, e := range k.exports {
		out[path] = e.state
	}
	return out
}

// Run ticks every interval until ctx ends.
func (k *PathKeeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			k.Tick(ctx)
		}
	}
}
