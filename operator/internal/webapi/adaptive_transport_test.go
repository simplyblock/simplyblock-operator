// The default address follows the ControlPlane, and no other address does.
//
// A client built with no address is built before any ControlPlane exists, so the
// scheme is decided per request from the installed policy, for the default
// address only.

package webapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// recorder is a transport that remembers what it was asked to send.
type recorder struct {
	scheme string
	calls  int
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	r.scheme = req.URL.Scheme
	return &http.Response{StatusCode: 200, Body: http.NoBody, Request: req}, nil
}

func TestTheAdaptiveTransportFollowsThePolicyForTheDefaultAddress(t *testing.T) {
	const defaultURL, otherURL = "http://simplyblock-webappapi:5000/x", "http://elsewhere.example:5000/x"

	for name, tc := range map[string]struct {
		policy                func(context.Context) bool
		url                   string
		wantPlain, wantSecure int
		wantSecureScheme      string
	}{
		"policy off stays plain":  {func(context.Context) bool { return false }, defaultURL, 1, 0, ""},
		"no policy stays plain":   {nil, defaultURL, 1, 0, ""},
		"policy on upgrades":      {func(context.Context) bool { return true }, defaultURL, 0, 1, schemeHTTPS},
		"another address is left": {func(context.Context) bool { return true }, otherURL, 1, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			SetTLSPolicy(tc.policy)
			t.Cleanup(func() { SetTLSPolicy(nil) })
			plain, secure := &recorder{}, &recorder{}
			tr := &adaptiveTransport{plain: plain, secure: func() (http.RoundTripper, error) { return secure, nil }}

			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, tc.url, nil)
			if _, err := tr.RoundTrip(req); err != nil {
				t.Fatal(err)
			}
			if plain.calls != tc.wantPlain || secure.calls != tc.wantSecure || secure.scheme != tc.wantSecureScheme {
				t.Errorf("plain=%+v secure=%+v", plain, secure)
			}
		})
	}
}

// TLS asked for and unavailable is an error. Falling back to plaintext would send
// the bearer token in the clear on exactly the deployment that asked not to.
func TestAnUnbuildableTLSClientIsAnErrorNotAFallback(t *testing.T) {
	SetTLSPolicy(func(context.Context) bool { return true })
	t.Cleanup(func() { SetTLSPolicy(nil) })
	plain := &recorder{}
	tr := &adaptiveTransport{plain: plain, secure: func() (http.RoundTripper, error) {
		return nil, errors.New("no CA bundle")
	}}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://simplyblock-webappapi:5000/x", nil)
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("expected an error")
	}
	if plain.calls != 0 {
		t.Error("the request fell back to plaintext")
	}
}
