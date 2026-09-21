// The connection a Client is given, rather than the one it would have built.
//
// A control plane behind TLS is reached with a pool that verifies it and, where
// it requires one, a certificate to present. Neither is expressible by an
// endpoint and a token, so a Client that always built its own transport could
// only ever reach a plaintext control plane -- which is what it did.

package controlplane

import (
	"net/http"
	"testing"
	"time"
)

// countingTransport records that it carried a request and answers a bare 200.
type countingTransport struct{ calls int }

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

// A configured transport is the one the requests go over.
func TestTheConfiguredTransportCarriesTheRequests(t *testing.T) {
	carrier := &countingTransport{}
	client, err := New(Config{
		Endpoint:  "https://control-plane.simplyblock.svc.cluster.local:5000",
		Token:     "a-token",
		Timeout:   time.Second,
		Transport: carrier,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Any call will do: what is asserted is which connection it went over, not
	// what came back, and a bare 200 with no body fails to decode by design.
	_, _ = client.ListBackups(t.Context(), "3f2b1c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d")

	if carrier.calls == 0 {
		t.Error("the client built its own transport, so a TLS endpoint is unreachable")
	}
}

// A Config with no transport is still valid, and is the plaintext case every
// existing caller is.
func TestNoTransportIsStillAClient(t *testing.T) {
	client, err := New(Config{Endpoint: "http://control-plane:5000"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client == nil {
		t.Error("a client with no transport was not built")
	}
}
