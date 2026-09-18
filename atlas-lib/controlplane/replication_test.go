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

func TestClientEnableVolumeReplication(t *testing.T) {
	var gotBody string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.EnableVolumeReplication(context.Background(), testHandle, testPolicy); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"replication_policy_id":"`+testPolicy+`"`) {
		t.Errorf("request body = %q, want it to carry replication_policy_id %s", gotBody, testPolicy)
	}
}

func TestClientEnableVolumeReplicationDifferentPolicyIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte("already attached to a different policy"))
	})
	err := c.EnableVolumeReplication(context.Background(), testHandle, testPolicy)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("err = %v, want a *StatusError carrying 412", err)
	}
}

// DisableVolumeReplication must send an EXPLICIT JSON null for
// replication_policy_id, not omit the key. The generated request struct's
// field is `omitempty`, so a nil pointer there would be dropped from the body
// entirely -- and the backend distinguishes "the key was absent" (leave the
// policy alone) from "the key was null" (detach) by which keys the request
// actually carries, not by the value. A body of `{}` would silently do
// nothing.
func TestClientDisableVolumeReplicationSendsExplicitNull(t *testing.T) {
	var gotBody string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DisableVolumeReplication(context.Background(), testHandle); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"replication_policy_id":null`) {
		t.Errorf("request body = %q, want an explicit null for replication_policy_id", gotBody)
	}
}

func TestClientDisableVolumeReplicationNotAttachedIsSuccess(t *testing.T) {
	// The backend's own idempotency: detaching a volume that follows no
	// policy already returns success, so the client has nothing extra to do
	// here beyond not treating any 2xx as an error.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if err := c.DisableVolumeReplication(context.Background(), testHandle); err != nil {
		t.Errorf("DisableVolumeReplication = %v, want nil", err)
	}
}

func TestClientDisableVolumeReplicationDuringCutoverIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	err := c.DisableVolumeReplication(context.Background(), testHandle)
	if !errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatalf("err = %v, want it to unwrap to a 409 sentinel so the caller can retry", err)
	}
}

func TestClientGetVolumeReplicationInfo(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/replication/status") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"role": "source", "state": "in_sync",
			"last_replicated_at": "2026-09-17T12:00:00Z",
			"lag_seconds": 42, "lag_budget_seconds": 900,
			"outstanding_count": 1, "outstanding_bytes": 1048576,
			"failing_count": 0, "max_retry_reached": false,
			"last_cycle_bytes": 2097152, "last_cycle_seconds": 12,
			"resyncing": false
		}`))
	})

	info, err := c.GetVolumeReplicationInfo(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if info.Role != "source" || info.State != "in_sync" {
		t.Errorf("role/state = %q/%q, want source/in_sync", info.Role, info.State)
	}
	if info.LastReplicatedAt == nil || info.LastReplicatedAt.Unix() != 1789646400 {
		t.Errorf("LastReplicatedAt = %v", info.LastReplicatedAt)
	}
	if info.LagSeconds == nil || *info.LagSeconds != 42 {
		t.Errorf("LagSeconds = %v, want 42", info.LagSeconds)
	}
	if info.LagBudgetSeconds == nil || *info.LagBudgetSeconds != 900 {
		t.Errorf("LagBudgetSeconds = %v, want 900", info.LagBudgetSeconds)
	}
	if info.OutstandingCount != 1 || info.OutstandingBytes != 1048576 {
		t.Errorf("outstanding = %d/%d, want 1/1048576", info.OutstandingCount, info.OutstandingBytes)
	}
	if info.FailingCount != 0 || info.MaxRetryReached {
		t.Errorf("failing/max-retry = %d/%v, want 0/false", info.FailingCount, info.MaxRetryReached)
	}
	if info.LastCycleBytes == nil || *info.LastCycleBytes != 2097152 {
		t.Errorf("LastCycleBytes = %v, want 2097152", info.LastCycleBytes)
	}
	if info.LastCycleSeconds == nil || *info.LastCycleSeconds != 12 {
		t.Errorf("LastCycleSeconds = %v, want 12", info.LastCycleSeconds)
	}
	if info.Resyncing {
		t.Error("Resyncing = true, want false")
	}
}

// A volume that never replicated is a valid, non-error answer: role "none",
// state "not_replicating", and every timing/lag field null.
func TestClientGetVolumeReplicationInfoNeverReplicated(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"role": "none", "state": "not_replicating",
			"outstanding_count": 0, "outstanding_bytes": 0,
			"failing_count": 0, "max_retry_reached": false, "resyncing": false
		}`))
	})

	info, err := c.GetVolumeReplicationInfo(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if info.Role != "none" || info.State != "not_replicating" {
		t.Errorf("role/state = %q/%q", info.Role, info.State)
	}
	if info.LastReplicatedAt != nil || info.LagSeconds != nil || info.LagBudgetSeconds != nil {
		t.Errorf("expected every timing field nil, got LastReplicatedAt=%v LagSeconds=%v LagBudgetSeconds=%v",
			info.LastReplicatedAt, info.LagSeconds, info.LagBudgetSeconds)
	}
}

func TestClientGetVolumeReplicationInfoNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := c.GetVolumeReplicationInfo(context.Background(), testHandle); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestClientPromoteVolumeForcedIgnoresDemoteState(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/replication/failover") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.PromoteVolume(context.Background(), testHandle, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotQuery, "planned=true") {
		t.Errorf("query = %q, forced promote must not ask for the planned gate", gotQuery)
	}
}

func TestClientPromoteVolumePlannedSendsThePlannedFlag(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.PromoteVolume(context.Background(), testHandle, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "planned=true") {
		t.Errorf("query = %q, want planned=true", gotQuery)
	}
}

// The whole point of the planned gate: a demote still converging must surface
// as something the driver can map to codes.Aborted (retryable), never
// codes.FailedPrecondition -- the vendored csi-addons controller escalates
// ANY FAILED_PRECONDITION from a force=false promote to force=true inline,
// with no wait-and-retry grace period of its own.
func TestClientPromoteVolumeWhileDemoteConvergingIsA409(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	err := c.PromoteVolume(context.Background(), testHandle, false)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusConflict {
		t.Fatalf("err = %v, want a *StatusError carrying 409", err)
	}
}

func TestClientPromoteVolumeWithNoDemoteIsA412(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
	})
	err := c.PromoteVolume(context.Background(), testHandle, false)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("err = %v, want a *StatusError carrying 412", err)
	}
}

func TestClientDemoteVolumeNotYetDone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/replication/demote") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	done, err := c.DemoteVolume(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("done = true, want false while still converging")
	}
}

func TestClientDemoteVolumeDone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	done, err := c.DemoteVolume(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Error("done = false, want true")
	}
}

func TestClientDemoteVolumeFailureIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := c.DemoteVolume(context.Background(), testHandle); err == nil {
		t.Error("want an error on a genuine backend failure")
	}
}

func TestClientResyncVolume(t *testing.T) {
	var gotBody string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/replication/failback") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.ResyncVolume(context.Background(), testHandle, testCluster); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"source_cluster_id":"`+testCluster+`"`) {
		t.Errorf("request body = %q, want source_cluster_id %s", gotBody, testCluster)
	}
}

func TestClientResyncVolumeWithoutSourceCluster(t *testing.T) {
	var gotBody string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.ResyncVolume(context.Background(), testHandle, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotBody, "source_cluster_id") {
		t.Errorf("request body = %q, want no source_cluster_id when none is given", gotBody)
	}
}
