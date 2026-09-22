// Whether CreateCluster and ClusterByName carry a managed control plane's
// admin credential.
//
// These two are the only calls made before any cluster secret exists to
// authenticate with -- there is no cluster yet to have one, and no adoption
// has happened yet either. Without the credential reaching them, a
// StorageCluster CR applied against a managed control plane could never
// create its backend identity there at all, which is the gap this closes.
// Every other call on this interface keeps authenticating however it already
// did: as the cluster it was adopted or created as, via the context wrap the
// reconciler applies at the call site (storagecluster_controller.go).

package cluster

import (
	"context"
	"net/http"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
)

const clusterAdminSpecPath = "../../../../shared/openapi.json"

func TestCreateClusterAuthenticatesWithTheManagedAdminCredentialWhenOneResolves(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, clusterAdminSpecPath, false)
	defer mock.Close()

	mock.Register(http.MethodPost, "/api/v2/clusters/", webapimock.RouteResponse{
		Status: http.StatusCreated,
		Body:   `{"id":"cluster-uuid","secret":"cluster-secret"}`,
	})

	resolveEndpoint := func(context.Context) string { return mock.URL() }
	resolveCredential := func(context.Context) (string, bool) { return "admin-token", true }

	api := NewControlPlane(resolveEndpoint, resolveCredential)
	if _, err := api.CreateCluster(context.Background(), utils.ClusterAddParams{Name: "b"}); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected one request, got %d", len(reqs))
	}
	if got := reqs[0].Headers["Authorization"]; got != "Bearer admin-token" {
		t.Errorf("authorization header = %q, want the managed admin credential", got)
	}
}

func TestClusterByNameAuthenticatesWithTheManagedAdminCredentialWhenOneResolves(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, clusterAdminSpecPath, false)
	defer mock.Close()

	mock.Register(http.MethodGet, "/api/v2/clusters/", webapimock.RouteResponse{
		Status: http.StatusOK, Body: `[]`,
	})

	resolveEndpoint := func(context.Context) string { return mock.URL() }
	resolveCredential := func(context.Context) (string, bool) { return "admin-token", true }

	api := NewControlPlane(resolveEndpoint, resolveCredential)
	if _, _, err := api.ClusterByName(context.Background(), "b"); err != nil {
		t.Fatalf("ClusterByName: %v", err)
	}

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected one request, got %d", len(reqs))
	}
	if got := reqs[0].Headers["Authorization"]; got != "Bearer admin-token" {
		t.Errorf("authorization header = %q, want the managed admin credential", got)
	}
}

// A local control plane's resolver names no credential, so CreateCluster
// authenticates exactly as it always has: the client's own (empty) saToken,
// not the admin credential.
func TestCreateClusterCarriesNoAdminCredentialWhenNoneResolves(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, clusterAdminSpecPath, false)
	defer mock.Close()

	mock.Register(http.MethodPost, "/api/v2/clusters/", webapimock.RouteResponse{
		Status: http.StatusCreated,
		Body:   `{"id":"cluster-uuid","secret":"cluster-secret"}`,
	})

	resolveEndpoint := func(context.Context) string { return mock.URL() }
	resolveCredential := func(context.Context) (string, bool) { return "", false }

	api := NewControlPlane(resolveEndpoint, resolveCredential)
	if _, err := api.CreateCluster(context.Background(), utils.ClusterAddParams{Name: "b"}); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected one request, got %d", len(reqs))
	}
	if got := reqs[0].Headers["Authorization"]; got == "Bearer admin-token" {
		t.Errorf("authorization header = %q, want the client's own (empty) token, not the admin credential", got)
	}
}
