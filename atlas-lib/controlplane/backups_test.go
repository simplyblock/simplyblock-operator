package controlplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/errs"
)

const testBackup = "44444444-4444-4444-4444-444444444444"
const testPolicy = "66666666-6666-6666-6666-666666666666"
const testSnapshot = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const testPrevBackup = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

// backupJSON is one BackupDTO with every field the wire declares, so that a
// field the decoder stops reading shows up as a zero rather than as a decode
// error nobody sees.
//
// The identifiers are UUIDs because the control plane types them as UUIDs: a
// backup names a volume, a snapshot, a node, and the previous link in its chain,
// and a fixture carrying `node-1` would decode against a client that had stopped
// reading them as identifiers at all.
const backupJSON = `{"id":"` + testBackup + `","s3_id":7,"lvol_id":"` + testVolume + `",` +
	`"lvol_name":"vol1","snapshot_id":"` + testSnapshot + `","snapshot_name":"snap-1",` +
	`"node_id":"` + testNode + `","status":"completed","prev_backup_id":"` + testPrevBackup + `",` +
	`"size":1024,"created_at":1700000000,"completed_at":1700000060,"encrypted":true}`

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
	if got.SnapshotID != testSnapshot || got.NodeID != testNode || got.PrevBackupID != testPrevBackup {
		t.Errorf("backups[0] identifiers = %+v", got)
	}
	if !got.Encrypted {
		t.Errorf("encrypted = false, want true")
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
	const running = `{"id":"` + testBackup + `","s3_id":0,"lvol_id":"` + testVolume + `",` +
		`"lvol_name":"vol1","snapshot_id":"` + testSnapshot + `","snapshot_name":"snap-1",` +
		`"node_id":"` + testNode + `","status":"in_progress","size":0,` +
		`"created_at":0,"completed_at":0,"encrypted":false}`

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
	// The first backup of a chain has no predecessor, and the wire omits the
	// key rather than sending an empty one.
	if backups[0].PrevBackupID != "" {
		t.Errorf("prevBackupID = %q, want empty", backups[0].PrevBackupID)
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
func TestClientAttachBackupPolicyToConsistencyGroup(t *testing.T) {
	const groupID = testCluster + "/c9c9c9c9-c9c9-4c9c-8c9c-c9c9c9c9c9c9"
	var body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.WriteHeader(http.StatusCreated)
	})

	if err := c.AttachBackupPolicyToConsistencyGroup(
		context.Background(), testCluster, testPolicy, groupID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, AttachTargetConsistencyGroup) || !strings.Contains(body, groupID) {
		t.Errorf("attach body = %q, want the consistency-group target and id", body)
	}
}

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

func TestClientRestoreGroupBackup(t *testing.T) {
	const (
		groupBackup  = "55555555-5555-5555-5555-555555555555"
		restoredCG   = "11111111-1111-1111-1111-111111111111/99999999-9999-9999-9999-999999999999"
		memberVolOne = "66666666-6666-6666-6666-666666666666"
		memberVolTwo = "77777777-7777-7777-7777-777777777777"
	)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if want := "/backups/groups/" + groupBackup + "/restore"; !strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), want) {
			t.Errorf("path = %s, want it to end in %s", r.URL.Path, want)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"group_id":"` + restoredCG + `","members":[` +
			`{"lvol_id":"` + memberVolOne + `","member_name":"db"},` +
			`{"lvol_id":"` + memberVolTwo + `","member_name":"data"}]}`))
	})

	// The composite "<cluster>/<uuid>" form the label carries is accepted; the
	// endpoint is reached by the UUID.
	groupID, members, err := c.RestoreGroupBackup(context.Background(), testCluster, RestoreGroupBackupParams{
		GroupBackupID: testCluster + "/" + groupBackup, IntoGroupName: "restored-cg", Pool: "pool1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if groupID != restoredCG {
		t.Errorf("restored group = %q, want %q", groupID, restoredCG)
	}
	want := []RestoredGroupMember{
		{LvolID: memberVolOne, MemberName: "db"},
		{LvolID: memberVolTwo, MemberName: "data"},
	}
	if len(members) != 2 || members[0] != want[0] || members[1] != want[1] {
		t.Errorf("restored members = %v, want %v", members, want)
	}
}

// A group restore whose response names no group or no volumes leaves the caller
// with nothing to wait on, so it is an error rather than an empty result.
func TestClientRestoreGroupBackupWithoutVolumesFails(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"group_id":"11111111-1111-1111-1111-111111111111/abc","members":[]}`))
	})

	if _, _, err := c.RestoreGroupBackup(context.Background(), testCluster,
		RestoreGroupBackupParams{GroupBackupID: "55555555-5555-5555-5555-555555555555", IntoGroupName: "restored-cg", Pool: "pool1"},
	); !errors.Is(err, errs.ErrInvalidResponse) {
		t.Errorf("err = %v, want ErrInvalidResponse", err)
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

func TestClientCreateVolumeBackup(t *testing.T) {
	var method, path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Location", "/api/v2/clusters/"+testCluster+"/backups/"+testBackup)
		w.WriteHeader(http.StatusCreated)
	})

	id, err := c.CreateVolumeBackup(context.Background(), testCluster, testPool, testVolume)
	if err != nil || id != testBackup {
		t.Fatalf("id = %q, err = %v, want %q from the Location header", id, err, testBackup)
	}
	want := "/api/v2/clusters/" + testCluster + "/storage-pools/" + testPool + "/volumes/" + testVolume + "/backups"
	if method != http.MethodPost || strings.TrimSuffix(path, "/") != want {
		t.Errorf("request = %s %s, want POST %s", method, path, want)
	}
}

func TestClientCreateVolumeBackupRejectsALocationThatIsNotAnID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v2/clusters/"+testCluster+"/backups/")
		w.WriteHeader(http.StatusCreated)
	})

	if id, err := c.CreateVolumeBackup(context.Background(), testCluster, testPool, testVolume); err == nil {
		t.Errorf("id = %q, want an error for a response that names no backup", id)
	}
}

// Taking a group generation addresses the group by its UUID, whichever form the
// caller holds it in, sends the caller's request id so a retry is answered with
// the same generation, and returns the generation's sequence: the number a group
// backup of that generation is asked for by.
func TestClientTakeGroupSnapshot(t *testing.T) {
	const group = "99999999-9999-9999-9999-999999999999"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), group+"/snapshots") {
			t.Errorf("request = %s %s, want POST .../consistency-groups/%s/snapshots", r.Method, r.URL.Path, group)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"request_id":"sbk-req-1"`) {
			t.Errorf("body = %s, want it to carry the request id", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"group_seq":7,"created_at":1700000000,"expected":2,"present":2,"complete":true,"members":[]}`))
	})

	seq, err := c.TakeGroupSnapshot(context.Background(), testCluster, testCluster+"/"+group, "sbk-req-1")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 7 {
		t.Errorf("seq = %d, want 7", seq)
	}
}

// A group backup is asked for one named generation of the group the body names,
// so a retry that names the same generation backs up nothing new, and the call
// returns the group backup's id.
func TestClientCreateGroupBackup(t *testing.T) {
	const (
		group       = "99999999-9999-9999-9999-999999999999"
		groupBackup = "55555555-5555-5555-5555-555555555555"
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/backups/groups") {
			t.Errorf("request = %s %s, want POST .../backups/groups/", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"group_seq":7`) || !strings.Contains(string(body), `"consistency_group_id":"`+group+`"`) {
			t.Errorf("body = %s, want it to name group %s and generation 7", body, group)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + groupBackup + `","cluster_id":"` + testCluster + `",` +
			`"consistency_group_id":"` + group + `","consistency_group_name":"cg","group_seq":7,` +
			`"status":"in_progress","members":[{"lvol_id":"a","backup_id":"b"},{"lvol_id":"c","backup_id":"d"}],` +
			`"size":0,"created_at":1,"completed_at":0,"encrypted":false}`))
	})

	created, err := c.CreateGroupBackup(context.Background(), testCluster, group, 7)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != groupBackup || created.MemberCount != 2 {
		t.Errorf("group backup = %+v, want %s with 2 members", created, groupBackup)
	}
}

// The control plane refuses a group it cannot back up as one unit with a 409,
// which is a status error the caller can read rather than a transport failure.
func TestClientCreateGroupBackupRefused(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"not homogeneous"}`))
	})

	_, err := c.CreateGroupBackup(context.Background(), testCluster, "99999999-9999-9999-9999-999999999999", 7)
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusConflict {
		t.Errorf("err = %v, want a 409 StatusError", err)
	}
}
