// Tests for asking the node plugins for host network checks. The peers and the
// remote calls are fakes: what is checked is which node is asked, over which
// connection, and what an unlinked or incapable node turns into.

package csilink

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/netcheck"
	"github.com/simplyblock/atlas/netcheck/netcheckrpc"
)

type fakeConn struct{ grpc.ClientConnInterface }

type hostCalls struct {
	readSel  netcheck.Selector
	readConn grpc.ClientConnInterface
	connAddr string
	connPort int
	report   netcheck.Report
	readErr  error
	result   netcheckrpc.Result
	connErr  error
}

func aHostCheck(peers []NodePeer, calls *hostCalls) *HostCheck {
	h := NewHostCheck(func() []NodePeer { return peers })
	h.read = func(_ context.Context, conn grpc.ClientConnInterface, sel netcheck.Selector) (netcheck.Report, error) {
		calls.readSel, calls.readConn = sel, conn
		return calls.report, calls.readErr
	}
	h.connect = func(_ context.Context, _ grpc.ClientConnInterface, address string, port int, _ time.Duration) (netcheckrpc.Result, error) {
		calls.connAddr, calls.connPort = address, port
		return calls.result, calls.connErr
	}
	return h
}

func TestAWorkerWithACapablePeerIsAskedOverItsConnection(t *testing.T) {
	conn := &fakeConn{}
	calls := &hostCalls{report: netcheck.Report{Found: true, Addresses: []string{"10.10.100.8/24"}}}
	h := aHostCheck([]NodePeer{{Name: "worker-1", CanCheck: true}, {Name: "worker-2", CanCheck: true, Conn: conn}}, calls)

	got, err := h.InterfaceAddresses(context.Background(), "worker-2", netcheck.Selector{Name: "ens6"})
	if err != nil || !got.Found || got.Addresses[0] != "10.10.100.8/24" {
		t.Fatalf("InterfaceAddresses = %+v, %v, want the node's report", got, err)
	}
	if calls.readSel.Name != "ens6" || calls.readConn != grpc.ClientConnInterface(conn) {
		t.Errorf("the call went to %v with %+v, want worker-2's connection and the selector", calls.readConn, calls.readSel)
	}
}

func TestAWorkerWithNoLinkedPluginIsUnavailable(t *testing.T) {
	h := aHostCheck([]NodePeer{{Name: "worker-1", CanCheck: true}}, &hostCalls{})
	_, err := h.InterfaceAddresses(context.Background(), "worker-9", netcheck.Selector{Name: "ens6"})
	if !errors.Is(err, netcheckrpc.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable for a worker no plugin is linked on", err)
	}
	if _, _, err := h.Connect(context.Background(), "worker-9", "10.10.100.8", 4426); !errors.Is(err, netcheckrpc.ErrUnavailable) {
		t.Errorf("Connect err = %v, want ErrUnavailable", err)
	}
}

func TestAPluginThatDoesNotAdvertiseTheCheckIsUnavailable(t *testing.T) {
	h := aHostCheck([]NodePeer{{Name: "worker-1", CanCheck: false}}, &hostCalls{})
	_, err := h.InterfaceAddresses(context.Background(), "worker-1", netcheck.Selector{Name: "ens6"})
	if !errors.Is(err, netcheckrpc.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable for an older plugin", err)
	}
}

func TestAFailedCallIsNotMistakenForAnUnavailableNode(t *testing.T) {
	h := aHostCheck([]NodePeer{{Name: "worker-1", CanCheck: true}}, &hostCalls{readErr: errors.New("link dropped")})
	_, err := h.InterfaceAddresses(context.Background(), "worker-1", netcheck.Selector{Name: "ens6"})
	if err == nil || errors.Is(err, netcheckrpc.ErrUnavailable) {
		t.Errorf("err = %v, want the call's own failure, which is not ErrUnavailable", err)
	}
}

func TestAConnectionAnswerCarriesItsReason(t *testing.T) {
	calls := &hostCalls{result: netcheckrpc.Result{Connected: false, Detail: "connection refused"}}
	h := aHostCheck([]NodePeer{{Name: "worker-1", CanCheck: true}}, calls)

	connected, detail, err := h.Connect(context.Background(), "worker-1", "10.10.100.200", 4426)
	if err != nil || connected || detail != "connection refused" {
		t.Fatalf("Connect = %v, %q, %v, want not connected with the reason", connected, detail, err)
	}
	if calls.connAddr != "10.10.100.200" || calls.connPort != 4426 {
		t.Errorf("the node was asked to connect to %s:%d, want 10.10.100.200:4426", calls.connAddr, calls.connPort)
	}

	calls.result = netcheckrpc.Result{Connected: true}
	if connected, detail, err := h.Connect(context.Background(), "worker-1", "10.10.100.200", 4426); err != nil || !connected || detail != "" {
		t.Errorf("Connect = %v, %q, %v, want connected", connected, detail, err)
	}
}

func TestTheWorkersAreTheCapablePluginsInOrder(t *testing.T) {
	h := aHostCheck([]NodePeer{
		{Name: "worker-3", CanCheck: true},
		{Name: "worker-2", CanCheck: false},
		{Name: "worker-1", CanCheck: true},
	}, &hostCalls{})
	if got := h.Workers(); !slices.Equal(got, []string{"worker-1", "worker-3"}) {
		t.Errorf("Workers() = %v, want [worker-1 worker-3]", got)
	}
}
