package link

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

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

// SiteAuthenticator is the relay's authenticator. A site's forwarder links with
// a token for the site link's ServiceAccount in that site's namespace on the
// hub, which the forwarder requests with the credentials the hub's add-on
// registration gave it. The namespace is the site: Open Cluster Management names
// a member's namespace on the hub after the member, and only that member's
// add-on may request a token there.
//
// A forwarder claiming any other site, or a token for any other ServiceAccount,
// is refused rather than corrected.
type SiteAuthenticator struct {
	// Client talks to the hub's API server. It needs create on tokenreviews.
	Client kubernetes.Interface

	// ServiceAccount is the site link's ServiceAccount name, the same in every
	// member namespace.
	ServiceAccount string

	// Audiences, when set, is the audience the token must have been issued for.
	Audiences []string
}

// Authenticate verifies a forwarder's token and returns the site it is for.
func (a *SiteAuthenticator) Authenticate(ctx context.Context, token string, claim Claim) (Identity, error) {
	if a.Client == nil || a.ServiceAccount == "" {
		return Identity{}, status.Error(codes.Internal, "link: site authenticator is not configured")
	}
	if claim.ID.Kind != PeerKindSite {
		return Identity{}, status.Errorf(codes.PermissionDenied, "link: only a site forwarder links to the relay, not a %s peer",
			claim.ID.Kind)
	}

	review, err := a.Client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: a.Audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		return Identity{}, status.Errorf(codes.Unavailable, "link: token review: %v", err)
	}
	if !review.Status.Authenticated {
		reason := review.Status.Error
		if reason == "" {
			reason = "token not authenticated"
		}
		return Identity{}, status.Errorf(codes.Unauthenticated, "link: %s", reason)
	}

	namespace, account, err := splitServiceAccount(review.Status.User.Username)
	if err != nil {
		return Identity{}, status.Errorf(codes.PermissionDenied, "link: %v", err)
	}
	if account != a.ServiceAccount {
		return Identity{}, status.Errorf(codes.PermissionDenied,
			"link: ServiceAccount %s/%s is not the site link's", namespace, account)
	}
	if claim.ID.Name != "" && claim.ID.Name != namespace {
		return Identity{}, status.Errorf(codes.PermissionDenied,
			"link: forwarder claimed site %s but its token is for site %s", claim.ID.Name, namespace)
	}
	return Identity{ID: SitePeer(namespace), InstanceUID: claim.InstanceUID}, nil
}
