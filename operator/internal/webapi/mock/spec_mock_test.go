package mock

import (
	"net/http"
	"testing"
)

func TestSpecServerEnforcesSpecWhenStrict(t *testing.T) {
	s := NewSpecServerFromFile(t, "../../../openapi.json", false)
	defer s.Close()

	s.Register(http.MethodPost, "/api/v2/clusters/c1/storage-pools/", RouteResponse{Status: http.StatusOK, Body: `{}`})

	resp, err := http.Post(s.URL()+"/api/v2/clusters/c1/storage-pools/", "application/json", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestSpecServerRejectsUnknownPathWhenStrict(t *testing.T) {
	s := NewSpecServerFromFile(t, "../../../openapi.json", false)
	defer s.Close()

	resp, err := http.Get(s.URL() + "/api/v2/clusters/c1/nonexistent-endpoint/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected status 400 for unknown strict path, got %d", resp.StatusCode)
	}
}
