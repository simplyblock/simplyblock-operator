package switchover

// The operator's backend for the switchover server reaches the control plane the
// ControlPlane object names, with the credential it names, read on every call so
// a changed endpoint or a rotated token reaches the next move.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simplyblock/atlas/lvol"
)

const handle = lvol.VolumeHandle("11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222:33333333-3333-3333-3333-333333333333")

func TestTheBackendUsesTheControlPlanesEndpointAndCredential(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"site":"site-a","role":"primary","state":"in_sync","peer_ready":true}`))
	}))
	defer srv.Close()

	b := &Backend{
		Endpoint:   func(context.Context) string { return srv.URL },
		Credential: func(context.Context) (string, bool) { return "admin-token", true },
	}
	st, err := b.SyncStatus(context.Background(), handle)
	if err != nil {
		t.Fatalf("SyncStatus: %v", err)
	}
	if st.Site != "site-a" {
		t.Errorf("site = %q, want site-a", st.Site)
	}
	if auth != "Bearer admin-token" {
		t.Errorf("Authorization = %q, want the ControlPlane's credential", auth)
	}
}

func TestTheBackendRefusesWithoutAnEndpoint(t *testing.T) {
	b := &Backend{
		Endpoint:   func(context.Context) string { return "" },
		Credential: func(context.Context) (string, bool) { return "", false },
	}
	if _, err := b.SyncStatus(context.Background(), handle); err == nil {
		t.Error("a backend with no control-plane endpoint answered")
	}
}
