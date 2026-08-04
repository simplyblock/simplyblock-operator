package webapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// testNQN carries the colons a real NQN has, which must survive path escaping.
const testNQN = "nqn.2014-08.io.simplyblock:cluster:lvol:vol-1"

func TestMigrationURLs(t *testing.T) {
	const wantCollection = "/api/v2/clusters/cluster-1/subsystems/" + testNQN + "/migrations"

	if got := migrationsURL("cluster-1", testNQN); got != wantCollection {
		t.Errorf("migrationsURL = %q, want %q", got, wantCollection)
	}
	if got, want := migrationURL("cluster-1", testNQN, "mig-1"), wantCollection+"/mig-1"; got != want {
		t.Errorf("migrationURL = %q, want %q", got, want)
	}
	// A slash in the NQN must not open a new path segment.
	if got, want := migrationsURL("cluster-1", "nqn.x/y"), "/api/v2/clusters/cluster-1/subsystems/nqn.x%2Fy/migrations"; got != want {
		t.Errorf("migrationsURL with slash = %q, want %q", got, want)
	}
}

// A create rejected because a migration already exists must cancel the in-flight
// one — the finished ones in the list are not what blocks the request — and
// return (nil, nil) so the caller retries the create on its next pass.
func TestCreateMigrationCancelsInFlightMigrationOnConflict(t *testing.T) {
	collection := migrationsURL("cluster-1", testNQN)
	var cancelled []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == collection:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"a migration already exists. Cancel it first."}`))
		case r.Method == http.MethodGet && r.URL.Path == collection:
			_, _ = w.Write([]byte(`[{"id":"mig-new","status":"running"},{"id":"mig-old","status":"done"}]`))
		case r.Method == http.MethodDelete:
			cancelled = append(cancelled, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	m, err := NewClient(srv.URL).CreateMigration(context.Background(), "cluster-1", testNQN, "target-node")
	if err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}
	if m != nil {
		t.Errorf("migration = %+v, want nil so the caller retries", m)
	}
	want := collection + "/mig-new"
	if len(cancelled) != 1 || cancelled[0] != want {
		t.Errorf("cancelled = %v, want [%s]", cancelled, want)
	}
}

func TestIsExistingMigrationConflict(t *testing.T) {
	// The exact body observed in the field: the API rejects CreateMigration with
	// 400 (not 409) when a migration already exists for the volume.
	existing400 := []byte(`{"detail":"An active migration for f1757a88-7688-4750-af9e-3e60492ef692 already exists targeting a different node (fe5bfec8-8968-4342-8f48-579e6cf9043a). Cancel it first."}`)

	cases := []struct {
		name   string
		status int
		body   []byte
		want   bool
	}{
		{"409 conflict", http.StatusConflict, []byte(`{"detail":"conflict"}`), true},
		{"400 already-exists (field case)", http.StatusBadRequest, existing400, true},
		{"400 already-exists uppercase", http.StatusBadRequest, []byte(`{"detail":"An active MIGRATION already EXISTS"}`), true},
		{"400 volume already on node", http.StatusBadRequest, []byte(`{"detail":"volume already on node"}`), false},
		{"400 generic bad request", http.StatusBadRequest, []byte(`{"detail":"invalid target node"}`), false},
		{"400 empty body", http.StatusBadRequest, nil, false},
		{"200 ok", http.StatusOK, []byte(`{}`), false},
		{"500 server error", http.StatusInternalServerError, []byte(`already exists migration`), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExistingMigrationConflict(tc.status, tc.body); got != tc.want {
				t.Fatalf("isExistingMigrationConflict(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}
