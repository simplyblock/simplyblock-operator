package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestProvider builds a Provider pointed at an httptest server that returns the given
// body for /api/v1/query_range.
func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestGetClustersWindowedLatency_ParsesMatrix(t *testing.T) {
	const body = `{
      "status":"success",
      "data":{
        "resultType":"matrix",
        "result":[
          {"metric":{"cluster":"c1","node":"n1"},"values":[[1000,"1100"],[1300,"1150"],[1600,"1120"]]},
          {"metric":{"cluster":"c1","node":"n2"},"values":[[1000,"2200"]]}
        ]
      }
    }`
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})

	got, err := p.GetClustersWindowedLatency(
		context.Background(), []string{"c1"}, PercentileP50, 6*time.Hour, 5*time.Minute,
	)
	if err != nil {
		t.Fatalf("GetClustersWindowedLatency: %v", err)
	}

	n1 := got["c1"]["n1"]
	if len(n1) != 3 {
		t.Fatalf("n1 samples = %v, want 3 values", n1)
	}
	if n1[0] != 1100 || n1[1] != 1150 || n1[2] != 1120 {
		t.Errorf("n1 samples = %v, want [1100 1150 1120]", n1)
	}
	if len(got["c1"]["n2"]) != 1 || got["c1"]["n2"][0] != 2200 {
		t.Errorf("n2 samples = %v, want [2200]", got["c1"]["n2"])
	}
}

func TestGetClustersWindowedLatency_NoClusters(t *testing.T) {
	// No server contact should be needed for an empty cluster list.
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	})
	got, err := p.GetClustersWindowedLatency(context.Background(), nil, PercentileP50, time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}
