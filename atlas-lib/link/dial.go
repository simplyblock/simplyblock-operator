package link

import (
	"context"
	"crypto/tls"
	"net"
)

// TLSDialer opens TLS connections to the hub at addr ("host:port").
//
// This is the dialer to use. The credentials an agent presents are bearer
// tokens: they authenticate whoever holds them, so a plaintext link hands every
// node's identity to anything on the path. cfg supplies the trust roots for the
// hub's serving certificate — a nil cfg falls back to the system roots and
// derives the expected server name from addr, which is right for a
// publicly-rooted certificate and wrong for the in-cluster CA that normally
// signs one.
func TLSDialer(addr string, cfg *tls.Config) Dialer {
	dialer := &tls.Dialer{Config: cfg}
	return func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", addr)
	}
}

// InsecureDialer opens unencrypted TCP connections to addr.
//
// For tests, and for a hub reached over a transport that is already
// authenticated and encrypted underneath (a service mesh with mTLS). Anywhere
// else it publishes the peer's credentials; use [TLSDialer].
func InsecureDialer(addr string) Dialer {
	var dialer net.Dialer
	return func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", addr)
	}
}