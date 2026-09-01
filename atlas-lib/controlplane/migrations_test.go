package controlplane

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const (
	testMigration = "88888888-8888-8888-8888-888888888888"
	testNQN       = "nqn.2023-02.io.simplyblock:c:lvol:v"
)

// singleMigrationJSON and batchMigrationJSON are the two shapes the endpoint
// answers with, undiscriminated. member_count is the only field that tells them
// apart.
func singleMigrationJSON(target string) string {
	return `{"id":"` + testMigration + `","lvol_id":"` + testVolume + `","source_node_id":"s",` +
		`"target_node_id":"` + target + `","phase":"pre_created","status":"running","error_message":"",` +
		`"retry_count":0,"max_retries":3,"snaps_migrated":1,"snaps_total":2,"completed_at":0,` +
		`"started_at":0,"intermediate_snap_rounds":0,"max_intermediate_snap_rounds":0}`
}

func batchMigrationJSON(target string) string {
	return `{"id":"` + testMigration + `","cluster_id":"` + testCluster + `","source_node_id":"s",` +
		`"target_node_id":"` + target + `","target_nqn":"` + testNQN + `","phase":"pre_created",` +
		`"status":"running","error_message":"","member_count":4}`
}

func TestClientListSubsystemMigrations(t *testing.T) {
	var path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[` + singleMigrationJSON("b") + `,` + batchMigrationJSON("b") + `]`))
	})
	ms, err := c.ListSubsystemMigrations(context.Background(), testCluster, testNQN)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "/subsystems/") {
		t.Errorf("path = %q, want the subsystem-scoped route", path)
	}
	if len(ms) != 2 {
		t.Fatalf("migrations = %+v", ms)
	}

	// A heterogeneous list must come back as both kinds, each with its own
	// fields populated and the other kind's left alone.
	if ms[0].Kind != MigrationKindSingle || ms[0].LvolID != testVolume || ms[0].SnapsTotal != 2 {
		t.Errorf("single = %+v", ms[0])
	}
	if ms[0].MemberCount != 0 || ms[0].TargetNQN != "" {
		t.Errorf("single carries batch fields: %+v", ms[0])
	}
	if ms[1].Kind != MigrationKindBatch || ms[1].MemberCount != 4 || ms[1].TargetNQN != testNQN {
		t.Errorf("batch = %+v", ms[1])
	}
	if ms[1].LvolID != "" || ms[1].MaxRetries != 0 {
		t.Errorf("batch carries single fields: %+v", ms[1])
	}
}

// A batch decoded as a single migration would succeed structurally and yield an
// object whose every distinguishing field is zero, so the discrimination is
// what the getter is really being tested for.
func TestClientGetSubsystemMigrationDiscriminatesBatch(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(batchMigrationJSON("b")))
	})
	m, err := c.GetSubsystemMigration(context.Background(), testCluster, testNQN, testMigration)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != MigrationKindBatch || m.MemberCount != 4 {
		t.Errorf("migration = %+v", m)
	}
}

func TestClientGetSubsystemMigrationSingle(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(singleMigrationJSON("b")))
	})
	m, err := c.GetSubsystemMigration(context.Background(), testCluster, testNQN, testMigration)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != MigrationKindSingle || m.SnapsTotal != 2 || m.LvolID != testVolume {
		t.Errorf("migration = %+v", m)
	}
}

func TestClientSubsystemMigrationActions(t *testing.T) {
	cases := []struct {
		name       string
		call       func(*Client) error
		wantMethod string
		wantPath   string
		status     int
	}{
		{
			name: "continue POSTs",
			call: func(c *Client) error {
				return c.ContinueSubsystemMigration(context.Background(), testCluster, testNQN, testMigration)
			},
			wantMethod: http.MethodPost,
			wantPath:   "/continue",
			status:     http.StatusOK,
		},
		{
			name: "cancel DELETEs",
			call: func(c *Client) error {
				return c.CancelSubsystemMigration(context.Background(), testCluster, testNQN, testMigration)
			},
			wantMethod: http.MethodDelete,
			status:     http.StatusNoContent,
		},
		{
			name: "cleanup-target POSTs",
			call: func(c *Client) error {
				return c.CleanupSubsystemMigrationTarget(context.Background(), testCluster, testNQN, testMigration)
			},
			wantMethod: http.MethodPost,
			wantPath:   "/cleanup-target",
			status:     http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var method, path string
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.Path
				w.WriteHeader(tc.status)
			})
			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			if method != tc.wantMethod {
				t.Errorf("method = %s, want %s", method, tc.wantMethod)
			}
			if tc.wantPath != "" && !strings.HasSuffix(path, tc.wantPath) {
				t.Errorf("path = %q, want it to end in %q", path, tc.wantPath)
			}
		})
	}
}

func TestClientCreateSubsystemMigration(t *testing.T) {
	const target = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(singleMigrationJSON(target)))
	})
	m, err := c.CreateSubsystemMigration(context.Background(), testCluster, testNQN, target)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != testMigration || m.TargetNodeID != target || m.Phase != "pre_created" {
		t.Errorf("migration = %+v", m)
	}
}

// The control plane, not the caller, decides that a subsystem carrying several
// namespaces migrates as a batch, so creating one can answer with either shape.
func TestClientCreateSubsystemMigrationReturnsBatch(t *testing.T) {
	const target = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(batchMigrationJSON(target)))
	})
	m, err := c.CreateSubsystemMigration(context.Background(), testCluster, testNQN, target)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != MigrationKindBatch || m.MemberCount != 4 || m.TargetNQN != testNQN {
		t.Errorf("migration = %+v", m)
	}
}

// TestClientCreateSubsystemMigrationPreConnect covers the other side of the
// namespace asymmetry: a migration's pre-connect strings share the /connect
// model but have no namespace yet, and must not be rejected for it.
func TestClientCreateSubsystemMigrationPreConnect(t *testing.T) {
	const target = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + testMigration + `","lvol_id":"` + testVolume + `","source_node_id":"s",` +
			`"target_node_id":"` + target + `","phase":"pre_created","status":"new","error_message":"",` +
			`"retry_count":0,"max_retries":3,"snaps_migrated":0,"snaps_total":0,"completed_at":0,` +
			`"started_at":0,"intermediate_snap_rounds":0,"max_intermediate_snap_rounds":0,` +
			`"connect_strings":[{"transport":"tcp","ip":"10.10.10.1","port":9090,"nqn":"nqn.t",` +
			`"reconnect-delay":2,"ctrl-loss-tmo":60,"fast-io-fail-tmo":20,"nr-io-queues":6,` +
			`"keep-alive-tmo":5,"connect":"sudo nvme connect ...","ns-id":null}]}`))
	})

	m, err := c.CreateSubsystemMigration(context.Background(), testCluster, testNQN, target)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != testMigration || m.Phase != "pre_created" {
		t.Errorf("migration = %+v", m)
	}
}
