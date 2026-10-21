// Package nvmeprtest is an in-memory NVMe namespace with persistent
// reservations, for tests of whatever drives package nvmepr. It models what a
// real target did when the fence's commands were tried against SPDK on
// 2026-10-21, and nothing beyond it: each Host is one host's view of the one
// shared namespace, so a test can play an old active and a newer one against the
// same reservation.
package nvmeprtest

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/simplyblock/atlas/nvmepr"
)

// Namespace is the shared reservation state of one namespace.
type Namespace struct {
	mu            sync.Mutex
	registrants   []registrant
	holder        int // index into registrants, or -1
	rtype         nvmepr.Type
	generation    uint32
	maxRegistrant int
	noAbort       bool
	fail          map[string][]error
	calls         []Call
}

type registrant struct {
	host [16]byte
	key  uint64
}

// Call is one command a host sent, for a test to assert on the order of.
type Call struct {
	Host [16]byte
	Op   string
	Keys []uint64
}

// Option configures New.
type Option func(*Namespace)

// WithMaxRegistrants limits the namespace to n registered hosts, as SPDK's
// fork did at 16 before its limit was raised.
func WithMaxRegistrants(n int) Option { return func(ns *Namespace) { ns.maxRegistrant = n } }

// WithoutPreemptAndAbort makes the target answer Preempt and Abort with
// Invalid Field.
func WithoutPreemptAndAbort() Option { return func(ns *Namespace) { ns.noAbort = true } }

// New returns an empty namespace.
func New(opts ...Option) *Namespace {
	ns := &Namespace{holder: -1, fail: map[string][]error{}}
	for _, o := range opts {
		o(ns)
	}
	return ns
}

// FailNext makes the next command of op return err, once per call. The ops are
// `register`, `acquire`, `preempt`, and `report`.
func (ns *Namespace) FailNext(op string, errs ...error) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	ns.fail[op] = append(ns.fail[op], errs...)
}

// Calls returns every command sent so far, oldest first.
func (ns *Namespace) Calls() []Call {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return slices.Clone(ns.calls)
}

// CanRead reports whether the target would serve a read from host.
func (ns *Namespace) CanRead(host [16]byte) bool { return ns.canAccess(host) }

// CanWrite reports whether the target would serve a write from host.
func (ns *Namespace) CanWrite(host [16]byte) bool { return ns.canAccess(host) }

// canAccess applies a registrants-only reservation: with one held, a host that
// is not registered can neither read nor write.
func (ns *Namespace) canAccess(host [16]byte) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return ns.holder < 0 || ns.index(host) >= 0
}

// Host returns the namespace as host sees it.
func (ns *Namespace) Host(host [16]byte) nvmepr.Device {
	return &hostView{ns: ns, host: host}
}

func (ns *Namespace) index(host [16]byte) int {
	return slices.IndexFunc(ns.registrants, func(r registrant) bool { return r.host == host })
}

type hostView struct {
	ns   *Namespace
	host [16]byte
}

func (v *hostView) Close() error { return nil }

// enter records the call and returns an injected failure, with the lock held.
func (v *hostView) enter(op string, keys ...uint64) error {
	ns := v.ns
	ns.mu.Lock()
	ns.calls = append(ns.calls, Call{Host: v.host, Op: op, Keys: keys})
	if q := ns.fail[op]; len(q) > 0 {
		err := q[0]
		ns.fail[op] = q[1:]
		ns.mu.Unlock()
		return err
	}
	return nil
}

func (v *hostView) Report(context.Context) (nvmepr.Report, error) {
	if err := v.enter("report"); err != nil {
		return nvmepr.Report{}, err
	}
	defer v.ns.mu.Unlock()
	ns := v.ns
	r := nvmepr.Report{Generation: ns.generation}
	if ns.holder >= 0 {
		r.Type = ns.rtype
	}
	for i, reg := range ns.registrants {
		r.Registrants = append(r.Registrants, nvmepr.Registrant{
			ControllerID: 0xffff, HostID: reg.host, Key: reg.key, HoldsReservation: i == ns.holder,
		})
	}
	return r, nil
}

func (v *hostView) Register(_ context.Context, oldKey, newKey uint64, ignoreExistingKey bool) error {
	if err := v.enter("register", oldKey, newKey); err != nil {
		return err
	}
	defer v.ns.mu.Unlock()
	ns := v.ns
	i := ns.index(v.host)
	switch {
	case i < 0 && oldKey == 0 || i < 0 && ignoreExistingKey:
		if ns.maxRegistrant > 0 && len(ns.registrants) >= ns.maxRegistrant {
			return errors.New("register: too many registrants")
		}
		ns.registrants = append(ns.registrants, registrant{host: v.host, key: newKey})
	case i < 0:
		return nvmepr.ErrReservationConflict
	case oldKey == 0 && ns.registrants[i].key != newKey:
		return nvmepr.ErrReservationConflict
	case oldKey != 0 && !ignoreExistingKey && ns.registrants[i].key != oldKey:
		return nvmepr.ErrReservationConflict
	default:
		ns.registrants[i].key = newKey
	}
	ns.generation++
	return nil
}

func (v *hostView) Acquire(_ context.Context, key uint64, t nvmepr.Type) error {
	if err := v.enter("acquire", key); err != nil {
		return err
	}
	defer v.ns.mu.Unlock()
	ns := v.ns
	i := ns.index(v.host)
	if i < 0 || ns.registrants[i].key != key {
		return nvmepr.ErrReservationConflict
	}
	if ns.holder >= 0 && ns.holder != i {
		return nvmepr.ErrReservationConflict
	}
	ns.holder, ns.rtype = i, t
	ns.generation++
	return nil
}

func (v *hostView) Preempt(_ context.Context, key, victim uint64, t nvmepr.Type, abort bool) error {
	if err := v.enter("preempt", key, victim); err != nil {
		return err
	}
	defer v.ns.mu.Unlock()
	ns := v.ns
	if abort && ns.noAbort {
		return nvmepr.ErrInvalidField
	}
	caller := ns.index(v.host)
	if caller < 0 || ns.registrants[caller].key != key {
		return nvmepr.ErrReservationConflict
	}
	holderHost := [16]byte{}
	if ns.holder >= 0 {
		holderHost = ns.registrants[ns.holder].host
	}
	holderPreempted := false
	kept := ns.registrants[:0:0]
	for _, reg := range ns.registrants {
		if reg.key == victim && reg.host != v.host {
			holderPreempted = holderPreempted || (ns.holder >= 0 && reg.host == holderHost)
			continue
		}
		kept = append(kept, reg)
	}
	ns.registrants = kept
	switch {
	case ns.holder >= 0 && holderPreempted:
		ns.holder, ns.rtype = ns.index(v.host), t
	case ns.holder >= 0:
		ns.holder = ns.index(holderHost)
	}
	ns.generation++
	return nil
}
