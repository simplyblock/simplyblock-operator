package webapi

import "context"

type bearerTokenKey struct{}

// WithBearerToken attaches a bearer credential to ctx that Do and
// DoWithHeaders send instead of the client's own service-account token. It is
// how a call scoped to one cluster authenticates as that cluster rather than
// as this process's own Kubernetes identity -- the only way to reach a
// control plane a different Kubernetes cluster runs (ControlPlane.spec.source.managed),
// since a Kubernetes TokenReview can never cross a cluster boundary.
func WithBearerToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, bearerTokenKey{}, token)
}

// BearerTokenFromContext returns the token WithBearerToken attached, and
// whether one was.
func BearerTokenFromContext(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(bearerTokenKey{}).(string)
	return token, ok
}
