package webapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// testNQN carries the colons a real NQN has, which must survive path escaping.
const testNQN = "nqn.2014-08.io.simplyblock:cluster:lvol:vol-1"

// The paths must match what the control plane declares, trailing slash included —
// the slashless form only works via a 307 redirect.
func TestMigrationURLs(t *testing.T) {
	const wantCollection = "/api/v2/clusters/cluster-1/subsystems/" + testNQN + "/migrations/"

	if got := migrationsURL("cluster-1", testNQN); got != wantCollection {
		t.Errorf("migrationsURL = %q, want %q", got, wantCollection)
	}
	if got, want := migrationURL("cluster-1", testNQN, "mig-1"), wantCollection+"mig-1/"; got != want {
		t.Errorf("migrationURL = %q, want %q", got, want)
	}
	// A slash in the NQN must not open a new path segment.
	if got, want := migrationsURL("cluster-1", "nqn.x/y"), "/api/v2/clusters/cluster-1/subsystems/nqn.x%2Fy/migrations/"; got != want {
		t.Errorf("migrationsURL with slash = %q, want %q", got, want)
	}
}

// A migration of a single-namespace subsystem comes back as the control plane's
// solo DTO: no target_nqn, no member_count, plus counters this client ignores. It
// still moved one volume in the subsystem that was asked for, and must be reported
// that way rather than as zero volumes in no subsystem.
func TestGetMigrationNormalizesSoloResponse(t *testing.T) {
	solo := `{"id":"mig-1","lvol_id":"lv-1","source_node_id":"A","target_node_id":"B",
	          "phase":"pre_created","status":"new","snaps_total":4,"snaps_migrated":1,
	          "retry_count":0,"max_retries":10,"error_message":"","started_at":0,"completed_at":0}`
	batch := `{"id":"mig-2","cluster_id":"c","source_node_id":"A","target_node_id":"B",
	           "target_nqn":"` + testNQN + `","phase":"pre_created","status":"new",
	           "member_count":3,"error_message":""}`

	for _, tc := range []struct {
		name        string
		body        string
		wantMembers int
	}{
		{"single-namespace subsystem", solo, 1},
		{"shared subsystem", batch, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			m, err := NewClient(srv.URL).GetMigration(context.Background(), "c", testNQN, "mig-1")
			if err != nil {
				t.Fatalf("GetMigration: %v", err)
			}
			if m.MemberCount != tc.wantMembers {
				t.Errorf("MemberCount = %d, want %d", m.MemberCount, tc.wantMembers)
			}
			if m.TargetNQN != testNQN {
				t.Errorf("TargetNQN = %q, want %q", m.TargetNQN, testNQN)
			}
			if m.Phase != MigrationPhasePreCreated || m.Status != MigrationStatusNew {
				t.Errorf("phase/status = %q/%q, want pre_created/new", m.Phase, m.Status)
			}
		})
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
	want := collection + "mig-new/"
	if len(cancelled) != 1 || cancelled[0] != want {
		t.Errorf("cancelled = %v, want [%s]", cancelled, want)
	}
}

// A cluster busy rebalancing (often because a previous migration triggered the
// realignment) refuses new migrations with a 400 that clears by itself. It must be
// reported as ErrMigrationNotAcceptingYet so callers retry, while genuinely bad
// requests keep failing fast.
func TestCreateMigrationReportsDeferralSeparately(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantDeferId bool
	}{
		{"cluster rebalancing", `{"detail":"Cluster c1 is rebalancing; wait for it to finish before migrating"}`, true},
		{"node busy with a data migration", `{"detail":"Node n1 has a data migration in progress; wait for it to finish"}`, true},
		{"volume already on the target node", `{"detail":"LVol v1 is already on node n1; cannot migrate to the same node"}`, false},
		{"unknown target node", `{"detail":"Target node n9 not found"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := NewClient(srv.URL).CreateMigration(context.Background(), "c1", testNQN, "n1")
			if err == nil {
				t.Fatalf("expected an error for %s", tc.body)
			}
			if got := errors.Is(err, ErrMigrationNotAcceptingYet); got != tc.wantDeferId {
				t.Errorf("errors.Is(err, ErrMigrationNotAcceptingYet) = %v, want %v (err: %v)",
					got, tc.wantDeferId, err)
			}
			// The control plane's own wording must survive into the error either way.
			if !strings.Contains(err.Error(), "detail") {
				t.Errorf("error %q does not carry the API detail", err)
			}
		})
	}
}

// Subsystem membership decides which nodes get their NVMe paths switched before
// cutover, so both halves matter: every volume sharing the NQN must be found, and
// nothing else may be.
func TestGetSubsystemVolumes(t *testing.T) {
	const otherNQN = "nqn.2014-08.io.simplyblock:cluster:lvol:vol-9"

	t.Run("collects members across pools and filters by NQN", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/storage-pools/"):
				_, _ = w.Write([]byte(`[{"id":"pool-a"},{"id":"pool-b"}]`))
			case strings.Contains(r.URL.Path, "/storage-pools/pool-a/volumes/"):
				_, _ = w.Write([]byte(`[
					{"id":"vol-1","nqn":"` + testNQN + `"},
					{"id":"vol-9","nqn":"` + otherNQN + `"}]`))
			case strings.Contains(r.URL.Path, "/storage-pools/pool-b/volumes/"):
				// A subsystem is scoped to a storage node, not a pool, so a member can
				// sit in another pool than the one the migration was addressed from.
				_, _ = w.Write([]byte(`[{"id":"vol-2","nqn":"` + testNQN + `"}]`))
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()

		vols, err := NewClient(srv.URL).GetSubsystemVolumes(context.Background(), "c1", testNQN)
		if err != nil {
			t.Fatalf("GetSubsystemVolumes: %v", err)
		}
		got := make([]string, 0, len(vols))
		for _, v := range vols {
			got = append(got, v.UUID)
		}
		slices.Sort(got)
		if !slices.Equal(got, []string{"vol-1", "vol-2"}) {
			t.Errorf("members = %v, want [vol-1 vol-2] (vol-9 belongs to another subsystem)", got)
		}
	})

	t.Run("single-namespace subsystem has exactly one member", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/storage-pools/") {
				_, _ = w.Write([]byte(`[{"id":"pool-a"}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":"vol-1","nqn":"` + testNQN + `"},
				{"id":"vol-9","nqn":"` + otherNQN + `"}]`))
		}))
		defer srv.Close()

		vols, err := NewClient(srv.URL).GetSubsystemVolumes(context.Background(), "c1", testNQN)
		if err != nil {
			t.Fatalf("GetSubsystemVolumes: %v", err)
		}
		if len(vols) != 1 || vols[0].UUID != "vol-1" {
			t.Errorf("members = %+v, want just vol-1", vols)
		}
	})

	t.Run("no member matches", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/storage-pools/") {
				_, _ = w.Write([]byte(`[{"id":"pool-a"}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":"vol-9","nqn":"` + otherNQN + `"}]`))
		}))
		defer srv.Close()

		vols, err := NewClient(srv.URL).GetSubsystemVolumes(context.Background(), "c1", testNQN)
		if err != nil {
			t.Fatalf("GetSubsystemVolumes: %v", err)
		}
		if len(vols) != 0 {
			t.Errorf("members = %+v, want none", vols)
		}
	})

	// The failures must surface rather than yield a short member list: a partial list
	// means a node gets missed, which is exactly the outage this feeds.
	t.Run("pool listing fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		if _, err := NewClient(srv.URL).GetSubsystemVolumes(context.Background(), "c1", testNQN); err == nil {
			t.Errorf("expected the pool-listing failure to surface")
		}
	})

	t.Run("one pool's volume listing fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/storage-pools/"):
				_, _ = w.Write([]byte(`[{"id":"pool-a"},{"id":"pool-b"}]`))
			case strings.Contains(r.URL.Path, "/pool-a/"):
				_, _ = w.Write([]byte(`[{"id":"vol-1","nqn":"` + testNQN + `"}]`))
			default:
				w.WriteHeader(http.StatusInternalServerError)
			}
		}))
		defer srv.Close()

		_, err := NewClient(srv.URL).GetSubsystemVolumes(context.Background(), "c1", testNQN)
		if err == nil {
			t.Fatalf("expected a partial listing to be an error, not a truncated member set")
		}
		if !strings.Contains(err.Error(), "pool-b") {
			t.Errorf("error = %q, want it to name the pool that failed", err)
		}
	})

	t.Run("empty NQN is rejected before any call", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			t.Errorf("unexpected request %s %s for an empty NQN", r.Method, r.URL.Path)
		}))
		defer srv.Close()

		if _, err := NewClient(srv.URL).GetSubsystemVolumes(context.Background(), "c1", ""); err == nil {
			t.Errorf("expected an empty NQN to be rejected")
		}
	})
}

// The listing mixes shapes — batch groups and single-volume migrations of the same
// subsystem — so normalization has to apply to every entry, not just a single GET.
func TestGetMigrationsNormalizesEveryEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id":"mig-batch","target_nqn":"` + testNQN + `","member_count":3,"status":"running"},
			{"id":"mig-solo","lvol_id":"vol-1","status":"new"}]`))
	}))
	defer srv.Close()

	migs, err := NewClient(srv.URL).GetMigrations(context.Background(), "c1", testNQN)
	if err != nil {
		t.Fatalf("GetMigrations: %v", err)
	}
	if len(migs) != 2 {
		t.Fatalf("got %d migrations, want 2", len(migs))
	}
	for _, m := range migs {
		if m.TargetNQN != testNQN {
			t.Errorf("%s: TargetNQN = %q, want the addressed subsystem", m.ID, m.TargetNQN)
		}
		if m.MemberCount < 1 {
			t.Errorf("%s: MemberCount = %d, want at least 1", m.ID, m.MemberCount)
		}
	}
	if migs[0].MemberCount != 3 {
		t.Errorf("batch MemberCount = %d, want the reported 3", migs[0].MemberCount)
	}
	if migs[1].MemberCount != 1 {
		t.Errorf("solo MemberCount = %d, want 1", migs[1].MemberCount)
	}
}

// The conflict path cancels the migration that blocks a new one. With nothing
// in flight there is nothing to cancel, and reporting that is better than
// looping: the create failed for a reason we have not understood.
func TestCreateMigrationConflictWithNothingInFlight(t *testing.T) {
	collection := migrationsURL("cluster-1", testNQN)
	cases := []struct {
		name    string
		listing string
	}{
		{"empty listing", `[]`},
		{"only terminal migrations", `[{"id":"mig-old","status":"done"},{"id":"mig-x","status":"cancelled"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost:
					w.WriteHeader(http.StatusConflict)
					_, _ = w.Write([]byte(`{"detail":"a migration already exists. Cancel it first."}`))
				case r.Method == http.MethodGet && r.URL.Path == collection:
					_, _ = w.Write([]byte(tc.listing))
				case r.Method == http.MethodDelete:
					t.Errorf("nothing in flight, yet a cancel was attempted: %s", r.URL.Path)
				}
			}))
			defer srv.Close()

			m, err := NewClient(srv.URL).CreateMigration(context.Background(), "cluster-1", testNQN, "n1")
			if err == nil {
				t.Fatalf("expected an error, got migration %+v", m)
			}
			if !strings.Contains(err.Error(), "no in-flight migration") {
				t.Errorf("error = %q, want it to say nothing was in flight", err)
			}
		})
	}
}

// Continue and cancel are fire-and-forget calls whose only signal is the status code;
// a non-2xx must not be mistaken for success, and continue carries an empty body so
// the control plane applies its own defaults.
func TestContinueAndCancelMigration(t *testing.T) {
	t.Run("continue posts an empty parameter object", func(t *testing.T) {
		var body string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		if err := NewClient(srv.URL).ContinueMigration(context.Background(), "c1", testNQN, "mig-1"); err != nil {
			t.Fatalf("ContinueMigration: %v", err)
		}
		if body != "{}" {
			t.Errorf("continue body = %q, want {} so the control plane defaults apply", body)
		}
	})

	for _, tc := range []struct {
		name string
		call func(c *Client) error
	}{
		{"continue", func(c *Client) error {
			return c.ContinueMigration(context.Background(), "c1", testNQN, "mig-1")
		}},
		{"cancel", func(c *Client) error {
			return c.CancelMigration(context.Background(), "c1", testNQN, "mig-1")
		}},
	} {
		t.Run(tc.name+" reports a non-2xx", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"detail":"nope"}`, http.StatusBadRequest)
			}))
			defer srv.Close()

			err := tc.call(NewClient(srv.URL))
			if err == nil {
				t.Fatalf("expected an error for a 400")
			}
			if !strings.Contains(err.Error(), "mig-1") || !strings.Contains(err.Error(), "nope") {
				t.Errorf("error = %q, want the migration id and the API detail", err)
			}
		})
	}

	t.Run("cancel succeeds on 2xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"cancelled"}`))
		}))
		defer srv.Close()

		if err := NewClient(srv.URL).CancelMigration(context.Background(), "c1", testNQN, "mig-1"); err != nil {
			t.Errorf("CancelMigration: %v", err)
		}
	})
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
