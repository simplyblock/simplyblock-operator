// Who is calling a service the hub serves back to its peers.
//
// The hub serves those services on a session from the moment it exists, which
// is before the peer has said Hello. Until it has, the caller is anyone who can
// reach the listener, so the hub refuses every call but Hello, and hands each
// call it admits the identity its authenticator verified. A service open to one
// kind of peer reads that identity rather than anything the request claims.

package link

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/link/linkv1"
)

type peerContextKey struct{}

// PeerFromContext is the verified identity of the peer whose call the context
// belongs to, in a service the hub serves. It is false outside one.
func PeerFromContext(ctx context.Context) (PeerID, bool) {
	id, ok := ctx.Value(peerContextKey{}).(PeerID)
	return id, ok
}

// withPeer records the caller's identity on a call's context.
func withPeer(ctx context.Context, id PeerID) context.Context {
	return context.WithValue(ctx, peerContextKey{}, id)
}

// linkServicePrefix is the full method prefix of the handshake service, the
// one service a session may call before it is identified.
var linkServicePrefix = "/" + linkv1.LinkService_ServiceDesc.ServiceName + "/"

// identifiedOnly returns the interceptors that refuse a call to anything but
// the handshake until identity reports the session identified, and record the
// identity on the calls they admit.
func identifiedOnly(identity func() (PeerID, bool)) []grpc.ServerOption {
	admit := func(ctx context.Context, method string) (context.Context, error) {
		if strings.HasPrefix(method, linkServicePrefix) {
			return ctx, nil
		}
		id, ok := identity()
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "link: the session has not identified itself")
		}
		return withPeer(ctx, id), nil
	}
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(
			ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
		) (any, error) {
			ctx, err := admit(ctx, info.FullMethod)
			if err != nil {
				return nil, err
			}
			return handler(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(
			srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler,
		) error {
			ctx, err := admit(ss.Context(), info.FullMethod)
			if err != nil {
				return err
			}
			return handler(srv, &peerStream{ServerStream: ss, ctx: ctx})
		}),
	}
}

// peerStream is a server stream whose context carries the caller.
type peerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *peerStream) Context() context.Context { return s.ctx }
