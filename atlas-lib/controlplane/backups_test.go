package controlplane

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/simplyblock/atlas/errs"
)

const testBackup = "44444444-4444-4444-4444-444444444444"
const testPolicy = "66666666-6666-6666-6666-666666666666"

// backupJSON is one BackupDTO with every field the wire declares, so that a
// field the decoder stops reading shows up as a zero rather than as a decode
// error nobody sees.
const backupJSON = `{"id":"` + testBackup + `","source_cluster_id":"` + testCluster + `",` +
	`"s3_id":7,"lvol_id":"` + testVolume + `","lvol_name":"vol1","snapshot_id":"snap-id",` +
	`"snapshot_name":"snap-1","node_id":"node-1","status":"completed","prev_backup_id":"prev-1",` +
	`"size":1024,"created_at":1700000000,"completed_at":1700000060,"allowed_hosts":[]}`

func TestClientListAndFindBackups(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + backupJSON + "]"))
	})

	backups, err := c.ListBackups(context.Background(), testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("got %d backups, want 1", len(backups))
	}

	got := backups[0]
	if got.ID != testBackup || got.LvolID != testVolume || got.SizeBytes != 1024 {
		t.Errorf("backups[0] = %+v", got)
	}
	if want := time.Unix(1700000000, 0).UTC(); !got.CreatedAt.Equal(want) {
		t.Errorf("createdAt = %v, want %v", got.CreatedAt, want)
	}
	if want := time.Unix(1700000060, 0).UTC(); !got.CompletedAt.Equal(want) {
		t.Errorf("completedAt = %v, want %v", got.CompletedAt, want)
	}

	found, err := c.BackupByID(context.Background(), testCluster, testBackup)
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != testBackup {
		t.Errorf("BackupByID = %+v", found)
	}
	if _, err := c.BackupByID(context.Background(), testCluster, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("BackupByID(missing) err = %v, want ErrNotFound", err)
	}
}

// A backup that has not finished carries no completion instant, and reporting
// 1970 for it would make a duration metric and an age alert both wrong.
func TestClientBackupWithoutTimestampsHasZeroTimes(t *testing.T) {
	const running = `{"id":"` + testBackup + `","source_cluster_id":"","s3_id":0,"lvol_id":"",` +
		`"lvol_name":"","snapshot_id":"","snapshot_name":"","node_id":"","status":"in_progress",` +
		`"prev_backup_id":"","size":0,"created_at":0,"completed_at":0,"allowed_hosts":[]}`

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + running + "]"))
	})

	backups, err := c.ListBackups(context.Background(), testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if !backups[0].CreatedAt.IsZero() || !backups[0].CompletedAt.IsZero() {
		t.Errorf("times = %v / %v, want both zero", backups[0].CreatedAt, backups[0].CompletedAt)
	}
}

func TestClientCreateBackupPolicyReadsTheHeader(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set(backupPolicyIDHeader, testPolicy)
		w.WriteHeader(http.StatusCreated)
	})

	id, err := c.CreateBackupPolicy(context.Background(), testCluster,
		CreateBackupPolicyParams{Name: "nightly", Schedule: "24h,7", MaxVersions: 7})
	if err != nil {
		t.Fatal(err)
	}
	if id != testPolicy {
		t.Errorf("policy id = %q, want %q", id, testPolicy)
	}
}

// The endpoint declares no response model, so the header is the only place the
// identifier appears. A create that succeeds without one leaves the caller with
// a policy it cannot attach anything to, and failing here says so where it
// happened.
func TestClientCreateBackupPolicyWithoutAnIDFails(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	if _, err := c.CreateBackupPolicy(context.Background(), testCluster,
		CreateBackupPolicyParams{Name: "nightly"}); !errors.Is(err, errs.ErrInvalidResponse) {
		t.Errorf("err = %v, want ErrInvalidResponse", err)
	}
}

func TestClientBackupPolicyByName(t *testing.T) {
	const policyJSON = `{"id":"` + testPolicy + `","name":"nightly","backup_schedule":"24h,7",` +
		`"max_age":"30d","max_versions":7,"status":"active"}`

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + policyJSON + "]"))
	})

	got, err := c.BackupPolicyByName(context.Background(), testCluster, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != testPolicy || got.MaxVersions != 7 || got.MaxAge != "30d" {
		t.Errorf("policy = %+v", got)
	}
	if _, err := c.BackupPolicyByName(context.Background(), testCluster, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("BackupPolicyByName(missing) err = %v, want ErrNotFound", err)
	}
}

// A claim can stop matching a selector twice, and the caller retries a detach
// that timed out, so a detach of something already detached has to converge
// rather than fail. The control plane reports that as a 400 with a message
// rather than as a 404, which is the only reason this is worth a test.
func TestClientDetachBackupPolicyTreatsAMissingAttachmentAsSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"Attachment not found"}`))
	})

	if err := c.DetachBackupPolicy(context.Background(), testCluster, testPolicy, testVolume); err != nil {
		t.Errorf("DetachBackupPolicy = %v, want nil", err)
	}
}

// Every other bad request still has to fail, or the idempotence above would
// swallow a rejected detach.
func TestClientDetachBackupPolicyStillFailsOnAnotherBadRequest(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"target_type must be lvol"}`))
	})

	if err := c.DetachBackupPolicy(context.Background(), testCluster, testPolicy, testVolume); err == nil {
		t.Error("DetachBackupPolicy = nil, want an error")
	}
}

func TestClientDeleteBackupPolicyTreatsAMissingPolicyAsSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	if err := c.DeleteBackupPolicy(context.Background(), testCluster, testPolicy); err != nil {
		t.Errorf("DeleteBackupPolicy = %v, want nil", err)
	}
}

func TestClientRestoreBackup(t *testing.T) {
	const restoredID = "77777777-7777-7777-7777-777777777777"

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"lvol_id":"` + restoredID + `"}`))
	})

	got, err := c.RestoreBackup(context.Background(), testCluster, RestoreBackupParams{
		BackupID: testBackup, LvolName: "restore-1", Pool: "pool1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != restoredID {
		t.Errorf("restored volume = %q, want %q", got, restoredID)
	}
}

// A restore whose response names no volume has left the caller with nothing to
// wait on, so it is an error rather than an empty string that fails later.
func TestClientRestoreBackupWithoutAVolumeFails(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	})

	if _, err := c.RestoreBackup(context.Background(), testCluster,
		RestoreBackupParams{BackupID: testBackup, LvolName: "restore-1", Pool: "pool1"},
	); !errors.Is(err, errs.ErrInvalidResponse) {
		t.Errorf("err = %v, want ErrInvalidResponse", err)
	}
}
