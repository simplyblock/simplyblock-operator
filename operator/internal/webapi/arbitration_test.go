package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetClusterArbitration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/clusters/c1/arbitration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				// preferred_node: an older control plane still sends it; the
				// arbiter has no preferred node any more and it is ignored.
				"cluster_id": "c1", "epoch": 8, "state": "partitioned", "preferred_node": "a",
				"lvs": []map[string]any{
					{"jm_vuid": 3, "leader": "a", "state": "solo", "fenced_node": "b"},
				},
				"taint_requests": []string{"b", "b"}, "enabled": true,
				"leases": map[string]any{}, "lease_age_ms": map[string]any{"a": 120},
				"fenced_taint": "storage.simplyblock.io/fenced",
			})
		case "/api/v2/clusters/c2/arbitration":
			w.WriteHeader(http.StatusNotFound)
		case "/api/v2/clusters/c4/arbitration":
			w.WriteHeader(http.StatusConflict)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)

	rec, err := c.GetClusterArbitration(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Epoch != 8 || rec.State != "partitioned" || !rec.Enabled {
		t.Fatalf("record %+v", rec)
	}
	if f := rec.FencedNodes(); len(f) != 1 || f[0] != "b" {
		t.Fatalf("fenced %v", f)
	}

	if rec, err := c.GetClusterArbitration(context.Background(), "c2"); err != nil || rec != nil {
		t.Fatalf("404 must be no record: %v %v", rec, err)
	}
	if rec, err := c.GetClusterArbitration(context.Background(), "c4"); err != nil || rec != nil {
		t.Fatalf("409 (not a two-node cluster) must be no record: %v %v", rec, err)
	}
	if _, err := c.GetClusterArbitration(context.Background(), "c3"); err == nil {
		t.Fatal("500 must be an error")
	}
}

func TestFencedNodesFollowTaintRequests(t *testing.T) {
	rec := &ClusterArbitration{State: ArbitrationSteady, LVS: []ArbitrationLVS{{FencedNode: "b"}}}
	if f := rec.FencedNodes(); f != nil {
		t.Fatalf("no taint_requests must mean nobody tainted: %v", f)
	}
}

func TestReportArbitrationRemediation(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v2/clusters/c1/arbitration/remediation" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL).ReportArbitrationRemediation(context.Background(), "c1", "b", true); err != nil {
		t.Fatal(err)
	}
	if got["node_id"] != "b" || got["fenced"] != true {
		t.Fatalf("body %v", got)
	}
}

// sbcli docs/design/two-node-kubernetes-fencing.md §3.5: the host-fencing
// fields of the record and the three calls the operator makes.
func TestHostFencingAPI(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cluster_id": "c1", "epoch": 7, "state": "degraded", "enabled": true, "host_fencing": true,
				"host_fences": map[string]any{"b": map[string]any{"state": "revoked", "on": "a", "epoch": 7,
					"since": 1761130000000, "host_nqns": []string{"nqn.x"}}},
				"client_hosts": map[string]any{"b": []string{"nqn.x"}},
			})
		case r.URL.Path == "/api/v2/clusters/c1/arbitration/host-fence/release":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	ctx := context.Background()

	rec, err := c.GetClusterArbitration(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	f := rec.HostFences["b"]
	if !rec.HostFencing || f.State != HostFenceRevoked || f.On != "a" || !f.Effective() || rec.ClientHosts["b"][0] != "nqn.x" {
		t.Fatalf("record %+v", rec)
	}
	if err := c.SetArbitrationClientHosts(ctx, "c1", "b", []string{"nqn.x", "nqn.y"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ReleaseHostFence(ctx, "c1", "b"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetTwoNodeSettings(ctx, "c1", true, true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`GET /api/v2/clusters/c1/arbitration null`,
		`PUT /api/v2/clusters/c1/arbitration/client-host {"host_nqns":["nqn.x","nqn.y"],"node_id":"b"}`,
		`POST /api/v2/clusters/c1/arbitration/host-fence/release {"node_id":"b"}`,
		`PUT /api/v2/clusters/c1/ {"two_node_arbitration":true,"two_node_host_fencing":true}`,
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("call %d = %q, want %q (all: %q)", i, got[i], want[i], got)
		}
	}
}

func TestReleaseHostFenceNotYetReleasableIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL).ReleaseHostFence(context.Background(), "c1", "b"); err == nil {
		t.Fatal("409 must be an error so the operator retries")
	}
}
