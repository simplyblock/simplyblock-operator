package controller

// A volume of a stretched, sync-replicated cluster is born led from the site
// whose driver creates it (design-stretched-sync-replication-ramen.md §9.5): the
// create names the driver's site, and the control plane places the volume on
// that site's nodes.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/csi-driver/internal/clusters"
)

const driverSite = "site-b"

// newSiteControllerServer is newTestControllerServer with a secret marking the
// cluster sync-replicated or not, as an operator of a stretched cluster writes it.
func newSiteControllerServer(t *testing.T, mock *mockSBCLI, sync bool) *Server {
	t.Helper()
	cs := newTestControllerServer(t, mock)
	cfg := clusters.Config{
		ClusterID: sanityClusterID, ClusterEndpoint: mock.URL(), ClusterSecret: sanitySecret,
		Sync: sync,
	}
	if sync {
		cfg.Site = driverSite
	}
	data, _ := json.Marshal(clusters.Info{Clusters: []clusters.Config{cfg}})
	secret := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(secret, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPDKCSI_SECRET", secret)
	return cs
}

func createdSite(t *testing.T, mock *mockSBCLI) (string, bool) {
	t.Helper()
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.createVolumeBodies) != 1 {
		t.Fatalf("%d create bodies, want 1", len(mock.createVolumeBodies))
	}
	var body map[string]any
	if err := json.Unmarshal(mock.createVolumeBodies[0], &body); err != nil {
		t.Fatal(err)
	}
	site, ok := body["site"].(string)
	return site, ok
}

func TestASyncCreateNamesTheDriversSite(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newSiteControllerServer(t, mock, true)
	if _, err := cs.CreateVolume(context.Background(), basicCreateVolumeRequest("pvc-born")); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if site, _ := createdSite(t, mock); site != driverSite {
		t.Errorf("create sent site %q, want %q", site, driverSite)
	}
}

// Off a sync cluster the control plane refuses any site, so none is sent.
func TestANonSyncCreateNamesNoSite(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newSiteControllerServer(t, mock, false)
	if _, err := cs.CreateVolume(context.Background(), basicCreateVolumeRequest("pvc-plain")); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if site, ok := createdSite(t, mock); ok {
		t.Errorf("create sent site %q on a cluster that is not sync-replicated", site)
	}
}

// Regression: 2026-10-23-create-site-conflict-read-as-exists: the control
// plane answers a create pinned to a node of the other site with a 409, which
// the client read as "the volume exists". The create then reconciled a volume
// that was never made, and retried forever instead of reporting the pin.
func TestACreatePinnedToTheOtherSiteIsAFailedPrecondition(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newSiteControllerServer(t, mock, true)
	mock.mu.Lock()
	mock.injectStatus = func(r *http.Request) int {
		if r.Method == http.MethodPost {
			return http.StatusConflict
		}
		return 0
	}
	mock.injectBody = map[string]any{"detail": map[string]any{
		"message": "node n-1 is on site site-a, not site-b", "node_id": "n-1", "node_site": "site-a", "site": driverSite,
	}}
	mock.mu.Unlock()
	_, err := cs.CreateVolume(context.Background(), basicCreateVolumeRequest("pvc-pinned"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("err = %v, want FailedPrecondition naming the site conflict", err)
	}
}
