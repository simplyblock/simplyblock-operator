// Starting the device rebuild: a transient failure is retried, a refusal ends
// the op, and a "already running" answer is the latch.

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// devicePostServer answers the migrate-devices POST with postStatus and every
// other request as a stopped node, so a resume attempt is visible in posts.
func devicePostServer(t *testing.T, postStatus int, posts *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/migrate-devices"):
			w.WriteHeader(postStatus)
		case r.Method == http.MethodPost:
			*posts++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + opsTestNodeUUID + `","status":"` + nodeStatusMigratingDevices + `"}`))
		}
	}))
}

func TestDrainMigrateDevices_ARefusedStartFailsTheOpInsteadOfRetryingForEver(t *testing.T) {
	posts := 0
	srv := devicePostServer(t, http.StatusBadRequest, &posts)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseMigratingDevices)
	if _, err := r.drainMigrateDevices(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainMigrateDevices: %v", err)
	}
	updated := reloadOps(t, r)
	if updated.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Errorf("phase: got %q, want Failed on a refused device migration", updated.Status.Phase)
	}
	if updated.Status.DevicesTriggered {
		t.Error("a refused POST was latched as started")
	}
	if posts != 0 {
		t.Errorf("attempted %d resume(s) of a stopped node", posts)
	}
}

func TestDrainMigrateDevices_ATransientStartFailureIsRetried(t *testing.T) {
	posts := 0
	srv := devicePostServer(t, http.StatusServiceUnavailable, &posts)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseMigratingDevices)
	res, err := r.drainMigrateDevices(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("drainMigrateDevices: %v", err)
	}
	if res.RequeueAfter != drainRequeueDevices {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, drainRequeueDevices)
	}
	updated := reloadOps(t, r)
	if updated.Status.Phase == simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Error("a transient failure ended the op")
	}
	if updated.Status.DevicesTriggered {
		t.Error("a failed POST was latched as started")
	}
}

func TestDrainMigrateDevices_AlreadyRunningIsTheLatch(t *testing.T) {
	posts := 0
	srv := devicePostServer(t, http.StatusConflict, &posts)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseMigratingDevices)
	if _, err := r.drainMigrateDevices(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainMigrateDevices: %v", err)
	}
	if !reloadOps(t, r).Status.DevicesTriggered {
		t.Error("a 409 (rebuild already running) was not taken as the latch")
	}
}
