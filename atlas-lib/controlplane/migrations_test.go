package controlplane

import (
	"context"
	"net/http"
	"testing"
)

const testMigration = "88888888-8888-8888-8888-888888888888"

func TestClientListVolumeMigrations(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"` + testMigration + `","lvol_id":"` + testVolume + `",` +
			`"source_node_id":"a","target_node_id":"b","phase":"pre_created","status":"running",` +
			`"error_message":"","retry_count":0,"max_retries":3,"snaps_migrated":1,"snaps_total":2,` +
			`"completed_at":0,"started_at":0,"intermediate_snap_rounds":0,"max_intermediate_snap_rounds":0}]`))
	})
	ms, err := c.ListVolumeMigrations(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].ID != testMigration || ms[0].Phase != "pre_created" ||
		ms[0].TargetNodeID != "b" || ms[0].SnapsTotal != 2 {
		t.Errorf("migrations = %+v", ms)
	}
}

func TestClientContinueAndCancelVolumeMigration(t *testing.T) {
	t.Run("continue POSTs", func(t *testing.T) {
		var method string
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			w.WriteHeader(http.StatusOK)
		})
		if err := c.ContinueVolumeMigration(context.Background(), testHandle, testMigration); err != nil {
			t.Fatal(err)
		}
		if method != http.MethodPost {
			t.Errorf("method = %s, want POST", method)
		}
	})
	t.Run("cancel DELETEs", func(t *testing.T) {
		var method string
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			w.WriteHeader(http.StatusNoContent)
		})
		if err := c.CancelVolumeMigration(context.Background(), testHandle, testMigration); err != nil {
			t.Fatal(err)
		}
		if method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", method)
		}
	})
}

func TestClientCreateVolumeMigration(t *testing.T) {
	const target = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"` + testMigration + `","lvol_id":"` + testVolume + `","source_node_id":"s",` +
			`"target_node_id":"` + target + `","phase":"pre_created","status":"running","error_message":"",` +
			`"retry_count":0,"max_retries":3,"snaps_migrated":0,"snaps_total":0,"completed_at":0,` +
			`"started_at":0,"intermediate_snap_rounds":0,"max_intermediate_snap_rounds":0}`))
	})
	m, err := c.CreateVolumeMigration(context.Background(), testHandle, target)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != testMigration || m.TargetNodeID != target || m.Phase != "pre_created" {
		t.Errorf("migration = %+v", m)
	}
}
