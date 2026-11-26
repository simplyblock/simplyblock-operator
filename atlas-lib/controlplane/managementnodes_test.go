package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSiteNode = "44444444-4444-4444-4444-444444444444"

// A site's control plane registers itself with its site and the endpoint the
// other site redirects to; the record comes back as the control plane kept it.
func TestRegisterManagementNodeSendsSiteAndEndpoint(t *testing.T) {
	var body map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/management-nodes/" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("cluster_id"); got != testCluster {
			t.Errorf("cluster_id = %q, want %q", got, testCluster)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + testSiteNode + `","hostname":"","ip":"0.0.0.0","status":"online","site":"b"}`))
	})
	node, err := c.RegisterManagementNode(context.Background(), testCluster,
		RegisterManagementNodeParams{Site: "b", APIEndpoint: "https://cp-b.example"})
	if err != nil {
		t.Fatal(err)
	}
	if body["site"] != "b" || body["api_endpoint"] != "https://cp-b.example" {
		t.Errorf("body = %v", body)
	}
	if node.ID != testSiteNode || node.Site != "b" {
		t.Errorf("node = %+v", node)
	}
}

func TestRegisterManagementNodeRefusalIsAStatusError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"a management node with address 0.0.0.0 already exists"}`))
	})
	_, err := c.RegisterManagementNode(context.Background(), testCluster, RegisterManagementNodeParams{Site: "b"})
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusConflict {
		t.Fatalf("err = %v, want a 409 StatusError", err)
	}
}

func TestManagementNodesListsSites(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/management-nodes/" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"` + testSiteNode + `","hostname":"h","ip":"0.0.0.0","status":"online","site":"a"},` +
			`{"id":"` + testCluster + `","hostname":"","ip":"0.0.0.0","status":"online"}]`))
	})
	nodes, err := c.ManagementNodes(context.Background(), testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].Site != "a" || nodes[1].Site != "" {
		t.Errorf("nodes = %+v", nodes)
	}
}

// A standby site answers a mutation with a 307 to the leader on another host.
// Go drops the bearer token on a cross-host redirect; the client puts it back,
// so the call reaches the leader authenticated.
func TestAMutationRedirectedToTheLeaderKeepsItsCredentials(t *testing.T) {
	var leaderAuth, leaderMethod string
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaderAuth, leaderMethod = r.Header.Get("Authorization"), r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + testSiteNode + `","hostname":"","ip":"0.0.0.0","status":"online","site":"b"}`))
	}))
	t.Cleanup(leader.Close)
	// 127.0.0.1 and localhost are different hosts to Go's redirect policy.
	leaderURL := strings.Replace(leader.URL, "127.0.0.1", "localhost", 1)
	standby := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leaderURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
	if _, err := standby.RegisterManagementNode(context.Background(), testCluster,
		RegisterManagementNodeParams{Site: "b"}); err != nil {
		t.Fatal(err)
	}
	if leaderMethod != http.MethodPost || leaderAuth != "Bearer sekret" {
		t.Errorf("leader saw %s with Authorization %q, want POST with Bearer sekret", leaderMethod, leaderAuth)
	}
}
