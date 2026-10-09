// Tests for the snapshot and backup-request calls.

package controlplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

func TestClientCreateSnapshot(t *testing.T) {
	var body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Location", "/snapshots/"+testSnapshot+"/")
		w.WriteHeader(http.StatusCreated)
	})

	id, err := c.CreateSnapshot(context.Background(), testCluster, testPool, testVolume, "sbk-1")
	if err != nil || id != testSnapshot {
		t.Fatalf("id = %q, err = %v, want %q from the Location header", id, err, testSnapshot)
	}
	if !strings.Contains(body, `"name":"sbk-1"`) || strings.Contains(body, `"backup":true`) {
		t.Errorf("body = %s, want a name and no inline backup", body)
	}
}

func TestClientCreateSnapshotReportsANameInUse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) })

	_, err := c.CreateSnapshot(context.Background(), testCluster, testPool, testVolume, "sbk-1")
	if !errors.Is(err, errs.ErrAlreadyExists) {
		t.Errorf("err = %v, want errs.ErrAlreadyExists", err)
	}
}

func TestClientVolumeSnapshotsAndCreateBackup(t *testing.T) {
	var backupBody string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			backupBody = string(b)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"` + testSnapshot + `","name":"sbk-1","status":"online","group_id":"","group_seq":0,` +
			`"created_at":"2026-10-09T10:00:00Z","health_check":true,"migrating":false,"size":1,"used_size":1,"lvol":null}]`))
	})

	got, err := c.VolumeSnapshots(context.Background(), testCluster, testPool, testVolume)
	if err != nil || len(got) != 1 || got[0].ID != testSnapshot || got[0].Name != "sbk-1" {
		t.Fatalf("snapshots = %+v, err = %v", got, err)
	}
	if err := c.CreateBackup(context.Background(), testCluster, testSnapshot); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(backupBody, `"snapshot_id":"`+testSnapshot+`"`) {
		t.Errorf("backup request = %s, want it to name the snapshot", backupBody)
	}
}
