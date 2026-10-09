// Tests for asking the node plugins to forget the flows translated to a
// replaced metadata server pod. The peers and the remote call are fakes: what
// is checked is who is asked, for what, and how often.

package csilink

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/conntrack"
)

type forgetCall struct {
	node string
	sel  conntrack.Selector
}

type recordedForgets struct {
	mu    sync.Mutex
	calls []forgetCall
	done  chan struct{}
	want  int
	fail  string
}

func (r *recordedForgets) forget(_ context.Context, node string, _ grpc.ClientConnInterface, sel conntrack.Selector) (uint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, forgetCall{node: node, sel: sel})
	if len(r.calls) == r.want {
		close(r.done)
	}
	if node == r.fail {
		return 0, errors.New("operation not permitted")
	}
	return 1, nil
}

func newTestFlows(peers []NodePeer, rec *recordedForgets) *Flows {
	f := NewFlows(func() []NodePeer { return peers })
	f.delay = time.Millisecond
	f.forget = rec.forget
	return f
}

func wait(t *testing.T, rec *recordedForgets) {
	t.Helper()
	select {
	case <-rec.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("only %d forget call(s) arrived, want %d", len(rec.calls), rec.want)
	}
}

var oldPod = "10.244.3.215"

// Every node that can forget is asked, for NFS's port and the old address only.
func TestEveryCapableNodeForgetsTheOldAddress(t *testing.T) {
	rec := &recordedForgets{done: make(chan struct{}), want: 2}
	peers := []NodePeer{
		{Name: "worker-1", CanForget: true},
		{Name: "worker-2", CanForget: true},
		{Name: "worker-3", CanForget: false},
	}
	newTestFlows(peers, rec).Forget(oldPod)
	wait(t, rec)

	want := conntrack.Selector{Protocol: conntrack.TCP, DstPort: 2049, ReplySource: netip.MustParseAddr(oldPod)}
	nodes := make([]string, 0, len(rec.calls))
	for _, c := range rec.calls {
		nodes = append(nodes, c.node)
		if c.sel != want {
			t.Errorf("%s asked to forget %+v, want %+v", c.node, c.sel, want)
		}
	}
	slices.Sort(nodes)
	if !slices.Equal(nodes, []string{"worker-1", "worker-2"}) {
		t.Errorf("asked %v, want the two nodes advertising the capability", nodes)
	}
}

// Every export bound to one metadata server asks for the same address at once.
// The nodes are asked once.
func TestRequestsForOneAddressCollapse(t *testing.T) {
	rec := &recordedForgets{done: make(chan struct{}), want: 1}
	f := newTestFlows([]NodePeer{{Name: "worker-1", CanForget: true}}, rec)
	f.delay = 50 * time.Millisecond
	for range 5 {
		f.Forget(oldPod)
	}
	wait(t, rec)
	time.Sleep(100 * time.Millisecond)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 {
		t.Errorf("worker-1 asked %d times, want once", len(rec.calls))
	}
}

// One node failing does not stop the others.
func TestANodeFailingDoesNotStopTheOthers(t *testing.T) {
	rec := &recordedForgets{done: make(chan struct{}), want: 2, fail: "worker-1"}
	newTestFlows([]NodePeer{{Name: "worker-1", CanForget: true}, {Name: "worker-2", CanForget: true}}, rec).Forget(oldPod)
	wait(t, rec)
}

// Something that is not an address asks nobody.
func TestAnInvalidAddressAsksNobody(t *testing.T) {
	rec := &recordedForgets{done: make(chan struct{}), want: 1}
	f := newTestFlows([]NodePeer{{Name: "worker-1", CanForget: true}}, rec)
	f.Forget("not-an-ip")
	f.Forget("")
	time.Sleep(50 * time.Millisecond)
	if len(rec.calls) != 0 {
		t.Errorf("asked %v for an invalid address", rec.calls)
	}
}
