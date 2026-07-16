package webapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTriggerDataRealignment(t *testing.T) {
	const clusterUUID = "cluster-123"

	t.Run("success", func(t *testing.T) {
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := NewClient(srv.URL)
		if err := c.TriggerDataRealignment(context.Background(), clusterUUID); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotMethod != http.MethodPost {
			t.Errorf("method = %q, want POST", gotMethod)
		}
		if !strings.Contains(gotPath, clusterUUID) {
			t.Errorf("path %q does not contain cluster UUID %q", gotPath, clusterUUID)
		}
	})

	t.Run("error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"boom"}`))
		}))
		defer srv.Close()

		c := NewClient(srv.URL)
		err := c.TriggerDataRealignment(context.Background(), clusterUUID)
		if err == nil {
			t.Fatal("expected error on 500 response")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error %q does not mention status 500", err)
		}
	})
}
