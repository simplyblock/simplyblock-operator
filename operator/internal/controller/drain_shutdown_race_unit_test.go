// A shutdown that has not landed must not start the device step: in_shutdown
// ends with an offline write that undid the step's migrating_devices stamp
// (2026-09-30, runs 19 and 24).

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

func shutdownStatusServer(t *testing.T, nodeStatus string, posts *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			*posts++
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + opsTestNodeUUID + `","status":"` + nodeStatus + `"}`))
	}))
}

func TestIsNodeStopped_InShutdownIsNotStopped(t *testing.T) {
	if isNodeStopped(utils.NodeStatusInShutdown) {
		t.Error("in_shutdown counted as stopped; the shutdown is still running")
	}
	for _, s := range []string{utils.NodeStatusOffline, nodeStatusMigratingDevices, utils.NodeStatusRemoved} {
		if !isNodeStopped(s) {
			t.Errorf("%s not counted as stopped", s)
		}
	}
}

func TestDrainShutdown_WaitsWhileTheShutdownIsRunning(t *testing.T) {
	posts := 0
	srv := shutdownStatusServer(t, utils.NodeStatusInShutdown, &posts)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseShuttingDown)
	ops.Status.Triggered = true
	if _, err := r.drainShutdown(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainShutdown: %v", err)
	}
	if got := reloadOps(t, r).Status.SubPhase; got == simplyblockv1alpha1.StorageNodeOpsSubPhaseMigratingDevices {
		t.Error("advanced to MigratingDevices while the node was still in_shutdown")
	}
}

func TestDrainShutdown_AShutdownAlreadyRunningIsWaitedForNotRepeated(t *testing.T) {
	posts := 0
	srv := shutdownStatusServer(t, utils.NodeStatusInShutdown, &posts)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseShuttingDown)
	if _, err := r.drainShutdown(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainShutdown: %v", err)
	}
	updated := reloadOps(t, r)
	if posts != 0 {
		t.Errorf("POSTed %d shutdown(s) on top of one already running", posts)
	}
	if !updated.Status.Triggered {
		t.Error("the running shutdown was not adopted as this drain's own")
	}
	if updated.Status.SubPhase == simplyblockv1alpha1.StorageNodeOpsSubPhaseMigratingDevices {
		t.Error("advanced before the shutdown landed")
	}
}

func TestDrainShutdown_AdvancesOnceOffline(t *testing.T) {
	posts := 0
	srv := shutdownStatusServer(t, utils.NodeStatusOffline, &posts)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseShuttingDown)
	ops.Status.Triggered = true
	if _, err := r.drainShutdown(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainShutdown: %v", err)
	}
	if got := reloadOps(t, r).Status.SubPhase; got != simplyblockv1alpha1.StorageNodeOpsSubPhaseMigratingDevices {
		t.Errorf("subphase = %q, want MigratingDevices once the node is offline", got)
	}
}
