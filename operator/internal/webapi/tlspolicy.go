// Whether the default address is reached over TLS, decided per request.
//
// A client built with no address is built at startup, before the operator has
// read a ControlPlane, so it cannot know whether the control plane serves TLS.
// The ControlPlane is where that is stated, and it can appear, or be recreated
// with another answer, long after every client holding the default address was
// built. So the decision is deferred to the request: the operator installs a
// policy that reads the ControlPlane, and the transport this client carries asks
// it each time.
//
// Only the default address follows the policy. An address somebody supplied, the
// published endpoint of a ControlPlane included, carries its own scheme and is
// taken as written.

package webapi

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// defaultHost is the default address's host, which is what marks a request as
// one the policy decides.
const defaultHost = "simplyblock-webappapi:5000"

// tlsPolicy is the installed policy, held by pointer so that nil means no policy.
var tlsPolicy atomic.Pointer[func(context.Context) bool]

// SetTLSPolicy installs the answer to whether the control plane is reached over
// TLS. Nil removes it, which is plaintext.
func SetTLSPolicy(policy func(context.Context) bool) {
	if policy == nil {
		tlsPolicy.Store(nil)
		return
	}
	tlsPolicy.Store(&policy)
}

func wantsTLS(ctx context.Context) bool {
	policy := tlsPolicy.Load()
	return policy != nil && (*policy)(ctx)
}

// adaptiveTransport sends the default address plain or over the verified
// connection, whichever the policy says at the moment of the request.
type adaptiveTransport struct {
	plain http.RoundTripper

	// secure builds the verified transport, and may fail: the CA bundle arrives
	// with the pod's volumes.
	secure func() (http.RoundTripper, error)
}

func (t *adaptiveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != schemeHTTP || req.URL.Host != defaultHost || !wantsTLS(req.Context()) {
		return t.plain.RoundTrip(req)
	}

	// An error rather than the plain transport: a control plane that serves TLS
	// is asked for it because a token travels on this request.
	secure, err := t.secure()
	if err != nil {
		return nil, fmt.Errorf("webapi client init: %w", err)
	}
	upgraded := req.Clone(req.Context())
	upgraded.URL.Scheme = schemeHTTPS
	return secure.RoundTrip(upgraded)
}

// secureTransport is the verified transport the adaptive one upgrades to.
func secureTransport() (http.RoundTripper, error) {
	c, err := cachedTLSClient()
	if err != nil {
		return nil, err
	}
	return c.Transport, nil
}
