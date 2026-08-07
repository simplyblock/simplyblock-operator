package link

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"google.golang.org/grpc"
)

// Registry is the set of peers currently linked to a hub — who is reachable
// right now, and how to reach them.
//
// It is the hub's answer to "call node worker-3", and the place absence is
// expressed: a peer that has not linked, or whose session just dropped, is
// simply not in it, and lookups fail with [ErrNoSession]. Registration happens
// through the hub as sessions complete their handshake; callers read.
//
// It is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	peers map[PeerID]*Peer
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{peers: make(map[PeerID]*Peer)}
}

// Peer returns the peer under id.
func (r *Registry) Peer(id PeerID) (*Peer, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.peers[id]
	if !ok {
		return nil, fmt.Errorf("peer %s: %w", id, ErrNoSession)
	}
	return p, nil
}

// Conn returns a connection to the peer under id — the common case, for a
// caller that wants to make a call and not think about sessions.
func (r *Registry) Conn(id PeerID) (grpc.ClientConnInterface, error) {
	p, err := r.Peer(id)
	if err != nil {
		return nil, err
	}
	return p.Conn(), nil
}

// Peers returns every linked peer, ordered by id so the result is stable
// between calls (a map's is not, and this ends up in logs and status output).
func (r *Registry) Peers() []*Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b *Peer) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return out
}

// PeersOfKind returns every linked peer of one kind, ordered as Peers is.
func (r *Registry) PeersOfKind(kind PeerKind) []*Peer {
	all := r.Peers()
	out := make([]*Peer, 0, len(all))
	for _, p := range all {
		if p.ID.Kind == kind {
			out = append(out, p)
		}
	}
	return out
}

// Len is how many peers are linked.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.peers)
}

// register installs p and returns the peer it displaced, if any.
//
// A peer that links while an entry for it already exists is the normal shape of
// a restart: the pod came back and dialled in before the hub noticed the old
// TCP connection was half-open. The new session wins — it is the one demonstrably
// alive — and the caller closes the displaced one.
func (r *Registry) register(p *Peer) *Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.peers[p.ID]
	r.peers[p.ID] = p
	return old
}

// unregister removes the peer under id, but only if it is still the instance
// that owns sess.
//
// The guard is what keeps a slow teardown from undoing a fast reconnect: a
// session that died a moment ago runs its cleanup after its replacement has
// already registered, and an unconditional delete would leave a live peer
// missing from the registry until its next reconnect.
func (r *Registry) unregister(id PeerID, sess *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.peers[id]; ok && p.session == sess {
		delete(r.peers, id)
	}
}