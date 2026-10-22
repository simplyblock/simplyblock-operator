// Deleting a StorageCluster brings its backend cluster down completely: the
// delete is sent with force, so the control plane shuts down and deletes the
// cluster's storage nodes and pools instead of refusing a cluster that is not
// empty, which left the object's finalizer retrying forever.

package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTheBackendClusterIsDeletedWithForce(t *testing.T) {
	var method, path, force string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, force = r.Method, r.URL.Path, r.URL.Query().Get("force")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	api := NewControlPlane(func(context.Context) string { return srv.URL }, nil)

	if err := api.DeleteCluster(context.Background(), "4f0c0000-0000-4000-8000-00000000000a"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}

	if method != http.MethodDelete || path != "/api/v2/clusters/4f0c0000-0000-4000-8000-00000000000a" {
		t.Errorf("request = %s %s", method, path)
	}
	if force != "true" {
		t.Errorf("force = %q, want true: a deleted StorageCluster brings its cluster down completely", force)
	}
}

// A cluster the control plane no longer has is a cluster deleted. A forced
// delete can outlast the client's timeout while it shuts nodes down, and the
// retry that follows finds the cluster gone.
func TestAClusterAlreadyGoneIsDeleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"cluster not found"}`))
	}))
	t.Cleanup(srv.Close)
	api := NewControlPlane(func(context.Context) string { return srv.URL }, nil)

	if err := api.DeleteCluster(context.Background(), "4f0c0000-0000-4000-8000-00000000000a"); err != nil {
		t.Errorf("DeleteCluster of a cluster already gone = %v, want success", err)
	}
}
