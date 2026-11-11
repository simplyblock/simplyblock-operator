// Tests for the role relay: that a promotion reaches the guest with connections
// the pod resolved for its own host NQN, that a fenced guest's maintenance never
// registers a key, and that a refusal reaches the caller as the guest gave it.

package relay

import (
	"context"
	"errors"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/lvol"
	export "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

const stateVolume = "state"

const podNQN = "nqn.2014-08.io.simplyblock:uuid:7c0e1f52-3a39-5d1c-8d0e-4b6f2a9c1d33"

// roleGuest records the role calls that reached the agent.
type roleGuest struct {
	described nfsexportrpc.Description
	result    nfsexportrpc.PromoteResult
	err       error
	promoted  []nfsexportrpc.PromoteRequest
}

func (g *roleGuest) Describe(context.Context) (nfsexportrpc.Description, error) {
	return g.described, g.err
}

func (g *roleGuest) Promote(_ context.Context, r nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error) {
	g.promoted = append(g.promoted, r)
	return g.result, g.err
}

// volumeResolver answers per volume UUID, and records who it was asked for.
type volumeResolver struct {
	conns map[string]lvol.Connection
	named string
	fail  map[string]error
	asked []askedVolume
}

type askedVolume struct{ volume, identity string }

func (v *volumeResolver) resolve(_ context.Context, s export.Spec, identity string) (lvol.Connection, string, error) {
	v.asked = append(v.asked, askedVolume{s.VolumeUUID, identity})
	if err := v.fail[s.VolumeUUID]; err != nil {
		return lvol.Connection{}, "", err
	}
	return v.conns[s.VolumeUUID], v.named, nil
}

func connFor(volume string, addrs ...string) lvol.Connection {
	c := lvol.Connection{NQN: "nqn.2023-02.io.simplyblock:c1:lvol:" + volume, NSID: 1, UUID: volume}
	for _, a := range addrs {
		c.Endpoints = append(c.Endpoints,
			lvol.Endpoint{Transport: "tcp", Address: a, Port: 4420, DHCHAPSecret: "DHHC-1:00:" + volume})
	}
	return c
}

func exportSpec(volume string) export.Spec {
	return export.Spec{VolumeUUID: volume, ClusterID: "c1", PoolID: "p1", Path: "/var/lib/simplyblock/exports/" + volume,
		FSID: volume, Clients: []string{"10.0.0.1"}, HostNQN: operatorNQN}
}

func promoteRequest() nfsexportrpc.PromoteRequest {
	return nfsexportrpc.PromoteRequest{
		Epoch: 4, Key: 0x01ba781600000004, HostNQN: podNQN, StatefulSetUID: "uid-1",
		StateVolume: nfsexportrpc.VolumeSpec{VolumeUUID: stateVolume, ClusterID: "c1", PoolID: "p1"},
		Exports:     []export.Spec{exportSpec("e1"), exportSpec("e2")},
	}
}

func activeGuest() *roleGuest {
	return &roleGuest{result: nfsexportrpc.PromoteResult{Outcome: nfsexportrpc.PromoteActive}}
}

func newMDS(role *roleGuest, files *guest, res *volumeResolver) (MDS, *Tracker) {
	tr := &Tracker{Relay: Relay{Guest: files, Resolve: res.resolve}, Log: func(string, ...any) {}}
	return MDS{Guest: role, Tracker: tr}, tr
}

func standardResolver() *volumeResolver {
	return &volumeResolver{conns: map[string]lvol.Connection{
		stateVolume: connFor(stateVolume, "10.10.1.11"),
		"e1":        connFor("e1", "10.10.1.11", "10.10.1.12"),
		"e2":        connFor("e2", "10.10.1.11"),
	}}
}

func TestDescribeIsTheGuests(t *testing.T) {
	want := nfsexportrpc.Description{Role: nfsexportrpc.RoleActive, Epoch: 9, Key: 77}
	m, _ := newMDS(&roleGuest{described: want}, &guest{}, standardResolver())
	got, err := m.Describe(context.Background())
	if err != nil || got != want {
		t.Fatalf("Describe = %+v, %v; want %+v", got, err, want)
	}
}

// Every volume the guest connects, the state volume and each export's own, is
// resolved for the pod's host NQN, whatever identity the operator wrote on the spec.
func TestPromoteResolvesEveryVolumeForThePodsHostNQN(t *testing.T) {
	role, res := activeGuest(), standardResolver()
	m, _ := newMDS(role, &guest{}, res)
	if _, err := m.Promote(context.Background(), promoteRequest()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	want := []askedVolume{{stateVolume, podNQN}, {"e1", podNQN}, {"e2", podNQN}}
	if len(res.asked) != len(want) {
		t.Fatalf("resolved %v, want %v", res.asked, want)
	}
	for i, w := range want {
		if res.asked[i] != w {
			t.Errorf("resolution %d was %v, want %v", i, res.asked[i], w)
		}
	}
	if len(role.promoted) != 1 {
		t.Fatalf("the guest got %d promotions, want one", len(role.promoted))
	}
	got := role.promoted[0]
	if got.Epoch != 4 || got.Key != 0x01ba781600000004 || got.HostNQN != podNQN || got.StatefulSetUID != "uid-1" {
		t.Errorf("the promotion changed on the way: %+v", got)
	}
	if c := got.StateVolume.Connection; c == nil || c.UUID != stateVolume || len(c.Endpoints) != 1 {
		t.Errorf("state volume connection = %+v, want the resolved one", c)
	}
	for _, e := range got.Exports {
		if e.Connection == nil || e.Connection.UUID != e.VolumeUUID || e.HostNQN != podNQN {
			t.Errorf("export %s reached the guest as %+v, want its own resolved connection and the pod's NQN", e.VolumeUUID, e)
		}
	}
}

// Secrets reach the guest from the pod's resolution only.
func TestPromoteDropsEveryConnectionTheOperatorSent(t *testing.T) {
	role, res := activeGuest(), standardResolver()
	m, _ := newMDS(role, &guest{}, res)
	req := promoteRequest()
	forged := &lvol.Connection{NQN: "nqn.from-the-caller", NSID: 9, UUID: "x"}
	req.StateVolume.Connection = forged
	for i := range req.Exports {
		req.Exports[i].Connection = forged
	}
	if _, err := m.Promote(context.Background(), req); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(role.promoted) != 1 {
		t.Fatalf("the guest got %d promotions, want one", len(role.promoted))
	}
	got := role.promoted[0]
	if c := got.StateVolume.Connection; c == nil || c.NQN == forged.NQN {
		t.Errorf("the state volume kept the caller's connection")
	}
	for _, e := range got.Exports {
		if e.Connection == nil || e.Connection.NQN == forged.NQN {
			t.Errorf("export %s kept the caller's connection", e.VolumeUUID)
		}
	}
}

// A promotion that cannot resolve a volume reaches no guest: it would fence
// devices and then fail to connect them.
func TestPromoteWithAnUnresolvableVolumeReachesNoGuest(t *testing.T) {
	for _, volume := range []string{stateVolume, "e2"} {
		role, res := &roleGuest{}, standardResolver()
		res.fail = map[string]error{volume: errors.New("control plane unreachable")}
		m, _ := newMDS(role, &guest{}, res)
		if _, err := m.Promote(context.Background(), promoteRequest()); err == nil {
			t.Errorf("%s: Promote succeeded without a connection", volume)
		}
		if len(role.promoted) != 0 {
			t.Errorf("%s: the guest was promoted without a connection: %+v", volume, role.promoted)
		}
	}
}

// One host NQN governs the fence keys of a promotion, so a control plane that
// names another identity for a volume cannot be followed per volume.
func TestPromoteRefusesAnIdentityTheControlPlaneRenamed(t *testing.T) {
	role, res := &roleGuest{}, standardResolver()
	res.named = "nqn.2014-08.io.simplyblock:uuid:someone-else"
	m, _ := newMDS(role, &guest{}, res)
	if _, err := m.Promote(context.Background(), promoteRequest()); err == nil {
		t.Fatal("Promote followed a renamed identity")
	}
	if len(role.promoted) != 0 {
		t.Errorf("the guest was promoted: %+v", role.promoted)
	}
}

// After a promotion the endpoint refresh covers the exports it assembled.
func TestPromoteTracksTheExportsItAssembled(t *testing.T) {
	role, files, res := activeGuest(), &guest{}, standardResolver()
	m, tr := newMDS(role, files, res)
	if _, err := m.Promote(context.Background(), promoteRequest()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	ctx := context.Background()
	tr.Refresh(ctx)
	if len(files.created) != 0 {
		t.Fatalf("an unmoved volume was reassembled: %+v", files.created)
	}
	res.conns["e1"] = connFor("e1", "10.10.1.13", "10.10.1.12")
	tr.Refresh(ctx)
	if len(files.created) != 1 || files.created[0].VolumeUUID != "e1" {
		t.Fatalf("the guest got %+v, want one reassembly of e1", files.created)
	}
	re := files.created[0]
	if c := re.Connection; c == nil || c.Endpoints[0].Address != "10.10.1.13" || re.HostNQN != podNQN {
		t.Errorf("reassembled with %+v for %s, want the new endpoints for the pod's NQN", c, re.HostNQN)
	}
}

// Nothing is tracked from a promotion that did not make the guest active.
func TestAPromotionThatDidNotActivateTracksNothing(t *testing.T) {
	for _, outcome := range []nfsexportrpc.PromoteOutcome{
		nfsexportrpc.PromoteCollision, nfsexportrpc.PromoteSuperseded,
		nfsexportrpc.PromoteFenceFailed, nfsexportrpc.PromoteStaleEpoch,
	} {
		role, files, res := &roleGuest{result: nfsexportrpc.PromoteResult{Outcome: outcome}}, &guest{}, standardResolver()
		m, tr := newMDS(role, files, res)
		if _, err := m.Promote(context.Background(), promoteRequest()); err != nil {
			t.Fatalf("%v: Promote: %v", outcome, err)
		}
		asked := len(res.asked)
		tr.Refresh(context.Background())
		if len(res.asked) != asked {
			t.Errorf("%v: the tracker asked the control plane about exports a failed promotion assembled", outcome)
		}
	}
}

// The outcome and where it failed reach the operator as the guest gave them.
func TestPromoteReturnsTheGuestsOutcomeUnchanged(t *testing.T) {
	want := nfsexportrpc.PromoteResult{
		Outcome: nfsexportrpc.PromoteFenceFailed, Device: "/dev/nvme2n1", Step: "preempt", Message: "boom",
	}
	m, _ := newMDS(&roleGuest{result: want}, &guest{}, standardResolver())
	got, err := m.Promote(context.Background(), promoteRequest())
	if err != nil || got != want {
		t.Fatalf("Promote = %+v, %v; want %+v", got, err, want)
	}
}

// A call the guest fails keeps its class, so the operator tells a refusal from an outage.
func TestAGuestsFailureKeepsItsClass(t *testing.T) {
	refused := class.FromStatus(status.Error(codes.FailedPrecondition, "guest is already active at another epoch"))
	m, _ := newMDS(&roleGuest{err: refused}, &guest{}, standardResolver())
	_, err := m.Promote(context.Background(), promoteRequest())
	if err == nil || class.Code(err) != codes.FailedPrecondition || class.Retryable(err) {
		t.Errorf("Promote error = %v (code %v), want the guest's permanent FailedPrecondition", err, class.Code(err))
	}
	if _, err := m.Describe(context.Background()); err == nil || class.Code(err) != codes.FailedPrecondition {
		t.Errorf("Describe error = %v, want the guest's", err)
	}
}

// refusingGuest is an agent that is not Active: it answers every export call
// with a permanent refusal, and counts them.
type refusingGuest struct{ calls int }

func (r *refusingGuest) Create(context.Context, export.Spec) error {
	r.calls++
	return class.FromStatus(status.Error(codes.FailedPrecondition, "the guest is not active"))
}
func (r *refusingGuest) Delete(context.Context, export.Spec) error { return nil }
func (r *refusingGuest) Check(context.Context, export.Spec) error  { return nil }

// A CreateExport the agent refuses surfaces as the refusal, after one attempt.
func TestARefusedCreateIsSurfacedAndNotRetried(t *testing.T) {
	g := &refusingGuest{}
	tr := &Tracker{Relay: Relay{Guest: g, Resolve: (&resolver{}).resolve}, Log: func(string, ...any) {}}
	err := tr.Create(context.Background(), spec())
	if err == nil || class.Code(err) != codes.FailedPrecondition || class.Retryable(err) {
		t.Fatalf("Create error = %v (code %v), want the agent's permanent refusal", err, class.Code(err))
	}
	if g.calls != 1 {
		t.Errorf("the agent was asked %d times, want one", g.calls)
	}
}

// A fenced old guest keeps getting refreshed. The relay may only reassemble
// (the agent reconnects paths and registers nothing for an export it holds):
// never promote, never delete, and one attempt per pass whatever the answer.
func TestRefreshOnAFencedGuestOnlyReassemblesOncePerPass(t *testing.T) {
	role, files, res := activeGuest(), &guest{}, standardResolver()
	m, tr := newMDS(role, files, res)
	if _, err := m.Promote(context.Background(), promoteRequest()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	refusing := &refusingGuest{}
	tr.Relay.Guest = refusing
	res.conns["e1"] = connFor("e1", "10.10.1.13")
	res.conns["e2"] = connFor("e2", "10.10.1.14")

	tr.Refresh(context.Background())
	if refusing.calls != 2 {
		t.Errorf("the pass made %d Create calls, want one per moved export", refusing.calls)
	}
	if len(role.promoted) != 1 {
		t.Errorf("a refresh promoted the guest: %d promotions, want only the original", len(role.promoted))
	}
	if len(files.deleted) != 0 {
		t.Errorf("a refresh deleted exports: %+v", files.deleted)
	}
}

// Over the link, the operator sees the agent's refusal as a permanent
// FailedPrecondition, not an opaque failure it would retry.
func TestARefusalReachesTheOperatorOverTheLinkAsRefused(t *testing.T) {
	tr := &Tracker{Relay: Relay{Guest: &refusingGuest{}, Resolve: (&resolver{}).resolve}, Log: func(string, ...any) {}}
	srv, err := nfsexportrpc.NewServer(tr)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	srv.Register(g)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	err = nfsexportrpc.Remote(conn).Create(context.Background(), spec())
	if err == nil || class.Code(err) != codes.FailedPrecondition || class.Retryable(err) {
		t.Errorf("the operator saw %v (code %v), want a permanent FailedPrecondition", err, class.Code(err))
	}
}
