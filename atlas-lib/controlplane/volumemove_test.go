// The calls that move a volume to another cluster on the same control plane,
// against a test server speaking the control plane's responses.

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

const (
	moveTargetCluster = "44444444-4444-4444-4444-444444444444"
	moveTargetPool    = "55555555-5555-5555-5555-555555555555"
	testTask          = "66666666-6666-6666-6666-666666666666"
)

func TestStartVolumeMoveAsksForAMigrationIntoThePool(t *testing.T) {
	var path, body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusNoContent)
	})

	err := c.StartVolumeMove(context.Background(), testHandle,
		MoveTarget{ClusterID: moveTargetCluster, PoolID: moveTargetPool, IntervalMinutes: 1})

	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/volumes/"+testVolume+"/replication/start") {
		t.Errorf("path = %s, want the volume's replication/start", path)
	}
	for _, want := range []string{`"mode":"migration"`, `"replication_cluster_id":"` + moveTargetCluster + `"`,
		`"replication_pool_id":"` + moveTargetPool + `"`, `"interval_min":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %s, want it to carry %s", body, want)
		}
	}
}

func TestARefusedStartCarriesTheReason(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"replication cluster X is the volume's own cluster"}`))
	})

	err := c.StartVolumeMove(context.Background(), testHandle, MoveTarget{ClusterID: moveTargetCluster})

	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusBadRequest || !strings.Contains(err.Error(), "own cluster") {
		t.Errorf("err = %v, want a 400 carrying the reason", err)
	}
}

func TestCommitVolumeMoveDeletesTheSourceAndReturnsTheTask(t *testing.T) {
	var body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Location", "/api/v2/clusters/"+testCluster+"/tasks/"+testTask+"/")
		w.WriteHeader(http.StatusAccepted)
	})

	task, err := c.CommitVolumeMove(context.Background(), testHandle)

	if err != nil || task != testTask {
		t.Fatalf("CommitVolumeMove = %q, %v, want task %s", task, err, testTask)
	}
	if !strings.Contains(body, `"delete_source":true`) {
		t.Errorf("body = %s, want delete_source true", body)
	}
}

// A commit the control plane already holds answers 409 naming the task, and
// the resend learns the task it is waiting on rather than failing.
func TestARepeatedCommitReturnsTheTaskAlreadyRunning(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/api/v2/clusters/"+testCluster+"/tasks/"+testTask+"/")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"task_id":"` + testTask + `"}`))
	})

	task, err := c.CommitVolumeMove(context.Background(), testHandle)

	if err != nil || task != testTask {
		t.Errorf("CommitVolumeMove = %q, %v, want the running task", task, err)
	}
}

func TestProceedBeforeTheCutoverIsPendingIsNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/replication/cutover-proceed") {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	})

	if err := c.ProceedVolumeCutover(context.Background(), testHandle); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("ProceedVolumeCutover = %v, want ErrNotFound", err)
	}
}

func TestStopVolumeReplication(t *testing.T) {
	var path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.StopVolumeReplication(context.Background(), testHandle); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/replication/stop") {
		t.Errorf("path = %s, want replication/stop", path)
	}
}

func TestTaskReadsStatusAndResult(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/clusters/"+testCluster+"/tasks/"+testTask) {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + testTask + `","cluster_id":"` + testCluster + `","status":"done",` +
			`"function_name":"replication_final","function_result":"cutover done","canceled":false,` +
			`"retry":1,"max_retry":10,"storage_node_id":null,"device_id":null,` +
			`"function_params":{}}`))
	})

	task, err := c.Task(context.Background(), testCluster, testTask)

	if err != nil {
		t.Fatal(err)
	}
	if !task.Done() || task.Result != "cutover done" || task.Canceled {
		t.Errorf("task = %+v, want done with its result", task)
	}
}

func TestRelationshipCarriesItsStateAndMode(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"replication_id":"77777777-7777-7777-7777-777777777777","is_source":true,"state":"cutover_pending","mode":"migration",` +
			`"direction":"to_target","source_lvol_id":"` + testVolume + `","target_lvol_id":"` + testVolume + `"}`))
	})

	rel, err := c.GetVolumeReplicationRelationship(context.Background(), testHandle)

	if err != nil {
		t.Fatal(err)
	}
	if rel.State != RelationshipCutoverPending || rel.Mode != "migration" {
		t.Errorf("relationship = %+v, want cutover_pending in migration mode", rel)
	}
}
