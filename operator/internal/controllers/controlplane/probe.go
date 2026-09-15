// The readiness probe, and the endpoint it is aimed at.
//
// The probe is a direct GET rather than a value taken off the event stream,
// because it is the check that the stream itself can be established
// (design-crd-model.md §7.7 makes the stream the way state arrives, and this is
// the one read that cannot depend on it).
//
// It is an interface rather than a function so that the reconciler's branches —
// a control plane that answers, one that does not, one whose version disagrees
// with what an upgrade asked for — are unit-testable without an HTTP server. The
// live implementation is one http.Client and two paths.

package controlplane

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The two reads design-controlplane.md §8 names.
const (
	readyPath   = "/api/v2/_meta/ready"
	versionPath = "/api/v2/_meta/version"
)

// probeTimeout bounds one readiness check. It is short on purpose: the probe
// runs on every reconcile of every control plane, and a control plane that takes
// longer than this to say it is ready is one whose latency is already the
// problem.
const probeTimeout = 10 * time.Second

// Prober answers the two questions the reconciler asks of a running control
// plane.
type Prober interface {
	// Ready reports whether the control plane answers its readiness endpoint. A
	// false comes with the control plane's own words rather than a paraphrase of
	// them, because that string is what lands in status.message and in the event
	// an administrator reads.
	Ready(ctx context.Context, endpoint string) (ok bool, reason string)

	// Version reports what the management API says it is. An empty version with
	// a nil error is an endpoint that does not serve the read, which is the
	// state every control plane is in until it ships: design-controlplane.md §8
	// records that /_meta/version does not exist yet, so the caller publishes
	// nothing rather than treating its absence as a failure.
	Version(ctx context.Context, endpoint string) (string, error)
}

// HTTPProber probes over HTTP with a bearer token.
type HTTPProber struct {
	// Client is the transport, which carries the TLS configuration where the
	// deployment has one. A nil client is one with the probe's own timeout.
	Client *http.Client

	// Token is the bearer the operator authenticates with. Empty is an
	// unauthenticated request, which is what the readiness endpoint takes on a
	// deployment that does not require a token for it.
	Token string
}

// Ready performs the readiness read.
func (p *HTTPProber) Ready(ctx context.Context, endpoint string) (bool, string) {
	body, status, err := p.get(ctx, endpoint, readyPath)
	switch {
	case err != nil:
		return false, err.Error()
	case status >= 300:
		// The control plane's own body rather than a sentence about it: a 503
		// whose body names the FoundationDB error is the whole of what an
		// administrator needs, and wrapping it loses that.
		if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
			return false, fmt.Sprintf("status=%d: %s", status, trimmed)
		}
		return false, fmt.Sprintf("status=%d", status)
	default:
		return true, ""
	}
}

// Version performs the version read.
//
// A 404 is an endpoint that does not serve it, which is reported as no version
// rather than as an error: the read is a prerequisite this repository is waiting
// on, and until it lands every deployment would otherwise report a failure on
// every pass.
func (p *HTTPProber) Version(ctx context.Context, endpoint string) (string, error) {
	body, status, err := p.get(ctx, endpoint, versionPath)
	switch {
	case err != nil:
		return "", err
	case status == http.StatusNotFound:
		return "", nil
	case status >= 300:
		return "", fmt.Errorf("status=%d: %s", status, strings.TrimSpace(string(body)))
	}
	return parseVersion(body), nil
}

// get performs one request against a path of the endpoint.
func (p *HTTPProber) get(ctx context.Context, endpoint, path string) ([]byte, int, error) {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: probeTimeout}
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	url := strings.TrimSuffix(endpoint, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	// The body is read to a bound rather than in full: it lands in
	// status.message, which is one sentence, and a control plane returning a
	// stack trace should not put it on the object.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// parseVersion pulls the version out of whatever the endpoint returns.
//
// The read does not exist yet (design-controlplane.md §8), so its response shape
// is not settled either. What is handled is the two shapes it could reasonably
// take — a bare string, or a JSON object with a version field — and anything
// else reads as no version, which publishes nothing rather than publishing
// noise.
func parseVersion(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	if !strings.HasPrefix(trimmed, "{") {
		return strings.Trim(trimmed, `"`)
	}
	for _, key := range []string{`"version"`, `"result"`} {
		if i := strings.Index(trimmed, key); i >= 0 {
			rest := trimmed[i+len(key):]
			if j := strings.Index(rest, `"`); j >= 0 {
				rest = rest[j+1:]
				if k := strings.Index(rest, `"`); k >= 0 {
					return rest[:k]
				}
			}
		}
	}
	return ""
}
