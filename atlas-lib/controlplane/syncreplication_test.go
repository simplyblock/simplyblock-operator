// Unit tests for the hand-rolled sync-replication client: the status-code
// protocol (200/204 success, 409 retryable, 412 escalate, 400 bad site) and the
// site/planned query parameters. An httptest backend, no control plane.
package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simplyblock/atlas/lvol"
)

const testSyncHandle = "11111111-1111-1111-1111-111111111111:" +
	"22222222-2222-2222-2222-222222222222:" +
	"33333333-3333-3333-3333-333333333333"

func syncClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{Endpoint: srv.URL, Token: "secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func TestSyncDemote(t *testing.T) {
	t.Run("204 is success and passes the site", func(t *testing.T) {
		var gotSite, gotAuth string
		c, _ := syncClient(t, func(w http.ResponseWriter, r *http.Request) {
			gotSite = r.URL.Query().Get("site")
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusNoContent)
		})
		if err := c.SyncDemote(context.Background(), lvol.VolumeHandle(testSyncHandle), "site-a"); err != nil {
			t.Fatalf("SyncDemote: %v", err)
		}
		if gotSite != "site-a" {
			t.Errorf("site = %q, want site-a", gotSite)
		}
		if gotAuth != "Bearer secret" {
			t.Errorf("auth = %q, want Bearer secret", gotAuth)
		}
	})

	t.Run("409 gate refusal carries the status", func(t *testing.T) {
		c, _ := syncClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":{"message":"a store is not in sync"}}`))
		})
		err := c.SyncDemote(context.Background(), lvol.VolumeHandle(testSyncHandle), "site-a")
		var se *SyncStatusError
		if !errors.As(err, &se) || se.Status != http.StatusConflict {
			t.Fatalf("err = %v, want SyncStatusError{409}", err)
		}
		if se.Message != "a store is not in sync" {
			t.Errorf("message = %q, want the backend detail", se.Message)
		}
	})
}

func TestSyncPromote(t *testing.T) {
	t.Run("200 returns the connection entries and inverts force to planned", func(t *testing.T) {
		var gotSite, gotPlanned string
		c, _ := syncClient(t, func(w http.ResponseWriter, r *http.Request) {
			gotSite = r.URL.Query().Get("site")
			gotPlanned = r.URL.Query().Get("planned")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"lvol_id":"lv1","connection_strings":["nvme://a","nvme://b"]}`))
		})
		res, err := c.SyncPromote(context.Background(), lvol.VolumeHandle(testSyncHandle), "site-b", true)
		if err != nil {
			t.Fatalf("SyncPromote: %v", err)
		}
		if gotSite != "site-b" || gotPlanned != "true" {
			t.Errorf("query site=%q planned=%q, want site-b/true", gotSite, gotPlanned)
		}
		if res.LvolID != "lv1" || len(res.ConnectionStrings) != 2 {
			t.Errorf("result = %+v, want lvol_id lv1 and two connection strings", res)
		}
	})

	t.Run("412 is the escalation trigger", func(t *testing.T) {
		c, _ := syncClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusPreconditionFailed)
		})
		_, err := c.SyncPromote(context.Background(), lvol.VolumeHandle(testSyncHandle), "site-b", true)
		var se *SyncStatusError
		if !errors.As(err, &se) || se.Status != http.StatusPreconditionFailed {
			t.Fatalf("err = %v, want SyncStatusError{412}", err)
		}
	})

	t.Run("409 in progress is retryable, not escalation", func(t *testing.T) {
		c, _ := syncClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":{"message":"in progress","task_id":"t1"}}`))
		})
		_, err := c.SyncPromote(context.Background(), lvol.VolumeHandle(testSyncHandle), "site-b", true)
		var se *SyncStatusError
		if !errors.As(err, &se) || se.Status != http.StatusConflict {
			t.Fatalf("err = %v, want SyncStatusError{409}", err)
		}
	})
}
