package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

const (
	testMigration = "88888888-8888-8888-8888-888888888888"
	testNQN       = "nqn.2023-02.io.simplyblock:" + testCluster + ":lvol:" + testVolume
)

// singleMigration is what the control plane returns for a subsystem holding one
// namespace: a migration of that volume, with its snapshot progress.
const singleMigration = `{"id":"` + testMigration + `","lvol_id":"` + testVolume + `",` +
	`"source_node_id":"a","target_node_id":"b","phase":"pre_created","status":"running",` +
	`"error_message":"","retry_count":0,"max_retries":3,"snaps_migrated":1,"snaps_total":2,` +
	`"completed_at":0,"started_at":0,"intermediate_snap_rounds":0,"max_intermediate_snap_rounds":0}`

// batchMigration is what it returns for a subsystem configured for several
// namespaces, which migrates as one group: no lvol of its own, a member count,
// and the subsystem the members land on.
const batchMigration = `{"id":"` + testMigration + `","cluster_id":"` + testCluster + `",` +
	`"source_node_id":"a","target_node_id":"b","phase":"migrating","status":"running",` +
	`"error_message":"","member_count":6,"target_nqn":"nqn.2023-02.io.simplyblock:target"}`

// TestClientListMigrationsAddressesTheSubsystem holds the move this port is
// about. Migrations are addressed by the subsystem they belong to rather than
// by one volume inside it, which is what a namespaced pool made necessary: one
// subsystem exports several volumes, so a volume is not the thing being
// migrated.
func TestClientListMigrationsAddressesTheSubsystem(t *testing.T) {
	var path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[` + singleMigration + `]`))
	})

	if _, err := c.ListMigrations(context.Background(), testCluster, testNQN); err != nil {
		t.Fatal(err)
	}

	want := "/api/v2/clusters/" + testCluster + "/subsystems/" + testNQN + "/migrations/"
	if path != want {
		t.Errorf("asked for\n  %s\nwant\n  %s", path, want)
	}
}

// TestClientListMigrationsReadsBothKinds. One subsystem can have both a single
// volume's migration and a group in flight, and the control plane returns them
// in one list, so a client that assumed one shape would decode the other into a
// migration with every interesting field zeroed.
func TestClientListMigrationsReadsBothKinds(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[` + singleMigration + `,` + batchMigration + `]`))
	})

	ms, err := c.ListMigrations(context.Background(), testCluster, testNQN)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("got %d migrations, want 2: %+v", len(ms), ms)
	}

	if ms[0].Kind != MigrationOfVolume {
		t.Errorf("first is %q, want a volume's migration", ms[0].Kind)
	}
	if ms[0].LvolID != testVolume || ms[0].SnapsTotal != 2 || ms[0].MaxRetries != 3 {
		t.Errorf("volume migration = %+v", ms[0])
	}

	if ms[1].Kind != MigrationOfSubsystem {
		t.Errorf("second is %q, want a subsystem's migration", ms[1].Kind)
	}
	if ms[1].MemberCount != 6 || !strings.HasSuffix(ms[1].TargetNQN, ":target") {
		t.Errorf("batch migration = %+v", ms[1])
	}
	if ms[1].LvolID != "" {
		t.Errorf("a group migration names a volume it does not have: %q", ms[1].LvolID)
	}
}

// TestClientGetMigrationDiscriminatesTheKind. One id, either shape, and the
// caller cannot know which before asking: the phase and the status mean
// different things for a group than for one volume.
func TestClientGetMigrationDiscriminatesTheKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want MigrationKind
	}{
		{"one volume", singleMigration, MigrationOfVolume},
		{"a whole subsystem", batchMigration, MigrationOfSubsystem},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			})

			m, err := c.GetMigration(context.Background(), testCluster, testNQN, testMigration)
			if err != nil {
				t.Fatal(err)
			}
			if m.Kind != tc.want {
				t.Errorf("kind = %q, want %q", m.Kind, tc.want)
			}
			if m.ID != testMigration {
				t.Errorf("id = %q", m.ID)
			}
		})
	}
}

// TestClientCreateMigrationTakesWhicheverKindWasMade. The caller asks for a
// target node and nothing else: whether that means one volume or the whole
// subsystem is the control plane's decision, taken from the subsystem's own
// namespace capacity. A client that named the kind in its request would be
// describing a choice it does not make.
func TestClientCreateMigrationTakesWhicheverKindWasMade(t *testing.T) {
	const target = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	var method, body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(batchMigration))
	})

	m, err := c.CreateMigration(context.Background(), testCluster, testNQN, target)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if !strings.Contains(body, target) {
		t.Errorf("request body %q does not name the target node", body)
	}
	if m.Kind != MigrationOfSubsystem || m.MemberCount != 6 {
		t.Errorf("created migration = %+v, want the group the control plane made", m)
	}
}

// TestClientContinueAndCancelMigration are kind-agnostic: both apply to a group
// and to one volume, so they carry the id and nothing about the shape.
func TestClientContinueAndCancelMigration(t *testing.T) {
	t.Run("continue POSTs to the migration", func(t *testing.T) {
		var method, path string
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			method, path = r.Method, r.URL.Path
			w.WriteHeader(http.StatusOK)
		})
		if err := c.ContinueMigration(context.Background(), testCluster, testNQN, testMigration); err != nil {
			t.Fatal(err)
		}
		if method != http.MethodPost {
			t.Errorf("method = %s, want POST", method)
		}
		if !strings.HasSuffix(path, "/migrations/"+testMigration+"/continue") {
			t.Errorf("path = %s", path)
		}
	})
	t.Run("cancel DELETEs it", func(t *testing.T) {
		var method, path string
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			method, path = r.Method, r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		})
		if err := c.CancelMigration(context.Background(), testCluster, testNQN, testMigration); err != nil {
			t.Fatal(err)
		}
		if method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", method)
		}
		if !strings.HasSuffix(path, "/migrations/"+testMigration+"/") {
			t.Errorf("path = %s", path)
		}
	})
}

// TestClientRefusesAMigrationOfNoKnownKind. Both shapes are recognized by a
// field the other does not have, so a body carrying neither is a control plane
// this version does not understand. Reporting that beats handing back a
// migration whose every field is the zero value and whose kind is a guess.
func TestClientRefusesAMigrationOfNoKnownKind(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + testMigration + `","phase":"running"}`))
	})

	_, err := c.GetMigration(context.Background(), testCluster, testNQN, testMigration)
	if err == nil {
		t.Fatal("accepted a migration that is neither a volume's nor a subsystem's")
	}
	if !strings.Contains(err.Error(), testMigration) {
		t.Errorf("the error does not say which migration it was: %v", err)
	}
	// A body that deserialized and carries neither key is the version skew this
	// sentinel names, and a caller classifies it the way it classifies every
	// other unreadable response rather than by matching on the text.
	if !errors.Is(err, errs.ErrInvalidResponse) {
		t.Errorf("err = %v, want it to wrap errs.ErrInvalidResponse", err)
	}
}

// TestMigrationJSONShapesAreDistinguishable is the assumption the two above
// rest on, asserted directly: the discriminating fields are present in one
// shape and absent from the other, so neither decodes as the other by accident.
func TestMigrationJSONShapesAreDistinguishable(t *testing.T) {
	var single, batch map[string]any
	if err := json.Unmarshal([]byte(singleMigration), &single); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(batchMigration), &batch); err != nil {
		t.Fatal(err)
	}
	if _, ok := single["lvol_id"]; !ok {
		t.Error("a volume's migration no longer carries lvol_id")
	}
	if _, ok := batch["lvol_id"]; ok {
		t.Error("a group migration carries lvol_id, so the two cannot be told apart")
	}
	if _, ok := batch["member_count"]; !ok {
		t.Error("a group migration no longer carries member_count")
	}
	if _, ok := single["member_count"]; ok {
		t.Error("a volume's migration carries member_count, so the two cannot be told apart")
	}
}
