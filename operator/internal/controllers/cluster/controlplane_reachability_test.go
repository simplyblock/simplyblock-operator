// The control-plane adapter's leader-site write and address probe, against the
// routes as the control plane serves them (design-stretched-sync-replication-
// ramen.md §5.4, §11.1).

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func adapterAgainst(t *testing.T, h http.HandlerFunc) ControlPlane {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewControlPlane(func(context.Context) string { return srv.URL },
		func(context.Context) (string, bool) { return "", false })
}

func TestSetLeaderSitePutsTheSiteOnTheCluster(t *testing.T) {
	var method, path string
	var body map[string]string
	api := adapterAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusNoContent)
	})
	if err := api.SetLeaderSite(context.Background(), "c-1", "site-a"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut || path != "/api/v2/clusters/c-1/" || body["leader_site"] != "site-a" {
		t.Errorf("sent %s %s %v", method, path, body)
	}
}

func TestReachabilityAsksForTheGivenAddresses(t *testing.T) {
	var asked []string
	api := adapterAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query()["address"]
		_, _ = w.Write([]byte(`[{"id":"","mgmt_ip":"192.0.2.1","site":"","reachable":true},
			{"id":"","mgmt_ip":"192.0.2.2","site":"","reachable":false}]`))
	})
	got, err := api.Reachability(context.Background(), "c-1", []string{"192.0.2.1", "192.0.2.2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 || len(got) != 2 || !got[0].Reachable || got[1].Reachable {
		t.Errorf("asked %v, got %+v", asked, got)
	}
}

// A control plane without the probe answers 404, and one that predates the
// address parameter answers its nodes' rows instead: both are no probe, never a
// worker reported unreachable.
func TestAControlPlaneWithoutTheAddressProbeIsUnsupported(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"404": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		"node rows": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[{"id":"n-1","mgmt_ip":"10.0.0.9","site":"a","reachable":true}]`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := adapterAgainst(t, h).Reachability(context.Background(), "c-1", []string{"192.0.2.1"})
			if !errors.Is(err, ErrReachabilityUnsupported) {
				t.Errorf("err = %v, want ErrReachabilityUnsupported", err)
			}
		})
	}
}
