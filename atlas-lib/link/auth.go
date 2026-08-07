package link

import (
	"context"
	"crypto/subtle"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// authorizationHeader is where a peer puts its bearer token in the Hello call's
// metadata. It is the standard gRPC spelling, so an interceptor or proxy in
// between reads it as credentials rather than as an opaque field.
const authorizationHeader = "authorization"

const bearerPrefix = "Bearer "

// Authenticator decides who a peer is.
//
// It is handed the credentials the peer presented and everything the peer
// claimed about itself, and it answers with the identity the hub should
// register — which is not required to be, and for a shared credential must not
// be, the identity that was claimed. Returning an error refuses the link.
//
// Errors should carry a gRPC status (codes.Unauthenticated for credentials that
// do not check out, codes.PermissionDenied for credentials that do but do not
// entitle the peer to the identity it asked for), since the peer sees them.
// errs/class turns an ordinary error into a status if one does not.
//
// Implementations must be safe for concurrent use.
type Authenticator interface {
	Authenticate(ctx context.Context, token string, claim Claim) (Identity, error)
}

// InsecureStaticAuthenticator accepts any peer that presents a fixed shared
// token and registers it as whatever it claimed to be.
//
// The name is the warning: it takes identity from the peer. Every peer holding
// the token can register as any other, so one compromised node can claim
// another's name and answer for its volumes. It exists for tests and for
// single-peer development setups. In a cluster use [KubeAuthenticator].
type InsecureStaticAuthenticator struct {
	// Token is the shared secret every peer must present.
	Token string
}

// Authenticate compares tokens in constant time and echoes the claim back.
func (a InsecureStaticAuthenticator) Authenticate(_ context.Context, token string, claim Claim) (Identity, error) {
	if a.Token == "" {
		return Identity{}, status.Error(codes.Internal, "link: static authenticator has no token configured")
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(a.Token)) != 1 {
		return Identity{}, status.Error(codes.Unauthenticated, "link: invalid token")
	}
	if err := claim.ID.validate(); err != nil {
		return Identity{}, status.Errorf(codes.InvalidArgument, "link: %v", err)
	}
	return Identity{ID: claim.ID, InstanceUID: claim.InstanceUID}, nil
}

// bearerToken pulls the peer's credentials out of the Hello call's metadata.
func bearerToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "link: no credentials")
	}
	values := md.Get(authorizationHeader)
	if len(values) == 0 {
		return "", status.Error(codes.Unauthenticated, "link: no credentials")
	}
	if len(values[0]) <= len(bearerPrefix) || values[0][:len(bearerPrefix)] != bearerPrefix {
		return "", status.Error(codes.Unauthenticated, "link: credentials are not a bearer token")
	}
	return values[0][len(bearerPrefix):], nil
}

// withBearerToken attaches a peer's credentials to an outgoing Hello.
func withBearerToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, authorizationHeader, bearerPrefix+token)
}

// TokenSource produces the credentials a peer presents when it links.
//
// It is called on every link attempt rather than once at startup, because the
// credentials it returns expire: a Kubernetes projected ServiceAccount token is
// rewritten well before its expiry, and a peer that read it once would go on
// presenting the stale one and start failing to reconnect hours later — during
// an outage, which is exactly when it is reconnecting.
type TokenSource func(ctx context.Context) (string, error)

// StaticToken is a TokenSource that always returns the same token. For tests
// and for [InsecureStaticAuthenticator]; a real deployment's credentials rotate.
func StaticToken(token string) TokenSource {
	return func(context.Context) (string, error) { return token, nil }
}

// TokenFile reads the peer's token from a file on every call, which is what
// makes projected ServiceAccount token rotation transparent.
func TokenFile(path string) TokenSource {
	return func(context.Context) (string, error) {
		token, err := readTrimmedFile(path)
		if err != nil {
			return "", fmt.Errorf("link: reading token %s: %w", path, err)
		}
		if token == "" {
			return "", fmt.Errorf("link: token file %s is empty", path)
		}
		return token, nil
	}
}