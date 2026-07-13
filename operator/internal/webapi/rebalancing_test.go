package webapi

import (
	"net/http"
	"testing"
)

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
