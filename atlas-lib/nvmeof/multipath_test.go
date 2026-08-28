package nvmeof

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
)

const testNQN = "nqn.2023-02.io.simplyblock:vol"

// fabric is a fake kernel fabrics device: a connect write adds a controller
// and the resolver reports the controllers added so far. It records the
// address of every write so tests can assert the order paths were attached in.
type fabric struct {
	ctrls []nvme.Controller
	order []string         // traddr of each connect write, in order
	fail  map[string]error // addresses whose connect write is rejected
	stuck map[string]bool  // addresses whose controller never goes live
	inst  int
}

func (f *fabric) attach(_ context.Context, t Target) (string, error) {
	addr := t.Address
	f.order = append(f.order, addr)
	if err := f.fail[addr]; err != nil {
		return "", err
	}
	state := "live"
	if f.stuck[addr] {
		state = "connecting"
	}
	f.ctrls = append(f.ctrls, ctrl(fmt.Sprintf("nvme%d", f.inst), addr, state))
	f.inst++
	return "instance=0,cntlid=1", nil
}

func (f *fabric) byNQN(_ context.Context, nqn string) (nvme.Subsystem, error) {
	if len(f.ctrls) == 0 {
		return notFound()
	}
	return nvme.Subsystem{NQN: nqn, Controllers: f.ctrls}, nil
}

func (f *fabric) connector() *FabricsConnector {
	return &FabricsConnector{connector: connector{
		subs:        fakeSubs{byNQN: f.byNQN},
		poll:        time.Millisecond,
		pathTimeout: 50 * time.Millisecond,
		attach:      f.attach}}
}

// targets builds the ordered path list the control plane would return:
// primary first, then secondary and tertiary.
func targets(addrs ...string) []Target {
	out := make([]Target, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, Target{NQN: testNQN, Address: a})
	}
	return out
}

func TestConnectPaths_AttachesInPriorityOrder(t *testing.T) {
	f := &fabric{}
	res, err := f.connector().ConnectPaths(context.Background(), targets("10.0.0.1", "10.0.0.2", "10.0.0.3"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if !slices.Equal(f.order, want) {
		t.Errorf("attach order = %v, want %v", f.order, want)
	}
	for i, r := range res {
		if !r.Live || r.Err != nil {
			t.Errorf("path %d (%s): live=%v err=%v, want live", i, r.Target.Address, r.Live, r.Err)
		}
	}
}

// The primary node is restarting: its path is unreachable, but the secondary,
// the current leader, must still be attached before the tertiary.
func TestConnectPaths_SkipsUnreachablePrimaryKeepingOrder(t *testing.T) {
	f := &fabric{fail: map[string]error{"10.0.0.1": errors.New("connection refused")}}
	res, err := f.connector().ConnectPaths(context.Background(), targets("10.0.0.1", "10.0.0.2", "10.0.0.3"))
	if err != nil {
		t.Fatalf("ConnectPaths = %v, want nil: two paths came up", err)
	}
	want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if !slices.Equal(f.order, want) {
		t.Errorf("attach order = %v, want %v (secondary before tertiary)", f.order, want)
	}
	if res[0].Live || res[0].Err == nil {
		t.Errorf("primary: live=%v err=%v, want a recorded failure", res[0].Live, res[0].Err)
	}
	if !res[1].Live || !res[2].Live {
		t.Errorf("secondary/tertiary live = %v/%v, want both live", res[1].Live, res[2].Live)
	}
}

// A path the kernel accepts but never brings live must not hold up the paths
// behind it beyond the per-path timeout.
func TestConnectPaths_PathThatNeverGoesLiveTimesOut(t *testing.T) {
	f := &fabric{stuck: map[string]bool{"10.0.0.1": true}}
	res, err := f.connector().ConnectPaths(context.Background(), targets("10.0.0.1", "10.0.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Live || !errors.Is(res[0].Err, context.DeadlineExceeded) {
		t.Errorf("primary: live=%v err=%v, want a deadline-exceeded failure", res[0].Live, res[0].Err)
	}
	if !res[1].Live {
		t.Errorf("secondary was not attached after the primary stalled: %v", res[1].Err)
	}
}

// The per-path timeout has to bound the controller-state lookups too, not just
// the polling between them: a resolver that blocks (a wedged sysfs read, say)
// would otherwise stall the whole walk. Without the bound this test hangs
// rather than fails, because the fake only returns once its context is done.
func TestConnectPaths_BlockingResolverIsBoundedByPathTimeout(t *testing.T) {
	calls := 0
	c := &FabricsConnector{connector: connector{
		poll:        time.Millisecond,
		pathTimeout: 20 * time.Millisecond,
		subs: fakeSubs{byNQN: func(ctx context.Context, nqn string) (nvme.Subsystem, error) {
			calls++
			if calls == 1 { // the primary's very first lookup wedges
				<-ctx.Done()
				return nvme.Subsystem{}, ctx.Err()
			}
			return nvme.Subsystem{NQN: nqn, Controllers: []nvme.Controller{ctrl("nvme0", "10.0.0.2", "live")}}, nil
		}},
		attach: func(context.Context, Target) (string, error) { return "instance=0,cntlid=1", nil }}}

	res, err := c.ConnectPaths(context.Background(), targets("10.0.0.1", "10.0.0.2"))
	if err != nil {
		t.Fatalf("ConnectPaths = %v, want nil: the secondary came up", err)
	}
	if !errors.Is(res[0].Err, context.DeadlineExceeded) {
		t.Errorf("primary err = %v, want it to carry the per-path deadline", res[0].Err)
	}
	if !res[1].Live {
		t.Errorf("secondary was not attached after the primary's lookup wedged: %v", res[1].Err)
	}
}

func TestConnectPaths_NoPathAvailable(t *testing.T) {
	f := &fabric{fail: map[string]error{
		"10.0.0.1": errors.New("primary refused"),
		"10.0.0.2": errors.New("secondary refused"),
	}}
	_, err := f.connector().ConnectPaths(context.Background(), targets("10.0.0.1", "10.0.0.2"))
	if err == nil {
		t.Fatal("ConnectPaths = nil, want an error when no path could be established")
	}
	for _, want := range []string{"primary refused", "secondary refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to report %q", err, want)
		}
	}
}

func TestConnectPaths_LeavesExistingPathsAlone(t *testing.T) {
	f := &fabric{ctrls: []nvme.Controller{ctrl("nvme0", "10.0.0.1", "live")}, inst: 1}
	res, err := f.connector().ConnectPaths(context.Background(), targets("10.0.0.1", "10.0.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.2"}; !slices.Equal(f.order, want) {
		t.Errorf("attach order = %v, want %v (primary already attached)", f.order, want)
	}
	if !res[0].AlreadyPresent || !res[0].Live {
		t.Errorf("primary: alreadyPresent=%v live=%v, want both true", res[0].AlreadyPresent, res[0].Live)
	}
}

// A controller the kernel is still connecting must be waited for, not
// connected again, because a second write would add a duplicate path.
func TestConnectPaths_WaitsForConnectingPathWithoutReconnecting(t *testing.T) {
	looks := 0
	c := &FabricsConnector{connector: connector{
		poll:        time.Millisecond,
		pathTimeout: time.Second,
		subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
			looks++
			state := "connecting"
			if looks > 2 {
				state = "live"
			}
			return nvme.Subsystem{NQN: nqn, Controllers: []nvme.Controller{ctrl("nvme0", "10.0.0.1", state)}}, nil
		}},
		attach: func(context.Context, Target) (string, error) {
			return "", errors.New("connect must not be re-issued for a connecting path")
		}}}
	res, err := c.ConnectPaths(context.Background(), targets("10.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Live || !res[0].AlreadyPresent {
		t.Errorf("path: live=%v alreadyPresent=%v err=%v", res[0].Live, res[0].AlreadyPresent, res[0].Err)
	}
}

// A canceled context stops the walk instead of running the remaining paths
// against a context that can only fail.
func TestConnectPaths_StopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fabric{fail: map[string]error{"10.0.0.1": errors.New("refused")}}
	c := f.connector()
	c.attach = func(ctx context.Context, tgt Target) (string, error) {
		cancel()
		return f.attach(ctx, tgt)
	}
	res, err := c.ConnectPaths(ctx, targets("10.0.0.1", "10.0.0.2"))
	if err == nil {
		t.Fatal("ConnectPaths = nil, want an error: no path came up")
	}
	if len(res) != 1 {
		t.Errorf("results = %d, want 1: the walk should stop when the context is done", len(res))
	}
}

func TestConnectPaths_RejectsBadInput(t *testing.T) {
	c := (&fabric{}).connector()
	ctx := context.Background()
	if _, err := c.ConnectPaths(ctx, nil); err == nil {
		t.Error("ConnectPaths with no targets = nil, want an error")
	}
	if _, err := c.ConnectPaths(ctx, []Target{{Address: "10.0.0.1"}}); err == nil {
		t.Error("ConnectPaths with an empty NQN = nil, want an error")
	}
	mixed := []Target{{NQN: testNQN, Address: "10.0.0.1"}, {NQN: "nqn.other", Address: "10.0.0.2"}}
	if _, err := c.ConnectPaths(ctx, mixed); err == nil {
		t.Error("ConnectPaths across two subsystems = nil, want an error")
	}
}

// anaSub builds a multipath subsystem: one namespace whose per-controller legs
// carry the given ANA states, keyed by controller id.
func anaSub(states map[string]nvme.ANAState, ctrlIDs ...string) nvme.Subsystem {
	s := nvme.Subsystem{NQN: testNQN}
	ns := nvme.Namespace{ID: 1, Name: "nvme0n1"}
	for _, id := range ctrlIDs {
		s.Controllers = append(s.Controllers, nvme.Controller{
			ID:        nvme.ControllerID(id),
			SysfsPath: filepath.Join("/sys/class/nvme", id),
			State:     "live",
		})
		if st, ok := states[id]; ok {
			ns.Paths = append(ns.Paths, nvme.Path{Controller: nvme.ControllerID(id), NSID: 1, ANAState: st})
		}
	}
	s.Namespaces = []nvme.Namespace{ns}
	return s
}

func ids(ctrls []nvme.Controller) []string {
	out := make([]string, 0, len(ctrls))
	for _, c := range ctrls {
		out = append(out, string(c.ID))
	}
	return out
}

func TestDisconnectOrder(t *testing.T) {
	tests := []struct {
		name string
		sub  nvme.Subsystem
		want []string
	}{
		{
			name: "optimized path last",
			sub: anaSub(map[string]nvme.ANAState{
				"nvme0": nvme.ANAOptimized,
				"nvme1": nvme.ANANonOptimized,
				"nvme2": nvme.ANAInaccessible,
			}, "nvme0", "nvme1", "nvme2"),
			want: []string{"nvme2", "nvme1", "nvme0"},
		},
		{
			name: "optimized path last wherever it sits",
			sub: anaSub(map[string]nvme.ANAState{
				"nvme0": nvme.ANANonOptimized,
				"nvme1": nvme.ANAOptimized,
				"nvme2": nvme.ANANonOptimized,
			}, "nvme0", "nvme1", "nvme2"),
			// Equal rank falls back to reverse attach order.
			want: []string{"nvme2", "nvme0", "nvme1"},
		},
		{
			name: "persistent loss counts as unusable",
			sub: anaSub(map[string]nvme.ANAState{
				"nvme0": nvme.ANAOptimized,
				"nvme1": nvme.ANANonOptimized,
				"nvme2": nvme.ANAPersistentLoss,
			}, "nvme0", "nvme1", "nvme2"),
			want: []string{"nvme2", "nvme1", "nvme0"},
		},
		{
			name: "controller with no ANA leg is released before the optimized one",
			sub: anaSub(map[string]nvme.ANAState{
				"nvme0": nvme.ANAOptimized,
			}, "nvme0", "nvme1"),
			want: []string{"nvme1", "nvme0"},
		},
		{
			name: "equal rank releases in reverse attach order",
			sub: anaSub(map[string]nvme.ANAState{
				"nvme1":  nvme.ANANonOptimized,
				"nvme3":  nvme.ANANonOptimized,
				"nvme10": nvme.ANANonOptimized,
			}, "nvme1", "nvme10", "nvme3"),
			want: []string{"nvme10", "nvme3", "nvme1"},
		},
		{
			name: "single path subsystem",
			sub:  anaSub(nil, "nvme0"),
			want: []string{"nvme0"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ids(disconnectOrder(tc.sub)); !slices.Equal(got, tc.want) {
				t.Errorf("disconnectOrder = %v, want %v", got, tc.want)
			}
		})
	}
}

// A controller can be optimized for one namespace and inaccessible for
// another. Its best state decides, because it may still be carrying I/O.
func TestDisconnectOrder_RanksControllerByBestNamespaceState(t *testing.T) {
	s := nvme.Subsystem{
		NQN: testNQN,
		Controllers: []nvme.Controller{
			{ID: "nvme0", SysfsPath: "/sys/class/nvme/nvme0"},
			{ID: "nvme1", SysfsPath: "/sys/class/nvme/nvme1"},
		},
		Namespaces: []nvme.Namespace{
			{ID: 1, Paths: []nvme.Path{
				{Controller: "nvme0", NSID: 1, ANAState: nvme.ANANonOptimized},
				{Controller: "nvme1", NSID: 1, ANAState: nvme.ANAInaccessible},
			}},
			{ID: 2, Paths: []nvme.Path{
				{Controller: "nvme0", NSID: 2, ANAState: nvme.ANANonOptimized},
				{Controller: "nvme1", NSID: 2, ANAState: nvme.ANAOptimized},
			}},
		},
	}
	if got, want := ids(disconnectOrder(s)), []string{"nvme0", "nvme1"}; !slices.Equal(got, want) {
		t.Errorf("disconnectOrder = %v, want %v (nvme1 is optimized for nsid 2)", got, want)
	}
}

func TestDisconnect_ReleasesOptimizedPathLast(t *testing.T) {
	s := anaSub(map[string]nvme.ANAState{
		"nvme0": nvme.ANAOptimized,
		"nvme1": nvme.ANANonOptimized,
		"nvme2": nvme.ANAInaccessible,
	}, "nvme0", "nvme1", "nvme2")

	var released []string
	c := &FabricsConnector{connector: connector{
		subs: fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) { return s, nil }},
		deleteCtrl: func(ctrl nvme.Controller) error {
			released = append(released, string(ctrl.ID))
			return nil
		}}}
	if err := c.Disconnect(context.Background(), testNQN); err != nil {
		t.Fatal(err)
	}
	if want := []string{"nvme2", "nvme1", "nvme0"}; !slices.Equal(released, want) {
		t.Errorf("release order = %v, want %v", released, want)
	}
}

// One path that refuses to go away must not leave the optimized path attached.
func TestDisconnect_ContinuesPastAFailedRelease(t *testing.T) {
	s := anaSub(map[string]nvme.ANAState{
		"nvme0": nvme.ANAOptimized,
		"nvme1": nvme.ANAInaccessible,
	}, "nvme0", "nvme1")

	var released []string
	c := &FabricsConnector{connector: connector{
		subs: fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) { return s, nil }},
		deleteCtrl: func(ctrl nvme.Controller) error {
			released = append(released, string(ctrl.ID))
			if ctrl.ID == "nvme1" {
				return errors.New("device busy")
			}
			return nil
		}}}
	err := c.Disconnect(context.Background(), testNQN)
	if err == nil || !strings.Contains(err.Error(), "device busy") {
		t.Errorf("Disconnect = %v, want it to report the failed release", err)
	}
	if want := []string{"nvme1", "nvme0"}; !slices.Equal(released, want) {
		t.Errorf("release order = %v, want %v", released, want)
	}
}

func TestTargets_PreservesControlPlaneOrder(t *testing.T) {
	conn := lvol.Connection{
		NQN: testNQN,
		Endpoints: []lvol.Endpoint{
			{Transport: "tcp", Address: "10.0.0.1", Port: 4420},
			{Transport: "tcp", Address: "10.0.0.2", Port: 4421},
			{Transport: "tcp", Address: "10.0.0.3", Port: 4422},
		},
	}

	got := Targets(conn, WithHostNQN("nqn.host"), WithNrIOQueues(4), WithCtrlLossTMOSec(60))
	if len(got) != len(conn.Endpoints) {
		t.Fatalf("Targets returned %d targets, want %d", len(got), len(conn.Endpoints))
	}
	for i, want := range conn.Endpoints {
		if got[i].Address != want.Address || got[i].Port != want.Port {
			t.Errorf("target %d = %s:%d, want %s:%d", i, got[i].Address, got[i].Port, want.Address, want.Port)
		}
		if got[i].NQN != testNQN || got[i].Transport != TransportTCP {
			t.Errorf("target %d = %q over %q, want %q over tcp", i, got[i].NQN, got[i].Transport, testNQN)
		}
		if got[i].HostNQN != "nqn.host" || got[i].NrIOQueues != 4 ||
			got[i].CtrlLossTMOSec == nil || *got[i].CtrlLossTMOSec != 60 {
			t.Errorf("target %d did not get the options applied: %+v", i, got[i])
		}
	}
}

// The control plane picks the connect parameters per path, and all of them have
// to reach the target, not just the address.
func TestTargets_CarriesPerPathConnectParameters(t *testing.T) {
	clt, fiof := 60, 0
	conn := lvol.Connection{
		NQN:  testNQN,
		NSID: 2,
		Endpoints: []lvol.Endpoint{{
			Transport:         "tcp",
			Address:           "10.0.0.1",
			Port:              4420,
			NrIOQueues:        8,
			ReconnectDelaySec: 2,
			KeepAliveTMOSec:   5,
			CtrlLossTMOSec:    &clt,
			FastIOFailTMOSec:  &fiof,
			HostIface:         "eth1",
			TLS:               true,
		}},
	}
	got := Targets(conn, WithHostNQN("nqn.host"), WithHostID("host-id"))[0]

	if got.NrIOQueues != 8 || got.ReconnectDelaySec != 2 || got.KeepAliveTMOSec != 5 {
		t.Errorf("queues/delay/kato = %d/%d/%d, want 8/2/5", got.NrIOQueues, got.ReconnectDelaySec, got.KeepAliveTMOSec)
	}
	if got.CtrlLossTMOSec == nil || *got.CtrlLossTMOSec != clt {
		t.Errorf("ctrl_loss_tmo = %v, want %d", got.CtrlLossTMOSec, clt)
	}
	if got.FastIOFailTMOSec == nil || *got.FastIOFailTMOSec != fiof {
		t.Errorf("fast_io_fail_tmo = %v, want %d", got.FastIOFailTMOSec, fiof)
	}
	if got.HostIface != "eth1" || !got.TLS {
		t.Errorf("host_iface/tls = %q/%v, want eth1/true", got.HostIface, got.TLS)
	}
	if got.HostNQN != "nqn.host" || got.HostID != "host-id" {
		t.Errorf("host identity = %q/%q, want it from the options", got.HostNQN, got.HostID)
	}
}

// The DHCHAP secrets come from the control plane with the rest of the path and
// have to reach the Target, since they are the only key material the connect
// can authenticate with, and no option may rewrite them, because each was
// issued for the host the connection was resolved for.
func TestTargets_CarriesDHCHAPSecrets(t *testing.T) {
	conn := lvol.Connection{NQN: testNQN, Endpoints: []lvol.Endpoint{{
		Transport: "tcp", Address: "10.0.0.1", Port: 4420,
		DHCHAPSecret: "DHHC-1:00:host-secret:", DHCHAPCtrlSecret: "DHHC-1:00:ctrl-secret:",
	}}}
	got := Targets(conn, WithHostNQN(sbHostNQN))[0]

	if got.DHCHAPSecret != "DHHC-1:00:host-secret:" || got.DHCHAPCtrlSecret != "DHHC-1:00:ctrl-secret:" {
		t.Errorf("dhchap secrets = %q/%q, want the control plane's",
			got.DHCHAPSecret, got.DHCHAPCtrlSecret)
	}
	// And they render, under the host identity they belong to.
	opts := mustOptions(t, got, "node-nqn", "node-id")
	for _, want := range []string{
		"hostnqn=" + sbHostNQN, "hostid=" + sbHostID,
		"dhchap_secret=DHHC-1:00:host-secret:", "dhchap_ctrl_secret=DHHC-1:00:ctrl-secret:",
	} {
		if !strings.Contains(opts, want) {
			t.Errorf("options missing %q\n  got: %s", want, opts)
		}
	}
}

// A caller with a local policy overrides what the control plane suggested.
func TestTargets_OptionsOverrideEndpointTunables(t *testing.T) {
	cpTMO, cpFastIOFail := 10, 15
	conn := lvol.Connection{NQN: testNQN, Endpoints: []lvol.Endpoint{{
		Transport: "tcp", Address: "10.0.0.1", Port: 4420,
		NrIOQueues: 8, ReconnectDelaySec: 2, KeepAliveTMOSec: 5,
		CtrlLossTMOSec: &cpTMO, FastIOFailTMOSec: &cpFastIOFail,
		HostIface: "eth1", TLS: true,
	}}}

	got := Targets(conn,
		WithNrIOQueues(2),
		WithReconnectDelaySec(4),
		WithKeepAliveTMOSec(10),
		WithCtrlLossTMOSec(60),
		WithFastIOFailTMOSec(0),
		WithHostIface("eth0"),
		WithTLS(false),
	)[0]

	if got.NrIOQueues != 2 || got.ReconnectDelaySec != 4 || got.KeepAliveTMOSec != 10 {
		t.Errorf("queues/delay/kato = %d/%d/%d, want 2/4/10", got.NrIOQueues, got.ReconnectDelaySec, got.KeepAliveTMOSec)
	}
	if *got.CtrlLossTMOSec != 60 || *got.FastIOFailTMOSec != 0 {
		t.Errorf("ctrl_loss_tmo/fast_io_fail_tmo = %d/%d, want 60/0", *got.CtrlLossTMOSec, *got.FastIOFailTMOSec)
	}
	if got.HostIface != "eth0" || got.TLS {
		t.Errorf("host_iface/tls = %q/%v, want eth0/false", got.HostIface, got.TLS)
	}
	// The endpoint identity stays untouched: options cannot rewrite the path.
	if got.NQN != testNQN || got.Transport != TransportTCP || got.Address != "10.0.0.1" || got.Port != 4420 {
		t.Errorf("endpoint identity changed: %+v", got)
	}
}

// A zero value is a real setting, not "unset": it omits the option and takes
// the kernel default even when the control plane suggested something.
func TestTargets_ZeroOptionClearsControlPlaneValue(t *testing.T) {
	conn := lvol.Connection{NQN: testNQN, Endpoints: []lvol.Endpoint{{
		Transport: "tcp", Address: "10.0.0.1", Port: 4420, NrIOQueues: 8,
	}}}
	opts := mustOptions(t, Targets(conn, WithNrIOQueues(0))[0], "", "")
	if strings.Contains(opts, "nr_io_queues") {
		t.Errorf("options = %q, want no nr_io_queues", opts)
	}
}

// Everything Targets carries has to survive into the connect options line.
func TestTargets_RenderIntoConnectOptions(t *testing.T) {
	clt, fiof := 60, 0
	conn := lvol.Connection{NQN: testNQN, Endpoints: []lvol.Endpoint{{
		Transport: "tcp", Address: "10.0.0.1", Port: 4420,
		NrIOQueues: 8, ReconnectDelaySec: 2, KeepAliveTMOSec: 5,
		CtrlLossTMOSec: &clt, FastIOFailTMOSec: &fiof, HostIface: "eth1", TLS: true,
	}}}
	opts := mustOptions(t, Targets(conn)[0], "nqn.host", "host-id")
	want := "transport=tcp,traddr=10.0.0.1,trsvcid=4420,nqn=" + testNQN +
		",hostnqn=nqn.host,hostid=host-id,host_iface=eth1,nr_io_queues=8," +
		"reconnect_delay=2,keep_alive_tmo=5,ctrl_loss_tmo=60,fast_io_fail_tmo=0,tls"
	if opts != want {
		t.Errorf("options =\n  %q\nwant\n  %q", opts, want)
	}
}

func TestTargets_EmptyConnection(t *testing.T) {
	if got := Targets(lvol.Connection{NQN: testNQN}); len(got) != 0 {
		t.Errorf("Targets = %v, want none", got)
	}
}

// Targets feeds ConnectPaths directly: the control plane's order is the attach
// order.
func TestTargets_FeedsConnectPathsInOrder(t *testing.T) {
	conn := lvol.Connection{
		NQN: testNQN,
		Endpoints: []lvol.Endpoint{
			{Transport: "tcp", Address: "10.0.0.1", Port: defaultTrSvcID},
			{Transport: "tcp", Address: "10.0.0.2", Port: defaultTrSvcID},
		},
	}
	f := &fabric{}
	if _, err := f.connector().ConnectPaths(context.Background(), Targets(conn)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.1", "10.0.0.2"}; !slices.Equal(f.order, want) {
		t.Errorf("attach order = %v, want %v", f.order, want)
	}
}
