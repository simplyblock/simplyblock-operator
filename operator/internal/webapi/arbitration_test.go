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
					{"jm_vuid": 4, "leader": "a", "state": "solo", "fenced_node": "b"},
				},
			})
		case "/api/v2/clusters/c2/arbitration":
			w.WriteHeader(http.StatusNotFound)
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
	if rec.Epoch != 8 || rec.State != "partitioned" {
		t.Fatalf("record %+v", rec)
	}
	if f := rec.FencedNodes(); len(f) != 1 || f[0] != "b" {
		t.Fatalf("fenced %v", f)
	}

	if rec, err := c.GetClusterArbitration(context.Background(), "c2"); err != nil || rec != nil {
		t.Fatalf("404 must be no record: %v %v", rec, err)
	}
	if _, err := c.GetClusterArbitration(context.Background(), "c3"); err == nil {
		t.Fatal("500 must be an error")
	}
}

func TestFencedNodesSteadyIsEmpty(t *testing.T) {
	rec := &ClusterArbitration{State: ArbitrationSteady, LVS: []ArbitrationLVS{{FencedNode: "b"}}}
	if f := rec.FencedNodes(); f != nil {
		t.Fatalf("steady must hold nobody fenced: %v", f)
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
