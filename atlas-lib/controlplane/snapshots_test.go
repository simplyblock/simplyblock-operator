// Tests for the snapshot calls and the backup request: the two calls an
// on-demand backup of one volume is made of, and the volume record's
// consistency group, which is what stops that backup being taken of a member.

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

const testSnapshotPath = "/storage-pools/" + testPool + "/volumes/" + testVolume + "/snapshots"

func TestClientCreateSnapshotReadsTheIDFromTheLocation(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, testSnapshotPath) {
			t.Errorf("request = %s %s, want POST ...%s", r.Method, r.URL.Path, testSnapshotPath)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("the body is not JSON: %v", err)
		}
		if body["name"] != "pvops-backup-1" {
			t.Errorf("body name = %v, want pvops-backup-1", body["name"])
		}
		if backup, present := body["backup"]; present && backup != false {
			t.Errorf("the snapshot asked for a backup of its own (%v), and the backup is requested separately", backup)
		}
		w.Header().Set("Location", "/api/v2/clusters/"+testCluster+"/storage-pools/"+testPool+"/snapshots/"+testSnapshot)
		w.WriteHeader(http.StatusCreated)
	})

	id, err := c.CreateSnapshot(context.Background(), testCluster, testPool, testVolume, "pvops-backup-1")
	if err != nil {
		t.Fatal(err)
	}
	if id != testSnapshot {
		t.Errorf("snapshot id = %q, want %q", id, testSnapshot)
	}
}

// A snapshot name already in use is how a retried creation shows up, so it has
// to be a value the caller can recognize rather than a string to match.
func TestClientCreateSnapshotNameTakenIsAlreadyExists(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})

	if _, err := c.CreateSnapshot(context.Background(), testCluster, testPool, testVolume, "taken"); !errors.Is(err, errs.ErrAlreadyExists) {
		t.Errorf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestClientVolumeSnapshotsMapsTheGroupAndName(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, testSnapshotPath) {
			t.Errorf("request = %s %s, want GET ...%s", r.Method, r.URL.Path, testSnapshotPath)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"` + testSnapshot + `","name":"pvops-backup-1","status":"online",` +
			`"group_id":"","group_seq":0,"created_at":"2026-10-09T10:00:00Z","health_check":true,` +
			`"migrating":false,"size":1024,"used_size":512,"lvol":null}]`))
	})

	snapshots, err := c.VolumeSnapshots(context.Background(), testCluster, testPool, testVolume)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].ID != testSnapshot || snapshots[0].Name != "pvops-backup-1" {
		t.Errorf("snapshots = %+v", snapshots)
	}
}

func TestClientDeleteSnapshotTreatsGoneAsDone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.Contains(r.URL.Path, "/storage-pools/"+testPool+"/snapshots/"+testSnapshot) {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	})

	if err := c.DeleteSnapshot(context.Background(), testCluster, testPool, testSnapshot); err != nil {
		t.Errorf("err = %v, want nil: a snapshot that is gone is what a delete asks for", err)
	}
}

func TestClientCreateBackupReadsTheBackupIDHeader(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/clusters/"+testCluster+"/backups/") {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"snapshot_id":"`+testSnapshot+`"`) {
			t.Errorf("body = %s, want the snapshot id", raw)
		}
		w.Header().Set("X-Backup-Id", testBackup)
		w.WriteHeader(http.StatusCreated)
	})

	id, err := c.CreateBackup(context.Background(), testCluster, testSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if id != testBackup {
		t.Errorf("backup id = %q, want %q", id, testBackup)
	}
}

// The control plane answers 400 with the reason when it cannot start the
// backup, such as a cluster with no backup store. The reason has to reach the
// caller, since it is the whole of what a user can act on.
func TestClientCreateBackupCarriesTheRefusalReason(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"backup is not configured on this cluster"}`))
	})

	_, err := c.CreateBackup(context.Background(), testCluster, testSnapshot)
	if err == nil || !strings.Contains(err.Error(), "backup is not configured") {
		t.Errorf("err = %v, want the control plane's reason", err)
	}
}

func TestClientCreateBackupWithoutAnIDFails(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	if _, err := c.CreateBackup(context.Background(), testCluster, testSnapshot); !errors.Is(err, errs.ErrInvalidResponse) {
		t.Errorf("err = %v, want ErrInvalidResponse", err)
	}
}

func TestClientVolumeReportsItsConsistencyGroup(t *testing.T) {
	const group = "99999999-9999-9999-9999-999999999999"
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + testVolume + `","name":"vol1","pool_name":"pool1",` +
			`"size":20971520,"ns_id":1,"nqn":"nqn.x","group_id":"` + group + `","group_seq":3}`))
	})

	v, err := c.Volume(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if v.ConsistencyGroup != group {
		t.Errorf("consistency group = %q, want %q", v.ConsistencyGroup, group)
	}
}

func TestClientVolumeOutsideAGroupReportsNone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + testVolume + `","name":"vol1","pool_name":"pool1",` +
			`"size":20971520,"ns_id":1,"nqn":"nqn.x"}`))
	})

	v, err := c.Volume(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if v.ConsistencyGroup != "" {
		t.Errorf("consistency group = %q, want none", v.ConsistencyGroup)
	}
}
