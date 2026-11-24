// Which control plane the metadata server's state volume is managed through.
package driver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/simplyblock-operator/internal/mdsstate"
)

// localVolumeAPI stands for the operator's own control plane.
type localVolumeAPI struct{ mdsstate.VolumeAPI }

// Regression: the state volume of a remote cluster was looked up through the
// local control plane, so on a client cluster no metadata server was promoted
// and no pNFS export was served.
func TestTheStateVolumeOfARemoteClusterGoesToItsEndpoint(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != "remote-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	d := withRemote(testDriver("simplyblock"), euClusterID, srv.URL, "eu-creds")
	c := fake.NewClientBuilder().WithScheme(reconcilerScheme(t)).
		WithObjects(d, credentialsSecret("eu-creds", "remote-secret")).Build()

	api, err := StateVolumeAPI(c, localVolumeAPI{})(context.Background(), euClusterID)
	if err != nil {
		t.Fatalf("StateVolumeAPI: %v", err)
	}
	if _, isLocal := api.(localVolumeAPI); isLocal {
		t.Fatal("got the local control plane for a remote cluster")
	}
	if _, err := api.ListVolumes(context.Background(), euClusterID, "922d8ed3-b95f-4a91-9037-5c80aba8b499"); err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if hits.Load() == 0 {
		t.Error("the remote endpoint was never called")
	}
}

func TestTheStateVolumeOfAnUndeclaredClusterGoesToTheLocalControlPlane(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(reconcilerScheme(t)).WithObjects(testDriver("simplyblock")).Build()

	api, err := StateVolumeAPI(c, localVolumeAPI{})(context.Background(), usClusterID)
	if err != nil {
		t.Fatalf("StateVolumeAPI: %v", err)
	}
	if _, isLocal := api.(localVolumeAPI); !isLocal {
		t.Errorf("got %T, want the local control plane", api)
	}
}
