package link

import (
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/errs"
)

// PeerKind is the role a peer links as. It is part of a peer's identity rather
// than a property of it: what the operator wants is "the peer that can answer
// for node worker-3", and the kind is what separates that from a controller
// that happens to run on worker-3.
type PeerKind string

const (
	// PeerKindNode is a CSI node plugin: one per node, named by its
	// Kubernetes node name, and the only kind that can speak for a node's
	// local NVMe state.
	PeerKindNode PeerKind = "node"
	// PeerKindController is a CSI controller plugin, named by its pod name.
	// There is normally one, but a rolling update briefly has two, which is
	// why it is not named by anything singular.
	PeerKindController PeerKind = "controller"
)

// PeerID identifies a peer within its kind.
type PeerID struct {
	Kind PeerKind
	Name string
}

// NodePeer is the id of the CSI node plugin on the named Kubernetes node.
func NodePeer(node string) PeerID { return PeerID{Kind: PeerKindNode, Name: node} }

// ControllerPeer is the id of the CSI controller plugin in the named pod.
func ControllerPeer(pod string) PeerID { return PeerID{Kind: PeerKindController, Name: pod} }

// String renders the id as "kind/name", e.g. "node/worker-3".
func (id PeerID) String() string { return string(id.Kind) + "/" + id.Name }

// Zero reports whether the id names nothing.
func (id PeerID) Zero() bool { return id.Kind == "" && id.Name == "" }

// validate rejects ids that cannot address a peer. It is deliberately not a
// check against the known kinds: an authenticator is free to mint kinds this
// package has no constant for, and only it knows which ones it authorizes.
func (id PeerID) validate() error {
	if id.Kind == "" {
		return fmt.Errorf("peer id %q: no kind: %w", id, errs.ErrUnsupported)
	}
	if id.Name == "" {
		return fmt.Errorf("peer id %q: no name: %w", id, errs.ErrUnsupported)
	}
	return nil
}

// Claim is what a peer says about itself when it links. Only an [Authenticator]
// consumes it, and only to check it against credentials — nothing in a claim is
// identity until an authenticator has confirmed it.
type Claim struct {
	// ID is the identity the peer is asking to register as.
	ID PeerID
	// InstanceUID distinguishes this process lifetime of the peer from the
	// one before it (in Kubernetes, the pod UID).
	InstanceUID string
	// Capabilities names the services the peer serves on the session.
	Capabilities []string
	// Version is the peer's atlas build version.
	Version string
}

// Identity is what an [Authenticator] concluded a peer actually is. It is what
// the hub registers and addresses the peer by, whatever the peer claimed.
type Identity struct {
	// ID is the verified identity.
	ID PeerID
	// InstanceUID is the verified process lifetime, when the credentials
	// carry one. An authenticator that cannot verify it may pass the claimed
	// value through: it decides only which of two sessions for one peer wins,
	// never which peer they belong to.
	InstanceUID string
}

// Peer is a linked peer: an identity plus the live session reaching it.
//
// A Peer is only ever handed out by a [Registry], and only while the session is
// up — holding one past that is how a caller ends up issuing RPCs into a dead
// connection. Take it from the registry per use rather than caching it.
type Peer struct {
	// ID is the verified identity the hub bound the session to.
	ID PeerID
	// InstanceUID is the peer's process lifetime; a new one supersedes.
	InstanceUID string
	// Capabilities are the services the peer said it serves.
	Capabilities []string
	// Version is the peer's atlas build version, for skew diagnostics.
	Version string
	// ConnectedAt is when the peer completed its handshake.
	ConnectedAt time.Time

	session *Session
}

// Conn is the connection to the peer: a client of every service the peer
// registered on its side of the link.
func (p *Peer) Conn() grpc.ClientConnInterface { return p.session.Conn() }

// HasCapability reports whether the peer said it serves the named capability.
// Asking beats calling and handling codes.Unimplemented, which is
// indistinguishable from a peer that is merely older.
func (p *Peer) HasCapability(name string) bool { return slices.Contains(p.Capabilities, name) }

// Done is closed when the peer's session ends, whether it was torn down, timed
// out or lost.
func (p *Peer) Done() <-chan struct{} { return p.session.Done() }

// Close tears the peer's session down. The peer is expected to reconnect.
func (p *Peer) Close() error { return p.session.Close() }

// String renders the peer as "kind/name@instance-uid".
func (p *Peer) String() string { return p.ID.String() + "@" + p.InstanceUID }