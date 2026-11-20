package relay

import (
	"context"
	"errors"
	"testing"

	"github.com/simplyblock/atlas/lvol"
	export "github.com/simplyblock/atlas/nfsexport"
)

const operatorNQN = "nqn.2014-08.io.simplyblock:uuid:5e1d7a0c-0f3b-4c1e-9a77-2b8d1f6e4c10"

var resolved = lvol.Connection{
	NQN: "nqn.2023-02.io.simplyblock:c1:lvol:v1", NSID: 1, UUID: "v1",
	Endpoints: []lvol.Endpoint{{Transport: "tcp", Address: "10.10.1.11", Port: 4420, DHCHAPSecret: "DHHC-1:00:s:"}},
}

// guest records what reached the agent.
type guest struct{ created, deleted, checked []export.Spec }

func (g *guest) Create(_ context.Context, s export.Spec) error {
	g.created = append(g.created, s)
	return nil
}
func (g *guest) Delete(_ context.Context, s export.Spec) error {
	g.deleted = append(g.deleted, s)
	return nil
}
func (g *guest) Check(_ context.Context, s export.Spec) error {
	g.checked = append(g.checked, s)
	return nil
}

// resolver answers with resolved, or fails, and records the identity asked for.
type resolver struct {
	err   error
	asked []string
}

func (r *resolver) resolve(_ context.Context, _ export.Spec, identity string) (lvol.Connection, string, error) {
	r.asked = append(r.asked, identity)
	if r.err != nil {
		return lvol.Connection{}, "", r.err
	}
	return resolved, "", nil
}

func spec() export.Spec {
	return export.Spec{VolumeUUID: "v1", ClusterID: "c1", PoolID: "p1", Path: "/var/lib/simplyblock/exports/a",
		FSID: "v1", Clients: []string{"10.0.0.1"}, HostNQN: operatorNQN}
}

// The connection is resolved for the identity the operator decided, and the
// guest attaches with both.
func TestCreateHandsTheGuestAConnectionResolvedForTheOperatorsIdentity(t *testing.T) {
	g, res := &guest{}, &resolver{}
	if err := (Relay{Guest: g, Resolve: res.resolve}).Create(context.Background(), spec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(res.asked) != 1 || res.asked[0] != operatorNQN {
		t.Errorf("resolved for %v, want [%s]", res.asked, operatorNQN)
	}
	if len(g.created) != 1 || g.created[0].Connection == nil || g.created[0].Connection.NQN != resolved.NQN ||
		g.created[0].HostNQN != operatorNQN {
		t.Fatalf("guest got %+v, want the resolved connection and the operator's identity", g.created)
	}
}

func TestCreateWithoutAConnectionReachesNoGuest(t *testing.T) {
	g := &guest{}
	res := &resolver{err: errors.New("control plane unreachable")}
	if err := (Relay{Guest: g, Resolve: res.resolve}).Create(context.Background(), spec()); err == nil {
		t.Fatal("Create succeeded without a connection")
	}
	if len(g.created) != 0 {
		t.Errorf("guest was asked to assemble without a connection: %+v", g.created)
	}
}

func TestDeleteGoesAheadWithoutTheControlPlane(t *testing.T) {
	g := &guest{}
	res := &resolver{err: errors.New("control plane unreachable")}
	if err := (Relay{Guest: g, Resolve: res.resolve}).Delete(context.Background(), spec()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(g.deleted) != 1 || g.deleted[0].Connection != nil {
		t.Errorf("guest got %+v, want one teardown left to its stack record", g.deleted)
	}
}

func TestDeleteCarriesTheConnectionWhenItResolves(t *testing.T) {
	g := &guest{}
	if err := (Relay{Guest: g, Resolve: (&resolver{}).resolve}).Delete(context.Background(), spec()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(g.deleted) != 1 || g.deleted[0].Connection == nil {
		t.Errorf("guest got %+v, want the resolved connection", g.deleted)
	}
}

func TestCheckResolvesNothing(t *testing.T) {
	g, res := &guest{}, &resolver{}
	if err := (Relay{Guest: g, Resolve: res.resolve}).Check(context.Background(), spec()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(res.asked) != 0 || len(g.checked) != 1 {
		t.Errorf("resolved %v, checked %d, want no resolve and one check", res.asked, len(g.checked))
	}
}

// Secrets reach the guest from the pod's own resolution only. A connection
// arriving with the call is not trusted and not passed on.
func TestAConnectionArrivingWithTheCallIsNotPassedOn(t *testing.T) {
	g := &guest{}
	incoming := spec()
	incoming.Connection = &lvol.Connection{NQN: "nqn.from-the-caller"}
	res := &resolver{err: errors.New("control plane unreachable")}

	if err := (Relay{Guest: g, Resolve: res.resolve}).Delete(context.Background(), incoming); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := (Relay{Guest: g, Resolve: res.resolve}).Check(context.Background(), incoming); err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, s := range append(g.deleted, g.checked...) {
		if s.Connection != nil {
			t.Errorf("passed on the caller's connection %+v", s.Connection)
		}
	}
}
