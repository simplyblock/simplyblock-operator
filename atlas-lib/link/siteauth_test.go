package link

// The relay's authenticator: a site's forwarder presents a token for the site
// link's ServiceAccount in that site's namespace on the hub, and the namespace,
// not the forwarder's claim, is the site it is.

import (
	"context"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func siteAccount(username string) authenticationv1.TokenReviewStatus {
	return authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: username},
	}
}

func siteAuthenticator(status authenticationv1.TokenReviewStatus) *SiteAuthenticator {
	return &SiteAuthenticator{Client: kubeClient(status), ServiceAccount: "simplyblock-site-link"}
}

// The site is the namespace of the ServiceAccount the token was issued for.
func TestSiteAuthenticatorTakesTheSiteFromTheNamespace(t *testing.T) {
	a := siteAuthenticator(siteAccount("system:serviceaccount:site-a:simplyblock-site-link"))
	id, err := a.Authenticate(context.Background(), "token", Claim{ID: SitePeer("site-a")})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.ID != SitePeer("site-a") {
		t.Errorf("identity = %s, want %s", id.ID, SitePeer("site-a"))
	}
}

// A forwarder claiming another site than its token's is refused, not corrected.
func TestSiteAuthenticatorRefusesAClaimOfAnotherSite(t *testing.T) {
	a := siteAuthenticator(siteAccount("system:serviceaccount:site-a:simplyblock-site-link"))
	_, err := a.Authenticate(context.Background(), "token", Claim{ID: SitePeer("site-b")})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("site-a's token claiming site-b: %v, want PermissionDenied", err)
	}
}

// Any other ServiceAccount, even in a member's namespace, is not a site link.
func TestSiteAuthenticatorRefusesAnotherServiceAccount(t *testing.T) {
	a := siteAuthenticator(siteAccount("system:serviceaccount:site-a:default"))
	_, err := a.Authenticate(context.Background(), "token", Claim{ID: SitePeer("site-a")})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("another ServiceAccount: %v, want PermissionDenied", err)
	}
}

// Only the site kind links to a relay.
func TestSiteAuthenticatorRefusesAnotherKind(t *testing.T) {
	a := siteAuthenticator(siteAccount("system:serviceaccount:site-a:simplyblock-site-link"))
	_, err := a.Authenticate(context.Background(), "token", Claim{ID: OperatorPeer("site-a")})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("an operator claim: %v, want PermissionDenied", err)
	}
}

// A token the hub does not authenticate is Unauthenticated.
func TestSiteAuthenticatorRefusesAnUnauthenticatedToken(t *testing.T) {
	a := siteAuthenticator(authenticationv1.TokenReviewStatus{Authenticated: false, Error: "expired"})
	_, err := a.Authenticate(context.Background(), "token", Claim{ID: SitePeer("site-a")})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("an unauthenticated token: %v, want Unauthenticated", err)
	}
}
