// A simulated metadata server cluster for the takeover tests: one object that
// is the observer, the record store, the guests, the gateway, the exports, the
// pods, and the event sink, so a test reads what the Failover did as one log.
//
// It lives apart from the tests because every group of them (the takeover, the
// restarts, the rollout) needs the same world, and a second copy would drift.

package mdsfailover

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

const (
	pod0       = "mds-0"
	pod1       = "mds-1"
	pod2       = "mds-2"
	rev1       = "rev1"
	rev2       = "rev2"
	simCluster = "cl1"
	stsName    = "mds"
	boot0      = "u0/c0"
	stepRebind = "rebind"

	simStatefulSetUID = "sts-uid"
	simClusterID      = "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90"
	simStateVolumeID  = "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13"
	simStateVolume    = simClusterID + ":pool1:" + simStateVolumeID
)

type simEvent struct {
	scope, eventType, reason, message string
}

type simPromote struct {
	pod string
	req nfsexportrpc.PromoteRequest
}

type sim struct {
	t       *testing.T
	clock   *fakeClock
	obs     Observation
	rv      int
	roles   map[string]nfsexportrpc.Description
	exports []ExportState

	log      []string
	writes   []Record
	promotes []simPromote
	deleted  []string
	events   []simEvent

	// promoteFn answers a Promote. The default promotes the guest.
	promoteFn func(s *sim, pod string, req nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error)
	// faults fail the n-th call (1-based) of a named step once, as a crash.
	faults map[string]int
	calls  map[string]int
	// observeErr fails every Observe while it is set.
	observeErr error
	// conflictNext makes the next record write lose.
	conflictNext bool

	reg       *prometheus.Registry
	metrics   *Metrics
	failover  *Failover
	config    Config
	maxFlight int
	withdrawn bool
	served    string
}

func newSim(t *testing.T) *sim {
	t.Helper()
	s := &sim{
		t:      t,
		clock:  newFakeClock(),
		roles:  map[string]nfsexportrpc.Description{},
		faults: map[string]int{},
		calls:  map[string]int{},
		reg:    prometheus.NewRegistry(),
	}
	s.metrics = NewMetrics(s.reg)
	s.obs = Observation{
		Cluster: simCluster, StatefulSet: stsName, StatefulSetUID: simStatefulSetUID,
		StateVolume: simStateVolume, Replicas: 2, UpdateRevision: rev1,
		Guests: []Guest{
			s.newGuest(0, "a"), s.newGuest(1, "b"),
		},
	}
	s.roles[pod0] = nfsexportrpc.Description{Role: nfsexportrpc.RoleStandby}
	s.roles[pod1] = nfsexportrpc.Description{Role: nfsexportrpc.RoleStandby}
	return s
}

func (s *sim) newGuest(ordinal int, zone string) Guest {
	return Guest{
		Pod: fmt.Sprintf("mds-%d", ordinal), Ordinal: ordinal, Node: fmt.Sprintf("n%d", ordinal), Zone: zone,
		UID: fmt.Sprintf("u%d", ordinal), Instance: fmt.Sprintf("u%d/c%d", ordinal, ordinal),
		PodIP: fmt.Sprintf("10.0.0.%d", ordinal+1), Revision: rev1,
		Ready: true, NodeReady: true, HasSession: true,
	}
}

func (s *sim) guest(pod string) *Guest {
	for i := range s.obs.Guests {
		if s.obs.Guests[i].Pod == pod {
			return &s.obs.Guests[i]
		}
	}
	s.t.Fatalf("no guest %s", pod)
	return nil
}

// serving makes pod the recorded and running active at the epoch.
func (s *sim) serving(pod string, epoch uint64) {
	g := s.guest(pod)
	for other := range s.roles {
		s.roles[other] = nfsexportrpc.Description{Role: nfsexportrpc.RoleStandby}
	}
	s.obs.Record = Record{Active: pod, Instance: g.Instance, Epoch: epoch}
	s.roles[pod] = nfsexportrpc.Description{Role: nfsexportrpc.RoleActive, Epoch: epoch}
	s.rv++
}

// lose makes the guest vanish from the operator's view: no session, pod and
// node not Ready.
func (s *sim) lose(pod string) {
	g := s.guest(pod)
	g.HasSession, g.Ready, g.NodeReady = false, false, false
}

// bind adds n exports bound to the recorded active, assembled by the instance
// that serves it.
func (s *sim) bind(n int) {
	pod := s.obs.Record.Active
	instance := s.guest(pod).Instance
	for range n {
		s.exports = append(s.exports, ExportState{
			Key:         types.NamespacedName{Namespace: "ns", Name: fmt.Sprintf("export-%02d", len(s.exports))},
			Pod:         pod,
			AssembledBy: instance,
		})
	}
}

// unbound adds n exports that no pod serves yet.
func (s *sim) unbound(n int) {
	for range n {
		s.exports = append(s.exports, ExportState{
			Key: types.NamespacedName{Namespace: "ns", Name: fmt.Sprintf("export-%02d", len(s.exports))},
		})
	}
}

// assemble is the existing resync: every export handed to the active pod is
// assembled by its instance.
func (s *sim) assemble() {
	active := s.obs.Record.Active
	if active == "" {
		return
	}
	for i := range s.exports {
		if s.exports[i].Pod == active {
			s.exports[i].AssembledBy = s.guest(active).Instance
		}
	}
}

func (s *sim) inflight() int {
	n := 0
	active := s.obs.Record.Active
	if active == "" {
		return 0
	}
	for _, e := range s.exports {
		if e.Pod == active && e.AssembledBy != s.guest(active).Instance {
			n++
		}
	}
	return n
}

func (s *sim) deps() Deps {
	return Deps{
		Clock: s.clock, Observer: s, Store: s, Guests: s, Gateway: s, Exports: s, Pods: s, Events: s,
		Metrics: s.metrics, Config: s.config,
		HostNQN: func(uid string, ordinal int) (string, error) { return fmt.Sprintf("nqn-%s-%d", uid, ordinal), nil },
	}
}

// pass runs one pass of the current Failover.
func (s *sim) pass() (Result, error) {
	if s.failover == nil {
		s.failover = New(s.deps())
	}
	return s.failover.Pass(context.Background())
}

// tick runs a pass whose outcome the test reads from the world instead.
func (s *sim) tick() { _, _ = s.pass() }

// restart is an operator restart: every in-memory fact is gone.
func (s *sim) restart() { s.failover = New(s.deps()) }

func (s *sim) fault(step string, nth int) { s.faults[step] = nth }

func (s *sim) crash(step string) error {
	s.calls[step]++
	if s.faults[step] == s.calls[step] {
		s.log = append(s.log, "crash:"+step)
		return errors.New("crash at " + step)
	}
	return nil
}

func (s *sim) logged(entry string) bool { return slices.Contains(s.log, entry) }

func (s *sim) rebinds() int {
	n := 0
	for _, l := range s.log {
		if strings.HasPrefix(l, "rebind:") {
			n++
		}
	}
	return n
}

func (s *sim) reasons() []string {
	out := make([]string, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.reason)
	}
	return out
}

func (s *sim) hasEvent(scope, eventType, reason string) bool {
	return slices.ContainsFunc(s.events, func(e simEvent) bool {
		return e.scope == scope && e.eventType == eventType && e.reason == reason
	})
}

func (s *sim) lastEvent(reason string) (simEvent, bool) {
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i].reason == reason {
			return s.events[i], true
		}
	}
	return simEvent{}, false
}

// Observer.
func (s *sim) Observe(context.Context) (Observation, error) {
	if s.observeErr != nil {
		return Observation{}, s.observeErr
	}
	o := s.obs
	o.Guests = slices.Clone(s.obs.Guests)
	o.Version = strconv.Itoa(s.rv)
	return o, nil
}

// Store.
func (s *sim) Write(_ context.Context, version string, rec Record) (string, error) {
	if s.conflictNext {
		s.conflictNext = false
		s.log = append(s.log, "conflict")
		return "", ErrConflict
	}
	if version != strconv.Itoa(s.rv) {
		s.log = append(s.log, "conflict")
		return "", ErrConflict
	}
	s.rv++
	s.obs.Record = rec
	s.writes = append(s.writes, rec)
	s.log = append(s.log, fmt.Sprintf("record:%s@%d", rec.Active, rec.Epoch))
	return strconv.Itoa(s.rv), nil
}

// Guests.
func (s *sim) Describe(_ context.Context, pod string) (nfsexportrpc.Description, error) {
	if !s.guest(pod).HasSession {
		return nfsexportrpc.Description{}, errors.New("no session")
	}
	return s.roles[pod], nil
}

func (s *sim) Promote(_ context.Context, pod string, req nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error) {
	if err := s.crash("promote"); err != nil {
		return nfsexportrpc.PromoteResult{}, err
	}
	s.promotes = append(s.promotes, simPromote{pod: pod, req: req})
	s.log = append(s.log, fmt.Sprintf("promote:%s@%d", pod, req.Epoch))
	if s.promoteFn != nil {
		return s.promoteFn(s, pod, req)
	}
	return s.activate(pod, req), nil
}

func (s *sim) activate(pod string, req nfsexportrpc.PromoteRequest) nfsexportrpc.PromoteResult {
	s.roles[pod] = nfsexportrpc.Description{Role: nfsexportrpc.RoleActive, Epoch: req.Epoch, Key: req.Key}
	return nfsexportrpc.PromoteResult{Outcome: nfsexportrpc.PromoteActive}
}

// Gateway.
func (s *sim) Withdraw(context.Context, string) error {
	if err := s.crash("withdraw"); err != nil {
		return err
	}
	// Idempotent as the real gateway is: an empty slice asks for nothing.
	if !s.withdrawn {
		s.withdrawn = true
		s.log = append(s.log, "withdraw")
	}
	return nil
}

func (s *sim) Serve(_ context.Context, _, ip string) error {
	if err := s.crash("serve"); err != nil {
		return err
	}
	if s.served != ip {
		s.served = ip
		s.withdrawn = false
		s.log = append(s.log, "serve:"+ip)
	}
	return nil
}

// Exports.
func (s *sim) List(context.Context) ([]ExportState, error) { return slices.Clone(s.exports), nil }

func (s *sim) Specs(_ context.Context, hostNQN string) ([]nfsexport.Spec, error) {
	var specs []nfsexport.Spec
	for _, e := range s.exports {
		if e.Pod != "" {
			specs = append(specs, nfsexport.Spec{VolumeUUID: e.Key.Name, HostNQN: hostNQN})
		}
	}
	return specs, nil
}

func (s *sim) Rebind(_ context.Context, export types.NamespacedName, pod string) error {
	if err := s.crash(stepRebind); err != nil {
		return err
	}
	for i := range s.exports {
		if s.exports[i].Key == export {
			s.exports[i].Pod = pod
		}
	}
	s.log = append(s.log, "rebind:"+export.Name)
	if n := s.inflight(); n > s.maxFlight {
		s.maxFlight = n
	}
	return nil
}

// Pods.
func (s *sim) Delete(_ context.Context, pod string) error {
	s.deleted = append(s.deleted, pod)
	s.log = append(s.log, "delete:"+pod)
	s.guest(pod).Deleting = true
	return nil
}

// Events.
func (s *sim) Driver(eventType, reason, message string) {
	s.events = append(s.events, simEvent{"driver", eventType, reason, message})
}

func (s *sim) Exports(eventType, reason, message string) {
	s.events = append(s.events, simEvent{"exports", eventType, reason, message})
}
