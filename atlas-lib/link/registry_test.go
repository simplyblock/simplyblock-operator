package link

import (
	"errors"
	"testing"
)

func testPeer(id PeerID, uid string, sess *Session) *Peer {
	return &Peer{ID: id, InstanceUID: uid, session: sess}
}

func TestRegistryRegisterReturnsDisplacedPeer(t *testing.T) {
	r := NewRegistry()

	first := testPeer(NodePeer("worker-1"), "instance-1", &Session{})
	if displaced := r.register(first); displaced != nil {
		t.Fatalf("registering into an empty registry displaced %s", displaced)
	}

	second := testPeer(NodePeer("worker-1"), "instance-2", &Session{})
	displaced := r.register(second)
	if displaced != first {
		t.Fatalf("displaced = %v, want the first peer", displaced)
	}

	got, err := r.Peer(NodePeer("worker-1"))
	if err != nil {
		t.Fatalf("Peer: %v", err)
	}
	if got != second {
		t.Errorf("registry kept the displaced peer")
	}
	if n := r.Len(); n != 1 {
		t.Errorf("Len = %d, want 1", n)
	}
}

// The guard that keeps a slow teardown from undoing a fast reconnect: the old
// session's cleanup runs after its replacement has registered, and must not
// take the live peer with it.
func TestRegistryUnregisterOnlyRemovesItsOwnSession(t *testing.T) {
	r := NewRegistry()

	oldSession, newSession := &Session{}, &Session{}
	r.register(testPeer(NodePeer("worker-1"), "instance-1", oldSession))
	r.register(testPeer(NodePeer("worker-1"), "instance-2", newSession))

	r.unregister(NodePeer("worker-1"), oldSession)

	peer, err := r.Peer(NodePeer("worker-1"))
	if err != nil {
		t.Fatalf("the superseded session's cleanup removed the live peer: %v", err)
	}
	if peer.InstanceUID != "instance-2" {
		t.Errorf("instance = %q, want instance-2", peer.InstanceUID)
	}

	r.unregister(NodePeer("worker-1"), newSession)
	if _, err := r.Peer(NodePeer("worker-1")); !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

func TestRegistryListsPeersInStableOrder(t *testing.T) {
	r := NewRegistry()
	r.register(testPeer(NodePeer("worker-3"), "c", &Session{}))
	r.register(testPeer(NodePeer("worker-1"), "a", &Session{}))
	r.register(testPeer(ControllerPeer("csi-controller-0"), "b", &Session{}))

	want := []string{"controller/csi-controller-0", "node/worker-1", "node/worker-3"}
	for i, peer := range r.Peers() {
		if peer.ID.String() != want[i] {
			t.Errorf("Peers()[%d] = %s, want %s", i, peer.ID, want[i])
		}
	}

	nodes := r.PeersOfKind(PeerKindNode)
	if len(nodes) != 2 {
		t.Fatalf("PeersOfKind(node) returned %d peers, want 2", len(nodes))
	}
	for _, peer := range nodes {
		if peer.ID.Kind != PeerKindNode {
			t.Errorf("PeersOfKind(node) returned %s", peer.ID)
		}
	}
}

func TestPeerIDValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   PeerID
		ok   bool
	}{
		{"complete", NodePeer("worker-1"), true},
		{"no name", PeerID{Kind: PeerKindNode}, false},
		{"no kind", PeerID{Name: "worker-1"}, false},
		{"empty", PeerID{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.id.validate()
			if tc.ok && err != nil {
				t.Errorf("validate() = %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Error("validate() = nil, want an error")
			}
		})
	}
}