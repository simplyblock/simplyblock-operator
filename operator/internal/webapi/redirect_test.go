package webapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A stretched control plane's standby site answers a mutation with a 307 to the
// leader's endpoint on another host. Go drops the bearer token on a cross-host
// redirect; the client puts it back, so the leader sees the same credential
// and the same method and body the standby was sent.
func TestAMutationRedirectedToTheLeaderKeepsItsCredentials(t *testing.T) {
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "")
	var auth, method, body string
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, method = r.Header.Get("Authorization"), r.Method
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`"task-1"`))
	}))
	t.Cleanup(leader.Close)
	// 127.0.0.1 and localhost are different hosts to Go's redirect policy.
	leaderURL := strings.Replace(leader.URL, "127.0.0.1", "localhost", 1)
	standby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leaderURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(standby.Close)

	c := NewClient(standby.URL)
	ctx := WithBearerToken(context.Background(), "cluster-secret")
	data, status, err := c.Do(ctx, http.MethodPost, "/api/v2/clusters/c1/storage-nodes", map[string]string{"site": "b"})
	if err != nil || status != http.StatusCreated {
		t.Fatalf("status %d, err %v, body %s", status, err, data)
	}
	if auth != "Bearer cluster-secret" || method != http.MethodPost || !strings.Contains(body, `"site":"b"`) {
		t.Errorf("the leader saw %s %q with Authorization %q", method, body, auth)
	}
}
