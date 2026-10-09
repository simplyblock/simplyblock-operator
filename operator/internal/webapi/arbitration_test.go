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

func TestSetArbitrationPreferredNode(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v2/clusters/c1/arbitration/preferred" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL).SetArbitrationPreferredNode(context.Background(), "c1", "a"); err != nil {
		t.Fatal(err)
	}
	if got["node_id"] != "a" {
		t.Fatalf("body %v", got)
	}
}
