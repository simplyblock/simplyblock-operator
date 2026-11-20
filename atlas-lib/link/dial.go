package link

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
)

// TLSDialer opens TLS connections to the hub at addr ("host:port").
//
// The credentials an agent presents are bearer tokens: they authenticate
// whoever holds them, so a plaintext link hands every node's identity to
// anything on the path. Use this dialer when the caller already holds a
// [tls.Config], and [TLSFileDialer] when the trust roots are files mounted into
// the pod, which is the in-cluster case. cfg supplies the trust roots for the
// hub's serving certificate. A nil cfg falls back to the system roots and
// derives the expected server name from addr, which is right for a publicly
// rooted certificate and wrong for the in-cluster CA that normally signs one.
func TLSDialer(addr string, cfg *tls.Config) Dialer {
	dialer := &tls.Dialer{Config: cfg}
	return func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", addr)
	}
}

// TLSFiles names the files a [TLSFileDialer] reads on every attempt.
type TLSFiles struct {
	// CAFile is the bundle that signs the hub's serving certificate.
	// Required: a peer that has no way to verify the hub must not hand it a
	// token.
	CAFile string

	// CertFile and KeyFile are the client keypair presented to a hub that
	// requires mutual TLS. Both or neither.
	CertFile string
	KeyFile  string

	// ServerName is the name verified against the hub's certificate. Empty
	// derives it from the address dialed.
	ServerName string
}

// TLSFileDialer opens TLS connections to the hub at addr, reading the CA bundle
// and the client keypair in files at each attempt.
//
// This is the dialer for a peer in a cluster. The files are mounted from
// Secrets that cert-manager or the service-CA operator populate, so on a fresh
// install a peer can start before they exist, and they are rotated in place
// later. Reading them per attempt covers both, which is the same reason
// [TokenFile] re-reads the token. A file that is missing fails the attempt and
// never turns the connection into a plaintext one.
func TLSFileDialer(addr string, files TLSFiles) (Dialer, error) {
	switch {
	case addr == "":
		return nil, errors.New("link dialer: no hub address")
	case files.CAFile == "":
		return nil, errors.New("link dialer: no CA bundle to verify the hub with")
	case (files.CertFile == "") != (files.KeyFile == ""):
		return nil, fmt.Errorf("link dialer: client certificate %q and key %q must be given together",
			files.CertFile, files.KeyFile)
	}

	return func(ctx context.Context) (net.Conn, error) {
		cfg, err := files.clientConfig()
		if err != nil {
			return nil, err
		}
		return TLSDialer(addr, cfg)(ctx)
	}, nil
}

// clientConfig reads the files into the configuration for one attempt.
func (f TLSFiles) clientConfig() (*tls.Config, error) {
	bundle, err := os.ReadFile(f.CAFile)
	if err != nil {
		return nil, fmt.Errorf("link dialer: reading CA bundle: %w", err)
	}
	// The system roots are deliberately not added: the hub is verified by the
	// CA that issued its certificate, and widening that to every public
	// authority would be a downgrade.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, fmt.Errorf("link dialer: CA bundle %s has no usable certificates", f.CAFile)
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: f.ServerName, RootCAs: pool}
	if f.CertFile != "" {
		pair, err := tls.LoadX509KeyPair(f.CertFile, f.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("link dialer: loading client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
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
