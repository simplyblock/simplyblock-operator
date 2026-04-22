package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
)

func TestDoAgainstSpecMockSendsHeadersBodyAndReturnsResponse(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()

	mock.Register(
		http.MethodPost,
		"/api/v2/clusters/cluster-uuid/storage-pools/",
		webapimock.RouteResponse{
			Status: http.StatusCreated,
			Body:   `{"ok":true,"id":"pool-1"}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)

	c := NewClient(mock.URL())

	respBody, status, err := c.Do(
		context.Background(),
		"top-secret",
		http.MethodPost,
		"/api/v2/clusters/cluster-uuid/storage-pools/",
		map[string]any{"name": "pool-a"},
	)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, status)
	}
	if string(respBody) != `{"ok":true,"id":"pool-1"}` {
		t.Fatalf("unexpected response body: %s", string(respBody))
	}

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected one request, got %d", len(reqs))
	}
	req := reqs[0]
	if req.Method != http.MethodPost {
		t.Fatalf("unexpected method: %s", req.Method)
	}
	if req.Path != "/api/v2/clusters/cluster-uuid/storage-pools" {
		t.Fatalf("unexpected path: %s", req.Path)
	}
	if req.Headers["Authorization"] != "Bearer top-secret" {
		t.Fatalf("expected authorization header to be set")
	}
	if !strings.HasPrefix(req.Headers["Content-Type"], "application/json") {
		t.Fatalf("expected content-type application/json, got %q", req.Headers["Content-Type"])
	}

	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("request body is not valid json: %v", err)
	}
	if body["name"] != "pool-a" {
		t.Fatalf("unexpected request body: %#v", body)
	}
}

func TestDoAgainstStrictSpecMockReturns400ForUnknownPath(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()

	c := NewClient(mock.URL())
	body, status, err := c.Do(
		context.Background(),
		"secret",
		http.MethodGet,
		"/api/v2/clusters/cluster-uuid/nonexistent-endpoint/",
		nil,
	)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("expected status 400 for strict spec mismatch, got %d", status)
	}
	if !strings.Contains(strings.ToLower(string(body)), "not defined in openapi spec") {
		t.Fatalf("unexpected response body for strict mismatch: %s", string(body))
	}
}

func TestDoReturnsMarshalErrorForUnsupportedBody(t *testing.T) {
	ts := httptest.NewServer(nil)
	ts.Close()
	c := NewClient(ts.URL)
	_, _, err := c.Do(
		context.Background(),
		"secret",
		http.MethodPost,
		"/api/v2/clusters/cluster-uuid/storage-pools/",
		map[string]any{"bad": make(chan int)},
	)
	if err == nil {
		t.Fatalf("expected marshal error")
	}
	if !strings.Contains(err.Error(), "marshal body") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDoReturnsHTTPErrorForUnreachableServer(t *testing.T) {
	ts := httptest.NewServer(nil)
	ts.Close()
	c := NewClient(ts.URL)
	_, status, err := c.Do(
		context.Background(),
		"secret",
		http.MethodGet,
		"/api/v2/clusters/cluster-uuid/storage-pools/",
		nil,
	)
	if err == nil {
		t.Fatalf("expected HTTP transport error")
	}
	if status != 0 {
		t.Fatalf("expected status 0 on transport error, got %d", status)
	}
}
