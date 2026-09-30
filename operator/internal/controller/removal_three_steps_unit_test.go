// The removal's three steps from the operator's side, and when a drain waits.

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

func TestDrainShutdown_TriggersTheRemovalWithPrepareRemoval(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			paths = append(paths, r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + opsTestNodeUUID + `","status":"` + utils.NodeStatusOnline + `"}`))
	}))
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseShuttingDown)
	if _, err := r.drainShutdown(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainShutdown: %v", err)
	}
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/prepare-removal") {
		t.Fatalf("POSTs = %v, want one prepare-removal", paths)
	}
	if !reloadOps(t, r).Status.Triggered {
		t.Error("the removal was not recorded as triggered")
	}
}

func TestDrainShutdown_ARefusedAdmissionFailsWithoutAResume(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			paths = append(paths, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"Can not remove node: FTT"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + opsTestNodeUUID + `","status":"` + utils.NodeStatusOnline + `"}`))
	}))
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseShuttingDown)
	if _, err := r.drainShutdown(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainShutdown: %v", err)
	}
	if got := reloadOps(t, r).Status.Phase; got != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed on a refused admission", got)
	}
	for _, p := range paths {
		if strings.HasSuffix(p, "/resume") {
			t.Error("resumed a node the refused admission never touched")
		}
	}
}

func TestDrainVerify_WaitsWhileTheControlPlaneSeesSomethingLeft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/verify-drained") {
			_, _ = w.Write([]byte(`{"drained":false,"lvols":[],"snapshots":["s1"]}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseVerifying)
	if _, err := r.drainVerify(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainVerify: %v", err)
	}
	if got := reloadOps(t, r).Status.SubPhase; got != simplyblockv1alpha1.StorageNodeOpsSubPhaseVerifying {
		t.Errorf("subPhase = %q, want Verifying while a snapshot is left on the node", got)
	}
}

func TestDrainPauseReason(t *testing.T) {
	cases := []struct {
		name  string
		flags drainClusterFlags
		want  string
	}{
		{"active", drainClusterFlags{Status: utils.ClusterStatusActive}, ""},
		{"degraded only by the removal", drainClusterFlags{Status: utils.ClusterStatusDegraded, DegradedByRemoval: true}, ""},
		{"degraded by something else", drainClusterFlags{Status: utils.ClusterStatusDegraded}, "not active"},
		{"suspended", drainClusterFlags{Status: utils.ClusterStatusSuspended, DegradedByRemoval: true}, "not active"},
		{"legacy in_shrink", drainClusterFlags{Status: "in_shrink"}, "not active"},
		{"data rebalancing", drainClusterFlags{Status: utils.ClusterStatusActive, DataRebalancing: true}, "rebalancing"},
	}
	for _, c := range cases {
		got := drainPauseReason("", c.flags)
		if (c.want == "") != (got == "") || (c.want != "" && !strings.Contains(got, c.want)) {
			t.Errorf("%s: reason = %q, want %q", c.name, got, c.want)
		}
	}
}
