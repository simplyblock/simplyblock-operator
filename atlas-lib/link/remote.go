package link

import (
	"context"
	"fmt"
	"strings"

	"github.com/simplyblock/atlas/errs"
)

// Kinds a site link adds to the ones a hub's own peers link as.
const (
	// PeerKindSite is a site's forwarder linked to the relay on the hub, named
	// by the site.
	PeerKindSite PeerKind = "site"
	// PeerKindOperator is a site's operator linked to its site's forwarder,
	// named by its pod name.
	PeerKindOperator PeerKind = "operator"
)

// SitePeer is the id a site's forwarder links to the relay as.
func SitePeer(site string) PeerID { return PeerID{Kind: PeerKindSite, Name: site} }

// OperatorPeer is the id of the operator in the named pod, linked to its site's
// forwarder.
func OperatorPeer(pod string) PeerID { return PeerID{Kind: PeerKindOperator, Name: pod} }

// RemotePeer addresses a peer on a site of a stretched cluster, reached through
// the site link rather than through a hub the caller is linked to.
type RemotePeer struct {
	// Site is the site the peer is on.
	Site string
	// Peer is the peer within its site. An empty Name addresses whichever peer
	// of the kind is linked there, which is how a site's operator is addressed:
	// only its leader links.
	Peer PeerID
}

// Remote addresses a peer of kind on site, any linked one when name is empty.
func Remote(site string, kind PeerKind, name string) RemotePeer {
	return RemotePeer{Site: site, Peer: PeerID{Kind: kind, Name: name}}
}

// String renders the address as site/<site>/<kind>, or site/<site>/<kind>/<name> for a named peer.
func (r RemotePeer) String() string {
	s := string(PeerKindSite) + "/" + r.Site + "/" + string(r.Peer.Kind)
	if r.Peer.Name != "" {
		s += "/" + r.Peer.Name
	}
	return s
}

// ParseRemote reads an address [RemotePeer.String] wrote.
func ParseRemote(s string) (RemotePeer, error) {
	parts := strings.SplitN(s, "/", 4)
	if len(parts) < 3 || parts[0] != string(PeerKindSite) || parts[1] == "" || parts[2] == "" {
		return RemotePeer{}, fmt.Errorf("remote peer %q: want site/<site>/<kind>[/<name>]: %w", s, errs.ErrUnsupported)
	}
	r := RemotePeer{Site: parts[1], Peer: PeerID{Kind: PeerKind(parts[2])}}
	if len(parts) == 4 {
		if parts[3] == "" {
			return RemotePeer{}, fmt.Errorf("remote peer %q: empty name: %w", s, errs.ErrUnsupported)
		}
		r.Peer.Name = parts[3]
	}
	return r, nil
}

// peerContextKey carries the verified identity of the peer a call arrived from.
type peerContextKey struct{}

// PeerFromContext is the verified identity of the peer whose session a call
// arrived on, for a service a [Hub] serves to its peers. It is false before the
// session's Hello has succeeded, and on a call that did not arrive through a hub.
func PeerFromContext(ctx context.Context) (PeerID, bool) {
	id, ok := ctx.Value(peerContextKey{}).(PeerID)
	return id, ok
}
